package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func newCareContinuityTestProcessor(
	t *testing.T,
) (*App, *CareContinuityProcessor, time.Time) {
	t.Helper()
	app := &App{
		DB:         testutil.SetupTestDB(t),
		Log:        testutil.NopLogger(),
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	}
	now := time.Now().UTC().Add(time.Second)
	processor := NewCareContinuityProcessor(app, time.Hour)
	processor.now = func() time.Time { return now }
	processor.sweepInterval = 24 * time.Hour
	processor.lastSweep = now
	processor.deliverWebhooks = func(
		context.Context,
		uuid.UUID,
		string,
		map[string]any,
	) error {
		return nil
	}
	t.Cleanup(processor.Stop)
	return app, processor, now
}

func enableCareContinuityEntitlements(
	t *testing.T,
	db *gorm.DB,
	organizationID, userID uuid.UUID,
	keys ...string,
) {
	t.Helper()
	snapshot := models.JSONB{}
	for _, key := range keys {
		snapshot[key] = true
	}
	plan := models.Plan{
		BaseModel:   models.BaseModel{ID: uuid.New()},
		ScopeKey:    "care-test-" + uuid.NewString(),
		Code:        "care-test-" + uuid.NewString(),
		Name:        "Care continuity test plan",
		Status:      models.CommercialPlanStatusActive,
		Vertical:    "general",
		IsPublic:    false,
		Metadata:    models.JSONB{},
		CreatedByID: &userID,
	}
	require.NoError(t, db.Create(&plan).Error)
	account := models.BillingAccount{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organizationID,
		Provider:        models.BillingProviderManual,
		Status:          models.BillingAccountStatusActive,
		DefaultCurrency: "MYR",
		BillingProfile:  models.JSONB{},
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	periodStart := time.Now().UTC().Add(-time.Hour)
	periodEnd := periodStart.AddDate(0, 1, 0)
	subscription := models.Subscription{
		BaseModel:            models.BaseModel{ID: uuid.New()},
		OrganizationID:       organizationID,
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
	require.NoError(t, db.Create(&subscription).Error)
}

func createCareContinuityBooking(
	t *testing.T,
	db *gorm.DB,
	organizationID, contactID uuid.UUID,
	status models.BookingStatus,
	startsAt time.Time,
) *models.Booking {
	t.Helper()
	service := models.BookingService{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organizationID,
		Name:            "Care service " + uuid.NewString()[:8],
		Kind:            models.BookingServiceKindAppointment,
		DurationMinutes: 30,
		DefaultCapacity: 1,
		Currency:        "MYR",
		IsActive:        true,
		ReminderPolicy:  models.JSONB{},
		Metadata:        models.JSONB{},
		Version:         1,
	}
	require.NoError(t, db.Create(&service).Error)
	resource := models.BookingResource{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		Name:           "Care room " + uuid.NewString()[:8],
		Kind:           models.BookingResourceKindRoom,
		Timezone:       "Asia/Kuala_Lumpur",
		IsActive:       true,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, db.Create(&resource).Error)
	event := models.BookingEvent{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
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
		OrganizationID: organizationID,
		EventID:        event.ID,
		ContactID:      contactID,
		Status:         status,
		Quantity:       1,
		Source:         models.BookingSourceAgent,
		IdempotencyKey: "care-booking-" + uuid.NewString(),
		Metadata:       models.JSONB{},
		Version:        1,
	}
	switch status {
	case models.BookingStatusCompleted:
		completedAt := startsAt.Add(30 * time.Minute)
		booking.CompletedAt = &completedAt
	case models.BookingStatusNoShow:
		noShowAt := startsAt.Add(30 * time.Minute)
		booking.NoShowAt = &noShowAt
	case models.BookingStatusCancelled:
		cancelledAt := startsAt.Add(-time.Hour)
		booking.CancelledAt = &cancelledAt
	}
	require.NoError(t, db.Create(&booking).Error)
	return &booking
}

func createCareContinuityActivity(
	t *testing.T,
	db *gorm.DB,
	organizationID, contactID uuid.UUID,
	eventType models.CustomerActivityEventType,
	category models.CustomerActivityCategory,
	sourceType string,
	sourceID *uuid.UUID,
) (*models.CustomerActivityEvent, *models.OutboxEvent) {
	t.Helper()
	var activity *models.CustomerActivityEvent
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		activity, err = recordCustomerActivity(tx, organizationID, customerActivityInput{
			ContactID:        contactID,
			EventType:        eventType,
			Category:         category,
			Title:            "Care continuity test event",
			ActorType:        models.CustomerActivityActorSystem,
			SourceObjectType: sourceType,
			SourceObjectID:   sourceID,
			OccurredAt:       time.Now().UTC(),
			Metadata:         models.JSONB{},
			IdempotencyKey:   "care-activity-" + uuid.NewString(),
		})
		return err
	}))
	var outbox models.OutboxEvent
	require.NoError(t, db.Where(
		"organization_id = ? AND idempotency_key = ?",
		organizationID,
		"customer-activity-webhook:"+activity.ID.String(),
	).First(&outbox).Error)
	return activity, &outbox
}

func TestCareContinuityProcessor_ClaimsAndDeliversMessageIncomingLegacyPayload(
	t *testing.T,
) {
	app, processor, now := newCareContinuityTestProcessor(t)
	organization := testutil.CreateTestOrganization(t, app.DB)
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)
	messageID := uuid.New()
	untrustedID := uuid.NewString()

	var activity *models.CustomerActivityEvent
	require.NoError(t, app.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		activity, err = recordCustomerActivity(tx, organization.ID, customerActivityInput{
			ContactID:        contact.ID,
			EventType:        models.CustomerActivityMessageIncoming,
			Category:         models.CustomerActivityCategoryMessage,
			Title:            "Incoming WhatsApp message",
			Summary:          "Can I book tomorrow?",
			ActorType:        models.CustomerActivityActorContact,
			SourceObjectType: "message",
			SourceObjectID:   &messageID,
			OccurredAt:       now,
			Metadata:         models.JSONB{"message_status": "received"},
			WebhookData: models.JSONB{
				"message_id":        messageID.String(),
				"contact_phone":     "+60123456789",
				"contact_name":      "Customer",
				"message_type":      "text",
				"content":           "Can I book tomorrow?",
				"whatsapp_account":  "support",
				"direction":         "incoming",
				"activity_event_id": untrustedID,
				"contact_id":        untrustedID,
				"event_type":        string(models.CustomerActivityContactUpdated),
				"category":          string(models.CustomerActivityCategoryContact),
				"title":             "Untrusted title",
				"source_type":       "contact",
				"source_id":         untrustedID,
				"outbox_event_id":   untrustedID,
			},
			IdempotencyKey: "message-incoming:" + messageID.String(),
		})
		return err
	}))

	var outbox models.OutboxEvent
	require.NoError(t, app.DB.Where(
		"organization_id = ? AND idempotency_key = ?",
		organization.ID,
		"customer-activity-webhook:"+activity.ID.String(),
	).First(&outbox).Error)
	require.Equal(t, messageID.String(), outbox.Payload["message_id"])
	require.Equal(t, "+60123456789", outbox.Payload["contact_phone"])
	require.Equal(t, activity.ID.String(), outbox.Payload["activity_event_id"])
	require.Equal(t, contact.ID.String(), outbox.Payload["contact_id"])
	require.Equal(t, string(models.CustomerActivityMessageIncoming), outbox.Payload["event_type"])
	require.Equal(t, string(models.CustomerActivityCategoryMessage), outbox.Payload["category"])
	require.Equal(t, "Incoming WhatsApp message", outbox.Payload["title"])
	require.Equal(t, "message", outbox.Payload["source_type"])
	require.Equal(t, messageID.String(), outbox.Payload["source_id"])
	require.NotContains(t, outbox.Payload, "outbox_event_id")

	var deliveredOrganizationID uuid.UUID
	var deliveredEventType string
	var deliveredData map[string]any
	processor.deliverWebhooks = func(
		_ context.Context,
		organizationID uuid.UUID,
		eventType string,
		data map[string]any,
	) error {
		deliveredOrganizationID = organizationID
		deliveredEventType = eventType
		deliveredData = make(map[string]any, len(data))
		for key, value := range data {
			deliveredData[key] = value
		}
		return nil
	}

	claimed, err := processor.claimOutboxEvent(context.Background(), organization.ID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, outbox.ID, claimed.ID)
	require.NoError(
		t,
		processor.processOutboxEvent(context.Background(), organization.ID, claimed),
	)

	require.Equal(t, organization.ID, deliveredOrganizationID)
	require.Equal(t, string(models.WebhookEventMessageIncoming), deliveredEventType)
	require.Equal(t, messageID.String(), deliveredData["message_id"])
	require.Equal(t, "+60123456789", deliveredData["contact_phone"])
	require.Equal(t, "Customer", deliveredData["contact_name"])
	require.Equal(t, "text", deliveredData["message_type"])
	require.Equal(t, "Can I book tomorrow?", deliveredData["content"])
	require.Equal(t, "support", deliveredData["whatsapp_account"])
	require.Equal(t, "incoming", deliveredData["direction"])
	require.Equal(t, activity.ID.String(), deliveredData["activity_event_id"])
	require.Equal(t, outbox.ID.String(), deliveredData["outbox_event_id"])
	require.Equal(t, contact.ID.String(), deliveredData["contact_id"])
	require.Equal(t, string(models.CustomerActivityMessageIncoming), deliveredData["event_type"])
	require.Equal(t, string(models.CustomerActivityCategoryMessage), deliveredData["category"])
	require.Equal(t, "Incoming WhatsApp message", deliveredData["title"])
	require.Equal(t, "message", deliveredData["source_type"])
	require.Equal(t, messageID.String(), deliveredData["source_id"])

	var stored models.OutboxEvent
	require.NoError(t, app.DB.First(&stored, "id = ?", outbox.ID).Error)
	require.Equal(t, models.OutboxEventStatusPublished, stored.Status)
	require.Equal(t, 1, stored.Attempts)
}

func TestCareContinuityProcessor_MultiTenantReplayIsIdempotentAndNeverMessagesCustomers(
	t *testing.T,
) {
	app, processor, now := newCareContinuityTestProcessor(t)
	organizationA := testutil.CreateTestOrganization(t, app.DB)
	organizationB := testutil.CreateTestOrganization(t, app.DB)
	userA := testutil.CreateTestUser(t, app.DB, organizationA.ID)
	userB := testutil.CreateTestUser(t, app.DB, organizationB.ID)
	enableCareContinuityEntitlements(
		t,
		app.DB,
		organizationA.ID,
		userA.ID,
		"crm.enabled",
		"bookings.enabled",
	)
	enableCareContinuityEntitlements(
		t,
		app.DB,
		organizationB.ID,
		userB.ID,
		"crm.enabled",
		"bookings.enabled",
	)
	contactA := testutil.CreateTestContact(t, app.DB, organizationA.ID)
	contactB := testutil.CreateTestContact(t, app.DB, organizationB.ID)
	bookingA := createCareContinuityBooking(
		t,
		app.DB,
		organizationA.ID,
		contactA.ID,
		models.BookingStatusCompleted,
		now.Add(-24*time.Hour),
	)
	bookingB := createCareContinuityBooking(
		t,
		app.DB,
		organizationB.ID,
		contactB.ID,
		models.BookingStatusNoShow,
		now.Add(-24*time.Hour),
	)
	_, outboxA := createCareContinuityActivity(
		t,
		app.DB,
		organizationA.ID,
		contactA.ID,
		models.CustomerActivityBookingStatus,
		models.CustomerActivityCategoryBooking,
		"booking",
		&bookingA.ID,
	)
	_, outboxB := createCareContinuityActivity(
		t,
		app.DB,
		organizationB.ID,
		contactB.ID,
		models.CustomerActivityBookingStatus,
		models.CustomerActivityCategoryBooking,
		"booking",
		&bookingB.ID,
	)

	require.NoError(t, processor.RunOnce(context.Background()))
	require.NoError(t, processor.RunOnce(context.Background()))

	var tasks []models.FollowUpTask
	require.NoError(t, app.DB.Where("source = ?", careTaskSource).Order("organization_id").Find(&tasks).Error)
	require.Len(t, tasks, 2)
	for _, task := range tasks {
		switch task.OrganizationID {
		case organizationA.ID:
			require.NotNil(t, task.ContactID)
			assert.Equal(t, contactA.ID, *task.ContactID)
			assert.Equal(t, &bookingA.ID, task.BookingID)
		case organizationB.ID:
			require.NotNil(t, task.ContactID)
			assert.Equal(t, contactB.ID, *task.ContactID)
			assert.Equal(t, &bookingB.ID, task.BookingID)
		default:
			t.Fatalf("unexpected task tenant %s", task.OrganizationID)
		}
	}

	for _, eventID := range []uuid.UUID{outboxA.ID, outboxB.ID} {
		var outbox models.OutboxEvent
		require.NoError(t, app.DB.First(&outbox, "id = ?", eventID).Error)
		assert.Equal(t, models.OutboxEventStatusPublished, outbox.Status)
		assert.Equal(t, 1, outbox.Attempts)
	}

	var messageCount int64
	var customerOutboxCount int64
	organizationIDs := []uuid.UUID{organizationA.ID, organizationB.ID}
	require.NoError(t, app.DB.Model(&models.Message{}).
		Where("organization_id IN ?", organizationIDs).
		Count(&messageCount).Error)
	require.NoError(t, app.DB.Model(&models.OutboxJob{}).
		Where("organization_id IN ?", organizationIDs).
		Count(&customerOutboxCount).Error)
	assert.Zero(t, messageCount)
	assert.Zero(t, customerOutboxCount)
}

func TestCareContinuityProcessor_ReclaimsStaleReminderLeaseAndTargetsOwner(t *testing.T) {
	app, processor, now := newCareContinuityTestProcessor(t)
	organization := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(t, app.DB, organization.ID)
	enableCareContinuityEntitlements(
		t,
		app.DB,
		organization.ID,
		owner.ID,
		"crm.enabled",
	)
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)
	remindAt := now.Add(-time.Minute)
	dueAt := now.Add(time.Hour)
	task := models.FollowUpTask{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		ContactID:      &contact.ID,
		Title:          "Owned reminder",
		Status:         models.FollowUpTaskStatusOpen,
		Priority:       models.FollowUpTaskPriorityNormal,
		OwnerUserID:    &owner.ID,
		DueAt:          &dueAt,
		RemindAt:       &remindAt,
		Source:         "manual",
		IdempotencyKey: "care-reminder-task-" + uuid.NewString(),
		Metadata:       models.JSONB{},
		Version:        3,
	}
	require.NoError(t, app.DB.Create(&task).Error)
	staleLock := now.Add(-processor.lease - time.Minute)
	job := models.ScheduledJob{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Kind:           careTaskReminderKind,
		AggregateType:  "follow_up_task",
		AggregateID:    &task.ID,
		RunAt:          remindAt,
		Status:         models.ScheduledJobStatusProcessing,
		Attempts:       0,
		MaxAttempts:    5,
		IdempotencyKey: "care-reminder-job-" + uuid.NewString(),
		Payload: models.JSONB{
			"task_id":      task.ID.String(),
			"task_version": task.Version,
		},
		LockedAt: &staleLock,
		LockedBy: "dead-worker",
		Version:  1,
	}
	require.NoError(t, app.DB.Create(&job).Error)

	var notifications atomic.Int32
	processor.notifyReminder = func(notification careReminderNotification) error {
		notifications.Add(1)
		require.NotNil(t, notification.OwnerUserID)
		assert.Equal(t, owner.ID, *notification.OwnerUserID)
		assert.Equal(t, organization.ID, notification.OrganizationID)
		assert.Equal(t, task.ID, notification.TaskID)
		return nil
	}

	require.NoError(t, processor.RunOnce(context.Background()))
	processor.Stop()
	processor.Stop()

	var stored models.ScheduledJob
	require.NoError(t, app.DB.First(&stored, "id = ?", job.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusCompleted, stored.Status)
	assert.Equal(t, 1, stored.Attempts)
	assert.Empty(t, stored.LockedBy)
	assert.Equal(t, int32(1), notifications.Load())
}

func TestCareContinuityProcessor_ReclaimsStaleOutboxLease(t *testing.T) {
	app, processor, now := newCareContinuityTestProcessor(t)
	organization := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, organization.ID)
	enableCareContinuityEntitlements(t, app.DB, organization.ID, user.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)
	_, outbox := createCareContinuityActivity(
		t,
		app.DB,
		organization.ID,
		contact.ID,
		models.CustomerActivityContactUpdated,
		models.CustomerActivityCategoryContact,
		"contact",
		&contact.ID,
	)
	staleLock := now.Add(-processor.lease - time.Minute)
	require.NoError(t, app.DB.Model(&models.OutboxEvent{}).
		Where("id = ?", outbox.ID).
		Updates(map[string]any{
			"status":    models.OutboxEventStatusProcessing,
			"locked_at": staleLock,
			"locked_by": "dead-worker",
			"attempts":  0,
		}).Error)

	require.NoError(t, processor.RunOnce(context.Background()))
	var stored models.OutboxEvent
	require.NoError(t, app.DB.First(&stored, "id = ?", outbox.ID).Error)
	assert.Equal(t, models.OutboxEventStatusPublished, stored.Status)
	assert.Equal(t, 1, stored.Attempts)
	assert.Empty(t, stored.LockedBy)
}

func TestCareContinuityProcessor_ConcurrentWorkersDoNotDoubleClaim(t *testing.T) {
	app, first, now := newCareContinuityTestProcessor(t)
	organization := testutil.CreateTestOrganization(t, app.DB)
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)
	_, outbox := createCareContinuityActivity(
		t,
		app.DB,
		organization.ID,
		contact.ID,
		models.CustomerActivityContactUpdated,
		models.CustomerActivityCategoryContact,
		"contact",
		&contact.ID,
	)

	second := NewCareContinuityProcessor(app, time.Hour)
	second.now = func() time.Time { return now }
	second.sweepInterval = 24 * time.Hour
	second.lastSweep = now
	t.Cleanup(second.Stop)

	started := make(chan struct{})
	release := make(chan struct{})
	var deliveries atomic.Int32
	first.deliverWebhooks = func(
		context.Context,
		uuid.UUID,
		string,
		map[string]any,
	) error {
		deliveries.Add(1)
		close(started)
		<-release
		return nil
	}
	second.deliverWebhooks = func(
		context.Context,
		uuid.UUID,
		string,
		map[string]any,
	) error {
		t.Error("second worker delivered an event with a fresh lease")
		return nil
	}

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- first.RunOnce(context.Background())
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first worker did not reach synchronous webhook delivery")
	}

	require.NoError(t, second.RunOnce(context.Background()))
	close(release)
	require.NoError(t, <-firstResult)

	var stored models.OutboxEvent
	require.NoError(t, app.DB.First(&stored, "id = ?", outbox.ID).Error)
	assert.Equal(t, models.OutboxEventStatusPublished, stored.Status)
	assert.Equal(t, 1, stored.Attempts)
	assert.Equal(t, int32(1), deliveries.Load())
}

func TestCareContinuityProcessor_WebhookFailureHonorsMaxAttempts(t *testing.T) {
	app, processor, _ := newCareContinuityTestProcessor(t)
	organization := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, organization.ID)
	enableCareContinuityEntitlements(t, app.DB, organization.ID, user.ID, "crm.enabled")
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)

	var deliveries atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deliveries.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	app.HTTPClient = server.Client()
	webhook := models.Webhook{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Name:           "Failing care webhook",
		URL:            server.URL,
		Events:         models.StringArray{string(models.WebhookEventContactUpdated)},
		Headers:        models.JSONB{},
		IsActive:       true,
	}
	require.NoError(t, app.DB.Create(&webhook).Error)
	_, outbox := createCareContinuityActivity(
		t,
		app.DB,
		organization.ID,
		contact.ID,
		models.CustomerActivityContactUpdated,
		models.CustomerActivityCategoryContact,
		"contact",
		&contact.ID,
	)
	require.NoError(t, app.DB.Model(&models.OutboxEvent{}).
		Where("id = ?", outbox.ID).
		Update("max_attempts", 1).Error)
	processor.deliverWebhooks = processor.deliverConfiguredWebhooks

	err := processor.RunOnce(context.Background())
	require.Error(t, err)

	var stored models.OutboxEvent
	require.NoError(t, app.DB.First(&stored, "id = ?", outbox.ID).Error)
	assert.Equal(t, models.OutboxEventStatusFailed, stored.Status)
	assert.Equal(t, 1, stored.Attempts)
	assert.Contains(t, stored.LastError, "Bad Gateway")
	assert.Equal(t, int32(1), deliveries.Load())
}

func TestCareContinuityProcessor_SweepCreatesLifecycleEventsAndTasksOnce(t *testing.T) {
	app, processor, now := newCareContinuityTestProcessor(t)
	organization := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, organization.ID)
	enableCareContinuityEntitlements(
		t,
		app.DB,
		organization.ID,
		user.ID,
		"crm.enabled",
		"commerce.enabled",
	)
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)
	invoice := models.CommerceInvoice{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		ContactID:      contact.ID,
		InvoiceNumber:  "CARE-" + uuid.NewString()[:8],
		IdempotencyKey: "care-invoice-" + uuid.NewString(),
		Status:         models.CommerceInvoiceStatusOpen,
		Currency:       "MYR",
		SubtotalMinor:  10000,
		TotalMinor:     10000,
		DueMinor:       10000,
		IssuedAt:       careTimePointer(now.Add(-7 * 24 * time.Hour)),
		DueAt:          careTimePointer(now.Add(-24 * time.Hour)),
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, app.DB.Create(&invoice).Error)
	contactPackage := createCareContinuityLowExpiringPackage(
		t,
		app.DB,
		organization.ID,
		contact.ID,
		now,
	)

	processor.lastSweep = time.Time{}
	require.NoError(t, processor.RunOnce(context.Background()))

	var taskCount int64
	require.NoError(t, app.DB.Model(&models.FollowUpTask{}).
		Where("organization_id = ? AND source = ?", organization.ID, careTaskSource).
		Count(&taskCount).Error)
	assert.Equal(t, int64(3), taskCount)

	var lifecycleCount int64
	require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).
		Where(
			"organization_id = ? AND event_type IN ?",
			organization.ID,
			[]models.CustomerActivityEventType{
				models.CustomerActivityPackageLow,
				models.CustomerActivityPackageExpiring,
				models.CustomerActivityInvoiceOverdue,
			},
		).
		Count(&lifecycleCount).Error)
	assert.Equal(t, int64(3), lifecycleCount)

	var sourceIDs []uuid.UUID
	require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).
		Where(
			"organization_id = ? AND event_type IN ?",
			organization.ID,
			[]models.CustomerActivityEventType{
				models.CustomerActivityPackageLow,
				models.CustomerActivityPackageExpiring,
			},
		).
		Pluck("source_object_id", &sourceIDs).Error)
	assert.ElementsMatch(t, []uuid.UUID{contactPackage.ID, contactPackage.ID}, sourceIDs)

	processor.lastSweep = time.Time{}
	require.NoError(t, processor.RunOnce(context.Background()))
	require.NoError(t, app.DB.Model(&models.FollowUpTask{}).
		Where("organization_id = ? AND source = ?", organization.ID, careTaskSource).
		Count(&taskCount).Error)
	assert.Equal(t, int64(3), taskCount)
	require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).
		Where(
			"organization_id = ? AND event_type IN ?",
			organization.ID,
			[]models.CustomerActivityEventType{
				models.CustomerActivityPackageLow,
				models.CustomerActivityPackageExpiring,
				models.CustomerActivityInvoiceOverdue,
			},
		).
		Count(&lifecycleCount).Error)
	assert.Equal(t, int64(3), lifecycleCount)

	var messageCount int64
	var customerOutboxCount int64
	require.NoError(t, app.DB.Model(&models.Message{}).
		Where("organization_id = ?", organization.ID).
		Count(&messageCount).Error)
	require.NoError(t, app.DB.Model(&models.OutboxJob{}).
		Where("organization_id = ?", organization.ID).
		Count(&customerOutboxCount).Error)
	assert.Zero(t, messageCount)
	assert.Zero(t, customerOutboxCount)
}

func TestCareContinuityProcessor_SweepWaitsForMergeAndUsesCanonicalContact(t *testing.T) {
	app, processor, now := newCareContinuityTestProcessor(t)
	organization := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(t, app.DB, organization.ID)
	enableCareContinuityEntitlements(
		t,
		app.DB,
		organization.ID,
		owner.ID,
		"crm.enabled",
		"commerce.enabled",
	)
	canonical := testutil.CreateTestContact(t, app.DB, organization.ID)
	source := testutil.CreateTestContact(t, app.DB, organization.ID)
	require.NoError(t, app.DB.Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", canonical.ID, organization.ID).
		Update("assigned_user_id", owner.ID).Error)

	invoice := models.CommerceInvoice{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		ContactID:      source.ID,
		InvoiceNumber:  "MERGED-CARE-" + uuid.NewString()[:8],
		IdempotencyKey: "merged-care-invoice-" + uuid.NewString(),
		Status:         models.CommerceInvoiceStatusOpen,
		Currency:       "MYR",
		SubtotalMinor:  10000,
		TotalMinor:     10000,
		DueMinor:       10000,
		IssuedAt:       careTimePointer(now.Add(-7 * 24 * time.Hour)),
		DueAt:          careTimePointer(now.Add(-24 * time.Hour)),
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, app.DB.Create(&invoice).Error)
	contactPackage := createCareContinuityLowExpiringPackage(
		t,
		app.DB,
		organization.ID,
		source.ID,
		now,
	)

	mergeTx := app.DB.Begin()
	require.NoError(t, mergeTx.Error)
	t.Cleanup(func() { _ = mergeTx.Rollback().Error })
	var lockedContacts []models.Contact
	require.NoError(t, mergeTx.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND id IN ?",
			organization.ID,
			[]uuid.UUID{canonical.ID, source.ID},
		).
		Order("id").
		Find(&lockedContacts).Error)
	require.Len(t, lockedContacts, 2)
	require.NoError(t, mergeTx.Unscoped().Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", source.ID, organization.ID).
		Updates(map[string]any{
			"merged_into_id": canonical.ID,
			"deleted_at":     now,
		}).Error)

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- processor.sweepOrganization(
			context.Background(),
			organization.ID,
			now,
		)
	}()

	select {
	case err := <-resultCh:
		t.Fatalf("care sweep bypassed the merge contact lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}

	require.NoError(t, mergeTx.Commit().Error)
	require.NoError(t, <-resultCh)

	var tasks []models.FollowUpTask
	require.NoError(t, app.DB.
		Where("organization_id = ? AND source = ?", organization.ID, careTaskSource).
		Order("id").
		Find(&tasks).Error)
	require.Len(t, tasks, 3)
	for _, task := range tasks {
		require.NotNil(t, task.ContactID)
		assert.Equal(t, canonical.ID, *task.ContactID)
		require.NotNil(t, task.OwnerUserID)
		assert.Equal(t, owner.ID, *task.OwnerUserID)
		switch task.Metadata["automation_kind"] {
		case "invoice_overdue":
			assert.Equal(t, invoice.ID.String(), task.Metadata["invoice_id"])
		case "package_low", "package_expiring":
			assert.Equal(t, contactPackage.ID.String(), task.Metadata["contact_package_id"])
		default:
			t.Fatalf("unexpected care automation kind: %v", task.Metadata["automation_kind"])
		}
	}

	var activities []models.CustomerActivityEvent
	require.NoError(t, app.DB.
		Where(
			"organization_id = ? AND event_type IN ?",
			organization.ID,
			[]models.CustomerActivityEventType{
				models.CustomerActivityPackageLow,
				models.CustomerActivityPackageExpiring,
				models.CustomerActivityInvoiceOverdue,
				models.CustomerActivityTaskCreated,
			},
		).
		Find(&activities).Error)
	require.Len(t, activities, 6)
	for _, activity := range activities {
		assert.Equal(t, canonical.ID, activity.ContactID)
		switch activity.EventType {
		case models.CustomerActivityInvoiceOverdue:
			require.NotNil(t, activity.SourceObjectID)
			assert.Equal(t, invoice.ID, *activity.SourceObjectID)
		case models.CustomerActivityPackageLow, models.CustomerActivityPackageExpiring:
			require.NotNil(t, activity.SourceObjectID)
			assert.Equal(t, contactPackage.ID, *activity.SourceObjectID)
		}
	}

	var storedInvoice models.CommerceInvoice
	var storedPackage models.ContactPackage
	require.NoError(t, app.DB.First(&storedInvoice, invoice.ID).Error)
	require.NoError(t, app.DB.First(&storedPackage, contactPackage.ID).Error)
	assert.Equal(t, source.ID, storedInvoice.ContactID)
	assert.Equal(t, source.ID, storedPackage.ContactID)

	var aliasTaskCount, aliasActivityCount int64
	require.NoError(t, app.DB.Model(&models.FollowUpTask{}).
		Where("organization_id = ? AND contact_id = ?", organization.ID, source.ID).
		Count(&aliasTaskCount).Error)
	require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).
		Where("organization_id = ? AND contact_id = ?", organization.ID, source.ID).
		Count(&aliasActivityCount).Error)
	assert.Zero(t, aliasTaskCount)
	assert.Zero(t, aliasActivityCount)
}

func TestCareContinuityProcessor_PackageLinkedAliasBookingCreatesCanonicalTask(
	t *testing.T,
) {
	app, processor, now := newCareContinuityTestProcessor(t)
	organization := testutil.CreateTestOrganization(t, app.DB)
	owner := testutil.CreateTestUser(t, app.DB, organization.ID)
	enableCareContinuityEntitlements(
		t,
		app.DB,
		organization.ID,
		owner.ID,
		"crm.enabled",
		"bookings.enabled",
	)
	canonical := testutil.CreateTestContact(t, app.DB, organization.ID)
	source := testutil.CreateTestContact(t, app.DB, organization.ID)
	require.NoError(t, app.DB.Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", canonical.ID, organization.ID).
		Update("assigned_user_id", owner.ID).Error)

	booking := createCareContinuityBooking(
		t,
		app.DB,
		organization.ID,
		source.ID,
		models.BookingStatusNoShow,
		now.Add(-24*time.Hour),
	)
	contactPackage := createCareContinuityLowExpiringPackage(
		t,
		app.DB,
		organization.ID,
		source.ID,
		now,
	)
	require.NoError(t, app.DB.Model(&models.Booking{}).
		Where("id = ? AND organization_id = ?", booking.ID, organization.ID).
		Update("contact_package_id", contactPackage.ID).Error)
	_, outbox := createCareContinuityActivity(
		t,
		app.DB,
		organization.ID,
		source.ID,
		models.CustomerActivityBookingStatus,
		models.CustomerActivityCategoryBooking,
		"booking",
		&booking.ID,
	)

	require.NoError(t, app.DB.Unscoped().Model(&models.Contact{}).
		Where("id = ? AND organization_id = ?", source.ID, organization.ID).
		Updates(map[string]any{
			"merged_into_id": canonical.ID,
			"merged_at":      now,
			"deleted_at":     now,
		}).Error)

	require.NoError(t, processor.RunOnce(context.Background()))

	var storedOutbox models.OutboxEvent
	require.NoError(t, app.DB.First(&storedOutbox, outbox.ID).Error)
	assert.Equal(t, models.OutboxEventStatusPublished, storedOutbox.Status)

	var task models.FollowUpTask
	require.NoError(t, app.DB.
		Where(
			"organization_id = ? AND source = ? AND booking_id = ?",
			organization.ID,
			careTaskSource,
			booking.ID,
		).
		First(&task).Error)
	require.NotNil(t, task.ContactID)
	assert.Equal(t, canonical.ID, *task.ContactID)
	require.NotNil(t, task.BookingID)
	assert.Equal(t, booking.ID, *task.BookingID)
	require.NotNil(t, task.OwnerUserID)
	assert.Equal(t, owner.ID, *task.OwnerUserID)

	var taskActivity models.CustomerActivityEvent
	require.NoError(t, app.DB.
		Where(
			"organization_id = ? AND event_type = ? AND source_object_type = ? AND source_object_id = ?",
			organization.ID,
			models.CustomerActivityTaskCreated,
			"follow_up_task",
			task.ID,
		).
		First(&taskActivity).Error)
	assert.Equal(t, canonical.ID, taskActivity.ContactID)

	var aliasTaskCount, aliasTaskActivityCount int64
	require.NoError(t, app.DB.Model(&models.FollowUpTask{}).
		Where(
			"organization_id = ? AND contact_id = ? AND source = ?",
			organization.ID,
			source.ID,
			careTaskSource,
		).
		Count(&aliasTaskCount).Error)
	require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).
		Where(
			"organization_id = ? AND contact_id = ? AND event_type = ?",
			organization.ID,
			source.ID,
			models.CustomerActivityTaskCreated,
		).
		Count(&aliasTaskActivityCount).Error)
	assert.Zero(t, aliasTaskCount)
	assert.Zero(t, aliasTaskActivityCount)

	var storedBooking models.Booking
	require.NoError(t, app.DB.First(&storedBooking, booking.ID).Error)
	assert.Equal(t, source.ID, storedBooking.ContactID)
	require.NotNil(t, storedBooking.ContactPackageID)
	assert.Equal(t, contactPackage.ID, *storedBooking.ContactPackageID)
}

func TestCareContinuityProcessor_SweepFailsClosedWithoutEntitlements(t *testing.T) {
	app, processor, now := newCareContinuityTestProcessor(t)
	organization := testutil.CreateTestOrganization(t, app.DB)
	contact := testutil.CreateTestContact(t, app.DB, organization.ID)
	invoice := models.CommerceInvoice{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		ContactID:      contact.ID,
		InvoiceNumber:  "UNLICENSED-" + uuid.NewString()[:8],
		IdempotencyKey: "unlicensed-invoice-" + uuid.NewString(),
		Status:         models.CommerceInvoiceStatusOpen,
		Currency:       "MYR",
		SubtotalMinor:  10000,
		TotalMinor:     10000,
		DueMinor:       10000,
		DueAt:          careTimePointer(now.Add(-24 * time.Hour)),
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, app.DB.Create(&invoice).Error)
	processor.lastSweep = time.Time{}

	require.NoError(t, processor.RunOnce(context.Background()))

	var taskCount int64
	var lifecycleCount int64
	require.NoError(t, app.DB.Model(&models.FollowUpTask{}).
		Where("organization_id = ? AND source = ?", organization.ID, careTaskSource).
		Count(&taskCount).Error)
	require.NoError(t, app.DB.Model(&models.CustomerActivityEvent{}).
		Where("organization_id = ? AND event_type = ?", organization.ID, models.CustomerActivityInvoiceOverdue).
		Count(&lifecycleCount).Error)
	assert.Zero(t, taskCount)
	assert.Zero(t, lifecycleCount)
}

func createCareContinuityLowExpiringPackage(
	t *testing.T,
	db *gorm.DB,
	organizationID, contactID uuid.UUID,
	now time.Time,
) *models.ContactPackage {
	t.Helper()
	service := models.BookingService{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organizationID,
		Name:            "Package service " + uuid.NewString()[:8],
		Kind:            models.BookingServiceKindAppointment,
		DurationMinutes: 30,
		DefaultCapacity: 1,
		Currency:        "MYR",
		IsActive:        true,
		ReminderPolicy:  models.JSONB{},
		Metadata:        models.JSONB{},
		Version:         1,
	}
	require.NoError(t, db.Create(&service).Error)
	definition := models.PackageDefinition{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		Name:           "Care package " + uuid.NewString()[:8],
		PriceMinor:     20000,
		Currency:       "MYR",
		ValidityDays:   30,
		IsActive:       true,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, db.Create(&definition).Error)
	entitlement := models.PackageEntitlement{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      organizationID,
		PackageDefinitionID: definition.ID,
		BookingServiceID:    service.ID,
		Credits:             5,
		IsUnlimited:         false,
		Version:             1,
	}
	require.NoError(t, db.Create(&entitlement).Error)
	startsAt := now.Add(-20 * 24 * time.Hour)
	expiresAt := now.Add(7 * 24 * time.Hour)
	contactPackage := models.ContactPackage{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      organizationID,
		ContactID:           contactID,
		PackageDefinitionID: definition.ID,
		Status:              models.ContactPackageStatusActive,
		StartsAt:            &startsAt,
		ExpiresAt:           &expiresAt,
		PurchaseAmountMinor: 20000,
		Currency:            "MYR",
		Source:              "test",
		IdempotencyKey:      "care-package-" + uuid.NewString(),
		Metadata:            models.JSONB{},
		Version:             1,
	}
	require.NoError(t, db.Create(&contactPackage).Error)
	balance := models.CreditBalance{
		BaseModel:            models.BaseModel{ID: uuid.New()},
		OrganizationID:       organizationID,
		ContactPackageID:     contactPackage.ID,
		PackageEntitlementID: entitlement.ID,
		Granted:              5,
		Consumed:             3,
		Available:            2,
		Version:              1,
	}
	require.NoError(t, db.Create(&balance).Error)
	return &contactPackage
}

func TestCareContinuityProcessor_RetryBackoffIsBounded(t *testing.T) {
	assert.Equal(t, 5*time.Second, careRetryBackoff(1))
	assert.Equal(t, 10*time.Second, careRetryBackoff(2))
	assert.Equal(t, 15*time.Minute, careRetryBackoff(100))
	assert.NotPanics(t, func() {
		processor := &CareContinuityProcessor{stopCh: make(chan struct{})}
		processor.Stop()
		processor.Stop()
	})
	assert.Equal(t, "", careErrorString(nil))
}

func careTimePointer(value time.Time) *time.Time {
	return &value
}
