package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	metaDeauthorizationMaxSignedRequest    = 16 << 10
	metaDeauthorizationProcessingLease     = 2 * time.Minute
	metaDeauthorizationCompletedTTL        = 24 * time.Hour
	metaDeauthorizationCompletedRetention  = 30 * 24 * time.Hour
	metaDeauthorizationUnresolvedRetention = 90 * 24 * time.Hour
	metaDeauthorizationCleanupBatch        = 500
	metaDeauthorizationTargetPageSize      = 100
	metaDeauthorizationMaxTargets          = 1000
	metaDeauthorizationEventDigestKey      = "meta_deauthorization_event_digest"
	metaDeauthorizationEventIssuedAtKey    = "meta_deauthorization_event_issued_at"
	metaDeauthorizationPendingDigestKey    = "meta_deauthorization_pending_digest"
	metaDeauthorizationPendingIssuedKey    = "meta_deauthorization_pending_issued_at"
	metaDeauthorizationResolvedDigestKey   = "meta_deauthorization_resolved_digest"
	metaDeauthorizationResolvedStateKey    = "meta_deauthorization_resolved_state"
)

var errMetaDeauthorizationEventStale = errors.New("deauthorization event is stale")

type metaDeauthorizationClaimStatus string

const (
	metaDeauthorizationClaimAcquired   metaDeauthorizationClaimStatus = "acquired"
	metaDeauthorizationClaimProcessing metaDeauthorizationClaimStatus = "processing"
	metaDeauthorizationClaimCompleted  metaDeauthorizationClaimStatus = "completed"
)

type metaDeauthorizationClaim struct {
	Key    string
	Token  string
	Status metaDeauthorizationClaimStatus
}

type metaMessengerSignedRequestPayload struct {
	Algorithm string `json:"algorithm"`
	IssuedAt  int64  `json:"issued_at"`
	UserID    string `json:"user_id"`
}

type metaDeauthorizationTarget struct {
	OrganizationID uuid.UUID `gorm:"column:organization_id"`
	AccountID      uuid.UUID `gorm:"column:account_id"`
}

// DeauthorizeMetaMessenger is Meta's public signed_request callback. The HMAC
// is verified before the rate gate, and the gate is then isolated by the
// authenticated event digest before any global account lookup.
func (a *App) DeauthorizeMetaMessenger(r *fastglue.Request) error {
	if a == nil || a.Config == nil || !a.Config.MetaMessenger.Enabled || a.Redis == nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Not found", nil, "")
	}
	raw := strings.TrimSpace(string(r.RequestCtx.FormValue("signed_request")))
	if raw == "" || len(raw) > metaDeauthorizationMaxSignedRequest {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid signed request", nil, "")
	}
	now := time.Now().UTC()
	payload, digest, err := verifyMetaMessengerSignedRequestSignature(raw, a.Config.MetaMessenger.AppSecret)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid signed request", nil, "")
	}
	if allowed, err := a.requireMetaDeauthorizationRateLimit(r, digest); !allowed {
		return err
	}
	journal, err := a.loadOrCreateMetaDeauthorizationEvent(
		digest,
		a.Config.MetaMessenger.AppID,
		payload,
		now,
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
	targets, err := a.resolveAllMetaDeauthorizationTargets(a.Config.MetaMessenger.AppID, payload.UserID)
	if err != nil {
		a.Log.Warn("Messenger deauthorization target lookup failed")
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization is temporarily unavailable", nil, "")
	}
	if len(targets) == 0 {
		// BISU signed_request identity equivalence must be proven against a
		// real sandbox callback before the pilot is enabled. Never acknowledge
		// an uncorrelated callback as successfully processed.
		a.Log.Warn("Messenger deauthorization identity did not match a managed account")
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization is temporarily unavailable", nil, "")
	}
	eventIssuedAt := time.Unix(payload.IssuedAt, 0).UTC()
	checkedAt := time.Now().UTC()
	applied, err := processMetaDeauthorizationTargets(targets, func(target metaDeauthorizationTarget) (bool, error) {
		return a.revokeMetaDeauthorizationTarget(target, eventIssuedAt, digest, checkedAt)
	})
	if err != nil {
		a.Log.Warn("Messenger deauthorization did not revoke every target")
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization is temporarily unavailable", nil, "")
	}
	if err := a.completeMetaDeauthorizationEvent(journal, time.Now().UTC()); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization completion is temporarily unavailable", nil, "")
	}
	completed = true
	if err := a.completeMetaMessengerDeauthorization(requestContext(r), claim); err != nil {
		// The durable database journal is authoritative. A Redis settlement
		// failure cannot make an already-revoked event impossible to replay.
		a.Log.Warn("Messenger deauthorization Redis completion lagged durable journal")
	}
	return r.SendEnvelope(map[string]any{"deauthorized": true, "accounts_revoked": applied})
}

func (a *App) loadOrCreateMetaDeauthorizationEvent(
	digest string,
	appID string,
	payload metaMessengerSignedRequestPayload,
	now time.Time,
) (models.MetaDeauthorizationEvent, error) {
	var event models.MetaDeauthorizationEvent
	if a == nil || a.DB == nil || len(strings.TrimSpace(digest)) != sha256.Size*2 ||
		!validCanonicalMetaID(appID) || !validCanonicalMetaID(payload.UserID) || payload.IssuedAt <= 0 {
		return event, errMetaDeauthorizationEventStale
	}
	issuedAt := time.Unix(payload.IssuedAt, 0).UTC()
	now = now.UTC()
	if issuedAt.After(now.Add(2 * time.Minute)) {
		return event, errMetaDeauthorizationEventStale
	}
	root := a.rootApp()
	err := root.DB.Where("digest = ?", digest).First(&event).Error
	if err == nil {
		if event.PlatformAppID != appID || event.AuthorizingUserID != payload.UserID ||
			!event.IssuedAt.UTC().Equal(issuedAt) {
			return models.MetaDeauthorizationEvent{}, errors.New("deauthorization journal identity mismatch")
		}
		if event.State != "verified" && event.State != "completed" {
			return models.MetaDeauthorizationEvent{}, errors.New("deauthorization journal state is invalid")
		}
		return event, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return event, err
	}
	// A cryptographically authentic callback may be retried after a database
	// outage that prevented the first durable journal write. Credential-
	// generation fences make that delayed retry safe; retain the same bounded
	// window as unresolved journal entries instead of permanently losing the
	// revocation after the initial ten-minute delivery window.
	if now.Sub(issuedAt) > metaDeauthorizationUnresolvedRetention {
		return event, errMetaDeauthorizationEventStale
	}
	event = models.MetaDeauthorizationEvent{
		Digest: digest, PlatformAppID: appID, AuthorizingUserID: payload.UserID,
		IssuedAt: issuedAt, VerifiedAt: now, State: "verified", LastAttemptAt: &now,
	}
	if err := root.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&event).Error; err != nil {
		return models.MetaDeauthorizationEvent{}, err
	}
	if err := root.DB.Where("digest = ?", digest).First(&event).Error; err != nil {
		return models.MetaDeauthorizationEvent{}, err
	}
	if event.PlatformAppID != appID || event.AuthorizingUserID != payload.UserID ||
		!event.IssuedAt.UTC().Equal(issuedAt) {
		return models.MetaDeauthorizationEvent{}, errors.New("deauthorization journal identity mismatch")
	}
	return event, nil
}

func (a *App) completeMetaDeauthorizationEvent(
	event models.MetaDeauthorizationEvent,
	completedAt time.Time,
) error {
	if a == nil || a.DB == nil || event.Digest == "" {
		return errors.New("deauthorization journal is unavailable")
	}
	completedAt = completedAt.UTC()
	result := a.rootApp().DB.Model(&models.MetaDeauthorizationEvent{}).
		Where("digest = ? AND platform_app_id = ? AND authorizing_user_id = ? AND issued_at = ? AND state = ?",
			event.Digest, event.PlatformAppID, event.AuthorizingUserID, event.IssuedAt, "verified").
		Updates(map[string]any{"state": "completed", "completed_at": completedAt, "last_attempt_at": completedAt})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	var current models.MetaDeauthorizationEvent
	if err := a.rootApp().DB.Where("digest = ?", event.Digest).First(&current).Error; err != nil {
		return err
	}
	if current.State == "completed" && current.PlatformAppID == event.PlatformAppID &&
		current.AuthorizingUserID == event.AuthorizingUserID && current.IssuedAt.UTC().Equal(event.IssuedAt.UTC()) {
		return nil
	}
	return errors.New("deauthorization journal completion was superseded")
}

func (a *App) cleanupMetaDeauthorizationEvents(now time.Time) error {
	if a == nil || a.DB == nil {
		return errors.New("deauthorization journal is unavailable")
	}
	now = now.UTC()
	root := a.rootApp()
	for _, cleanup := range []struct {
		where string
		args  []any
	}{
		{
			where: "state = ? AND completed_at IS NOT NULL AND completed_at < ?",
			args:  []any{"completed", now.Add(-metaDeauthorizationCompletedRetention)},
		},
		{
			where: "state = ? AND verified_at < ?",
			args:  []any{"verified", now.Add(-metaDeauthorizationUnresolvedRetention)},
		},
	} {
		var digests []string
		if err := root.DB.Model(&models.MetaDeauthorizationEvent{}).
			Where(cleanup.where, cleanup.args...).
			Order("verified_at").Limit(metaDeauthorizationCleanupBatch).
			Pluck("digest", &digests).Error; err != nil {
			return err
		}
		if len(digests) == 0 {
			continue
		}
		if err := root.DB.Where("digest IN ?", digests).
			Delete(&models.MetaDeauthorizationEvent{}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (a *App) acquireMetaMessengerDeauthorization(
	ctx context.Context,
	digest string,
) (metaDeauthorizationClaim, error) {
	if a == nil || a.Redis == nil || strings.TrimSpace(digest) == "" {
		return metaDeauthorizationClaim{}, errors.New("deauthorization replay protection is unavailable")
	}
	claim := metaDeauthorizationClaim{
		Key:   "meta-messenger:deauth:replay:" + strings.TrimSpace(digest),
		Token: "processing:" + uuid.NewString(),
	}
	script := redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if current == "completed" then return "completed" end
if current then return "processing" end
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[2])
return "acquired"
`)
	status, err := script.Run(
		ctx, a.Redis, []string{claim.Key}, claim.Token,
		metaDeauthorizationProcessingLease.Milliseconds(),
	).Text()
	if err != nil {
		return metaDeauthorizationClaim{}, err
	}
	claim.Status = metaDeauthorizationClaimStatus(status)
	if claim.Status != metaDeauthorizationClaimAcquired &&
		claim.Status != metaDeauthorizationClaimProcessing &&
		claim.Status != metaDeauthorizationClaimCompleted {
		return metaDeauthorizationClaim{}, errors.New("deauthorization replay state is invalid")
	}
	return claim, nil
}

func (a *App) releaseMetaMessengerDeauthorization(
	ctx context.Context,
	claim metaDeauthorizationClaim,
) error {
	if a == nil || a.Redis == nil || claim.Key == "" || claim.Token == "" {
		return errors.New("deauthorization processing lease is unavailable")
	}
	script := redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)
	return script.Run(ctx, a.Redis, []string{claim.Key}, claim.Token).Err()
}

func (a *App) completeMetaMessengerDeauthorization(
	ctx context.Context,
	claim metaDeauthorizationClaim,
) error {
	if a == nil || a.Redis == nil || claim.Key == "" || claim.Token == "" {
		return errors.New("deauthorization processing lease is unavailable")
	}
	script := redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then return 0 end
redis.call("SET", KEYS[1], "completed", "PX", ARGV[2])
return 1
`)
	completed, err := script.Run(
		ctx, a.Redis, []string{claim.Key}, claim.Token,
		metaDeauthorizationCompletedTTL.Milliseconds(),
	).Int64()
	if err != nil {
		return err
	}
	if completed != 1 {
		return errors.New("deauthorization processing lease was superseded")
	}
	return nil
}

func processMetaDeauthorizationTargets(
	targets []metaDeauthorizationTarget,
	revoke func(metaDeauthorizationTarget) (bool, error),
) (int, error) {
	if revoke == nil {
		return 0, errors.New("deauthorization mutation callback is required")
	}
	applied := 0
	var failures []error
	for _, target := range targets {
		changed, err := revoke(target)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if changed {
			applied++
		}
	}
	return applied, errors.Join(failures...)
}

func (a *App) revokeMetaDeauthorizationTarget(
	target metaDeauthorizationTarget,
	eventIssuedAt time.Time,
	eventDigest string,
	checkedAt time.Time,
) (bool, error) {
	var changed bool
	var ambiguous bool
	err := a.WithCommittedTenantApp(target.OrganizationID, func(scoped *App) error {
		var account models.ChannelAccount
		if err := scoped.DB.Preload("Credentials", func(db *gorm.DB) *gorm.DB { return db.Order("version DESC") }).
			Where("id = ? AND organization_id = ?", target.AccountID, target.OrganizationID).
			First(&account).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		oauth := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindOAuth, checkedAt)
		webhook := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindWebhook, checkedAt)
		if oauth == nil || webhook == nil {
			if stringConfigValue(account.Metadata, "meta_ownership_state") == metaregistry.OwnershipRevoked &&
				account.Status == models.ChannelAccountStatusDisconnected &&
				!boolConfigValue(account.Config, "outbound_enabled") &&
				!boolConfigValue(account.Config, "ai_reply_enabled") {
				issued := eventIssuedAt.UTC().Format(time.RFC3339)
				if stringConfigValue(account.Metadata, metaDeauthorizationEventDigestKey) == eventDigest &&
					stringConfigValue(account.Metadata, metaDeauthorizationEventIssuedAtKey) == issued {
					return nil
				}
				metadata := cloneJSONB(account.Metadata)
				metadata[metaDeauthorizationEventDigestKey] = eventDigest
				metadata[metaDeauthorizationEventIssuedAtKey] = issued
				result := scoped.DB.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ? AND status = ?",
						account.ID, target.OrganizationID, models.ChannelAccountStatusDisconnected).
					Update("metadata", metadata)
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return metaregistry.ErrNotFound
				}
				return nil
			}
			return errors.New("deauthorization target has no revocable credential fence")
		}
		authorizedAt, parseErr := time.Parse(
			time.RFC3339Nano,
			stringConfigValue(account.Metadata, metaMessengerAuthorizationGrantedAtKey),
		)
		if parseErr != nil || eventIssuedAt.IsZero() || strings.TrimSpace(eventDigest) == "" {
			return errors.New("deauthorization target has no authorization generation fence")
		}
		authorizedSecond := authorizedAt.UTC().Truncate(time.Second)
		credentialSecond := oauth.CreatedAt.UTC().Truncate(time.Second)
		if eventIssuedAt.Before(authorizedSecond) ||
			(!oauth.CreatedAt.IsZero() && eventIssuedAt.Before(credentialSecond)) {
			return nil
		}
		if eventIssuedAt.Equal(authorizedSecond) ||
			(!oauth.CreatedAt.IsZero() && eventIssuedAt.Equal(credentialSecond)) {
			if stringConfigValue(account.Metadata, metaDeauthorizationResolvedDigestKey) == eventDigest &&
				stringConfigValue(account.Metadata, metaDeauthorizationResolvedStateKey) == "current_authorization_verified" {
				return nil
			}
			metadata := cloneJSONB(account.Metadata)
			metadata[metaDeauthorizationPendingDigestKey] = eventDigest
			metadata[metaDeauthorizationPendingIssuedKey] = eventIssuedAt.UTC().Format(time.RFC3339)
			metadata["meta_ownership_state"] = metaregistry.OwnershipStale
			metadata["meta_ownership_reason"] = "deauthorization_generation_ambiguous"
			metadata["meta_activation_state"] = "deauthorization_reconciliation_required"
			config := cloneJSONB(account.Config)
			config["outbound_enabled"] = false
			config["ai_reply_enabled"] = false
			result := scoped.DB.Model(&models.ChannelAccount{}).
				Where("id = ? AND organization_id = ?", account.ID, target.OrganizationID).
				Updates(map[string]any{
					"status": models.ChannelAccountStatusDegraded, "config": config, "metadata": metadata,
					"last_error":    "Meta deauthorization requires authority reconciliation",
					"last_error_at": checkedAt.UTC(), "updated_at": checkedAt.UTC(),
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return metaregistry.ErrNotFound
			}
			ambiguous = true
			return nil
		}
		mutationTime := checkedAt
		if previous, parseErr := time.Parse(time.RFC3339Nano, stringConfigValue(account.Metadata, "meta_ownership_checked_at")); parseErr == nil &&
			!mutationTime.After(previous) {
			mutationTime = previous.Add(time.Nanosecond)
		}
		var mutationErr error
		changed, mutationErr = scoped.applyMetaRegistryMutation(metaregistry.MutationRequest{
			ChannelAccountID: account.ID, CredentialID: oauth.ID, CredentialVersion: oauth.Version,
			WebhookCredentialID: webhook.ID, WebhookCredentialVersion: webhook.Version,
			Outcome: metaregistry.OwnershipRevoked, Reason: "meta_signed_deauthorization",
			CheckedAt: mutationTime,
		}, metaregistry.OwnershipRevoked)
		if mutationErr != nil || !changed {
			return mutationErr
		}
		if err := scoped.DB.Where("id = ? AND organization_id = ?", account.ID, target.OrganizationID).
			First(&account).Error; err != nil {
			return err
		}
		metadata := cloneJSONB(account.Metadata)
		metadata[metaDeauthorizationEventDigestKey] = eventDigest
		metadata[metaDeauthorizationEventIssuedAtKey] = eventIssuedAt.UTC().Format(time.RFC3339)
		return scoped.DB.Model(&models.ChannelAccount{}).
			Where("id = ? AND organization_id = ?", account.ID, target.OrganizationID).
			Update("metadata", metadata).Error
	})
	if err == nil && ambiguous {
		return false, errors.New("deauthorization event generation is ambiguous and quarantined")
	}
	return changed, err
}

func verifyMetaMessengerSignedRequest(
	raw, appSecret string, now time.Time,
) (metaMessengerSignedRequestPayload, string, error) {
	payload, digest, err := verifyMetaMessengerSignedRequestSignature(raw, appSecret)
	if err != nil {
		return payload, digest, err
	}
	issuedAt := time.Unix(payload.IssuedAt, 0).UTC()
	if issuedAt.After(now.Add(2*time.Minute)) || now.Sub(issuedAt) > 10*time.Minute {
		return payload, digest, errors.New("signed request payload is stale or invalid")
	}
	return payload, digest, nil
}

func verifyMetaMessengerSignedRequestSignature(
	raw, appSecret string,
) (metaMessengerSignedRequestPayload, string, error) {
	var payload metaMessengerSignedRequestPayload
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || len(appSecret) < 32 {
		return payload, "", errors.New("invalid signed request")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(signature) != sha256.Size {
		return payload, "", errors.New("invalid signed request signature")
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	_, _ = mac.Write([]byte(parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return payload, "", errors.New("invalid signed request signature")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(decoded) == 0 || len(decoded) > 8<<10 || json.Unmarshal(decoded, &payload) != nil {
		return payload, "", errors.New("invalid signed request payload")
	}
	payload.Algorithm = strings.ToUpper(strings.TrimSpace(payload.Algorithm))
	payload.UserID = strings.TrimSpace(payload.UserID)
	if payload.Algorithm != "HMAC-SHA256" || !validCanonicalMetaID(payload.UserID) || payload.IssuedAt <= 0 {
		return payload, "", errors.New("signed request payload is stale or invalid")
	}
	digest := sha256.Sum256([]byte(raw))
	return payload, hex.EncodeToString(digest[:]), nil
}

func (a *App) requireMetaDeauthorizationRateLimit(r *fastglue.Request, eventDigest string) (bool, error) {
	remote := "unknown"
	if r != nil && r.RequestCtx != nil && r.RequestCtx.RemoteIP() != nil {
		remote = r.RequestCtx.RemoteIP().String()
	}
	digest := sha256.Sum256([]byte(remote + "\x00" + strings.TrimSpace(eventDigest)))
	key := "meta-messenger:deauth:rate:" + hex.EncodeToString(digest[:])
	script := redis.NewScript(`
local n = redis.call("INCR", KEYS[1])
if n == 1 then redis.call("PEXPIRE", KEYS[1], ARGV[1]) end
return n
`)
	count, err := script.Run(requestContext(r), a.Redis, []string{key}, time.Minute.Milliseconds()).Int64()
	if err != nil {
		return false, r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Deauthorization protection is unavailable", nil, "")
	}
	if count > 60 {
		return false, r.SendErrorEnvelope(fasthttp.StatusTooManyRequests, "Too many requests", nil, "")
	}
	return true, nil
}

func (a *App) resolveAllMetaDeauthorizationTargets(
	appID, userID string,
) ([]metaDeauthorizationTarget, error) {
	if !validCanonicalMetaID(appID) || !validCanonicalMetaID(userID) {
		return nil, errors.New("deauthorization target identity is invalid")
	}
	targets := make([]metaDeauthorizationTarget, 0)
	after := uuid.Nil
	for {
		page, err := a.resolveMetaDeauthorizationTargetPage(appID, userID, after, metaDeauthorizationTargetPageSize)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return targets, nil
		}
		if len(targets)+len(page) > metaDeauthorizationMaxTargets {
			return nil, errors.New("deauthorization target set exceeds the safe bound")
		}
		targets = append(targets, page...)
		after = page[len(page)-1].AccountID
		if len(page) < metaDeauthorizationTargetPageSize {
			return targets, nil
		}
	}
}

func (a *App) resolveMetaDeauthorizationTargetPage(
	appID, userID string,
	after uuid.UUID,
	limit int,
) ([]metaDeauthorizationTarget, error) {
	if limit < 1 || limit > metaDeauthorizationTargetPageSize {
		return nil, errors.New("deauthorization target page size is invalid")
	}
	root := a.rootApp()
	var targets []metaDeauthorizationTarget
	if a.rlsEnabled() {
		if err := root.DB.Raw(
			"SELECT organization_id, account_id FROM public.rereply_meta_deauth_target_page(?, ?, ?, ?)",
			appID, userID, after, limit,
		).Scan(&targets).Error; err != nil {
			return nil, err
		}
		return targets, nil
	}
	if err := root.DB.Model(&models.ChannelAccount{}).
		Select("organization_id, id AS account_id").
		Where("channel = ? AND provider = ? AND status IN ? AND metadata ->> 'meta_platform_app_id' = ? AND metadata ->> 'meta_authorizing_user_id' = ?",
			models.ChannelMessenger, channelapi.RelayProvider,
			[]models.ChannelAccountStatus{
				models.ChannelAccountStatusPending,
				models.ChannelAccountStatusActive,
				models.ChannelAccountStatusDegraded,
				models.ChannelAccountStatusDisconnected,
			},
			appID, userID).
		Where("id > ?", after).
		Order("id").
		Limit(limit).Scan(&targets).Error; err != nil {
		return nil, err
	}
	return targets, nil
}
