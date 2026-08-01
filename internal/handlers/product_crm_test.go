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

func TestApp_CreateCRMLeadResolvesMergedAliasToCanonicalContact(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, user.ID, "crm.enabled")
	canonical := testutil.CreateTestContact(t, app.DB, org.ID)
	alias := testutil.CreateTestContact(t, app.DB, org.ID)
	pipeline, stage := createProductCRMTestPipeline(t, app.DB, org.ID, user.ID)
	mergedAt := time.Now().UTC()
	require.NoError(t, app.DB.Unscoped().Model(alias).Updates(map[string]any{
		"merged_into_id": canonical.ID,
		"merged_at":      mergedAt,
		"merged_by_id":   user.ID,
		"deleted_at":     mergedAt,
	}).Error)

	payload := CreateCRMLeadRequest{
		ContactID:      alias.ID,
		PipelineID:     pipeline.ID,
		StageID:        &stage.ID,
		Title:          "Alias-safe journey",
		Source:         models.CRMLeadSourceAPI,
		Currency:       "MYR",
		IdempotencyKey: "alias-lead-" + uuid.NewString(),
	}
	create := testutil.NewJSONRequest(t, payload)
	testutil.SetAuthContext(create, org.ID, user.ID)
	require.NoError(t, app.CreateCRMLead(create))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(create))

	var lead models.CRMLead
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND idempotency_key = ?",
		org.ID,
		payload.IdempotencyKey,
	).First(&lead).Error)
	require.Equal(t, canonical.ID, lead.ContactID)

	var activity models.CustomerActivityEvent
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND lead_id = ? AND event_type = ?",
		org.ID,
		lead.ID,
		models.CustomerActivityCRMLeadCreated,
	).First(&activity).Error)
	require.Equal(t, canonical.ID, activity.ContactID)

	// Either spelling of the customer identity must replay the same logical
	// request after canonicalization rather than creating another journey.
	for _, contactID := range []uuid.UUID{alias.ID, canonical.ID} {
		payload.ContactID = contactID
		replay := testutil.NewJSONRequest(t, payload)
		testutil.SetAuthContext(replay, org.ID, user.ID)
		require.NoError(t, app.CreateCRMLead(replay))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(replay))
	}

	var count int64
	require.NoError(t, app.DB.Model(&models.CRMLead{}).Where(
		"organization_id = ? AND idempotency_key = ?",
		org.ID,
		payload.IdempotencyKey,
	).Count(&count).Error)
	require.EqualValues(t, 1, count)
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

func TestApp_MoveCRMLeadConcurrentWritersCommitExactlyOneTransition(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, user.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	pipeline, openStage := createProductCRMTestPipeline(t, app.DB, org.ID, user.ID)
	terminalStages := []models.CRMPipelineStage{
		{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
			PipelineID: pipeline.ID, Name: "Won", Kind: models.CRMPipelineStageKindWon,
			DisplayOrder: 1, Probability: 100, IsActive: true, Version: 1,
			CreatedByID: &user.ID, UpdatedByID: &user.ID,
		},
		{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
			PipelineID: pipeline.ID, Name: "Lost", Kind: models.CRMPipelineStageKindLost,
			DisplayOrder: 2, Probability: 0, IsActive: true, Version: 1,
			CreatedByID: &user.ID, UpdatedByID: &user.ID,
		},
	}
	require.NoError(t, app.DB.Create(&terminalStages).Error)
	now := time.Now().UTC()
	lead := models.CRMLead{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		ContactID: contact.ID, PipelineID: pipeline.ID, StageID: openStage.ID,
		Title: "Concurrent stage move", Status: models.CRMLeadStatusOpen,
		Source: models.CRMLeadSourceReferral, Currency: "MYR",
		LastActivityAt: &now, Metadata: models.JSONB{}, Version: 1,
		CreatedByID: &user.ID, UpdatedByID: &user.ID,
	}
	require.NoError(t, app.DB.Create(&lead).Error)

	requests := []*fastglue.Request{
		newProductCRMMoveRequest(t, org.ID, user.ID, lead.ID, terminalStages[0].ID, 1),
		newProductCRMMoveRequest(t, org.ID, user.ID, lead.ID, terminalStages[1].ID, 1),
	}
	start := make(chan struct{})
	errs := make([]error, len(requests))
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			errs[index] = app.MoveCRMLead(requests[index])
		}(i)
	}
	close(start)
	wg.Wait()

	statuses := []int{
		testutil.GetResponseStatusCode(requests[0]),
		testutil.GetResponseStatusCode(requests[1]),
	}
	require.ElementsMatch(t, []int{fasthttp.StatusOK, fasthttp.StatusConflict}, statuses)
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", lead.ID, org.ID).
		First(&lead).Error)
	require.EqualValues(t, 2, lead.Version)
	require.Contains(t, []uuid.UUID{terminalStages[0].ID, terminalStages[1].ID}, lead.StageID)
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 1, 1, 1)
}

func TestApp_CRMLeadArchiveReopenLifecycleIsAuditedAndIdempotent(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, user.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	pipeline, openStage := createProductCRMTestPipeline(t, app.DB, org.ID, user.ID)
	wonStage := models.CRMPipelineStage{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		PipelineID: pipeline.ID, Name: "Won", Kind: models.CRMPipelineStageKindWon,
		DisplayOrder: 1, Probability: 100, IsActive: true, Version: 1,
		CreatedByID: &user.ID, UpdatedByID: &user.ID,
	}
	require.NoError(t, app.DB.Create(&wonStage).Error)
	wonAt := time.Now().UTC().Truncate(time.Microsecond).Add(-24 * time.Hour)
	lead := createProductCRMTestLead(
		t,
		app.DB,
		org.ID,
		user.ID,
		contact.ID,
		pipeline.ID,
		wonStage.ID,
		models.CRMLeadStatusWon,
	)
	require.NoError(t, app.DB.Model(lead).Update("won_at", wonAt).Error)
	lead.WonAt = &wonAt

	archiveBody := CRMLeadLifecycleRequest{
		Version: 1, Reason: "Duplicate opportunity retained for history",
		IdempotencyKey: "archive-" + uuid.NewString(),
		Metadata:       models.JSONB{"source": "test"},
	}
	archive := newProductCRMLifecycleRequest(t, org.ID, user.ID, lead.ID, archiveBody)
	require.NoError(t, app.ArchiveCRMLead(archive))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(archive))
	var archived CRMLeadResponse
	testutil.ParseEnvelopeResponse(t, archive, &archived)
	require.Equal(t, models.CRMLeadStatusArchived, archived.Status)
	require.EqualValues(t, 2, archived.Version)
	require.NotNil(t, archived.WonAt)
	require.True(t, archived.WonAt.Equal(wonAt))
	require.Nil(t, archived.LostAt)
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 1, 1)

	// An explicit key makes transport retries safe without weakening general
	// optimistic-version conflicts or writing duplicate activity/outbox rows.
	archiveReplay := newProductCRMLifecycleRequest(t, org.ID, user.ID, lead.ID, archiveBody)
	require.NoError(t, app.ArchiveCRMLead(archiveReplay))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(archiveReplay))
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 1, 1)

	moveArchived := newProductCRMMoveRequest(
		t,
		org.ID,
		user.ID,
		lead.ID,
		openStage.ID,
		2,
	)
	require.NoError(t, app.MoveCRMLead(moveArchived))
	testutil.AssertErrorResponse(
		t,
		moveArchived,
		fasthttp.StatusConflict,
		"Archived CRM leads must be reopened before moving stages",
	)
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 1, 1)

	reopenBody := CRMLeadLifecycleRequest{
		Version: 2, Reason: "Customer resumed the opportunity",
		IdempotencyKey: "reopen-" + uuid.NewString(),
	}
	reopen := newProductCRMLifecycleRequest(t, org.ID, user.ID, lead.ID, reopenBody)
	require.NoError(t, app.ReopenCRMLead(reopen))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(reopen))
	var reopened CRMLeadResponse
	testutil.ParseEnvelopeResponse(t, reopen, &reopened)
	require.Equal(t, models.CRMLeadStatusWon, reopened.Status,
		"reopen must derive status from the current won stage")
	require.EqualValues(t, 3, reopened.Version)
	require.NotNil(t, reopened.WonAt)
	require.True(t, reopened.WonAt.Equal(wonAt), "reopen must preserve the historical win timestamp")
	require.Nil(t, reopened.LostAt)
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 2, 2)

	reopenReplay := newProductCRMLifecycleRequest(t, org.ID, user.ID, lead.ID, reopenBody)
	require.NoError(t, app.ReopenCRMLead(reopenReplay))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(reopenReplay))
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 2, 2)

	var lifecycleEvents []models.CustomerActivityEvent
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND lead_id = ? AND event_type = ?",
		org.ID,
		lead.ID,
		models.CustomerActivityCRMLeadUpdated,
	).Order("occurred_at").Find(&lifecycleEvents).Error)
	require.Len(t, lifecycleEvents, 2)
	require.Equal(t, "archive", lifecycleEvents[0].Metadata["action"])
	require.Equal(t, "reopen", lifecycleEvents[1].Metadata["action"])
}

func TestApp_CRMLeadArchiveReopenRejectsStaleWrongTenantAndInvalidState(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	userA := testutil.CreateTestUser(t, app.DB, orgA.ID, testutil.WithSuperAdmin())
	userB := testutil.CreateTestUser(t, app.DB, orgB.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, app.DB, orgA.ID, userA.ID, "crm.enabled")
	contactA := testutil.CreateTestContact(t, app.DB, orgA.ID)
	contactB := testutil.CreateTestContact(t, app.DB, orgB.ID)
	pipelineA, stageA := createProductCRMTestPipeline(t, app.DB, orgA.ID, userA.ID)
	pipelineB, stageB := createProductCRMTestPipeline(t, app.DB, orgB.ID, userB.ID)
	leadA := createProductCRMTestLead(
		t, app.DB, orgA.ID, userA.ID, contactA.ID, pipelineA.ID, stageA.ID,
		models.CRMLeadStatusOpen,
	)
	leadB := createProductCRMTestLead(
		t, app.DB, orgB.ID, userB.ID, contactB.ID, pipelineB.ID, stageB.ID,
		models.CRMLeadStatusOpen,
	)

	stale := newProductCRMLifecycleRequest(t, orgA.ID, userA.ID, leadA.ID, CRMLeadLifecycleRequest{
		Version: 2,
	})
	require.NoError(t, app.ArchiveCRMLead(stale))
	testutil.AssertErrorResponse(
		t,
		stale,
		fasthttp.StatusConflict,
		"CRM lead was modified; refresh and retry",
	)
	assertProductCRMAtomicCounts(t, app.DB, orgA.ID, leadA.ID, 0, 0, 0)

	wrongTenant := newProductCRMLifecycleRequest(t, orgA.ID, userA.ID, leadB.ID, CRMLeadLifecycleRequest{
		Version: 1,
	})
	require.NoError(t, app.ArchiveCRMLead(wrongTenant))
	testutil.AssertErrorResponse(t, wrongTenant, fasthttp.StatusNotFound, "CRM lead not found")
	assertProductCRMAtomicCounts(t, app.DB, orgB.ID, leadB.ID, 0, 0, 0)

	reopenActive := newProductCRMLifecycleRequest(t, orgA.ID, userA.ID, leadA.ID, CRMLeadLifecycleRequest{
		Version: 1,
	})
	require.NoError(t, app.ReopenCRMLead(reopenActive))
	testutil.AssertErrorResponse(
		t,
		reopenActive,
		fasthttp.StatusConflict,
		"Only an archived CRM lead can be reopened",
	)
	assertProductCRMAtomicCounts(t, app.DB, orgA.ID, leadA.ID, 0, 0, 0)

	archive := newProductCRMLifecycleRequest(t, orgA.ID, userA.ID, leadA.ID, CRMLeadLifecycleRequest{
		Version: 1,
	})
	require.NoError(t, app.ArchiveCRMLead(archive))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(archive))
	alreadyArchived := newProductCRMLifecycleRequest(t, orgA.ID, userA.ID, leadA.ID, CRMLeadLifecycleRequest{
		Version: 2,
	})
	require.NoError(t, app.ArchiveCRMLead(alreadyArchived))
	testutil.AssertErrorResponse(
		t,
		alreadyArchived,
		fasthttp.StatusConflict,
		"CRM lead is already archived",
	)
	assertProductCRMAtomicCounts(t, app.DB, orgA.ID, leadA.ID, 0, 1, 1)
}

func TestApp_ListCRMLeadsIncludeArchivedFilter(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	otherOrg := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithSuperAdmin())
	otherUser := testutil.CreateTestUser(t, app.DB, otherOrg.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, user.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	otherContact := testutil.CreateTestContact(t, app.DB, otherOrg.ID)
	pipeline, stage := createProductCRMTestPipeline(t, app.DB, org.ID, user.ID)
	otherPipeline, otherStage := createProductCRMTestPipeline(
		t, app.DB, otherOrg.ID, otherUser.ID,
	)
	for _, status := range []models.CRMLeadStatus{
		models.CRMLeadStatusOpen,
		models.CRMLeadStatusWon,
		models.CRMLeadStatusLost,
		models.CRMLeadStatusArchived,
	} {
		createProductCRMTestLead(
			t, app.DB, org.ID, user.ID, contact.ID, pipeline.ID, stage.ID, status,
		)
	}
	createProductCRMTestLead(
		t,
		app.DB,
		otherOrg.ID,
		otherUser.ID,
		otherContact.ID,
		otherPipeline.ID,
		otherStage.ID,
		models.CRMLeadStatusArchived,
	)

	list := func(includeArchived *string) (*fastglue.Request, []CRMLeadResponse, int64) {
		t.Helper()
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, org.ID, user.ID)
		if includeArchived != nil {
			testutil.SetQueryParam(req, "include_archived", *includeArchived)
		}
		require.NoError(t, app.ListCRMLeads(req))
		if testutil.GetResponseStatusCode(req) != fasthttp.StatusOK {
			return req, nil, 0
		}
		var response struct {
			Leads []CRMLeadResponse `json:"leads"`
			Total int64             `json:"total"`
		}
		testutil.ParseEnvelopeResponse(t, req, &response)
		return req, response.Leads, response.Total
	}

	defaultReq, defaultLeads, defaultTotal := list(nil)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(defaultReq))
	require.Len(t, defaultLeads, 4, "default must remain backward compatible")
	require.EqualValues(t, 4, defaultTotal)

	falseValue := "false"
	filteredReq, filteredLeads, filteredTotal := list(&falseValue)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(filteredReq))
	require.Len(t, filteredLeads, 3)
	require.EqualValues(t, 3, filteredTotal)
	for _, lead := range filteredLeads {
		require.NotEqual(t, models.CRMLeadStatusArchived, lead.Status)
	}

	invalid := "sometimes"
	invalidReq, _, _ := list(&invalid)
	testutil.AssertErrorResponse(
		t,
		invalidReq,
		fasthttp.StatusBadRequest,
		"include_archived must be true or false",
	)
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

func createProductCRMTestLead(
	t *testing.T,
	db *gorm.DB,
	orgID, userID, contactID, pipelineID, stageID uuid.UUID,
	status models.CRMLeadStatus,
) *models.CRMLead {
	t.Helper()
	now := time.Now().UTC()
	lead := &models.CRMLead{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		ContactID: contactID, PipelineID: pipelineID, StageID: stageID,
		Title: "Lifecycle lead " + uuid.NewString()[:8], Status: status,
		Source: models.CRMLeadSourceReferral, Currency: "MYR",
		LastActivityAt: &now, Metadata: models.JSONB{}, Version: 1,
		CreatedByID: &userID, UpdatedByID: &userID,
	}
	require.NoError(t, db.Create(lead).Error)
	return lead
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

func newProductCRMLifecycleRequest(
	t *testing.T,
	orgID, userID, leadID uuid.UUID,
	body CRMLeadLifecycleRequest,
) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, body)
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
