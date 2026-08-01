package handlers

import (
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

func TestAvailabilityRuleValidationAndEffectiveDateParsing(t *testing.T) {
	t.Parallel()

	valid := AvailabilityRuleRequest{
		Weekday:        int(time.Monday),
		StartLocalTime: "09:00",
		EndLocalTime:   "17:00",
	}
	require.NoError(t, validateAvailabilityRuleRequest(&valid, false))

	invalidWeekday := valid
	invalidWeekday.Weekday = 7
	require.EqualError(
		t,
		validateAvailabilityRuleRequest(&invalidWeekday, false),
		"weekday must be between 0 (Sunday) and 6 (Saturday)",
	)
	invalidTime := valid
	invalidTime.StartLocalTime = "9:00"
	require.EqualError(
		t,
		validateAvailabilityRuleRequest(&invalidTime, false),
		"start_local_time must use HH:MM in 24-hour time",
	)
	overnight := valid
	overnight.StartLocalTime = "22:00"
	overnight.EndLocalTime = "02:00"
	require.EqualError(
		t,
		validateAvailabilityRuleRequest(&overnight, false),
		"availability rules must be same-day intervals with end_local_time after start_local_time",
	)

	location, err := time.LoadLocation("Asia/Kuala_Lumpur")
	require.NoError(t, err)
	dateOnly := "2026-08-03"
	rfc3339 := "2026-08-31T16:30:00Z"
	valid.EffectiveFrom = &dateOnly
	valid.EffectiveUntil = &rfc3339
	require.NoError(t, normalizeAvailabilityRuleEffectiveDates(&valid, location))
	require.NotNil(t, valid.normalizedEffectiveFrom)
	require.NotNil(t, valid.normalizedEffectiveUntil)
	assert.Equal(t, "2026-08-03", valid.normalizedEffectiveFrom.In(location).Format("2006-01-02"))
	assert.Equal(t, "2026-09-01", valid.normalizedEffectiveUntil.UTC().Format("2006-01-02"))

	reversedFrom := "2026-09-01"
	reversedUntil := "2026-08-31"
	reversed := valid
	reversed.EffectiveFrom = &reversedFrom
	reversed.EffectiveUntil = &reversedUntil
	require.ErrorContains(t, normalizeAvailabilityRuleEffectiveDates(&reversed, location), "effective_until")
}

func TestNormalizeBookingEventRequestTimesRejectsAmbiguousDSTWallTimes(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name       string
		timezone   string
		startsAt   string
		endsAt     string
		ambiguous  bool
		errorField string
	}{
		{
			name: "New York one-hour fall back", timezone: "America/New_York",
			startsAt: "2026-11-01T01:30", endsAt: "2026-11-01T02:30",
			ambiguous: true, errorField: "local_starts_at",
		},
		{
			name: "Lord Howe thirty-minute fall back", timezone: "Australia/Lord_Howe",
			startsAt: "2026-04-05T01:45", endsAt: "2026-04-05T02:30",
			ambiguous: true, errorField: "local_starts_at",
		},
		{
			name: "Casey historical three-hour fall back", timezone: "Antarctica/Casey",
			startsAt: "2018-03-11T02:00", endsAt: "2018-03-11T03:00",
			ambiguous: true, errorField: "local_starts_at",
		},
		{
			name: "New York after transition", timezone: "America/New_York",
			startsAt: "2026-11-01T03:00", endsAt: "2026-11-01T03:30",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			request := BookingEventRequest{
				LocalStartsAt: testCase.startsAt,
				LocalEndsAt:   testCase.endsAt,
				Timezone:      testCase.timezone,
			}
			err := normalizeBookingEventRequestTimes(&request)
			if testCase.ambiguous {
				require.ErrorContains(t, err, testCase.errorField+" is ambiguous in "+testCase.timezone)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestAvailabilityRuleCRUDTenantIsolationOverlapAndVersions(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	setBookingTestOrganizationTimezone(t, db, org, "UTC")
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "bookings.enabled")
	resource := createBookingAvailabilityTestResource(t, db, org.ID, user.ID, "Primary room")

	otherOrg := testutil.CreateTestOrganization(t, db)
	otherResource := createBookingAvailabilityTestResource(t, db, otherOrg.ID, user.ID, "Other tenant room")
	crossTenant := bookingAvailabilityRequest(t, nil, org.ID, user.ID, otherResource.ID)
	require.NoError(t, app.ListAvailabilityRules(crossTenant))
	testutil.AssertErrorResponse(t, crossTenant, fasthttp.StatusNotFound, "Booking resource not found")

	firstFrom := "2026-08-01"
	firstUntil := "2026-08-31"
	firstRequest := AvailabilityRuleRequest{
		Weekday:        int(time.Monday),
		StartLocalTime: "09:00",
		EndLocalTime:   "12:00",
		EffectiveFrom:  &firstFrom,
		EffectiveUntil: &firstUntil,
	}
	createFirst := bookingAvailabilityRequest(t, firstRequest, org.ID, user.ID, resource.ID)
	require.NoError(t, app.CreateAvailabilityRule(createFirst))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(createFirst))
	var first AvailabilityRuleResponse
	testutil.ParseEnvelopeResponse(t, createFirst, &first)
	require.NotNil(t, first.EffectiveFromDate)
	require.NotNil(t, first.EffectiveUntilDate)
	assert.Equal(t, firstFrom, *first.EffectiveFromDate)
	assert.Equal(t, firstUntil, *first.EffectiveUntilDate)
	assert.EqualValues(t, 1, first.Version)

	secondFrom := "2026-09-01"
	secondUntil := "2026-09-30"
	secondRequest := firstRequest
	secondRequest.EffectiveFrom = &secondFrom
	secondRequest.EffectiveUntil = &secondUntil
	createSecond := bookingAvailabilityRequest(t, secondRequest, org.ID, user.ID, resource.ID)
	require.NoError(t, app.CreateAvailabilityRule(createSecond))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(createSecond))

	boundaryFrom := "2026-08-31"
	boundaryUntil := "2026-09-01"
	boundaryRequest := firstRequest
	boundaryRequest.EffectiveFrom = &boundaryFrom
	boundaryRequest.EffectiveUntil = &boundaryUntil
	createBoundary := bookingAvailabilityRequest(t, boundaryRequest, org.ID, user.ID, resource.ID)
	require.NoError(t, app.CreateAvailabilityRule(createBoundary))
	testutil.AssertErrorResponse(t, createBoundary, fasthttp.StatusConflict, "overlaps an active rule")

	overlappingTime := firstRequest
	overlappingTime.StartLocalTime = "11:00"
	overlappingTime.EndLocalTime = "13:00"
	createOverlap := bookingAvailabilityRequest(t, overlappingTime, org.ID, user.ID, resource.ID)
	require.NoError(t, app.CreateAvailabilityRule(createOverlap))
	testutil.AssertErrorResponse(t, createOverlap, fasthttp.StatusConflict, "overlaps an active rule")

	staleUpdateBody := firstRequest
	staleUpdateBody.Version = 99
	staleUpdate := bookingAvailabilityItemRequest(
		t,
		staleUpdateBody,
		org.ID,
		user.ID,
		resource.ID,
		first.ID,
	)
	require.NoError(t, app.UpdateAvailabilityRule(staleUpdate))
	testutil.AssertErrorResponse(t, staleUpdate, fasthttp.StatusConflict, "was modified")

	validUpdateBody := firstRequest
	validUpdateBody.StartLocalTime = "08:00"
	validUpdateBody.EndLocalTime = "09:00"
	validUpdateBody.Version = first.Version
	validUpdate := bookingAvailabilityItemRequest(
		t,
		validUpdateBody,
		org.ID,
		user.ID,
		resource.ID,
		first.ID,
	)
	require.NoError(t, app.UpdateAvailabilityRule(validUpdate))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(validUpdate))
	var updated AvailabilityRuleResponse
	testutil.ParseEnvelopeResponse(t, validUpdate, &updated)
	assert.EqualValues(t, 2, updated.Version)

	list := bookingAvailabilityRequest(t, nil, org.ID, user.ID, resource.ID)
	require.NoError(t, app.ListAvailabilityRules(list))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(list))
	var listed struct {
		AvailabilityRules []AvailabilityRuleResponse `json:"availability_rules"`
		Total             int64                      `json:"total"`
	}
	testutil.ParseEnvelopeResponse(t, list, &listed)
	assert.EqualValues(t, 2, listed.Total)
	assert.Len(t, listed.AvailabilityRules, 2)

	staleDelete := bookingAvailabilityItemRequest(
		t,
		BookingVersionRequest{Version: 1},
		org.ID,
		user.ID,
		resource.ID,
		first.ID,
	)
	require.NoError(t, app.DeleteAvailabilityRule(staleDelete))
	testutil.AssertErrorResponse(t, staleDelete, fasthttp.StatusConflict, "was modified")
	validDelete := bookingAvailabilityItemRequest(
		t,
		BookingVersionRequest{Version: updated.Version},
		org.ID,
		user.ID,
		resource.ID,
		first.ID,
	)
	require.NoError(t, app.DeleteAvailabilityRule(validDelete))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(validDelete))
}

func TestResourceTimeOffCRUDRejectsOverlapAndNonCancelledEvents(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	setBookingTestOrganizationTimezone(t, db, org, "UTC")
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "bookings.enabled")
	service, resource := createBookingAvailabilityTestServiceAndResource(t, db, org.ID, user.ID)

	scheduledStart := time.Date(2026, 8, 3, 2, 0, 0, 0, time.UTC)
	createBookingAvailabilityTestEvent(
		t,
		db,
		org.ID,
		user.ID,
		service.ID,
		resource.ID,
		scheduledStart,
		scheduledStart.Add(time.Hour),
		models.BookingEventStatusScheduled,
	)
	conflict := ResourceTimeOffRequest{
		StartsAt: scheduledStart.Add(30 * time.Minute),
		EndsAt:   scheduledStart.Add(90 * time.Minute),
		Reason:   "Conflicts with appointment",
	}
	createConflict := bookingAvailabilityRequest(t, conflict, org.ID, user.ID, resource.ID)
	require.NoError(t, app.CreateResourceTimeOff(createConflict))
	testutil.AssertErrorResponse(t, createConflict, fasthttp.StatusConflict, "non-cancelled booking event")

	cancelledStart := time.Date(2026, 8, 3, 4, 0, 0, 0, time.UTC)
	createBookingAvailabilityTestEvent(
		t,
		db,
		org.ID,
		user.ID,
		service.ID,
		resource.ID,
		cancelledStart,
		cancelledStart.Add(time.Hour),
		models.BookingEventStatusCancelled,
	)
	allowed := ResourceTimeOffRequest{
		StartsAt: cancelledStart.Add(30 * time.Minute),
		EndsAt:   cancelledStart.Add(90 * time.Minute),
		Reason:   "Leave",
	}
	createAllowed := bookingAvailabilityRequest(t, allowed, org.ID, user.ID, resource.ID)
	require.NoError(t, app.CreateResourceTimeOff(createAllowed))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(createAllowed))
	var created models.ResourceTimeOff
	testutil.ParseEnvelopeResponse(t, createAllowed, &created)
	assert.EqualValues(t, 1, created.Version)

	overlap := ResourceTimeOffRequest{
		StartsAt: allowed.EndsAt.Add(-30 * time.Minute),
		EndsAt:   allowed.EndsAt.Add(30 * time.Minute),
	}
	createOverlap := bookingAvailabilityRequest(t, overlap, org.ID, user.ID, resource.ID)
	require.NoError(t, app.CreateResourceTimeOff(createOverlap))
	testutil.AssertErrorResponse(t, createOverlap, fasthttp.StatusConflict, "overlaps an existing time-off")

	staleBody := allowed
	staleBody.Version = 2
	stale := bookingAvailabilityItemRequest(t, staleBody, org.ID, user.ID, resource.ID, created.ID)
	require.NoError(t, app.UpdateResourceTimeOff(stale))
	testutil.AssertErrorResponse(t, stale, fasthttp.StatusConflict, "was modified")

	validBody := ResourceTimeOffRequest{
		StartsAt: time.Date(2026, 8, 3, 6, 0, 0, 0, time.UTC),
		EndsAt:   time.Date(2026, 8, 3, 7, 0, 0, 0, time.UTC),
		Reason:   "Updated leave",
		Version:  1,
	}
	valid := bookingAvailabilityItemRequest(t, validBody, org.ID, user.ID, resource.ID, created.ID)
	require.NoError(t, app.UpdateResourceTimeOff(valid))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(valid))
	var updated models.ResourceTimeOff
	testutil.ParseEnvelopeResponse(t, valid, &updated)
	assert.EqualValues(t, 2, updated.Version)

	list := bookingAvailabilityRequest(t, nil, org.ID, user.ID, resource.ID)
	require.NoError(t, app.ListResourceTimeOff(list))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(list))
	var listed struct {
		TimeOff []models.ResourceTimeOff `json:"time_off"`
		Total   int64                    `json:"total"`
	}
	testutil.ParseEnvelopeResponse(t, list, &listed)
	assert.EqualValues(t, 1, listed.Total)
	assert.Len(t, listed.TimeOff, 1)

	staleDelete := bookingAvailabilityItemRequest(
		t,
		BookingVersionRequest{Version: 1},
		org.ID,
		user.ID,
		resource.ID,
		created.ID,
	)
	require.NoError(t, app.DeleteResourceTimeOff(staleDelete))
	testutil.AssertErrorResponse(t, staleDelete, fasthttp.StatusConflict, "was modified")
	validDelete := bookingAvailabilityItemRequest(
		t,
		BookingVersionRequest{Version: updated.Version},
		org.ID,
		user.ID,
		resource.ID,
		created.ID,
	)
	require.NoError(t, app.DeleteResourceTimeOff(validDelete))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(validDelete))
}

func TestBookingEventCreateAndUpdateEnforceRecurringAvailability(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	// Deliberately differ from the resource timezone. Recurring windows follow
	// the selected resource's validated timezone, matching local event input.
	setBookingTestOrganizationTimezone(t, db, org, "UTC")
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "bookings.enabled")
	service, resource := createBookingAvailabilityTestServiceAndResource(t, db, org.ID, user.ID)
	effectiveFrom := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	effectiveUntil := effectiveFrom
	rule := models.AvailabilityRule{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		ResourceID: resource.ID, Weekday: int(time.Monday),
		StartLocalTime: "09:00", EndLocalTime: "17:00",
		EffectiveFrom: &effectiveFrom, EffectiveUntil: &effectiveUntil,
		IsActive: true, Version: 1, CreatedByID: &user.ID, UpdatedByID: &user.ID,
	}
	require.NoError(t, db.Create(&rule).Error)

	outside := BookingEventRequest{
		ServiceID: service.ID, ResourceID: resource.ID,
		StartsAt: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		EndsAt:   time.Date(2026, 8, 3, 0, 30, 0, 0, time.UTC),
		Capacity: 1,
	}
	outsideRequest := bookingEventAvailabilityRequest(t, outside, org.ID, user.ID)
	require.NoError(t, app.CreateBookingEvent(outsideRequest))
	testutil.AssertErrorResponse(t, outsideRequest, fasthttp.StatusConflict, "outside the resource's recurring availability")

	inHours := outside
	inHours.StartsAt = time.Date(2026, 8, 3, 1, 30, 0, 0, time.UTC)
	inHours.EndsAt = time.Date(2026, 8, 3, 2, 0, 0, 0, time.UTC)
	inHoursRequest := bookingEventAvailabilityRequest(t, inHours, org.ID, user.ID)
	require.NoError(t, app.CreateBookingEvent(inHoursRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(inHoursRequest))
	var created models.BookingEvent
	testutil.ParseEnvelopeResponse(t, inHoursRequest, &created)
	assert.EqualValues(t, 1, created.Version)

	updateOutside := outside
	updateOutside.Status = models.BookingEventStatusScheduled
	updateOutside.Version = created.Version
	updateRequest := bookingEventAvailabilityRequest(t, updateOutside, org.ID, user.ID)
	testutil.SetPathParam(updateRequest, "id", created.ID.String())
	require.NoError(t, app.UpdateBookingEvent(updateRequest))
	testutil.AssertErrorResponse(t, updateRequest, fasthttp.StatusConflict, "outside the resource's recurring availability")

	outsideEffectiveRange := inHours
	outsideEffectiveRange.StartsAt = time.Date(2026, 8, 10, 1, 30, 0, 0, time.UTC)
	outsideEffectiveRange.EndsAt = time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC)
	outOfRangeRequest := bookingEventAvailabilityRequest(t, outsideEffectiveRange, org.ID, user.ID)
	require.NoError(t, app.CreateBookingEvent(outOfRangeRequest))
	testutil.AssertErrorResponse(t, outOfRangeRequest, fasthttp.StatusConflict, "outside the resource's recurring availability")

	multiDay := inHours
	multiDay.StartsAt = time.Date(2026, 8, 3, 8, 30, 0, 0, time.UTC)
	multiDay.EndsAt = time.Date(2026, 8, 3, 16, 30, 0, 0, time.UTC)
	multiDayRequest := bookingEventAvailabilityRequest(t, multiDay, org.ID, user.ID)
	require.NoError(t, app.CreateBookingEvent(multiDayRequest))
	testutil.AssertErrorResponse(t, multiDayRequest, fasthttp.StatusConflict, "one resource-local calendar day")
}

func TestRecurringAvailabilityRejectsDSTOffsetTransition(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	setBookingTestOrganizationTimezone(t, db, org, "America/New_York")
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	resource := createBookingAvailabilityTestResource(t, db, org.ID, user.ID, "DST room")
	require.NoError(t, db.Model(&models.BookingResource{}).Where(
		"id = ? AND organization_id = ?",
		resource.ID,
		org.ID,
	).Update("timezone", "America/New_York").Error)
	effective := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rule := models.AvailabilityRule{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		ResourceID: resource.ID, Weekday: int(time.Sunday),
		StartLocalTime: "00:00", EndLocalTime: "04:00",
		EffectiveFrom: &effective, EffectiveUntil: &effective,
		IsActive: true, Version: 1,
	}
	require.NoError(t, db.Create(&rule).Error)

	err := ensureBookingEventWithinRecurringAvailability(
		db,
		org.ID,
		resource.ID,
		time.Date(2026, 11, 1, 4, 30, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC),
	)
	require.ErrorContains(t, err, "daylight-saving timezone transition")
}

func bookingAvailabilityRequest(
	t *testing.T,
	body any,
	orgID, userID, resourceID uuid.UUID,
) *fastglue.Request {
	t.Helper()
	var request *fastglue.Request
	if body == nil {
		request = testutil.NewGETRequest(t)
	} else {
		request = testutil.NewJSONRequest(t, body)
	}
	testutil.SetAuthContext(request, orgID, userID)
	testutil.SetPathParam(request, "resource_id", resourceID.String())
	return request
}

func bookingAvailabilityItemRequest(
	t *testing.T,
	body any,
	orgID, userID, resourceID, itemID uuid.UUID,
) *fastglue.Request {
	t.Helper()
	request := bookingAvailabilityRequest(t, body, orgID, userID, resourceID)
	testutil.SetPathParam(request, "id", itemID.String())
	return request
}

func bookingEventAvailabilityRequest(
	t *testing.T,
	body BookingEventRequest,
	orgID, userID uuid.UUID,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(request, orgID, userID)
	return request
}

func setBookingTestOrganizationTimezone(
	t *testing.T,
	db *gorm.DB,
	org *models.Organization,
	timezone string,
) {
	t.Helper()
	org.Settings = models.JSONB{"timezone": timezone}
	require.NoError(t, db.Model(&models.Organization{}).Where("id = ?", org.ID).
		Update("settings", org.Settings).Error)
}

func createBookingAvailabilityTestResource(
	t *testing.T,
	db *gorm.DB,
	orgID, userID uuid.UUID,
	name string,
) models.BookingResource {
	t.Helper()
	resource := models.BookingResource{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		Name: name + " " + uuid.NewString()[:8], Kind: models.BookingResourceKindRoom,
		Timezone: "Asia/Kuala_Lumpur", IsActive: true,
		Metadata: models.JSONB{}, Version: 1,
		CreatedByID: &userID, UpdatedByID: &userID,
	}
	require.NoError(t, db.Create(&resource).Error)
	return resource
}

func createBookingAvailabilityTestServiceAndResource(
	t *testing.T,
	db *gorm.DB,
	orgID, userID uuid.UUID,
) (models.BookingService, models.BookingResource) {
	t.Helper()
	service := models.BookingService{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		Name: "Availability service " + uuid.NewString()[:8],
		Kind: models.BookingServiceKindAppointment, DurationMinutes: 30,
		DefaultCapacity: 1, Currency: "MYR", IsActive: true,
		Metadata: models.JSONB{}, Version: 1,
		CreatedByID: &userID, UpdatedByID: &userID,
	}
	require.NoError(t, db.Create(&service).Error)
	resource := createBookingAvailabilityTestResource(t, db, orgID, userID, "Availability room")
	return service, resource
}

func createBookingAvailabilityTestEvent(
	t *testing.T,
	db *gorm.DB,
	orgID, userID, serviceID, resourceID uuid.UUID,
	startsAt, endsAt time.Time,
	status models.BookingEventStatus,
) models.BookingEvent {
	t.Helper()
	event := models.BookingEvent{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		ServiceID: serviceID, ResourceID: resourceID,
		StartsAt: startsAt, EndsAt: endsAt, Capacity: 1, Status: status,
		Metadata: models.JSONB{}, Version: 1,
		CreatedByID: &userID, UpdatedByID: &userID,
	}
	require.NoError(t, db.Create(&event).Error)
	return event
}
