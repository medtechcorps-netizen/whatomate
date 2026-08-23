package handlers

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newMetaMessengerSubscriptionFenceFixture(
	t *testing.T,
	pageID string,
) (*App, metaRegistryFixture) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	fixture := createMetaRegistryFixture(t, db, organization.ID, models.ChannelMessenger, pageID)
	configured := metaRegistryTestApp(db, organization.ID)
	return &App{DB: db, Log: testutil.NopLogger(), Config: configured.Config}, fixture
}

func TestMetaMessengerAuthorizationDurabilityMetadataMarksDevelopmentUserTokens(t *testing.T) {
	expiresAt := time.Now().UTC().Add(time.Hour)
	metadata := models.JSONB{}
	markMetaMessengerAuthorizationDurability(metadata, metaMessengerTokenKindUser, &expiresAt)
	assert.Equal(t, metaMessengerTokenKindUser, stringConfigValue(metadata, metaMessengerAuthorizationTokenKindKey))
	assert.Equal(t, "expiring", stringConfigValue(metadata, metaMessengerAuthorizationStateKey))
	assert.True(t, boolConfigValue(metadata, metaMessengerAuthorizationReconnectRequiredKey))
	assert.Equal(t, expiresAt.Format(time.RFC3339Nano), stringConfigValue(metadata, metaMessengerAuthorizationExpiresAtKey))

	markMetaMessengerAuthorizationDurability(metadata, metaMessengerTokenKindSystemUser, nil)
	assert.Equal(t, metaMessengerTokenKindSystemUser, stringConfigValue(metadata, metaMessengerAuthorizationTokenKindKey))
	assert.Equal(t, "durable", stringConfigValue(metadata, metaMessengerAuthorizationStateKey))
	assert.False(t, boolConfigValue(metadata, metaMessengerAuthorizationReconnectRequiredKey))
	_, hasExpiry := metadata[metaMessengerAuthorizationExpiresAtKey]
	assert.False(t, hasExpiry)
}

func TestRotateMetaMessengerBindingRejectsUnprovenBusinessAuthority(t *testing.T) {
	organizationID := uuid.New()
	base := metaMessengerRotateInput{
		AccountID:      uuid.New(),
		OrganizationID: organizationID,
		UserID:         uuid.New(),
		Page: metaMessengerStoredPage{metaMessengerPageSummary: metaMessengerPageSummary{
			BusinessID: "200000000000001",
			PageID:     "700000000000001",
		}},
		Platform: metaMessengerPlatformUser{
			UserID:           "900000000000001",
			TokenKind:        metaMessengerTokenKindSystemUser,
			ClientBusinessID: "200000000000001",
		},
		Inspection: metaMessengerTokenInspection{
			Type:      metaMessengerTokenKindSystemUser,
			UserID:    "900000000000001",
			CheckedAt: time.Now().UTC(),
		},
		BusinessAuthorityVerified: true,
	}
	for _, testCase := range []struct {
		name   string
		mutate func(*metaMessengerRotateInput)
	}{
		{
			name: "inspection token kind mismatch",
			mutate: func(input *metaMessengerRotateInput) {
				input.Inspection.Type = metaMessengerTokenKindUser
			},
		},
		{
			name: "inspection identity mismatch",
			mutate: func(input *metaMessengerRotateInput) {
				input.Inspection.UserID = "900000000000099"
			},
		},
		{
			name: "client business mismatch",
			mutate: func(input *metaMessengerRotateInput) {
				input.Platform.ClientBusinessID = "200000000000099"
			},
		},
		{
			name: "missing exact edge proof",
			mutate: func(input *metaMessengerRotateInput) {
				input.BusinessAuthorityVerified = false
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input := base
			testCase.mutate(&input)
			app := &App{tenantOrgID: organizationID}
			_, err := app.rotateMetaMessengerBinding(input)
			require.ErrorIs(t, err, metaregistry.ErrInvalidRequest)
		})
	}
}

func claimMetaMessengerReconnectForTest(
	t *testing.T,
	root *App,
	fixture metaRegistryFixture,
	operationID uuid.UUID,
	expiresAt time.Time,
) (metaRegistryProvisionResult, error) {
	t.Helper()
	checkedAt := time.Now().UTC()
	var result metaRegistryProvisionResult
	err := root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
		var rotateErr error
		result, rotateErr = scoped.rotateMetaMessengerBinding(metaMessengerRotateInput{
			AccountID:      fixture.account.ID,
			OrganizationID: fixture.account.OrganizationID,
			UserID:         fixture.userID,
			Page: metaMessengerStoredPage{
				metaMessengerPageSummary: metaMessengerPageSummary{
					BusinessID: "business-" + fixture.account.ExternalAccountID,
					PageID:     fixture.account.ExternalAccountID,
					PageName:   "Fence test Page",
				},
				OwnershipVerifiedAt: checkedAt.Add(time.Nanosecond),
			},
			Platform: metaMessengerPlatformUser{
				UserID:           "fence-test-user",
				TokenKind:        metaMessengerTokenKindSystemUser,
				ClientBusinessID: "business-" + fixture.account.ExternalAccountID,
			},
			Inspection: metaMessengerTokenInspection{
				Type:      metaMessengerTokenKindSystemUser,
				UserID:    "fence-test-user",
				CheckedAt: checkedAt,
				Scopes:    append([]string(nil), metaMessengerRequiredScopes...),
			},
			BusinessAuthorityVerified:      true,
			PageToken:                      "new-page-token",
			AuthorityToken:                 "new-authority-token",
			SubscriptionOperationID:        operationID,
			SubscriptionOperationExpiresAt: expiresAt,
		})
		return rotateErr
	})
	return result, err
}

func claimMetaMessengerDisconnectForTest(
	t *testing.T,
	root *App,
	fixture metaRegistryFixture,
	operationID uuid.UUID,
	expiresAt time.Time,
) (metaMessengerDisconnectClaim, error) {
	t.Helper()
	var claim metaMessengerDisconnectClaim
	err := root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
		var claimErr error
		claim, claimErr = scoped.claimMetaMessengerDisconnect(
			fixture.account.OrganizationID,
			fixture.userID,
			fixture.account.ID,
			fixture.account.ExternalAccountID,
			operationID,
			expiresAt,
		)
		return claimErr
	})
	return claim, err
}

func expireMetaMessengerAmbiguityFenceForTest(t *testing.T, root *App, accountID uuid.UUID) {
	t.Helper()
	var account models.ChannelAccount
	require.NoError(t, root.DB.First(&account, "id = ?", accountID).Error)
	metadata := cloneJSONB(account.Metadata)
	expired := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	metadata[metaMessengerSubscriptionOperationExpiresKey] = expired
	metadata[metaMessengerSubscriptionFencedOperationEndKey] = expired
	require.NoError(t, root.DB.Model(&models.ChannelAccount{}).
		Where("id = ?", accountID).
		Update("metadata", metadata).Error)
}

func TestMetaMessengerNewPageClaimIsDurableAndGloballyUniqueBeforeProviderMutation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	firstOrganization := testutil.CreateTestOrganization(t, db)
	secondOrganization := testutil.CreateTestOrganization(t, db)
	firstUser := testutil.CreateTestUser(t, db, firstOrganization.ID)
	secondUser := testutil.CreateTestUser(t, db, secondOrganization.ID)
	root := &App{
		DB:  db,
		Log: testutil.NopLogger(),
		Config: &config.Config{
			App: config.AppConfig{EncryptionKey: metaRegistryTestEncryptionKey},
		},
	}
	pageID := "780000000000101"
	now := time.Now().UTC()
	input := func(organizationID, userID uuid.UUID, operationID uuid.UUID) metaRegistryProvisionInput {
		return metaRegistryProvisionInput{
			OrganizationID:                 organizationID,
			UserID:                         userID,
			Channel:                        models.ChannelMessenger,
			Name:                           "Durable Page " + organizationID.String(),
			ExternalAccountID:              pageID,
			WebhookApp:                     "messenger",
			PlatformAppID:                  "123",
			MetaBusinessID:                 "business-" + pageID,
			AuthorizingMetaUserID:          "user-" + pageID,
			AuthorizationTokenKind:         metaMessengerTokenKindSystemUser,
			BusinessAuthorityVerified:      true,
			GrantedScopes:                  append([]string(nil), metaMessengerRequiredScopes...),
			AccessToken:                    "page-token",
			AuthorityToken:                 "authority-token",
			OwnershipCheckedAt:             now,
			ReReplyBaseURL:                 "https://app.example.test",
			RelayBaseURL:                   "https://relay.example.test",
			SubscriptionOperationID:        operationID,
			SubscriptionOperationExpiresAt: now.Add(metaMessengerSubscriptionOperationLease),
		}
	}

	var first metaRegistryProvisionResult
	err := root.WithCommittedTenantApp(firstOrganization.ID, func(scoped *App) error {
		var provisionErr error
		first, provisionErr = scoped.provisionMetaRegistryBinding(
			input(firstOrganization.ID, firstUser.ID, uuid.New()),
		)
		return provisionErr
	})
	require.NoError(t, err)

	var losingProviderMutations atomic.Int32
	err = root.WithCommittedTenantApp(secondOrganization.ID, func(scoped *App) error {
		_, provisionErr := scoped.provisionMetaRegistryBinding(
			input(secondOrganization.ID, secondUser.ID, uuid.New()),
		)
		if provisionErr == nil {
			losingProviderMutations.Add(1)
		}
		return provisionErr
	})
	require.Error(t, err)
	assert.Zero(t, losingProviderMutations.Load(), "the losing Page claim must not reach Meta")

	var stored models.ChannelAccount
	require.NoError(t, db.First(&stored, "id = ?", first.Account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, stored.Status)
	operation, ok := metaMessengerSubscriptionOperationFromMetadata(stored.Metadata)
	require.True(t, ok)
	assert.Equal(t, metaMessengerSubscriptionSubscribePending, operation.State)
	assert.Equal(t, metaMessengerSubscriptionRemoteUnknown, stringConfigValue(
		stored.Metadata,
		metaMessengerSubscriptionRemoteStateKey,
	))
}

func TestMetaMessengerProviderSuccessWithStaleFinalizerKeepsRecoverablePendingClaim(t *testing.T) {
	root, fixture := newMetaMessengerSubscriptionFenceFixture(t, "780000000000102")
	expiresAt := time.Now().UTC().Add(metaMessengerSubscriptionOperationLease)
	result, err := claimMetaMessengerReconnectForTest(t, root, fixture, uuid.New(), expiresAt)
	require.NoError(t, err)
	require.NoError(t, root.preflightMetaMessengerSubscriptionOperation(
		fixture.account.OrganizationID,
		fixture.account.ID,
		result.SubscriptionOperation,
		metaMessengerSubscriptionSubscribePending,
		time.Now().UTC(),
	))

	var providerMutations atomic.Int32
	providerMutations.Add(1)
	staleFinalizer := result.SubscriptionOperation
	staleFinalizer.WebhookVersion++
	err = root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
		_, finalizeErr := scoped.finalizeMetaMessengerSubscribeOperation(
			fixture.account.OrganizationID,
			fixture.userID,
			fixture.account.ID,
			staleFinalizer,
			time.Now().UTC(),
		)
		return finalizeErr
	})
	require.ErrorIs(t, err, errMetaMessengerSubscriptionFence)
	assert.Equal(t, int32(1), providerMutations.Load())

	var stored models.ChannelAccount
	require.NoError(t, root.DB.First(&stored, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, stored.Status)
	storedOperation, ok := metaMessengerSubscriptionOperationFromMetadata(stored.Metadata)
	require.True(t, ok)
	assert.Equal(t, result.SubscriptionOperation, storedOperation)
	assert.Equal(t, metaMessengerSubscriptionRemoteUnknown, stringConfigValue(
		stored.Metadata,
		metaMessengerSubscriptionRemoteStateKey,
	))
	require.NoError(t, root.preflightMetaMessengerSubscriptionOperation(
		fixture.account.OrganizationID,
		fixture.account.ID,
		result.SubscriptionOperation,
		metaMessengerSubscriptionSubscribePending,
		time.Now().UTC(),
	), "the original durable claim remains available for reconciliation")
}

func TestMetaMessengerReconnectAndDisconnectOperationsExcludeEachOtherInBothOrders(t *testing.T) {
	t.Run("reconnect owns lease before disconnect", func(t *testing.T) {
		root, fixture := newMetaMessengerSubscriptionFenceFixture(t, "780000000000103")
		reconnect, err := claimMetaMessengerReconnectForTest(
			t,
			root,
			fixture,
			uuid.New(),
			time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
		)
		require.NoError(t, err)

		providerEntered := make(chan struct{})
		releaseProvider := make(chan struct{})
		providerDone := make(chan error, 1)
		go func() {
			if preflightErr := root.preflightMetaMessengerSubscriptionOperation(
				fixture.account.OrganizationID,
				fixture.account.ID,
				reconnect.SubscriptionOperation,
				metaMessengerSubscriptionSubscribePending,
				time.Now().UTC(),
			); preflightErr != nil {
				providerDone <- preflightErr
				return
			}
			close(providerEntered)
			<-releaseProvider
			providerDone <- root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
				_, finalizeErr := scoped.finalizeMetaMessengerSubscribeOperation(
					fixture.account.OrganizationID,
					fixture.userID,
					fixture.account.ID,
					reconnect.SubscriptionOperation,
					time.Now().UTC(),
				)
				return finalizeErr
			})
		}()
		<-providerEntered

		var losingUnsubscribeMutations atomic.Int32
		_, err = claimMetaMessengerDisconnectForTest(
			t,
			root,
			fixture,
			uuid.New(),
			time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
		)
		if err == nil {
			losingUnsubscribeMutations.Add(1)
		}
		require.ErrorIs(t, err, errMetaMessengerSubscriptionFence)
		assert.Zero(t, losingUnsubscribeMutations.Load())
		close(releaseProvider)
		require.NoError(t, <-providerDone)
	})

	t.Run("disconnect owns lease before reconnect", func(t *testing.T) {
		root, fixture := newMetaMessengerSubscriptionFenceFixture(t, "780000000000104")
		disconnect, err := claimMetaMessengerDisconnectForTest(
			t,
			root,
			fixture,
			uuid.New(),
			time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
		)
		require.NoError(t, err)

		providerEntered := make(chan struct{})
		releaseProvider := make(chan struct{})
		providerDone := make(chan error, 1)
		go func() {
			if preflightErr := root.preflightMetaMessengerSubscriptionOperation(
				fixture.account.OrganizationID,
				fixture.account.ID,
				disconnect.Operation,
				metaMessengerSubscriptionUnsubscribePending,
				time.Now().UTC(),
			); preflightErr != nil {
				providerDone <- preflightErr
				return
			}
			close(providerEntered)
			<-releaseProvider
			providerDone <- root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
				return scoped.finalizeMetaMessengerDisconnect(
					fixture.account.OrganizationID,
					disconnect.Operation,
					fixture.account.ID,
					time.Now().UTC(),
				)
			})
		}()
		<-providerEntered

		var losingSubscribeMutations atomic.Int32
		_, err = claimMetaMessengerReconnectForTest(
			t,
			root,
			fixture,
			uuid.New(),
			time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
		)
		if err == nil {
			losingSubscribeMutations.Add(1)
		}
		require.ErrorIs(t, err, errMetaMessengerSubscriptionFence)
		assert.Zero(t, losingSubscribeMutations.Load())
		close(releaseProvider)
		require.NoError(t, <-providerDone)
	})
}

func TestMetaMessengerExpiredPendingOperationStillRequiresProviderReconciliation(t *testing.T) {
	root, fixture := newMetaMessengerSubscriptionFenceFixture(t, "780000000000105")
	disconnect, err := claimMetaMessengerDisconnectForTest(
		t,
		root,
		fixture,
		uuid.New(),
		time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
	)
	require.NoError(t, err)

	var stored models.ChannelAccount
	require.NoError(t, root.DB.First(&stored, "id = ?", fixture.account.ID).Error)
	metadata := cloneJSONB(stored.Metadata)
	metadata[metaMessengerSubscriptionOperationExpiresKey] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	require.NoError(t, root.DB.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).
		Update("metadata", metadata).Error)

	var reconnectProviderMutations atomic.Int32
	_, err = claimMetaMessengerReconnectForTest(
		t,
		root,
		fixture,
		uuid.New(),
		time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
	)
	if err == nil {
		reconnectProviderMutations.Add(1)
	}
	require.ErrorIs(t, err, errMetaMessengerSubscriptionFence)
	assert.Zero(t, reconnectProviderMutations.Load())

	var staleUnsubscribeMutations atomic.Int32
	err = root.preflightMetaMessengerSubscriptionOperation(
		fixture.account.OrganizationID,
		fixture.account.ID,
		disconnect.Operation,
		metaMessengerSubscriptionUnsubscribePending,
		time.Now().UTC(),
	)
	if err == nil {
		staleUnsubscribeMutations.Add(1)
	}
	require.ErrorIs(t, err, errMetaMessengerSubscriptionFence)
	assert.Zero(t, staleUnsubscribeMutations.Load(), "an expired unacknowledged provider claim stays quarantined")
}

func TestMetaMessengerDisconnectClaimIsNonRoutableBeforeProviderMutation(t *testing.T) {
	root, fixture := newMetaMessengerSubscriptionFenceFixture(t, "780000000000110")
	config := cloneJSONB(fixture.account.Config)
	config["outbound_enabled"] = true
	config["ai_reply_enabled"] = true
	require.NoError(t, root.DB.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).
		Update("config", config).Error)

	disconnect, err := claimMetaMessengerDisconnectForTest(
		t,
		root,
		fixture,
		uuid.New(),
		time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
	)
	require.NoError(t, err)

	assertQuarantined := func() {
		t.Helper()
		var stored models.ChannelAccount
		require.NoError(t, root.DB.First(&stored, "id = ?", fixture.account.ID).Error)
		assert.Equal(t, models.ChannelAccountStatusPending, stored.Status)
		assert.False(t, boolConfigValue(stored.Config, "outbound_enabled"))
		assert.False(t, boolConfigValue(stored.Config, "ai_reply_enabled"))
		for _, purpose := range []string{
			metaregistry.ResolvePurposeHealth,
			metaregistry.ResolvePurposeInbound,
			metaregistry.ResolvePurposeOutbound,
		} {
			err := root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
				_, resolveErr := scoped.loadMetaRegistryBinding(metaregistry.ResolveRequest{
					Channel: models.ChannelMessenger, ExternalAccountID: fixture.account.ExternalAccountID,
					Purpose: purpose,
				}, time.Now().UTC())
				return resolveErr
			})
			require.ErrorIs(t, err, metaregistry.ErrStaleBinding, purpose)
		}
	}
	assertQuarantined()

	require.NoError(t, root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
		return scoped.recordMetaMessengerSubscriptionOperationFailure(
			fixture.account.OrganizationID,
			fixture.account.ID,
			disconnect.Operation,
			metaMessengerSubscriptionUnsubscribePending,
			metaMessengerSubscriptionUnsubscribeFailed,
			time.Now().UTC(),
		)
	}))
	assertQuarantined()
}

func TestMetaMessengerAmbiguousProviderTimeoutCannotBeOvertakenByOppositeOperation(t *testing.T) {
	t.Run("late subscribe is ordered before final disconnect", func(t *testing.T) {
		root, fixture := newMetaMessengerSubscriptionFenceFixture(t, "780000000000106")
		reconnect, err := claimMetaMessengerReconnectForTest(
			t,
			root,
			fixture,
			uuid.New(),
			time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
		)
		require.NoError(t, err)
		require.NoError(t, root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
			return scoped.recordMetaMessengerSubscriptionOperationFailure(
				fixture.account.OrganizationID,
				fixture.account.ID,
				reconnect.SubscriptionOperation,
				metaMessengerSubscriptionSubscribePending,
				metaMessengerSubscriptionSubscribeFailed,
				time.Now().UTC(),
			)
		}))

		var oppositeProviderMutations atomic.Int32
		_, err = claimMetaMessengerDisconnectForTest(
			t,
			root,
			fixture,
			uuid.New(),
			time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
		)
		if err == nil {
			oppositeProviderMutations.Add(1)
		}
		require.ErrorIs(t, err, errMetaMessengerSubscriptionFence)
		assert.Zero(t, oppositeProviderMutations.Load())

		remoteSubscribed := true // the timed-out POST commits after the local lease expires
		expireMetaMessengerAmbiguityFenceForTest(t, root, fixture.account.ID)
		_, err = claimMetaMessengerDisconnectForTest(
			t,
			root,
			fixture,
			uuid.New(),
			time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
		)
		require.ErrorIs(t, err, errMetaMessengerSubscriptionFence)
		assert.True(t, remoteSubscribed)
	})

	t.Run("late delete is ordered before final reconnect", func(t *testing.T) {
		root, fixture := newMetaMessengerSubscriptionFenceFixture(t, "780000000000107")
		disconnect, err := claimMetaMessengerDisconnectForTest(
			t,
			root,
			fixture,
			uuid.New(),
			time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
		)
		require.NoError(t, err)
		require.NoError(t, root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
			return scoped.recordMetaMessengerSubscriptionOperationFailure(
				fixture.account.OrganizationID,
				fixture.account.ID,
				disconnect.Operation,
				metaMessengerSubscriptionUnsubscribePending,
				metaMessengerSubscriptionUnsubscribeFailed,
				time.Now().UTC(),
			)
		}))

		var oppositeProviderMutations atomic.Int32
		_, err = claimMetaMessengerReconnectForTest(
			t,
			root,
			fixture,
			uuid.New(),
			time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
		)
		if err == nil {
			oppositeProviderMutations.Add(1)
		}
		require.ErrorIs(t, err, errMetaMessengerSubscriptionFence)
		assert.Zero(t, oppositeProviderMutations.Load())

		remoteSubscribed := false // the timed-out DELETE commits after the local lease expires
		expireMetaMessengerAmbiguityFenceForTest(t, root, fixture.account.ID)
		_, err = claimMetaMessengerReconnectForTest(
			t,
			root,
			fixture,
			uuid.New(),
			time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
		)
		require.ErrorIs(t, err, errMetaMessengerSubscriptionFence)
		assert.False(t, remoteSubscribed)
	})
}

func TestMetaMessengerPendingCrashRequiresExactProviderReconciliationBeforeOppositeClaim(t *testing.T) {
	root, fixture := newMetaMessengerSubscriptionFenceFixture(t, "780000000000109")
	reconnect, err := claimMetaMessengerReconnectForTest(
		t,
		root,
		fixture,
		uuid.New(),
		time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
	)
	require.NoError(t, err)

	// The process crashes after the committed claim and provider call but
	// before either finalization or failure metadata. Expiry alone must not
	// let the opposite mutation overtake that potentially late POST.
	expireMetaMessengerAmbiguityFenceForTest(t, root, fixture.account.ID)
	_, err = claimMetaMessengerDisconnectForTest(
		t,
		root,
		fixture,
		uuid.New(),
		time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
	)
	require.ErrorIs(t, err, errMetaMessengerSubscriptionFence)

	remoteSubscribed := true // old POST committed after the local timeout
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			remoteSubscribed = true
			_, _ = w.Write([]byte(`{"success":true}`))
		case http.MethodGet:
			fields := ""
			if remoteSubscribed {
				fields = `{"id":"123","subscribed_fields":["messages"]}`
			}
			_, _ = w.Write([]byte(`{"data":[` + fields + `]}`))
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer graph.Close()
	root.Config.MetaRegistry.Enabled = true
	root.Config.MetaMessenger.Enabled = true
	root.Config.MetaMessenger.AppID = "123"
	root.Config.MetaMessenger.ConfigID = "456"
	root.Config.MetaMessenger.AppSecret = metaLifecycleTestAppSecret
	root.Config.MetaMessenger.GraphAPIVersion = "v25.0"
	root.Config.MetaMessenger.GraphBaseURL = "https://graph.meta.test"
	root.Config.MetaMessenger.ReReplyBaseURL = "https://app.example.test"
	root.Config.MetaMessenger.RelayBaseURL = "https://relay.example.test"
	root.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		"https://graph.meta.test": graph,
	})

	var claim metaMessengerReconciliationClaim
	require.NoError(t, database.WithTenantReadCommitted(root.DB, fixture.account.OrganizationID, func(tx *gorm.DB) error {
		var loadErr error
		claim, loadErr = root.scopedApp(tx, fixture.account.OrganizationID).
			loadMetaMessengerReconciliationClaim(fixture.account.OrganizationID, fixture.account.ID)
		return loadErr
	}))
	require.Equal(t, reconnect.SubscriptionOperation.ID, claim.Operation.ID)
	require.NoError(t, root.subscribeMetaMessengerPage(t.Context(), claim.Account.ExternalAccountID, claim.PageToken))
	confirmedAt := time.Now().UTC()
	require.NoError(t, root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
		return scoped.acknowledgeMetaMessengerSubscriptionReconciliation(
			fixture.account.OrganizationID,
			fixture.userID,
			fixture.account.ID,
			claim.Operation,
			confirmedAt,
		)
	}))
	require.NoError(t, root.WithCommittedTenantApp(fixture.account.OrganizationID, func(scoped *App) error {
		_, finalizeErr := scoped.finalizeMetaMessengerSubscribeOperation(
			fixture.account.OrganizationID,
			fixture.userID,
			fixture.account.ID,
			claim.Operation,
			confirmedAt,
		)
		return finalizeErr
	}))

	var reconciled models.ChannelAccount
	require.NoError(t, root.DB.First(&reconciled, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, metaMessengerSubscriptionSubscribeComplete, stringConfigValue(
		reconciled.Metadata,
		metaMessengerSubscriptionOperationStateKey,
	))
	assert.Empty(t, stringConfigValue(reconciled.Metadata, metaMessengerSubscriptionFencedOperationIDKey))
	_, err = claimMetaMessengerDisconnectForTest(
		t,
		root,
		fixture,
		uuid.New(),
		time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
	)
	require.NoError(t, err, "opposite intent is available only after exact provider reconciliation")
}
