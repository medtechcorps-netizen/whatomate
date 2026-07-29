package handlers_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func crmInsightsPermissions(
	t *testing.T,
	app *handlers.App,
	resources ...string,
) []models.Permission {
	t.Helper()
	wanted := make(map[string]bool, len(resources))
	for _, resource := range resources {
		wanted[resource] = true
	}

	all := testutil.GetOrCreateTestPermissions(t, app.DB)
	permissions := make([]models.Permission, 0, len(resources))
	for _, permission := range all {
		if permission.Action == models.ActionRead && wanted[permission.Resource] {
			permissions = append(permissions, permission)
		}
	}
	require.Len(t, permissions, len(resources))
	return permissions
}

func allCRMInsightsPermissions(t *testing.T, app *handlers.App) []models.Permission {
	t.Helper()
	return crmInsightsPermissions(
		t,
		app,
		models.ResourceContacts,
		models.ResourceCRMLeads,
		models.ResourceTasks,
		models.ResourceBookings,
		models.ResourcePackages,
		models.ResourcePayments,
	)
}

func enableCRMInsightsTestEntitlements(
	t *testing.T,
	app *handlers.App,
	orgID, userID uuid.UUID,
	keys ...string,
) {
	t.Helper()
	snapshot := models.JSONB{}
	for _, key := range keys {
		snapshot[key] = true
	}
	plan := models.Plan{
		BaseModel:   models.BaseModel{ID: uuid.New()},
		ScopeKey:    "crm-insights-" + uuid.NewString(),
		Code:        "crm-insights-" + uuid.NewString(),
		Name:        "CRM Insights test plan",
		Status:      models.CommercialPlanStatusActive,
		Vertical:    "general",
		IsPublic:    false,
		Metadata:    models.JSONB{},
		CreatedByID: &userID,
	}
	require.NoError(t, app.DB.Create(&plan).Error)
	account := models.BillingAccount{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgID,
		Provider:        models.BillingProviderManual,
		Status:          models.BillingAccountStatusActive,
		DefaultCurrency: "MYR",
		BillingProfile:  models.JSONB{},
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&account).Error)
	periodStart := time.Now().UTC().Add(-time.Hour)
	periodEnd := periodStart.AddDate(0, 1, 0)
	subscription := models.Subscription{
		BaseModel:            models.BaseModel{ID: uuid.New()},
		OrganizationID:       orgID,
		BillingAccountID:     account.ID,
		PlanID:               plan.ID,
		Provider:             models.BillingProviderManual,
		Status:               models.SubscriptionStatusActive,
		Quantity:             1,
		CollectionMethod:     "send_invoice",
		EntitlementsSnapshot: snapshot,
		ProviderData:         models.JSONB{},
		CurrentPeriodStart:   &periodStart,
		CurrentPeriodEnd:     &periodEnd,
		CreatedByID:          &userID,
	}
	require.NoError(t, app.DB.Create(&subscription).Error)
}

func createCRMInsightsTask(
	t *testing.T,
	app *handlers.App,
	orgID, contactID uuid.UUID,
	dueAt time.Time,
) *models.FollowUpTask {
	t.Helper()
	task := &models.FollowUpTask{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		ContactID:      &contactID,
		Title:          "Follow up",
		Status:         models.FollowUpTaskStatusOpen,
		Priority:       models.FollowUpTaskPriorityNormal,
		DueAt:          &dueAt,
		Source:         "test",
		IdempotencyKey: "crm-insights-test-" + uuid.NewString(),
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, app.DB.Create(task).Error)
	return task
}

func TestApp_GetCRMInsights_ExactContractAndNonNullMoneyArrays(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleExact(
		t,
		app.DB,
		org.ID,
		"CRM Insights",
		false,
		false,
		allCRMInsightsPermissions(t, app),
	)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("crm-insights")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)
	enableCRMInsightsTestEntitlements(
		t,
		app,
		org.ID,
		user.ID,
		"crm.enabled",
		"bookings.enabled",
		"commerce.enabled",
	)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	createCRMInsightsTask(t, app, org.ID, contact.ID, time.Now().UTC().Add(-time.Hour))

	today := time.Now().UTC().Format("2006-01-02")
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetQueryParam(req, "from", today)
	testutil.SetQueryParam(req, "to", today)

	require.NoError(t, app.GetCRMInsights(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var response struct {
		Data handlers.CRMInsightsResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
	assert.NotEmpty(t, response.Data.Range.From)
	assert.NotEmpty(t, response.Data.Range.To)
	assert.NotEmpty(t, response.Data.GeneratedAt)
	assert.NotNil(t, response.Data.Pipeline.OpenValue)
	assert.NotNil(t, response.Data.Revenue.Collected)
	assert.NotNil(t, response.Data.Revenue.Outstanding)
	assert.Equal(t, int64(1), response.Data.Tasks.Open)
	assert.Equal(t, int64(1), response.Data.Tasks.Overdue)
	assert.InDelta(t, 0, response.Data.Pipeline.ConversionRate, 0.001)
	assert.InDelta(t, 0, response.Data.Bookings.AttendanceRate, 0.001)
	assert.InDelta(t, 0, response.Data.Bookings.NoShowRate, 0.001)
}

func TestApp_GetCRMInsights_DoesNotLeakRevenueWithoutPaymentsPermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleExact(
		t,
		app.DB,
		org.ID,
		"Tasks Only",
		false,
		false,
		crmInsightsPermissions(t, app, models.ResourceTasks),
	)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("crm-insights-tasks")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)
	enableCRMInsightsTestEntitlements(t, app, org.ID, user.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	now := time.Now().UTC()
	invoice := &models.CommerceInvoice{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		ContactID:      contact.ID,
		InvoiceNumber:  "INV-" + uuid.NewString()[:8],
		IdempotencyKey: "invoice-" + uuid.NewString(),
		Status:         models.CommerceInvoiceStatusOpen,
		Currency:       "MYR",
		SubtotalMinor:  25000,
		TotalMinor:     25000,
		DueMinor:       25000,
		IssuedAt:       &now,
		DueAt:          timePointer(now.Add(-24 * time.Hour)),
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, app.DB.Create(invoice).Error)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetQueryParam(req, "from", now.Format("2006-01-02"))
	testutil.SetQueryParam(req, "to", now.Format("2006-01-02"))

	require.NoError(t, app.GetCRMInsights(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var response struct {
		Data handlers.CRMInsightsResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
	assert.Empty(t, response.Data.Revenue.Collected)
	assert.Empty(t, response.Data.Revenue.Outstanding)
	assert.Zero(t, response.Data.Revenue.OverdueInvoices)
}

func TestApp_GetCRMInsights_RequiresARelevantReadPermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("crm-insights-forbidden")),
		testutil.WithPassword("password"),
	)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)

	require.NoError(t, app.GetCRMInsights(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_GetCRMInsights_RequiresCRMEntitlement(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleExact(
		t,
		app.DB,
		org.ID,
		"Unlicensed CRM Insights",
		false,
		false,
		crmInsightsPermissions(t, app, models.ResourceTasks),
	)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("crm-insights-unlicensed")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	require.NoError(t, app.GetCRMInsights(req))
	assert.Equal(t, fasthttp.StatusPaymentRequired, testutil.GetResponseStatusCode(req))
}

func TestApp_GetCRMInsights_SeparatesPeriodOutcomesFromCurrentState(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleExact(
		t,
		app.DB,
		org.ID,
		"CRM Current State",
		false,
		false,
		crmInsightsPermissions(
			t,
			app,
			models.ResourceCRMLeads,
			models.ResourceTasks,
			models.ResourcePayments,
		),
	)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("crm-insights-current-state")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)
	enableCRMInsightsTestEntitlements(
		t,
		app,
		org.ID,
		user.ID,
		"crm.enabled",
		"commerce.enabled",
	)

	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	pipeline := models.CRMPipeline{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Current state",
		IsActive:       true,
		Version:        1,
	}
	require.NoError(t, app.DB.Create(&pipeline).Error)
	stage := models.CRMPipelineStage{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PipelineID:     pipeline.ID,
		Name:           "Open",
		Kind:           models.CRMPipelineStageKindOpen,
		IsActive:       true,
		Version:        1,
	}
	require.NoError(t, app.DB.Create(&stage).Error)

	now := time.Now().UTC()
	old := now.AddDate(0, 0, -90)
	openLead := models.CRMLead{
		BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: old, UpdatedAt: old},
		OrganizationID: org.ID,
		ContactID:      contact.ID,
		PipelineID:     pipeline.ID,
		StageID:        stage.ID,
		Title:          "Old but still open",
		Status:         models.CRMLeadStatusOpen,
		Source:         models.CRMLeadSourceOther,
		ValueMinor:     12500,
		Currency:       "MYR",
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, app.DB.Create(&openLead).Error)
	wonLead := openLead
	wonLead.ID = uuid.New()
	wonLead.Title = "Won today"
	wonLead.Status = models.CRMLeadStatusWon
	wonLead.WonAt = &now
	wonLead.ValueMinor = 5000
	require.NoError(t, app.DB.Create(&wonLead).Error)
	lostLead := openLead
	lostLead.ID = uuid.New()
	lostLead.Title = "Lost before the period"
	lostLead.Status = models.CRMLeadStatusLost
	lostLead.LostAt = &old
	require.NoError(t, app.DB.Create(&lostLead).Error)

	task := createCRMInsightsTask(t, app, org.ID, contact.ID, now.Add(-time.Hour))
	require.NoError(t, app.DB.Model(task).Updates(map[string]any{
		"created_at": old,
		"updated_at": old,
	}).Error)
	invoice := models.CommerceInvoice{
		BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: old, UpdatedAt: old},
		OrganizationID: org.ID,
		ContactID:      contact.ID,
		InvoiceNumber:  "INV-" + uuid.NewString()[:8],
		IdempotencyKey: "invoice-" + uuid.NewString(),
		Status:         models.CommerceInvoiceStatusOpen,
		Currency:       "MYR",
		SubtotalMinor:  9900,
		TotalMinor:     9900,
		DueMinor:       9900,
		IssuedAt:       &old,
		DueAt:          timePointer(now.Add(-24 * time.Hour)),
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, app.DB.Create(&invoice).Error)

	today := now.Format("2006-01-02")
	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetQueryParam(req, "from", today)
	testutil.SetQueryParam(req, "to", today)
	require.NoError(t, app.GetCRMInsights(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var response struct {
		Data handlers.CRMInsightsResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
	assert.Equal(t, int64(1), response.Data.Pipeline.OpenCount)
	assert.Equal(t, int64(1), response.Data.Pipeline.WonCount)
	assert.Zero(t, response.Data.Pipeline.LostCount)
	require.Equal(t, []handlers.CRMInsightsMoney{{
		Currency:    "MYR",
		AmountMinor: 12500,
	}}, response.Data.Pipeline.OpenValue)
	assert.Equal(t, int64(1), response.Data.Tasks.Open)
	assert.Equal(t, int64(1), response.Data.Tasks.Overdue)
	require.Equal(t, []handlers.CRMInsightsMoney{{
		Currency:    "MYR",
		AmountMinor: 9900,
	}}, response.Data.Revenue.Outstanding)
	assert.Equal(t, int64(1), response.Data.Revenue.OverdueInvoices)
}

func TestApp_GetCRMInsights_RejectsUnboundedRange(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleExact(
		t,
		app.DB,
		org.ID,
		"Bounded CRM Insights",
		false,
		false,
		crmInsightsPermissions(t, app, models.ResourceTasks),
	)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("crm-insights-range")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)
	enableCRMInsightsTestEntitlements(t, app, org.ID, user.ID, "crm.enabled")

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetQueryParam(req, "from", "2024-01-01")
	testutil.SetQueryParam(req, "to", "2025-12-31")
	require.NoError(t, app.GetCRMInsights(req))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_ListCRMSystemSegments_CountsAndContactsAreTenantScoped(t *testing.T) {
	app := newTestApp(t)
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleExact(
		t,
		app.DB,
		orgA.ID,
		"CRM Segments",
		false,
		false,
		allCRMInsightsPermissions(t, app),
	)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		orgA.ID,
		testutil.WithEmail(testutil.UniqueEmail("crm-segments")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)
	enableCRMInsightsTestEntitlements(
		t,
		app,
		orgA.ID,
		user.ID,
		"crm.enabled",
		"bookings.enabled",
		"commerce.enabled",
	)

	now := time.Now().UTC()
	contactA1 := testutil.CreateTestContact(t, app.DB, orgA.ID)
	contactA2 := testutil.CreateTestContact(t, app.DB, orgA.ID)
	contactB := testutil.CreateTestContact(t, app.DB, orgB.ID)
	require.NoError(t, app.DB.Model(&models.Contact{}).
		Where("id IN ?", []uuid.UUID{contactA1.ID, contactA2.ID, contactB.ID}).
		Update("last_message_at", now).Error)
	createCRMInsightsTask(t, app, orgA.ID, contactA1.ID, now.Add(-time.Hour))
	createCRMInsightsTask(t, app, orgA.ID, contactA2.ID, now.Add(-time.Hour))
	createCRMInsightsTask(t, app, orgB.ID, contactB.ID, now.Add(-time.Hour))

	listRequest := testutil.NewGETRequest(t)
	testutil.SetAuthContext(listRequest, orgA.ID, user.ID)
	require.NoError(t, app.ListCRMSystemSegments(listRequest))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(listRequest))

	var listResponse struct {
		Data struct {
			Segments []handlers.CRMSystemSegment `json:"segments"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(listRequest), &listResponse))
	require.Len(t, listResponse.Data.Segments, 7)
	counts := make(map[string]int64, len(listResponse.Data.Segments))
	for _, segment := range listResponse.Data.Segments {
		counts[segment.Key] = segment.Count
	}
	assert.Equal(t, int64(2), counts["needs_follow_up"])

	contactsRequest := testutil.NewGETRequest(t)
	testutil.SetAuthContext(contactsRequest, orgA.ID, user.ID)
	testutil.SetPathParam(contactsRequest, "key", "needs_follow_up")
	testutil.SetQueryParam(contactsRequest, "page", 1)
	testutil.SetQueryParam(contactsRequest, "limit", 1)
	require.NoError(t, app.ListCRMSystemSegmentContacts(contactsRequest))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(contactsRequest))

	var contactsResponse struct {
		Data struct {
			Segment  handlers.CRMSystemSegment          `json:"segment"`
			Contacts []handlers.CRMSystemSegmentContact `json:"contacts"`
			Total    int64                              `json:"total"`
			Page     int                                `json:"page"`
			Limit    int                                `json:"limit"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(contactsRequest), &contactsResponse))
	assert.Equal(t, "needs_follow_up", contactsResponse.Data.Segment.Key)
	assert.Equal(t, int64(2), contactsResponse.Data.Total)
	assert.Equal(t, 1, contactsResponse.Data.Page)
	assert.Equal(t, 1, contactsResponse.Data.Limit)
	require.Len(t, contactsResponse.Data.Contacts, 1)
	assert.NotEqual(t, contactB.ID, contactsResponse.Data.Contacts[0].ID)
}

func TestApp_ListCRMSystemSegments_OmitsSectionsWithoutUnderlyingPermission(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleExact(
		t,
		app.DB,
		org.ID,
		"Contact Tasks",
		false,
		false,
		crmInsightsPermissions(
			t,
			app,
			models.ResourceContacts,
			models.ResourceTasks,
		),
	)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("crm-segments-limited")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)
	enableCRMInsightsTestEntitlements(t, app, org.ID, user.ID, "crm.enabled")

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	require.NoError(t, app.ListCRMSystemSegments(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var response struct {
		Data struct {
			Segments []handlers.CRMSystemSegment `json:"segments"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
	keys := make([]string, 0, len(response.Data.Segments))
	for _, segment := range response.Data.Segments {
		keys = append(keys, segment.Key)
	}
	assert.ElementsMatch(t, []string{"needs_follow_up"}, keys)
}

func TestApp_ListCRMSystemSegmentContacts_AppliesOrganizationMasking(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	require.NoError(t, app.DB.Model(&models.Organization{}).
		Where("id = ?", org.ID).
		Update("settings", models.JSONB{
			"mask_phone_numbers": true,
			"timezone":           "Asia/Kuala_Lumpur",
		}).Error)
	role := testutil.CreateTestRoleExact(
		t,
		app.DB,
		org.ID,
		"Masked CRM Segments",
		false,
		false,
		crmInsightsPermissions(
			t,
			app,
			models.ResourceContacts,
			models.ResourceTasks,
		),
	)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("crm-segments-masked")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)
	enableCRMInsightsTestEntitlements(t, app, org.ID, user.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	const rawPhone = "+60123456789"
	require.NoError(t, app.DB.Model(contact).Updates(map[string]any{
		"profile_name": rawPhone,
		"phone_number": rawPhone,
	}).Error)
	createCRMInsightsTask(t, app, org.ID, contact.ID, time.Now().UTC().Add(-time.Hour))

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "key", "needs_follow_up")
	require.NoError(t, app.ListCRMSystemSegmentContacts(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	var response struct {
		Data struct {
			Contacts []handlers.CRMSystemSegmentContact `json:"contacts"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &response))
	require.Len(t, response.Data.Contacts, 1)
	assert.NotEqual(t, rawPhone, response.Data.Contacts[0].ProfileName)
	assert.NotEqual(t, rawPhone, response.Data.Contacts[0].PhoneNumber)
}

func TestApp_ListCRMSystemSegmentContacts_RejectsUnknownKey(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleExact(
		t,
		app.DB,
		org.ID,
		"CRM Segment Invalid",
		false,
		false,
		crmInsightsPermissions(t, app, models.ResourceContacts),
	)
	user := testutil.CreateTestUser(
		t,
		app.DB,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("crm-segment-invalid")),
		testutil.WithPassword("password"),
		testutil.WithRoleID(&role.ID),
	)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "key", fmt.Sprintf("invalid-%s", uuid.NewString()))
	require.NoError(t, app.ListCRMSystemSegmentContacts(req))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(req))
}

func timePointer(value time.Time) *time.Time {
	return &value
}
