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
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const campaignCanonicalContactAttempts = 3

const campaignMarketingOptOutMessage = "Contact opted out of marketing messages"
const campaignAmbiguousDeliveryMessage = "Provider delivery outcome is unknown; message was not retried to prevent a duplicate"

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
		scoped := *w
		scoped.DB = tx

		if err := tx.
			Where("id = ? AND organization_id = ?", job.CampaignID, job.OrganizationID).
			Preload("Template", "organization_id = ?", job.OrganizationID).
			First(&campaign).Error; err != nil {
			w.Log.Error("Failed to load campaign", "error", err, "campaign_id", job.CampaignID)
			return fmt.Errorf("failed to load campaign: %w", err)
		}
		if campaign.Status == models.CampaignStatusPaused ||
			campaign.Status == models.CampaignStatusCancelled {
			w.Log.Info(
				"Campaign not active, skipping recipient",
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
			scoped.updateRecipientStatus(job.RecipientID, models.MessageStatusFailed, "", "WhatsApp account not found")
			scoped.incrementCampaignCount(job.CampaignID, "failed_count")
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
			scoped.updateRecipientStatus(job.RecipientID, models.MessageStatusFailed, "", "Failed to create contact")
			scoped.incrementCampaignCount(job.CampaignID, "failed_count")
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
	if delivery.alreadyProcessed {
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

	if err := w.withRecipientTenantTransaction(ctx, job.OrganizationID, func(tx *gorm.DB) error {
		scoped := *w
		scoped.DB = tx
		// The durable legacy message is authoritative. The idempotent
		// migration backfill can repair a transient omnichannel mirror failure
		// without resending.
		if delivery.message != nil {
			if _, err := channelapi.MirrorLegacyWhatsAppMessage(
				tx,
				channelapi.LegacyMetaAccountRef{
					ID:             account.ID,
					OrganizationID: account.OrganizationID,
					Name:           account.Name,
					Status:         account.Status,
				},
				delivery.message.ID,
			); err != nil {
				w.Log.Error(
					"Failed to mirror campaign message into omnichannel inbox",
					"error", err,
					"organization_id", account.OrganizationID,
					"message_id", delivery.message.ID,
				)
			}
		}
		scoped.checkCampaignCompletion(ctx, job.CampaignID, job.OrganizationID)
		return nil
	}); err != nil {
		return fmt.Errorf("finalize campaign recipient tenant phase: %w", err)
	}

	return nil
}

type campaignRecipientDelivery struct {
	message          *models.Message
	contactID        uuid.UUID
	sendErr          error
	consentRejected  bool
	ambiguous        bool
	alreadyProcessed bool
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
	return w.attemptPreparedCampaignDelivery(ctx, job, campaign, account, requestedContactID, delivery)
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
	providerAttempted := false
	var waMessageID string
	var sendErr error
	err := w.campaignCanonicalContactTransaction(
		ctx,
		job.OrganizationID,
		func(tx *gorm.DB, deliveryAttempted *bool) error {
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

			contact, err := contactutil.ResolveCanonicalContactForUpdate(
				tx,
				job.OrganizationID,
				requestedContactID,
			)
			if err != nil {
				return fmt.Errorf("resolve canonical claimed contact: %w", err)
			}
			delivery.contactID = contact.ID

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
			if message.ContactID != contact.ID {
				if err := tx.Model(&models.Message{}).
					Where("id = ? AND organization_id = ?", message.ID, job.OrganizationID).
					Update("contact_id", contact.ID).Error; err != nil {
					return fmt.Errorf("move claimed campaign message to canonical contact: %w", err)
				}
				message.ContactID = contact.ID
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
			waMessageID, sendErr = w.sendTemplateMessage(
				ctx,
				account,
				campaign.Template,
				recipient,
				campaign.HeaderMediaID,
				campaign.HeaderMediaFilename,
			)

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
		return delivery, err
	}

	recovered, recoveryErr := w.recoverCampaignDelivery(
		ctx,
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

// publishCampaignStats publishes campaign stats for real-time updates
func (w *Worker) publishCampaignStats(ctx context.Context, campaignID, organizationID uuid.UUID) {
	if w.Publisher == nil {
		return
	}
	var campaign models.BulkMessageCampaign
	if err := w.DB.Where("id = ?", campaignID).First(&campaign).Error; err != nil {
		return
	}

	_ = w.Publisher.PublishCampaignStats(ctx, &queue.CampaignStatsUpdate{
		CampaignID:     campaignID.String(),
		OrganizationID: organizationID,
		Status:         campaign.Status,
		SentCount:      campaign.SentCount,
		DeliveredCount: campaign.DeliveredCount,
		ReadCount:      campaign.ReadCount,
		FailedCount:    campaign.FailedCount,
	})
}

// checkCampaignCompletion checks if all recipients are processed and marks campaign as completed
func (w *Worker) checkCampaignCompletion(ctx context.Context, campaignID, organizationID uuid.UUID) {
	// Count pending recipients
	var pendingCount int64
	w.DB.Model(&models.BulkMessageRecipient{}).
		Where("campaign_id = ? AND status = ?", campaignID, models.MessageStatusPending).
		Count(&pendingCount)

	// If no pending recipients, mark campaign as completed
	if pendingCount == 0 {
		var campaign models.BulkMessageCampaign
		if err := w.DB.Where("id = ?", campaignID).First(&campaign).Error; err != nil {
			return
		}

		// Only complete if currently processing
		if campaign.Status != models.CampaignStatusProcessing {
			return
		}

		now := time.Now()
		w.DB.Model(&campaign).Updates(map[string]any{
			"status":       models.CampaignStatusCompleted,
			"completed_at": now,
		})

		w.Log.Info("Campaign completed", "campaign_id", campaignID, "sent", campaign.SentCount, "failed", campaign.FailedCount)

		// Publish completion status
		if w.Publisher != nil {
			_ = w.Publisher.PublishCampaignStats(ctx, &queue.CampaignStatsUpdate{
				CampaignID:     campaignID.String(),
				OrganizationID: organizationID,
				Status:         models.CampaignStatusCompleted,
				SentCount:      campaign.SentCount,
				DeliveredCount: campaign.DeliveredCount,
				ReadCount:      campaign.ReadCount,
				FailedCount:    campaign.FailedCount,
			})
		}
	} else {
		// Publish current stats
		w.publishCampaignStats(ctx, campaignID, organizationID)
	}
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
