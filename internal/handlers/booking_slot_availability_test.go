package handlers

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestEnsureBookingEventHasNotStarted(t *testing.T) {
	now := time.Date(2026, time.September, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		startsAt  time.Time
		wantError bool
	}{
		{name: "future", startsAt: now.Add(time.Nanosecond)},
		{name: "equal", startsAt: now, wantError: true},
		{name: "past", startsAt: now.Add(-time.Nanosecond), wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ensureBookingEventHasNotStarted(
				&models.BookingEvent{StartsAt: test.startsAt},
				now,
			)
			if !test.wantError {
				require.NoError(t, err)
				return
			}
			var clientErr *bookingCommerceClientError
			require.Error(t, err)
			require.True(t, errors.As(err, &clientErr))
			assert.Equal(t, fasthttp.StatusConflict, clientErr.status)
			assert.Contains(t, strings.ToLower(clientErr.message), "already started")
		})
	}
}

func TestListBookingAvailabilityFiltersUnsafeSlotsAndReturnsLocalDetails(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	contact := testutil.CreateTestContact(t, db, organization.ID)
	enableBookingCommerceTestEntitlement(t, db, organization.ID, user.ID, "bookings.enabled")

	now := time.Now().UTC().Truncate(time.Second)
	available := createBookingAvailabilityFixture(
		t,
		db,
		organization.ID,
		user.ID,
		now.Add(2*time.Hour),
		models.BookingEventStatusScheduled,
		true,
		true,
		2,
	)
	available.resource.Location = "Klinik Ampang"
	require.NoError(t, db.Model(&models.BookingResource{}).
		Where("id = ?", available.resource.ID).
		Update("location", available.resource.Location).Error)
	require.NoError(t, db.Create(&models.Booking{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		EventID:        available.event.ID,
		ContactID:      contact.ID,
		Status:         models.BookingStatusReserved,
		Quantity:       1,
		Source:         models.BookingSourceAgent,
		IdempotencyKey: "availability-occupied-" + uuid.NewString(),
		Metadata:       models.JSONB{},
		Version:        1,
	}).Error)

	full := createBookingAvailabilityFixture(
		t,
		db,
		organization.ID,
		user.ID,
		now.Add(3*time.Hour),
		models.BookingEventStatusScheduled,
		true,
		true,
		1,
	)
	require.NoError(t, db.Create(&models.Booking{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		EventID:        full.event.ID,
		ContactID:      contact.ID,
		Status:         models.BookingStatusConfirmed,
		Quantity:       1,
		Source:         models.BookingSourceAgent,
		IdempotencyKey: "availability-full-" + uuid.NewString(),
		Metadata:       models.JSONB{},
		Version:        1,
	}).Error)
	createBookingAvailabilityFixture(
		t, db, organization.ID, user.ID, now.Add(-time.Hour),
		models.BookingEventStatusScheduled, true, true, 1,
	)
	createBookingAvailabilityFixture(
		t, db, organization.ID, user.ID, now.Add(4*time.Hour),
		models.BookingEventStatusCancelled, true, true, 1,
	)
	createBookingAvailabilityFixture(
		t, db, organization.ID, user.ID, now.Add(5*time.Hour),
		models.BookingEventStatusScheduled, false, true, 1,
	)
	createBookingAvailabilityFixture(
		t, db, organization.ID, user.ID, now.Add(6*time.Hour),
		models.BookingEventStatusScheduled, true, false, 1,
	)
	createBookingAvailabilityFixture(
		t, db, organization.ID, user.ID, now.AddDate(0, 0, defaultBookingAvailabilityDays+1),
		models.BookingEventStatusScheduled, true, true, 1,
	)

	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, organization.ID, user.ID)
	require.NoError(t, app.ListBookingAvailability(request))
	require.Equal(
		t,
		fasthttp.StatusOK,
		testutil.GetResponseStatusCode(request),
		string(testutil.GetResponseBody(request)),
	)
	var response struct {
		Slots []BookingAvailabilitySlotResponse `json:"slots"`
		Total int64                             `json:"total"`
		Page  int                               `json:"page"`
		Limit int                               `json:"limit"`
	}
	testutil.ParseEnvelopeResponse(t, request, &response)
	require.Len(t, response.Slots, 1)
	assert.EqualValues(t, 1, response.Total)
	assert.Equal(t, available.event.ID, response.Slots[0].ID)
	assert.Equal(t, 1, response.Slots[0].BookedQuantity)
	assert.Equal(t, 1, response.Slots[0].RemainingCapacity)
	assert.Equal(t, "Asia/Kuala_Lumpur", response.Slots[0].Timezone)
	assert.Equal(t, "Klinik Ampang", response.Slots[0].Location)
	assert.Equal(
		t,
		available.event.StartsAt.In(time.FixedZone("MYT", 8*60*60)).Format(time.RFC3339),
		response.Slots[0].LocalStartsAt,
	)
	require.NotNil(t, response.Slots[0].Service)
	assert.True(t, response.Slots[0].Service.IsActive)
	require.NotNil(t, response.Slots[0].Resource)
	assert.True(t, response.Slots[0].Resource.IsActive)
}

func TestListBookingAvailabilityDoesNotShrinkPageAfterConcurrentCapacityChange(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	contact := testutil.CreateTestContact(t, db, organization.ID)
	enableBookingCommerceTestEntitlement(t, db, organization.ID, user.ID, "bookings.enabled")

	now := time.Now().UTC().Truncate(time.Second)
	first := createBookingAvailabilityFixture(
		t,
		db,
		organization.ID,
		user.ID,
		now.Add(2*time.Hour),
		models.BookingEventStatusScheduled,
		true,
		true,
		1,
	)
	createBookingAvailabilityFixture(
		t,
		db,
		organization.ID,
		user.ID,
		now.Add(3*time.Hour),
		models.BookingEventStatusScheduled,
		true,
		true,
		1,
	)

	// Fill the first slot immediately after the paginated availability SELECT.
	// The response is allowed to represent that SELECT's snapshot because the
	// create transaction rechecks capacity. It must not perform a second read,
	// drop this row, and make page 1 look like the end of a two-row result set.
	callbackName := "test:fill_slot_after_availability_page_" + uuid.NewString()
	filled := false
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(
		callbackName,
		func(tx *gorm.DB) {
			statement := strings.ToLower(tx.Statement.SQL.String())
			if filled || tx.Statement.Table != "booking_events" ||
				!strings.Contains(statement, "order by") ||
				!strings.Contains(statement, "limit") {
				return
			}
			filled = true
			require.NoError(t, db.Session(&gorm.Session{NewDB: true}).Create(&models.Booking{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: organization.ID,
				EventID:        first.event.ID,
				ContactID:      contact.ID,
				Status:         models.BookingStatusReserved,
				Quantity:       1,
				Source:         models.BookingSourceAgent,
				IdempotencyKey: "availability-race-" + uuid.NewString(),
				Metadata:       models.JSONB{},
				Version:        1,
			}).Error)
		},
	))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Query().Remove(callbackName))
	})

	request := testutil.NewGETRequest(t)
	request.RequestCtx.QueryArgs().Set("page", "1")
	request.RequestCtx.QueryArgs().Set("limit", "1")
	testutil.SetAuthContext(request, organization.ID, user.ID)
	require.NoError(t, app.ListBookingAvailability(request))
	require.Equal(
		t,
		fasthttp.StatusOK,
		testutil.GetResponseStatusCode(request),
		string(testutil.GetResponseBody(request)),
	)

	var response struct {
		Slots []BookingAvailabilitySlotResponse `json:"slots"`
		Total int64                             `json:"total"`
		Page  int                               `json:"page"`
		Limit int                               `json:"limit"`
	}
	testutil.ParseEnvelopeResponse(t, request, &response)
	require.True(t, filled)
	assert.EqualValues(t, 2, response.Total)
	assert.Equal(t, 1, response.Page)
	assert.Equal(t, 1, response.Limit)
	require.Len(t, response.Slots, 1)
	assert.Equal(t, first.event.ID, response.Slots[0].ID)
	assert.Zero(t, response.Slots[0].BookedQuantity)
	assert.Equal(t, 1, response.Slots[0].RemainingCapacity)
}

func TestCreateBookingRejectsStaleAvailabilityWithActionableConflict(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, organization.ID, user.ID, "bookings.enabled")

	tests := []struct {
		name            string
		startsAt        time.Time
		status          models.BookingEventStatus
		serviceActive   bool
		resourceActive  bool
		fillCapacity    bool
		expectedMessage string
	}{
		{
			name: "past slot", startsAt: time.Now().UTC().Add(-time.Hour),
			status: models.BookingEventStatusScheduled, serviceActive: true, resourceActive: true,
			expectedMessage: "already started",
		},
		{
			name: "cancelled slot", startsAt: time.Now().UTC().Add(time.Hour),
			status: models.BookingEventStatusCancelled, serviceActive: true, resourceActive: true,
			expectedMessage: "no longer scheduled",
		},
		{
			name: "inactive service", startsAt: time.Now().UTC().Add(2 * time.Hour),
			status: models.BookingEventStatusScheduled, resourceActive: true,
			expectedMessage: "service is inactive",
		},
		{
			name: "inactive resource", startsAt: time.Now().UTC().Add(3 * time.Hour),
			status: models.BookingEventStatusScheduled, serviceActive: true,
			expectedMessage: "resource is inactive",
		},
		{
			name: "capacity taken", startsAt: time.Now().UTC().Add(4 * time.Hour),
			status: models.BookingEventStatusScheduled, serviceActive: true, resourceActive: true,
			fillCapacity: true, expectedMessage: "just filled",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := createBookingAvailabilityFixture(
				t,
				db,
				organization.ID,
				user.ID,
				test.startsAt,
				test.status,
				test.serviceActive,
				test.resourceActive,
				1,
			)
			if test.fillCapacity {
				existingContact := testutil.CreateTestContact(t, db, organization.ID)
				require.NoError(t, db.Create(&models.Booking{
					BaseModel:      models.BaseModel{ID: uuid.New()},
					OrganizationID: organization.ID,
					EventID:        fixture.event.ID,
					ContactID:      existingContact.ID,
					Status:         models.BookingStatusReserved,
					Quantity:       1,
					Source:         models.BookingSourceAgent,
					IdempotencyKey: "stale-existing-" + uuid.NewString(),
					Metadata:       models.JSONB{},
					Version:        1,
				}).Error)
			}

			contact := testutil.CreateTestContact(t, db, organization.ID)
			idempotencyKey := "stale-attempt-" + uuid.NewString()
			request := testutil.NewJSONRequest(t, CreateBookingRequest{
				EventID:        fixture.event.ID,
				ContactID:      contact.ID,
				Quantity:       1,
				Status:         models.BookingStatusReserved,
				Source:         models.BookingSourceAgent,
				AllowWaitlist:  false,
				IdempotencyKey: idempotencyKey,
			})
			testutil.SetAuthContext(request, organization.ID, user.ID)
			testutil.SetPathParam(request, "id", fixture.event.ID.String())

			require.NoError(t, app.CreateBooking(request))
			require.Equal(
				t,
				fasthttp.StatusConflict,
				testutil.GetResponseStatusCode(request),
				string(testutil.GetResponseBody(request)),
			)
			var envelope testutil.APIEnvelope
			testutil.ParseJSONResponse(t, request, &envelope)
			require.NotNil(t, envelope.Message)
			assert.Contains(t, strings.ToLower(*envelope.Message), test.expectedMessage)

			var created int64
			require.NoError(t, db.Model(&models.Booking{}).
				Where("organization_id = ? AND idempotency_key = ?", organization.ID, idempotencyKey).
				Count(&created).Error)
			assert.Zero(t, created)
		})
	}
}

func TestCreateBookingRechecksStartAfterReservationLocks(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	contact := testutil.CreateTestContact(t, db, organization.ID)
	enableBookingCommerceTestEntitlement(t, db, organization.ID, user.ID, "bookings.enabled")

	startsAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Microsecond)
	fixture := createBookingAvailabilityFixture(
		t,
		db,
		organization.ID,
		user.ID,
		startsAt,
		models.BookingEventStatusScheduled,
		true,
		true,
		1,
	)
	clockNanos, clockReads := installBookingReservationTestClock(
		t,
		startsAt.Add(-time.Minute),
	)

	lockTx := db.Begin()
	require.NoError(t, lockTx.Error)
	lockReleased := false
	t.Cleanup(func() {
		if !lockReleased {
			_ = lockTx.Rollback().Error
		}
	})
	var lockedResource models.BookingResource
	require.NoError(t, lockTx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", fixture.resource.ID, organization.ID).
		First(&lockedResource).Error)

	resourceLockAttempted := make(chan struct{}, 1)
	callbackName := "test:booking_resource_lock_boundary_" + uuid.NewString()
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(
		callbackName,
		func(tx *gorm.DB) {
			if tx.Statement.Table != "booking_resources" {
				return
			}
			if _, locking := tx.Statement.Clauses["FOR"]; !locking {
				return
			}
			select {
			case resourceLockAttempted <- struct{}{}:
			default:
			}
		},
	))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Query().Remove(callbackName))
	})

	idempotencyKey := "reservation-lock-boundary-" + uuid.NewString()
	request := testutil.NewJSONRequest(t, CreateBookingRequest{
		EventID:        fixture.event.ID,
		ContactID:      contact.ID,
		Quantity:       1,
		Status:         models.BookingStatusReserved,
		Source:         models.BookingSourceAgent,
		AllowWaitlist:  false,
		IdempotencyKey: idempotencyKey,
	})
	testutil.SetAuthContext(request, organization.ID, user.ID)
	testutil.SetPathParam(request, "id", fixture.event.ID.String())

	done := make(chan error, 1)
	go func() {
		done <- app.CreateBooking(request)
	}()

	select {
	case <-resourceLockAttempted:
	case err := <-done:
		lockReleased = true
		require.NoError(t, lockTx.Rollback().Error)
		require.NoError(t, err)
		t.Fatalf("booking returned before waiting for the resource lock: %s", testutil.GetResponseBody(request))
	case <-time.After(5 * time.Second):
		lockReleased = true
		require.NoError(t, lockTx.Rollback().Error)
		t.Fatal("booking did not reach the resource lock")
	}
	require.Zero(t, clockReads.Load(), "reservation clock was sampled before all booking locks")
	clockNanos.Store(startsAt.UnixNano())
	require.NoError(t, lockTx.Commit().Error)
	lockReleased = true

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("booking did not finish after the resource lock was released")
	}
	require.Equal(
		t,
		fasthttp.StatusConflict,
		testutil.GetResponseStatusCode(request),
		string(testutil.GetResponseBody(request)),
	)
	var envelope testutil.APIEnvelope
	testutil.ParseJSONResponse(t, request, &envelope)
	require.NotNil(t, envelope.Message)
	assert.Contains(t, strings.ToLower(*envelope.Message), "already started")
	assert.EqualValues(t, 1, clockReads.Load())

	var created int64
	require.NoError(t, db.Model(&models.Booking{}).
		Where("organization_id = ? AND idempotency_key = ?", organization.ID, idempotencyKey).
		Count(&created).Error)
	assert.Zero(t, created)
}

func TestCreateBookingAllowsPostLockIdempotentReplayAfterStart(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithSuperAdmin())
	contact := testutil.CreateTestContact(t, db, organization.ID)
	enableBookingCommerceTestEntitlement(t, db, organization.ID, user.ID, "bookings.enabled")

	startsAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Microsecond)
	fixture := createBookingAvailabilityFixture(
		t,
		db,
		organization.ID,
		user.ID,
		startsAt,
		models.BookingEventStatusScheduled,
		true,
		true,
		1,
	)
	_, clockReads := installBookingReservationTestClock(t, startsAt)

	lockTx := db.Begin()
	require.NoError(t, lockTx.Error)
	lockReleased := false
	t.Cleanup(func() {
		if !lockReleased {
			_ = lockTx.Rollback().Error
		}
	})
	var lockedEvent models.BookingEvent
	require.NoError(t, lockTx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", fixture.event.ID, organization.ID).
		First(&lockedEvent).Error)

	idempotencyKey := "post-lock-replay-" + uuid.NewString()
	existing := models.Booking{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		EventID:        fixture.event.ID,
		ContactID:      contact.ID,
		Status:         models.BookingStatusReserved,
		Quantity:       1,
		Source:         models.BookingSourceAgent,
		IdempotencyKey: idempotencyKey,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, lockTx.Create(&existing).Error)

	eventLockAttempted := make(chan struct{}, 1)
	callbackName := "test:booking_event_lock_replay_" + uuid.NewString()
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(
		callbackName,
		func(tx *gorm.DB) {
			if tx.Statement.Table != "booking_events" {
				return
			}
			if _, locking := tx.Statement.Clauses["FOR"]; !locking {
				return
			}
			select {
			case eventLockAttempted <- struct{}{}:
			default:
			}
		},
	))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Query().Remove(callbackName))
	})

	request := testutil.NewJSONRequest(t, CreateBookingRequest{
		EventID:        fixture.event.ID,
		ContactID:      contact.ID,
		Quantity:       1,
		Status:         models.BookingStatusReserved,
		Source:         models.BookingSourceAgent,
		AllowWaitlist:  false,
		IdempotencyKey: idempotencyKey,
	})
	testutil.SetAuthContext(request, organization.ID, user.ID)
	testutil.SetPathParam(request, "id", fixture.event.ID.String())

	done := make(chan error, 1)
	go func() {
		done <- app.CreateBooking(request)
	}()

	select {
	case <-eventLockAttempted:
	case err := <-done:
		lockReleased = true
		require.NoError(t, lockTx.Rollback().Error)
		require.NoError(t, err)
		t.Fatalf("booking returned before waiting for the event lock: %s", testutil.GetResponseBody(request))
	case <-time.After(5 * time.Second):
		lockReleased = true
		require.NoError(t, lockTx.Rollback().Error)
		t.Fatal("booking did not reach the event lock")
	}
	require.Zero(t, clockReads.Load(), "reservation clock was sampled before the post-lock replay lookup")
	require.NoError(t, lockTx.Commit().Error)
	lockReleased = true

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("idempotent replay did not finish after the event lock was released")
	}
	require.Equal(
		t,
		fasthttp.StatusOK,
		testutil.GetResponseStatusCode(request),
		string(testutil.GetResponseBody(request)),
	)
	assert.Zero(t, clockReads.Load(), "completed idempotent replay must not be rejected by the start boundary")
	var replayed models.Booking
	testutil.ParseEnvelopeResponse(t, request, &replayed)
	assert.Equal(t, existing.ID, replayed.ID)
}

func installBookingReservationTestClock(
	t *testing.T,
	initial time.Time,
) (*atomic.Int64, *atomic.Int32) {
	t.Helper()
	original := bookingReservationNow
	var clockNanos atomic.Int64
	clockNanos.Store(initial.UnixNano())
	var reads atomic.Int32
	bookingReservationNow = func() time.Time {
		reads.Add(1)
		return time.Unix(0, clockNanos.Load()).UTC()
	}
	t.Cleanup(func() {
		bookingReservationNow = original
	})
	return &clockNanos, &reads
}

type bookingAvailabilityFixture struct {
	service  models.BookingService
	resource models.BookingResource
	event    models.BookingEvent
}

func createBookingAvailabilityFixture(
	t *testing.T,
	db *gorm.DB,
	organizationID, userID uuid.UUID,
	startsAt time.Time,
	status models.BookingEventStatus,
	serviceActive, resourceActive bool,
	capacity int,
) bookingAvailabilityFixture {
	t.Helper()
	service := models.BookingService{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organizationID,
		Name:            "Availability service " + uuid.NewString()[:8],
		Kind:            models.BookingServiceKindAppointment,
		DurationMinutes: 30,
		DefaultCapacity: capacity,
		Currency:        "MYR",
		IsActive:        serviceActive,
		Metadata:        models.JSONB{},
		Version:         1,
		CreatedByID:     &userID,
		UpdatedByID:     &userID,
	}
	require.NoError(t, db.Create(&service).Error)
	serviceActiveUpdate := db.Model(&models.BookingService{}).
		Where("id = ? AND organization_id = ?", service.ID, organizationID).
		UpdateColumn("is_active", serviceActive)
	require.NoError(t, serviceActiveUpdate.Error)
	require.EqualValues(t, 1, serviceActiveUpdate.RowsAffected)
	var persistedService models.BookingService
	require.NoError(t, db.Select("id", "is_active").
		Where("id = ? AND organization_id = ?", service.ID, organizationID).
		First(&persistedService).Error)
	require.Equal(t, serviceActive, persistedService.IsActive)
	service.IsActive = persistedService.IsActive
	resource := models.BookingResource{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		Name:           "Availability practitioner " + uuid.NewString()[:8],
		Kind:           models.BookingResourceKindPractitioner,
		Timezone:       "Asia/Kuala_Lumpur",
		IsActive:       resourceActive,
		Metadata:       models.JSONB{},
		Version:        1,
		CreatedByID:    &userID,
		UpdatedByID:    &userID,
	}
	require.NoError(t, db.Create(&resource).Error)
	resourceActiveUpdate := db.Model(&models.BookingResource{}).
		Where("id = ? AND organization_id = ?", resource.ID, organizationID).
		UpdateColumn("is_active", resourceActive)
	require.NoError(t, resourceActiveUpdate.Error)
	require.EqualValues(t, 1, resourceActiveUpdate.RowsAffected)
	var persistedResource models.BookingResource
	require.NoError(t, db.Select("id", "is_active").
		Where("id = ? AND organization_id = ?", resource.ID, organizationID).
		First(&persistedResource).Error)
	require.Equal(t, resourceActive, persistedResource.IsActive)
	resource.IsActive = persistedResource.IsActive
	event := models.BookingEvent{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		ServiceID:      service.ID,
		ResourceID:     resource.ID,
		StartsAt:       startsAt.UTC(),
		EndsAt:         startsAt.UTC().Add(30 * time.Minute),
		Capacity:       capacity,
		Status:         status,
		Metadata:       models.JSONB{},
		Version:        1,
		CreatedByID:    &userID,
		UpdatedByID:    &userID,
	}
	require.NoError(t, db.Create(&event).Error)
	return bookingAvailabilityFixture{service: service, resource: resource, event: event}
}
