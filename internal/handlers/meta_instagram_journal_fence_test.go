package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func metaInstagramJournalFenceTestDigest(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(digest[:])
}

func ageMetaInstagramCallbackGeneration(
	t *testing.T,
	fixture *metaInstagramLifecycleFixture,
	authorizationStartedAt, credentialCreatedAt time.Time,
) {
	t.Helper()
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata[metaMessengerAuthorizationGrantedAtKey] =
		authorizationStartedAt.UTC().Format(time.RFC3339Nano)
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("id IN ?", []uuid.UUID{fixture.oauth.ID, fixture.webhook.ID}).
		UpdateColumn("created_at", credentialCreatedAt.UTC()).Error)
	fixture.account.Metadata = metadata
	fixture.oauth.CreatedAt = credentialCreatedAt.UTC()
	fixture.webhook.CreatedAt = credentialCreatedAt.UTC()
}

func TestManagedInstagramSignedJournalCommitAtomicallyFencesCurrentGeneration(t *testing.T) {
	for _, test := range []struct {
		name  string
		apply func(
			*testing.T, metaInstagramLifecycleFixture,
			metaMessengerSignedRequestPayload, string, time.Time,
		) error
		expectStatus models.ChannelAccountStatus
	}{
		{
			name: "deauthorization",
			apply: func(
				t *testing.T, fixture metaInstagramLifecycleFixture,
				payload metaMessengerSignedRequestPayload, digest string, now time.Time,
			) error {
				event, err := fixture.app.loadOrCreateMetaInstagramDeauthorizationEvent(
					fixture.org.ID, digest, metaInstagramTestAppID, payload, now,
				)
				require.NoError(t, err)
				assert.Equal(t, "verified", event.State)
				return nil
			},
			expectStatus: models.ChannelAccountStatusDisconnected,
		},
		{
			name: "data deletion",
			apply: func(
				t *testing.T, fixture metaInstagramLifecycleFixture,
				payload metaMessengerSignedRequestPayload, digest string, now time.Time,
			) error {
				event, err := fixture.app.loadOrCreateMetaInstagramDeletionEvent(
					fixture.org.ID, digest, metaInstagramTestAppID, payload, now,
				)
				require.NoError(t, err)
				assert.Equal(t, "verified", event.State)
				return nil
			},
			expectStatus: models.ChannelAccountStatusDisconnected,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
			now := time.Now().UTC()
			ageMetaInstagramCallbackGeneration(
				t, &fixture, now.Add(-10*time.Minute), now.Add(-9*time.Minute),
			)
			issuedAt := now.Add(-time.Minute).Truncate(time.Second)
			payload := metaMessengerSignedRequestPayload{
				Algorithm: "HMAC-SHA256", IssuedAt: issuedAt.Unix(), UserID: fixture.profileID,
			}
			digest := metaInstagramJournalFenceTestDigest(
				test.name + ":current:" + fixture.org.ID.String(),
			)
			require.NoError(t, test.apply(t, fixture, payload, digest, now))

			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, test.expectStatus, account.Status)
			assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
			assert.False(t, boolConfigValue(account.Config, "ai_reply_enabled"))
			assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
			_, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).
				loadMetaRegistryBinding(metaregistry.ResolveRequest{
					Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
					Purpose: metaregistry.ResolvePurposeOutbound,
				}, time.Now().UTC())
			require.Error(t, err)

			var providerCalls atomic.Int32
			fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
				func(*http.Request) (*http.Response, error) {
					providerCalls.Add(1)
					return nil, errors.New("journal-fenced account reached provider")
				},
			)}
			fixture.app.revalidateOneMetaInstagramBinding(
				context.Background(), fixture.org.ID, fixture.account.ID, time.Now().UTC(),
			)
			assert.Zero(t, providerCalls.Load())
		})
	}
}

func TestManagedInstagramSignedJournalStrictlyOlderThanReconnectDoesNotQuarantine(t *testing.T) {
	for _, deletion := range []bool{false, true} {
		name := "deauthorization"
		if deletion {
			name = "data deletion"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			queued, _ := createMetaInstagramManualOutboxPair(t, fixture)
			now := time.Now().UTC()
			authorizationStartedAt := now.Add(-10 * time.Minute)
			credentialCreatedAt := now.Add(-9 * time.Minute)
			ageMetaInstagramCallbackGeneration(
				t, &fixture, authorizationStartedAt, credentialCreatedAt,
			)
			issuedAt := authorizationStartedAt.Add(-time.Minute).Truncate(time.Second)
			payload := metaMessengerSignedRequestPayload{
				Algorithm: "HMAC-SHA256", IssuedAt: issuedAt.Unix(), UserID: fixture.profileID,
			}
			digest := metaInstagramJournalFenceTestDigest(
				name + ":older:" + fixture.org.ID.String(),
			)
			if deletion {
				_, err := fixture.app.loadOrCreateMetaInstagramDeletionEvent(
					fixture.org.ID, digest, metaInstagramTestAppID, payload, now,
				)
				require.NoError(t, err)
			} else {
				_, err := fixture.app.loadOrCreateMetaInstagramDeauthorizationEvent(
					fixture.org.ID, digest, metaInstagramTestAppID, payload, now,
				)
				require.NoError(t, err)
			}
			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, models.ChannelAccountStatusActive, account.Status)
			assert.True(t, boolConfigValue(account.Config, "outbound_enabled"))
			var queuedAfter models.OutboxJob
			require.NoError(t, fixture.db.First(&queuedAfter, "id = ?", queued.ID).Error)
			assert.Equal(t, models.OutboxJobStatusProcessing, queuedAfter.Status)
			_, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).
				loadMetaRegistryBinding(metaregistry.ResolveRequest{
					Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
					Purpose: metaregistry.ResolvePurposeOutbound,
				}, time.Now().UTC())
			require.NoError(t, err)
		})
	}
}

func TestManagedInstagramLifecycleRequiresDurablePendingDeauthorizationJournalBeforeGraph(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	issuedAt := fixture.oauth.CreatedAt.UTC().Truncate(time.Second)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata[metaDeauthorizationPendingDigestKey] = strings.Repeat("a", 64)
	metadata[metaDeauthorizationPendingIssuedKey] = issuedAt.Format(time.RFC3339)
	metadata["meta_ownership_state"] = metaregistry.OwnershipStale
	metadata["meta_ownership_reason"] = "deauthorization_generation_ambiguous"
	metadata["meta_activation_state"] = "deauthorization_reconciliation_required"
	config := cloneJSONB(fixture.account.Config)
	config["outbound_enabled"] = false
	config["ai_reply_enabled"] = false
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Updates(map[string]any{
		"status":   models.ChannelAccountStatusDegraded,
		"metadata": metadata, "config": config,
	}).Error)
	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
		func(*http.Request) (*http.Response, error) {
			providerCalls.Add(1)
			return nil, errors.New("missing durable journal reached Graph")
		},
	)}
	fixture.app.revalidateOneMetaInstagramBinding(
		context.Background(), fixture.org.ID, fixture.account.ID, time.Now().UTC(),
	)
	assert.Zero(t, providerCalls.Load())
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
	assert.True(t, metaDeauthorizationReconciliationPending(account.Metadata))
}

func TestManagedInstagramRevalidationFinalizationRejectsUnappliedDurableJournal(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	issuedAt := fixture.oauth.CreatedAt.UTC().Truncate(time.Second)
	digest := metaInstagramJournalFenceTestDigest(
		"unapplied:" + fixture.org.ID.String(),
	)
	require.NoError(t, fixture.db.Create(&models.MetaDeauthorizationEvent{
		Digest: digest, PlatformAppID: metaInstagramTestAppID,
		AuthorizingUserID: fixture.profileID, IssuedAt: issuedAt,
		VerifiedAt: time.Now().UTC(), State: "verified",
	}).Error)
	previousCheck, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(fixture.account.Metadata, "meta_ownership_checked_at"),
	)
	require.NoError(t, err)
	snapshot := metaInstagramRevalidationSnapshot{
		OrganizationID: fixture.org.ID, Account: fixture.account,
		OAuth: fixture.oauth, Webhook: fixture.webhook,
		AccessToken: "synthetic-old-instagram-token", PreviousCheck: previousCheck,
	}
	expiresAt := *fixture.oauth.ExpiresAt
	err = fixture.app.storeMetaInstagramRevalidation(
		snapshot, "synthetic-old-instagram-token", false, &expiresAt,
		metaInstagramTokenInspection{
			AppID: metaInstagramTestAppID, UserID: fixture.profileID,
			Scopes:    append([]string(nil), metaInstagramRequiredScopes...),
			CheckedAt: time.Now().UTC(),
		},
		time.Now().UTC(),
	)
	require.ErrorIs(t, err, errMetaInstagramAuthorizationSuperseded)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, account.Status)
	assert.Equal(
		t, stringConfigValue(fixture.account.Metadata, "meta_ownership_checked_at"),
		stringConfigValue(account.Metadata, "meta_ownership_checked_at"),
	)
}
