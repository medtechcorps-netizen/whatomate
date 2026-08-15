package handlers

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestMetaInstagramSignedEventMustPredateBothGenerationFences(t *testing.T) {
	start := time.Date(2026, time.August, 15, 1, 2, 3, 500_000_000, time.UTC)
	credential := start.Add(2 * time.Second)
	assert.True(t, metaInstagramSignedEventStrictlyPredatesGeneration(
		start.Add(-time.Second), start, credential,
	))
	assert.False(t, metaInstagramSignedEventStrictlyPredatesGeneration(
		start.Truncate(time.Second), start, credential,
	), "same-second evidence must fail closed")
	assert.False(t, metaInstagramSignedEventStrictlyPredatesGeneration(
		start.Add(time.Second), start, credential,
	), "event after the OAuth lower bound cannot be treated as stale")
	assert.False(t, metaInstagramSignedEventStrictlyPredatesGeneration(
		credential, start, credential,
	), "event equal to credential creation cannot be treated as stale")
}

func rotateSyntheticMetaInstagramGeneration(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	authorizationStartedAt time.Time,
	token string,
) (metaRegistryProvisionResult, error) {
	t.Helper()
	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	var result metaRegistryProvisionResult
	err := fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
		var rotateErr error
		result, rotateErr = scoped.rotateMetaInstagramBinding(metaInstagramRotateInput{
			AccountID: fixture.account.ID, OrganizationID: fixture.org.ID,
			UserID: fixture.user.ID,
			Profile: metaInstagramProfile{
				ID: fixture.profileID, UserID: fixture.profileID, Username: "synthetic_generation_profile",
				AccountType: "MEDIA_CREATOR",
			},
			Inspection: metaInstagramTokenInspection{
				AppID: metaInstagramTestAppID, UserID: fixture.profileID,
				Scopes:    append([]string(nil), metaInstagramRequiredScopes...),
				CheckedAt: time.Now().UTC(),
			},
			AccessToken: token, AuthorizationStartedAt: authorizationStartedAt,
			TokenExpiresAt: &expiresAt,
		})
		return rotateErr
	})
	return result, err
}

func TestManagedInstagramCompletedCallbackAfterReconnectStartPreventsDelayedRotation(
	t *testing.T,
) {
	for _, callback := range []struct {
		name string
		run  func(*testing.T, metaInstagramLifecycleFixture, time.Time)
	}{
		{
			name: "deauthorization",
			run: func(t *testing.T, fixture metaInstagramLifecycleFixture, issuedAt time.Time) {
				request := newMetaInstagramDeauthorizationRequest(t, signMetaInstagramLifecycleRequest(
					t, issuedAt, fixture.profileID, metaInstagramTestAppSecret,
				))
				require.NoError(t, fixture.app.DeauthorizeMetaInstagram(request))
				assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
			},
		},
		{
			name: "data deletion",
			run: func(t *testing.T, fixture metaInstagramLifecycleFixture, issuedAt time.Time) {
				request := newMetaInstagramDeletionRequest(t, signMetaInstagramLifecycleRequest(
					t, issuedAt, fixture.profileID, metaInstagramTestAppSecret,
				))
				require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
				assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
			},
		},
	} {
		t.Run(callback.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			eventIssuedAt := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Second)
			flowStartedAt := eventIssuedAt.Add(-2 * time.Second)
			// Make the old N generation definitively older than the callback. This
			// test exercises completed-event resurrection; mixed/same-second N+1
			// evidence is covered by the quarantine test below.
			require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).Where(
				"id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID,
			).UpdateColumn("created_at", eventIssuedAt.Add(-time.Hour)).Error)
			callback.run(t, fixture, eventIssuedAt)

			_, err := rotateSyntheticMetaInstagramGeneration(
				t, fixture, flowStartedAt, "synthetic-delayed-oauth-token",
			)
			require.ErrorIs(t, err, errMetaInstagramAuthorizationSuperseded)

			var oauthCredentials []models.ChannelCredential
			require.NoError(t, fixture.db.Where(
				"organization_id = ? AND channel_account_id = ? AND kind = ?",
				fixture.org.ID, fixture.account.ID, models.ChannelCredentialKindOAuth,
			).Find(&oauthCredentials).Error)
			require.Len(t, oauthCredentials, 1, "delayed OAuth must not create N+1")
			assert.Equal(t, models.ChannelCredentialStatusRevoked, oauthCredentials[0].Status)
		})
	}
}

func TestManagedInstagramCompletedNoTargetDeletionPreventsFreshProvision(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	profileID := "700000000009951"
	eventIssuedAt := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Second)
	flowStartedAt := eventIssuedAt // same-second signed evidence is ambiguous and must fence
	request := newMetaInstagramDeletionRequest(t, signMetaInstagramLifecycleRequest(
		t, eventIssuedAt, profileID, metaInstagramTestAppSecret,
	))
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())

	input := syntheticMetaInstagramProvisionInput(fixture, profileID, uuid.New())
	input.AuthorizationStartedAt = flowStartedAt
	var provisionResult metaRegistryProvisionResult
	err := fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
		var provisionErr error
		provisionResult, provisionErr = scoped.provisionMetaRegistryBinding(input)
		return provisionErr
	})
	require.ErrorIs(t, err, errMetaInstagramAuthorizationSuperseded)
	assert.Equal(t, uuid.Nil, provisionResult.Account.ID)

	var accounts, credentials int64
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Where(
		"organization_id = ? AND channel = ? AND external_account_id = ?",
		fixture.org.ID, models.ChannelInstagram, profileID,
	).Count(&accounts).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).Where(
		"organization_id = ? AND channel_account_id NOT IN ?",
		fixture.org.ID, []uuid.UUID{fixture.account.ID},
	).Count(&credentials).Error)
	assert.Zero(t, accounts)
	assert.Zero(t, credentials)
}

func TestManagedInstagramRotationWinnerIsNonRoutableAfterEventFollowingFlowStart(t *testing.T) {
	for _, callback := range []struct {
		name string
		run  func(*testing.T, metaInstagramLifecycleFixture, time.Time)
	}{
		{
			name: "deauthorization",
			run: func(t *testing.T, fixture metaInstagramLifecycleFixture, issuedAt time.Time) {
				request := newMetaInstagramDeauthorizationRequest(t, signMetaInstagramLifecycleRequest(
					t, issuedAt, fixture.profileID, metaInstagramTestAppSecret,
				))
				require.NoError(t, fixture.app.DeauthorizeMetaInstagram(request))
				assert.Equal(t, fasthttp.StatusServiceUnavailable, request.RequestCtx.Response.StatusCode(),
					"ambiguous deauthorization remains unacknowledged until reconciliation")
			},
		},
		{
			name: "data deletion",
			run: func(t *testing.T, fixture metaInstagramLifecycleFixture, issuedAt time.Time) {
				request := newMetaInstagramDeletionRequest(t, signMetaInstagramLifecycleRequest(
					t, issuedAt, fixture.profileID, metaInstagramTestAppSecret,
				))
				require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
				assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
			},
		},
	} {
		t.Run(callback.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			flowStartedAt := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
			result, err := rotateSyntheticMetaInstagramGeneration(
				t, fixture, flowStartedAt, "synthetic-oauth-winner-token",
			)
			require.NoError(t, err)
			require.NotEqual(t, fixture.oauth.ID, result.OAuthCredentialID)

			// Meta timestamps are second-granular. This event is strictly after the
			// OAuth lower bound even though it predates token inspection/persistence.
			callback.run(t, fixture, flowStartedAt.Add(2*time.Second))

			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.NotEqual(t, models.ChannelAccountStatusActive, account.Status)
			assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
			assert.True(t,
				metaDeauthorizationReconciliationPending(account.Metadata) ||
					metaInstagramDeletionReconciliationPending(account.Metadata),
				"mixed OAuth/event generation must remain durably quarantined",
			)
			_, leaseErr := fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
				metaregistry.ResolveRequest{
					Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
					Purpose: metaregistry.ResolvePurposeOutbound,
				},
				time.Now().UTC(),
			)
			require.ErrorIs(t, leaseErr, metaregistry.ErrStaleBinding)

			var providerCalls int
			providerErr := fixture.app.withLockedMetaInstagramSubscriptionProviderAttempt(
				t.Context(), fixture.org.ID, fixture.account.ID,
				result.SubscriptionOperation,
				metaMessengerSubscriptionSubscribePending,
				func(models.ChannelAccount, string) error {
					providerCalls++
					return nil
				},
			)
			require.ErrorIs(t, providerErr, errMetaMessengerSubscriptionFence)
			assert.Zero(t, providerCalls, "quarantined generation must not subscribe")
		})
	}
}
