package handlers

import (
	"encoding/json"
	"fmt"
	"math"
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

func TestBookingCommerceValidation(t *testing.T) {
	t.Parallel()

	service := BookingServiceRequest{
		Name:            "Consultation",
		Kind:            models.BookingServiceKindAppointment,
		DurationMinutes: 30,
		DefaultCapacity: 1,
		PriceMinor:      5000,
		Currency:        "myr",
	}
	require.NoError(t, validateBookingServiceRequest(&service, false))
	assert.Equal(t, "MYR", service.Currency)

	service.PriceMinor = -1
	require.EqualError(t, validateBookingServiceRequest(&service, false), "price_minor must be zero or greater")

	resource := BookingResourceRequest{
		Name:     "Room 1",
		Kind:     models.BookingResourceKindRoom,
		Timezone: "Not/A_Timezone",
	}
	require.EqualError(
		t,
		validateBookingResourceRequest(&resource, false),
		"timezone must be a valid IANA timezone",
	)

	now := time.Now().UTC()
	event := BookingEventRequest{
		ServiceID:  uuid.New(),
		ResourceID: uuid.New(),
		StartsAt:   now,
		EndsAt:     now,
		Capacity:   1,
	}
	require.EqualError(
		t,
		validateBookingEventRequest(&event, false),
		"starts_at and ends_at must define a valid increasing interval",
	)

	localEvent := BookingEventRequest{
		ServiceID:     uuid.New(),
		ResourceID:    uuid.New(),
		LocalStartsAt: "2026-07-27T09:00",
		LocalEndsAt:   "2026-07-27T10:00",
		Timezone:      "Asia/Kuala_Lumpur",
		Capacity:      1,
	}
	require.NoError(t, normalizeBookingEventRequestTimes(&localEvent))
	assert.Equal(t, "2026-07-27T01:00:00Z", localEvent.StartsAt.Format(time.RFC3339))
	assert.Equal(t, "2026-07-27T02:00:00Z", localEvent.EndsAt.Format(time.RFC3339))
	require.NoError(t, validateBookingEventRequest(&localEvent, false))
}

func TestBookingCommerceMoneyAndManualPaymentValidation(t *testing.T) {
	t.Parallel()

	value, err := safeMoneyMultiply(1250, 4)
	require.NoError(t, err)
	assert.Equal(t, int64(5000), value)
	_, err = safeMoneyMultiply(math.MaxInt64, 2)
	require.Error(t, err)
	_, err = safeMoneyAdd(math.MaxInt64, 1)
	require.Error(t, err)

	manual := RecordManualInvoicePaymentRequest{
		Version:        1,
		AmountMinor:    100,
		Currency:       "MYR",
		Reference:      "BANK-123",
		IdempotencyKey: "manual-123",
	}
	require.EqualError(
		t,
		validateRecordManualInvoicePaymentRequest(&manual),
		"confirm_manual must be true to record an external manual payment",
	)
	manual.ConfirmManual = true
	require.NoError(t, validateRecordManualInvoicePaymentRequest(&manual))

	invoice := CreateCommerceInvoiceRequest{
		ContactID:      uuid.New(),
		Currency:       "MYR",
		IdempotencyKey: "invoice-123",
		Lines: []CommerceInvoiceLineInput{{
			Description:     "Custom service",
			Quantity:        1,
			UnitAmountMinor: int64Pointer(100),
		}},
	}
	require.NoError(t, validateCreateCommerceInvoiceRequest(&invoice))
	invoice.IdempotencyKey = ""
	require.EqualError(
		t,
		validateCreateCommerceInvoiceRequest(&invoice),
		"idempotency_key is required and must not exceed 255 characters",
	)
}

func TestBookingTransitionPathMapping(t *testing.T) {
	t.Parallel()

	status, err := bookingStatusFromTransition("check-in")
	require.NoError(t, err)
	assert.Equal(t, models.BookingStatusCheckedIn, status)
	status, err = bookingStatusFromTransition("no_show")
	require.NoError(t, err)
	assert.Equal(t, models.BookingStatusNoShow, status)
	_, err = bookingStatusFromTransition("collect-payment")
	require.Error(t, err)
}

func TestBookingCommercePostgresCapacityAndCreditLedger(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	contact := testutil.CreateTestContact(t, db, org.ID)
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "commerce.enabled")

	service := models.BookingService{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		Name:            "Credit service " + uuid.NewString()[:8],
		Kind:            models.BookingServiceKindAppointment,
		DurationMinutes: 30,
		DefaultCapacity: 3,
		PriceMinor:      1000,
		Currency:        "MYR",
		IsActive:        true,
		Metadata:        models.JSONB{},
		Version:         1,
	}
	require.NoError(t, db.Create(&service).Error)
	resource := models.BookingResource{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Credit room " + uuid.NewString()[:8],
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
		Capacity:       3,
		Status:         models.BookingEventStatusScheduled,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, db.Create(&event).Error)

	definition := models.PackageDefinition{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "Three credits " + uuid.NewString()[:8],
		PriceMinor:     2500,
		Currency:       "MYR",
		ValidityDays:   30,
		IsActive:       true,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, db.Create(&definition).Error)
	entitlement := models.PackageEntitlement{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      org.ID,
		PackageDefinitionID: definition.ID,
		BookingServiceID:    service.ID,
		Credits:             3,
		Version:             1,
	}
	require.NoError(t, db.Create(&entitlement).Error)
	contactPackage := models.ContactPackage{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      org.ID,
		ContactID:           contact.ID,
		PackageDefinitionID: definition.ID,
		Status:              models.ContactPackageStatusActive,
		StartsAt:            &startsAt,
		ExpiresAt:           bookingCommerceTimePointer(startsAt.AddDate(0, 0, 30)),
		PurchaseAmountMinor: definition.PriceMinor,
		Currency:            definition.Currency,
		IdempotencyKey:      "cp-" + uuid.NewString(),
		Metadata:            models.JSONB{},
		Version:             1,
	}
	activationTime := time.Now().UTC()
	contactPackage.StartsAt = &activationTime
	require.NoError(t, db.Create(&contactPackage).Error)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return grantContactPackageCredits(
			tx,
			org.ID,
			&contactPackage,
			user.ID,
			activationTime,
			"test-grant",
		)
	}))

	booking := models.Booking{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   org.ID,
		EventID:          event.ID,
		ContactID:        contact.ID,
		Status:           models.BookingStatusReserved,
		Quantity:         2,
		Source:           models.BookingSourceAgent,
		ContactPackageID: &contactPackage.ID,
		IdempotencyKey:   "booking-" + uuid.NewString(),
		Metadata:         models.JSONB{},
		Version:          1,
	}
	require.NoError(t, db.Create(&booking).Error)
	waitlisted := models.Booking{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		EventID:        event.ID,
		ContactID:      contact.ID,
		Status:         models.BookingStatusWaitlisted,
		Quantity:       10,
		Source:         models.BookingSourceAgent,
		IdempotencyKey: "waitlist-" + uuid.NewString(),
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, db.Create(&waitlisted).Error)

	occupied, err := bookingEventOccupiedQuantity(db, org.ID, event.ID, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, 2, occupied)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := reserveBookingPackageCredit(
			tx, org.ID, &booking, service.ID, user.ID, activationTime,
		); err != nil {
			return err
		}
		return consumeBookingPackageCredit(
			tx, org.ID, &booking, service.ID, user.ID, activationTime,
		)
	}))

	var balance models.CreditBalance
	require.NoError(t, db.Where(
		"organization_id = ? AND contact_package_id = ?",
		org.ID, contactPackage.ID,
	).First(&balance).Error)
	assert.Equal(t, 3, balance.Granted)
	assert.Equal(t, 1, balance.Available)
	assert.Zero(t, balance.Reserved)
	assert.Equal(t, 2, balance.Consumed)

	var ledgerCount int64
	require.NoError(t, db.Model(&models.CreditLedgerEntry{}).
		Where("organization_id = ? AND contact_package_id = ?", org.ID, contactPackage.ID).
		Count(&ledgerCount).Error)
	assert.Equal(t, int64(3), ledgerCount)
}

func TestCreateBookingConcurrentRequestsCannotOversubscribeFinalCapacity(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "bookings.enabled")
	contacts := []*models.Contact{
		testutil.CreateTestContact(t, db, org.ID),
		testutil.CreateTestContact(t, db, org.ID),
	}
	event := createCapacityRaceBookingEvent(t, db, org.ID, user.ID, 1)

	requests := make([]*fastglue.Request, len(contacts))
	for i, contact := range contacts {
		requests[i] = testutil.NewJSONRequest(t, CreateBookingRequest{
			EventID:        event.ID,
			ContactID:      contact.ID,
			Status:         models.BookingStatusReserved,
			Quantity:       1,
			Source:         models.BookingSourceAgent,
			IdempotencyKey: "capacity-race-" + uuid.NewString(),
		})
		testutil.SetAuthContext(requests[i], org.ID, user.ID)
		testutil.SetPathParam(requests[i], "id", event.ID.String())
	}

	start := make(chan struct{})
	errs := make([]error, len(requests))
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			errs[index] = app.CreateBooking(requests[index])
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.ElementsMatch(t, []int{
		fasthttp.StatusOK,
		fasthttp.StatusConflict,
	}, []int{
		testutil.GetResponseStatusCode(requests[0]),
		testutil.GetResponseStatusCode(requests[1]),
	})
	occupied, err := bookingEventOccupiedQuantity(db, org.ID, event.ID, uuid.Nil)
	require.NoError(t, err)
	require.Equal(t, 1, occupied)
	var bookingCount int64
	require.NoError(t, db.Model(&models.Booking{}).Where(
		"organization_id = ? AND event_id = ?",
		org.ID,
		event.ID,
	).Count(&bookingCount).Error)
	require.EqualValues(t, 1, bookingCount)
	require.NoError(t, db.Where("id = ? AND organization_id = ?", event.ID, org.ID).
		First(&event).Error)
	require.EqualValues(t, 2, event.Version)
}

func TestTransitionBookingConcurrentWaitlistPromotionsRespectCapacity(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "bookings.enabled")
	event := createCapacityRaceBookingEvent(t, db, org.ID, user.ID, 1)
	bookings := make([]models.Booking, 2)
	for i := range bookings {
		contact := testutil.CreateTestContact(t, db, org.ID)
		bookings[i] = models.Booking{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
			EventID: event.ID, ContactID: contact.ID,
			Status: models.BookingStatusWaitlisted, Quantity: 1,
			Source:         models.BookingSourceAgent,
			IdempotencyKey: "waitlist-promotion-" + uuid.NewString(),
			Metadata:       models.JSONB{}, Version: 1,
		}
		require.NoError(t, db.Create(&bookings[i]).Error)
	}

	requests := make([]*fastglue.Request, len(bookings))
	for i := range bookings {
		requests[i] = testutil.NewJSONRequest(t, TransitionBookingRequest{
			Version: 1,
			Status:  models.BookingStatusConfirmed,
		})
		testutil.SetAuthContext(requests[i], org.ID, user.ID)
		testutil.SetPathParam(requests[i], "id", bookings[i].ID.String())
		testutil.SetPathParam(requests[i], "transition", "confirm")
	}

	start := make(chan struct{})
	errs := make([]error, len(requests))
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			errs[index] = app.TransitionBooking(requests[index])
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.ElementsMatch(t, []int{
		fasthttp.StatusOK,
		fasthttp.StatusConflict,
	}, []int{
		testutil.GetResponseStatusCode(requests[0]),
		testutil.GetResponseStatusCode(requests[1]),
	})
	var confirmed, waitlisted int64
	require.NoError(t, db.Model(&models.Booking{}).Where(
		"organization_id = ? AND event_id = ? AND status = ?",
		org.ID,
		event.ID,
		models.BookingStatusConfirmed,
	).Count(&confirmed).Error)
	require.NoError(t, db.Model(&models.Booking{}).Where(
		"organization_id = ? AND event_id = ? AND status = ?",
		org.ID,
		event.ID,
		models.BookingStatusWaitlisted,
	).Count(&waitlisted).Error)
	require.EqualValues(t, 1, confirmed)
	require.EqualValues(t, 1, waitlisted)
}

func TestCreateBookingConcurrentRequestsCannotDoubleReservePackageCredit(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "bookings.enabled")
	contact := testutil.CreateTestContact(t, db, org.ID)
	firstEvent := createCapacityRaceBookingEvent(t, db, org.ID, user.ID, 1)
	secondResource := models.BookingResource{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		Name: "Second credit-race room " + uuid.NewString()[:8],
		Kind: models.BookingResourceKindRoom, Timezone: "Asia/Kuala_Lumpur",
		IsActive: true, Metadata: models.JSONB{}, Version: 1,
		CreatedByID: &user.ID, UpdatedByID: &user.ID,
	}
	require.NoError(t, db.Create(&secondResource).Error)
	secondStart := firstEvent.StartsAt.Add(time.Hour)
	secondEvent := models.BookingEvent{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		ServiceID: firstEvent.ServiceID, ResourceID: secondResource.ID,
		StartsAt: secondStart, EndsAt: secondStart.Add(30 * time.Minute),
		Capacity: 1, Status: models.BookingEventStatusScheduled,
		Metadata: models.JSONB{}, Version: 1,
		CreatedByID: &user.ID, UpdatedByID: &user.ID,
	}
	require.NoError(t, db.Create(&secondEvent).Error)

	definition := models.PackageDefinition{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		Name: "Single credit " + uuid.NewString()[:8], PriceMinor: 100,
		Currency: "MYR", ValidityDays: 30, IsActive: true,
		Metadata: models.JSONB{}, Version: 1,
		CreatedByID: &user.ID, UpdatedByID: &user.ID,
	}
	require.NoError(t, db.Create(&definition).Error)
	entitlement := models.PackageEntitlement{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		PackageDefinitionID: definition.ID, BookingServiceID: firstEvent.ServiceID,
		Credits: 1, Version: 1,
	}
	require.NoError(t, db.Create(&entitlement).Error)
	now := time.Now().UTC()
	contactPackage := models.ContactPackage{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		ContactID: contact.ID, PackageDefinitionID: definition.ID,
		Status: models.ContactPackageStatusActive, StartsAt: &now,
		ExpiresAt:           bookingCommerceTimePointer(now.AddDate(0, 0, 30)),
		PurchaseAmountMinor: definition.PriceMinor, Currency: definition.Currency,
		IdempotencyKey: "single-credit-" + uuid.NewString(),
		Metadata:       models.JSONB{}, Version: 1,
		CreatedByID: &user.ID, UpdatedByID: &user.ID,
	}
	require.NoError(t, db.Create(&contactPackage).Error)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return grantContactPackageCredits(
			tx,
			org.ID,
			&contactPackage,
			user.ID,
			now,
			"single-credit-grant",
		)
	}))

	events := []models.BookingEvent{firstEvent, secondEvent}
	requests := make([]*fastglue.Request, len(events))
	for i := range events {
		requests[i] = testutil.NewJSONRequest(t, CreateBookingRequest{
			EventID: events[i].ID, ContactID: contact.ID,
			Status: models.BookingStatusReserved, Quantity: 1,
			Source: models.BookingSourceAgent, ContactPackageID: &contactPackage.ID,
			IdempotencyKey: "credit-race-booking-" + uuid.NewString(),
		})
		testutil.SetAuthContext(requests[i], org.ID, user.ID)
		testutil.SetPathParam(requests[i], "id", events[i].ID.String())
	}

	start := make(chan struct{})
	errs := make([]error, len(requests))
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			errs[index] = app.CreateBooking(requests[index])
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.ElementsMatch(t, []int{
		fasthttp.StatusOK,
		fasthttp.StatusConflict,
	}, []int{
		testutil.GetResponseStatusCode(requests[0]),
		testutil.GetResponseStatusCode(requests[1]),
	})
	var balance models.CreditBalance
	require.NoError(t, db.Where(
		"organization_id = ? AND contact_package_id = ? AND package_entitlement_id = ?",
		org.ID,
		contactPackage.ID,
		entitlement.ID,
	).First(&balance).Error)
	require.Equal(t, 1, balance.Granted)
	require.Zero(t, balance.Available)
	require.Equal(t, 1, balance.Reserved)
	require.Zero(t, balance.Consumed)
	var reservationCount int64
	require.NoError(t, db.Model(&models.CreditLedgerEntry{}).Where(
		"organization_id = ? AND contact_package_id = ? AND type = ?",
		org.ID,
		contactPackage.ID,
		models.CreditLedgerEntryTypeReserve,
	).Count(&reservationCount).Error)
	require.EqualValues(t, 1, reservationCount)
}

func TestBookingCommercePostgresManualPaymentAtomicIdempotency(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	contact := testutil.CreateTestContact(t, db, org.ID)
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "commerce.enabled")

	invoice := models.CommerceInvoice{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		ContactID:      contact.ID,
		InvoiceNumber:  "INV-TEST-" + uuid.NewString()[:8],
		IdempotencyKey: "invoice-" + uuid.NewString(),
		Status:         models.CommerceInvoiceStatusOpen,
		Currency:       "MYR",
		SubtotalMinor:  100,
		TotalMinor:     100,
		DueMinor:       100,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, db.Create(&invoice).Error)

	newRequest := func(version, amount int64, key, reference string) *fastglue.Request {
		request := testutil.NewJSONRequest(t, RecordManualInvoicePaymentRequest{
			Version:        version,
			AmountMinor:    amount,
			Currency:       "MYR",
			Reference:      reference,
			IdempotencyKey: key,
			ConfirmManual:  true,
		})
		testutil.SetAuthContext(request, org.ID, user.ID)
		testutil.SetPathParam(request, "id", invoice.ID.String())
		return request
	}

	first := newRequest(1, 40, "payment-first", "BANK-001")
	require.NoError(t, app.RecordManualInvoicePayment(first))

	var updated models.CommerceInvoice
	require.NoError(t, db.Where("id = ? AND organization_id = ?", invoice.ID, org.ID).
		First(&updated).Error)
	assert.Equal(t, int64(40), updated.PaidMinor)
	assert.Equal(t, int64(60), updated.DueMinor)
	assert.Equal(t, int64(2), updated.Version)
	assert.Equal(t, models.CommerceInvoiceStatusOpen, updated.Status)

	replay := newRequest(1, 40, "payment-first", "BANK-001")
	require.NoError(t, app.RecordManualInvoicePayment(replay))
	var transactionCount int64
	require.NoError(t, db.Model(&models.PaymentTransaction{}).
		Where("organization_id = ? AND invoice_id = ?", org.ID, invoice.ID).
		Count(&transactionCount).Error)
	assert.Equal(t, int64(1), transactionCount)

	duplicateReference := newRequest(2, 10, "payment-second", "bank-001")
	require.NoError(t, app.RecordManualInvoicePayment(duplicateReference))
	testutil.AssertErrorResponse(
		t,
		duplicateReference,
		fasthttp.StatusConflict,
		"Manual payment reference has already been recorded",
	)

	finalPayment := newRequest(2, 60, "payment-final", "BANK-002")
	require.NoError(t, app.RecordManualInvoicePayment(finalPayment))
	require.NoError(t, db.Where("id = ? AND organization_id = ?", invoice.ID, org.ID).
		First(&updated).Error)
	assert.Equal(t, int64(100), updated.PaidMinor)
	assert.Zero(t, updated.DueMinor)
	assert.Equal(t, int64(3), updated.Version)
	assert.Equal(t, models.CommerceInvoiceStatusPaid, updated.Status)
	require.NotNil(t, updated.PaidAt)

	require.NoError(t, db.Model(&models.PaymentTransaction{}).
		Where("organization_id = ? AND invoice_id = ?", org.ID, invoice.ID).
		Count(&transactionCount).Error)
	assert.Equal(t, int64(2), transactionCount)
}

func TestRecordManualInvoicePaymentConcurrentDuplicateReferenceCommitsOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "commerce.enabled")
	account := models.PaymentProviderAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		Name: "Manual race account", Provider: "manual",
		ExternalAccountID: "manual-race-" + uuid.NewString(),
		Environment:       models.PaymentEnvironmentLive, PublicConfig: models.JSONB{},
		IsActive: true, Metadata: models.JSONB{}, Version: 1,
		CreatedByID: &user.ID, UpdatedByID: &user.ID,
	}
	require.NoError(t, db.Create(&account).Error)

	invoices := make([]models.CommerceInvoice, 2)
	for i := range invoices {
		contact := testutil.CreateTestContact(t, db, org.ID)
		invoices[i] = models.CommerceInvoice{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
			ContactID: contact.ID, InvoiceNumber: "INV-RACE-" + uuid.NewString()[:8],
			IdempotencyKey: "invoice-race-" + uuid.NewString(),
			Status:         models.CommerceInvoiceStatusOpen, Currency: "MYR",
			SubtotalMinor: 50, TotalMinor: 50, DueMinor: 50,
			Metadata: models.JSONB{}, Version: 1,
		}
		require.NoError(t, db.Create(&invoices[i]).Error)
	}

	references := []string{"BANK-RACE-001", "bank-race-001"}
	requests := make([]*fastglue.Request, len(invoices))
	for i := range invoices {
		requests[i] = testutil.NewJSONRequest(t, RecordManualInvoicePaymentRequest{
			Version: 1, ProviderAccountID: &account.ID,
			AmountMinor: 50, Currency: "MYR", Reference: references[i],
			IdempotencyKey: "payment-race-" + uuid.NewString(), ConfirmManual: true,
		})
		testutil.SetAuthContext(requests[i], org.ID, user.ID)
		testutil.SetPathParam(requests[i], "id", invoices[i].ID.String())
	}

	start := make(chan struct{})
	errs := make([]error, len(requests))
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			errs[index] = app.RecordManualInvoicePayment(requests[index])
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.ElementsMatch(t, []int{
		fasthttp.StatusOK,
		fasthttp.StatusConflict,
	}, []int{
		testutil.GetResponseStatusCode(requests[0]),
		testutil.GetResponseStatusCode(requests[1]),
	})
	var paymentCount int64
	require.NoError(t, db.Model(&models.PaymentTransaction{}).Where(
		"organization_id = ? AND provider_account_id = ? AND LOWER(provider_transaction_id) = LOWER(?)",
		org.ID,
		account.ID,
		references[0],
	).Count(&paymentCount).Error)
	require.EqualValues(t, 1, paymentCount)
	var paidTotal int64
	require.NoError(t, db.Model(&models.CommerceInvoice{}).Where(
		"organization_id = ? AND id IN ?",
		org.ID,
		[]uuid.UUID{invoices[0].ID, invoices[1].ID},
	).Select("COALESCE(SUM(paid_minor), 0)").Scan(&paidTotal).Error)
	require.EqualValues(t, 50, paidTotal)
}

func TestSellContactPackageTransactionIsAtomicAndIdempotent(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	definition := models.PackageDefinition{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Name:           "Recovery plan",
		PriceMinor:     15000,
		Currency:       "MYR",
		ValidityDays:   30,
		IsActive:       true,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, db.Create(&definition).Error)

	request := SellContactPackageRequest{
		ContactID:           contact.ID,
		PackageDefinitionID: definition.ID,
		IdempotencyKey:      "sale-" + uuid.NewString(),
	}
	require.NoError(t, validateSellContactPackageRequest(&request))
	fingerprint, err := sellContactPackageRequestFingerprint(&request)
	require.NoError(t, err)

	var first SellContactPackageResponse
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var transactionErr error
		first, transactionErr = sellContactPackageTransaction(
			tx,
			organization.ID,
			user.ID,
			&request,
			fingerprint,
		)
		return transactionErr
	}))
	assert.Equal(t, models.CommerceInvoiceStatusOpen, first.Invoice.Status)
	assert.Equal(t, models.ContactPackageStatusPending, first.ContactPackage.Status)
	require.NotNil(t, first.ContactPackage.InvoiceID)
	assert.Equal(t, first.Invoice.ID, *first.ContactPackage.InvoiceID)

	var replay SellContactPackageResponse
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var transactionErr error
		replay, transactionErr = sellContactPackageTransaction(
			tx,
			organization.ID,
			user.ID,
			&request,
			fingerprint,
		)
		return transactionErr
	}))
	assert.Equal(t, first.Invoice.ID, replay.Invoice.ID)
	assert.Equal(t, first.ContactPackage.ID, replay.ContactPackage.ID)

	var invoiceCount int64
	var contactPackageCount int64
	require.NoError(t, db.Model(&models.CommerceInvoice{}).
		Where("organization_id = ?", organization.ID).
		Count(&invoiceCount).Error)
	require.NoError(t, db.Model(&models.ContactPackage{}).
		Where("organization_id = ?", organization.ID).
		Count(&contactPackageCount).Error)
	assert.EqualValues(t, 1, invoiceCount)
	assert.EqualValues(t, 1, contactPackageCount)
}

func TestSellContactPackageTransactionRollsBackBothRecordsOnFailure(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	definition := models.PackageDefinition{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Name:           "Rollback plan",
		PriceMinor:     100,
		Currency:       "MYR",
		ValidityDays:   30,
		IsActive:       true,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, db.Create(&definition).Error)

	request := SellContactPackageRequest{
		ContactID:           contact.ID,
		PackageDefinitionID: definition.ID,
		IdempotencyKey:      "rollback-" + uuid.NewString(),
	}
	fingerprint, err := sellContactPackageRequestFingerprint(&request)
	require.NoError(t, err)
	err = db.Transaction(func(tx *gorm.DB) error {
		_, transactionErr := sellContactPackageTransaction(
			tx,
			organization.ID,
			user.ID,
			&request,
			fingerprint,
		)
		if transactionErr != nil {
			return transactionErr
		}
		return assert.AnError
	})
	require.ErrorIs(t, err, assert.AnError)

	var invoiceCount int64
	var contactPackageCount int64
	require.NoError(t, db.Model(&models.CommerceInvoice{}).
		Where("organization_id = ?", organization.ID).
		Count(&invoiceCount).Error)
	require.NoError(t, db.Model(&models.ContactPackage{}).
		Where("organization_id = ?", organization.ID).
		Count(&contactPackageCount).Error)
	assert.Zero(t, invoiceCount)
	assert.Zero(t, contactPackageCount)
}

func TestBookingCommerceCreateActiveStatePersistsAndAudits(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "bookings.enabled")
	event := createCapacityRaceBookingEvent(t, db, org.ID, user.ID, 1)
	active, inactive := true, false
	for _, tc := range []struct {
		name  string
		input *bool
		want  bool
	}{
		{"omitted", nil, true}, {"true", &active, true}, {"false", &inactive, false},
	} {
		t.Run("service_"+tc.name, func(t *testing.T) {
			req := testutil.NewJSONRequest(t, BookingServiceRequest{
				Name: "Create state service " + uuid.NewString(), Kind: models.BookingServiceKindAppointment,
				DurationMinutes: 30, DefaultCapacity: 1, Currency: "MYR",
				IsActive: tc.input, ResourceIDs: []uuid.UUID{event.ResourceID},
			})
			testutil.SetAuthContext(req, org.ID, user.ID)
			require.NoError(t, app.CreateBookingService(req))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
			var response BookingServiceResponse
			testutil.ParseEnvelopeResponse(t, req, &response)
			var stored models.BookingService
			require.NoError(t, db.First(&stored, "id = ? AND organization_id = ?", response.ID, org.ID).Error)
			require.Equal(t, tc.want, stored.IsActive)
			require.Equal(t, stored.IsActive, response.IsActive)
			require.EqualValues(t, 1, stored.Version)
			var links []models.BookingServiceResource
			require.NoError(t, db.Where("organization_id = ? AND service_id = ?", org.ID, stored.ID).Find(&links).Error)
			require.Len(t, links, 1)
			require.Equal(t, event.ResourceID, links[0].ResourceID)
			require.Equal(t, []uuid.UUID{event.ResourceID}, response.ResourceIDs)
			assertBookingCommerceAuditActive(t, db, org.ID, stored.ID, models.AuditActionCreated, nil, tc.want)
		})
		t.Run("resource_"+tc.name, func(t *testing.T) {
			req := testutil.NewJSONRequest(t, BookingResourceRequest{
				Name: "Create state room " + uuid.NewString(), Kind: models.BookingResourceKindRoom,
				Timezone: "Asia/Kuala_Lumpur", IsActive: tc.input,
			})
			testutil.SetAuthContext(req, org.ID, user.ID)
			require.NoError(t, app.CreateBookingResource(req))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
			var response models.BookingResource
			testutil.ParseEnvelopeResponse(t, req, &response)
			var stored models.BookingResource
			require.NoError(t, db.First(&stored, "id = ? AND organization_id = ?", response.ID, org.ID).Error)
			require.Equal(t, tc.want, stored.IsActive)
			require.Equal(t, stored.IsActive, response.IsActive)
			require.EqualValues(t, 1, stored.Version)
			assertBookingCommerceAuditActive(t, db, org.ID, stored.ID, models.AuditActionCreated, nil, tc.want)
		})
	}
}

func TestBookingCommerceCreateInactiveFailureRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name, table     string
		service, update bool
	}{
		{"service_active_update", "booking_services", true, true},
		{"service_links", "booking_service_resources", true, false},
		{"service_audit", "audit_logs", true, false},
		{"resource_active_update", "booking_resources", false, true},
		{"resource_audit", "audit_logs", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			app := &App{DB: db, Log: testutil.NopLogger()}
			org := testutil.CreateTestOrganization(t, db)
			user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
			enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "bookings.enabled")
			event := createCapacityRaceBookingEvent(t, db, org.ID, user.ID, 1)
			tables := []string{"booking_services", "booking_resources", "booking_service_resources", "audit_logs"}
			before := bookingCommerceRowsSnapshot(t, db, org.ID, tables...)
			callbackName := "test:booking_create_failure:" + uuid.NewString()
			reached := false
			callback := func(tx *gorm.DB) {
				if tx.Statement.Table == tc.table {
					reached = true
					tx.AddError(assert.AnError)
				}
			}
			if tc.update {
				require.NoError(t, db.Callback().Update().After("gorm:update").Register(callbackName, callback))
				t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callbackName)) })
			} else {
				require.NoError(t, db.Callback().Create().After("gorm:create").Register(callbackName, callback))
				t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove(callbackName)) })
			}
			inactive := false
			var req *fastglue.Request
			if tc.service {
				req = testutil.NewJSONRequest(t, BookingServiceRequest{
					Name: "Rollback service " + uuid.NewString(), Kind: models.BookingServiceKindAppointment,
					DurationMinutes: 30, DefaultCapacity: 1, Currency: "MYR",
					IsActive: &inactive, ResourceIDs: []uuid.UUID{event.ResourceID},
				})
				testutil.SetAuthContext(req, org.ID, user.ID)
				require.NoError(t, app.CreateBookingService(req))
			} else {
				req = testutil.NewJSONRequest(t, BookingResourceRequest{
					Name: "Rollback room " + uuid.NewString(), Kind: models.BookingResourceKindRoom,
					Timezone: "Asia/Kuala_Lumpur", IsActive: &inactive,
				})
				testutil.SetAuthContext(req, org.ID, user.ID)
				require.NoError(t, app.CreateBookingResource(req))
			}
			require.True(t, reached, "the intended transactional failure point must be reached")
			require.Equal(t, fasthttp.StatusInternalServerError, testutil.GetResponseStatusCode(req))
			require.Equal(t, before, bookingCommerceRowsSnapshot(t, db, org.ID, tables...))
		})
	}
}

func TestUpdatePackagePermissionMatrix(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for mask := 0; mask < 8; mask++ {
		keys := []string{}
		for bit, action := range []string{models.ActionRead, models.ActionWrite, models.ActionDelete} {
			if mask&(1<<bit) != 0 {
				keys = append(keys, models.ResourcePackages+":"+action)
			}
		}
		for _, operation := range []string{"edit_omitted", "edit_true", "retire"} {
			t.Run(fmt.Sprintf("permissions_%03b/%s", mask, operation), func(t *testing.T) {
				app := &App{DB: db, Log: testutil.NopLogger()}
				org := testutil.CreateTestOrganization(t, db)
				role := createBookingCommerceTestRoleWithKeys(t, db, org.ID, "package-permissions", keys)
				user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
				enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "commerce.enabled")
				pkg := createBookingCommercePackageFixture(t, db, org.ID)
				payload := bookingCommercePackageEditPayload(pkg)
				payload.Description = "Edited through the versioned endpoint"
				active := operation != "retire"
				if operation != "edit_omitted" {
					payload.IsActive = &active
				}
				before := bookingCommerceRowsSnapshot(t, db, org.ID, "package_definitions", "package_entitlements", "audit_logs")
				req := newBookingCommercePackageUpdate(t, org.ID, user.ID, pkg.ID, payload)
				require.NoError(t, app.UpdatePackage(req))
				allowed := mask&2 != 0 && (operation != "retire" || mask&4 != 0)
				if !allowed {
					testutil.AssertErrorResponse(t, req, fasthttp.StatusForbidden, "Insufficient permissions")
					require.Equal(t, before, bookingCommerceRowsSnapshot(t, db, org.ID, "package_definitions", "package_entitlements", "audit_logs"))
					return
				}
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
				var stored models.PackageDefinition
				require.NoError(t, db.First(&stored, "id = ?", pkg.ID).Error)
				require.EqualValues(t, 2, stored.Version)
				require.Equal(t, active, stored.IsActive)
				require.Equal(t, payload.Description, stored.Description)
				if operation == "retire" {
					assertBookingCommerceAuditActive(t, db, org.ID, pkg.ID, models.AuditActionUpdated, true, false)
				}
			})
		}
	}
}

func TestUpdatePackageRejectsTenantLicenseVersionAndPreservesPurchasedEntitlements(t *testing.T) {
	for _, scenario := range []string{"tenant", "license", "version", "purchased_entitlements", "agent"} {
		t.Run(scenario, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			app := &App{DB: db, Log: testutil.NopLogger()}
			org := testutil.CreateTestOrganization(t, db)
			actorOrg := org
			if scenario == "tenant" {
				actorOrg = testutil.CreateTestOrganization(t, db)
			}
			keys := []string{"packages:read", "packages:write", "packages:delete"}
			if scenario == "agent" {
				keys = models.SystemRolePermissions()["agent"]
			}
			role := createBookingCommerceTestRoleWithKeys(t, db, actorOrg.ID, "package-rejection", keys)
			user := testutil.CreateTestUser(t, db, actorOrg.ID, testutil.WithRoleID(&role.ID))
			if scenario != "license" {
				enableBookingCommerceTestEntitlement(t, db, actorOrg.ID, user.ID, "commerce.enabled")
			}
			pkg := createBookingCommercePackageFixture(t, db, org.ID)
			payload := bookingCommercePackageEditPayload(pkg)
			inactive := false
			payload.IsActive = &inactive
			expectedStatus, expectedMessage := fasthttp.StatusConflict, "Package was modified"
			switch scenario {
			case "tenant":
				expectedStatus, expectedMessage = fasthttp.StatusNotFound, "Package not found"
			case "license":
				expectedStatus, expectedMessage = fasthttp.StatusPaymentRequired, "Feature is not included"
			case "agent":
				expectedStatus, expectedMessage = fasthttp.StatusForbidden, "Insufficient permissions"
			case "version":
				payload.Version++
			case "purchased_entitlements":
				contact := testutil.CreateTestContact(t, db, org.ID)
				purchase := models.ContactPackage{
					BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
					ContactID: contact.ID, PackageDefinitionID: pkg.ID,
					Status: models.ContactPackageStatusActive, Currency: "MYR",
					IdempotencyKey: "purchased-" + uuid.NewString(), Version: 1,
				}
				require.NoError(t, db.Create(&purchase).Error)
				payload.Entitlements = []PackageEntitlementInput{{BookingServiceID: uuid.New(), Credits: 5}}
				expectedMessage = "Package entitlements cannot change"
			}
			tables := []string{"package_definitions", "package_entitlements", "contact_packages", "audit_logs"}
			before := bookingCommerceRowsSnapshot(t, db, org.ID, tables...)
			req := newBookingCommercePackageUpdate(t, actorOrg.ID, user.ID, pkg.ID, payload)
			require.NoError(t, app.UpdatePackage(req))
			testutil.AssertErrorResponse(t, req, expectedStatus, expectedMessage)
			require.Equal(t, before, bookingCommerceRowsSnapshot(t, db, org.ID, tables...))
		})
	}
}

func TestUpdatePackageConcurrentEditAndRetirementCommitOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "commerce.enabled")
	pkg := createBookingCommercePackageFixture(t, db, org.ID)
	edit, retire := bookingCommercePackageEditPayload(pkg), bookingCommercePackageEditPayload(pkg)
	edit.Description = "Concurrent edit"
	inactive := false
	retire.IsActive = &inactive
	requests := []*fastglue.Request{
		newBookingCommercePackageUpdate(t, org.ID, user.ID, pkg.ID, edit),
		newBookingCommercePackageUpdate(t, org.ID, user.ID, pkg.ID, retire),
	}
	start := make(chan struct{})
	errs := make([]error, len(requests))
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			errs[index] = app.UpdatePackage(requests[index])
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.ElementsMatch(t, []int{fasthttp.StatusOK, fasthttp.StatusConflict}, []int{
		testutil.GetResponseStatusCode(requests[0]), testutil.GetResponseStatusCode(requests[1]),
	})
	var stored models.PackageDefinition
	require.NoError(t, db.First(&stored, "id = ?", pkg.ID).Error)
	require.EqualValues(t, 2, stored.Version)
	if testutil.GetResponseStatusCode(requests[0]) == fasthttp.StatusOK {
		require.True(t, stored.IsActive)
		require.Equal(t, edit.Description, stored.Description)
	} else {
		require.False(t, stored.IsActive)
		require.Equal(t, pkg.Description, stored.Description)
	}
	var auditCount int64
	require.NoError(t, db.Model(&models.AuditLog{}).
		Where("organization_id = ? AND resource_id = ?", org.ID, pkg.ID).Count(&auditCount).Error)
	require.EqualValues(t, 1, auditCount)
}

func TestPackageRetirementPreservesHistoryCreditsAndCompletedReplay(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithSuperAdmin())
	contact := testutil.CreateTestContact(t, db, org.ID)
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "commerce.enabled")
	require.NoError(t, db.Model(&models.Subscription{}).Where("organization_id = ?", org.ID).
		Update("entitlements_snapshot", models.JSONB{"commerce.enabled": true, "bookings.enabled": true}).Error)
	event := createCapacityRaceBookingEvent(t, db, org.ID, user.ID, 3)
	pkg := createBookingCommercePackageFixture(t, db, org.ID)
	entitlement := models.PackageEntitlement{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID,
		PackageDefinitionID: pkg.ID, BookingServiceID: event.ServiceID, Credits: 3, Version: 1,
	}
	require.NoError(t, db.Create(&entitlement).Error)
	grantPayload := CreateContactPackageRequest{
		ContactID: contact.ID, PackageDefinitionID: pkg.ID, IdempotencyKey: "grant-" + uuid.NewString(),
	}
	grantRequest := testutil.NewJSONRequest(t, grantPayload)
	testutil.SetAuthContext(grantRequest, org.ID, user.ID)
	require.NoError(t, app.CreateContactPackage(grantRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(grantRequest))
	var grant models.ContactPackage
	testutil.ParseEnvelopeResponse(t, grantRequest, &grant)
	salePayload := SellContactPackageRequest{
		ContactID: contact.ID, PackageDefinitionID: pkg.ID, IdempotencyKey: "sale-" + uuid.NewString(),
	}
	saleRequest := testutil.NewJSONRequest(t, salePayload)
	testutil.SetAuthContext(saleRequest, org.ID, user.ID)
	require.NoError(t, app.SellContactPackage(saleRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(saleRequest))
	var sale SellContactPackageResponse
	testutil.ParseEnvelopeResponse(t, saleRequest, &sale)
	bookingPayload := CreateBookingRequest{
		EventID: event.ID, ContactID: contact.ID, ContactPackageID: &grant.ID,
		Status: models.BookingStatusReserved, Quantity: 1, Source: models.BookingSourceAgent,
		IdempotencyKey: "booking-before-retire-" + uuid.NewString(),
	}
	bookingRequest := testutil.NewJSONRequest(t, bookingPayload)
	testutil.SetAuthContext(bookingRequest, org.ID, user.ID)
	testutil.SetPathParam(bookingRequest, "id", event.ID.String())
	require.NoError(t, app.CreateBooking(bookingRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(bookingRequest))
	tables := []string{"package_entitlements", "contact_packages", "credit_balances", "credit_ledger_entries",
		"commerce_invoices", "invoice_lines", "bookings", "booking_events", "subscriptions", "customer_activity_events"}
	before := bookingCommerceRowsSnapshot(t, db, org.ID, tables...)
	var historicalAudit []models.AuditLog
	require.NoError(t, db.Where("organization_id = ?", org.ID).Find(&historicalAudit).Error)
	require.NotEmpty(t, historicalAudit)
	retirePayload := bookingCommercePackageEditPayload(pkg)
	inactive := false
	retirePayload.IsActive = &inactive
	retireRequest := newBookingCommercePackageUpdate(t, org.ID, user.ID, pkg.ID, retirePayload)
	require.NoError(t, app.UpdatePackage(retireRequest))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retireRequest))
	require.Equal(t, before, bookingCommerceRowsSnapshot(t, db, org.ID, tables...))
	for _, previous := range historicalAudit {
		var current models.AuditLog
		require.NoError(t, db.First(&current, "id = ?", previous.ID).Error)
		require.Equal(t, previous, current)
	}
	assertBookingCommerceAuditActive(t, db, org.ID, pkg.ID, models.AuditActionUpdated, true, false)
	var retired models.PackageDefinition
	require.NoError(t, db.First(&retired, "id = ?", pkg.ID).Error)
	require.False(t, retired.IsActive)
	require.EqualValues(t, 2, retired.Version)

	// Repeating the versioned update is a stale conflict, never a second mutation.
	frozen := bookingCommerceRowsSnapshot(t, db, org.ID, append(tables, "package_definitions", "audit_logs")...)
	stale := newBookingCommercePackageUpdate(t, org.ID, user.ID, pkg.ID, retirePayload)
	require.NoError(t, app.UpdatePackage(stale))
	testutil.AssertErrorResponse(t, stale, fasthttp.StatusConflict, "Package was modified")
	require.Equal(t, frozen, bookingCommerceRowsSnapshot(t, db, org.ID, append(tables, "package_definitions", "audit_logs")...))

	// Completed sale and grant requests replay after retirement; new keys fail.
	for _, replay := range []bool{true, false} {
		saleInput, grantInput := salePayload, grantPayload
		if !replay {
			saleInput.IdempotencyKey = "new-sale-" + uuid.NewString()
			grantInput.IdempotencyKey = "new-grant-" + uuid.NewString()
		}
		saleReq := testutil.NewJSONRequest(t, saleInput)
		testutil.SetAuthContext(saleReq, org.ID, user.ID)
		require.NoError(t, app.SellContactPackage(saleReq))
		grantReq := testutil.NewJSONRequest(t, grantInput)
		testutil.SetAuthContext(grantReq, org.ID, user.ID)
		require.NoError(t, app.CreateContactPackage(grantReq))
		if replay {
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(saleReq))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(grantReq))
			var saleReplay SellContactPackageResponse
			var grantReplay models.ContactPackage
			testutil.ParseEnvelopeResponse(t, saleReq, &saleReplay)
			testutil.ParseEnvelopeResponse(t, grantReq, &grantReplay)
			require.Equal(t, sale.Invoice.ID, saleReplay.Invoice.ID)
			require.Equal(t, sale.ContactPackage.ID, saleReplay.ContactPackage.ID)
			require.Equal(t, grant.ID, grantReplay.ID)
		} else {
			testutil.AssertErrorResponse(t, saleReq, fasthttp.StatusBadRequest, "active package")
			testutil.AssertErrorResponse(t, grantReq, fasthttp.StatusBadRequest, "active package")
		}
		require.Equal(t, frozen, bookingCommerceRowsSnapshot(t, db, org.ID, append(tables, "package_definitions", "audit_logs")...))
	}
	invoiceReq := testutil.NewJSONRequest(t, CreateCommerceInvoiceRequest{
		ContactID: contact.ID, Currency: "MYR", IdempotencyKey: "retired-invoice-" + uuid.NewString(),
		Lines: []CommerceInvoiceLineInput{{PackageDefinitionID: &pkg.ID, Quantity: 1}},
	})
	testutil.SetAuthContext(invoiceReq, org.ID, user.ID)
	require.NoError(t, app.CreateCommerceInvoice(invoiceReq))
	testutil.AssertErrorResponse(t, invoiceReq, fasthttp.StatusBadRequest, "active tenant package")
	require.Equal(t, frozen, bookingCommerceRowsSnapshot(t, db, org.ID, append(tables, "package_definitions", "audit_logs")...))

	// Write-only commercial edits leave a retired definition retired.
	role := createBookingCommerceTestRoleWithKeys(t, db, org.ID, "package-editor", []string{"packages:write"})
	editor := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
	editPayload := bookingCommercePackageEditPayload(retired)
	editPayload.Description = "Retired historical plan terms"
	edit := newBookingCommercePackageUpdate(t, org.ID, editor.ID, pkg.ID, editPayload)
	require.NoError(t, app.UpdatePackage(edit))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(edit))
	require.NoError(t, db.First(&retired, "id = ?", pkg.ID).Error)
	require.False(t, retired.IsActive)
	require.EqualValues(t, 3, retired.Version)
	require.Equal(t, before, bookingCommerceRowsSnapshot(t, db, org.ID, tables...))

	// Retirement does not invalidate credits already granted to the contact.
	bookingPayload.IdempotencyKey = "booking-after-retire-" + uuid.NewString()
	afterBooking := testutil.NewJSONRequest(t, bookingPayload)
	testutil.SetAuthContext(afterBooking, org.ID, user.ID)
	testutil.SetPathParam(afterBooking, "id", event.ID.String())
	require.NoError(t, app.CreateBooking(afterBooking))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(afterBooking))
	var balance models.CreditBalance
	require.NoError(t, db.First(&balance, "organization_id = ? AND contact_package_id = ?", org.ID, grant.ID).Error)
	require.Equal(t, 3, balance.Granted)
	require.Equal(t, 2, balance.Reserved)
	require.Equal(t, 1, balance.Available)
	require.Zero(t, balance.Consumed)
}

func createBookingCommerceTestRoleWithKeys(t *testing.T, db *gorm.DB, orgID uuid.UUID, name string, keys []string) *models.CustomRole {
	t.Helper()
	// Other focused tests may have populated only part of the global catalog.
	// Resolve every requested key instead of treating any catalog row as a
	// complete seed; the role itself is new and tenant-scoped.
	permissions := make([]models.Permission, 0, len(keys))
	for _, key := range keys {
		resource, action, ok := strings.Cut(key, ":")
		require.True(t, ok)
		permission := models.Permission{Resource: resource, Action: action}
		require.NoError(t, db.Where("resource = ? AND action = ?", resource, action).
			FirstOrCreate(&permission).Error)
		permissions = append(permissions, permission)
	}
	role := testutil.CreateTestRole(t, db, orgID, name, permissions)
	require.Len(t, role.Permissions, len(keys))
	return role
}

func createBookingCommercePackageFixture(t *testing.T, db *gorm.DB, orgID uuid.UUID) models.PackageDefinition {
	t.Helper()
	pkg := models.PackageDefinition{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		Name: "Reviewed package " + uuid.NewString(), PriceMinor: 15000,
		Currency: "MYR", ValidityDays: 30, IsActive: true, Metadata: models.JSONB{}, Version: 1,
	}
	require.NoError(t, db.Create(&pkg).Error)
	return pkg
}

func bookingCommercePackageEditPayload(pkg models.PackageDefinition) PackageRequest {
	return PackageRequest{
		Name: pkg.Name, Description: pkg.Description, PriceMinor: pkg.PriceMinor,
		Currency: pkg.Currency, ValidityDays: pkg.ValidityDays, Metadata: pkg.Metadata, Version: pkg.Version,
	}
}

func newBookingCommercePackageUpdate(t *testing.T, orgID, userID, packageID uuid.UUID, payload PackageRequest) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, payload)
	testutil.SetAuthContext(req, orgID, userID)
	testutil.SetPathParam(req, "id", packageID.String())
	return req
}

func bookingCommerceRowsSnapshot(t *testing.T, db *gorm.DB, orgID uuid.UUID, tables ...string) map[string]string {
	t.Helper()
	result := make(map[string]string, len(tables))
	for _, table := range tables {
		var rows []map[string]any
		require.NoError(t, db.Table(table).Where("organization_id = ?", orgID).Order("id").Find(&rows).Error)
		encoded, err := json.Marshal(rows)
		require.NoError(t, err)
		result[table] = string(encoded)
	}
	return result
}

func assertBookingCommerceAuditActive(t *testing.T, db *gorm.DB, orgID, resourceID uuid.UUID, action models.AuditAction, old any, active bool) {
	t.Helper()
	var rows []models.AuditLog
	require.NoError(t, db.Where("organization_id = ? AND resource_id = ? AND action = ?", orgID, resourceID, action).Find(&rows).Error)
	require.Len(t, rows, 1)
	for _, raw := range rows[0].Changes {
		change, ok := raw.(map[string]any)
		require.True(t, ok, "audit changes must be JSON objects")
		if change["field"] == "is_active" {
			require.Equal(t, old, change["old_value"])
			require.Equal(t, active, change["new_value"])
			return
		}
	}
	t.Fatal("audit must include the persisted is_active value")
}

func int64Pointer(value int64) *int64 {
	return &value
}

func bookingCommerceTimePointer(value time.Time) *time.Time {
	return &value
}

func createCapacityRaceBookingEvent(
	t *testing.T,
	db *gorm.DB,
	orgID, userID uuid.UUID,
	capacity int,
) models.BookingEvent {
	t.Helper()
	service := models.BookingService{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		Name: "Capacity race service " + uuid.NewString()[:8],
		Kind: models.BookingServiceKindAppointment, DurationMinutes: 30,
		DefaultCapacity: capacity, Currency: "MYR", IsActive: true,
		Metadata: models.JSONB{}, Version: 1,
		CreatedByID: &userID, UpdatedByID: &userID,
	}
	require.NoError(t, db.Create(&service).Error)
	resource := models.BookingResource{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		Name: "Capacity race room " + uuid.NewString()[:8],
		Kind: models.BookingResourceKindRoom, Timezone: "Asia/Kuala_Lumpur",
		IsActive: true, Metadata: models.JSONB{}, Version: 1,
		CreatedByID: &userID, UpdatedByID: &userID,
	}
	require.NoError(t, db.Create(&resource).Error)
	startsAt := time.Now().UTC().Add(time.Hour)
	event := models.BookingEvent{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		ServiceID: service.ID, ResourceID: resource.ID,
		StartsAt: startsAt, EndsAt: startsAt.Add(30 * time.Minute),
		Capacity: capacity, Status: models.BookingEventStatusScheduled,
		Metadata: models.JSONB{}, Version: 1,
		CreatedByID: &userID, UpdatedByID: &userID,
	}
	require.NoError(t, db.Create(&event).Error)
	return event
}

func enableBookingCommerceTestEntitlement(
	t *testing.T,
	db *gorm.DB,
	orgID, userID uuid.UUID,
	key string,
) {
	t.Helper()
	plan := models.Plan{
		BaseModel:   models.BaseModel{ID: uuid.New()},
		ScopeKey:    "test-" + uuid.NewString(),
		Code:        "test-" + uuid.NewString(),
		Name:        "Test plan",
		Status:      models.CommercialPlanStatusActive,
		Vertical:    "general",
		IsPublic:    false,
		Metadata:    models.JSONB{},
		CreatedByID: &userID,
	}
	require.NoError(t, db.Create(&plan).Error)
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
	require.NoError(t, db.Create(&account).Error)
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
		EntitlementsSnapshot: models.JSONB{key: true},
		ProviderData:         models.JSONB{},
		CurrentPeriodStart:   &periodStart,
		CurrentPeriodEnd:     &periodEnd,
		CreatedByID:          &userID,
	}
	require.NoError(t, db.Create(&subscription).Error)
}
