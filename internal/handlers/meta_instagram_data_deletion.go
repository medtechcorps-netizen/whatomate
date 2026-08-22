package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	metaInstagramDeletionSubjectType         = "instagram_profile"
	metaInstagramDeletionReceivedChannel     = "instagram"
	metaInstagramDeletionVerification        = "meta_instagram_signed_request"
	metaInstagramDeletionProcessingLease     = 2 * time.Minute
	metaInstagramDeletionCompletedTTL        = 24 * time.Hour
	metaInstagramDeletionCompletedRetention  = 365 * 24 * time.Hour
	metaInstagramDeletionUnresolvedRetention = 90 * 24 * time.Hour
	metaInstagramDeletionCleanupBatch        = 500
	metaInstagramDeletionEventDigestKey      = "meta_data_deletion_event_digest"
	metaInstagramDeletionEventIssuedAtKey    = "meta_data_deletion_event_issued_at"
	metaInstagramDeletionPendingDigestKey    = "meta_data_deletion_pending_digest"
	metaInstagramDeletionPendingIssuedAtKey  = "meta_data_deletion_pending_issued_at"
	metaInstagramDeletionResolvedDigestKey   = "meta_data_deletion_resolved_digest"
	metaInstagramDeletionResolvedStateKey    = "meta_data_deletion_resolved_state"
	metaInstagramDeletionResolvedAtKey       = "meta_data_deletion_resolved_at"
	metaInstagramDeletionResolutionExact     = "exact_target"
	metaInstagramDeletionResolutionNoTarget  = "no_current_target"
	metaInstagramDeletionResolutionAmbiguous = "ambiguous_targets"
)

var (
	errMetaInstagramDeletionEventStale     = errors.New("instagram data-deletion event is stale")
	errMetaInstagramDeletionTargetNotExact = errors.New("instagram data-deletion target binding is not exact")
)

type metaInstagramDeletionResponse struct {
	URL              string `json:"url"`
	ConfirmationCode string `json:"confirmation_code"`
}

type metaInstagramDeletionClaim struct {
	Key      string
	Token    string
	Acquired bool
}

type metaInstagramDeletionJournalClaim struct {
	Event      models.MetaInstagramDataDeletionEvent
	OwnerID    uuid.UUID
	SubjectKey string
	Resolution string
}

// DeleteMetaInstagramUserData is Meta's dedicated data-deletion callback for
// the managed Instagram Login application. It is intentionally not the
// deauthorization handler: an authentic request creates a privacy workflow
// and stable status URL even when there is no current authorization to revoke.
func (a *App) DeleteMetaInstagramUserData(r *fastglue.Request) error {
	setMetaInstagramDeletionResponseHeaders(r)
	settings, err := a.metaInstagramOnboardingSettings()
	if err != nil || a.Redis == nil {
		if a != nil && a.Config != nil && a.Config.MetaInstagram.Enabled &&
			(strings.TrimSpace(a.Config.MetaInstagram.AllowedOrganizationIDs) != "" ||
				strings.TrimSpace(a.Config.MetaInstagram.QuarantinedOrganizationIDs) != "" ||
				strings.TrimSpace(a.Config.MetaInstagram.DataDeletionComplianceOrganizationID) != "") &&
			a.Config.MetaInstagram.DataDeletionComplianceOrganization() == "" {
			return sendMetaInstagramDeletionError(r, fasthttp.StatusServiceUnavailable)
		}
		return sendMetaInstagramDeletionError(r, fasthttp.StatusNotFound)
	}
	body := r.RequestCtx.PostBody()
	contentType := strings.ToLower(strings.TrimSpace(string(r.RequestCtx.Request.Header.ContentType())))
	if separator := strings.IndexByte(contentType, ';'); separator >= 0 {
		contentType = strings.TrimSpace(contentType[:separator])
	}
	if len(body) == 0 || len(body) > metaDeauthorizationMaxSignedRequest ||
		contentType != "application/x-www-form-urlencoded" {
		return sendMetaInstagramDeletionError(r, fasthttp.StatusBadRequest)
	}
	raw := strings.TrimSpace(string(r.RequestCtx.PostArgs().Peek("signed_request")))
	if raw == "" || len(raw) > metaDeauthorizationMaxSignedRequest {
		return sendMetaInstagramDeletionError(r, fasthttp.StatusBadRequest)
	}

	// Verify before touching Redis, the tenant journal, or another tenant table.
	payload, digest, err := verifyMetaMessengerSignedRequestSignature(raw, settings.AppSecret)
	if err != nil {
		return sendMetaInstagramDeletionError(r, fasthttp.StatusBadRequest)
	}
	if allowed, rateErr := a.requireMetaInstagramDeletionRateLimit(r, digest); !allowed {
		return rateErr
	}
	now := time.Now().UTC()
	journalClaim, err := a.loadOrCreateMetaInstagramDeletionEvent(
		digest, settings, payload, now,
	)
	if err != nil {
		if errors.Is(err, errMetaInstagramDeletionEventStale) {
			return sendMetaInstagramDeletionError(r, fasthttp.StatusBadRequest)
		}
		a.Log.Warn("Instagram data-deletion journal is unavailable")
		return sendMetaInstagramDeletionError(r, fasthttp.StatusServiceUnavailable)
	}
	if journalClaim.Event.State == "completed" {
		return a.sendCompletedMetaInstagramDeletion(
			r, settings.ReReplyBaseURL, journalClaim.Event,
		)
	}

	claim, err := a.acquireMetaInstagramDeletion(requestContext(r), digest)
	if err != nil {
		return sendMetaInstagramDeletionError(r, fasthttp.StatusServiceUnavailable)
	}
	if !claim.Acquired {
		return sendMetaInstagramDeletionError(r, fasthttp.StatusServiceUnavailable)
	}
	completed := false
	defer func() {
		if !completed {
			_ = a.releaseMetaInstagramDeletion(context.Background(), claim)
		}
	}()

	eventIssuedAt := time.Unix(payload.IssuedAt, 0).UTC()
	var privacyRequest models.PrivacyRequest
	persistPrivacyRequest := func(scoped *App) error {
		var mutationErr error
		privacyRequest, mutationErr = scoped.createOrResumeMetaInstagramDeletionRequest(
			journalClaim.OwnerID,
			journalClaim.Event.PlatformAppID,
			journalClaim.Event.AuthorizingUserID,
			journalClaim.SubjectKey,
			digest,
			journalClaim.Resolution,
			journalClaim.Event.IdentityHashed,
			eventIssuedAt,
			now,
		)
		return mutationErr
	}
	if journalClaim.Event.IdentityHashed {
		// Organization-set no-target/ambiguous receipts belong only to the
		// dedicated exact-purpose Instagram compliance tenant. Ordinary and
		// legacy singleton receipts continue through ordinary admission.
		err = a.withPlatformComplianceInstagramTenantApp(
			journalClaim.OwnerID, persistPrivacyRequest,
		)
	} else {
		err = a.WithCommittedTenantApp(journalClaim.OwnerID, persistPrivacyRequest)
	}
	if err != nil {
		a.Log.Warn("Instagram data-deletion privacy workflow could not be persisted")
		return sendMetaInstagramDeletionError(r, fasthttp.StatusServiceUnavailable)
	}
	if err := a.completeMetaInstagramDeletionEvent(
		journalClaim.Event, journalClaim.OwnerID, privacyRequest, time.Now().UTC(),
	); err != nil {
		a.Log.Warn("Instagram data-deletion journal completion failed")
		return sendMetaInstagramDeletionError(r, fasthttp.StatusServiceUnavailable)
	}
	completed = true
	if err := a.completeMetaInstagramDeletion(requestContext(r), claim); err != nil {
		// The durable database journal is authoritative; a subsequent retry can
		// still return the same confirmation code without repeating mutations.
		a.Log.Warn("Instagram data-deletion Redis completion lagged durable journal")
	}
	statusURL, err := metaInstagramDeletionStatusURL(settings.ReReplyBaseURL, privacyRequest.RequestNumber)
	if err != nil {
		return sendMetaInstagramDeletionError(r, fasthttp.StatusServiceUnavailable)
	}
	return sendMetaInstagramDeletionJSON(r, fasthttp.StatusOK, metaInstagramDeletionResponse{
		URL: statusURL, ConfirmationCode: privacyRequest.RequestNumber,
	})
}

// MetaInstagramDataDeletionStatus is the public, non-sensitive status page
// returned to Meta. The 128-bit confirmation code is the only lookup token;
// provider and tenant identifiers are never rendered.
func (a *App) MetaInstagramDataDeletionStatus(r *fastglue.Request) error {
	setMetaInstagramDeletionResponseHeaders(r)
	confirmationCode := strings.TrimSpace(stringPathValue(r, "confirmation_code"))
	if !validMetaInstagramDeletionConfirmationCode(confirmationCode) || a == nil || a.DB == nil ||
		a.Config == nil {
		return sendMetaInstagramDeletionText(r, fasthttp.StatusNotFound, "Request not found")
	}
	organizationIDs, err := a.metaInstagramDeletionJournalOrganizations()
	if err != nil || len(organizationIDs) == 0 {
		return sendMetaInstagramDeletionText(r, fasthttp.StatusNotFound, "Request not found")
	}
	var privacyRequest models.PrivacyRequest
	found := false
	for _, organizationID := range organizationIDs {
		var candidateJournal models.MetaInstagramDataDeletionEvent
		var candidateRequest models.PrivacyRequest
		err = database.WithTenantReadCommitted(
			a.rootApp().DB, organizationID, func(tx *gorm.DB) error {
				if queryErr := tx.Where(
					"organization_id = ? AND state = ? AND request_number = ? AND privacy_request_id IS NOT NULL",
					organizationID, "completed", confirmationCode,
				).First(&candidateJournal).Error; queryErr != nil {
					return queryErr
				}
				return tx.Where(
					"id = ? AND organization_id = ? AND request_number = ? AND type = ? AND received_channel = ? AND verification_method = ?",
					*candidateJournal.PrivacyRequestID, organizationID, confirmationCode,
					models.PrivacyRequestTypeDeletion, metaInstagramDeletionReceivedChannel,
					metaInstagramDeletionVerification,
				).First(&candidateRequest).Error
			},
		)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil || found {
			return sendMetaInstagramDeletionText(r, fasthttp.StatusNotFound, "Request not found")
		}
		privacyRequest = candidateRequest
		found = true
	}
	if !found {
		return sendMetaInstagramDeletionText(r, fasthttp.StatusNotFound, "Request not found")
	}
	displayStatus, statusReason := metaInstagramDeletionDisplayStatus(
		privacyRequest.Status, privacyRequest.DecisionReason,
	)
	reasonHTML := ""
	if statusReason != "" {
		reasonHTML = fmt.Sprintf("<p>Decision: %s</p>", html.EscapeString(statusReason))
	}
	body := fmt.Sprintf(
		"<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><meta name=\"robots\" content=\"noindex,nofollow\"><title>ReReply data deletion status</title></head><body><main><h1>Instagram data deletion request</h1><p>Confirmation code: <strong>%s</strong></p><p>Status: <strong>%s</strong></p>%s<p>Received: %s</p><p>Target completion date: %s</p></main></body></html>",
		html.EscapeString(privacyRequest.RequestNumber), html.EscapeString(displayStatus), reasonHTML,
		privacyRequest.ReceivedAt.UTC().Format("2 January 2006"),
		privacyRequest.DueAt.UTC().Format("2 January 2006"),
	)
	r.RequestCtx.Response.Header.SetContentType("text/html; charset=utf-8")
	r.RequestCtx.Response.Header.Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	r.RequestCtx.Response.SetStatusCode(fasthttp.StatusOK)
	r.RequestCtx.Response.SetBodyString(body)
	return nil
}

func (a *App) sendCompletedMetaInstagramDeletion(
	r *fastglue.Request,
	baseURL string,
	event models.MetaInstagramDataDeletionEvent,
) error {
	if event.OrganizationID == uuid.Nil || event.PrivacyRequestID == nil ||
		!validMetaInstagramDeletionConfirmationCode(event.RequestNumber) {
		return sendMetaInstagramDeletionError(r, fasthttp.StatusServiceUnavailable)
	}
	statusURL, err := metaInstagramDeletionStatusURL(baseURL, event.RequestNumber)
	if err != nil {
		return sendMetaInstagramDeletionError(r, fasthttp.StatusServiceUnavailable)
	}
	return sendMetaInstagramDeletionJSON(r, fasthttp.StatusOK, metaInstagramDeletionResponse{
		URL: statusURL, ConfirmationCode: event.RequestNumber,
	})
}

func (a *App) metaInstagramDeletionJournalOrganizations() ([]uuid.UUID, error) {
	if a == nil || a.Config == nil {
		return nil, errors.New("instagram data-deletion journal configuration is unavailable")
	}
	values := append(
		[]string(nil), a.Config.MetaInstagram.ManagedOrganizationIDs()...,
	)
	if compliance := a.Config.MetaInstagram.DataDeletionComplianceOrganization(); compliance != "" {
		values = append(values, compliance)
	} else if a.Config.MetaInstagram.UsesOrganizationSetModel() {
		return nil, errors.New("instagram data-deletion compliance tenant is unavailable")
	}
	seen := make(map[uuid.UUID]struct{}, len(values))
	organizationIDs := make([]uuid.UUID, 0, len(values))
	for _, value := range values {
		organizationID, err := uuid.Parse(value)
		if err != nil || organizationID == uuid.Nil || organizationID.String() != value {
			return nil, errors.New("instagram data-deletion journal tenant is invalid")
		}
		if _, exists := seen[organizationID]; exists {
			continue
		}
		seen[organizationID] = struct{}{}
		organizationIDs = append(organizationIDs, organizationID)
	}
	sort.Slice(organizationIDs, func(left, right int) bool {
		return organizationIDs[left].String() < organizationIDs[right].String()
	})
	if len(organizationIDs) == 0 {
		return nil, errors.New("instagram data-deletion journal tenant is unavailable")
	}
	return organizationIDs, nil
}

func metaInstagramComplianceIdentityHash(
	appSecret, field, value string,
) (string, error) {
	appSecret = strings.TrimSpace(appSecret)
	field = strings.TrimSpace(field)
	value = strings.TrimSpace(value)
	if appSecret == "" || (field != "app" && field != "subject") ||
		!validCanonicalMetaID(value) {
		return "", errors.New("instagram data-deletion compliance identity is invalid")
	}
	digest := hmac.New(sha256.New, []byte(appSecret))
	_, _ = digest.Write([]byte("managed-instagram-data-deletion-v1\x00" + field + "\x00" + value))
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (a *App) loadOrCreateMetaInstagramDeletionEvent(
	digest string,
	settings configpkg.MetaInstagramConfig,
	payload metaMessengerSignedRequestPayload,
	now time.Time,
) (metaInstagramDeletionJournalClaim, error) {
	var claim metaInstagramDeletionJournalClaim
	appID := strings.TrimSpace(settings.AppID)
	if a == nil || a.DB == nil || a.Config == nil ||
		len(strings.TrimSpace(digest)) != sha256.Size*2 ||
		!validCanonicalMetaID(appID) || !validCanonicalMetaID(payload.UserID) || payload.IssuedAt <= 0 {
		return claim, errMetaInstagramDeletionEventStale
	}
	issuedAt := time.Unix(payload.IssuedAt, 0).UTC()
	now = now.UTC()
	if issuedAt.After(now.Add(2*time.Minute)) ||
		now.Sub(issuedAt) > metaInstagramDeletionUnresolvedRetention {
		return claim, errMetaInstagramDeletionEventStale
	}
	journalOrganizations, err := a.metaInstagramDeletionJournalOrganizations()
	if err != nil {
		return claim, err
	}
	err = a.rootApp().DB.Transaction(func(tx *gorm.DB) error {
		// The app-scoped subject mutex serializes callbacks with OAuth across
		// every configured tenant. Organization locks are then acquired in one
		// canonical order before any tenant journal/account row is read.
		if lockErr := lockMetaInstagramIdentityScopeTx(tx, appID, payload.UserID); lockErr != nil {
			return lockErr
		}
		for _, organizationID := range journalOrganizations {
			if setErr := a.setMetaInstagramTenantContextTx(tx, organizationID); setErr != nil {
				return setErr
			}
			if lockErr := lockChannelAIOrganizationScopeTx(tx, organizationID); lockErr != nil {
				return lockErr
			}
		}

		var existing *models.MetaInstagramDataDeletionEvent
		for _, organizationID := range journalOrganizations {
			if setErr := a.setMetaInstagramTenantContextTx(tx, organizationID); setErr != nil {
				return setErr
			}
			var candidate models.MetaInstagramDataDeletionEvent
			queryErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
				"organization_id = ? AND digest = ?", organizationID, digest,
			).First(&candidate).Error
			if errors.Is(queryErr, gorm.ErrRecordNotFound) {
				continue
			}
			if queryErr != nil {
				return queryErr
			}
			if existing != nil {
				return errors.New("instagram data-deletion journal is duplicated across tenants")
			}
			existing = &candidate
		}
		if existing != nil {
			resolution := strings.TrimSpace(existing.TargetResolution)
			if resolution == "" {
				resolution = metaInstagramDeletionResolutionExact
			}
			switch resolution {
			case metaInstagramDeletionResolutionExact,
				metaInstagramDeletionResolutionNoTarget,
				metaInstagramDeletionResolutionAmbiguous:
			default:
				return errors.New("instagram data-deletion journal resolution is invalid")
			}
			ownerText := existing.OrganizationID.String()
			if settings.UsesOrganizationSetModel() {
				ownerIsCompliance := ownerText == settings.DataDeletionComplianceOrganization()
				if existing.IdentityHashed {
					if !ownerIsCompliance || resolution == metaInstagramDeletionResolutionExact {
						return errors.New("instagram data-deletion journal ownership is invalid")
					}
				} else {
					// A no-target receipt created while this same binary was still in
					// legacy-singleton rollout mode remains owned by that original
					// tenant for idempotent replay/status retention. New no-target and
					// ambiguous receipts in the organization-set model are always
					// HMAC-bound to the separate compliance tenant below.
					validLegacyResolution := resolution == metaInstagramDeletionResolutionExact ||
						resolution == metaInstagramDeletionResolutionNoTarget
					if ownerIsCompliance || !settings.OrganizationManaged(ownerText) ||
						!validLegacyResolution {
						return errors.New("instagram data-deletion journal ownership is invalid")
					}
				}
			} else if existing.IdentityHashed || !settings.OrganizationManaged(ownerText) ||
				resolution == metaInstagramDeletionResolutionAmbiguous {
				return errors.New("instagram data-deletion journal ownership is invalid")
			}
			expectedAppID := appID
			expectedSubjectID := payload.UserID
			if existing.IdentityHashed {
				expectedAppID, err = metaInstagramComplianceIdentityHash(
					settings.AppSecret, "app", appID,
				)
				if err != nil {
					return err
				}
				expectedSubjectID, err = metaInstagramComplianceIdentityHash(
					settings.AppSecret, "subject", payload.UserID,
				)
				if err != nil {
					return err
				}
			}
			if existing.PlatformAppID != expectedAppID ||
				existing.AuthorizingUserID != expectedSubjectID ||
				!existing.IssuedAt.UTC().Equal(issuedAt) ||
				(existing.State != "verified" && existing.State != "completed") {
				return errors.New("instagram data-deletion journal identity mismatch")
			}
			claim = metaInstagramDeletionJournalClaim{
				Event: *existing, OwnerID: existing.OrganizationID,
				SubjectKey: existing.AuthorizingUserID, Resolution: resolution,
			}
			return nil
		}

		targets, resolveErr := a.resolveMetaInstagramCallbackTargetsTx(
			tx, appID, payload.UserID,
		)
		if resolveErr != nil {
			return resolveErr
		}
		resolution := metaInstagramDeletionResolutionExact
		ownerID := uuid.Nil
		journalAppID := appID
		journalSubjectID := payload.UserID
		identityHashed := false
		var exactTarget *metaDeauthorizationTarget
		switch len(targets) {
		case 1:
			ownerID = targets[0].OrganizationID
			exactTarget = &targets[0]
		case 0:
			resolution = metaInstagramDeletionResolutionNoTarget
		default:
			resolution = metaInstagramDeletionResolutionAmbiguous
		}
		if exactTarget == nil {
			if settings.UsesOrganizationSetModel() {
				complianceText := settings.DataDeletionComplianceOrganization()
				ownerID, err = uuid.Parse(complianceText)
				if err != nil || ownerID == uuid.Nil || ownerID.String() != complianceText {
					return errors.New("instagram data-deletion compliance tenant is unavailable")
				}
				journalAppID, err = metaInstagramComplianceIdentityHash(
					settings.AppSecret, "app", appID,
				)
				if err != nil {
					return err
				}
				journalSubjectID, err = metaInstagramComplianceIdentityHash(
					settings.AppSecret, "subject", payload.UserID,
				)
				if err != nil {
					return err
				}
				identityHashed = true
			} else {
				// Reader-first rollout preserves the former singleton no-target
				// privacy owner. Legacy ambiguity remains fail closed until the
				// dedicated compliance tenant is configured.
				if len(targets) > 1 {
					return errors.New("instagram data-deletion target is ambiguous")
				}
				managed := settings.ManagedOrganizationIDs()
				if len(managed) != 1 {
					return errors.New("instagram data-deletion legacy tenant is unavailable")
				}
				ownerID, err = uuid.Parse(managed[0])
				if err != nil || ownerID == uuid.Nil {
					return errors.New("instagram data-deletion legacy tenant is unavailable")
				}
			}
		}
		if setErr := a.setMetaInstagramTenantContextTx(tx, ownerID); setErr != nil {
			return setErr
		}
		event := models.MetaInstagramDataDeletionEvent{
			Digest: digest, OrganizationID: ownerID,
			PlatformAppID: journalAppID, AuthorizingUserID: journalSubjectID,
			IssuedAt: issuedAt, VerifiedAt: now, State: "verified",
			TargetResolution: resolution, IdentityHashed: identityHashed,
			LastAttemptAt: &now,
		}
		if createErr := tx.Create(&event).Error; createErr != nil {
			return createErr
		}
		claim = metaInstagramDeletionJournalClaim{
			Event: event, OwnerID: ownerID,
			SubjectKey: journalSubjectID, Resolution: resolution,
		}
		if exactTarget == nil {
			return nil
		}
		// Exact target mutation is atomic with the first journal write. A
		// current/same-second generation becomes non-routable before commit;
		// a strictly older signed event remains privacy evidence only.
		return a.rootApp().scopedApp(tx, ownerID).applyMetaInstagramDeletionToAccount(
			ownerID, exactTarget.AccountID, appID, payload.UserID,
			digest, issuedAt, now,
		)
	})
	return claim, err
}

func (a *App) completeMetaInstagramDeletionEvent(
	event models.MetaInstagramDataDeletionEvent,
	organizationID uuid.UUID,
	privacyRequest models.PrivacyRequest,
	completedAt time.Time,
) error {
	if a == nil || a.DB == nil || event.Digest == "" || organizationID == uuid.Nil ||
		privacyRequest.ID == uuid.Nil || !validMetaInstagramDeletionConfirmationCode(privacyRequest.RequestNumber) {
		return errors.New("instagram data-deletion journal is unavailable")
	}
	completedAt = completedAt.UTC()
	updates := map[string]any{
		"state":              "completed",
		"privacy_request_id": privacyRequest.ID, "request_number": privacyRequest.RequestNumber,
		"completed_at": completedAt, "last_attempt_at": completedAt,
	}
	return database.WithTenantReadCommitted(
		a.rootApp().DB, organizationID, func(tx *gorm.DB) error {
			result := tx.Model(&models.MetaInstagramDataDeletionEvent{}).
				Where("organization_id = ? AND digest = ? AND platform_app_id = ? AND authorizing_user_id = ? AND issued_at = ? AND state = ?",
					organizationID, event.Digest, event.PlatformAppID,
					event.AuthorizingUserID, event.IssuedAt, "verified").
				Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 1 {
				return nil
			}
			var current models.MetaInstagramDataDeletionEvent
			if err := tx.Where(
				"organization_id = ? AND digest = ?", organizationID, event.Digest,
			).First(&current).Error; err != nil {
				return err
			}
			if current.State == "completed" && current.PrivacyRequestID != nil &&
				current.OrganizationID == organizationID &&
				*current.PrivacyRequestID == privacyRequest.ID &&
				current.RequestNumber == privacyRequest.RequestNumber {
				return nil
			}
			return errors.New("instagram data-deletion journal completion was superseded")
		},
	)
}

func (a *App) cleanupMetaInstagramDeletionEvents(now time.Time) error {
	if a == nil || a.DB == nil || a.Config == nil {
		return errors.New("instagram data-deletion journal is unavailable")
	}
	organizationIDs, err := a.metaInstagramDeletionJournalOrganizations()
	if err != nil {
		return err
	}
	now = now.UTC()
	for _, organizationID := range organizationIDs {
		err = database.WithTenantReadCommitted(
			a.rootApp().DB, organizationID, func(tx *gorm.DB) error {
				purpose, purposeErr := database.IsPlatformComplianceOrganization(tx, organizationID)
				if purposeErr != nil {
					return purposeErr
				}
				if purpose {
					return nil
				}
				for _, cleanup := range []struct {
					where string
					args  []any
				}{
					{
						where: "organization_id = ? AND state = ? AND completed_at IS NOT NULL AND completed_at < ?",
						args:  []any{organizationID, "completed", now.Add(-metaInstagramDeletionCompletedRetention)},
					},
					{
						where: "organization_id = ? AND state = ? AND verified_at < ?",
						args:  []any{organizationID, "verified", now.Add(-metaInstagramDeletionUnresolvedRetention)},
					},
				} {
					var digests []string
					if queryErr := tx.Model(&models.MetaInstagramDataDeletionEvent{}).
						Where(cleanup.where, cleanup.args...).Order("verified_at").
						Limit(metaInstagramDeletionCleanupBatch).Pluck("digest", &digests).Error; queryErr != nil {
						return queryErr
					}
					if len(digests) == 0 {
						continue
					}
					if deleteErr := tx.Where(
						"organization_id = ? AND digest IN ?", organizationID, digests,
					).Delete(&models.MetaInstagramDataDeletionEvent{}).Error; deleteErr != nil {
						return deleteErr
					}
				}
				return nil
			},
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *App) createOrResumeMetaInstagramDeletionRequest(
	organizationID uuid.UUID,
	journalAppID, journalUserID, subjectKey, digest, resolution string,
	identityHashed bool,
	eventIssuedAt, now time.Time,
) (models.PrivacyRequest, error) {
	if err := lockChannelAIOrganizationScopeTx(a.DB, organizationID); err != nil {
		return models.PrivacyRequest{}, err
	}
	// The Redis lease bounds public replay pressure, but the journal row is the
	// durable serializer. Lock it before the privacy request lookup so a lease
	// expiry cannot let two READ COMMITTED transactions both observe a missing
	// tenant request and create different confirmation codes.
	var journal models.MetaInstagramDataDeletionEvent
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("organization_id = ? AND digest = ?", organizationID, digest).
		First(&journal).Error; err != nil {
		return models.PrivacyRequest{}, err
	}
	journalResolution := strings.TrimSpace(journal.TargetResolution)
	if journalResolution == "" {
		journalResolution = metaInstagramDeletionResolutionExact
	}
	if journal.PlatformAppID != journalAppID ||
		journal.AuthorizingUserID != journalUserID ||
		journalResolution != resolution ||
		journal.IdentityHashed != identityHashed ||
		!journal.IssuedAt.UTC().Equal(eventIssuedAt.UTC()) {
		return models.PrivacyRequest{}, errors.New("instagram data-deletion journal identity mismatch")
	}
	if journal.State == "completed" {
		if journal.OrganizationID != organizationID || journal.PrivacyRequestID == nil {
			return models.PrivacyRequest{}, errors.New("instagram data-deletion journal completion mismatch")
		}
		var completed models.PrivacyRequest
		if err := a.DB.Where(
			"id = ? AND organization_id = ? AND verification_token_hash = ?",
			*journal.PrivacyRequestID, organizationID,
			metaInstagramDeletionEventHash(journalAppID, journalUserID, digest),
		).First(&completed).Error; err != nil {
			return models.PrivacyRequest{}, err
		}
		if completed.RequestNumber != journal.RequestNumber {
			return models.PrivacyRequest{}, errors.New("instagram data-deletion confirmation mismatch")
		}
		return completed, nil
	}
	if journal.State != "verified" {
		return models.PrivacyRequest{}, errors.New("instagram data-deletion journal is not actionable")
	}
	eventHash := metaInstagramDeletionEventHash(journalAppID, journalUserID, digest)
	var privacyRequest models.PrivacyRequest
	err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"organization_id = ? AND type = ? AND received_channel = ? AND verification_method = ? AND verification_token_hash = ?",
		organizationID, models.PrivacyRequestTypeDeletion,
		metaInstagramDeletionReceivedChannel, metaInstagramDeletionVerification, eventHash,
	).First(&privacyRequest).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return models.PrivacyRequest{}, err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		requestID := uuid.New()
		privacyRequest = models.PrivacyRequest{
			BaseModel: models.BaseModel{ID: requestID}, OrganizationID: organizationID,
			RequestNumber: "IGDEL" + strings.ToUpper(strings.ReplaceAll(requestID.String(), "-", "")),
			Type:          models.PrivacyRequestTypeDeletion, Status: models.PrivacyRequestStatusInProgress,
			SubjectType: metaInstagramDeletionSubjectType, SubjectKey: subjectKey,
			ReceivedChannel:  metaInstagramDeletionReceivedChannel,
			RequesterProfile: models.JSONB{},
			RequestDetails: models.JSONB{
				"source": "meta_instagram_managed_login", "target_resolution": resolution,
				"requires_manual_resolution": resolution == metaInstagramDeletionResolutionAmbiguous,
			},
			VerificationMethod:    metaInstagramDeletionVerification,
			VerificationTokenHash: eventHash, ReceivedAt: now.UTC(),
			DueAt: now.UTC().Add(30 * 24 * time.Hour), VerifiedAt: utcTimePointer(now),
		}
		if err := a.DB.Create(&privacyRequest).Error; err != nil {
			return models.PrivacyRequest{}, err
		}
		event := models.PrivacyRequestEvent{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID,
			PrivacyRequestID: privacyRequest.ID, EventType: "request_created",
			ToStatus:   privacyRequest.Status,
			Message:    "Managed Instagram data deletion request verified and initiated",
			Details:    models.JSONB{"verification": metaInstagramDeletionVerification},
			OccurredAt: now.UTC(),
		}
		if err := a.DB.Create(&event).Error; err != nil {
			return models.PrivacyRequest{}, err
		}
		if resolution == metaInstagramDeletionResolutionAmbiguous {
			escalation := models.PrivacyRequestEvent{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: organizationID,
				PrivacyRequestID: privacyRequest.ID, EventType: "target_resolution_required",
				ToStatus: privacyRequest.Status,
				Message:  "Managed Instagram deletion target resolution requires compliance review",
				Details: models.JSONB{
					"target_resolution": resolution,
				},
				OccurredAt: now.UTC(),
			}
			if err := a.DB.Create(&escalation).Error; err != nil {
				return models.PrivacyRequest{}, err
			}
		}
	}
	return privacyRequest, nil
}

func (a *App) applyMetaInstagramDeletionToAccount(
	organizationID, accountID uuid.UUID,
	appID, userID, digest string,
	eventIssuedAt, checkedAt time.Time,
) error {
	var account models.ChannelAccount
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
		Preload("Credentials", func(db *gorm.DB) *gorm.DB { return db.Order("version DESC") }).
		Where("id = ? AND organization_id = ? AND channel = ? AND provider = ?",
			accountID, organizationID, models.ChannelInstagram, channelapi.RelayProvider).
		First(&account).Error; err != nil {
		return err
	}
	if !exactManagedInstagramCallbackBinding(&account, appID, userID) {
		return errMetaInstagramDeletionTargetNotExact
	}
	oauth := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindOAuth, checkedAt)
	webhook := currentMetaRegistryCredential(account.Credentials, models.ChannelCredentialKindWebhook, checkedAt)
	if !metaInstagramCredentialPairGenerationValid(oauth, webhook, checkedAt) {
		if account.Status == models.ChannelAccountStatusDisconnected {
			return nil
		}
		return a.quarantineMetaInstagramDeletionAccount(
			organizationID, account, digest, eventIssuedAt,
			"data_deletion_credential_fence_missing", checkedAt,
		)
	}
	authorizedAt, err := time.Parse(
		time.RFC3339Nano, stringConfigValue(account.Metadata, metaMessengerAuthorizationGrantedAtKey),
	)
	if err != nil || eventIssuedAt.IsZero() || strings.TrimSpace(digest) == "" {
		return a.quarantineMetaInstagramDeletionAccount(
			organizationID, account, digest, eventIssuedAt,
			"data_deletion_authorization_fence_missing", checkedAt,
		)
	}
	eventSecond := eventIssuedAt.UTC().Truncate(time.Second)
	authorizedSecond := authorizedAt.UTC().Truncate(time.Second)
	credentialSecond := oauth.CreatedAt.UTC().Truncate(time.Second)
	if !oauth.CreatedAt.IsZero() && eventSecond.Before(authorizedSecond) &&
		eventSecond.Before(credentialSecond) {
		// A delayed request must never revoke a newer reconnect generation. The
		// privacy workflow still exists and will erase data for the subject.
		return nil
	}
	if oauth.CreatedAt.IsZero() || !eventSecond.After(authorizedSecond) ||
		!eventSecond.After(credentialSecond) {
		return a.quarantineMetaInstagramDeletionAccount(
			organizationID, account, digest, eventIssuedAt,
			"data_deletion_generation_ambiguous", checkedAt,
		)
	}
	mutationTime := checkedAt.UTC()
	if previous, parseErr := time.Parse(
		time.RFC3339Nano, stringConfigValue(account.Metadata, "meta_ownership_checked_at"),
	); parseErr == nil && !mutationTime.After(previous) {
		mutationTime = previous.Add(time.Nanosecond)
	}
	changed, err := a.applyMetaRegistryMutation(metaregistry.MutationRequest{
		ChannelAccountID: account.ID, CredentialID: oauth.ID, CredentialVersion: oauth.Version,
		WebhookCredentialID: webhook.ID, WebhookCredentialVersion: webhook.Version,
		Outcome: metaregistry.OwnershipRevoked, Reason: "meta_signed_data_deletion",
		CheckedAt: mutationTime,
	}, metaregistry.OwnershipRevoked)
	if err != nil {
		return err
	}
	if !changed {
		return metaregistry.ErrNotFound
	}
	if err := cancelManagedMetaQueuedWorkForAccountTx(
		a.DB, organizationID, account.ID, "managed_instagram_data_deletion",
	); err != nil {
		return err
	}
	if err := a.DB.Where("id = ? AND organization_id = ?", account.ID, organizationID).
		First(&account).Error; err != nil {
		return err
	}
	metadata := cloneJSONB(account.Metadata)
	metadata[metaInstagramDeletionEventDigestKey] = digest
	metadata[metaInstagramDeletionEventIssuedAtKey] = eventIssuedAt.UTC().Format(time.RFC3339)
	metadata["meta_activation_state"] = "data_deletion_requested"
	result := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ? AND status = ?",
			account.ID, organizationID, models.ChannelAccountStatusDisconnected).
		Update("metadata", metadata)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return metaregistry.ErrNotFound
	}
	return nil
}

func (a *App) quarantineMetaInstagramDeletionAccount(
	organizationID uuid.UUID,
	account models.ChannelAccount,
	digest string,
	eventIssuedAt time.Time,
	reason string,
	checkedAt time.Time,
) error {
	metadata := cloneJSONB(account.Metadata)
	metadata["meta_ownership_state"] = metaregistry.OwnershipStale
	metadata["meta_ownership_reason"] = reason
	metadata["meta_ownership_checked_at"] = checkedAt.UTC().Format(time.RFC3339Nano)
	metadata["meta_activation_state"] = "data_deletion_reconciliation_required"
	metadata[metaInstagramDeletionPendingDigestKey] = digest
	metadata[metaInstagramDeletionPendingIssuedAtKey] = eventIssuedAt.UTC().Format(time.RFC3339)
	config := cloneJSONB(account.Config)
	config["outbound_enabled"] = false
	config["ai_reply_enabled"] = false
	result := a.DB.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, organizationID).
		Updates(map[string]any{
			"status": models.ChannelAccountStatusDegraded, "config": config, "metadata": metadata,
			"last_error":    "Managed Instagram data deletion requires authorization reconciliation",
			"last_error_at": checkedAt.UTC(), "connected_at": nil, "updated_at": checkedAt.UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return metaregistry.ErrNotFound
	}
	if err := cancelManagedMetaQueuedWorkForAccountTx(
		a.DB, organizationID, account.ID, "managed_instagram_data_deletion_reconciliation",
	); err != nil {
		return err
	}
	actorID, err := a.metaInstagramAuditActor(account, organizationID)
	if err != nil {
		return err
	}
	return audit.LogAudit(
		a.DB, organizationID, actorID, metaRegistryAuditActor,
		"meta_channel_registry", account.ID, models.AuditActionUpdated,
		nil, nil,
		map[string]any{
			"field": "ownership_state", "old_value": stringConfigValue(account.Metadata, "meta_ownership_state"),
			"new_value": metaregistry.OwnershipStale,
		},
		map[string]any{"field": "data_deletion_reconciliation", "old_value": nil, "new_value": reason},
	)
}

func (a *App) metaInstagramAuditActor(account models.ChannelAccount, organizationID uuid.UUID) (uuid.UUID, error) {
	if account.UpdatedByID != nil && *account.UpdatedByID != uuid.Nil {
		return *account.UpdatedByID, nil
	}
	if account.CreatedByID != nil && *account.CreatedByID != uuid.Nil {
		return *account.CreatedByID, nil
	}
	var membership models.UserOrganization
	if err := a.DB.Where("organization_id = ?", organizationID).
		Order("created_at").First(&membership).Error; err != nil {
		return uuid.Nil, err
	}
	return membership.UserID, nil
}

func metaInstagramDeletionEventHash(appID, userID, digest string) string {
	hash := sha256.Sum256([]byte(strings.Join(
		[]string{"data_deletion", appID, userID, digest}, "\x00",
	)))
	return hex.EncodeToString(hash[:])
}

func metaInstagramDeletionReconciliationPending(metadata models.JSONB) bool {
	return stringConfigValue(metadata, metaInstagramDeletionPendingDigestKey) != "" ||
		stringConfigValue(metadata, metaInstagramDeletionPendingIssuedAtKey) != ""
}

func parsePendingMetaInstagramDeletionFence(
	metadata models.JSONB,
) (string, time.Time, bool, error) {
	digest := stringConfigValue(metadata, metaInstagramDeletionPendingDigestKey)
	issued := stringConfigValue(metadata, metaInstagramDeletionPendingIssuedAtKey)
	if digest == "" && issued == "" {
		return "", time.Time{}, false, nil
	}
	decoded, decodeErr := hex.DecodeString(digest)
	issuedAt, timeErr := time.Parse(time.RFC3339, issued)
	if decodeErr != nil || len(decoded) != sha256.Size || timeErr != nil || issuedAt.IsZero() {
		return "", time.Time{}, true, errMetaMessengerSubscriptionFence
	}
	return digest, issuedAt.UTC(), true, nil
}

func validMetaInstagramDeletionConfirmationCode(value string) bool {
	if len(value) != len("IGDEL")+32 || !strings.HasPrefix(value, "IGDEL") {
		return false
	}
	_, err := hex.DecodeString(value[len("IGDEL"):])
	return err == nil
}

func metaInstagramDeletionStatusURL(baseURL, confirmationCode string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		!validMetaInstagramDeletionConfirmationCode(confirmationCode) {
		return "", errors.New("instagram data-deletion status URL is unavailable")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") +
		"/api/integrations/meta/instagram/data-deletion/status/" + confirmationCode
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String(), nil
}

func metaInstagramDeletionDisplayStatus(
	status models.PrivacyRequestStatus,
	decisionReason string,
) (string, string) {
	reason := strings.TrimSpace(decisionReason)
	switch status {
	case models.PrivacyRequestStatusCompleted:
		return "Completed", ""
	case models.PrivacyRequestStatusDenied:
		if reason == "" {
			reason = "The request could not be fulfilled. Contact ReReply support with the confirmation code for the recorded justification."
		}
		return "Denied", reason
	case models.PrivacyRequestStatusCanceled:
		if reason == "" {
			reason = "The request was canceled. Contact ReReply support with the confirmation code for details."
		}
		return "Canceled", reason
	case models.PrivacyRequestStatusExpired:
		if reason == "" {
			reason = "The request expired before completion. Contact ReReply support with the confirmation code to reopen it."
		}
		return "Expired", reason
	default:
		return "Processing", ""
	}
}

func (a *App) requireMetaInstagramDeletionRateLimit(
	r *fastglue.Request,
	digest string,
) (bool, error) {
	remote := "unknown"
	if r != nil && r.RequestCtx != nil && r.RequestCtx.RemoteIP() != nil {
		remote = r.RequestCtx.RemoteIP().String()
	}
	keyDigest := sha256.Sum256([]byte(remote + "\x00" + strings.TrimSpace(digest)))
	key := "meta-instagram:data-deletion:rate:" + hex.EncodeToString(keyDigest[:])
	script := redis.NewScript(`
local n = redis.call("INCR", KEYS[1])
if n == 1 then redis.call("PEXPIRE", KEYS[1], ARGV[1]) end
return n
`)
	count, err := script.Run(
		requestContext(r), a.Redis, []string{key}, time.Minute.Milliseconds(),
	).Int64()
	if err != nil {
		return false, sendMetaInstagramDeletionError(r, fasthttp.StatusServiceUnavailable)
	}
	if count > 60 {
		return false, sendMetaInstagramDeletionError(r, fasthttp.StatusTooManyRequests)
	}
	return true, nil
}

func (a *App) acquireMetaInstagramDeletion(
	ctx context.Context,
	digest string,
) (metaInstagramDeletionClaim, error) {
	if a == nil || a.Redis == nil || strings.TrimSpace(digest) == "" {
		return metaInstagramDeletionClaim{}, errors.New("instagram data-deletion replay protection is unavailable")
	}
	claim := metaInstagramDeletionClaim{
		Key:   "meta-instagram:data-deletion:replay:" + strings.TrimSpace(digest),
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
		metaInstagramDeletionProcessingLease.Milliseconds(),
	).Text()
	if err != nil {
		return metaInstagramDeletionClaim{}, err
	}
	claim.Acquired = status == "acquired"
	if status != "acquired" && status != "processing" && status != "completed" {
		return metaInstagramDeletionClaim{}, errors.New("instagram data-deletion replay state is invalid")
	}
	return claim, nil
}

func (a *App) releaseMetaInstagramDeletion(
	ctx context.Context,
	claim metaInstagramDeletionClaim,
) error {
	if a == nil || a.Redis == nil || claim.Key == "" || claim.Token == "" {
		return errors.New("instagram data-deletion processing lease is unavailable")
	}
	script := redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)
	return script.Run(ctx, a.Redis, []string{claim.Key}, claim.Token).Err()
}

func (a *App) completeMetaInstagramDeletion(
	ctx context.Context,
	claim metaInstagramDeletionClaim,
) error {
	if a == nil || a.Redis == nil || claim.Key == "" || claim.Token == "" {
		return errors.New("instagram data-deletion processing lease is unavailable")
	}
	script := redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then return 0 end
redis.call("SET", KEYS[1], "completed", "PX", ARGV[2])
return 1
`)
	completed, err := script.Run(
		ctx, a.Redis, []string{claim.Key}, claim.Token,
		metaInstagramDeletionCompletedTTL.Milliseconds(),
	).Int64()
	if err != nil {
		return err
	}
	if completed != 1 {
		return errors.New("instagram data-deletion processing lease was superseded")
	}
	return nil
}

func setMetaInstagramDeletionResponseHeaders(r *fastglue.Request) {
	if r == nil || r.RequestCtx == nil {
		return
	}
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	r.RequestCtx.Response.Header.Set("Pragma", "no-cache")
	r.RequestCtx.Response.Header.Set("Referrer-Policy", "no-referrer")
	r.RequestCtx.Response.Header.Set("X-Content-Type-Options", "nosniff")
	r.RequestCtx.Response.Header.Set("X-Robots-Tag", "noindex, nofollow")
}

func sendMetaInstagramDeletionError(r *fastglue.Request, status int) error {
	return sendMetaInstagramDeletionJSON(r, status, map[string]string{"error": "invalid_request"})
}

func sendMetaInstagramDeletionJSON(r *fastglue.Request, status int, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	r.RequestCtx.Response.Header.SetContentType("application/json; charset=utf-8")
	r.RequestCtx.Response.SetStatusCode(status)
	r.RequestCtx.Response.SetBody(body)
	return nil
}

func sendMetaInstagramDeletionText(r *fastglue.Request, status int, body string) error {
	r.RequestCtx.Response.Header.SetContentType("text/plain; charset=utf-8")
	r.RequestCtx.Response.SetStatusCode(status)
	r.RequestCtx.Response.SetBodyString(body)
	return nil
}

func utcTimePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}
