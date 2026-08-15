package handlers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DeauthorizeMetaInstagram is the public signed_request callback for the
// distinct managed Instagram Login application. Signature verification and a
// durable identity journal happen before the configured-tenant RLS lookup.
func (a *App) DeauthorizeMetaInstagram(r *fastglue.Request) error {
	if a == nil || a.Config == nil || !a.Config.MetaInstagram.Enabled || a.Redis == nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Not found", nil, "")
	}
	body := r.RequestCtx.PostBody()
	contentType := strings.ToLower(strings.TrimSpace(string(r.RequestCtx.Request.Header.ContentType())))
	if separator := strings.IndexByte(contentType, ';'); separator >= 0 {
		contentType = strings.TrimSpace(contentType[:separator])
	}
	if !r.RequestCtx.IsPost() || len(body) == 0 || len(body) > metaDeauthorizationMaxSignedRequest ||
		contentType != "application/x-www-form-urlencoded" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid signed request", nil, "")
	}
	postArgs := r.RequestCtx.PostArgs()
	validForm := true
	values := 0
	postArgs.VisitAll(func(key, _ []byte) {
		values++
		if values != 1 || string(key) != "signed_request" {
			validForm = false
		}
	})
	raw := strings.TrimSpace(string(postArgs.Peek("signed_request")))
	if !validForm || values != 1 || raw == "" || len(raw) > metaDeauthorizationMaxSignedRequest {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid signed request", nil, "")
	}
	now := time.Now().UTC()
	payload, digest, err := verifyMetaMessengerSignedRequestSignature(
		raw, a.Config.MetaInstagram.AppSecret,
	)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid signed request", nil, "")
	}
	if allowed, err := a.requireMetaDeauthorizationRateLimit(r, digest); !allowed {
		return err
	}
	allowedOrganizationID, parseErr := uuid.Parse(
		strings.TrimSpace(a.Config.MetaInstagram.AllowedOrganizationID),
	)
	if parseErr != nil || allowedOrganizationID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization is temporarily unavailable", nil, "")
	}
	journal, err := a.loadOrCreateMetaInstagramDeauthorizationEvent(
		allowedOrganizationID, digest, a.Config.MetaInstagram.AppID, payload, now,
	)
	if err != nil {
		if errors.Is(err, errMetaDeauthorizationEventStale) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid signed request", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization journal is unavailable", nil, "")
	}
	if journal.State == "completed" {
		return r.SendEnvelope(map[string]any{"deauthorized": true, "replayed": true})
	}
	claim, err := a.acquireMetaMessengerDeauthorization(requestContext(r), digest)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization protection is unavailable", nil, "")
	}
	if claim.Status == metaDeauthorizationClaimCompleted {
		return r.SendEnvelope(map[string]any{"deauthorized": true, "replayed": true})
	}
	if claim.Status != metaDeauthorizationClaimAcquired {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization is already being processed", nil, "")
	}
	completed := false
	defer func() {
		if !completed {
			_ = a.releaseMetaMessengerDeauthorization(context.Background(), claim)
		}
	}()
	targets, err := a.resolveMetaInstagramCallbackTargets(
		allowedOrganizationID, a.Config.MetaInstagram.AppID, payload.UserID,
	)
	if err != nil {
		a.Log.Warn("Instagram deauthorization target lookup failed")
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization is temporarily unavailable", nil, "")
	}
	if len(targets) == 0 {
		// Fail closed: a foreign duplicate cannot poison the exact pilot target,
		// but a foreign-only callback remains unacknowledged and unmutated.
		a.Log.Warn("Instagram deauthorization identity did not match the managed pilot account")
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization is temporarily unavailable", nil, "")
	}
	eventIssuedAt := time.Unix(payload.IssuedAt, 0).UTC()
	checkedAt := time.Now().UTC()
	applied, err := processMetaDeauthorizationTargets(
		targets,
		func(target metaDeauthorizationTarget) (bool, error) {
			return a.revokeMetaDeauthorizationTargetForBinding(
				target, models.ChannelInstagram, a.Config.MetaInstagram.AppID, payload.UserID,
				eventIssuedAt, digest, checkedAt,
			)
		},
	)
	if err != nil {
		a.Log.Warn("Instagram deauthorization did not revoke every target")
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization is temporarily unavailable", nil, "")
	}
	if err := a.completeMetaDeauthorizationEvent(journal, time.Now().UTC()); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization completion is temporarily unavailable", nil, "")
	}
	completed = true
	if err := a.completeMetaMessengerDeauthorization(requestContext(r), claim); err != nil {
		a.Log.Warn("Instagram deauthorization Redis completion lagged durable journal")
	}
	return r.SendEnvelope(map[string]any{
		"deauthorized":     true,
		"accounts_revoked": applied,
	})
}

func (a *App) loadOrCreateMetaInstagramDeauthorizationEvent(
	organizationID uuid.UUID,
	digest, appID string,
	payload metaMessengerSignedRequestPayload,
	now time.Time,
) (models.MetaDeauthorizationEvent, error) {
	var event models.MetaDeauthorizationEvent
	if a == nil || a.DB == nil || organizationID == uuid.Nil {
		return event, errMetaDeauthorizationEventStale
	}
	err := database.WithTenantReadCommitted(
		a.rootApp().DB, organizationID, func(tx *gorm.DB) error {
			// The durable Instagram deauthorization write participates in the
			// same organization mutex as OAuth provision/rotation. Messenger keeps
			// its existing global journal path unchanged.
			if lockErr := lockChannelAIOrganizationScopeTx(tx, organizationID); lockErr != nil {
				return lockErr
			}
			var journalErr error
			event, journalErr = loadOrCreateMetaDeauthorizationEventDB(
				tx, digest, appID, payload, now,
			)
			if journalErr != nil {
				return journalErr
			}
			targets, resolveErr := resolveMetaInstagramCallbackTargetsTx(
				tx, organizationID, appID, payload.UserID,
			)
			if resolveErr != nil {
				return resolveErr
			}
			if len(targets) == 0 {
				return nil
			}
			return a.rootApp().scopedApp(tx, organizationID).
				fenceMetaInstagramDeauthorizationJournalTarget(
					targets[0], appID, payload.UserID, digest,
					time.Unix(payload.IssuedAt, 0).UTC(), now,
				)
		},
	)
	return event, err
}

// fenceMetaInstagramDeauthorizationJournalTarget runs in the same tenant
// transaction and organizations.id critical section as the first durable
// signed-event write. A current or ambiguous generation becomes non-routable
// before the journal commit is visible; an event strictly older than both
// authorization-generation fences is retained as evidence without disturbing
// a newer reconnect.
func (a *App) fenceMetaInstagramDeauthorizationJournalTarget(
	target metaDeauthorizationTarget,
	appID, userID, digest string,
	eventIssuedAt, checkedAt time.Time,
) error {
	if a == nil || a.DB == nil || a.tenantOrgID == uuid.Nil ||
		target.OrganizationID != a.tenantOrgID || target.AccountID == uuid.Nil ||
		!validCanonicalMetaID(appID) || !validCanonicalMetaID(userID) ||
		strings.TrimSpace(digest) == "" || eventIssuedAt.IsZero() {
		return metaregistry.ErrInvalidRequest
	}
	var account models.ChannelAccount
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
		Preload("Credentials", func(db *gorm.DB) *gorm.DB { return db.Order("version DESC") }).
		Where("id = ? AND organization_id = ?", target.AccountID, target.OrganizationID).
		First(&account).Error; err != nil {
		return err
	}
	if !exactManagedInstagramCallbackBinding(&account, appID, userID) {
		return metaregistry.ErrNotFound
	}
	now := checkedAt.UTC()
	oauth := currentMetaRegistryCredential(
		account.Credentials, models.ChannelCredentialKindOAuth, now,
	)
	webhook := currentMetaRegistryCredential(
		account.Credentials, models.ChannelCredentialKindWebhook, now,
	)
	if oauth == nil && webhook == nil && account.Status == models.ChannelAccountStatusDisconnected {
		return nil
	}
	reason := "deauthorization_generation_ambiguous"
	authorizedAt, parseErr := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(account.Metadata, metaMessengerAuthorizationGrantedAtKey),
	)
	validGeneration := parseErr == nil && !authorizedAt.IsZero() &&
		metaInstagramCredentialPairGenerationValid(oauth, webhook, now)
	if !validGeneration {
		reason = "deauthorization_generation_fence_missing"
	} else if metaInstagramSignedEventStrictlyPredatesGeneration(
		eventIssuedAt, authorizedAt, oauth.CreatedAt,
	) {
		return nil
	} else {
		eventSecond := eventIssuedAt.UTC().Truncate(time.Second)
		authorizedSecond := authorizedAt.UTC().Truncate(time.Second)
		credentialSecond := oauth.CreatedAt.UTC().Truncate(time.Second)
		if eventSecond.After(authorizedSecond) && eventSecond.After(credentialSecond) {
			if previous, err := time.Parse(
				time.RFC3339Nano,
				stringConfigValue(account.Metadata, "meta_ownership_checked_at"),
			); err == nil && !now.After(previous) {
				now = previous.Add(time.Nanosecond)
			}
			changed, err := a.applyMetaRegistryMutation(metaregistry.MutationRequest{
				ChannelAccountID: account.ID,
				CredentialID:     oauth.ID, CredentialVersion: oauth.Version,
				WebhookCredentialID: webhook.ID, WebhookCredentialVersion: webhook.Version,
				Outcome: metaregistry.OwnershipRevoked, Reason: "meta_signed_deauthorization",
				CheckedAt: now,
			}, metaregistry.OwnershipRevoked)
			if err != nil {
				return err
			}
			if !changed {
				return metaregistry.ErrNotFound
			}
			var revoked models.ChannelAccount
			if err := a.DB.Where(
				"id = ? AND organization_id = ?", account.ID, target.OrganizationID,
			).First(&revoked).Error; err != nil {
				return err
			}
			metadata := cloneJSONB(revoked.Metadata)
			metadata[metaDeauthorizationEventDigestKey] = digest
			metadata[metaDeauthorizationEventIssuedAtKey] =
				eventIssuedAt.UTC().Format(time.RFC3339)
			return a.DB.Model(&models.ChannelAccount{}).
				Where("id = ? AND organization_id = ?", account.ID, target.OrganizationID).
				Update("metadata", metadata).Error
		}
	}
	if stringConfigValue(account.Metadata, metaDeauthorizationResolvedDigestKey) == digest &&
		stringConfigValue(account.Metadata, metaDeauthorizationResolvedStateKey) ==
			"current_authorization_verified" {
		return nil
	}
	if previous, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(account.Metadata, "meta_ownership_checked_at"),
	); err == nil && !now.After(previous) {
		now = previous.Add(time.Nanosecond)
	}
	metadata := cloneJSONB(account.Metadata)
	metadata[metaDeauthorizationPendingDigestKey] = digest
	metadata[metaDeauthorizationPendingIssuedKey] = eventIssuedAt.UTC().Format(time.RFC3339)
	metadata["meta_ownership_state"] = metaregistry.OwnershipStale
	metadata["meta_ownership_reason"] = reason
	metadata["meta_ownership_checked_at"] = now.Format(time.RFC3339Nano)
	metadata["meta_activation_state"] = "deauthorization_reconciliation_required"
	config := cloneJSONB(account.Config)
	config["outbound_enabled"] = false
	config["ai_reply_enabled"] = false
	result := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, target.OrganizationID).
		Updates(map[string]any{
			"status": models.ChannelAccountStatusDegraded, "config": config,
			"metadata": metadata, "last_error": "Meta deauthorization requires authority reconciliation",
			"last_error_at": now, "connected_at": nil, "updated_at": now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return metaregistry.ErrNotFound
	}
	return cancelManagedMetaQueuedWorkForAccountTx(
		a.DB, target.OrganizationID, account.ID,
		"managed_instagram_deauthorization_journal_fence",
	)
}

// resolveMetaInstagramCallbackTargets scopes public Instagram callback lookup
// inside the configured pilot tenant before cardinality is evaluated. Foreign
// rows therefore cannot poison deauthorization or data-deletion callbacks by
// exhausting the global pager. Messenger keeps its existing global resolver.
func (a *App) resolveMetaInstagramCallbackTargets(
	allowedOrganizationID uuid.UUID,
	appID, profileID string,
) ([]metaDeauthorizationTarget, error) {
	if allowedOrganizationID == uuid.Nil || !validCanonicalMetaID(appID) ||
		!validCanonicalMetaID(profileID) {
		return nil, errors.New("instagram deauthorization target identity is invalid")
	}
	targets := make([]metaDeauthorizationTarget, 0, 1)
	err := database.WithTenantReadCommitted(
		a.rootApp().DB, allowedOrganizationID,
		func(tx *gorm.DB) error {
			var resolveErr error
			targets, resolveErr = resolveMetaInstagramCallbackTargetsTx(
				tx, allowedOrganizationID, appID, profileID,
			)
			return resolveErr
		},
	)
	if err != nil {
		return nil, err
	}
	if len(targets) > 1 {
		return nil, errors.New("instagram deauthorization target is ambiguous")
	}
	return targets, nil
}

func resolveMetaInstagramCallbackTargetsTx(
	tx *gorm.DB,
	allowedOrganizationID uuid.UUID,
	appID, profileID string,
) ([]metaDeauthorizationTarget, error) {
	if tx == nil || allowedOrganizationID == uuid.Nil || !validCanonicalMetaID(appID) ||
		!validCanonicalMetaID(profileID) {
		return nil, errors.New("instagram callback target identity is invalid")
	}
	targets := make([]metaDeauthorizationTarget, 0, 1)
	if err := tx.Model(&models.ChannelAccount{}).
		Select("organization_id, id AS account_id").
		Where(
			"organization_id = ? AND channel = ? AND provider = ? AND status IN ? AND config ->> 'meta_registry_managed' = 'true' AND config ->> 'meta_management_mode' = ? AND config ->> 'instagram_api_mode' = 'instagram_login' AND metadata ->> 'meta_platform_app_id' = ? AND metadata ->> 'meta_webhook_app' = 'instagram_login' AND metadata ->> 'meta_authorizing_user_id' = ? AND metadata ->> 'meta_oauth_subject_id' = ? AND metadata ->> 'meta_authority_asset_id' = external_account_id",
			allowedOrganizationID, models.ChannelInstagram, channelapi.RelayProvider,
			[]models.ChannelAccountStatus{
				models.ChannelAccountStatusPending,
				models.ChannelAccountStatusActive,
				models.ChannelAccountStatusDegraded,
				models.ChannelAccountStatusDisconnected,
			},
			metaregistry.ManagementModePlatformOAuth, appID, profileID, profileID,
		).
		Order("id").Limit(2).Scan(&targets).Error; err != nil {
		return nil, err
	}
	if len(targets) > 1 {
		return nil, errors.New("instagram callback target is ambiguous")
	}
	return targets, nil
}
