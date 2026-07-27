package handlers

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

func TestProductCRMLeadValidation(t *testing.T) {
	t.Parallel()

	valid := func() CreateCRMLeadRequest {
		return CreateCRMLeadRequest{
			ContactID:  uuid.New(),
			PipelineID: uuid.New(),
			Title:      "Qualified enquiry",
			Source:     models.CRMLeadSourceWhatsApp,
			ValueMinor: 12500,
			Currency:   "MYR",
		}
	}

	tests := []struct {
		name    string
		mutate  func(*CreateCRMLeadRequest)
		wantErr string
	}{
		{
			name: "accepts valid integer minor amount and currency",
		},
		{
			name: "rejects negative minor amount",
			mutate: func(req *CreateCRMLeadRequest) {
				req.ValueMinor = -1
			},
			wantErr: "value_minor cannot be negative",
		},
		{
			name: "rejects malformed currency",
			mutate: func(req *CreateCRMLeadRequest) {
				req.Currency = "RM"
			},
			wantErr: "three-letter ISO code",
		},
		{
			name: "rejects unsupported source",
			mutate: func(req *CreateCRMLeadRequest) {
				req.Source = models.CRMLeadSource("spreadsheet_magic")
			},
			wantErr: "invalid lead source",
		},
		{
			name: "rejects overlong title",
			mutate: func(req *CreateCRMLeadRequest) {
				req.Title = strings.Repeat("x", 256)
			},
			wantErr: "title must be at most 255 characters",
		},
		{
			name: "requires tenant references",
			mutate: func(req *CreateCRMLeadRequest) {
				req.ContactID = uuid.Nil
			},
			wantErr: "contact_id is required",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := valid()
			if tt.mutate != nil {
				tt.mutate(&req)
			}

			err := validateCreateCRMLeadRequest(&req)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestProductCRMTaskValidation(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	dueAt := now.Add(2 * time.Hour)
	remindAt := now.Add(time.Hour)
	require.NoError(t, validateFollowUpTaskSchedule(&dueAt, &remindAt))

	afterDue := dueAt.Add(time.Minute)
	err := validateFollowUpTaskSchedule(&dueAt, &afterDue)
	require.EqualError(t, err, "remind_at cannot be after due_at")

	completed := models.FollowUpTaskStatusCompleted
	update := UpdateFollowUpTaskRequest{
		Version: 1,
		Status:  &completed,
	}
	err = validateUpdateFollowUpTaskRequest(&update)
	require.EqualError(t, err, "use the complete task endpoint to mark a task completed")

	title := strings.Repeat("t", 256)
	update = UpdateFollowUpTaskRequest{
		Version: 1,
		Title:   &title,
	}
	err = validateUpdateFollowUpTaskRequest(&update)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "title must be at most 255 characters")
}

func TestApp_CreateCRMLeadRejectsCrossTenantContact(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	userA := testutil.CreateTestUser(t, app.DB, orgA.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, app.DB, orgA.ID, userA.ID, "crm.enabled")
	contactB := testutil.CreateTestContact(t, app.DB, orgB.ID)
	pipeline, stage := createProductCRMTestPipeline(t, app.DB, orgA.ID, userA.ID)

	req := testutil.NewJSONRequest(t, CreateCRMLeadRequest{
		ContactID:  contactB.ID,
		PipelineID: pipeline.ID,
		StageID:    &stage.ID,
		Title:      "Cross-tenant lead",
		Source:     models.CRMLeadSourceAPI,
		ValueMinor: 1000,
		Currency:   "MYR",
	})
	testutil.SetAuthContext(req, orgA.ID, userA.ID)

	require.NoError(t, app.CreateCRMLead(req))
	testutil.AssertErrorResponse(
		t,
		req,
		fasthttp.StatusBadRequest,
		"contact_id does not belong to the organization",
	)

	var leadCount int64
	require.NoError(t, app.DB.Model(&models.CRMLead{}).
		Where("organization_id = ?", orgA.ID).
		Count(&leadCount).Error)
	assert.Zero(t, leadCount)
}

func TestApp_CreateCRMLeadIdempotencyIsAtomicAndPayloadBound(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, user.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	pipeline, stage := createProductCRMTestPipeline(t, app.DB, org.ID, user.ID)

	idempotencyKey := "lead-" + uuid.NewString()
	payload := CreateCRMLeadRequest{
		ContactID:      contact.ID,
		PipelineID:     pipeline.ID,
		StageID:        &stage.ID,
		Title:          "Concurrent lead",
		Source:         models.CRMLeadSourceAPI,
		Currency:       "MYR",
		IdempotencyKey: idempotencyKey,
	}
	requests := []*fastglue.Request{
		testutil.NewJSONRequest(t, payload),
		testutil.NewJSONRequest(t, payload),
	}
	for _, req := range requests {
		testutil.SetAuthContext(req, org.ID, user.ID)
	}

	start := make(chan struct{})
	errs := make([]error, len(requests))
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			errs[index] = app.CreateCRMLead(requests[index])
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range requests {
		require.NoError(t, errs[i])
		assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(requests[i]))
	}

	var leads []models.CRMLead
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND idempotency_key = ?",
		org.ID,
		idempotencyKey,
	).Find(&leads).Error)
	require.Len(t, leads, 1)
	require.NotEmpty(t, leads[0].RequestFingerprint)

	var historyCount int64
	require.NoError(t, app.DB.Model(&models.CRMStageHistory{}).
		Where("organization_id = ? AND lead_id = ?", org.ID, leads[0].ID).
		Count(&historyCount).Error)
	assert.EqualValues(t, 1, historyCount)

	payload.Title = "Different payload"
	conflict := testutil.NewJSONRequest(t, payload)
	testutil.SetAuthContext(conflict, org.ID, user.ID)
	require.NoError(t, app.CreateCRMLead(conflict))
	testutil.AssertErrorResponse(
		t,
		conflict,
		fasthttp.StatusConflict,
		"idempotency_key was already used with a different CRM lead payload",
	)
}

func TestApp_CreateFollowUpTaskIdempotencyIsPayloadBound(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, user.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	payload := CreateFollowUpTaskRequest{
		ContactID:      &contact.ID,
		Title:          "Call the customer",
		IdempotencyKey: "task-" + uuid.NewString(),
	}
	first := testutil.NewJSONRequest(t, payload)
	testutil.SetAuthContext(first, org.ID, user.ID)
	require.NoError(t, app.CreateFollowUpTask(first))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))

	replay := testutil.NewJSONRequest(t, payload)
	testutil.SetAuthContext(replay, org.ID, user.ID)
	require.NoError(t, app.CreateFollowUpTask(replay))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(replay))

	var tasks []models.FollowUpTask
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND idempotency_key = ?",
		org.ID,
		payload.IdempotencyKey,
	).Find(&tasks).Error)
	require.Len(t, tasks, 1)
	require.NotEmpty(t, tasks[0].RequestFingerprint)

	payload.Description = "This changes the request"
	conflict := testutil.NewJSONRequest(t, payload)
	testutil.SetAuthContext(conflict, org.ID, user.ID)
	require.NoError(t, app.CreateFollowUpTask(conflict))
	testutil.AssertErrorResponse(
		t,
		conflict,
		fasthttp.StatusConflict,
		"idempotency_key was already used with a different follow-up task payload",
	)
}

func TestValidateFollowUpTaskReferencesRejectsBookingContactMismatch(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	bookingContact := testutil.CreateTestContact(t, db, org.ID)
	otherContact := testutil.CreateTestContact(t, db, org.ID)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())

	service := models.BookingService{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		Name:            "Task validation service",
		Kind:            models.BookingServiceKindAppointment,
		DurationMinutes: 30,
		DefaultCapacity: 1,
		Currency:        "MYR",
		IsActive:        true,
		Metadata:        models.JSONB{},
		Version:         1,
	}
	require.NoError(t, db.Create(&service).Error)
	resource := models.BookingResource{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Task validation room",
		Kind:           models.BookingResourceKindRoom,
		Timezone:       "Asia/Kuala_Lumpur",
		IsActive:       true,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, db.Create(&resource).Error)
	startsAt := time.Now().UTC().Add(time.Hour)
	event := models.BookingEvent{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		ServiceID:      service.ID,
		ResourceID:     resource.ID,
		StartsAt:       startsAt,
		EndsAt:         startsAt.Add(30 * time.Minute),
		Capacity:       1,
		Status:         models.BookingEventStatusScheduled,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, db.Create(&event).Error)
	booking := models.Booking{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		EventID:        event.ID,
		ContactID:      bookingContact.ID,
		Status:         models.BookingStatusReserved,
		Quantity:       1,
		Source:         models.BookingSourceAgent,
		IdempotencyKey: "booking-" + uuid.NewString(),
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, db.Create(&booking).Error)

	err := validateFollowUpTaskReferences(
		db,
		org.ID,
		&otherContact.ID,
		nil,
		&booking.ID,
		&user.ID,
		nil,
		uuid.Nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contact_id does not match the selected booking")
}

func TestApp_MoveCRMLeadTenantAndOptimisticAtomicity(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	userA := testutil.CreateTestUser(t, app.DB, orgA.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, app.DB, orgA.ID, userA.ID, "crm.enabled")
	userB := testutil.CreateTestUser(t, app.DB, orgB.ID, testutil.WithSuperAdmin())
	contactA := testutil.CreateTestContact(t, app.DB, orgA.ID)

	pipelineA, openStage := createProductCRMTestPipeline(t, app.DB, orgA.ID, userA.ID)
	wonStage := &models.CRMPipelineStage{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgA.ID,
		PipelineID:     pipelineA.ID,
		Name:           "Won",
		Kind:           models.CRMPipelineStageKindWon,
		DisplayOrder:   1,
		Probability:    100,
		IsActive:       true,
		Version:        1,
		CreatedByID:    &userA.ID,
		UpdatedByID:    &userA.ID,
	}
	require.NoError(t, app.DB.Create(wonStage).Error)

	_, otherTenantStage := createProductCRMTestPipeline(t, app.DB, orgB.ID, userB.ID)
	now := time.Now().UTC()
	lead := &models.CRMLead{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgA.ID,
		ContactID:      contactA.ID,
		PipelineID:     pipelineA.ID,
		StageID:        openStage.ID,
		Title:          "Optimistic move",
		Status:         models.CRMLeadStatusOpen,
		OwnerUserID:    &userA.ID,
		Source:         models.CRMLeadSourceReferral,
		ValueMinor:     50000,
		Currency:       "MYR",
		LastActivityAt: &now,
		Metadata:       models.JSONB{},
		Version:        1,
		CreatedByID:    &userA.ID,
		UpdatedByID:    &userA.ID,
	}
	require.NoError(t, app.DB.Create(lead).Error)

	crossTenantReq := newProductCRMMoveRequest(
		t,
		orgA.ID,
		userA.ID,
		lead.ID,
		otherTenantStage.ID,
		lead.Version,
	)
	require.NoError(t, app.MoveCRMLead(crossTenantReq))
	testutil.AssertErrorResponse(
		t,
		crossTenantReq,
		fasthttp.StatusBadRequest,
		"stage_id does not belong to the lead's active pipeline",
	)
	assertProductCRMAtomicCounts(t, app.DB, orgA.ID, lead.ID, 0, 0, 0)

	staleReq := newProductCRMMoveRequest(
		t,
		orgA.ID,
		userA.ID,
		lead.ID,
		wonStage.ID,
		lead.Version+1,
	)
	require.NoError(t, app.MoveCRMLead(staleReq))
	testutil.AssertErrorResponse(
		t,
		staleReq,
		fasthttp.StatusConflict,
		"CRM lead was modified; refresh and retry",
	)
	assertProductCRMAtomicCounts(t, app.DB, orgA.ID, lead.ID, 0, 0, 0)

	moveReq := newProductCRMMoveRequest(
		t,
		orgA.ID,
		userA.ID,
		lead.ID,
		wonStage.ID,
		lead.Version,
	)
	require.NoError(t, app.MoveCRMLead(moveReq))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(moveReq))

	var moved models.CRMLead
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", lead.ID, orgA.ID).
		First(&moved).Error)
	assert.Equal(t, wonStage.ID, moved.StageID)
	assert.Equal(t, models.CRMLeadStatusWon, moved.Status)
	assert.EqualValues(t, 2, moved.Version)
	require.NotNil(t, moved.WonAt)
	assert.Nil(t, moved.LostAt)
	assertProductCRMAtomicCounts(t, app.DB, orgA.ID, lead.ID, 1, 1, 1)

	replayReq := newProductCRMMoveRequest(
		t,
		orgA.ID,
		userA.ID,
		lead.ID,
		wonStage.ID,
		lead.Version,
	)
	require.NoError(t, app.MoveCRMLead(replayReq))
	testutil.AssertErrorResponse(
		t,
		replayReq,
		fasthttp.StatusConflict,
		"CRM lead was modified; refresh and retry",
	)
	assertProductCRMAtomicCounts(t, app.DB, orgA.ID, lead.ID, 1, 1, 1)
}

func newProductCRMDatabaseTestApp(t *testing.T) *App {
	t.Helper()
	db := testutil.SetupTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&models.CRMPipeline{},
		&models.CRMPipelineStage{},
		&models.CRMLead{},
		&models.CRMStageHistory{},
		&models.OutboxEvent{},
	))
	return &App{DB: db, Log: testutil.NopLogger()}
}

func createProductCRMTestPipeline(
	t *testing.T,
	db *gorm.DB,
	orgID, userID uuid.UUID,
) (*models.CRMPipeline, *models.CRMPipelineStage) {
	t.Helper()
	pipeline := &models.CRMPipeline{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		Name:           "Pipeline " + uuid.NewString()[:8],
		IsActive:       true,
		Version:        1,
		CreatedByID:    &userID,
		UpdatedByID:    &userID,
	}
	require.NoError(t, db.Create(pipeline).Error)
	stage := &models.CRMPipelineStage{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		PipelineID:     pipeline.ID,
		Name:           "Open",
		Kind:           models.CRMPipelineStageKindOpen,
		IsActive:       true,
		Version:        1,
		CreatedByID:    &userID,
		UpdatedByID:    &userID,
	}
	require.NoError(t, db.Create(stage).Error)
	return pipeline, stage
}

func newProductCRMMoveRequest(
	t *testing.T,
	orgID, userID, leadID, stageID uuid.UUID,
	version int64,
) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, MoveCRMLeadRequest{
		StageID: stageID,
		Version: version,
		Reason:  "Customer accepted proposal",
	})
	testutil.SetAuthContext(req, orgID, userID)
	testutil.SetPathParam(req, "id", leadID.String())
	return req
}

func assertProductCRMAtomicCounts(
	t *testing.T,
	db *gorm.DB,
	orgID, leadID uuid.UUID,
	wantHistory, wantOutbox, wantAudit int64,
) {
	t.Helper()
	var historyCount, outboxCount, auditCount int64
	require.NoError(t, db.Model(&models.CRMStageHistory{}).
		Where("organization_id = ? AND lead_id = ?", orgID, leadID).
		Count(&historyCount).Error)
	require.NoError(t, db.Model(&models.OutboxEvent{}).
		Where("organization_id = ? AND aggregate_id = ?", orgID, leadID).
		Count(&outboxCount).Error)
	require.NoError(t, db.Model(&models.AuditLog{}).
		Where(
			"organization_id = ? AND resource_type = ? AND resource_id = ?",
			orgID,
			productCRMLeadResource,
			leadID,
		).
		Count(&auditCount).Error)
	assert.Equal(t, wantHistory, historyCount)
	assert.Equal(t, wantOutbox, outboxCount)
	assert.Equal(t, wantAudit, auditCount)
}
