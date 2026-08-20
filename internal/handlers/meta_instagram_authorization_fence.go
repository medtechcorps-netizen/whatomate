package handlers

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

var errMetaInstagramAuthorizationSuperseded = errors.New(
	"instagram authorization was superseded by a signed lifecycle event",
)

// metaInstagramOAuthAuthorizationStartValid treats the OAuth state issue time
// as the conservative lower bound of the authorization generation. Meta's
// signed_request timestamps have only second precision, so all comparisons
// below intentionally fail closed for events in the same second.
func metaInstagramOAuthAuthorizationStartValid(startedAt, now time.Time) bool {
	if startedAt.IsZero() {
		return false
	}
	startedAt = startedAt.UTC()
	now = now.UTC()
	return !startedAt.After(now.Add(time.Minute)) &&
		!startedAt.Before(now.Add(-metaInstagramOAuthStateTTL-metaInstagramProviderOperationLimit-2*time.Minute))
}

func metaInstagramSignedEventStrictlyPredatesGeneration(
	eventIssuedAt, authorizationStartedAt, credentialCreatedAt time.Time,
) bool {
	if eventIssuedAt.IsZero() || authorizationStartedAt.IsZero() || credentialCreatedAt.IsZero() {
		return false
	}
	eventSecond := eventIssuedAt.UTC().Truncate(time.Second)
	startSecond := authorizationStartedAt.UTC().Truncate(time.Second)
	credentialSecond := credentialCreatedAt.UTC().Truncate(time.Second)
	return eventSecond.Before(startSecond) && eventSecond.Before(credentialSecond)
}

func metaInstagramLifecycleMetadataEventTimes(
	metadata models.JSONB,
) ([]time.Time, error) {
	// A completed/pending marker without its signed event time cannot prove that
	// it predates the new authorization and therefore remains a hard fence.
	for marker, issuedKey := range map[string]string{
		"meta_deauthorized_at":                metaDeauthorizationEventIssuedAtKey,
		metaDeauthorizationPendingDigestKey:   metaDeauthorizationPendingIssuedKey,
		metaInstagramDeletionEventDigestKey:   metaInstagramDeletionEventIssuedAtKey,
		metaInstagramDeletionPendingDigestKey: metaInstagramDeletionPendingIssuedAtKey,
	} {
		if strings.TrimSpace(stringConfigValue(metadata, marker)) != "" &&
			strings.TrimSpace(stringConfigValue(metadata, issuedKey)) == "" {
			return nil, errMetaInstagramAuthorizationSuperseded
		}
	}
	keys := []string{
		metaDeauthorizationEventIssuedAtKey,
		metaDeauthorizationPendingIssuedKey,
		metaInstagramDeletionEventIssuedAtKey,
		metaInstagramDeletionPendingIssuedAtKey,
	}
	events := make([]time.Time, 0, len(keys))
	for _, key := range keys {
		raw := strings.TrimSpace(stringConfigValue(metadata, key))
		if raw == "" {
			continue
		}
		issuedAt, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || issuedAt.IsZero() {
			return nil, errMetaInstagramAuthorizationSuperseded
		}
		events = append(events, issuedAt.UTC())
	}
	return events, nil
}

// metaInstagramOAuthGenerationFence runs only inside the exact tenant
// transaction after its organizations.id mutex has been acquired. It combines
// account-local completed/pending evidence with both durable signed-callback
// journals. A journal row committed first prevents OAuth persistence/provider
// subscription. If OAuth owns the mutex first, its lower-bound timestamp is
// persisted so the callback that follows can revoke the new generation.
func (a *App) metaInstagramOAuthGenerationFence(
	organizationID uuid.UUID,
	appID, profileID string,
	authorizationStartedAt, credentialCreatedAt time.Time,
	metadata models.JSONB,
) error {
	if a == nil || a.DB == nil || a.Config == nil || a.tenantOrgID != organizationID ||
		organizationID == uuid.Nil || !validCanonicalMetaID(appID) ||
		!validCanonicalMetaID(profileID) || authorizationStartedAt.IsZero() ||
		authorizationStartedAt.After(time.Now().UTC().Add(time.Minute)) {
		return errMetaInstagramAuthorizationSuperseded
	}
	startedSecond := authorizationStartedAt.UTC().Truncate(time.Second)
	if !credentialCreatedAt.IsZero() &&
		credentialCreatedAt.UTC().Truncate(time.Second).Before(startedSecond) {
		return errMetaInstagramAuthorizationSuperseded
	}

	metadataEvents, err := metaInstagramLifecycleMetadataEventTimes(metadata)
	if err != nil {
		return errMetaInstagramAuthorizationSuperseded
	}
	for _, issuedAt := range metadataEvents {
		if credentialCreatedAt.IsZero() {
			if !issuedAt.UTC().Truncate(time.Second).Before(startedSecond) {
				return errMetaInstagramAuthorizationSuperseded
			}
			continue
		}
		if !metaInstagramSignedEventStrictlyPredatesGeneration(
			issuedAt, authorizationStartedAt, credentialCreatedAt,
		) {
			return errMetaInstagramAuthorizationSuperseded
		}
	}

	var deauthorization models.MetaDeauthorizationEvent
	err = a.DB.Select("digest").Where(
		"platform_app_id = ? AND authorizing_user_id = ? AND issued_at >= ?",
		appID, profileID, startedSecond,
	).Order("issued_at DESC").Take(&deauthorization).Error
	if err == nil {
		return errMetaInstagramAuthorizationSuperseded
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	journalOrganizations, err := a.metaInstagramDeletionJournalOrganizations()
	if err != nil {
		return err
	}
	complianceText := a.Config.MetaInstagram.DataDeletionComplianceOrganization()
	for _, journalOrganizationID := range journalOrganizations {
		if err := a.setMetaInstagramTenantContextTx(a.DB, journalOrganizationID); err != nil {
			return err
		}
		journalAppID := appID
		journalSubjectID := profileID
		identityHashed := false
		if journalOrganizationID.String() == complianceText {
			journalAppID, err = metaInstagramComplianceIdentityHash(
				a.Config.MetaInstagram.AppSecret, "app", appID,
			)
			if err != nil {
				return err
			}
			journalSubjectID, err = metaInstagramComplianceIdentityHash(
				a.Config.MetaInstagram.AppSecret, "subject", profileID,
			)
			if err != nil {
				return err
			}
			identityHashed = true
		}
		var deletion models.MetaInstagramDataDeletionEvent
		err = a.DB.Select("digest").Where(
			"organization_id = ? AND platform_app_id = ? AND authorizing_user_id = ? AND identity_hashed = ? AND issued_at >= ?",
			journalOrganizationID, journalAppID, journalSubjectID, identityHashed, startedSecond,
		).Order("issued_at DESC").Take(&deletion).Error
		if err == nil {
			_ = a.setMetaInstagramTenantContextTx(a.DB, organizationID)
			return errMetaInstagramAuthorizationSuperseded
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			_ = a.setMetaInstagramTenantContextTx(a.DB, organizationID)
			return err
		}
	}
	if err := a.setMetaInstagramTenantContextTx(a.DB, organizationID); err != nil {
		return err
	}
	return nil
}

// metaInstagramRevalidationJournalFence is the lifecycle-only final boundary.
// It runs after the organizations.id mutex and exact credential rows are
// locked. A deauthorization journal may be accepted only when it is the exact
// pending/resolved digest whose current credential generation was just checked
// through Graph; any other current-generation deauthorization or any deletion
// journal supersedes the provider result. This also fences legacy journal rows
// created before the atomic account-marker write existed.
func (a *App) metaInstagramRevalidationJournalFence(
	account models.ChannelAccount,
	oauth models.ChannelCredential,
) error {
	if a == nil || a.DB == nil || a.tenantOrgID == uuid.Nil ||
		account.OrganizationID != a.tenantOrgID || oauth.ID == uuid.Nil ||
		oauth.ChannelAccountID != account.ID {
		return errMetaInstagramAuthorizationSuperseded
	}
	appID := strings.TrimSpace(stringConfigValue(account.Metadata, "meta_platform_app_id"))
	profileID := strings.TrimSpace(
		stringConfigValue(account.Metadata, "meta_authorizing_user_id"),
	)
	startedAt, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(account.Metadata, metaMessengerAuthorizationGrantedAtKey),
	)
	if err != nil || startedAt.IsZero() || oauth.CreatedAt.IsZero() ||
		!validCanonicalMetaID(appID) || !validCanonicalMetaID(profileID) ||
		oauth.CreatedAt.UTC().Truncate(time.Second).
			Before(startedAt.UTC().Truncate(time.Second)) {
		return errMetaInstagramAuthorizationSuperseded
	}
	acceptedDigests := map[string]struct{}{}
	pendingDigest, pendingIssuedAt, pending, pendingErr :=
		parsePendingMetaDeauthorizationFence(account.Metadata)
	if pendingErr != nil {
		return errMetaInstagramAuthorizationSuperseded
	}
	if pending {
		acceptedDigests[pendingDigest] = struct{}{}
	}
	if stringConfigValue(account.Metadata, metaDeauthorizationResolvedStateKey) ==
		"current_authorization_verified" {
		if resolved := strings.TrimSpace(stringConfigValue(
			account.Metadata, metaDeauthorizationResolvedDigestKey,
		)); resolved != "" {
			acceptedDigests[resolved] = struct{}{}
		}
	}

	var deauthorizations []models.MetaDeauthorizationEvent
	if err := a.DB.Select("digest, issued_at").Where(
		"platform_app_id = ? AND authorizing_user_id = ? AND issued_at >= ?",
		appID, profileID, startedAt.UTC().Truncate(time.Second),
	).Order("issued_at, digest").Limit(3).Find(&deauthorizations).Error; err != nil {
		return err
	}
	pendingMatched := false
	for _, event := range deauthorizations {
		if _, accepted := acceptedDigests[event.Digest]; !accepted {
			return errMetaInstagramAuthorizationSuperseded
		}
		if pending && event.Digest == pendingDigest &&
			event.IssuedAt.UTC().Equal(pendingIssuedAt.UTC()) {
			pendingMatched = true
		}
	}
	if pending && !pendingMatched {
		return errMetaInstagramAuthorizationSuperseded
	}
	var deletions int64
	if err := a.DB.Model(&models.MetaInstagramDataDeletionEvent{}).Where(
		"organization_id = ? AND platform_app_id = ? AND authorizing_user_id = ? AND issued_at >= ?",
		account.OrganizationID, appID, profileID,
		startedAt.UTC().Truncate(time.Second),
	).Count(&deletions).Error; err != nil {
		return err
	}
	if deletions != 0 {
		return errMetaInstagramAuthorizationSuperseded
	}
	return nil
}
