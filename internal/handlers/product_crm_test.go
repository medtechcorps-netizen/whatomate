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
	var historyCount, outboxCount, auditCount, activityCount int64
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
	require.NoError(t, db.Model(&models.CustomerActivityEvent{}).
		Where("organization_id = ? AND lead_id = ?", orgID, leadID).
		Count(&activityCount).Error)
	assert.Equal(t, wantOutbox, activityCount, "each CRM activity must retain its matching outbox event")
}
func TestApp_CRMLeadLifecyclePermissionMatrix(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	owner := createProductCRMTestUserWithLeadPermissions(t, app, org.ID, models.ActionWrite, models.ActionDelete)
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, owner.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	pipeline, stage := createProductCRMTestPipeline(t, app.DB, org.ID, owner.ID)

	// The default frontline role may edit/reopen, but it must not archive.
	defaults := models.SystemRolePermissions()
	require.Contains(t, defaults["agent"], "crm.leads:write")
	require.NotContains(t, defaults["agent"], "crm.leads:delete")
	require.Contains(t, defaults["manager"], "crm.leads:delete")

	for _, permissions := range []struct {
		name    string
		actions []string
	}{
		{name: "none"},
		{name: "read", actions: []string{models.ActionRead}},
		{name: "write", actions: []string{models.ActionWrite}},
		{name: "delete", actions: []string{models.ActionDelete}},
		{name: "read_write", actions: []string{models.ActionRead, models.ActionWrite}},
		{name: "read_delete", actions: []string{models.ActionRead, models.ActionDelete}},
		{name: "write_delete", actions: []string{models.ActionWrite, models.ActionDelete}},
		{name: "read_write_delete", actions: []string{models.ActionRead, models.ActionWrite, models.ActionDelete}},
	} {
		t.Run(permissions.name, func(t *testing.T) {
			user := createProductCRMTestUserWithLeadPermissions(t, app, org.ID, permissions.actions...)
			for _, transition := range []struct {
				name       string
				permission string
				initial    models.CRMLeadStatus
				target     models.CRMLeadStatus
				call       func(*fastglue.Request) error
			}{
				{"archive", models.ActionDelete, models.CRMLeadStatusOpen, models.CRMLeadStatusArchived, app.ArchiveCRMLead},
				{"reopen", models.ActionWrite, models.CRMLeadStatusArchived, models.CRMLeadStatusOpen, app.ReopenCRMLead},
			} {
				t.Run(transition.name, func(t *testing.T) {
					lead := createProductCRMTestLead(t, app.DB, org.ID, owner.ID, contact.ID, pipeline.ID, stage.ID, transition.initial)
					var before models.CRMLead
					require.NoError(t, app.DB.First(&before, "id = ?", lead.ID).Error)
					req := newProductCRMLifecycleRequest(t, org.ID, user.ID, lead.ID, CRMLeadLifecycleRequest{
						Version: 1, IdempotencyKey: uuid.NewString(),
					})
					require.NoError(t, transition.call(req))
					allowed := false
					for _, action := range permissions.actions {
						allowed = allowed || action == transition.permission
					}
					if !allowed {
						testutil.AssertErrorResponse(t, req, fasthttp.StatusForbidden, "Insufficient permissions")
						assertProductCRMLeadUnchanged(t, app.DB, &before)
						assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 0, 0)
						return
					}
					require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
					var response CRMLeadResponse
					testutil.ParseEnvelopeResponse(t, req, &response)
					require.Equal(t, transition.target, response.Status)
					require.EqualValues(t, 2, response.Version)
					assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 1, 1)
				})
			}
		})
	}
}

func TestApp_CRMLeadLifecycleAuthorizedRejectionsDoNotMutate(t *testing.T) {
	for _, action := range []string{"archive", "reopen"} {
		for _, rejection := range []string{"unlicensed", "wrong_tenant", "stale_version"} {
			t.Run(action+"/"+rejection, func(t *testing.T) {
				app := newProductCRMDatabaseTestApp(t)
				org := testutil.CreateTestOrganization(t, app.DB)
				permission := models.ActionDelete
				initial := models.CRMLeadStatusOpen
				call := app.ArchiveCRMLead
				if action == "reopen" {
					permission = models.ActionWrite
					initial = models.CRMLeadStatusArchived
					call = app.ReopenCRMLead
				}
				user := createProductCRMTestUserWithLeadPermissions(t, app, org.ID, permission)
				if rejection != "unlicensed" {
					enableBookingCommerceTestEntitlement(t, app.DB, org.ID, user.ID, "crm.enabled")
				}
				leadOrg, leadOwner := org, user
				if rejection == "wrong_tenant" {
					leadOrg = testutil.CreateTestOrganization(t, app.DB)
					leadOwner = createProductCRMTestUserWithLeadPermissions(t, app, leadOrg.ID, permission)
				}
				contact := testutil.CreateTestContact(t, app.DB, leadOrg.ID)
				pipeline, stage := createProductCRMTestPipeline(t, app.DB, leadOrg.ID, leadOwner.ID)
				lead := createProductCRMTestLead(t, app.DB, leadOrg.ID, leadOwner.ID, contact.ID, pipeline.ID, stage.ID, initial)
				var before models.CRMLead
				require.NoError(t, app.DB.First(&before, "id = ?", lead.ID).Error)
				body := CRMLeadLifecycleRequest{Version: 1, IdempotencyKey: uuid.NewString()}
				wantStatus := fasthttp.StatusPaymentRequired
				wantMessage := "Feature is not included in the organization's active plan"
				switch rejection {
				case "wrong_tenant":
					wantStatus, wantMessage = fasthttp.StatusNotFound, "CRM lead not found"
				case "stale_version":
					body.Version = 2
					wantStatus, wantMessage = fasthttp.StatusConflict, "CRM lead was modified; refresh and retry"
				}
				req := newProductCRMLifecycleRequest(t, org.ID, user.ID, lead.ID, body)
				require.NoError(t, call(req))
				testutil.AssertErrorResponse(t, req, wantStatus, wantMessage)
				assertProductCRMLeadUnchanged(t, app.DB, &before)
				assertProductCRMAtomicCounts(t, app.DB, leadOrg.ID, lead.ID, 0, 0, 0)
			})
		}
	}
}

func TestApp_CRMLeadLifecycleReplayRequiresCurrentPermission(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	archiver := createProductCRMTestUserWithLeadPermissions(t, app, org.ID, models.ActionDelete)
	writer := createProductCRMTestUserWithLeadPermissions(t, app, org.ID, models.ActionWrite)
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, archiver.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	pipeline, stage := createProductCRMTestPipeline(t, app.DB, org.ID, archiver.ID)
	lead := createProductCRMTestLead(t, app.DB, org.ID, archiver.ID, contact.ID, pipeline.ID, stage.ID, models.CRMLeadStatusOpen)
	archiveBody := CRMLeadLifecycleRequest{Version: 1, IdempotencyKey: uuid.NewString(), Reason: "Retain duplicate history"}
	archive := newProductCRMLifecycleRequest(t, org.ID, archiver.ID, lead.ID, archiveBody)
	require.NoError(t, app.ArchiveCRMLead(archive))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(archive))
	var archived models.CRMLead
	require.NoError(t, app.DB.First(&archived, "id = ?", lead.ID).Error)

	var deletePermission models.Permission
	require.NoError(t, app.DB.Where("resource = ? AND action = ?", models.ResourceCRMLeads, models.ActionDelete).First(&deletePermission).Error)
	grant := models.RolePermission{CustomRoleID: *archiver.RoleID, PermissionID: deletePermission.ID}
	require.NoError(t, app.DB.Where("custom_role_id = ? AND permission_id = ?", grant.CustomRoleID, grant.PermissionID).
		Delete(&models.RolePermission{}).Error)
	replayWithoutPermission := newProductCRMLifecycleRequest(t, org.ID, archiver.ID, lead.ID, archiveBody)
	require.NoError(t, app.ArchiveCRMLead(replayWithoutPermission))
	testutil.AssertErrorResponse(t, replayWithoutPermission, fasthttp.StatusForbidden, "Insufficient permissions")
	assertProductCRMLeadUnchanged(t, app.DB, &archived)
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 1, 1)

	require.NoError(t, app.DB.Create(&grant).Error)
	replay := newProductCRMLifecycleRequest(t, org.ID, archiver.ID, lead.ID, archiveBody)
	require.NoError(t, app.ArchiveCRMLead(replay))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(replay))
	assertProductCRMLeadUnchanged(t, app.DB, &archived)
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 1, 1)

	changedBody := archiveBody
	changedBody.Reason = "A different request must not reuse the successful key"
	conflictingReplay := newProductCRMLifecycleRequest(t, org.ID, archiver.ID, lead.ID, changedBody)
	require.NoError(t, app.ArchiveCRMLead(conflictingReplay))
	require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(conflictingReplay))
	assertProductCRMLeadUnchanged(t, app.DB, &archived)
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 1, 1)

	reopenBody := CRMLeadLifecycleRequest{Version: 2, IdempotencyKey: uuid.NewString()}
	deleteOnlyReopen := newProductCRMLifecycleRequest(t, org.ID, archiver.ID, lead.ID, reopenBody)
	require.NoError(t, app.ReopenCRMLead(deleteOnlyReopen))
	testutil.AssertErrorResponse(t, deleteOnlyReopen, fasthttp.StatusForbidden, "Insufficient permissions")
	assertProductCRMLeadUnchanged(t, app.DB, &archived)
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 1, 1)

	reopen := newProductCRMLifecycleRequest(t, org.ID, writer.ID, lead.ID, reopenBody)
	require.NoError(t, app.ReopenCRMLead(reopen))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(reopen))
	var reopened CRMLeadResponse
	testutil.ParseEnvelopeResponse(t, reopen, &reopened)
	require.Equal(t, models.CRMLeadStatusOpen, reopened.Status)
	require.EqualValues(t, 3, reopened.Version)
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 2, 2)
}

func TestApp_CRMLeadGenericUpdateCannotBypassLifecyclePermissions(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	writer := createProductCRMTestUserWithLeadPermissions(t, app, org.ID, models.ActionWrite)
	archiver := createProductCRMTestUserWithLeadPermissions(t, app, org.ID, models.ActionDelete)
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, writer.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	pipeline, stage := createProductCRMTestPipeline(t, app.DB, org.ID, writer.ID)
	lead := createProductCRMTestLead(t, app.DB, org.ID, writer.ID, contact.ID, pipeline.ID, stage.ID, models.CRMLeadStatusOpen)
	update := func(userID uuid.UUID, body map[string]any) *fastglue.Request {
		req := testutil.NewJSONRequest(t, body)
		testutil.SetAuthContext(req, org.ID, userID)
		testutil.SetPathParam(req, "id", lead.ID.String())
		require.NoError(t, app.UpdateCRMLead(req))
		return req
	}

	// Unknown lifecycle fields cannot become ordinary editable lead fields.
	injectedArchive := update(writer.ID, map[string]any{
		"version": 1, "title": "Ordinary edit", "status": "archived",
	})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(injectedArchive))
	var edited models.CRMLead
	require.NoError(t, app.DB.First(&edited, "id = ?", lead.ID).Error)
	require.Equal(t, "Ordinary edit", edited.Title)
	require.Equal(t, models.CRMLeadStatusOpen, edited.Status)
	require.EqualValues(t, 2, edited.Version)
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 1, 1)

	statusOnly := update(writer.ID, map[string]any{"version": 2, "status": "archived"})
	testutil.AssertErrorResponse(t, statusOnly, fasthttp.StatusBadRequest, "at least one lead field must be supplied")
	deleteOnlyEdit := update(archiver.ID, map[string]any{"version": 2, "title": "Delete is not write"})
	testutil.AssertErrorResponse(t, deleteOnlyEdit, fasthttp.StatusForbidden, "Insufficient permissions")
	assertProductCRMLeadUnchanged(t, app.DB, &edited)
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 1, 1)

	archive := newProductCRMLifecycleRequest(t, org.ID, archiver.ID, lead.ID, CRMLeadLifecycleRequest{Version: 2})
	require.NoError(t, app.ArchiveCRMLead(archive))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(archive))
	var archived models.CRMLead
	require.NoError(t, app.DB.First(&archived, "id = ?", lead.ID).Error)
	injectedReopen := update(writer.ID, map[string]any{
		"version": 3, "title": "Bypass reopen", "status": "open",
	})
	testutil.AssertErrorResponse(t, injectedReopen, fasthttp.StatusConflict, "Archived CRM leads must be reopened before editing")
	moveArchived := newProductCRMMoveRequest(t, org.ID, writer.ID, lead.ID, stage.ID, 3)
	require.NoError(t, app.MoveCRMLead(moveArchived))
	testutil.AssertErrorResponse(t, moveArchived, fasthttp.StatusConflict, "Archived CRM leads must be reopened before moving stages")
	assertProductCRMLeadUnchanged(t, app.DB, &archived)
	assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 0, 2, 2)
}

func createProductCRMTestUserWithLeadPermissions(
	t *testing.T,
	app *App,
	orgID uuid.UUID,
	actions ...string,
) *models.User {
	t.Helper()
	var permissions []models.Permission
	for _, action := range actions {
		permission := models.Permission{Resource: models.ResourceCRMLeads, Action: action}
		require.NoError(t, app.DB.Where("resource = ? AND action = ?", permission.Resource, permission.Action).
			FirstOrCreate(&permission).Error)
		permissions = append(permissions, permission)
	}
	role := testutil.CreateTestRole(t, app.DB, orgID, "CRM lifecycle", permissions)
	user := testutil.CreateTestUser(t, app.DB, orgID, testutil.WithRoleID(&role.ID))
	require.False(t, user.IsSuperAdmin, "permission regressions must exercise the real role boundary")
	return user
}

func assertProductCRMLeadUnchanged(t *testing.T, db *gorm.DB, before *models.CRMLead) {
	t.Helper()
	var after models.CRMLead
	require.NoError(t, db.Unscoped().Where("id = ? AND organization_id = ?", before.ID, before.OrganizationID).
		First(&after).Error)
	require.Equal(t, *before, after, "a rejected or replayed request must not mutate any lead field")
}

func TestApp_CRMLeadArchiveReopenPreservesLinkedRecords(t *testing.T) {
	app := newProductCRMDatabaseTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := createProductCRMTestUserWithLeadPermissions(t, app, org.ID, models.ActionWrite, models.ActionDelete)
	enableBookingCommerceTestEntitlement(t, app.DB, org.ID, user.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	pipeline, stage := createProductCRMTestPipeline(t, app.DB, org.ID, user.ID)
	lead := createProductCRMTestLead(t, app.DB, org.ID, user.ID, contact.ID, pipeline.ID, stage.ID, models.CRMLeadStatusOpen)
	message := &models.Message{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		ContactID: contact.ID, WhatsAppAccount: account.Name,
		WhatsAppMessageID: "wamid." + uuid.NewString(),
		Direction:         models.DirectionIncoming, MessageType: models.MessageTypeText,
		Content: "Existing customer conversation", Metadata: models.JSONB{},
	}
	task := &models.FollowUpTask{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		ContactID: &contact.ID, LeadID: &lead.ID, Title: "Existing follow-up",
		Status: models.FollowUpTaskStatusOpen, Priority: models.FollowUpTaskPriorityNormal,
		CreatedByID: &user.ID, Version: 1, Metadata: models.JSONB{},
	}
	history := &models.CRMStageHistory{
		ID: uuid.New(), OrganizationID: org.ID, LeadID: lead.ID, ToStageID: stage.ID,
		ChangedByID: &user.ID, Reason: "Original stage assignment", Metadata: models.JSONB{},
	}
	invoice := &models.CommerceInvoice{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, ContactID: contact.ID,
		InvoiceNumber: "CRM-" + uuid.NewString(), IdempotencyKey: uuid.NewString(),
		Status: models.CommerceInvoiceStatusPaid, Currency: "MYR",
		SubtotalMinor: 10000, TotalMinor: 10000, PaidMinor: 10000,
		Version: 1, CreatedByID: &user.ID, Metadata: models.JSONB{},
	}
	for _, record := range []any{message, task, history, invoice} {
		require.NoError(t, app.DB.Create(record).Error)
	}
	records := []struct {
		id     uuid.UUID
		before any
		after  any
	}{
		{contact.ID, contact, &models.Contact{}},
		{message.ID, message, &models.Message{}},
		{task.ID, task, &models.FollowUpTask{}},
		{history.ID, history, &models.CRMStageHistory{}},
		{invoice.ID, invoice, &models.CommerceInvoice{}},
	}
	for _, record := range records {
		require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", record.id, org.ID).First(record.before).Error)
	}
	for _, transition := range []struct {
		version int64
		status  models.CRMLeadStatus
		call    func(*fastglue.Request) error
	}{
		{1, models.CRMLeadStatusArchived, app.ArchiveCRMLead},
		{2, models.CRMLeadStatusOpen, app.ReopenCRMLead},
	} {
		req := newProductCRMLifecycleRequest(t, org.ID, user.ID, lead.ID, CRMLeadLifecycleRequest{
			Version: transition.version, IdempotencyKey: uuid.NewString(),
		})
		require.NoError(t, transition.call(req))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
		var response CRMLeadResponse
		testutil.ParseEnvelopeResponse(t, req, &response)
		require.Equal(t, transition.status, response.Status)
		require.Equal(t, contact.ID, response.ContactID)
		require.Equal(t, pipeline.ID, response.PipelineID)
		require.Equal(t, stage.ID, response.StageID)
		require.Equal(t, lead.ValueMinor, response.ValueMinor)
		require.Equal(t, lead.Currency, response.Currency)
		for _, record := range records {
			require.NoError(t, app.DB.Unscoped().Where("id = ? AND organization_id = ?", record.id, org.ID).First(record.after).Error)
			require.Equal(t, record.before, record.after, "Archive/Reopen must preserve linked record %s", record.id)
		}
		assertProductCRMAtomicCounts(t, app.DB, org.ID, lead.ID, 1, transition.version, transition.version)
	}
}

func TestProductCRMDestructiveRoleDefaults(t *testing.T) {
	t.Parallel()
	defaults := models.SystemRolePermissions()
	require.Contains(t, defaults["manager"], "crm.leads:delete")
	require.Contains(t, defaults["manager"], "packages:delete")
	require.NotContains(t, defaults["manager"], "crm.pipelines:delete", "whole-pipeline retirement is outside this change")
	for _, permission := range []string{"crm.leads:delete", "crm.pipelines:delete", "packages:delete"} {
		require.NotContains(t, defaults["agent"], permission, "frontline defaults must not acquire destructive authority")
	}
}
