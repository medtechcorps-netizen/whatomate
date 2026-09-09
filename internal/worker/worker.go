package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/internal/templateutil"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const campaignCanonicalContactAttempts = 3
const campaignDurableSettlementTimeout = 10 * time.Second

const campaignMarketingOptOutMessage = "Contact opted out of marketing messages"
const campaignAmbiguousDeliveryMessage = "Provider delivery outcome is unknown; message was not retried to prevent a duplicate"
const campaignInactiveBeforeDeliveryMessage = "Campaign became inactive before provider delivery"

func campaignJobHasCurrentGeneration(campaign *models.BulkMessageCampaign, job *queue.RecipientJob) bool {
	if campaign == nil || job == nil || campaign.Status != models.CampaignStatusProcessing ||
		campaign.StartedAt == nil || campaign.StartedAt.IsZero() || job.EnqueuedAt.IsZero() {
		return false
	}
	return !job.EnqueuedAt.Before(campaign.StartedAt.UTC())
}

// Worker processes jobs from the queue
type Worker struct {
	Config    *config.Config
	DB        *gorm.DB
	Redis     *redis.Client
	Log       logf.Logger
	WhatsApp  *whatsapp.Client
	QwenHTTP  *http.Client
	Consumer  *queue.RedisConsumer
	Publisher *queue.Publisher
	// channelAdapterFactory is an internal deterministic seam for exercising
	// the complete outbox authorization/send/settlement path in package tests.
	// Production workers leave it nil and use the built-in provider adapters.
	channelAdapterFactory func(*models.ChannelAccount) (channelapi.Adapter, error)
}

// Ensure Worker implements JobHandler interface
var _ queue.JobHandler = (*Worker)(nil)

// New creates a new Worker instance
func New(cfg *config.Config, db *gorm.DB, rdb *redis.Client, log logf.Logger) (*Worker, error) {
	consumer, err := queue.NewRedisConsumer(rdb, log)
	if err != nil {
		return nil, fmt.Errorf("failed to create consumer: %w", err)
	}

	publisher := queue.NewPublisher(rdb, log)

	return &Worker{
		Config:    cfg,
		DB:        db,
		Redis:     rdb,
		Log:       log,
		WhatsApp:  newWhatsAppClient(cfg, log),
		QwenHTTP:  &http.Client{Timeout: 30 * time.Second},
		Consumer:  consumer,
		Publisher: publisher,
	}, nil
}

func newWhatsAppClient(cfg *config.Config, log logf.Logger) *whatsapp.Client {
	if cfg != nil {
		if baseURL := strings.TrimSpace(cfg.WhatsApp.BaseURL); baseURL != "" {
			return whatsapp.NewWithBaseURL(log, strings.TrimRight(baseURL, "/"))
		}
	}
	return whatsapp.New(log)
}

// Run starts the worker and processes jobs until context is cancelled
func (w *Worker) Run(ctx context.Context) error {
	w.Log.Info("Worker starting")

	if err := w.publishHeartbeat(ctx); err != nil {
		return fmt.Errorf("publish initial worker heartbeat: %w", err)
	}
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go w.maintainHeartbeat(heartbeatCtx)

	err := w.Consumer.Consume(ctx, w)
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("consumer error: %w", err)
	}

	w.Log.Info("Worker stopped")
	return nil
}

func (w *Worker) publishHeartbeat(ctx context.Context) error {
	if w == nil || w.Redis == nil {
		return errors.New("worker heartbeat requires Redis")
	}
	return w.Redis.Set(
		ctx,
		queue.WorkerHeartbeatKey,
		time.Now().UTC().Format(time.RFC3339Nano),
		queue.WorkerHeartbeatTTL,
	).Err()
}

func (w *Worker) maintainHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(queue.WorkerHeartbeatInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.publishHeartbeat(ctx); err != nil && ctx.Err() == nil {
				w.Log.Error("Failed to publish worker heartbeat", "error", err)
			}
		}
	}
}

// HandleRecipientJob processes a single recipient message job
func (w *Worker) HandleRecipientJob(ctx context.Context, job *queue.RecipientJob) error {
	var campaign models.BulkMessageCampaign
	var account models.WhatsAppAccount
	var contact *models.Contact
	terminal := false
	if err := w.withRecipientTenantTransaction(ctx, job.OrganizationID, func(tx *gorm.DB) error {
		if err := tx.
			Where("id = ? AND organization_id = ?", job.CampaignID, job.OrganizationID).
			Preload("Template", "organization_id = ?", job.OrganizationID).
			First(&campaign).Error; err != nil {
			w.Log.Error("Failed to load campaign", "error", err, "campaign_id", job.CampaignID)
			return fmt.Errorf("failed to load campaign: %w", err)
		}
		if !campaignJobHasCurrentGeneration(&campaign, job) {
			w.Log.Info(
				"Campaign job is not active for the current generation, skipping recipient",
				"campaign_id", job.CampaignID,
				"status", campaign.Status,
				"recipient_id", job.RecipientID,
			)
			terminal = true
			return nil
		}
		if err := tx.
			Where("name = ? AND organization_id = ?", campaign.WhatsAppAccount, job.OrganizationID).
			First(&account).Error; err != nil {
			w.Log.Error("Failed to load WhatsApp account", "error", err, "account_name", campaign.WhatsAppAccount)
			if failErr := failCampaignRecipientBeforeClaimTx(
				tx,
				job,
				"WhatsApp account not found",
			); failErr != nil {
				return failErr
			}
			terminal = true
			return nil
		}
		if err := whatsappaccount.RequireActiveForOutbound(&account); err != nil {
			w.Log.Warn(
				"WhatsApp account is inactive; campaign recipient will not be sent",
				"account_id", account.ID,
				"campaign_id", job.CampaignID,
			)
			if failErr := failCampaignRecipientBeforeClaimTx(
				tx,
				job,
				whatsappaccount.ErrOutboundInactive.Error(),
			); failErr != nil {
				return failErr
			}
			terminal = true
			return nil
		}

		resolved, _, err := contactutil.GetOrCreateContact(
			tx,
			job.OrganizationID,
			job.PhoneNumber,
			job.RecipientName,
		)
		if err != nil || resolved == nil {
			w.Log.Error("Failed to get or create contact", "error", err, "phone", job.PhoneNumber)
			if failErr := failCampaignRecipientBeforeClaimTx(
				tx,
				job,
				"Failed to create contact",
			); failErr != nil {
				return failErr
			}
			terminal = true
			return nil
		}
		contact = resolved
		return nil
	}); err != nil {
		return err
	}
	if terminal {
		return nil
	}
	w.decryptAccountSecrets(&account)

	delivery, err := w.deliverCampaignRecipient(
		ctx,
		job,
		&campaign,
		&account,
		contact.ID,
	)
	if err != nil {
		return fmt.Errorf("deliver campaign recipient: %w", err)
	}
	if delivery.campaignInactive {
		w.Log.Info(
			"Campaign became inactive before provider delivery",
			"campaign_id", job.CampaignID,
			"recipient_id", job.RecipientID,
		)
	} else if delivery.alreadyProcessed {
		w.Log.Info(
			"Campaign recipient was already processed",
			"campaign_id", job.CampaignID,
			"recipient_id", job.RecipientID,
		)
	} else if delivery.ambiguous {
		w.Log.Error(
			"Campaign delivery was dead-lettered without a retry",
			"campaign_id", job.CampaignID,
			"recipient_id", job.RecipientID,
			"message_id", delivery.message.ID,
		)
	} else if delivery.consentRejected {
		w.Log.Info(
			"Skipping marketing message for opted-out canonical contact",
			"contact_id", delivery.contactID,
			"phone", job.PhoneNumber,
		)
	} else if delivery.sendErr != nil {
		w.Log.Error("Failed to send message", "error", delivery.sendErr, "recipient", job.PhoneNumber)
	} else {
		w.Log.Info(
			"Message sent",
			"recipient", job.PhoneNumber,
			"message_id", delivery.message.WhatsAppMessageID,
		)
	}

	if err := w.withRecipientTenantTransactionAndCampaignStats(ctx, job.OrganizationID, func(tx *gorm.DB) (*queue.CampaignStatsUpdate, error) {
		scoped := *w
		scoped.DB = tx
		return scoped.checkCampaignCompletion(ctx, job.CampaignID, job.OrganizationID)
	}); err != nil {
		return fmt.Errorf("finalize campaign recipient tenant phase: %w", err)
	}

	return nil
}

type campaignRecipientDelivery struct {
	message              *models.Message
	contactID            uuid.UUID
	sendErr              error
	consentRejected      bool
	ambiguous            bool
	campaignInactive     bool
	alreadyProcessed     bool
	mirrorConversationID uuid.UUID
}

// deliverCampaignRecipient uses a two-phase, at-most-once protocol for legacy
// Meta delivery. Meta does not accept a client idempotency key for this API:
// first we durably claim the recipient with a pending Message, then we hold the
// canonical contact lock across the final consent check and provider attempt.
// A redelivery that finds an unresolved claim is dead-lettered rather than
// risking a duplicate. This deliberately prefers a visible ambiguous failure
// over duplicate marketing delivery if the process dies around the API call.
func (w *Worker) deliverCampaignRecipient(
	ctx context.Context,
	job *queue.RecipientJob,
	campaign *models.BulkMessageCampaign,
	account *models.WhatsAppAccount,
	requestedContactID uuid.UUID,
) (campaignRecipientDelivery, error) {
	delivery, err := w.prepareCampaignRecipient(
		ctx,
		job,
		campaign,
		requestedContactID,
	)
	if err != nil || delivery.alreadyProcessed || delivery.consentRejected ||
		delivery.ambiguous || delivery.message == nil {
		return delivery, err
	}
	delivery, err = w.mirrorPreparedCampaignDelivery(ctx, job, account, delivery)
	if err != nil {
		return delivery, err
	}
	return w.attemptPreparedCampaignDelivery(ctx, job, campaign, account, requestedContactID, delivery)
}

// mirrorPreparedCampaignDelivery commits the provider-neutral inbox linkage
// before the Graph side-effect boundary. A provider request must never depend
// on a mirror that is visible only inside its own still-uncommitted transaction.
func (w *Worker) mirrorPreparedCampaignDelivery(
	ctx context.Context,
	job *queue.RecipientJob,
	account *models.WhatsAppAccount,
	prepared campaignRecipientDelivery,
) (campaignRecipientDelivery, error) {
	delivery := prepared
	if job == nil || account == nil || prepared.message == nil || prepared.message.ID == uuid.Nil {
		return delivery, errors.New("prepared campaign mirror authority is incomplete")
	}

	var conversationID uuid.UUID
	err := w.withRecipientTenantTransaction(ctx, job.OrganizationID, func(tx *gorm.DB) error {
		var recipient models.BulkMessageRecipient
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND campaign_id = ?", job.RecipientID, job.CampaignID).
			First(&recipient).Error; err != nil {
			return fmt.Errorf("lock campaign recipient before inbox mirror: %w", err)
		}
		if recipient.Status != models.MessageStatusPending ||
			recipient.MessageID == nil || *recipient.MessageID != prepared.message.ID {
			return errors.New("campaign delivery claim changed before inbox mirror")
		}

		result, err := channelapi.MirrorLegacyWhatsAppMessage(
			tx,
			channelapi.LegacyMetaAccountRef{
				ID:             account.ID,
				OrganizationID: account.OrganizationID,
				Name:           account.Name,
				Status:         account.Status,
			},
			prepared.message.ID,
		)
		if err != nil {
			return fmt.Errorf("mirror campaign message before provider delivery: %w", err)
		}
		if result.ConversationID == uuid.Nil {
			return errors.New("campaign inbox mirror returned no conversation")
		}

		var mirrored models.Message
		if err := tx.Select("id", "inbox_conversation_id").
			Where("id = ? AND organization_id = ?", prepared.message.ID, job.OrganizationID).
			First(&mirrored).Error; err != nil {
			return fmt.Errorf("reload committed campaign inbox mirror: %w", err)
		}
		if mirrored.InboxConversationID == nil || *mirrored.InboxConversationID != result.ConversationID {
			return errors.New("campaign inbox mirror projection is inconsistent")
		}
		conversationID = result.ConversationID
		return nil
	})
	if err != nil {
		// The mirror transaction did not report a commit, so no provider call has
		// occurred. Reconcile/release using a fresh bounded context even when the
		// queue lease or caller was cancelled. A commit-ambiguous linked message
		// is rejected by releasePreparedCampaignDeliveryTx and stays fail-closed.
		settlementCtx, cancel := context.WithTimeout(context.Background(), campaignDurableSettlementTimeout)
		defer cancel()
		if cleanupErr := w.releasePreparedCampaignDelivery(
			settlementCtx,
			job,
			prepared.message.ID,
		); cleanupErr != nil {
			return delivery, fmt.Errorf(
				"release unattempted campaign delivery after mirror failure: %w (mirror error: %v)",
				cleanupErr,
				err,
			)
		}
		delivery.message = nil
		return delivery, err
	}

	delivery.mirrorConversationID = conversationID
	delivery.message.InboxConversationID = &conversationID
	return delivery, nil
}

func (w *Worker) prepareCampaignRecipient(
	ctx context.Context,
	job *queue.RecipientJob,
	campaign *models.BulkMessageCampaign,
	requestedContactID uuid.UUID,
) (campaignRecipientDelivery, error) {
	var delivery campaignRecipientDelivery
	if campaign == nil || campaign.Template == nil {
		return delivery, errors.New("campaign template is required")
	}

	err := w.campaignCanonicalContactTransaction(
		ctx,
		job.OrganizationID,
		func(tx *gorm.DB, deliveryAttempted *bool) error {
			delivery = campaignRecipientDelivery{}

			var storedRecipient models.BulkMessageRecipient
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND campaign_id = ?", job.RecipientID, job.CampaignID).
				First(&storedRecipient).Error; err != nil {
				return fmt.Errorf("lock campaign recipient: %w", err)
			}
			if storedRecipient.Status != models.MessageStatusPending {
				delivery.alreadyProcessed = true
				return nil
			}
			if storedRecipient.MessageID != nil {
				var claimedMessage models.Message
				if err := tx.Where(
					"id = ? AND organization_id = ?",
					*storedRecipient.MessageID,
					job.OrganizationID,
				).First(&claimedMessage).Error; err != nil {
					return fmt.Errorf("load claimed campaign message: %w", err)
				}
				if err := finalizePreparedCampaignDeliveryTx(
					tx,
					job,
					&claimedMessage,
					models.MessageStatusFailed,
					"",
					campaignAmbiguousDeliveryMessage,
					"failed_count",
				); err != nil {
					return err
				}
				claimedMessage.Status = models.MessageStatusFailed
				claimedMessage.ErrorMessage = campaignAmbiguousDeliveryMessage
				delivery.message = &claimedMessage
				delivery.contactID = claimedMessage.ContactID
				delivery.ambiguous = true
				return nil
			}

			contact, err := contactutil.ResolveCanonicalContactForUpdate(
				tx,
				job.OrganizationID,
				requestedContactID,
			)
			if err != nil {
				return fmt.Errorf("resolve canonical campaign contact: %w", err)
			}
			delivery.contactID = contact.ID

			if strings.EqualFold(campaign.Template.Category, "MARKETING") &&
				contact.MarketingOptOut {
				recipientUpdate := tx.Model(&models.BulkMessageRecipient{}).
					Where(
						"id = ? AND campaign_id = ? AND status = ?",
						job.RecipientID,
						job.CampaignID,
						models.MessageStatusPending,
					).
					Updates(map[string]any{
						"status":               models.MessageStatusFailed,
						"whats_app_message_id": "",
						"message_id":           nil,
						"error_message":        campaignMarketingOptOutMessage,
						"sent_at":              nil,
					})
				if recipientUpdate.Error != nil {
					return fmt.Errorf("record campaign consent rejection: %w", recipientUpdate.Error)
				}
				if recipientUpdate.RowsAffected != 1 {
					return errors.New("campaign recipient changed before consent rejection")
				}
				if err := incrementCampaignCountTx(
					tx,
					job.OrganizationID,
					job.CampaignID,
					"failed_count",
				); err != nil {
					return err
				}
				delivery.consentRejected = true
				return nil
			}

			message := campaignMessage(job, campaign, contact.ID)
			if err := tx.Create(&message).Error; err != nil {
				return fmt.Errorf("create pending campaign message: %w", err)
			}
			recipientUpdate := tx.Model(&models.BulkMessageRecipient{}).
				Where(
					"id = ? AND campaign_id = ? AND status = ? AND message_id IS NULL",
					job.RecipientID,
					job.CampaignID,
					models.MessageStatusPending,
				).
				Updates(map[string]any{
					"message_id":    message.ID,
					"error_message": "",
				})
			if recipientUpdate.Error != nil {
				return fmt.Errorf("claim campaign recipient: %w", recipientUpdate.Error)
			}
			if recipientUpdate.RowsAffected != 1 {
				return errors.New("campaign recipient changed before delivery claim")
			}

			delivery.message = &message
			return nil
		},
	)
	return delivery, err
}

func (w *Worker) attemptPreparedCampaignDelivery(
	ctx context.Context,
	job *queue.RecipientJob,
	campaign *models.BulkMessageCampaign,
	account *models.WhatsAppAccount,
	requestedContactID uuid.UUID,
	prepared campaignRecipientDelivery,
) (campaignRecipientDelivery, error) {
	delivery := prepared
	if job == nil || account == nil || prepared.message == nil ||
		prepared.mirrorConversationID == uuid.Nil {
		return delivery, errors.New("committed campaign inbox mirror is required")
	}
	providerAttempted := false
	var waMessageID string
	var sendErr error
	err := w.campaignCanonicalContactTransaction(
		ctx,
		job.OrganizationID,
		func(tx *gorm.DB, deliveryAttempted *bool) error {
			// Serialize pause/cancel with the complete provider attempt. The inbox
			// mirror is already committed, so a transition that wins this lock is
			// settled terminally rather than deleting a linked projection.
			var lockedCampaign models.BulkMessageCampaign
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Select("id", "organization_id", "status", "started_at").
				Where("id = ? AND organization_id = ?", job.CampaignID, job.OrganizationID).
				First(&lockedCampaign).Error; err != nil {
				return fmt.Errorf("lock campaign before provider delivery: %w", err)
			}
			var storedRecipient models.BulkMessageRecipient
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND campaign_id = ?", job.RecipientID, job.CampaignID).
				First(&storedRecipient).Error; err != nil {
				return fmt.Errorf("lock claimed campaign recipient: %w", err)
			}
			if storedRecipient.Status != models.MessageStatusPending ||
				storedRecipient.MessageID == nil ||
				*storedRecipient.MessageID != prepared.message.ID {
				delivery.alreadyProcessed = true
				return nil
			}

			var lockedAccount *models.WhatsAppAccount
			var accountGuardErr error
			var contact *models.Contact
			if campaignJobHasCurrentGeneration(&lockedCampaign, job) {
				// Keep WhatsAppAccount before contact/message in the established
				// outbound lock order. This provider-only phase never reacquires
				// ChannelAccount authority from the committed mirror phase.
				lockedAccount, accountGuardErr = whatsappaccount.LockAndLoadActiveForOutbound(
					tx,
					job.OrganizationID,
					account.ID,
				)
				if accountGuardErr != nil &&
					!errors.Is(accountGuardErr, whatsappaccount.ErrOutboundInactive) {
					return accountGuardErr
				}
				if accountGuardErr == nil {
					w.decryptAccountSecrets(lockedAccount)
				}

				var err error
				contact, err = contactutil.ResolveCanonicalContactForUpdate(
					tx,
					job.OrganizationID,
					requestedContactID,
				)
				if err != nil {
					return fmt.Errorf("resolve canonical claimed contact: %w", err)
				}
				delivery.contactID = contact.ID
			}

			var message models.Message
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND organization_id = ?", prepared.message.ID, job.OrganizationID).
				First(&message).Error; err != nil {
				return fmt.Errorf("lock claimed campaign message: %w", err)
			}
			if message.Status != models.MessageStatusPending {
				delivery.message = &message
				delivery.alreadyProcessed = true
				return nil
			}
			if message.InboxConversationID == nil ||
				*message.InboxConversationID != prepared.mirrorConversationID {
				return errors.New("committed campaign inbox mirror changed before provider delivery")
			}

			if !campaignJobHasCurrentGeneration(&lockedCampaign, job) {
				if err := finalizePreparedCampaignDeliveryTx(
					tx,
					job,
					&message,
					models.MessageStatusFailed,
					"",
					campaignInactiveBeforeDeliveryMessage,
					"failed_count",
				); err != nil {
					return err
				}
				message.Status = models.MessageStatusFailed
				message.ErrorMessage = campaignInactiveBeforeDeliveryMessage
				delivery.message = &message
				delivery.campaignInactive = true
				return nil
			}

			// Repointing after the independent mirror commit would make the
			// provider request authoritative for a projection that was never
			// committed. Fail closed and let redelivery dead-letter the claim.
			if message.ContactID != contact.ID {
				return errors.New("campaign contact changed after committed inbox mirror")
			}

			if accountGuardErr != nil {
				if err := finalizePreparedCampaignDeliveryTx(
					tx,
					job,
					&message,
					models.MessageStatusFailed,
					"",
					whatsappaccount.ErrOutboundInactive.Error(),
					"failed_count",
				); err != nil {
					return err
				}
				message.Status = models.MessageStatusFailed
				message.ErrorMessage = whatsappaccount.ErrOutboundInactive.Error()
				delivery.message = &message
				delivery.sendErr = whatsappaccount.ErrOutboundInactive
				return nil
			}

			if strings.EqualFold(campaign.Template.Category, "MARKETING") &&
				contact.MarketingOptOut {
				if err := finalizePreparedCampaignDeliveryTx(
					tx,
					job,
					&message,
					models.MessageStatusFailed,
					"",
					campaignMarketingOptOutMessage,
					"failed_count",
				); err != nil {
					return err
				}
				message.Status = models.MessageStatusFailed
				message.ErrorMessage = campaignMarketingOptOutMessage
				delivery.message = &message
				delivery.consentRejected = true
				return nil
			}

			recipient := &models.BulkMessageRecipient{
				PhoneNumber:    contact.PhoneNumber,
				RecipientName:  job.RecipientName,
				TemplateParams: job.TemplateParams,
				HeaderParams:   job.HeaderParams,
			}
			*deliveryAttempted = true
			providerAttempted = true
			waMessageID, sendErr = w.sendCampaignTemplateMessage(
				ctx,
				lockedAccount,
				campaign.Template,
				recipient,
				campaign.HeaderMediaID,
				campaign.HeaderMediaFilename,
			)
			waMessageID = strings.TrimSpace(waMessageID)
			if sendErr == nil && waMessageID == "" {
				sendErr = errors.New(campaignAmbiguousDeliveryMessage)
			}

			status := models.MessageStatusSent
			counter := "sent_count"
			errorMessage := ""
			if sendErr != nil {
				status = models.MessageStatusFailed
				counter = "failed_count"
				errorMessage = sendErr.Error()
			}
			if err := finalizePreparedCampaignDeliveryTx(
				tx,
				job,
				&message,
				status,
				waMessageID,
				errorMessage,
				counter,
			); err != nil {
				return err
			}
			message.Status = status
			message.WhatsAppMessageID = waMessageID
			message.ErrorMessage = errorMessage
			delivery.message = &message
			delivery.sendErr = sendErr
			return nil
		},
	)
	if err == nil {
		return delivery, nil
	}
	if !providerAttempted {
		// The inbox mirror is already independently committed. Retain the exact
		// claim on an unexpected pre-provider error; a redelivery will settle it
		// ambiguous instead of risking a send after a crash window.
		return delivery, err
	}

	recoveryCtx, cancel := context.WithTimeout(context.Background(), campaignDurableSettlementTimeout)
	defer cancel()
	recovered, recoveryErr := w.recoverCampaignDelivery(
		recoveryCtx,
		job,
		prepared.message.ID,
		waMessageID,
		sendErr,
	)
	if recoveryErr != nil {
		return delivery, fmt.Errorf(
			"persist provider-attempt result after transaction failure: %w (original error: %v)",
			recoveryErr,
			err,
		)
	}
	return recovered, nil
}

func (w *Worker) recoverCampaignDelivery(
	ctx context.Context,
	job *queue.RecipientJob,
	messageID uuid.UUID,
	waMessageID string,
	sendErr error,
) (campaignRecipientDelivery, error) {
	var delivery campaignRecipientDelivery
	err := w.withRecipientTenantTransaction(
		ctx,
		job.OrganizationID,
		func(tx *gorm.DB) error {
			var recipient models.BulkMessageRecipient
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND campaign_id = ?", job.RecipientID, job.CampaignID).
				First(&recipient).Error; err != nil {
				return err
			}
			var message models.Message
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND organization_id = ?", messageID, job.OrganizationID).
				First(&message).Error; err != nil {
				return err
			}
			delivery.message = &message
			delivery.contactID = message.ContactID
			if recipient.Status != models.MessageStatusPending {
				delivery.alreadyProcessed = true
				if message.Status == models.MessageStatusFailed && message.ErrorMessage != "" {
					delivery.sendErr = errors.New(message.ErrorMessage)
				}
				return nil
			}
			if recipient.MessageID == nil || *recipient.MessageID != messageID {
				return errors.New("campaign delivery claim changed before recovery")
			}

			status := models.MessageStatusSent
			counter := "sent_count"
			errorMessage := ""
			if sendErr != nil {
				status = models.MessageStatusFailed
				counter = "failed_count"
				errorMessage = sendErr.Error()
			}
			if err := finalizePreparedCampaignDeliveryTx(
				tx,
				job,
				&message,
				status,
				waMessageID,
				errorMessage,
				counter,
			); err != nil {
				return err
			}
			message.Status = status
			message.WhatsAppMessageID = waMessageID
			message.ErrorMessage = errorMessage
			delivery.message = &message
			delivery.sendErr = sendErr
			return nil
		},
	)
	return delivery, err
}

func (w *Worker) releasePreparedCampaignDelivery(
	ctx context.Context,
	job *queue.RecipientJob,
	messageID uuid.UUID,
) error {
	return w.withRecipientTenantTransaction(ctx, job.OrganizationID, func(tx *gorm.DB) error {
		return releasePreparedCampaignDeliveryTx(tx, job, messageID)
	})
}

// releasePreparedCampaignDeliveryTx removes only a provably unattempted claim.
// A linked message or provider ID fails closed because either can indicate that
// the side-effect boundary was crossed.
func releasePreparedCampaignDeliveryTx(
	tx *gorm.DB,
	job *queue.RecipientJob,
	messageID uuid.UUID,
) error {
	var recipient models.BulkMessageRecipient
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND campaign_id = ?", job.RecipientID, job.CampaignID).
		First(&recipient).Error; err != nil {
		return fmt.Errorf("lock unattempted campaign recipient: %w", err)
	}
	if recipient.Status != models.MessageStatusPending ||
		recipient.MessageID == nil || *recipient.MessageID != messageID {
		return errors.New("unattempted campaign delivery claim changed")
	}

	var message models.Message
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", messageID, job.OrganizationID).
		First(&message).Error; err != nil {
		return fmt.Errorf("lock unattempted campaign message: %w", err)
	}
	if message.Status != models.MessageStatusPending ||
		strings.TrimSpace(message.WhatsAppMessageID) != "" ||
		message.InboxConversationID != nil {
		return errors.New("campaign delivery may already have crossed the provider boundary")
	}

	result := tx.Model(&models.BulkMessageRecipient{}).
		Where(
			"id = ? AND campaign_id = ? AND status = ? AND message_id = ?",
			job.RecipientID,
			job.CampaignID,
			models.MessageStatusPending,
			messageID,
		).
		Updates(map[string]any{
			"message_id":           nil,
			"whats_app_message_id": "",
			"error_message":        "",
			"sent_at":              nil,
		})
	if result.Error != nil {
		return fmt.Errorf("release unattempted campaign recipient: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return errors.New("unattempted campaign recipient changed before release")
	}

	deleted := tx.Unscoped().
		Where("id = ? AND organization_id = ?", messageID, job.OrganizationID).
		Delete(&models.Message{})
	if deleted.Error != nil {
		return fmt.Errorf("delete unattempted campaign message: %w", deleted.Error)
	}
	if deleted.RowsAffected != 1 {
		return errors.New("unattempted campaign message changed before deletion")
	}
	return nil
}

func finalizePreparedCampaignDeliveryTx(
	tx *gorm.DB,
	job *queue.RecipientJob,
	message *models.Message,
	status models.MessageStatus,
	waMessageID, errorMessage, counter string,
) error {
	messageUpdate := tx.Model(&models.Message{}).
		Where("id = ? AND organization_id = ?", message.ID, job.OrganizationID).
		Updates(map[string]any{
			"whats_app_message_id": waMessageID,
			"status":               status,
			"error_message":        errorMessage,
		})
	if messageUpdate.Error != nil {
		return fmt.Errorf("finalize campaign message: %w", messageUpdate.Error)
	}
	if messageUpdate.RowsAffected != 1 {
		return errors.New("pending campaign message changed before finalization")
	}

	var sentAt any
	if status == models.MessageStatusSent {
		sentAt = time.Now().UTC()
	}
	recipientUpdate := tx.Model(&models.BulkMessageRecipient{}).
		Where(
			"id = ? AND campaign_id = ? AND status = ? AND message_id = ?",
			job.RecipientID,
			job.CampaignID,
			models.MessageStatusPending,
			message.ID,
		).
		Updates(map[string]any{
			"status":               status,
			"whats_app_message_id": waMessageID,
			"error_message":        errorMessage,
			"sent_at":              sentAt,
		})
	if recipientUpdate.Error != nil {
		return fmt.Errorf("finalize campaign recipient: %w", recipientUpdate.Error)
	}
	if recipientUpdate.RowsAffected != 1 {
		return errors.New("campaign recipient changed before finalization")
	}
	return incrementCampaignCountTx(
		tx,
		job.OrganizationID,
		job.CampaignID,
		counter,
	)
}

// failCampaignRecipientBeforeClaimTx settles only the pending recipient bound
// to this exact campaign. Queue payload IDs are not trusted across tenants, and
// a duplicate terminal job must not increment the campaign twice.
func failCampaignRecipientBeforeClaimTx(
	tx *gorm.DB,
	job *queue.RecipientJob,
	errorMessage string,
) error {
	result := tx.Model(&models.BulkMessageRecipient{}).
		Where(
			"id = ? AND campaign_id = ? AND status = ? AND message_id IS NULL",
			job.RecipientID,
			job.CampaignID,
			models.MessageStatusPending,
		).
		Updates(map[string]any{
			"status":               models.MessageStatusFailed,
			"whats_app_message_id": "",
			"error_message":        errorMessage,
		})
	if result.Error != nil {
		return fmt.Errorf("fail campaign recipient before claim: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil
	}
	return incrementCampaignCountTx(
		tx,
		job.OrganizationID,
		job.CampaignID,
		"failed_count",
	)
}

func campaignMessage(
	job *queue.RecipientJob,
	campaign *models.BulkMessageCampaign,
	contactID uuid.UUID,
) models.Message {
	message := models.Message{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  job.OrganizationID,
		WhatsAppAccount: campaign.WhatsAppAccount,
		ContactID:       contactID,
		Direction:       models.DirectionOutgoing,
		MessageType:     models.MessageTypeTemplate,
		Status:          models.MessageStatusPending,
		TemplateName:    campaign.Template.Name,
		TemplateParams:  job.TemplateParams,
		Content: templateutil.ReplaceWithJSONBParams(
			campaign.Template.BodyContent,
			campaign.Template.BodyContent,
			job.TemplateParams,
		),
		Metadata: models.JSONB{
			"campaign_id":    job.CampaignID.String(),
			"recipient_name": job.RecipientName,
		},
	}
	if campaign.HeaderMediaLocalPath != "" {
		message.MediaURL = campaign.HeaderMediaLocalPath
		message.MediaMimeType = campaign.HeaderMediaMimeType
	}
	return message
}

func incrementCampaignCountTx(
	tx *gorm.DB,
	organizationID, campaignID uuid.UUID,
	column string,
) error {
	result := tx.Model(&models.BulkMessageCampaign{}).
		Where("id = ? AND organization_id = ?", campaignID, organizationID).
		Update(column, gorm.Expr(column+" + 1"))
	if result.Error != nil {
		return fmt.Errorf("increment campaign %s: %w", column, result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("increment campaign %s: campaign was not found", column)
	}
	return nil
}

func (w *Worker) campaignCanonicalContactTransaction(
	ctx context.Context,
	organizationID uuid.UUID,
	write func(tx *gorm.DB, deliveryAttempted *bool) error,
) error {
	var err error
	for attempt := 0; attempt < campaignCanonicalContactAttempts; attempt++ {
		deliveryAttempted := false
		err = w.withRecipientTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
			return write(tx, &deliveryAttempted)
		})
		if err == nil || deliveryAttempted || !retryableCampaignContactWrite(err) {
			return err
		}
	}
	return err
}

// withRecipientTenantTransaction always begins a top-level phase from the
// worker pool. Under RLS this gives prepare, provider-attempt, recovery and
// finish independent commits instead of savepoints inside one tenant wrapper.
func (w *Worker) withRecipientTenantTransaction(
	ctx context.Context,
	organizationID uuid.UUID,
	write func(tx *gorm.DB) error,
) error {
	db := w.DB.WithContext(ctx)
	if w.Config != nil && w.Config.Database.RLSEnabled {
		return database.WithTenant(db, organizationID, write)
	}
	return db.Transaction(write)
}

// withRecipientTenantTransactionAndCampaignStats publishes the transaction's
// campaign projection only after a successful commit. A failed callback or
// commit discards the captured projection together with the database changes.
func (w *Worker) withRecipientTenantTransactionAndCampaignStats(
	ctx context.Context,
	organizationID uuid.UUID,
	write func(tx *gorm.DB) (*queue.CampaignStatsUpdate, error),
) error {
	var update *queue.CampaignStatsUpdate
	if err := w.withRecipientTenantTransaction(ctx, organizationID, func(tx *gorm.DB) error {
		var err error
		update, err = write(tx)
		return err
	}); err != nil {
		return err
	}
	w.publishCampaignStats(ctx, update)
	return nil
}

func retryableCampaignContactWrite(err error) bool {
	if errors.Is(err, contactutil.ErrCanonicalContactChanged) {
		return true
	}
	var sqlState interface {
		SQLState() string
	}
	if !errors.As(err, &sqlState) {
		return false
	}
	switch sqlState.SQLState() {
	case "40001", "40P01":
		return true
	default:
		return false
	}
}

// updateRecipientStatus updates the recipient's status in the database
func (w *Worker) updateRecipientStatus(recipientID uuid.UUID, status models.MessageStatus, waMessageID, errorMsg string) {
	updates := map[string]any{
		"status":               status,
		"whats_app_message_id": waMessageID,
	}
	if status == models.MessageStatusSent {
		updates["sent_at"] = time.Now()
	}
	if errorMsg != "" {
		updates["error_message"] = errorMsg
	}
	w.DB.Model(&models.BulkMessageRecipient{}).Where("id = ?", recipientID).Updates(updates)
}

// incrementCampaignCount increments a campaign counter atomically
func (w *Worker) incrementCampaignCount(campaignID uuid.UUID, column string) {
	w.DB.Model(&models.BulkMessageCampaign{}).
		Where("id = ?", campaignID).
		Update(column, gorm.Expr(column+" + 1"))
}

// publishCampaignStats publishes a committed campaign projection for real-time
// updates.
func (w *Worker) publishCampaignStats(ctx context.Context, update *queue.CampaignStatsUpdate) {
	if w.Publisher == nil || update == nil {
		return
	}
	_ = w.Publisher.PublishCampaignStats(ctx, update)
}

// checkCampaignCompletion checks if all recipients are processed and returns
// the transaction's campaign projection. The caller owns publication after
// commit.
func (w *Worker) checkCampaignCompletion(_ context.Context, campaignID, organizationID uuid.UUID) (*queue.CampaignStatsUpdate, error) {
	// Count pending recipients
	var pendingCount int64
	if err := w.DB.Model(&models.BulkMessageRecipient{}).
		Where("campaign_id = ? AND status = ?", campaignID, models.MessageStatusPending).
		Count(&pendingCount).Error; err != nil {
		return nil, fmt.Errorf("count pending campaign recipients: %w", err)
	}

	// If no pending recipients, mark campaign as completed
	if pendingCount == 0 {
		var campaign models.BulkMessageCampaign
		if err := w.DB.Where("id = ? AND organization_id = ?", campaignID, organizationID).
			First(&campaign).Error; err != nil {
			return nil, fmt.Errorf("load campaign before completion: %w", err)
		}

		// Only complete if currently processing
		if campaign.Status != models.CampaignStatusProcessing {
			return nil, nil
		}

		now := time.Now().UTC()
		updated := w.DB.Model(&models.BulkMessageCampaign{}).
			Where(
				"id = ? AND organization_id = ? AND status = ?",
				campaignID,
				organizationID,
				models.CampaignStatusProcessing,
			).
			Updates(map[string]any{
				"status":       models.CampaignStatusCompleted,
				"completed_at": now,
			})
		if updated.Error != nil {
			return nil, fmt.Errorf("complete campaign: %w", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return nil, errors.New("campaign changed before completion")
		}
		if err := w.DB.Where("id = ? AND organization_id = ?", campaignID, organizationID).
			First(&campaign).Error; err != nil {
			return nil, fmt.Errorf("reload completed campaign: %w", err)
		}
		if campaign.Status != models.CampaignStatusCompleted || campaign.CompletedAt == nil {
			return nil, errors.New("completed campaign projection is inconsistent")
		}

		w.Log.Info("Campaign completed", "campaign_id", campaignID, "sent", campaign.SentCount, "failed", campaign.FailedCount)

		return &queue.CampaignStatsUpdate{
			CampaignID:     campaignID.String(),
			OrganizationID: organizationID,
			Status:         campaign.Status,
			SentCount:      campaign.SentCount,
			DeliveredCount: campaign.DeliveredCount,
			ReadCount:      campaign.ReadCount,
			FailedCount:    campaign.FailedCount,
		}, nil
	}

	var campaign models.BulkMessageCampaign
	if err := w.DB.Where("id = ? AND organization_id = ?", campaignID, organizationID).
		First(&campaign).Error; err != nil {
		return nil, fmt.Errorf("load active campaign stats: %w", err)
	}
	return &queue.CampaignStatsUpdate{
		CampaignID:     campaignID.String(),
		OrganizationID: organizationID,
		Status:         campaign.Status,
		SentCount:      campaign.SentCount,
		DeliveredCount: campaign.DeliveredCount,
		ReadCount:      campaign.ReadCount,
		FailedCount:    campaign.FailedCount,
	}, nil
}

// sendCampaignTemplateMessage contains provider panics at the same durable
// no-resend boundary as an ambiguous transport result. Panic payloads are not
// copied into logs or persisted errors because they may contain provider data.
func (w *Worker) sendCampaignTemplateMessage(
	ctx context.Context,
	account *models.WhatsAppAccount,
	template *models.Template,
	recipient *models.BulkMessageRecipient,
	campaignHeaderMediaID, campaignHeaderMediaFilename string,
) (messageID string, sendErr error) {
	defer func() {
		if recover() != nil {
			messageID = ""
			sendErr = errors.New(campaignAmbiguousDeliveryMessage)
		}
	}()
	return w.sendTemplateMessage(
		ctx,
		account,
		template,
		recipient,
		campaignHeaderMediaID,
		campaignHeaderMediaFilename,
	)
}

// sendTemplateMessage sends a template message via WhatsApp Cloud API
func (w *Worker) sendTemplateMessage(ctx context.Context, account *models.WhatsAppAccount, template *models.Template, recipient *models.BulkMessageRecipient, campaignHeaderMediaID, campaignHeaderMediaFilename string) (string, error) {
	waAccount := account.ToWAAccount()

	// Resolve body parameters into a map for BuildTemplateComponents
	resolvedParams := templateutil.ResolveParams(template.BodyContent, recipient.TemplateParams)
	bodyParams := make(map[string]string, len(resolvedParams))
	paramNames := templateutil.ExtParamNames(template.BodyContent)
	for i, val := range resolvedParams {
		if i < len(paramNames) {
			bodyParams[paramNames[i]] = val
		} else {
			bodyParams[fmt.Sprintf("%d", i+1)] = val
		}
	}

	// Resolve the header text parameter (if any) into its own map. Prefer
	// recipient.HeaderParams (new path, populated by AddRecipients) and fall
	// back to a TemplateParams lookup for legacy recipient rows persisted
	// before HeaderParams existed.
	var headerParams map[string]string
	if template.HeaderType == "TEXT" {
		if hNames := templateutil.ExtParamNames(template.HeaderContent); len(hNames) == 1 {
			name := hNames[0]
			if raw, ok := recipient.HeaderParams[name]; ok {
				headerParams = map[string]string{name: fmt.Sprintf("%v", raw)}
			} else if raw, ok := recipient.TemplateParams[name]; ok {
				headerParams = map[string]string{name: fmt.Sprintf("%v", raw)}
			}
		}
	}

	// Use the shared component builder (same as chat template sending).
	components, err := whatsapp.BuildTemplateComponents(
		bodyParams,
		template.HeaderType, template.HeaderContent,
		headerParams,
		campaignHeaderMediaID, campaignHeaderMediaFilename,
	)
	if err != nil {
		return "", fmt.Errorf("failed to build template components: %w", err)
	}
	// Add auto-generated button components (Flow needs flow_token)
	flowComponents := whatsapp.AutoButtonComponents(template.Buttons)
	components = append(components, flowComponents...)

	rcpt := whatsapp.Recipient{Phone: recipient.PhoneNumber}
	return w.WhatsApp.SendTemplateMessage(ctx, waAccount, rcpt, template.Name, template.Language, components)
}

// decryptAccountSecrets decrypts the encrypted secrets on a WhatsApp account.
func (w *Worker) decryptAccountSecrets(account *models.WhatsAppAccount) {
	var key string
	if w.Config != nil {
		key = w.Config.App.EncryptionKey
	}
	account.DecryptSecrets(key)
}

// Close cleans up worker resources
func (w *Worker) Close() error {
	if w.Consumer != nil {
		return w.Consumer.Close()
	}
	return nil
}
