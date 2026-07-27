package handlers

import (
	"math"
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

func int64Pointer(value int64) *int64 {
	return &value
}

func bookingCommerceTimePointer(value time.Time) *time.Time {
	return &value
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
