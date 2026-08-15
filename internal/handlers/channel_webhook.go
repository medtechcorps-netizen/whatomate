package handlers

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/threadsreview"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	rawInboundProcessingLease = 2 * time.Minute
)

var (
	errThreadsWebhookBindingInactive       = errors.New("threads webhook binding is no longer active")
	errMetaInstagramWebhookBindingInactive = errors.New("managed Instagram webhook binding is no longer active")
)

// VerifyChannelWebhook handles Meta's GET subscription challenge for an
// OAuth-managed channel callback URL.
func (a *App) VerifyChannelWebhook(r *fastglue.Request) error {
	channelAccountID, err := uuid.Parse(strings.TrimSpace(pathString(r, "channel_account_id")))
	if err != nil || channelAccountID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel webhook not found", nil, "")
	}
	orgID, err := resolveChannelWebhookOrganization(a.DB, channelAccountID, a.rlsEnabled())
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel webhook not found", nil, "")
	}
	var account models.ChannelAccount
	var adapter channelapi.Adapter
	err = a.WithTenantApp(orgID, func(scoped *App) error {
		loaded, loadErr := loadChannelAccount(scoped.DB, orgID, channelAccountID, false)
		if loadErr != nil {
			return loadErr
		}
		account = *loaded
		if account.Channel == models.ChannelThreads {
			if loadErr = validateThreadsWebhookAccountBinding(
				scoped.Config,
				scoped.DB,
				&account,
				strings.TrimSpace(fmt.Sprint(account.Metadata["app_id"])),
			); loadErr != nil {
				return loadErr
			}
		}
		adapter, loadErr = scoped.channelAdapter(&account)
		return loadErr
	})
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel webhook not found", nil, "")
	}
	verifier, ok := adapter.(interface{ VerifyChallenge(string) error })
	if !ok || strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("hub.mode"))) != "subscribe" {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Webhook verification failed", nil, "")
	}
	challenge := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("hub.challenge")))
	if challenge == "" || len(challenge) > 64 {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Webhook verification failed", nil, "")
	}
	if _, err := strconv.ParseInt(challenge, 10, 64); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Webhook verification failed", nil, "")
	}
	if err := verifier.VerifyChallenge(string(r.RequestCtx.QueryArgs().Peek("hub.verify_token"))); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Webhook verification failed", nil, "")
	}
	r.RequestCtx.Response.Header.SetContentType("text/plain; charset=utf-8")
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	r.RequestCtx.Response.SetStatusCode(fasthttp.StatusOK)
	r.RequestCtx.Response.SetBodyString(challenge)
	return nil
}

// RelayChannelWebhook handles the public route:
// /api/webhooks/channels/{channel_account_id}.
//
// Tenant discovery deliberately uses a narrowly granted SECURITY DEFINER
// function so the runtime role never receives cross-tenant SELECT access.
func (a *App) RelayChannelWebhook(r *fastglue.Request) error {
	channelAccountID, err := uuid.Parse(strings.TrimSpace(pathString(r, "channel_account_id")))
	if err != nil || channelAccountID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel webhook not found", nil, "")
	}
	body := r.RequestCtx.PostBody()
	if len(body) == 0 || len(body) > channelapi.RelayWebhookMaxBodyBytes {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid webhook body", nil, "")
	}

	orgID, err := resolveChannelWebhookOrganization(a.DB, channelAccountID, a.rlsEnabled())
	if err != nil {
		a.Log.Warn(
			"Channel webhook route resolution failed",
			"error",
			err,
			"channel_account_id",
			channelAccountID,
		)
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel webhook not found", nil, "")
	}

	var account models.ChannelAccount
	if err := database.WithTenant(a.DB, orgID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		return tx.
			Preload("Credentials", func(credentials *gorm.DB) *gorm.DB {
				return credentials.
					Where(
						"organization_id = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
						orgID,
						[]models.ChannelCredentialStatus{
							models.ChannelCredentialStatusActive,
							models.ChannelCredentialStatusExpiring,
						},
						now,
					).
					Order("version DESC").
					Order("id ASC")
			}).
			Where(
				"organization_id = ? AND id = ? AND status IN ?",
				orgID,
				channelAccountID,
				[]models.ChannelAccountStatus{
					models.ChannelAccountStatusPending,
					models.ChannelAccountStatusActive,
					models.ChannelAccountStatusDegraded,
				},
			).
			First(&account).Error
	}); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel webhook not found", nil, "")
	}
	adapter, err := a.channelAdapter(&account)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel webhook not found", nil, "")
	}

	headers := relayHTTPHeaders(r)
	if err := adapter.VerifyWebhook(&account, headers, body); err != nil {
		a.Log.Warn(
			"Rejected channel webhook signature",
			"organization_id",
			orgID,
			"channel_account_id",
			account.ID,
		)
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Invalid webhook signature", nil, "")
	}
	managedInstagram := managedInstagramLoginWebhookAccount(&account)
	verifiedWebhookCredentialID := uuid.Nil
	verifiedWebhookCredentialVersion := 0
	if managedInstagram {
		verifiedWebhook := currentMetaRegistryCredential(
			account.Credentials,
			models.ChannelCredentialKindWebhook,
			time.Now().UTC(),
		)
		if verifiedWebhook == nil {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel webhook not found", nil, "")
		}
		verifiedWebhookCredentialID = verifiedWebhook.ID
		verifiedWebhookCredentialVersion = verifiedWebhook.Version
	}
	hint, err := adapter.RouteHint(headers, body)
	if err != nil || (account.Channel != models.ChannelThreads && hint.ExternalAccountID != account.ExternalAccountID) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Webhook route does not match payload", nil, "")
	}
	threadsPayloadAppID := ""
	if account.Channel == models.ChannelThreads {
		threadsPayloadAppID = strings.TrimSpace(fmt.Sprint(hint.Metadata["app_id"]))
		if err := database.WithTenant(a.DB, orgID, func(tx *gorm.DB) error {
			return validateThreadsWebhookAccountBinding(a.Config, tx, &account, threadsPayloadAppID)
		}); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Webhook route does not match the dedicated Threads app", nil, "")
		}
	}
	if validator, ok := adapter.(interface {
		ValidateWebhookAccount(*models.ChannelAccount, []byte) error
	}); ok {
		if err := validator.ValidateWebhookAccount(&account, body); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Webhook route does not match payload", nil, "")
		}
	}

	rawHash := sha256.Sum256(body)
	rawDedupeKey := "raw:" + hex.EncodeToString(rawHash[:])
	rawPayload := webhookPayloadJSON(body)
	rawEvent := models.InboundEvent{
		OrganizationID:   orgID,
		ChannelAccountID: account.ID,
		DedupeKey:        rawDedupeKey,
		EventType:        "raw_webhook",
		Status:           models.InboundEventStatusPending,
		SignatureValid:   true,
		ReceivedAt:       time.Now().UTC(),
		Headers:          safeRelayHeaders(headers),
		Payload:          rawPayload,
	}

	shouldProcess := false
	retry := false
	if err := database.WithTenant(a.DB, orgID, func(tx *gorm.DB) error {
		switch {
		case account.Channel == models.ChannelThreads:
			currentAccount, bindingErr := lockThreadsWebhookPersistenceBinding(
				a.Config,
				tx,
				orgID,
				account.ID,
				threadsPayloadAppID,
				account.ExternalAccountID,
				time.Now().UTC(),
			)
			if bindingErr != nil {
				return bindingErr
			}
			account = *currentAccount
			rawEvent.ChannelAccountID = account.ID
		case managedInstagram:
			currentAccount, bindingErr := a.lockMetaInstagramWebhookPersistenceBinding(
				tx,
				orgID,
				account.ID,
				account.ExternalAccountID,
				verifiedWebhookCredentialID,
				verifiedWebhookCredentialVersion,
				headers,
				body,
				time.Now().UTC(),
			)
			if bindingErr != nil {
				return bindingErr
			}
			account = *currentAccount
			rawEvent.ChannelAccountID = account.ID
		}
		var claimErr error
		shouldProcess, retry, claimErr = persistOrClaimRawInboundEvent(tx, &rawEvent)
		return claimErr
	}); err != nil {
		if errors.Is(err, errThreadsWebhookBindingInactive) ||
			errors.Is(err, errMetaInstagramWebhookBindingInactive) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel webhook not found", nil, "")
		}
		a.Log.Error("Failed to persist channel webhook", "error", err, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to accept webhook", nil, "")
	}
	if !shouldProcess {
		if rawInboundEventLeaseActive(rawEvent.Status) {
			// A concurrent delivery is still holding the processing lease. A
			// non-2xx response ensures the provider retries if that process
			// crashes after the durable raw-event commit.
			r.RequestCtx.Response.Header.Set("Retry-After", "5")
			return r.SendErrorEnvelope(
				fasthttp.StatusServiceUnavailable,
				"Webhook processing is already in progress",
				nil,
				"",
			)
		}
		return r.SendEnvelope(map[string]any{
			"accepted":  true,
			"duplicate": true,
		})
	}

	entitled := false
	entitlementErr := database.WithTenant(a.DB, orgID, func(tx *gorm.DB) error {
		var evaluateErr error
		entitled, evaluateErr = channelapi.HasDurableOmnichannelEntitlement(
			tx,
			orgID,
			time.Now().UTC(),
		)
		return evaluateErr
	})
	if entitlementErr != nil {
		markInboundEventFailed(
			a.DB,
			orgID,
			rawEvent.ID,
			"entitlement_check_failed",
			entitlementErr,
		)
		a.Log.Error(
			"Failed to evaluate channel webhook entitlement",
			"error",
			entitlementErr,
			"organization_id",
			orgID,
			"channel_account_id",
			account.ID,
		)
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Webhook entitlement could not be evaluated",
			nil,
			"",
		)
	}
	if !entitled {
		if err := markInboundEventIgnored(
			a.DB,
			orgID,
			rawEvent.ID,
			"omnichannel_not_entitled",
			"Verified webhook quarantined because omnichannel is not entitled",
		); err != nil {
			a.Log.Error(
				"Failed to discard unlicensed channel webhook",
				"error",
				err,
				"organization_id",
				orgID,
				"channel_account_id",
				account.ID,
				"inbound_event_id",
				rawEvent.ID,
			)
			return r.SendErrorEnvelope(
				fasthttp.StatusServiceUnavailable,
				"Webhook could not be safely discarded",
				nil,
				"",
			)
		}
		return r.SendEnvelope(map[string]any{
			"accepted":  true,
			"duplicate": false,
			"discarded": true,
			"reason":    "omnichannel_not_entitled",
		})
	}
	if account.Channel == models.ChannelThreads {
		threadsEntitled := false
		threadsEntitlementErr := a.WithTenantApp(orgID, func(scoped *App) error {
			decision, evaluateErr := scoped.EvaluateProductEntitlement(
				uuid.Nil,
				orgID,
				channelapi.ThreadsPublicEngagementEntitlementKey,
			)
			if evaluateErr == nil {
				threadsEntitled = decision.Allowed
			}
			return evaluateErr
		})
		if threadsEntitlementErr != nil {
			markInboundEventFailed(a.DB, orgID, rawEvent.ID, "threads_entitlement_check_failed", threadsEntitlementErr)
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Threads entitlement could not be evaluated", nil, "")
		}
		if !threadsEntitled {
			if err := markInboundEventIgnored(
				a.DB,
				orgID,
				rawEvent.ID,
				"threads_not_entitled",
				"Verified Threads webhook quarantined because public engagement is not entitled",
			); err != nil {
				return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Webhook could not be safely discarded", nil, "")
			}
			return r.SendEnvelope(map[string]any{
				"accepted":  true,
				"duplicate": false,
				"discarded": true,
				"reason":    "threads_not_entitled",
			})
		}
	}

	events, normalizeErr := adapter.NormalizeWebhook(requestContext(r), &account, body)
	if normalizeErr != nil {
		markInboundEventFailed(a.DB, orgID, rawEvent.ID, "normalize_failed", normalizeErr)
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Webhook payload could not be normalized", nil, "")
	}

	processErr := database.WithTenant(a.DB, orgID, func(tx *gorm.DB) error {
		var currentAccount *models.ChannelAccount
		if managedInstagram {
			// The durable raw event and canonical inbox writes are separate
			// transactions. Re-run the exact generation/signature fence at this
			// second boundary so a downgrade between them cannot persist contacts,
			// conversations, messages, or AI work.
			var bindingErr error
			currentAccount, bindingErr = a.lockMetaInstagramWebhookPersistenceBinding(
				tx,
				orgID,
				account.ID,
				account.ExternalAccountID,
				verifiedWebhookCredentialID,
				verifiedWebhookCredentialVersion,
				headers,
				body,
				time.Now().UTC(),
			)
			if bindingErr != nil {
				return bindingErr
			}
		} else {
			// AI scheduling and every control-plane change share this tenant
			// mutex. Acquire it before touching account, identity, contact,
			// conversation, or message rows so no later lock is held while
			// waiting for the mutex.
			if err := lockChannelAIOrganizationScopeTx(tx, orgID); err != nil {
				return err
			}
			var loadErr error
			currentAccount, loadErr = loadChannelAccount(tx, orgID, account.ID, false)
			if loadErr != nil {
				return loadErr
			}
		}
		account = *currentAccount
		for i := range events {
			if err := processNormalizedChannelEvent(tx, &account, &events[i], rawEvent.ReceivedAt); err != nil {
				return fmt.Errorf("process normalized event %d: %w", i, err)
			}
		}
		now := time.Now().UTC()
		if err := tx.Model(&models.InboundEvent{}).
			Where("id = ? AND organization_id = ?", rawEvent.ID, orgID).
			Updates(map[string]any{
				"status":       models.InboundEventStatusProcessed,
				"processed_at": now,
				"updated_at":   now,
			}).Error; err != nil {
			return err
		}
		return tx.Model(&models.ChannelAccount{}).
			Where("id = ? AND organization_id = ?", account.ID, orgID).
			Updates(map[string]any{
				"last_inbound_at": now,
				"updated_at":      now,
			}).Error
	})
	if processErr != nil {
		if errors.Is(processErr, errMetaInstagramWebhookBindingInactive) {
			if err := markInboundEventIgnored(
				a.DB,
				orgID,
				rawEvent.ID,
				"managed_instagram_binding_inactive",
				"Verified webhook quarantined because the managed Instagram binding changed before canonical persistence",
			); err != nil {
				a.Log.Error(
					"Failed to quarantine managed Instagram webhook",
					"error",
					err,
					"organization_id",
					orgID,
					"channel_account_id",
					account.ID,
					"inbound_event_id",
					rawEvent.ID,
				)
				return r.SendErrorEnvelope(
					fasthttp.StatusServiceUnavailable,
					"Webhook could not be safely quarantined",
					nil,
					"",
				)
			}
			return r.SendEnvelope(map[string]any{
				"accepted":  true,
				"duplicate": false,
				"discarded": true,
				"reason":    "managed_instagram_binding_inactive",
			})
		}
		markInboundEventFailed(a.DB, orgID, rawEvent.ID, "processing_failed", processErr)
		a.Log.Error(
			"Failed to process channel webhook",
			"error",
			processErr,
			"organization_id",
			orgID,
			"channel_account_id",
			account.ID,
			"inbound_event_id",
			rawEvent.ID,
		)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Webhook processing failed", nil, "")
	}
	if a.WSHub != nil {
		// The event carries no provider payload. It only asks authenticated
		// clients in this tenant to catch up from the canonical API.
		a.WSHub.BroadcastToOrg(orgID, appwebsocket.WSMessage{
			Type: "channel_sync",
			Payload: map[string]any{
				"channel_account_id": account.ID,
				"event_count":        len(events),
				"occurred_at":        time.Now().UTC(),
			},
		})
	}

	return r.SendEnvelope(map[string]any{
		"accepted":    true,
		"duplicate":   false,
		"retry":       retry,
		"event_count": len(events),
	})
}

func validateThreadsWebhookAccountBinding(
	cfg *config.Config,
	tx *gorm.DB,
	account *models.ChannelAccount,
	payloadAppID string,
) error {
	if tx == nil || account == nil || account.OrganizationID == uuid.Nil || account.ID == uuid.Nil ||
		account.Channel != models.ChannelThreads ||
		!strings.EqualFold(account.Provider, channelapi.ThreadsProvider) {
		return errors.New("threads webhook account binding is invalid")
	}
	payloadAppID = strings.TrimSpace(payloadAppID)
	accountAppID := strings.TrimSpace(fmt.Sprint(account.Metadata["app_id"]))
	if payloadAppID == "" || accountAppID == "" || payloadAppID != accountAppID {
		return errors.New("threads webhook app does not match the connected account")
	}

	var integration models.ProviderIntegration
	if err := tx.Where(
		"organization_id = ? AND provider = ? AND enabled = ?",
		account.OrganizationID,
		integrationProviderThreads,
		true,
	).First(&integration).Error; err != nil {
		return err
	}
	if integration.ThreadsAppID == nil || strings.TrimSpace(*integration.ThreadsAppID) != payloadAppID {
		return errors.New("threads webhook app is not bound to this workspace")
	}
	if threadsreview.AccessMode(
		cfg,
		account.OrganizationID,
		integration.Config,
		payloadAppID,
		account.ExternalAccountID,
	) == threadsreview.ModeBlocked {
		return errThreadsWebhookBindingInactive
	}

	var accountCount int64
	if err := tx.Model(&models.ChannelAccount{}).
		Where(
			"organization_id = ? AND channel = ? AND provider = ?",
			account.OrganizationID,
			models.ChannelThreads,
			channelapi.ThreadsProvider,
		).
		Count(&accountCount).Error; err != nil {
		return err
	}
	if accountCount != 1 {
		return errors.New("threads app must be bound to exactly one channel account")
	}
	return nil
}

// lockThreadsWebhookPersistenceBinding is the final authorization boundary
// before a signed Threads payload becomes durable. Integration mutations take
// locks in integration -> account -> credential order, so using the same order
// here makes disconnect, app rotation, OAuth rotation, and webhook acceptance
// linearizable without holding a database lock during signature verification.
func lockThreadsWebhookPersistenceBinding(
	cfg *config.Config,
	tx *gorm.DB,
	organizationID, accountID uuid.UUID,
	payloadAppID, expectedExternalAccountID string,
	now time.Time,
) (*models.ChannelAccount, error) {
	if tx == nil || organizationID == uuid.Nil || accountID == uuid.Nil {
		return nil, errThreadsWebhookBindingInactive
	}
	payloadAppID = strings.TrimSpace(payloadAppID)
	expectedExternalAccountID = strings.TrimSpace(expectedExternalAccountID)
	if payloadAppID == "" || expectedExternalAccountID == "" {
		return nil, errThreadsWebhookBindingInactive
	}

	var integration models.ProviderIntegration
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND provider = ? AND enabled = ?",
			organizationID,
			integrationProviderThreads,
			true,
		).
		First(&integration).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errThreadsWebhookBindingInactive
		}
		return nil, err
	}
	configuredAppID := stringConfigValue(integration.Config, "app_id")
	if integration.ThreadsAppID == nil || configuredAppID != payloadAppID ||
		strings.TrimSpace(*integration.ThreadsAppID) != payloadAppID {
		return nil, errThreadsWebhookBindingInactive
	}
	if threadsreview.AccessMode(
		cfg,
		organizationID,
		integration.Config,
		payloadAppID,
		expectedExternalAccountID,
	) == threadsreview.ModeBlocked {
		return nil, errThreadsWebhookBindingInactive
	}

	var account models.ChannelAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"id = ? AND organization_id = ? AND channel = ? AND provider = ? AND status IN ?",
			accountID,
			organizationID,
			models.ChannelThreads,
			channelapi.ThreadsProvider,
			[]models.ChannelAccountStatus{
				models.ChannelAccountStatusPending,
				models.ChannelAccountStatusActive,
				models.ChannelAccountStatusDegraded,
			},
		).
		First(&account).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errThreadsWebhookBindingInactive
		}
		return nil, err
	}
	if strings.TrimSpace(account.ExternalAccountID) != expectedExternalAccountID ||
		stringConfigValue(account.Metadata, "app_id") != payloadAppID {
		return nil, errThreadsWebhookBindingInactive
	}

	var accountCount int64
	if err := tx.Model(&models.ChannelAccount{}).
		Where(
			"organization_id = ? AND channel = ? AND provider = ?",
			organizationID,
			models.ChannelThreads,
			channelapi.ThreadsProvider,
		).
		Count(&accountCount).Error; err != nil {
		return nil, err
	}
	if accountCount != 1 {
		return nil, errThreadsWebhookBindingInactive
	}

	var credentials []models.ChannelCredential
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND kind = ? AND status IN ? AND expires_at IS NOT NULL AND expires_at > ?",
			organizationID,
			account.ID,
			models.ChannelCredentialKindOAuth,
			[]models.ChannelCredentialStatus{
				models.ChannelCredentialStatusActive,
				models.ChannelCredentialStatusExpiring,
			},
			now.UTC(),
		).
		Order("version DESC, id ASC").
		Find(&credentials).Error; err != nil {
		return nil, err
	}
	if len(credentials) != 1 || stringConfigValue(credentials[0].Metadata, "app_id") != payloadAppID {
		return nil, errThreadsWebhookBindingInactive
	}
	account.Credentials = credentials
	return &account, nil
}

func managedInstagramLoginWebhookAccount(account *models.ChannelAccount) bool {
	return managedInstagramControlPlaneIntent(account)
}

// lockMetaInstagramWebhookPersistenceBinding is the final authorization
// boundary before a relay-authenticated Instagram Login payload becomes
// durable. Control-plane changes use the same organization -> account ->
// credential lock order, so a downgrade that owns the organization mutex is
// visible here before any raw event, inbox message, or AI job can be written.
func (a *App) lockMetaInstagramWebhookPersistenceBinding(
	tx *gorm.DB,
	organizationID, accountID uuid.UUID,
	expectedExternalAccountID string,
	verifiedWebhookCredentialID uuid.UUID,
	verifiedWebhookCredentialVersion int,
	headers http.Header,
	body []byte,
	now time.Time,
) (*models.ChannelAccount, error) {
	if a == nil || a.Config == nil || tx == nil || organizationID == uuid.Nil ||
		accountID == uuid.Nil || verifiedWebhookCredentialID == uuid.Nil ||
		verifiedWebhookCredentialVersion < 1 {
		return nil, errMetaInstagramWebhookBindingInactive
	}
	expectedExternalAccountID = strings.TrimSpace(expectedExternalAccountID)
	if expectedExternalAccountID == "" {
		return nil, errMetaInstagramWebhookBindingInactive
	}
	now = now.UTC()
	if now.IsZero() {
		return nil, errMetaInstagramWebhookBindingInactive
	}
	if err := lockChannelAIOrganizationScopeTx(tx, organizationID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errMetaInstagramWebhookBindingInactive
		}
		return nil, err
	}

	var account models.ChannelAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ? AND channel = ? AND provider = ?",
		accountID, organizationID, models.ChannelInstagram, channelapi.RelayProvider,
	).First(&account).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errMetaInstagramWebhookBindingInactive
		}
		return nil, err
	}
	if account.Status != models.ChannelAccountStatusActive ||
		strings.TrimSpace(account.ExternalAccountID) != expectedExternalAccountID ||
		!managedInstagramLoginWebhookAccount(&account) ||
		!exactManagedInstagramCallbackBinding(
			&account,
			strings.TrimSpace(a.Config.MetaInstagram.AppID),
			stringConfigValue(account.Metadata, "meta_authorizing_user_id"),
		) {
		return nil, errMetaInstagramWebhookBindingInactive
	}

	var credentials []models.ChannelCredential
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND channel_account_id = ? AND status IN ?",
		organizationID,
		account.ID,
		[]models.ChannelCredentialStatus{
			models.ChannelCredentialStatusActive,
			models.ChannelCredentialStatusExpiring,
		},
	).Order("version DESC, id ASC").Find(&credentials).Error; err != nil {
		return nil, err
	}
	account.Credentials = credentials
	oauth := currentMetaRegistryCredential(
		account.Credentials,
		models.ChannelCredentialKindOAuth,
		now,
	)
	webhook := currentMetaRegistryCredential(
		account.Credentials,
		models.ChannelCredentialKindWebhook,
		now,
	)
	if !metaInstagramCredentialPairGenerationValid(oauth, webhook, now) ||
		webhook.ID != verifiedWebhookCredentialID ||
		webhook.Version != verifiedWebhookCredentialVersion ||
		a.metaInstagramCurrentRuntimeBindingReason(&account, organizationID, now) != "" ||
		!metaInstagramSubscribedOperationMatchesCredentials(account.Metadata, *oauth, *webhook) {
		return nil, errMetaInstagramWebhookBindingInactive
	}

	// Verify again with only the exact locked webhook credential. This prevents
	// a stale generation or a non-webhook credential from being used as a
	// fallback after the control-plane state was reloaded.
	account.Credentials = []models.ChannelCredential{*oauth, *webhook}
	verificationAccount := account
	verificationAccount.Credentials = []models.ChannelCredential{*webhook}
	adapter, err := a.channelAdapter(&verificationAccount)
	if err != nil || adapter.VerifyWebhook(&verificationAccount, headers, body) != nil {
		return nil, errMetaInstagramWebhookBindingInactive
	}
	return &account, nil
}

func rawInboundEventLeaseActive(status models.InboundEventStatus) bool {
	return status == models.InboundEventStatusPending ||
		status == models.InboundEventStatusProcessing
}

func persistOrClaimRawInboundEvent(tx *gorm.DB, rawEvent *models.InboundEvent) (bool, bool, error) {
	if tx == nil || rawEvent == nil {
		return false, false, errors.New("raw inbound event and database transaction are required")
	}
	now := time.Now().UTC()
	rawEvent.Status = models.InboundEventStatusProcessing
	rawEvent.ProcessingStartedAt = &now
	if rawEvent.AttemptCount < 1 {
		rawEvent.AttemptCount = 1
	}
	result := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "channel_account_id"},
			{Name: "dedupe_key"},
		},
		DoNothing: true,
	}).Create(rawEvent)
	if result.Error != nil {
		return false, false, result.Error
	}
	if result.RowsAffected == 1 {
		return true, false, nil
	}

	// A provider retry may reclaim a failed payload immediately or a
	// pending/processing payload whose processing lease expired. The row lock
	// ensures only one retry can own the next attempt.
	var existing models.InboundEvent
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND dedupe_key = ?",
			rawEvent.OrganizationID,
			rawEvent.ChannelAccountID,
			rawEvent.DedupeKey,
		).
		First(&existing).Error; err != nil {
		return false, false, err
	}
	*rawEvent = existing
	staleBefore := now.Add(-rawInboundProcessingLease)
	staleLease := (existing.Status == models.InboundEventStatusPending ||
		existing.Status == models.InboundEventStatusProcessing) &&
		(existing.ProcessingStartedAt == nil || existing.ProcessingStartedAt.Before(staleBefore))
	if existing.Status != models.InboundEventStatusFailed && !staleLease {
		return false, false, nil
	}
	claim := tx.Model(&models.InboundEvent{}).
		Where(
			"id = ? AND organization_id = ? AND status = ?",
			existing.ID,
			existing.OrganizationID,
			existing.Status,
		).
		Updates(map[string]any{
			"status":                models.InboundEventStatusProcessing,
			"processing_started_at": now,
			"processed_at":          nil,
			"attempt_count":         gorm.Expr("attempt_count + 1"),
			"error_code":            "",
			"error_message":         "",
			"updated_at":            now,
		})
	if claim.Error != nil {
		return false, false, claim.Error
	}
	claimed := claim.RowsAffected == 1
	if claimed {
		rawEvent.Status = models.InboundEventStatusProcessing
		rawEvent.ProcessingStartedAt = &now
		rawEvent.ProcessedAt = nil
		rawEvent.AttemptCount = existing.AttemptCount + 1
		rawEvent.ErrorCode = ""
		rawEvent.ErrorMessage = ""
	}
	return claimed, claimed, nil
}

func processNormalizedChannelEvent(
	tx *gorm.DB,
	account *models.ChannelAccount,
	event *channelapi.InboundEvent,
	acceptedAt time.Time,
) error {
	if acceptedAt.IsZero() {
		acceptedAt = time.Now().UTC()
	} else {
		acceptedAt = acceptedAt.UTC()
	}
	eventPayload, err := valueToJSONB(event)
	if err != nil {
		return err
	}
	canonical := models.InboundEvent{
		OrganizationID:      account.OrganizationID,
		ChannelAccountID:    account.ID,
		DedupeKey:           "event:" + event.DedupeKey,
		ProviderEventID:     event.ProviderEventID,
		EventType:           string(event.Type),
		Status:              models.InboundEventStatusProcessing,
		SignatureValid:      true,
		ReceivedAt:          acceptedAt,
		ProcessingStartedAt: timePointer(time.Now().UTC()),
		Headers:             models.JSONB{},
		Payload:             eventPayload,
	}
	result := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "channel_account_id"},
			{Name: "dedupe_key"},
		},
		DoNothing: true,
	}).Create(&canonical)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}

	switch event.Type {
	case channelapi.NormalizedEventTypeMessage:
		err = persistInboundChannelMessage(tx, account, event, event.Message, acceptedAt)
	case channelapi.NormalizedEventTypeMessageStatus:
		err = persistChannelMessageStatus(tx, account, event, event.MessageStatus)
	case channelapi.NormalizedEventTypeRead:
		err = persistChannelReadReceipt(tx, account, event, event.Read)
	case channelapi.NormalizedEventTypeReaction:
		err = persistChannelReaction(tx, account, event, event.Reaction)
	default:
		err = fmt.Errorf("unsupported normalized channel event type %q", event.Type)
	}
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	return tx.Model(&models.InboundEvent{}).
		Where("id = ? AND organization_id = ?", canonical.ID, account.OrganizationID).
		Updates(map[string]any{
			"status":       models.InboundEventStatusProcessed,
			"processed_at": now,
			"updated_at":   now,
		}).Error
}

func persistInboundChannelMessage(
	tx *gorm.DB,
	account *models.ChannelAccount,
	event *channelapi.InboundEvent,
	inbound *channelapi.InboundMessage,
	acceptedAt time.Time,
) error {
	if inbound == nil {
		return errors.New("normalized message payload is missing")
	}
	// Adapter validation is the first line of defense, but persistence remains
	// fail-closed in case another adapter emits an invalid canonical event.
	if inbound.Direction != models.DirectionIncoming {
		return errors.New("normalized inbound message direction must be incoming")
	}
	if inbound.Sender.Role != models.ConversationParticipantRoleCustomer {
		return errors.New("normalized inbound message sender role must be customer")
	}
	if acceptedAt.IsZero() {
		acceptedAt = time.Now().UTC()
	} else {
		acceptedAt = acceptedAt.UTC()
	}
	var eventOccurredAt time.Time
	if event != nil {
		eventOccurredAt = event.OccurredAt
	}
	serviceWindowOpenedAt := channelapi.InboundServiceWindowAnchor(
		acceptedAt,
		eventOccurredAt,
		inbound.SentAt,
		inbound.ReceivedAt,
	)
	identity, contact, err := findOrCreateChannelIdentity(tx, account, inbound.Sender, inbound.ReceivedAt)
	if err != nil {
		return err
	}
	conversation, err := findOrCreateInboxConversation(tx, account, identity, contact, inbound)
	if err != nil {
		return err
	}

	var existing models.Message
	if err := tx.
		Where(
			"organization_id = ? AND inbox_conversation_id = ? AND whats_app_message_id = ?",
			account.OrganizationID,
			conversation.ID,
			inbound.ExternalMessageID,
		).
		First(&existing).Error; err == nil {
		return nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	message := legacyMessageFromParts(
		account.OrganizationID,
		conversation,
		uuid.New(),
		models.DirectionIncoming,
		inbound.Parts,
		inbound.ReceivedAt,
	)
	message.WhatsAppMessageID = inbound.ExternalMessageID
	message.Status = models.MessageStatusReceived
	message.Metadata = models.JSONB{
		"channel":  account.Channel,
		"provider": account.Provider,
	}
	if inbound.ReplyToExternalID != "" {
		var reply models.Message
		if err := tx.
			Where(
				"organization_id = ? AND inbox_conversation_id = ? AND whats_app_message_id = ?",
				account.OrganizationID,
				conversation.ID,
				inbound.ReplyToExternalID,
			).
			First(&reply).Error; err == nil {
			message.IsReply = true
			message.ReplyToMessageID = &reply.ID
		}
	}
	if err := tx.Create(&message).Error; err != nil {
		return err
	}
	parts := persistentMessageParts(account.OrganizationID, conversation.ID, message.ID, inbound.Parts)
	if len(parts) > 0 {
		if err := tx.Create(&parts).Error; err != nil {
			return err
		}
	}
	if err := enqueueChannelAIReply(
		tx,
		account,
		conversation,
		&message,
		inbound.Parts,
		serviceWindowOpenedAt,
		acceptedAt,
	); err != nil {
		return err
	}

	preview := messagePreview(inbound.Parts)
	when := inbound.ReceivedAt
	if when.IsZero() {
		when = time.Now().UTC()
	}
	if err := tx.Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", contact.ID, account.OrganizationID).
		Updates(map[string]any{
			"last_message_at":      when,
			"last_inbound_at":      when,
			"last_message_preview": preview,
			"is_read":              false,
			"updated_at":           when,
		}).Error; err != nil {
		return err
	}
	conversationUpdates := map[string]any{
		"status":               models.InboxConversationStatusOpen,
		"last_message_at":      when,
		"last_inbound_at":      when,
		"last_message_preview": preview,
		"unread_count":         gorm.Expr("unread_count + 1"),
		"updated_at":           when,
	}
	if serviceWindowEndsAt := channelapi.InboundServiceWindowEndsAt(
		account.Channel,
		serviceWindowOpenedAt,
	); serviceWindowEndsAt != nil {
		conversationUpdates["service_window_ends_at"] = monotonicServiceWindowEndsAt(
			*serviceWindowEndsAt,
		)
	}
	if err := tx.Model(&models.InboxConversation{}).
		Where("id = ? AND organization_id = ?", conversation.ID, account.OrganizationID).
		Updates(conversationUpdates).Error; err != nil {
		return err
	}

	providerEventID := firstNonEmptyString(event.ProviderEventID, event.DedupeKey)
	messageEvent := models.MessageEvent{
		OrganizationID:    account.OrganizationID,
		ChannelAccountID:  account.ID,
		ConversationID:    conversation.ID,
		MessageID:         &message.ID,
		ProviderEventID:   providerEventID,
		ExternalMessageID: inbound.ExternalMessageID,
		Type:              models.MessageEventTypeReceived,
		OccurredAt:        event.OccurredAt,
		Payload:           models.JSONB{},
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "channel_account_id"},
			{Name: "provider_event_id"},
		},
		DoNothing: true,
	}).Create(&messageEvent).Error
}

// monotonicServiceWindowEndsAt keeps the provider service window from moving
// backwards when delayed events arrive after newer ones. Keeping the comparison
// inside the UPDATE also makes competing PostgreSQL writers serialize safely:
// after waiting on the row lock, each statement compares its candidate against
// the latest committed value rather than a value read before the lock wait.
func monotonicServiceWindowEndsAt(candidate time.Time) clause.Expr {
	candidate = candidate.UTC()
	return gorm.Expr(
		`CASE
			WHEN service_window_ends_at IS NULL OR service_window_ends_at < ?
				THEN ?
			ELSE service_window_ends_at
		END`,
		candidate,
		candidate,
	)
}

func findOrCreateChannelIdentity(
	tx *gorm.DB,
	account *models.ChannelAccount,
	participant channelapi.Participant,
	seenAt time.Time,
) (*models.ContactIdentity, *models.Contact, error) {
	orgID := account.OrganizationID
	if strings.TrimSpace(participant.ExternalID) == "" {
		return nil, nil, errors.New("channel participant external ID is required")
	}
	if seenAt.IsZero() {
		seenAt = time.Now().UTC()
	}
	var identity models.ContactIdentity
	err := tx.
		Where(
			"organization_id = ? AND channel_account_id = ? AND external_id = ?",
			orgID,
			account.ID,
			participant.ExternalID,
		).
		First(&identity).Error
	if err == nil {
		var contact models.Contact
		if err := tx.
			Where("id = ? AND organization_id = ?", identity.ContactID, orgID).
			First(&contact).Error; err != nil {
			return nil, nil, err
		}
		updates := map[string]any{
			"last_seen_at": seenAt,
			"updated_at":   seenAt,
		}
		if participant.DisplayName != "" {
			boundedName := truncateChannelRunes(participant.DisplayName, 255)
			updates["display_name"] = boundedName
			identity.DisplayName = boundedName
		}
		if participant.Address != "" {
			boundedAddress := truncateChannelRunes(participant.Address, 320)
			updates["address"] = boundedAddress
			updates["normalized_address"] = strings.ToLower(strings.TrimSpace(boundedAddress))
			identity.Address = boundedAddress
			identity.NormalizedAddress = strings.ToLower(strings.TrimSpace(boundedAddress))
		}
		if err := tx.Model(&models.ContactIdentity{}).
			Where("id = ? AND organization_id = ?", identity.ID, orgID).
			Updates(updates).Error; err != nil {
			return nil, nil, err
		}
		identity.LastSeenAt = &seenAt
		return &identity, &contact, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, err
	}

	if tx.Name() == "postgres" {
		lockKey := fmt.Sprintf("%s:%s:%s", orgID, account.ID, participant.ExternalID)
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", lockKey).Error; err != nil {
			return nil, nil, err
		}
		if err := tx.
			Where(
				"organization_id = ? AND channel_account_id = ? AND external_id = ?",
				orgID,
				account.ID,
				participant.ExternalID,
			).
			First(&identity).Error; err == nil {
			var contact models.Contact
			if err := tx.
				Where("id = ? AND organization_id = ?", identity.ContactID, orgID).
				First(&contact).Error; err != nil {
				return nil, nil, err
			}
			return &identity, &contact, nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, err
		}
	}

	address := legacyContactAddress(participant)
	contact := models.Contact{
		OrganizationID:  orgID,
		PhoneNumber:     address,
		ProfileName:     truncateChannelRunes(participant.DisplayName, 255),
		WhatsAppAccount: account.Name,
		LastMessageAt:   &seenAt,
		LastInboundAt:   &seenAt,
		IsRead:          false,
		Tags:            models.JSONBArray{},
		Metadata: models.JSONB{
			"channel":  account.Channel,
			"provider": account.Provider,
		},
	}
	if err := tx.Create(&contact).Error; err != nil {
		return nil, nil, err
	}
	identity = models.ContactIdentity{
		OrganizationID:    orgID,
		ContactID:         contact.ID,
		ChannelAccountID:  account.ID,
		Channel:           account.Channel,
		ExternalID:        participant.ExternalID,
		Address:           truncateChannelRunes(participant.Address, 320),
		NormalizedAddress: strings.ToLower(strings.TrimSpace(truncateChannelRunes(participant.Address, 320))),
		DisplayName:       truncateChannelRunes(participant.DisplayName, 255),
		IsPrimary:         true,
		FirstSeenAt:       &seenAt,
		LastSeenAt:        &seenAt,
		Metadata:          mapToJSONB(participant.Metadata),
	}
	if err := tx.Create(&identity).Error; err != nil {
		return nil, nil, err
	}
	return &identity, &contact, nil
}

func findOrCreateInboxConversation(
	tx *gorm.DB,
	account *models.ChannelAccount,
	identity *models.ContactIdentity,
	contact *models.Contact,
	inbound *channelapi.InboundMessage,
) (*models.InboxConversation, error) {
	var conversation models.InboxConversation
	err := tx.
		Where(
			"organization_id = ? AND channel_account_id = ? AND external_conversation_id = ?",
			account.OrganizationID,
			account.ID,
			inbound.Conversation.ExternalID,
		).
		First(&conversation).Error
	if err == nil {
		conversation.ChannelAccount = account
		conversation.Contact = contact
		conversation.ContactIdentity = identity
		return &conversation, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	openedAt := inbound.ReceivedAt
	if openedAt.IsZero() {
		openedAt = time.Now().UTC()
	}
	candidate := models.InboxConversation{
		OrganizationID:         account.OrganizationID,
		ChannelAccountID:       account.ID,
		ContactID:              contact.ID,
		ContactIdentityID:      &identity.ID,
		Channel:                account.Channel,
		ExternalConversationID: inbound.Conversation.ExternalID,
		Status:                 models.InboxConversationStatusOpen,
		Subject:                truncateChannelRunes(inbound.Conversation.Subject, 998),
		OpenedAt:               openedAt,
		Config:                 models.JSONB{},
		Metadata:               mapToJSONB(inbound.Conversation.Metadata),
	}
	result := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "channel_account_id"},
			{Name: "external_conversation_id"},
		},
		DoNothing: true,
	}).Create(&candidate)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 1 {
		conversation = candidate
	} else if err := tx.
		Where(
			"organization_id = ? AND channel_account_id = ? AND external_conversation_id = ?",
			account.OrganizationID,
			account.ID,
			inbound.Conversation.ExternalID,
		).
		First(&conversation).Error; err != nil {
		return nil, err
	}

	participant := models.ConversationParticipant{
		OrganizationID:    account.OrganizationID,
		ConversationID:    conversation.ID,
		ParticipantKey:    "external:" + inbound.Sender.ExternalID,
		Role:              models.ConversationParticipantRoleCustomer,
		ContactIdentityID: &identity.ID,
		ExternalID:        inbound.Sender.ExternalID,
		DisplayName:       truncateChannelRunes(inbound.Sender.DisplayName, 255),
		Address:           truncateChannelRunes(inbound.Sender.Address, 320),
		JoinedAt:          openedAt,
		Metadata:          mapToJSONB(inbound.Sender.Metadata),
	}
	if err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "conversation_id"},
			{Name: "participant_key"},
		},
		DoNothing: true,
	}).Create(&participant).Error; err != nil {
		return nil, err
	}
	conversation.ChannelAccount = account
	conversation.Contact = contact
	conversation.ContactIdentity = identity
	return &conversation, nil
}

func persistChannelMessageStatus(
	tx *gorm.DB,
	account *models.ChannelAccount,
	event *channelapi.InboundEvent,
	status *channelapi.MessageStatusUpdate,
) error {
	if status == nil {
		return errors.New("normalized status payload is missing")
	}
	var message models.Message
	if err := tx.
		Where(
			"organization_id = ? AND whats_app_account = ? AND whats_app_message_id = ?",
			account.OrganizationID,
			account.Name,
			status.ExternalMessageID,
		).
		First(&message).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if message.InboxConversationID == nil {
		return nil
	}
	messageStatus := canonicalMessageStatus(status.Type)
	if messageStatus != "" {
		if err := tx.Model(&models.Message{}).
			Where("id = ? AND organization_id = ?", message.ID, account.OrganizationID).
			Update("status", messageStatus).Error; err != nil {
			return err
		}
	}
	return createChannelMessageEvent(tx, account, event, *message.InboxConversationID, &message.ID, status)
}

func persistChannelReadReceipt(
	tx *gorm.DB,
	account *models.ChannelAccount,
	event *channelapi.InboundEvent,
	read *channelapi.ReadReceipt,
) error {
	if read == nil {
		return errors.New("normalized read payload is missing")
	}
	var conversation models.InboxConversation
	if err := tx.
		Where(
			"organization_id = ? AND channel_account_id = ? AND external_conversation_id = ?",
			account.OrganizationID,
			account.ID,
			read.Conversation.ExternalID,
		).
		First(&conversation).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	for index, externalID := range read.ExternalMessageIDs {
		var message models.Message
		messageQuery := tx.
			Where(
				"organization_id = ? AND inbox_conversation_id = ? AND whats_app_message_id = ?",
				account.OrganizationID,
				conversation.ID,
				externalID,
			).
			First(&message)
		if messageQuery.Error != nil && !errors.Is(messageQuery.Error, gorm.ErrRecordNotFound) {
			return messageQuery.Error
		}
		var messageID *uuid.UUID
		if messageQuery.Error == nil {
			messageID = &message.ID
			if err := tx.Model(&models.Message{}).
				Where("id = ? AND organization_id = ?", message.ID, account.OrganizationID).
				Update("status", models.MessageStatusRead).Error; err != nil {
				return err
			}
		}
		status := &channelapi.MessageStatusUpdate{
			ExternalMessageID: externalID,
			Type:              models.MessageEventTypeRead,
			OccurredAt:        read.ReadAt,
			ActorExternalID:   read.Reader.ExternalID,
		}
		eventCopy := *event
		eventCopy.ProviderEventID = fmt.Sprintf("%s:%d", firstNonEmptyString(event.ProviderEventID, event.DedupeKey), index)
		if err := createChannelMessageEvent(tx, account, &eventCopy, conversation.ID, messageID, status); err != nil {
			return err
		}
	}
	return nil
}

func persistChannelReaction(
	tx *gorm.DB,
	account *models.ChannelAccount,
	event *channelapi.InboundEvent,
	reaction *channelapi.Reaction,
) error {
	if reaction == nil {
		return errors.New("normalized reaction payload is missing")
	}
	var message models.Message
	if err := tx.
		Where(
			"organization_id = ? AND whats_app_account = ? AND whats_app_message_id = ?",
			account.OrganizationID,
			account.Name,
			reaction.ExternalMessageID,
		).
		First(&message).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if message.InboxConversationID == nil {
		return nil
	}
	status := &channelapi.MessageStatusUpdate{
		ExternalMessageID: reaction.ExternalMessageID,
		Type:              models.MessageEventTypeReaction,
		OccurredAt:        reaction.OccurredAt,
		ActorExternalID:   reaction.Sender.ExternalID,
		Metadata: map[string]any{
			"emoji":   reaction.Emoji,
			"removed": reaction.Removed,
		},
	}
	return createChannelMessageEvent(tx, account, event, *message.InboxConversationID, &message.ID, status)
}

func createChannelMessageEvent(
	tx *gorm.DB,
	account *models.ChannelAccount,
	event *channelapi.InboundEvent,
	conversationID uuid.UUID,
	messageID *uuid.UUID,
	status *channelapi.MessageStatusUpdate,
) error {
	payload := mapToJSONB(status.Metadata)
	occurredAt := status.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = event.OccurredAt
	}
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	messageEvent := models.MessageEvent{
		OrganizationID:    account.OrganizationID,
		ChannelAccountID:  account.ID,
		ConversationID:    conversationID,
		MessageID:         messageID,
		ProviderEventID:   firstNonEmptyString(event.ProviderEventID, event.DedupeKey),
		ExternalMessageID: status.ExternalMessageID,
		Type:              status.Type,
		OccurredAt:        occurredAt,
		ActorExternalID:   status.ActorExternalID,
		ErrorCode:         status.ErrorCode,
		ErrorMessage:      status.ErrorMessage,
		Payload:           payload,
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "channel_account_id"},
			{Name: "provider_event_id"},
		},
		DoNothing: true,
	}).Create(&messageEvent).Error
}

func resolveChannelWebhookOrganization(
	db *gorm.DB,
	channelAccountID uuid.UUID,
	rlsEnabled bool,
) (uuid.UUID, error) {
	if db == nil || channelAccountID == uuid.Nil {
		return uuid.Nil, gorm.ErrRecordNotFound
	}
	if !rlsEnabled {
		var account models.ChannelAccount
		if err := db.Model(&models.ChannelAccount{}).
			Select("organization_id").
			Where(
				"id = ? AND status IN ?",
				channelAccountID,
				[]models.ChannelAccountStatus{
					models.ChannelAccountStatusPending,
					models.ChannelAccountStatusActive,
					models.ChannelAccountStatusDegraded,
				},
			).
			First(&account).Error; err != nil {
			return uuid.Nil, err
		}
		if account.OrganizationID == uuid.Nil {
			return uuid.Nil, gorm.ErrRecordNotFound
		}
		return account.OrganizationID, nil
	}

	var value sql.NullString
	row := db.Raw(
		"SELECT public.rereply_resolve_channel_org(?)::text",
		channelAccountID,
	).Row()
	if err := row.Scan(&value); err != nil {
		return uuid.Nil, err
	}
	if !value.Valid || value.String == "" {
		return uuid.Nil, gorm.ErrRecordNotFound
	}
	return uuid.Parse(value.String)
}

func markInboundEventFailed(db *gorm.DB, orgID, eventID uuid.UUID, code string, processErr error) {
	_ = database.WithTenant(db, orgID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		return tx.Model(&models.InboundEvent{}).
			Where("id = ? AND organization_id = ?", eventID, orgID).
			Updates(map[string]any{
				"status":        models.InboundEventStatusFailed,
				"error_code":    code,
				"error_message": boundedErrorMessage(processErr),
				"processed_at":  now,
				"updated_at":    now,
			}).Error
	})
}

func markInboundEventIgnored(
	db *gorm.DB,
	orgID, eventID uuid.UUID,
	code, message string,
) error {
	return database.WithTenant(db, orgID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		result := tx.Model(&models.InboundEvent{}).
			Where("id = ? AND organization_id = ?", eventID, orgID).
			Updates(map[string]any{
				"status":        models.InboundEventStatusIgnored,
				"error_code":    code,
				"error_message": truncateChannelRunes(message, 1000),
				"headers":       models.JSONB{},
				"payload":       models.JSONB{"redacted": true},
				"processed_at":  now,
				"updated_at":    now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("inbound event discard lost its processing lease")
		}
		return nil
	})
}

func relayHTTPHeaders(r *fastglue.Request) http.Header {
	headers := make(http.Header)
	r.RequestCtx.Request.Header.VisitAll(func(key, value []byte) {
		headers.Add(string(key), string(value))
	})
	return headers
}

func safeRelayHeaders(headers http.Header) models.JSONB {
	result := models.JSONB{}
	for _, key := range []string{"Content-Type", "User-Agent", "X-Request-ID"} {
		if value := headers.Get(key); value != "" {
			result[strings.ToLower(key)] = value
		}
	}
	return result
}

func webhookPayloadJSON(body []byte) models.JSONB {
	var object models.JSONB
	if err := json.Unmarshal(body, &object); err == nil && object != nil {
		return object
	}
	return models.JSONB{"unparsed": true}
}

func pathString(r *fastglue.Request, key string) string {
	switch value := r.RequestCtx.UserValue(key).(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return ""
	}
}

func canonicalMessageStatus(eventType models.MessageEventType) models.MessageStatus {
	switch eventType {
	case models.MessageEventTypeReceived:
		return models.MessageStatusReceived
	case models.MessageEventTypeAccepted:
		return models.MessageStatusPending
	case models.MessageEventTypeSent:
		return models.MessageStatusSent
	case models.MessageEventTypeDelivered:
		return models.MessageStatusDelivered
	case models.MessageEventTypeRead:
		return models.MessageStatusRead
	case models.MessageEventTypeFailed:
		return models.MessageStatusFailed
	default:
		return ""
	}
}

func boundedErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	const limit = 1000
	message := err.Error()
	if len(message) > limit {
		return message[:limit]
	}
	return message
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func legacyContactAddress(participant channelapi.Participant) string {
	value := strings.TrimSpace(participant.Address)
	if value == "" {
		value = strings.TrimSpace(participant.ExternalID)
	}
	runes := []rune(value)
	if len(runes) <= 50 {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return "id:" + hex.EncodeToString(digest[:])[:40]
}

func mapToJSONB(value map[string]any) models.JSONB {
	if value == nil {
		return models.JSONB{}
	}
	return models.JSONB(value)
}
