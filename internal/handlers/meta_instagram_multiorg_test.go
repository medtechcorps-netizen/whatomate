package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/platformcompliance"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

func configureMetaInstagramOrganizationSet(
	fixture metaInstagramLifecycleFixture,
	active, quarantined []uuid.UUID,
	compliance uuid.UUID,
) {
	toText := func(values []uuid.UUID) string {
		result := make([]string, 0, len(values))
		for _, value := range values {
			result = append(result, value.String())
		}
		return strings.Join(result, ",")
	}
	fixture.app.Config.MetaInstagram.AllowedOrganizationID = ""
	fixture.app.Config.MetaInstagram.AllowedOrganizationIDs = toText(active)
	fixture.app.Config.MetaInstagram.QuarantinedOrganizationIDs = toText(quarantined)
	fixture.app.Config.MetaInstagram.DataDeletionComplianceOrganizationID = compliance.String()
}

func setMetaInstagramComplianceTenantMarker(
	t *testing.T,
	db *gorm.DB,
	organization *models.Organization,
	value any,
) {
	t.Helper()
	settings := cloneJSONB(organization.Settings)
	if settings == nil {
		settings = models.JSONB{}
	}
	if value == nil {
		delete(settings, metaInstagramDataDeletionComplianceTenantMarkerKey)
	} else {
		settings[metaInstagramDataDeletionComplianceTenantMarkerKey] = value
	}
	updateMetaInstagramComplianceOrganizationFixture(t, db, organization.ID, map[string]any{
		"settings": settings,
	})
	organization.Settings = settings
}

func setPlatformCompliancePurposeMarker(
	t *testing.T,
	db *gorm.DB,
	organization *models.Organization,
	value any,
) {
	t.Helper()
	settings := cloneJSONB(organization.Settings)
	if settings == nil {
		settings = models.JSONB{}
	}
	if value == nil {
		delete(settings, platformcompliance.PurposeMarkerKey)
	} else {
		settings[platformcompliance.PurposeMarkerKey] = value
	}
	updateMetaInstagramComplianceOrganizationFixture(t, db, organization.ID, map[string]any{
		"settings": settings,
	})
	organization.Settings = settings
	if enabled, exactBoolean := value.(bool); exactBoolean && enabled {
		t.Cleanup(func() {
			clearMetaInstagramPlatformComplianceMarkers(t, db, organization.ID)
		})
	}
}

func clearMetaInstagramPlatformComplianceMarkers(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
) {
	t.Helper()
	updateMetaInstagramComplianceOrganizationFixture(t, db, organizationID, map[string]any{
		"settings": gorm.Expr(`
			COALESCE(settings, '{}'::jsonb)
			- ARRAY[CAST(? AS text), CAST(? AS text), CAST(? AS text)]
		`,
			platformcompliance.PurposeMarkerKey,
			metaInstagramDataDeletionComplianceTenantMarkerKey,
			database.PlatformComplianceThreadsMarkerKey,
		),
	})
	var state struct {
		GuardEnabled bool `gorm:"column:guard_enabled"`
		Purpose      bool `gorm:"column:purpose"`
	}
	require.NoError(t, db.Raw(`
		SELECT
			(
				SELECT count(*) = 1 AND pg_catalog.bool_and(trigger.tgenabled = 'O')
				FROM pg_catalog.pg_trigger AS trigger
				JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
				JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public'
				  AND relation.relname = 'organizations'
				  AND trigger.tgname = 'rereply_platform_compliance_classification_guard'
				  AND NOT trigger.tgisinternal
			) AS guard_enabled,
			COALESCE(
				pg_catalog.jsonb_typeof(organization.settings -> CAST(? AS text)) = 'boolean'
				AND organization.settings -> CAST(? AS text) = 'true'::jsonb,
				false
			) AS purpose
		FROM public.organizations AS organization
		WHERE organization.id = ?
	`, platformcompliance.PurposeMarkerKey,
		platformcompliance.PurposeMarkerKey, organizationID).Scan(&state).Error)
	require.True(t, state.GuardEnabled, "organization compliance guard must remain enabled")
	require.False(t, state.Purpose, "synthetic platform compliance purpose must not survive the test")
}

func updateMetaInstagramComplianceOrganizationFixture(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	updates map[string]any,
) {
	t.Helper()
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var triggerExists bool
		if err := tx.Raw(`
			SELECT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_trigger AS trigger
				JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
				JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public' AND relation.relname = 'organizations'
				  AND trigger.tgname = 'rereply_platform_compliance_classification_guard'
				  AND NOT trigger.tgisinternal
			)
		`).Scan(&triggerExists).Error; err != nil {
			return err
		}
		if triggerExists {
			if err := tx.Exec(
				"ALTER TABLE public.organizations DISABLE TRIGGER rereply_platform_compliance_classification_guard",
			).Error; err != nil {
				return err
			}
		}
		if err := tx.Unscoped().Model(&models.Organization{}).
			Where("id = ?", organizationID).Updates(updates).Error; err != nil {
			return err
		}
		if triggerExists {
			return tx.Exec(
				"ALTER TABLE public.organizations ENABLE TRIGGER rereply_platform_compliance_classification_guard",
			).Error
		}
		return nil
	}))
}

func createMetaInstagramHistoricalDeletionFixture(
	t *testing.T,
	db *gorm.DB,
	event *models.MetaInstagramDataDeletionEvent,
) {
	t.Helper()
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var triggerExists bool
		if err := tx.Raw(`
			SELECT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_trigger AS trigger
				JOIN pg_catalog.pg_class AS relation ON relation.oid = trigger.tgrelid
				JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public'
				  AND relation.relname = 'meta_instagram_data_deletion_events'
				  AND trigger.tgname = 'rereply_platform_compliance_write_guard'
				  AND NOT trigger.tgisinternal
			)
		`).Scan(&triggerExists).Error; err != nil {
			return err
		}
		if triggerExists {
			if err := tx.Exec(`
				ALTER TABLE public.meta_instagram_data_deletion_events
				DISABLE TRIGGER rereply_platform_compliance_write_guard
			`).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(event).Error; err != nil {
			return err
		}
		if triggerExists {
			return tx.Exec(`
				ALTER TABLE public.meta_instagram_data_deletion_events
				ENABLE TRIGGER rereply_platform_compliance_write_guard
			`).Error
		}
		return nil
	}))
}

func createMetaInstagramPlatformComplianceOrganization(
	t *testing.T,
	db *gorm.DB,
) (*models.Reseller, *models.Organization) {
	t.Helper()
	return testutil.CreateTestPlatformComplianceOrganization(t, db, true, false)
}

func TestMetaInstagramMultiOrgOAuthFingerprintIsTargetScopedAndCompliancePinned(t *testing.T) {
	organizationA := uuid.New()
	organizationB := uuid.New()
	compliance := uuid.New()
	settings := configpkg.MetaInstagramConfig{
		Enabled: true, AppID: metaInstagramTestAppID, AppSecret: metaInstagramTestAppSecret,
		AppReviewStatus: "approved", GraphAPIVersion: "v25.0",
		AuthorizationBaseURL: "https://www.instagram.com",
		TokenBaseURL:         "https://api.instagram.com", GraphBaseURL: "https://graph.instagram.test",
		ReReplyBaseURL: "https://app.example.test", RelayBaseURL: "https://relay.example.test",
		AllowedOrganizationIDs:               organizationA.String() + "," + organizationB.String(),
		DataDeletionComplianceOrganizationID: compliance.String(),
	}
	original := metaInstagramOnboardingFingerprint(settings, organizationA)
	settings.AllowedOrganizationIDs = organizationA.String()
	settings.QuarantinedOrganizationIDs = organizationB.String()
	assert.Equal(t, original, metaInstagramOnboardingFingerprint(settings, organizationA))
	assert.NotEqual(t, original, metaInstagramOnboardingFingerprint(settings, organizationB))
	settings.DataDeletionComplianceOrganizationID = uuid.NewString()
	assert.NotEqual(t, original, metaInstagramOnboardingFingerprint(settings, organizationA))
}

func TestManagedInstagramMultiOrgOnboardingRequiresReleaseAndEntitlement(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	_, complianceOrganization := createMetaInstagramPlatformComplianceOrganization(t, fixture.db)
	configureMetaInstagramOrganizationSet(
		fixture, []uuid.UUID{fixture.org.ID}, nil, complianceOrganization.ID,
	)

	setMetaInstagramOmnichannelEntitlement(t, fixture, false)
	request := testutil.NewJSONRequest(t, map[string]any{})
	require.NoError(t, fixture.app.beginMetaInstagramOnboarding(
		request, fixture.org.ID, fixture.user.ID, uuid.Nil,
	))
	testutil.AssertErrorResponse(
		t, request, fasthttp.StatusPaymentRequired,
		"Instagram messaging is not included",
	)
	keys, err := fixture.app.Redis.Keys(
		t.Context(), metaInstagramOAuthStatePrefix+"*",
	).Result()
	require.NoError(t, err)
	assert.Empty(t, keys, "an active organization without entitlement must not receive OAuth state")

	setMetaInstagramOmnichannelEntitlement(t, fixture, true)
	configureMetaInstagramOrganizationSet(
		fixture, nil, []uuid.UUID{fixture.org.ID}, complianceOrganization.ID,
	)
	request = testutil.NewJSONRequest(t, map[string]any{})
	require.NoError(t, fixture.app.beginMetaInstagramOnboarding(
		request, fixture.org.ID, fixture.user.ID, uuid.Nil,
	))
	testutil.AssertErrorResponse(
		t, request, fasthttp.StatusServiceUnavailable,
		"Managed Instagram onboarding is unavailable",
	)
	keys, err = fixture.app.Redis.Keys(
		t.Context(), metaInstagramOAuthStatePrefix+"*",
	).Result()
	require.NoError(t, err)
	assert.Empty(t, keys, "a quarantined organization must not receive OAuth state despite entitlement")

	configureMetaInstagramOrganizationSet(
		fixture, []uuid.UUID{fixture.org.ID}, nil, complianceOrganization.ID,
	)
	request = testutil.NewJSONRequest(t, map[string]any{})
	require.NoError(t, fixture.app.beginMetaInstagramOnboarding(
		request, fixture.org.ID, fixture.user.ID, uuid.Nil,
	))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	keys, err = fixture.app.Redis.Keys(
		t.Context(), metaInstagramOAuthStatePrefix+"*",
	).Result()
	require.NoError(t, err)
	assert.Len(t, keys, 1, "only an active and entitled organization receives OAuth state")
}

func seedManagedInstagramMultiOrgAccount(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	organization *models.Organization,
	oauthSubjectID, professionalAccountID string,
	now time.Time,
) metaInstagramLifecycleFixture {
	t.Helper()
	role := testutil.CreateTestRoleWithKeys(
		t, fixture.db, organization.ID,
		"synthetic-instagram-multiorg-admin-"+uuid.NewString(),
		[]string{
			models.ResourceChannelAccounts + ":" + models.ActionRead,
			models.ResourceChannelAccounts + ":" + models.ActionWrite,
			models.ResourceChannelAccounts + ":" + models.ActionDelete,
			models.ResourceSettingsIntegrations + ":" + models.ActionRead,
			models.ResourceSettingsIntegrations + ":" + models.ActionWrite,
		},
	)
	user := testutil.CreateTestUser(t, fixture.db, organization.ID, testutil.WithRoleID(&role.ID))

	oauthID := uuid.New()
	webhookID := uuid.New()
	operation := metaMessengerSubscriptionOperation{
		ID: uuid.New(), OAuthCredentialID: oauthID, OAuthVersion: 1,
		WebhookCredentialID: webhookID, WebhookVersion: 1,
		DesiredState: metaMessengerSubscriptionDesiredSubscribed,
		State:        metaMessengerSubscriptionSubscribeComplete,
		ExpiresAt:    now.Add(time.Hour),
	}
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_authorizing_user_id"] = oauthSubjectID
	metadata[metaInstagramOAuthSubjectIDKey] = oauthSubjectID
	metadata["meta_authority_asset_id"] = professionalAccountID
	metadata[metaMessengerAuthorizationGrantedAtKey] = now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	metadata["meta_ownership_checked_at"] = now.Add(-time.Minute).Format(time.RFC3339Nano)
	metadata = metadataWithMetaMessengerSubscriptionOperation(
		metadata, operation, metaMessengerSubscriptionRemoteSubscribed,
	)
	metadata[metaMessengerSubscriptionRemoteConfirmedAtKey] = now.Add(-time.Minute).Format(time.RFC3339Nano)
	accountID := uuid.New()
	account := fixture.account
	account.BaseModel = models.BaseModel{ID: accountID}
	account.OrganizationID = organization.ID
	account.Name = "Synthetic multi-organization Instagram Profile"
	account.ExternalAccountID = professionalAccountID
	account.Config = cloneJSONB(fixture.account.Config)
	account.Config["rereply_webhook_url"] = "https://app.example.test/api/webhooks/channels/" + accountID.String()
	account.Config["relay_url"] = "https://relay.example.test/v1/accounts/instagram/" + professionalAccountID
	account.Metadata = metadata
	account.CreatedByID = &user.ID
	account.UpdatedByID = &user.ID
	account.Organization = nil
	account.Credentials = nil
	account.CreatedBy = nil
	account.UpdatedBy = nil
	require.NoError(t, fixture.db.Create(&account).Error)

	oauth := fixture.oauth
	oauth.BaseModel = models.BaseModel{ID: oauthID}
	oauth.OrganizationID = organization.ID
	oauth.ChannelAccountID = account.ID
	oauth.Organization = nil
	oauth.ChannelAccount = nil
	require.NoError(t, fixture.db.Create(&oauth).Error)
	webhook := fixture.webhook
	webhook.BaseModel = models.BaseModel{ID: webhookID}
	webhook.OrganizationID = organization.ID
	webhook.ChannelAccountID = account.ID
	webhook.Organization = nil
	webhook.ChannelAccount = nil
	require.NoError(t, fixture.db.Create(&webhook).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("id IN ?", []uuid.UUID{oauth.ID, webhook.ID}).
		UpdateColumn("created_at", now.Add(-time.Hour)).Error)

	return metaInstagramLifecycleFixture{
		app: fixture.app, db: fixture.db, org: organization, user: user,
		profileID: professionalAccountID, account: account, oauth: oauth, webhook: webhook,
	}
}

func TestManagedInstagramMultiOrgPerTenantQuarantineAndRegistryRouting(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	now := time.Now().UTC().Truncate(time.Second)
	secondOrganization := testutil.CreateTestOrganization(t, fixture.db)
	_, complianceOrganization := createMetaInstagramPlatformComplianceOrganization(t, fixture.db)
	second := seedManagedInstagramMultiOrgAccount(
		t, fixture, secondOrganization,
		"770000000000101", "780000000000101", now,
	)
	configureMetaInstagramOrganizationSet(
		fixture,
		[]uuid.UUID{fixture.org.ID, second.org.ID}, nil,
		complianceOrganization.ID,
	)

	firstOwner, err := fixture.app.resolveMetaRegistryOrganization(metaregistry.ResolveRequest{
		Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
		Purpose: metaregistry.ResolvePurposeOutbound,
	})
	require.NoError(t, err)
	assert.Equal(t, fixture.org.ID, firstOwner)
	secondOwner, err := fixture.app.resolveMetaRegistryOrganization(metaregistry.ResolveRequest{
		Channel: models.ChannelInstagram, ExternalAccountID: second.profileID,
		Purpose: metaregistry.ResolvePurposeOutbound,
	})
	require.NoError(t, err)
	assert.Equal(t, second.org.ID, secondOwner)
	_, err = fixture.app.scopedApp(fixture.db, second.org.ID).loadMetaRegistryBinding(
		metaregistry.ResolveRequest{
			Channel: models.ChannelInstagram, ExternalAccountID: second.profileID,
			Purpose: metaregistry.ResolvePurposeOutbound,
		}, now,
	)
	require.NoError(t, err)

	queued, dispatching := createMetaInstagramManualOutboxPair(t, second)
	messageID := uuid.New()
	scheduled := models.ScheduledJob{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: second.org.ID,
		Kind:          models.ScheduledJobKindChannelAIReply,
		AggregateType: models.ChannelAIReplyAggregateType, AggregateID: &messageID,
		RunAt: now, Status: models.ScheduledJobStatusPending, MaxAttempts: 5,
		IdempotencyKey: models.ChannelAIReplyIdempotencyKey(messageID), Version: 1,
		Payload: models.JSONB{"channel_account_id": second.account.ID.String()},
	}
	require.NoError(t, fixture.db.Create(&scheduled).Error)
	configureMetaInstagramOrganizationSet(
		fixture,
		[]uuid.UUID{fixture.org.ID}, []uuid.UUID{second.org.ID},
		complianceOrganization.ID,
	)
	require.NoError(t, fixture.app.ReconcileMetaInstagramQuarantineStartup(t.Context()))

	var firstAfter, secondAfter models.ChannelAccount
	require.NoError(t, fixture.db.First(&firstAfter, "id = ?", fixture.account.ID).Error)
	require.NoError(t, fixture.db.First(&secondAfter, "id = ?", second.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, firstAfter.Status)
	assert.Equal(t, models.ChannelAccountStatusDegraded, secondAfter.Status)
	assert.True(t, boolConfigValue(firstAfter.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(secondAfter.Config, "outbound_enabled"))
	assertMetaInstagramQueuedWorkFence(t, second, queued, dispatching)
	require.NoError(t, fixture.db.First(&scheduled, "id = ?", scheduled.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, scheduled.Status)
	assert.True(t, fixture.app.metaInstagramOnboardingAvailable(fixture.org.ID))
	assert.False(t, fixture.app.metaInstagramOnboardingAvailable(second.org.ID))
	_, err = fixture.app.scopedApp(fixture.db, second.org.ID).loadMetaRegistryBinding(
		metaregistry.ResolveRequest{
			Channel: models.ChannelInstagram, ExternalAccountID: second.profileID,
			Purpose: metaregistry.ResolvePurposeOutbound,
		}, time.Now().UTC(),
	)
	require.Error(t, err)
}

func TestManagedInstagramMultiOrgConcurrentProvisionHasOneProfileOwner(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	secondOrganization := testutil.CreateTestOrganization(t, fixture.db)
	_, complianceOrganization := createMetaInstagramPlatformComplianceOrganization(t, fixture.db)
	secondRole := testutil.CreateTestRoleWithKeys(
		t, fixture.db, secondOrganization.ID,
		"synthetic-instagram-multiorg-provision-"+uuid.NewString(),
		[]string{
			models.ResourceChannelAccounts + ":" + models.ActionRead,
			models.ResourceChannelAccounts + ":" + models.ActionWrite,
			models.ResourceSettingsIntegrations + ":" + models.ActionRead,
			models.ResourceSettingsIntegrations + ":" + models.ActionWrite,
		},
	)
	secondUser := testutil.CreateTestUser(
		t, fixture.db, secondOrganization.ID, testutil.WithRoleID(&secondRole.ID),
	)
	enableBookingCommerceTestEntitlement(
		t, fixture.db, secondOrganization.ID, secondUser.ID,
		channelapi.OmnichannelEntitlementKey,
	)
	configureMetaInstagramOrganizationSet(
		fixture, []uuid.UUID{fixture.org.ID, secondOrganization.ID}, nil,
		complianceOrganization.ID,
	)
	professionalAccountID := "780000000000151"
	oauthSubjectID := "770000000000151"
	firstInput := syntheticMetaInstagramProvisionInput(
		fixture, professionalAccountID, uuid.New(),
	)
	firstInput.AuthorizingMetaUserID = oauthSubjectID
	secondInput := firstInput
	secondInput.OrganizationID = secondOrganization.ID
	secondInput.UserID = secondUser.ID
	secondInput.SubscriptionOperationID = uuid.New()

	type outcome struct {
		result metaRegistryProvisionResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var subscriptionAttempts atomic.Int32
	for _, input := range []metaRegistryProvisionInput{firstInput, secondInput} {
		input := input
		go func() {
			<-start
			var result metaRegistryProvisionResult
			err := fixture.app.WithCommittedTenantApp(input.OrganizationID, func(scoped *App) error {
				var provisionErr error
				result, provisionErr = scoped.provisionMetaRegistryBinding(input)
				return provisionErr
			})
			if err == nil {
				err = fixture.app.withLockedMetaInstagramSubscriptionProviderAttempt(
					t.Context(), input.OrganizationID, result.Account.ID,
					result.SubscriptionOperation, metaMessengerSubscriptionSubscribePending,
					func(models.ChannelAccount, string) error {
						subscriptionAttempts.Add(1)
						return nil
					},
				)
			}
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	results := []outcome{<-outcomes, <-outcomes}
	successes, conflicts := 0, 0
	for _, result := range results {
		switch {
		case result.err == nil:
			successes++
		case errors.Is(result.err, errMetaInstagramProfileAlreadyRegistered):
			conflicts++
		default:
			require.NoError(t, result.err)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, conflicts)
	assert.Equal(t, int32(1), subscriptionAttempts.Load())
	var accounts []models.ChannelAccount
	require.NoError(t, fixture.db.Where(
		"organization_id IN ? AND channel = ? AND external_account_id = ?",
		[]uuid.UUID{fixture.org.ID, secondOrganization.ID},
		models.ChannelInstagram, professionalAccountID,
	).Find(&accounts).Error)
	require.Len(t, accounts, 1)
	var credentials int64
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("channel_account_id = ?", accounts[0].ID).Count(&credentials).Error)
	assert.Equal(t, int64(2), credentials)
}

// This test deliberately remains top-level sequential because it proves
// deleted/inactive rejection by temporarily changing the canonical synthetic
// platform reseller, restoring it before any top-level parallel test can run.
func TestManagedInstagramMultiOrgStartupRequiresMarkedPlatformComplianceTenant(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
		func(*http.Request) (*http.Response, error) {
			providerCalls.Add(1)
			return nil, errors.New("compliance startup validation must not call the provider")
		},
	)}
	assertRejected := func(t *testing.T, complianceID uuid.UUID) {
		t.Helper()
		configureMetaInstagramOrganizationSet(
			fixture, []uuid.UUID{fixture.org.ID}, nil, complianceID,
		)
		err := fixture.app.ReconcileMetaInstagramQuarantineStartup(t.Context())
		require.ErrorIs(t, err, errMetaInstagramComplianceTenantInvalid)
		var account models.ChannelAccount
		require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
		assert.Equal(t, models.ChannelAccountStatusActive, account.Status)
		assert.True(t, boolConfigValue(account.Config, "outbound_enabled"))
		assert.Zero(t, providerCalls.Load())
	}

	t.Run("missing organization", func(t *testing.T) {
		assertRejected(t, uuid.New())
	})

	// Install and exercise the production atomic creator before seeding the
	// deliberately invalid historical purpose-marker rows below. The guard
	// installer must reject those rows at rest, so creating the valid purpose
	// organization after them makes this package-order fixture self-defeating.
	platformReseller, valid := createMetaInstagramPlatformComplianceOrganization(t, fixture.db)
	t.Cleanup(func() {
		_ = fixture.db.Unscoped().Model(platformReseller).Updates(map[string]any{
			"deleted_at": nil,
			"status":     models.ResellerStatusActive,
		}).Error
		updateMetaInstagramComplianceOrganizationFixture(t, fixture.db, valid.ID, map[string]any{
			"deleted_at": nil,
			"settings": models.JSONB{
				metaInstagramDataDeletionComplianceTenantMarkerKey: true,
				platformcompliance.PurposeMarkerKey:                true,
			},
		})
	})

	unowned := testutil.CreateTestOrganization(t, fixture.db)
	setPlatformCompliancePurposeMarker(t, fixture.db, unowned, true)
	setMetaInstagramComplianceTenantMarker(t, fixture.db, unowned, true)
	t.Run("marked but unowned organization", func(t *testing.T) {
		assertRejected(t, unowned.ID)
	})

	ordinaryReseller := testutil.CreateTestReseller(t, fixture.db)
	ordinary := testutil.CreateTestOrganizationForReseller(t, fixture.db, ordinaryReseller.ID)
	setPlatformCompliancePurposeMarker(t, fixture.db, ordinary, true)
	setMetaInstagramComplianceTenantMarker(t, fixture.db, ordinary, true)
	t.Run("marked organization owned by ordinary reseller", func(t *testing.T) {
		assertRejected(t, ordinary.ID)
	})

	unmarked := testutil.CreateTestOrganizationForReseller(t, fixture.db, platformReseller.ID)
	setPlatformCompliancePurposeMarker(t, fixture.db, unmarked, true)
	t.Run("unmarked platform organization", func(t *testing.T) {
		assertRejected(t, unmarked.ID)
	})
	setMetaInstagramComplianceTenantMarker(t, fixture.db, unmarked, "true")
	t.Run("marker must be exact JSON boolean", func(t *testing.T) {
		assertRejected(t, unmarked.ID)
	})

	setPlatformCompliancePurposeMarker(t, fixture.db, valid, "true")
	t.Run("purpose marker must be exact JSON boolean", func(t *testing.T) {
		assertRejected(t, valid.ID)
	})
	setPlatformCompliancePurposeMarker(t, fixture.db, valid, true)

	require.NoError(t, fixture.db.Model(platformReseller).
		Update("status", models.ResellerStatusSuspended).Error)
	t.Run("inactive platform reseller", func(t *testing.T) {
		assertRejected(t, valid.ID)
	})
	require.NoError(t, fixture.db.Model(platformReseller).
		Update("status", models.ResellerStatusActive).Error)
	platformReseller.Status = models.ResellerStatusActive

	require.NoError(t, fixture.db.Delete(platformReseller).Error)
	t.Run("deleted platform reseller", func(t *testing.T) {
		assertRejected(t, valid.ID)
	})
	require.NoError(t, fixture.db.Unscoped().Model(platformReseller).
		Update("deleted_at", nil).Error)
	platformReseller.DeletedAt = gorm.DeletedAt{}

	updateMetaInstagramComplianceOrganizationFixture(t, fixture.db, valid.ID, map[string]any{
		"deleted_at": time.Now().UTC(),
	})
	t.Run("deleted marked organization", func(t *testing.T) {
		assertRejected(t, valid.ID)
	})
	updateMetaInstagramComplianceOrganizationFixture(t, fixture.db, valid.ID, map[string]any{
		"deleted_at": nil,
	})
	valid.DeletedAt = gorm.DeletedAt{}

	configureMetaInstagramOrganizationSet(
		fixture, []uuid.UUID{fixture.org.ID}, nil, valid.ID,
	)
	require.NoError(t, fixture.app.ReconcileMetaInstagramQuarantineStartup(t.Context()))
	assert.Zero(t, providerCalls.Load())

	setMetaInstagramComplianceTenantMarker(t, fixture.db, valid, nil)
	t.Run("marker removal fails closed on next startup", func(t *testing.T) {
		assertRejected(t, valid.ID)
	})
}

func TestManagedInstagramMultiOrgDeletionFailsClosedWithoutComplianceConfig(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	fixture.app.Config.MetaInstagram.AllowedOrganizationID = ""
	fixture.app.Config.MetaInstagram.AllowedOrganizationIDs = fixture.org.ID.String()
	fixture.app.Config.MetaInstagram.QuarantinedOrganizationIDs = ""
	fixture.app.Config.MetaInstagram.DataDeletionComplianceOrganizationID = ""
	now := time.Now().UTC().Truncate(time.Second)
	signedRequest := signMetaInstagramLifecycleRequest(
		t, now, "770000000000181", metaInstagramTestAppSecret,
	)
	_, digest, err := verifyMetaMessengerSignedRequestSignature(
		signedRequest, metaInstagramTestAppSecret,
	)
	require.NoError(t, err)
	request := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, request.RequestCtx.Response.StatusCode())
	var journals, privacyRequests int64
	require.NoError(t, fixture.db.Model(&models.MetaInstagramDataDeletionEvent{}).
		Where("digest = ?", digest).Count(&journals).Error)
	require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
		Where(
			"organization_id = ? AND received_channel = ? AND verification_method = ?",
			fixture.org.ID, metaInstagramDeletionReceivedChannel,
			metaInstagramDeletionVerification,
		).Count(&privacyRequests).Error)
	assert.Zero(t, journals)
	assert.Zero(t, privacyRequests)
}

func TestManagedInstagramMultiOrgReplaysLegacyNoTargetReceiptAfterSetMigration(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	now := time.Now().UTC().Truncate(time.Second)
	subjectID := "770000000000182"
	signedRequest := signMetaInstagramLifecycleRequest(
		t, now, subjectID, metaInstagramTestAppSecret,
	)
	_, digest, err := verifyMetaMessengerSignedRequestSignature(
		signedRequest, metaInstagramTestAppSecret,
	)
	require.NoError(t, err)

	legacy := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(legacy))
	assert.Equal(t, fasthttp.StatusOK, legacy.RequestCtx.Response.StatusCode())
	var legacyResponse metaInstagramDeletionResponse
	require.NoError(t, json.Unmarshal(legacy.RequestCtx.Response.Body(), &legacyResponse))

	_, complianceOrganization := createMetaInstagramPlatformComplianceOrganization(t, fixture.db)
	configureMetaInstagramOrganizationSet(
		fixture, []uuid.UUID{fixture.org.ID}, nil, complianceOrganization.ID,
	)
	replay := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(replay))
	assert.Equal(t, fasthttp.StatusOK, replay.RequestCtx.Response.StatusCode())
	var replayResponse metaInstagramDeletionResponse
	require.NoError(t, json.Unmarshal(replay.RequestCtx.Response.Body(), &replayResponse))
	assert.Equal(t, legacyResponse, replayResponse)

	var journal models.MetaInstagramDataDeletionEvent
	require.NoError(t, fixture.db.First(&journal, "digest = ?", digest).Error)
	assert.Equal(t, fixture.org.ID, journal.OrganizationID)
	assert.Equal(t, metaInstagramDeletionResolutionNoTarget, journal.TargetResolution)
	assert.False(t, journal.IdentityHashed)
	assert.Equal(t, metaInstagramTestAppID, journal.PlatformAppID)
	assert.Equal(t, subjectID, journal.AuthorizingUserID)
	var complianceRequests int64
	require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
		Where("organization_id = ?", complianceOrganization.ID).
		Count(&complianceRequests).Error)
	assert.Zero(t, complianceRequests)
}

func TestManagedInstagramMultiOrgExactOffboardedDeletionStaysWithTargetTenant(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	now := time.Now().UTC().Truncate(time.Second)
	offboardedOrganization := testutil.CreateTestOrganization(t, fixture.db)
	_, complianceOrganization := createMetaInstagramPlatformComplianceOrganization(t, fixture.db)
	foreignOrganization := testutil.CreateTestOrganization(t, fixture.db)
	offboarded := seedManagedInstagramMultiOrgAccount(
		t, fixture, offboardedOrganization,
		"770000000000201", "780000000000201", now,
	)
	foreign := seedManagedInstagramMultiOrgAccount(
		t, fixture, foreignOrganization,
		"770000000000201", "780000000000202", now,
	)
	configureMetaInstagramOrganizationSet(
		fixture, []uuid.UUID{fixture.org.ID}, []uuid.UUID{offboarded.org.ID},
		complianceOrganization.ID,
	)
	var foreignBefore models.ChannelAccount
	require.NoError(t, fixture.db.First(&foreignBefore, "id = ?", foreign.account.ID).Error)

	signedRequest := signMetaInstagramLifecycleRequest(
		t, now, "770000000000201", metaInstagramTestAppSecret,
	)
	_, digest, err := verifyMetaMessengerSignedRequestSignature(
		signedRequest, metaInstagramTestAppSecret,
	)
	require.NoError(t, err)
	request := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	var response metaInstagramDeletionResponse
	require.NoError(t, json.Unmarshal(request.RequestCtx.Response.Body(), &response))

	var targetAfter, activeAfter, foreignAfter models.ChannelAccount
	require.NoError(t, fixture.db.First(&targetAfter, "id = ?", offboarded.account.ID).Error)
	require.NoError(t, fixture.db.First(&activeAfter, "id = ?", fixture.account.ID).Error)
	require.NoError(t, fixture.db.First(&foreignAfter, "id = ?", foreign.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, targetAfter.Status)
	assert.Equal(t, models.ChannelAccountStatusActive, activeAfter.Status)
	assert.Equal(t, foreignBefore.Status, foreignAfter.Status)
	assert.Equal(t, foreignBefore.Config, foreignAfter.Config)
	assert.Equal(t, foreignBefore.Metadata, foreignAfter.Metadata)
	var journal models.MetaInstagramDataDeletionEvent
	require.NoError(t, fixture.db.First(&journal, "digest = ?", digest).Error)
	assert.Equal(t, offboarded.org.ID, journal.OrganizationID)
	assert.Equal(t, metaInstagramDeletionResolutionExact, journal.TargetResolution)
	assert.False(t, journal.IdentityHashed)
	assert.Equal(t, metaInstagramTestAppID, journal.PlatformAppID)
	assert.Equal(t, "770000000000201", journal.AuthorizingUserID)
	var requests int64
	require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
		Where("organization_id = ? AND request_number = ?", offboarded.org.ID, response.ConfirmationCode).
		Count(&requests).Error)
	assert.Equal(t, int64(1), requests)
	require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
		Where("organization_id = ?", complianceOrganization.ID).Count(&requests).Error)
	assert.Zero(t, requests)

	replay := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(replay))
	var replayResponse metaInstagramDeletionResponse
	require.NoError(t, json.Unmarshal(replay.RequestCtx.Response.Body(), &replayResponse))
	assert.Equal(t, response, replayResponse)
	status := testutil.NewRequest(t)
	status.RequestCtx.SetUserValue("confirmation_code", response.ConfirmationCode)
	require.NoError(t, fixture.app.MetaInstagramDataDeletionStatus(status))
	assert.Equal(t, fasthttp.StatusOK, status.RequestCtx.Response.StatusCode())
}

func TestManagedInstagramMultiOrgDeauthorizationScopesExactOffboardedTenant(
	t *testing.T,
) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	now := time.Now().UTC().Truncate(time.Second)
	offboardedOrganization := testutil.CreateTestOrganization(t, fixture.db)
	_, complianceOrganization := createMetaInstagramPlatformComplianceOrganization(t, fixture.db)
	foreignOrganization := testutil.CreateTestOrganization(t, fixture.db)
	offboarded := seedManagedInstagramMultiOrgAccount(
		t, fixture, offboardedOrganization,
		"770000000000251", "780000000000251", now,
	)
	foreign := seedManagedInstagramMultiOrgAccount(
		t, fixture, foreignOrganization,
		"770000000000251", "780000000000252", now,
	)
	configureMetaInstagramOrganizationSet(
		fixture, []uuid.UUID{fixture.org.ID}, []uuid.UUID{offboarded.org.ID},
		complianceOrganization.ID,
	)
	var foreignBefore models.ChannelAccount
	require.NoError(t, fixture.db.First(&foreignBefore, "id = ?", foreign.account.ID).Error)
	signedRequest := signMetaInstagramLifecycleRequest(
		t, now, "770000000000251", metaInstagramTestAppSecret,
	)
	request := newMetaInstagramDeauthorizationRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeauthorizeMetaInstagram(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())

	var offboardedAfter, foreignAfter, activeAfter models.ChannelAccount
	require.NoError(t, fixture.db.First(&offboardedAfter, "id = ?", offboarded.account.ID).Error)
	require.NoError(t, fixture.db.First(&foreignAfter, "id = ?", foreign.account.ID).Error)
	require.NoError(t, fixture.db.First(&activeAfter, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, offboardedAfter.Status)
	assert.Equal(t, foreignBefore.Status, foreignAfter.Status)
	assert.Equal(t, foreignBefore.Config, foreignAfter.Config)
	assert.Equal(t, foreignBefore.Metadata, foreignAfter.Metadata)
	assert.Equal(t, models.ChannelAccountStatusActive, activeAfter.Status)

	replay := newMetaInstagramDeauthorizationRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeauthorizeMetaInstagram(replay))
	assert.Equal(t, fasthttp.StatusOK, replay.RequestCtx.Response.StatusCode())
	var journals int64
	require.NoError(t, fixture.db.Model(&models.MetaDeauthorizationEvent{}).
		Where("platform_app_id = ? AND authorizing_user_id = ? AND state = ?",
			metaInstagramTestAppID, "770000000000251", "completed").
		Count(&journals).Error)
	assert.Equal(t, int64(1), journals)
}

func TestManagedInstagramMultiOrgNoTargetAndAmbiguousDeletionUseOpaqueComplianceWorkflow(
	t *testing.T,
) {
	for _, test := range []struct {
		name       string
		subjectID  string
		ambiguous  bool
		resolution string
	}{
		{name: "no target", subjectID: "770000000000301", resolution: metaInstagramDeletionResolutionNoTarget},
		{name: "ambiguous targets", subjectID: "770000000000302", ambiguous: true, resolution: metaInstagramDeletionResolutionAmbiguous},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			now := time.Now().UTC().Truncate(time.Second)
			secondOrganization := testutil.CreateTestOrganization(t, fixture.db)
			_, complianceOrganization := createMetaInstagramPlatformComplianceOrganization(t, fixture.db)
			active := []uuid.UUID{fixture.org.ID}
			var second metaInstagramLifecycleFixture
			var firstQueued, firstDispatching models.OutboxJob
			var offboardedQueued, offboardedDispatching models.OutboxJob
			if test.ambiguous {
				firstMetadata := cloneJSONB(fixture.account.Metadata)
				firstMetadata["meta_authorizing_user_id"] = test.subjectID
				firstMetadata[metaInstagramOAuthSubjectIDKey] = test.subjectID
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", firstMetadata).Error)
				second = seedManagedInstagramMultiOrgAccount(
					t, fixture, secondOrganization,
					test.subjectID, "780000000000302", now,
				)
				active = append(active, second.org.ID)
				firstQueued, firstDispatching = createMetaInstagramManualOutboxPair(t, fixture)
				offboardedQueued, offboardedDispatching =
					createMetaInstagramManualOutboxPair(t, second)
			} else {
				// An account in a clinic that has been removed from both the active
				// and quarantine sets is not a callback target. The authentic request
				// belongs to the compliance tenant and must not mutate that clinic.
				second = seedManagedInstagramMultiOrgAccount(
					t, fixture, secondOrganization,
					test.subjectID, "780000000000303", now,
				)
				offboardedQueued, offboardedDispatching =
					createMetaInstagramManualOutboxPair(t, second)
			}
			configureMetaInstagramOrganizationSet(
				fixture, active, nil, complianceOrganization.ID,
			)

			var firstBefore models.ChannelAccount
			require.NoError(t, fixture.db.First(&firstBefore, "id = ?", fixture.account.ID).Error)
			var secondBefore models.ChannelAccount
			require.NoError(t, fixture.db.First(&secondBefore, "id = ?", second.account.ID).Error)
			signedRequest := signMetaInstagramLifecycleRequest(
				t, now, test.subjectID, metaInstagramTestAppSecret,
			)
			_, digest, err := verifyMetaMessengerSignedRequestSignature(
				signedRequest, metaInstagramTestAppSecret,
			)
			require.NoError(t, err)
			request := newMetaInstagramDeletionRequest(t, signedRequest)
			require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
			assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
			var response metaInstagramDeletionResponse
			require.NoError(t, json.Unmarshal(request.RequestCtx.Response.Body(), &response))

			var firstAfter models.ChannelAccount
			require.NoError(t, fixture.db.First(&firstAfter, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, firstBefore.Status, firstAfter.Status)
			assert.Equal(t, firstBefore.Config, firstAfter.Config)
			assert.Equal(t, firstBefore.Metadata, firstAfter.Metadata)
			var secondAfter models.ChannelAccount
			require.NoError(t, fixture.db.First(&secondAfter, "id = ?", second.account.ID).Error)
			assert.Equal(t, secondBefore.Status, secondAfter.Status)
			assert.Equal(t, secondBefore.Config, secondAfter.Config)
			assert.Equal(t, secondBefore.Metadata, secondAfter.Metadata)
			for job, expected := range map[*models.OutboxJob]models.OutboxJobStatus{
				&offboardedQueued:      models.OutboxJobStatusProcessing,
				&offboardedDispatching: models.OutboxJobStatusDispatching,
			} {
				require.NoError(t, fixture.db.First(job, "id = ?", job.ID).Error)
				assert.Equal(t, expected, job.Status)
			}
			if test.ambiguous {
				for job, expected := range map[*models.OutboxJob]models.OutboxJobStatus{
					&firstQueued:      models.OutboxJobStatusProcessing,
					&firstDispatching: models.OutboxJobStatusDispatching,
				} {
					require.NoError(t, fixture.db.First(job, "id = ?", job.ID).Error)
					assert.Equal(t, expected, job.Status)
				}
			}

			hashedAppID, err := metaInstagramComplianceIdentityHash(
				metaInstagramTestAppSecret, "app", metaInstagramTestAppID,
			)
			require.NoError(t, err)
			hashedSubjectID, err := metaInstagramComplianceIdentityHash(
				metaInstagramTestAppSecret, "subject", test.subjectID,
			)
			require.NoError(t, err)
			var journal models.MetaInstagramDataDeletionEvent
			require.NoError(t, fixture.db.First(&journal, "digest = ?", digest).Error)
			assert.Equal(t, complianceOrganization.ID, journal.OrganizationID)
			assert.Equal(t, test.resolution, journal.TargetResolution)
			assert.True(t, journal.IdentityHashed)
			assert.Equal(t, hashedAppID, journal.PlatformAppID)
			assert.Equal(t, hashedSubjectID, journal.AuthorizingUserID)
			assert.NotContains(t, journal.PlatformAppID, metaInstagramTestAppID)
			assert.NotContains(t, journal.AuthorizingUserID, test.subjectID)

			var privacyRequest models.PrivacyRequest
			require.NoError(t, fixture.db.First(
				&privacyRequest,
				"organization_id = ? AND request_number = ?",
				complianceOrganization.ID, response.ConfirmationCode,
			).Error)
			assert.Equal(t, hashedSubjectID, privacyRequest.SubjectKey)
			assert.Equal(t, test.resolution, stringConfigValue(privacyRequest.RequestDetails, "target_resolution"))
			assert.Equal(t, test.ambiguous, boolConfigValue(privacyRequest.RequestDetails, "requires_manual_resolution"))
			serialized, err := json.Marshal(privacyRequest)
			require.NoError(t, err)
			assert.NotContains(t, string(serialized), test.subjectID)
			assert.NotContains(t, string(serialized), metaInstagramTestAppID)

			var clinicRequests int64
			require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
				Where("organization_id IN ?", active).Count(&clinicRequests).Error)
			assert.Zero(t, clinicRequests)
			var events int64
			require.NoError(t, fixture.db.Model(&models.PrivacyRequestEvent{}).
				Where("organization_id = ? AND privacy_request_id = ?", complianceOrganization.ID, privacyRequest.ID).
				Count(&events).Error)
			if test.ambiguous {
				assert.Equal(t, int64(2), events)
			} else {
				assert.Equal(t, int64(1), events)
			}
			var privacyEvents []models.PrivacyRequestEvent
			require.NoError(t, fixture.db.Where(
				"organization_id = ? AND privacy_request_id = ?",
				complianceOrganization.ID, privacyRequest.ID,
			).Find(&privacyEvents).Error)
			serialized, err = json.Marshal(privacyEvents)
			require.NoError(t, err)
			assert.NotContains(t, string(serialized), test.subjectID)
			assert.NotContains(t, string(serialized), metaInstagramTestAppID)

			replay := newMetaInstagramDeletionRequest(t, signedRequest)
			require.NoError(t, fixture.app.DeleteMetaInstagramUserData(replay))
			var replayResponse metaInstagramDeletionResponse
			require.NoError(t, json.Unmarshal(replay.RequestCtx.Response.Body(), &replayResponse))
			assert.Equal(t, response, replayResponse)
			require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
				Where("organization_id = ? AND verification_token_hash = ?",
					complianceOrganization.ID,
					metaInstagramDeletionEventHash(hashedAppID, hashedSubjectID, digest)).
				Count(&clinicRequests).Error)
			assert.Equal(t, int64(1), clinicRequests)

			if !test.ambiguous {
				configureMetaInstagramOrganizationSet(
					fixture,
					[]uuid.UUID{fixture.org.ID, second.org.ID}, nil,
					complianceOrganization.ID,
				)
				var providerCalls atomic.Int32
				fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
					func(*http.Request) (*http.Response, error) {
						providerCalls.Add(1)
						return nil, errors.New("startup journal fence must not call the provider")
					},
				)}
				require.NoError(t, fixture.app.ReconcileMetaInstagramQuarantineStartup(t.Context()))
				assert.Zero(t, providerCalls.Load())
				require.NoError(t, fixture.db.First(
					&secondAfter, "id = ?", second.account.ID,
				).Error)
				assert.Equal(t, models.ChannelAccountStatusDegraded, secondAfter.Status)
				assert.False(t, boolConfigValue(secondAfter.Config, "outbound_enabled"))
				assertMetaInstagramQueuedWorkFence(
					t, second, offboardedQueued, offboardedDispatching,
				)
			}
		})
	}
}

func TestManagedInstagramMultiOrgOAuthFenceIncludesOpaqueComplianceJournal(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	_, complianceOrganization := createMetaInstagramPlatformComplianceOrganization(t, fixture.db)
	configureMetaInstagramOrganizationSet(
		fixture, []uuid.UUID{fixture.org.ID}, nil, complianceOrganization.ID,
	)
	subjectID := "770000000000351"
	now := time.Now().UTC().Truncate(time.Second)
	eventIssuedAt := now.Add(-2 * time.Minute)
	hashedAppID, err := metaInstagramComplianceIdentityHash(
		metaInstagramTestAppSecret, "app", metaInstagramTestAppID,
	)
	require.NoError(t, err)
	hashedSubjectID, err := metaInstagramComplianceIdentityHash(
		metaInstagramTestAppSecret, "subject", subjectID,
	)
	require.NoError(t, err)
	require.NoError(t, fixture.db.Create(&models.MetaInstagramDataDeletionEvent{
		Digest: strings.Repeat("e", 64), OrganizationID: complianceOrganization.ID,
		PlatformAppID: hashedAppID, AuthorizingUserID: hashedSubjectID,
		IssuedAt: eventIssuedAt, VerifiedAt: eventIssuedAt,
		State: "verified", TargetResolution: metaInstagramDeletionResolutionNoTarget,
		IdentityHashed: true,
	}).Error)

	check := func(startedAt time.Time) error {
		return fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
			if err := lockChannelAIOrganizationScopeTx(scoped.DB, fixture.org.ID); err != nil {
				return err
			}
			return scoped.metaInstagramOAuthGenerationFence(
				fixture.org.ID, metaInstagramTestAppID, subjectID,
				startedAt, startedAt.Add(time.Second), models.JSONB{},
			)
		})
	}
	require.ErrorIs(t, check(eventIssuedAt.Add(-time.Minute)), errMetaInstagramAuthorizationSuperseded)
	require.NoError(t, check(eventIssuedAt.Add(time.Minute)))
}

func TestManagedInstagramMultiOrgDeletionReplayRejectsInvalidComplianceOwnership(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	_, complianceOrganization := createMetaInstagramPlatformComplianceOrganization(t, fixture.db)
	configureMetaInstagramOrganizationSet(
		fixture, []uuid.UUID{fixture.org.ID}, nil, complianceOrganization.ID,
	)
	now := time.Now().UTC().Truncate(time.Second)
	subjectID := "770000000000361"
	signedRequest := signMetaInstagramLifecycleRequest(
		t, now, subjectID, metaInstagramTestAppSecret,
	)
	_, digest, err := verifyMetaMessengerSignedRequestSignature(
		signedRequest, metaInstagramTestAppSecret,
	)
	require.NoError(t, err)
	historicalJournal := models.MetaInstagramDataDeletionEvent{
		Digest: digest, OrganizationID: complianceOrganization.ID,
		PlatformAppID: metaInstagramTestAppID, AuthorizingUserID: subjectID,
		IssuedAt: now, VerifiedAt: now, State: "verified",
		TargetResolution: metaInstagramDeletionResolutionAmbiguous,
		IdentityHashed:   false,
	}
	createMetaInstagramHistoricalDeletionFixture(t, fixture.db, &historicalJournal)

	request := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, request.RequestCtx.Response.StatusCode())
	var privacyRequests int64
	require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
		Where("organization_id = ?", complianceOrganization.ID).
		Count(&privacyRequests).Error)
	assert.Zero(t, privacyRequests)
}

// This is intentionally not parallel: applying/removing FORCE RLS changes the
// shared disposable PostgreSQL schema for the duration of the test.
func TestManagedInstagramMultiOrgComplianceCallbackUsesOnlyTenantRLS(t *testing.T) {
	adminDB := testutil.SetupTestDB(t)
	testutil.TruncateTables(adminDB)
	runtimeRole := "rereply_ig_multi_" + uuid.NewString()[:8]
	runtimePassword := "synthetic" + uuid.NewString()[:8]
	require.NoError(t, adminDB.Exec(fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
		runtimeRole, runtimePassword,
	)).Error)
	t.Cleanup(func() {
		testutil.TruncateTables(adminDB)
		_ = database.RemoveTenantRLS(adminDB)
		_ = adminDB.Exec("DROP OWNED BY " + runtimeRole).Error
		_ = adminDB.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error
	})

	reseller, compliance := createMetaInstagramPlatformComplianceOrganization(t, adminDB)
	clinic := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	unmarkedCompliance := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	ordinaryReseller := testutil.CreateTestReseller(t, adminDB)
	ordinaryCompliance := testutil.CreateTestOrganizationForReseller(
		t, adminDB, ordinaryReseller.ID,
	)
	foreign := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	foreignJournal := models.MetaInstagramDataDeletionEvent{
		Digest: strings.Repeat("f", 64), OrganizationID: foreign.ID,
		PlatformAppID: "710000000000999", AuthorizingUserID: "720000000000999",
		IssuedAt: time.Now().UTC().Add(-time.Minute), VerifiedAt: time.Now().UTC(),
		State: "verified", TargetResolution: metaInstagramDeletionResolutionExact,
	}
	require.NoError(t, adminDB.Create(&foreignJournal).Error)
	require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
	setMetaInstagramComplianceTenantMarker(t, adminDB, unmarkedCompliance, true)
	runtimeDB := openRuntimeRoleTestDB(t, runtimeRole, runtimePassword)
	require.NoError(t, database.VerifyTenantRLS(runtimeDB, runtimeRole))

	fixture := metaInstagramLifecycleFixture{
		app: &App{
			DB: runtimeDB, Log: testutil.NopLogger(), Redis: testutil.SetupTestRedis(t),
			Config: newMetaInstagramLifecycleFixtureConfigForMultiOrgRLS(
				clinic.ID, compliance.ID, runtimeRole,
			),
		},
		db: adminDB, org: clinic,
	}
	fixture.app.Config.MetaInstagram.DataDeletionComplianceOrganizationID = ordinaryCompliance.ID.String()
	require.ErrorIs(
		t,
		fixture.app.ReconcileMetaInstagramQuarantineStartup(t.Context()),
		errMetaInstagramComplianceTenantInvalid,
	)
	fixture.app.Config.MetaInstagram.DataDeletionComplianceOrganizationID = unmarkedCompliance.ID.String()
	require.ErrorIs(
		t,
		fixture.app.ReconcileMetaInstagramQuarantineStartup(t.Context()),
		errMetaInstagramComplianceTenantInvalid,
	)
	fixture.app.Config.MetaInstagram.DataDeletionComplianceOrganizationID = compliance.ID.String()
	require.NoError(t, fixture.app.ReconcileMetaInstagramQuarantineStartup(t.Context()))
	subjectID := "770000000000401"
	now := time.Now().UTC().Truncate(time.Second)
	signedRequest := signMetaInstagramLifecycleRequest(
		t, now, subjectID, metaInstagramTestAppSecret,
	)
	request := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())

	var complianceJournals int64
	require.NoError(t, adminDB.Model(&models.MetaInstagramDataDeletionEvent{}).
		Where("organization_id = ?", compliance.ID).Count(&complianceJournals).Error)
	assert.Equal(t, int64(1), complianceJournals)
	var foreignAfter models.MetaInstagramDataDeletionEvent
	require.NoError(t, adminDB.First(&foreignAfter, "digest = ?", foreignJournal.Digest).Error)
	assert.Equal(t, "verified", foreignAfter.State)

	var visibleForeign int64
	require.NoError(t, database.WithTenant(runtimeDB, clinic.ID, func(tx *gorm.DB) error {
		return tx.Model(&models.MetaInstagramDataDeletionEvent{}).
			Where("digest = ?", foreignJournal.Digest).Count(&visibleForeign).Error
	}))
	assert.Zero(t, visibleForeign)
	var instagramSecurityDefiners int64
	require.NoError(t, adminDB.Raw(`
		SELECT COUNT(*)
		FROM pg_catalog.pg_proc AS proc
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = proc.pronamespace
		WHERE namespace.nspname = 'public'
		  AND proc.prosecdef
		  AND proc.proname LIKE 'rereply%instagram%'
	`).Scan(&instagramSecurityDefiners).Error)
	assert.Zero(t, instagramSecurityDefiners)
}

func newMetaInstagramLifecycleFixtureConfigForMultiOrgRLS(
	clinicID, complianceID uuid.UUID,
	runtimeRole string,
) *configpkg.Config {
	return &configpkg.Config{
		App: configpkg.AppConfig{
			Environment: "production", EncryptionKey: metaInstagramTestEncryptionKey,
		},
		Database: configpkg.DatabaseConfig{RLSEnabled: true, RuntimeRole: runtimeRole},
		MetaRegistry: configpkg.MetaRegistryConfig{
			Enabled: true, LeaseSeconds: 30, OwnershipMaxAgeMins: 24 * 60,
		},
		MetaInstagram: configpkg.MetaInstagramConfig{
			Enabled: true, AppID: metaInstagramTestAppID, AppSecret: metaInstagramTestAppSecret,
			AppReviewStatus: "approved", GraphAPIVersion: "v25.0",
			AuthorizationBaseURL: "https://www.instagram.com",
			TokenBaseURL:         "https://api.instagram.com", GraphBaseURL: "https://graph.instagram.test",
			ReReplyBaseURL: "https://app.example.test", RelayBaseURL: "https://relay.example.test",
			HealthApprovalMaxAgeMins: 15, RevalidationLeadMins: 60,
			TokenRefreshLeadHours: 168, SchedulerIntervalSeconds: 60,
			AllowedOrganizationIDs:               clinicID.String(),
			DataDeletionComplianceOrganizationID: complianceID.String(),
		},
	}
}
