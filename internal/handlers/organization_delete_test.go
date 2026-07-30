package handlers_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
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
