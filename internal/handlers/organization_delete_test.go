package handlers_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

func createOrganizationSubscription(
	t *testing.T,
	app *handlers.App,
	organization *models.Organization,
	provider models.BillingProvider,
) (models.BillingAccount, models.Subscription) {
	t.Helper()
	require.NotNil(t, organization.ResellerID)

	plan := createCatalogPlan(
		t,
		app,
		organization.ResellerID,
		"delete-"+uuid.New().String()[:8],
		"Deletion Test Plan",
		models.CommercialPlanStatusActive,
	)
	price := createCatalogPrice(
		t,
		app,
		plan.ID,
		provider,
		models.BillingIntervalMonth,
		true,
		nil,
	)
	account := models.BillingAccount{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		ResellerID:      organization.ResellerID,
		Provider:        provider,
		Status:          models.BillingAccountStatusActive,
		DefaultCurrency: "MYR",
		BillingProfile:  models.JSONB{},
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&account).Error)

	now := time.Now().UTC()
	periodEnd := now.AddDate(0, 1, 0)
	subscription := models.Subscription{
		BaseModel:            models.BaseModel{ID: uuid.New()},
		OrganizationID:       organization.ID,
		BillingAccountID:     account.ID,
		PlanID:               plan.ID,
		PlanPriceID:          &price.ID,
		Provider:             provider,
		Status:               models.SubscriptionStatusActive,
		Quantity:             1,
		CollectionMethod:     "manual",
		EntitlementsSnapshot: models.JSONB{},
		ProviderData:         models.JSONB{},
		CurrentPeriodStart:   &now,
		CurrentPeriodEnd:     &periodEnd,
	}
	require.NoError(t, app.DB.Create(&subscription).Error)
	return account, subscription
}

func TestDeleteOrganizationArchivesWorkspaceAndCancelsManualLicense(
	t *testing.T,
) {
	app := newTestApp(t)
	controlOrganization := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(
		t,
		app.DB,
		controlOrganization.ID,
		testutil.WithSuperAdmin(),
	)
	reseller := testutil.CreateTestReseller(t, app.DB)
	target := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
	testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
	account, subscription := createOrganizationSubscription(
		t,
		app,
		target,
		models.BillingProviderManual,
	)

	request := testutil.NewJSONRequest(t, nil)
	testutil.SetAuthContext(request, controlOrganization.ID, owner.ID)
	testutil.SetPathParam(request, "id", target.ID.String())
	require.NoError(t, app.DeleteOrganization(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

	var active models.Organization
	assert.ErrorIs(
		t,
		app.DB.Where("id = ?", target.ID).First(&active).Error,
		gorm.ErrRecordNotFound,
	)
	var archived models.Organization
	require.NoError(t, app.DB.Unscoped().Where("id = ?", target.ID).First(&archived).Error)
	assert.True(t, archived.DeletedAt.Valid)

	var canceled models.Subscription
	require.NoError(t, app.DB.Where("id = ?", subscription.ID).First(&canceled).Error)
	assert.Equal(t, models.SubscriptionStatusCanceled, canceled.Status)
	assert.NotNil(t, canceled.CanceledAt)
	assert.NotNil(t, canceled.EndedAt)

	var closed models.BillingAccount
	require.NoError(t, app.DB.Where("id = ?", account.ID).First(&closed).Error)
	assert.Equal(t, models.BillingAccountStatusClosed, closed.Status)
	assert.NotNil(t, closed.ClosedAt)
}

func TestDeleteOrganizationRequiresAuditedChannelDeprovision(t *testing.T) {
	testCases := []struct {
		name       string
		channel    models.Channel
		provider   string
		externalID string
		metadata   models.JSONB
	}{
		{
			name:       "managed Messenger relay",
			channel:    models.ChannelMessenger,
			provider:   channelapi.RelayProvider,
			externalID: testutil.UniqueNumericID(t, "7"),
			metadata:   models.JSONB{"management_mode": "meta_messenger_oauth"},
		},
		{
			name:       "protected legacy Messenger relay",
			channel:    models.ChannelMessenger,
			provider:   channelapi.RelayProvider,
			externalID: testutil.UniqueNumericID(t, "7"),
			metadata:   models.JSONB{},
		},
		{
			name:       "Instagram relay",
			channel:    models.ChannelInstagram,
			provider:   channelapi.RelayProvider,
			externalID: testutil.UniqueNumericID(t, "7"),
			metadata:   models.JSONB{},
		},
		{
			name:       "Threads OAuth",
			channel:    models.ChannelThreads,
			provider:   channelapi.ThreadsProvider,
			externalID: testutil.UniqueNumericID(t, "7"),
			metadata:   models.JSONB{},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			app := newTestApp(t)
			controlOrganization := testutil.CreateTestOrganization(t, app.DB)
			owner := testutil.CreateTestUser(
				t,
				app.DB,
				controlOrganization.ID,
				testutil.WithSuperAdmin(),
			)
			reseller := testutil.CreateTestReseller(t, app.DB)
			target := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
			sibling := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
			billingAccount, subscription := createOrganizationSubscription(
				t,
				app,
				target,
				models.BillingProviderManual,
			)
			binding := models.ChannelAccount{
				BaseModel:         models.BaseModel{ID: uuid.New()},
				OrganizationID:    target.ID,
				Channel:           testCase.channel,
				Provider:          testCase.provider,
				Name:              testCase.name,
				ExternalAccountID: testCase.externalID,
				Status:            models.ChannelAccountStatusActive,
				Capabilities:      models.JSONB{},
				Config:            models.JSONB{"outbound_enabled": true},
				Metadata:          testCase.metadata,
			}
			require.NoError(t, app.DB.Create(&binding).Error)

			request := testutil.NewJSONRequest(t, nil)
			testutil.SetAuthContext(request, controlOrganization.ID, owner.ID)
			testutil.SetPathParam(request, "id", target.ID.String())
			require.NoError(t, app.DeleteOrganization(request))
			testutil.AssertErrorResponse(
				t,
				request,
				fasthttp.StatusConflict,
				"Audited deprovision is required",
			)

			var unchangedOrganization models.Organization
			require.NoError(t, app.DB.Where("id = ?", target.ID).First(&unchangedOrganization).Error)
			assert.False(t, unchangedOrganization.DeletedAt.Valid)

			var unchangedBinding models.ChannelAccount
			require.NoError(t, app.DB.Where("id = ?", binding.ID).First(&unchangedBinding).Error)
			assert.False(t, unchangedBinding.DeletedAt.Valid)
			assert.Equal(t, models.ChannelAccountStatusActive, unchangedBinding.Status)

			var unchangedSubscription models.Subscription
			require.NoError(t, app.DB.Where("id = ?", subscription.ID).First(&unchangedSubscription).Error)
			assert.Equal(t, models.SubscriptionStatusActive, unchangedSubscription.Status)
			assert.Nil(t, unchangedSubscription.CanceledAt)
			assert.Nil(t, unchangedSubscription.EndedAt)

			var unchangedBillingAccount models.BillingAccount
			require.NoError(t, app.DB.Where("id = ?", billingAccount.ID).First(&unchangedBillingAccount).Error)
			assert.Equal(t, models.BillingAccountStatusActive, unchangedBillingAccount.Status)
			assert.Nil(t, unchangedBillingAccount.ClosedAt)

			// The failed archive leaves the non-deleted row in the production
			// partial unique index, so another workspace cannot claim the same
			// provider-routable identity before audited deprovision.
			duplicate := binding
			duplicate.BaseModel = models.BaseModel{ID: uuid.New()}
			duplicate.OrganizationID = sibling.ID
			duplicate.Name = testCase.name + " duplicate"
			require.Error(t, app.DB.Create(&duplicate).Error)
		})
	}
}

func TestDeleteOrganizationRejectsCurrentAndLastWorkspace(t *testing.T) {
	t.Run("current workspace", func(t *testing.T) {
		app := newTestApp(t)
		reseller := testutil.CreateTestReseller(t, app.DB)
		target := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
		testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
		owner := testutil.CreateTestUser(
			t,
			app.DB,
			target.ID,
			testutil.WithSuperAdmin(),
		)

		request := testutil.NewJSONRequest(t, nil)
		testutil.SetAuthContext(request, target.ID, owner.ID)
		testutil.SetPathParam(request, "id", target.ID.String())
		require.NoError(t, app.DeleteOrganization(request))
		testutil.AssertErrorResponse(
			t,
			request,
			fasthttp.StatusConflict,
			"Switch to a different organization",
		)
	})

	t.Run("last portfolio workspace", func(t *testing.T) {
		app := newTestApp(t)
		controlOrganization := testutil.CreateTestOrganization(t, app.DB)
		owner := testutil.CreateTestUser(
			t,
			app.DB,
			controlOrganization.ID,
			testutil.WithSuperAdmin(),
		)
		reseller := testutil.CreateTestReseller(t, app.DB)
		target := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)

		request := testutil.NewJSONRequest(t, nil)
		testutil.SetAuthContext(request, controlOrganization.ID, owner.ID)
		testutil.SetPathParam(request, "id", target.ID.String())
		require.NoError(t, app.DeleteOrganization(request))
		testutil.AssertErrorResponse(
			t,
			request,
			fasthttp.StatusConflict,
			"must retain at least one organization",
		)
	})
}

func TestDeleteOrganizationRejectsProviderManagedLicenseAndNonOwner(
	t *testing.T,
) {
	t.Run("provider managed license", func(t *testing.T) {
		app := newTestApp(t)
		controlOrganization := testutil.CreateTestOrganization(t, app.DB)
		owner := testutil.CreateTestUser(
			t,
			app.DB,
			controlOrganization.ID,
			testutil.WithSuperAdmin(),
		)
		reseller := testutil.CreateTestReseller(t, app.DB)
		target := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
		testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
		_, subscription := createOrganizationSubscription(
			t,
			app,
			target,
			models.BillingProviderStripe,
		)

		request := testutil.NewJSONRequest(t, nil)
		testutil.SetAuthContext(request, controlOrganization.ID, owner.ID)
		testutil.SetPathParam(request, "id", target.ID.String())
		require.NoError(t, app.DeleteOrganization(request))
		testutil.AssertErrorResponse(
			t,
			request,
			fasthttp.StatusConflict,
			"Cancel the provider-managed subscription",
		)

		var unchanged models.Subscription
		require.NoError(t, app.DB.Where("id = ?", subscription.ID).First(&unchanged).Error)
		assert.Equal(t, models.SubscriptionStatusActive, unchanged.Status)
	})

	t.Run("non platform owner", func(t *testing.T) {
		app := newTestApp(t)
		controlOrganization := testutil.CreateTestOrganization(t, app.DB)
		user := testutil.CreateTestUser(t, app.DB, controlOrganization.ID)
		reseller := testutil.CreateTestReseller(t, app.DB)
		target := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
		testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)

		request := testutil.NewJSONRequest(t, nil)
		testutil.SetAuthContext(request, controlOrganization.ID, user.ID)
		testutil.SetPathParam(request, "id", target.ID.String())
		require.NoError(t, app.DeleteOrganization(request))
		testutil.AssertErrorResponse(
			t,
			request,
			fasthttp.StatusForbidden,
			"Only a platform owner",
		)
	})
}
