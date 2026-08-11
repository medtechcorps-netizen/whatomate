package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metareview"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	metaMessengerReviewBrokerBundleTTL = 30 * time.Second
	metaMessengerReviewReplayTTL       = 2 * metareview.MaximumRequestSkew
)

var errMetaMessengerReviewUnavailable = errors.New("staging Messenger review relay is unavailable")
var errMetaMessengerReviewWebhookInactive = errors.New("staging Messenger review webhook binding is inactive")
var errMetaMessengerReviewSubscribeDrainPending = errors.New("an earlier Messenger subscribe operation is still draining")

type metaMessengerReviewDeprovisionOperation struct {
	ID              uuid.UUID
	PageToken       string
	AlreadyComplete bool
}

type metaMessengerReviewWebhookCredential struct {
	ID               uuid.UUID
	Version          int
	InboundSecret    string
	Status           models.ChannelCredentialStatus
	ExpiresAt        *time.Time
	OrganizationID   uuid.UUID
	ChannelAccountID uuid.UUID
}

// ProvisionMetaMessengerReviewCredential is a server-to-server broker. It
// authenticates before touching Redis or tenant storage, consumes a one-time
// nonce, and returns only an AEAD ciphertext bundle containing the inbound
// HMAC. The Page token and outbound HMAC are never selected by this handler.
func (a *App) ProvisionMetaMessengerReviewCredential(r *fastglue.Request) error {
	settings, tuple, err := a.metaMessengerReviewSettings(time.Now().UTC())
	if err != nil || a.Redis == nil || !a.hasIntegrationEncryptionKey() {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Review credential broker not found", nil, "")
	}
	body := append([]byte(nil), r.RequestCtx.PostBody()...)
	if len(body) == 0 || len(body) > metareview.MaximumRequestBodyBytes {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid review credential request", nil, "")
	}
	protocol, err := metareview.NewProtocol(settings.BrokerAuthSecret, settings.BrokerWrapSecret)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Review credential broker unavailable", nil, "")
	}
	timestampText := strings.TrimSpace(string(r.RequestCtx.Request.Header.Peek(metareview.TimestampHeader)))
	timestamp, parseErr := strconv.ParseInt(timestampText, 10, 64)
	if parseErr != nil || strconv.FormatInt(timestamp, 10) != timestampText {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Invalid review credential authorization", nil, "")
	}
	nonce := strings.TrimSpace(string(r.RequestCtx.Request.Header.Peek(metareview.NonceHeader)))
	signature := strings.TrimSpace(string(r.RequestCtx.Request.Header.Peek(metareview.SignatureHeader)))
	now := time.Now().UTC()
	if err := protocol.VerifyProvisionRequest(timestamp, nonce, body, signature, now); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Invalid review credential authorization", nil, "")
	}
	request, err := metareview.DecodeProvisionRequest(body, now)
	if err != nil || request.Tuple != tuple {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Review credential binding not found", nil, "")
	}
	messengerAppSecretKeyID, err := metareview.MessengerAppSecretKeyID(
		a.Config.MetaMessengerOnboarding.AppSecret,
	)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Review credential broker unavailable", nil, "")
	}
	providerProofSecretKeyID, err := metareview.ProviderProofSecretKeyID(
		settings.ProviderProofSecret,
	)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Review credential broker unavailable", nil, "")
	}
	if request.MessengerAppSecretKeyID != messengerAppSecretKeyID ||
		request.ProviderProofSecretKeyID != providerProofSecretKeyID {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Review credential binding not found", nil, "")
	}
	replayKey, err := metareview.ReplayKey(nonce)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Invalid review credential authorization", nil, "")
	}
	consumed, err := a.Redis.SetNX(requestContext(r), replayKey, "1", metaMessengerReviewReplayTTL).Result()
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Review credential broker unavailable", nil, "")
	}
	if !consumed {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Invalid review credential authorization", nil, "")
	}

	credential, callbackURL, err := a.loadMetaMessengerReviewCredential(requestContext(r), tuple, settings)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, errMetaMessengerReviewUnavailable) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Review credential binding not found", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Review credential broker unavailable", nil, "")
	}
	expiresAt := now.Add(metaMessengerReviewBrokerBundleTTL)
	authorityExpiry, parseErr := time.Parse(time.RFC3339Nano, tuple.ExpiresAt)
	if parseErr != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Review credential broker unavailable", nil, "")
	}
	if authorityExpiry.Before(expiresAt) {
		expiresAt = authorityExpiry
	}
	response, err := protocol.SealCredentialBundle(
		body,
		timestamp,
		nonce,
		metareview.CredentialBundle{
			MessengerAppSecretKeyID:  messengerAppSecretKeyID,
			ProviderProofSecretKeyID: providerProofSecretKeyID,
			CredentialID:             credential.ID.String(),
			CredentialVersion:        credential.Version,
			ReReplyWebhookURL:        callbackURL,
			InboundSecret:            credential.InboundSecret,
		},
		now,
		expiresAt,
	)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Review credential broker unavailable", nil, "")
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Review credential broker unavailable", nil, "")
	}
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	r.RequestCtx.Response.Header.Set("Pragma", "no-cache")
	r.RequestCtx.Response.Header.SetContentType("application/json")
	r.RequestCtx.SetStatusCode(fasthttp.StatusOK)
	r.RequestCtx.SetBody(encoded)
	return nil
}

// DeprovisionMetaMessengerReviewAccount first revokes local inbound authority
// and cancels queued egress under the account lock. Only after that fence
// commits does it unsubscribe the exact Meta app while the Page token remains
// available. The OAuth token is erased and the row soft-deleted only after
// Meta confirms cleanup; failures remain quarantined and safely retryable.
func (a *App) DeprovisionMetaMessengerReviewAccount(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil || orgID == uuid.Nil || userID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	accountID, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}
	root := a.rootApp()
	if root == nil || root.DB == nil || !root.hasIntegrationEncryptionKey() {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Review deprovisioning is unavailable", nil, "")
	}
	operation, authErr, err := root.beginMetaMessengerReviewDeprovision(
		r,
		orgID,
		accountID,
		userID,
	)
	if authErr != nil {
		return nil
	}
	if err != nil {
		if errors.Is(err, errMetaMessengerReviewReplyDrainPending) {
			return r.SendErrorEnvelope(
				fasthttp.StatusServiceUnavailable,
				"An earlier Messenger review reply remains permanently fenced; audited operator reconciliation is required before deprovisioning",
				nil,
				"",
			)
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Review connection not found", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Review connection could not be quarantined", nil, "")
	}
	if operation.AlreadyComplete {
		return r.SendEnvelope(map[string]string{"message": "Staging Messenger review connection deprovisioned"})
	}
	if operation.PageToken == "" {
		_ = root.markMetaMessengerReviewCleanupPending(orgID, accountID, userID, operation.ID)
		return r.SendErrorEnvelope(
			fasthttp.StatusBadGateway,
			"The review connection is quarantined; Meta cleanup requires operator action",
			nil,
			"",
		)
	}

	ctx, cancel := context.WithTimeout(requestContext(r), 30*time.Second)
	defer cancel()
	if err := root.unsubscribeMetaMessengerPage(ctx, root.Config.MetaMessengerReviewRelay.PageID, operation.PageToken); err != nil {
		_ = root.markMetaMessengerReviewCleanupPending(orgID, accountID, userID, operation.ID)
		return r.SendErrorEnvelope(
			fasthttp.StatusBadGateway,
			"The review connection is quarantined; Meta cleanup must be retried",
			nil,
			"",
		)
	}
	if err := root.recordMetaMessengerReviewRemoteCleanupConfirmed(
		orgID,
		accountID,
		userID,
		operation.ID,
	); err != nil {
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Meta cleanup succeeded; its local confirmation must be retried",
			nil,
			"",
		)
	}
	if err := root.finalizeMetaMessengerReviewDeprovision(orgID, accountID, userID, operation.ID); err != nil {
		if errors.Is(err, errMetaMessengerReviewSubscribeDrainPending) {
			return r.SendErrorEnvelope(
				fasthttp.StatusServiceUnavailable,
				"Meta cleanup is confirmed; retry after the earlier onboarding request drains",
				nil,
				"",
			)
		}
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Meta cleanup succeeded; local credential erasure must be retried",
			nil,
			"",
		)
	}
	return r.SendEnvelope(map[string]string{"message": "Staging Messenger review connection deprovisioned"})
}

func (a *App) beginMetaMessengerReviewDeprovision(
	r *fastglue.Request,
	organizationID, accountID, userID uuid.UUID,
) (metaMessengerReviewDeprovisionOperation, error, error) {
	var result metaMessengerReviewDeprovisionOperation
	var authErr error
	if a == nil || a.DB == nil {
		return result, nil, errMetaMessengerReviewUnavailable
	}
	err := database.WithTenantReadCommitted(a.DB, organizationID, func(tx *gorm.DB) error {
		scoped := a.scopedApp(tx, organizationID)
		_, _, authErr = scoped.requireAuth(r, models.ResourceChannelAccounts, models.ActionDelete)
		if authErr != nil {
			return nil
		}
		if err := lockChannelAIOrganizationScopeTx(tx, organizationID); err != nil {
			return err
		}
		var account models.ChannelAccount
		if err := tx.Unscoped().Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", accountID, organizationID).
			First(&account).Error; err != nil {
			return err
		}
		if !scoped.configuredMetaMessengerReviewAccount(&account) {
			return gorm.ErrRecordNotFound
		}
		activeReviewSend, err := hasProtectedMetaMessengerReviewDispatchTx(
			tx,
			organizationID,
			accountID,
			time.Now().UTC(),
		)
		if err != nil {
			return err
		}
		if activeReviewSend {
			return errMetaMessengerReviewReplyDrainPending
		}
		if account.DeletedAt.Valid &&
			stringConfigValue(account.Metadata, metaMessengerSubscriptionDesiredStateKey) == metaMessengerSubscriptionDesiredUnsubscribed &&
			stringConfigValue(account.Metadata, metaMessengerSubscriptionRemoteStateKey) == metaMessengerSubscriptionRemoteUnsubscribed &&
			stringConfigValue(account.Metadata, "onboarding_state") == "review_deprovisioned" {
			result.AlreadyComplete = true
			return nil
		}

		old := account
		old.Config = cloneJSONB(account.Config)
		old.Metadata = cloneJSONB(account.Metadata)
		old.Capabilities = cloneJSONB(account.Capabilities)
		now := time.Now().UTC()
		account.Config = cloneJSONB(account.Config)
		account.Metadata = cloneJSONB(account.Metadata)
		priorOperation, priorOperationErr := metaMessengerSubscriptionOperationFromAccount(&account)
		priorDesiredState := stringConfigValue(account.Metadata, metaMessengerSubscriptionDesiredStateKey)

		if priorDesiredState == metaMessengerSubscriptionDesiredUnsubscribed {
			operationIDText := stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationIDKey)
			operationID, parseErr := uuid.Parse(operationIDText)
			if parseErr == nil && operationID != uuid.Nil && operationID.String() == operationIDText {
				result.ID = operationID
			}
		}
		if result.ID == uuid.Nil {
			result.ID = uuid.New()
			if priorOperationErr == nil &&
				priorOperation.DesiredState == metaMessengerSubscriptionDesiredSubscribed &&
				priorOperation.State == metaMessengerSubscriptionSubscribePending &&
				priorOperation.ExpiresAt.After(now) {
				account.Metadata[metaMessengerSubscriptionFencedOperationIDKey] = priorOperation.ID.String()
				account.Metadata[metaMessengerSubscriptionFencedOperationEndKey] = priorOperation.ExpiresAt.UTC().Format(time.RFC3339Nano)
				account.Metadata[metaMessengerSubscriptionFencedAckKey] = false
				delete(account.Metadata, metaMessengerSubscriptionFencedAckAtKey)
			} else {
				delete(account.Metadata, metaMessengerSubscriptionFencedOperationIDKey)
				delete(account.Metadata, metaMessengerSubscriptionFencedOperationEndKey)
				delete(account.Metadata, metaMessengerSubscriptionFencedAckKey)
				delete(account.Metadata, metaMessengerSubscriptionFencedAckAtKey)
			}
		}

		var oauth []models.ChannelCredential
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"organization_id = ? AND channel_account_id = ? AND kind = ? AND status IN ?",
				organizationID,
				accountID,
				models.ChannelCredentialKindOAuth,
				[]models.ChannelCredentialStatus{
					models.ChannelCredentialStatusActive,
					models.ChannelCredentialStatusExpiring,
				},
			).
			Order("version DESC, id ASC").
			Find(&oauth).Error; err != nil {
			return err
		}
		if len(oauth) == 1 &&
			stringConfigValue(oauth[0].Metadata, "app_id") == a.Config.MetaMessengerOnboarding.AppID &&
			stringConfigValue(oauth[0].Metadata, "page_id") == account.ExternalAccountID &&
			stringConfigValue(oauth[0].Metadata, "meta_business_id") == a.Config.MetaMessengerReviewRelay.MetaBusinessID {
			account.Metadata[metaMessengerSubscriptionOAuthCredentialIDKey] = oauth[0].ID.String()
			account.Metadata[metaMessengerSubscriptionOAuthVersionKey] = strconv.Itoa(oauth[0].Version)
			encryptedToken, _ := oauth[0].CredentialBlob["access_token"].(string)
			if appcrypto.IsEncrypted(encryptedToken) {
				decryptedToken, decryptErr := appcrypto.Decrypt(encryptedToken, a.integrationEncryptionKey())
				if decryptErr == nil && strings.TrimSpace(decryptedToken) != "" {
					result.PageToken = strings.TrimSpace(decryptedToken)
				}
			}
		}

		account.Metadata[metaMessengerSubscriptionDesiredStateKey] = metaMessengerSubscriptionDesiredUnsubscribed
		account.Metadata[metaMessengerSubscriptionOperationIDKey] = result.ID.String()
		account.Metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionUnsubscribePending
		account.Metadata[metaMessengerSubscriptionOperationExpiresKey] = now.Add(metaMessengerSubscriptionOperationLease).Format(time.RFC3339Nano)
		account.Metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnknown
		delete(account.Metadata, metaMessengerSubscriptionRemoteConfirmedAtKey)
		account.Status = models.ChannelAccountStatusDisconnected
		account.Config["outbound_enabled"] = false
		account.Config["ai_reply_enabled"] = false
		account.Config["onboarding_state"] = "review_deprovisioning"
		account.Metadata["review_ready"] = false
		account.Metadata["subscription_verified"] = false
		account.Metadata["onboarding_state"] = "review_deprovisioning"
		account.IsDefaultIncoming = false
		account.IsDefaultOutgoing = false
		account.UpdatedByID = &userID
		if err := tx.Unscoped().Save(&account).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.ChannelCredential{}).
			Where(
				"organization_id = ? AND channel_account_id = ? AND kind = ?",
				organizationID,
				accountID,
				models.ChannelCredentialKindWebhook,
			).
			Updates(map[string]any{
				"status":          models.ChannelCredentialStatusRevoked,
				"credential_blob": models.JSONB{},
				"revoked_at":      now,
				"rotated_at":      now,
				"updated_at":      now,
			}).Error; err != nil {
			return err
		}
		if err := cancelChannelAIReplyJobsForAccountTx(
			tx,
			organizationID,
			accountID,
			"staging_review_deprovisioned",
		); err != nil {
			return err
		}
		if err := cancelMetaMessengerReviewOutboxTx(tx, organizationID, accountID, now); err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			organizationID,
			userID,
			audit.GetUserName(tx, userID),
			"channel_account",
			account.ID,
			models.AuditActionUpdated,
			&old,
			&account,
		)
	})
	return result, authErr, err
}

func cancelMetaMessengerReviewOutboxTx(
	tx *gorm.DB,
	organizationID, channelAccountID uuid.UUID,
	now time.Time,
) error {
	if tx == nil || organizationID == uuid.Nil || channelAccountID == uuid.Nil || now.IsZero() {
		return errors.New("tenant channel account transaction is required")
	}
	queuedStatuses := []models.OutboxJobStatus{
		models.OutboxJobStatusPending,
		models.OutboxJobStatusRetrying,
		models.OutboxJobStatusProcessing,
	}
	var cancelledJobs []models.OutboxJob
	if err := tx.Model(&cancelledJobs).
		Clauses(clause.Returning{Columns: []clause.Column{{Name: "message_id"}}}).
		Where(
			"organization_id = ? AND channel_account_id = ? AND status IN ?",
			organizationID,
			channelAccountID,
			queuedStatuses,
		).
		Updates(map[string]any{
			"status":          models.OutboxJobStatusCancelled,
			"failed_at":       now,
			"last_error_code": "staging_review_deprovisioned",
			"last_error":      "Staging Messenger review connection deprovisioned before delivery",
			"locked_at":       nil,
			"locked_by":       "",
			"updated_at":      now,
		}).Error; err != nil {
		return err
	}
	messageIDs := make([]uuid.UUID, 0, len(cancelledJobs))
	for index := range cancelledJobs {
		if cancelledJobs[index].MessageID != nil {
			messageIDs = append(messageIDs, *cancelledJobs[index].MessageID)
		}
	}
	if len(messageIDs) == 0 {
		return nil
	}
	return tx.Model(&models.Message{}).
		Where(
			"organization_id = ? AND id IN ? AND status = ?",
			organizationID,
			messageIDs,
			models.MessageStatusPending,
		).
		Updates(map[string]any{
			"status":        models.MessageStatusFailed,
			"error_message": "Staging Messenger review connection deprovisioned before delivery",
			"updated_at":    now,
		}).Error
}

func (a *App) markMetaMessengerReviewCleanupPending(
	organizationID, channelAccountID, userID, operationID uuid.UUID,
) error {
	if a == nil || a.DB == nil || organizationID == uuid.Nil ||
		channelAccountID == uuid.Nil || userID == uuid.Nil || operationID == uuid.Nil {
		return errMetaMessengerReviewUnavailable
	}
	return database.WithTenantReadCommitted(a.DB, organizationID, func(tx *gorm.DB) error {
		var account models.ChannelAccount
		if err := tx.Unscoped().Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", channelAccountID, organizationID).
			First(&account).Error; err != nil {
			return err
		}
		if !a.configuredMetaMessengerReviewAccount(&account) {
			return gorm.ErrRecordNotFound
		}
		if stringConfigValue(account.Metadata, metaMessengerSubscriptionDesiredStateKey) != metaMessengerSubscriptionDesiredUnsubscribed ||
			stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationIDKey) != operationID.String() {
			return errMetaMessengerSubscriptionFence
		}
		old := account
		old.Config = cloneJSONB(account.Config)
		old.Metadata = cloneJSONB(account.Metadata)
		old.Capabilities = cloneJSONB(account.Capabilities)
		now := time.Now().UTC()
		account.Status = models.ChannelAccountStatusDisconnected
		account.Config = cloneJSONB(account.Config)
		account.Config["outbound_enabled"] = false
		account.Config["ai_reply_enabled"] = false
		account.Config["onboarding_state"] = "review_remote_cleanup_pending"
		account.Metadata = cloneJSONB(account.Metadata)
		account.Metadata["review_ready"] = false
		account.Metadata["onboarding_state"] = "review_remote_cleanup_pending"
		account.Metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionUnsubscribePending
		account.Metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnknown
		delete(account.Metadata, metaMessengerSubscriptionRemoteConfirmedAtKey)
		account.IsDefaultIncoming = false
		account.IsDefaultOutgoing = false
		account.LastError = "Meta review webhook cleanup is pending"
		account.LastErrorAt = &now
		account.UpdatedByID = &userID
		if err := tx.Unscoped().Save(&account).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			organizationID,
			userID,
			audit.GetUserName(tx, userID),
			"channel_account",
			account.ID,
			models.AuditActionUpdated,
			&old,
			&account,
		)
	})
}

func (a *App) recordMetaMessengerReviewRemoteCleanupConfirmed(
	organizationID, channelAccountID, userID, operationID uuid.UUID,
) error {
	if a == nil || a.DB == nil || organizationID == uuid.Nil ||
		channelAccountID == uuid.Nil || userID == uuid.Nil || operationID == uuid.Nil {
		return errMetaMessengerReviewUnavailable
	}
	return database.WithTenantReadCommitted(a.DB, organizationID, func(tx *gorm.DB) error {
		var account models.ChannelAccount
		if err := tx.Unscoped().Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", channelAccountID, organizationID).
			First(&account).Error; err != nil {
			return err
		}
		if !a.configuredMetaMessengerReviewAccount(&account) ||
			stringConfigValue(account.Metadata, metaMessengerSubscriptionDesiredStateKey) != metaMessengerSubscriptionDesiredUnsubscribed ||
			stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationIDKey) != operationID.String() {
			return errMetaMessengerSubscriptionFence
		}
		now := time.Now().UTC()
		account.Config = cloneJSONB(account.Config)
		account.Metadata = cloneJSONB(account.Metadata)
		account.Metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionUnsubscribeConfirmed
		account.Metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnsubscribed
		account.Metadata[metaMessengerSubscriptionRemoteConfirmedAtKey] = now.Format(time.RFC3339Nano)
		account.Metadata["review_ready"] = false
		if metaMessengerFencedSubscribeStillActive(&account, now) {
			account.Config["onboarding_state"] = "review_remote_cleanup_pending"
			account.Metadata["onboarding_state"] = "review_remote_cleanup_pending"
			account.LastError = "Waiting for an earlier Messenger subscribe operation to drain"
			account.LastErrorAt = &now
		} else {
			account.Config["onboarding_state"] = "review_deprovisioning"
			account.Metadata["onboarding_state"] = "review_deprovisioning"
		}
		account.UpdatedByID = &userID
		return tx.Unscoped().Save(&account).Error
	})
}

func metaMessengerFencedSubscribeStillActive(account *models.ChannelAccount, now time.Time) bool {
	if account == nil || account.Metadata == nil ||
		stringConfigValue(account.Metadata, metaMessengerSubscriptionFencedOperationIDKey) == "" {
		return false
	}
	if acknowledged, ok := account.Metadata[metaMessengerSubscriptionFencedAckKey].(bool); ok && acknowledged {
		return false
	}
	expiresAt, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(account.Metadata, metaMessengerSubscriptionFencedOperationEndKey),
	)
	// Corrupt or missing fence expiry fails closed so an operator can recover
	// without silently erasing the only Page token.
	return err != nil || expiresAt.After(now)
}

func (a *App) finalizeMetaMessengerReviewDeprovision(
	organizationID, channelAccountID, userID uuid.UUID,
	expectedOperationID uuid.UUID,
) error {
	if a == nil || a.DB == nil || organizationID == uuid.Nil ||
		channelAccountID == uuid.Nil || userID == uuid.Nil || expectedOperationID == uuid.Nil {
		return errMetaMessengerReviewUnavailable
	}
	return database.WithTenantReadCommitted(a.DB, organizationID, func(tx *gorm.DB) error {
		if err := lockChannelAIOrganizationScopeTx(tx, organizationID); err != nil {
			return err
		}
		var account models.ChannelAccount
		if err := tx.Unscoped().Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", channelAccountID, organizationID).
			First(&account).Error; err != nil {
			return err
		}
		if !a.configuredMetaMessengerReviewAccount(&account) {
			return gorm.ErrRecordNotFound
		}
		if stringConfigValue(account.Metadata, metaMessengerSubscriptionDesiredStateKey) != metaMessengerSubscriptionDesiredUnsubscribed ||
			stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationIDKey) != expectedOperationID.String() ||
			stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationStateKey) != metaMessengerSubscriptionUnsubscribeConfirmed ||
			stringConfigValue(account.Metadata, metaMessengerSubscriptionRemoteStateKey) != metaMessengerSubscriptionRemoteUnsubscribed {
			return errMetaMessengerSubscriptionFence
		}
		if metaMessengerFencedSubscribeStillActive(&account, time.Now().UTC()) {
			return errMetaMessengerReviewSubscribeDrainPending
		}
		if account.DeletedAt.Valid {
			if stringConfigValue(account.Metadata, "onboarding_state") == "review_deprovisioned" {
				return nil
			}
			return gorm.ErrRecordNotFound
		}
		old := account
		old.Config = cloneJSONB(account.Config)
		old.Metadata = cloneJSONB(account.Metadata)
		old.Capabilities = cloneJSONB(account.Capabilities)
		now := time.Now().UTC()
		if err := cancelChannelAIReplyJobsForAccountTx(
			tx,
			organizationID,
			channelAccountID,
			"staging_review_deprovisioned",
		); err != nil {
			return err
		}
		if err := cancelMetaMessengerReviewOutboxTx(
			tx,
			organizationID,
			channelAccountID,
			now,
		); err != nil {
			return err
		}
		if err := tx.Model(&models.ChannelCredential{}).
			Where("organization_id = ? AND channel_account_id = ?", organizationID, channelAccountID).
			Updates(map[string]any{
				"status":          models.ChannelCredentialStatusRevoked,
				"credential_blob": models.JSONB{},
				"revoked_at":      now,
				"rotated_at":      now,
				"updated_at":      now,
			}).Error; err != nil {
			return err
		}
		account.Status = models.ChannelAccountStatusDisconnected
		account.Config = cloneJSONB(account.Config)
		account.Config["outbound_enabled"] = false
		account.Config["ai_reply_enabled"] = false
		account.Config["onboarding_state"] = "review_deprovisioned"
		account.Metadata = cloneJSONB(account.Metadata)
		account.Metadata["review_ready"] = false
		account.Metadata["subscription_verified"] = false
		account.Metadata["onboarding_state"] = "review_deprovisioned"
		account.Metadata[metaMessengerSubscriptionDesiredStateKey] = metaMessengerSubscriptionDesiredUnsubscribed
		account.Metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnsubscribed
		account.Metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionUnsubscribeConfirmed
		account.IsDefaultIncoming = false
		account.IsDefaultOutgoing = false
		account.ConnectedAt = nil
		account.LastError = ""
		account.LastErrorAt = nil
		account.UpdatedByID = &userID
		if err := tx.Unscoped().Save(&account).Error; err != nil {
			return err
		}
		deleted := tx.Where("id = ? AND organization_id = ?", channelAccountID, organizationID).
			Delete(&models.ChannelAccount{})
		if deleted.Error != nil {
			return deleted.Error
		}
		if deleted.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return audit.LogAudit(
			tx,
			organizationID,
			userID,
			audit.GetUserName(tx, userID),
			"channel_account",
			account.ID,
			models.AuditActionDeleted,
			&old,
			nil,
		)
	})
}

func (a *App) metaMessengerReviewSettings(
	now time.Time,
) (configpkg.MetaMessengerReviewRelayConfig, metareview.ProvisionTuple, error) {
	var settings configpkg.MetaMessengerReviewRelayConfig
	var tuple metareview.ProvisionTuple
	if a == nil || a.Config == nil || a.Config.App.Environment != "staging" {
		return settings, tuple, errMetaMessengerReviewUnavailable
	}
	settings = a.Config.MetaMessengerReviewRelay
	if !settings.Enabled || settings.Mode != metareview.Mode {
		return configpkg.MetaMessengerReviewRelayConfig{}, tuple, errMetaMessengerReviewUnavailable
	}
	tuple = metareview.ProvisionTuple{
		OrganizationID:   settings.OrganizationID,
		MetaBusinessID:   settings.MetaBusinessID,
		PageID:           settings.PageID,
		MetaAppID:        a.Config.MetaMessengerOnboarding.AppID,
		ChannelAccountID: settings.ChannelAccountID,
		Generation:       settings.Generation,
		ExpiresAt:        settings.ExpiresAt,
	}
	if err := tuple.Validate(now); err != nil {
		return configpkg.MetaMessengerReviewRelayConfig{}, metareview.ProvisionTuple{}, errMetaMessengerReviewUnavailable
	}
	return settings, tuple, nil
}

func (a *App) configuredMetaMessengerReviewAccount(account *models.ChannelAccount) bool {
	if a == nil || a.Config == nil || a.Config.App.Environment != "staging" ||
		!a.Config.MetaMessengerReviewRelay.Enabled ||
		a.Config.MetaMessengerReviewRelay.Mode != metareview.Mode {
		return false
	}
	settings := a.Config.MetaMessengerReviewRelay
	tuple := metareview.ProvisionTuple{
		OrganizationID:   settings.OrganizationID,
		MetaBusinessID:   settings.MetaBusinessID,
		PageID:           settings.PageID,
		MetaAppID:        a.Config.MetaMessengerOnboarding.AppID,
		ChannelAccountID: settings.ChannelAccountID,
		Generation:       settings.Generation,
		ExpiresAt:        settings.ExpiresAt,
	}
	return metareview.DeploymentIdentityMatches(tuple, account)
}

func (a *App) filterMetaMessengerReviewInventory(
	organizationID uuid.UUID,
	businesses []metaMessengerBusinessSummary,
	pages []metaMessengerStoredPage,
) ([]metaMessengerBusinessSummary, []metaMessengerStoredPage, error) {
	_, tuple, err := a.metaMessengerReviewSettings(time.Now().UTC())
	if err != nil {
		if a == nil || a.Config == nil || !a.Config.MetaMessengerReviewRelay.Enabled {
			return businesses, pages, nil
		}
		return nil, nil, errMetaMessengerReviewUnavailable
	}
	if organizationID.String() != tuple.OrganizationID {
		return nil, nil, errMetaMessengerReviewUnavailable
	}
	var selectedBusiness *metaMessengerBusinessSummary
	for index := range businesses {
		if businesses[index].ID == tuple.MetaBusinessID {
			copy := businesses[index]
			selectedBusiness = &copy
			break
		}
	}
	var selectedPage *metaMessengerStoredPage
	for index := range pages {
		if pages[index].BusinessID == tuple.MetaBusinessID && pages[index].PageID == tuple.PageID {
			copy := pages[index]
			selectedPage = &copy
			break
		}
	}
	if selectedBusiness == nil {
		return nil, nil, fmt.Errorf("%w: configured business missing", errMetaMessengerReviewUnavailable)
	}
	if selectedPage == nil {
		return nil, nil, fmt.Errorf("%w: configured Page missing", errMetaMessengerReviewUnavailable)
	}
	if selectedPage.Ownership != metaMessengerOwnershipOwned {
		return nil, nil, fmt.Errorf("%w: configured Page is not business-owned", errMetaMessengerReviewUnavailable)
	}
	if !selectedPage.Selectable {
		reason := strings.TrimSpace(selectedPage.DisabledReason)
		switch reason {
		case metaMessengerDisabledAssignment, metaMessengerDisabledTokenMissing,
			metaMessengerDisabledTarget, metaMessengerDisabledTask:
		default:
			reason = "page_not_selectable"
		}
		return nil, nil, fmt.Errorf("%w: %s", errMetaMessengerReviewUnavailable, reason)
	}
	if !metaMessengerHasMessagingTask(selectedPage.Tasks) {
		return nil, nil, fmt.Errorf("%w: %s", errMetaMessengerReviewUnavailable, metaMessengerDisabledTask)
	}
	return []metaMessengerBusinessSummary{*selectedBusiness}, []metaMessengerStoredPage{*selectedPage}, nil
}

func (a *App) readyMetaMessengerReviewAccount(account *models.ChannelAccount) bool {
	settings, tuple, err := a.metaMessengerReviewSettings(time.Now().UTC())
	if err != nil || !metareview.ReadyInboundOnlyBindingMatches(tuple, account) {
		return false
	}
	expectedRelayURL, err := metaMessengerRelayURL(settings.RelayBaseURL, tuple.PageID)
	return err == nil && stringConfigValue(account.Config, "relay_url") == expectedRelayURL &&
		stringConfigValue(account.Metadata, "meta_app_owner_business_id") ==
			a.Config.MetaMessengerOnboarding.OwnerBusinessID
}

func (a *App) loadMetaMessengerReviewCredential(
	ctx context.Context,
	tuple metareview.ProvisionTuple,
	settings configpkg.MetaMessengerReviewRelayConfig,
) (metaMessengerReviewWebhookCredential, string, error) {
	credential := metaMessengerReviewWebhookCredential{}
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return credential, "", errors.New("review credential database is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	orgID, err := uuid.Parse(tuple.OrganizationID)
	if err != nil {
		return credential, "", errMetaMessengerReviewUnavailable
	}
	accountID, err := uuid.Parse(tuple.ChannelAccountID)
	if err != nil {
		return credential, "", errMetaMessengerReviewUnavailable
	}
	callbackURL := ""
	err = database.WithTenantReadCommitted(root.DB.WithContext(ctx), orgID, func(tx *gorm.DB) error {
		var account models.ChannelAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", accountID, orgID).
			First(&account).Error; err != nil {
			return err
		}
		if !root.readyMetaMessengerReviewAccount(&account) {
			return errMetaMessengerReviewUnavailable
		}
		now := time.Now().UTC()
		var webhookRows []metaMessengerReviewWebhookCredential
		if err := tx.Model(&models.ChannelCredential{}).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Select(
				"id, organization_id, channel_account_id, version, status, expires_at, "+
					"credential_blob ->> 'inbound_secret' AS inbound_secret",
			).
			Where(
				"organization_id = ? AND channel_account_id = ? AND kind = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
				orgID,
				accountID,
				models.ChannelCredentialKindWebhook,
				[]models.ChannelCredentialStatus{
					models.ChannelCredentialStatusActive,
					models.ChannelCredentialStatusExpiring,
				},
				now,
			).
			Order("version DESC, id ASC").
			Find(&webhookRows).Error; err != nil {
			return err
		}
		if len(webhookRows) != 1 || !appcrypto.IsEncrypted(webhookRows[0].InboundSecret) {
			return errMetaMessengerReviewUnavailable
		}
		var oauthRows []models.ChannelCredential
		if err := tx.Model(&models.ChannelCredential{}).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id, metadata").
			Where(
				"organization_id = ? AND channel_account_id = ? AND kind = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
				orgID,
				accountID,
				models.ChannelCredentialKindOAuth,
				[]models.ChannelCredentialStatus{
					models.ChannelCredentialStatusActive,
					models.ChannelCredentialStatusExpiring,
				},
				now,
			).
			Find(&oauthRows).Error; err != nil {
			return err
		}
		if len(oauthRows) != 1 ||
			stringConfigValue(oauthRows[0].Metadata, "app_id") != tuple.MetaAppID ||
			stringConfigValue(oauthRows[0].Metadata, "page_id") != tuple.PageID ||
			stringConfigValue(oauthRows[0].Metadata, "meta_business_id") != tuple.MetaBusinessID {
			return errMetaMessengerReviewUnavailable
		}
		plaintext, err := appcrypto.Decrypt(
			webhookRows[0].InboundSecret,
			root.integrationEncryptionKey(),
		)
		if err != nil || len([]byte(plaintext)) < 32 || strings.TrimSpace(plaintext) != plaintext {
			return errMetaMessengerReviewUnavailable
		}
		credential = webhookRows[0]
		credential.InboundSecret = plaintext
		callbackURL, err = metaMessengerReviewCallbackURL(settings.ReReplyBaseURL, tuple.ChannelAccountID)
		return err
	})
	return credential, callbackURL, err
}

func metaMessengerReviewCallbackURL(baseURL, accountID string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.Path != "" || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errMetaMessengerReviewUnavailable
	}
	id, err := uuid.Parse(accountID)
	if err != nil || id == uuid.Nil || id.String() != accountID {
		return "", errMetaMessengerReviewUnavailable
	}
	parsed.Path = "/api/webhooks/channels/" + accountID
	return parsed.String(), nil
}

func metaMessengerReviewAccountMarker(account *models.ChannelAccount) bool {
	return channelapi.IsStagingMessengerReviewMarked(account)
}

func (a *App) lockMetaMessengerReviewWebhookPersistenceBinding(
	tx *gorm.DB,
	organizationID, accountID uuid.UUID,
	headers http.Header,
	body []byte,
) (*models.ChannelAccount, error) {
	if tx == nil || organizationID == uuid.Nil || accountID == uuid.Nil {
		return nil, errMetaMessengerReviewWebhookInactive
	}
	settings, tuple, err := a.metaMessengerReviewSettings(time.Now().UTC())
	if err != nil || tuple.OrganizationID != organizationID.String() ||
		tuple.ChannelAccountID != accountID.String() {
		return nil, errMetaMessengerReviewWebhookInactive
	}
	var account models.ChannelAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", accountID, organizationID).
		First(&account).Error; err != nil {
		return nil, errMetaMessengerReviewWebhookInactive
	}
	if !a.readyMetaMessengerReviewAccount(&account) {
		return nil, errMetaMessengerReviewWebhookInactive
	}
	generation := strings.TrimSpace(headers.Get(metareview.GenerationHeader))
	credentialIDText := strings.TrimSpace(headers.Get(metareview.CredentialIDHeader))
	credentialVersionText := strings.TrimSpace(headers.Get(metareview.CredentialVersionHeader))
	credentialID, err := uuid.Parse(credentialIDText)
	if err != nil || credentialID == uuid.Nil || credentialID.String() != credentialIDText ||
		generation != tuple.Generation {
		return nil, errMetaMessengerReviewWebhookInactive
	}
	credentialVersion, err := strconv.Atoi(credentialVersionText)
	if err != nil || credentialVersion <= 0 || strconv.Itoa(credentialVersion) != credentialVersionText {
		return nil, errMetaMessengerReviewWebhookInactive
	}
	var rows []metaMessengerReviewWebhookCredential
	now := time.Now().UTC()
	if err := tx.Model(&models.ChannelCredential{}).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select(
			"id, organization_id, channel_account_id, version, status, expires_at, "+
				"credential_blob ->> 'inbound_secret' AS inbound_secret",
		).
		Where(
			"id = ? AND organization_id = ? AND channel_account_id = ? AND kind = ? AND version = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
			credentialID,
			organizationID,
			accountID,
			models.ChannelCredentialKindWebhook,
			credentialVersion,
			[]models.ChannelCredentialStatus{
				models.ChannelCredentialStatusActive,
				models.ChannelCredentialStatusExpiring,
			},
			now,
		).
		Find(&rows).Error; err != nil || len(rows) != 1 ||
		!appcrypto.IsEncrypted(rows[0].InboundSecret) {
		return nil, errMetaMessengerReviewWebhookInactive
	}
	if err := metareview.VerifyInboundProof(
		settings.ProviderProofSecret,
		tuple,
		credentialIDText,
		credentialVersion,
		body,
		headers.Get(metareview.ReviewProofHeader),
	); err != nil {
		return nil, errMetaMessengerReviewWebhookInactive
	}
	credential := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: rows[0].ID},
		OrganizationID:   organizationID,
		ChannelAccountID: accountID,
		Kind:             models.ChannelCredentialKindWebhook,
		Version:          rows[0].Version,
		Status:           rows[0].Status,
		ExpiresAt:        rows[0].ExpiresAt,
		CredentialBlob:   models.JSONB{"inbound_secret": rows[0].InboundSecret},
	}
	account.Credentials = []models.ChannelCredential{credential}
	adapter := channelapi.NewRelayAdapter(account.Channel, a.HTTPClient, a.integrationEncryptionKey()).
		WithExpectedMetaBusinessID(tuple.MetaBusinessID).
		WithMetaProviderProofSecret(settings.ProviderProofSecret).
		WithInboundOnly()
	if err := adapter.VerifyWebhook(&account, headers, body); err != nil {
		return nil, errMetaMessengerReviewWebhookInactive
	}
	return &account, nil
}
