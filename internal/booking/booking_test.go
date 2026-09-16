package booking

import (
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func pureOfferFixture(t *testing.T) (Offer, Binding, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 13, 1, 0, 0, 123, time.UTC)
	org := uuid.New()
	serviceID := uuid.New()
	resourceID := uuid.New()
	service := models.BookingService{BaseModel: models.BaseModel{ID: serviceID}, OrganizationID: org, Name: "General appointment", Version: 1, IsActive: true}
	resource := models.BookingResource{BaseModel: models.BaseModel{ID: resourceID}, OrganizationID: org, Name: "Practitioner A", Timezone: "Asia/Kuala_Lumpur", Location: "Room A", Version: 1, IsActive: true}
	event := models.BookingEvent{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org, ServiceID: serviceID, ResourceID: resourceID, Service: &service, Resource: &resource, StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour), Capacity: 2, Version: 1, Status: models.BookingEventStatusScheduled}
	binding := Binding{OrganizationID: org, ChannelAccountID: uuid.New(), ContactID: uuid.New(), ScopeID: uuid.New(), Channel: "whatsapp", Revision: "revision-1", RoutingDigest: strings.Repeat("a", 64)}
	offer, err := NewOffer(binding, uuid.New(), uuid.New(), []Slot{{Event: event, RemainingCapacity: 2}}, now, 10*time.Minute)
	require.NoError(t, err)
	return offer, binding, now
}

func TestPureBookingShadowRehearsalHasNoEffects(t *testing.T) {
	offer, binding, now := pureOfferFixture(t)
	intent, err := ParseIntent(`{"action":"offer","reply":"","service":"General appointment","date":"2026-09-13"}`)
	require.NoError(t, err)
	facts := offer.Choices[0].Slot
	service := models.BookingService{BaseModel: models.BaseModel{ID: facts.ServiceID}, OrganizationID: binding.OrganizationID, Name: facts.ServiceName, Version: facts.ServiceVersion, IsActive: true}
	resource := models.BookingResource{BaseModel: models.BaseModel{ID: facts.ResourceID}, OrganizationID: binding.OrganizationID, Name: facts.ResourceName, Timezone: facts.Timezone, Location: facts.Location, Version: facts.ResourceVersion, IsActive: true}
	// Pure rehearsal: a synthetic slot list, no database or output transport.
	slots := []Slot{{Event: models.BookingEvent{BaseModel: models.BaseModel{ID: facts.EventID}, OrganizationID: binding.OrganizationID, ServiceID: facts.ServiceID, ResourceID: facts.ResourceID, Service: &service, Resource: &resource, StartsAt: facts.StartsAt, EndsAt: facts.EndsAt, Capacity: facts.Capacity, Version: facts.EventVersion}, RemainingCapacity: facts.Capacity}}
	beforeFacts, err := factsForEvent(slots[0].Event)
	require.NoError(t, err)
	preview, err := NewOffer(binding, offer.OriginInboundID, offer.OfferMessageID, slots, now, time.Minute)
	require.NoError(t, err)
	_, err = ValidateConfirmation(preview, binding, preview.Choices[0].Token, false, now)
	require.Error(t, err, "a preview is not a delivered offer")
	require.Equal(t, "offer", intent.Action)
	require.NotContains(t, strings.ToLower(RenderOffer(preview)), "confirmed")
	afterFacts, err := factsForEvent(slots[0].Event)
	require.NoError(t, err)
	require.Equal(t, beforeFacts, afterFacts)
	require.Equal(t, offer.Choices[0].Slot, facts, "pure rendering must not alter the original proposal")
}

func TestBookingCoreLastSeatAndManualSemantics(t *testing.T) {
	db, org, contact, event, now := bookingDBFixture(t)
	otherContact := testutil.CreateTestContact(t, db, org)
	staff := testutil.CreateTestUser(t, db, org)
	facts, err := factsForEvent(event)
	require.NoError(t, err)
	ai := ReserveInput{EventID: event.ID, ContactID: contact, Quantity: 1, Source: models.BookingSourceWhatsApp, IdempotencyKey: uuid.NewString(), Expected: &facts, OfferExpiresAt: now.Add(time.Minute)}
	manual := ReserveInput{EventID: event.ID, ContactID: otherContact.ID, Quantity: 1, Status: models.BookingStatusConfirmed, Source: models.BookingSourceAgent, ActorUserID: &staff.ID, Notes: " staff note ", IdempotencyKey: uuid.NewString()}
	start := make(chan struct{})
	type outcome struct {
		result ReserveResult
		err    error
	}
	done := make(chan outcome, 2)
	for _, input := range []ReserveInput{ai, manual} {
		input := input
		go func() {
			<-start
			var result ReserveResult
			err := db.Transaction(func(tx *gorm.DB) error {
				var err error
				result, err = ReserveTx(tx, org, input, func() time.Time { return now })
				return err
			})
			done <- outcome{result, err}
		}()
	}
	close(start)
	success := 0
	for i := 0; i < 2; i++ {
		select {
		case out := <-done:
			if out.err == nil {
				success++
				require.True(t, out.result.Created)
			} else {
				require.True(t, IsOfferStale(out.err), out.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("last-seat contenders did not settle")
		}
	}
	require.Equal(t, 1, success)
	var quantity int64
	require.NoError(t, db.Model(&models.Booking{}).Where("organization_id = ? AND event_id = ?", org, event.ID).Select("COALESCE(SUM(quantity),0)").Scan(&quantity).Error)
	require.EqualValues(t, 1, quantity)
	manual.IdempotencyKey = uuid.NewString()
	manual.AllowWaitlist = true
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		result, err := ReserveTx(tx, org, manual, func() time.Time { return now })
		if err != nil {
			return err
		}
		require.Equal(t, models.BookingStatusWaitlisted, result.Booking.Status)
		require.Nil(t, result.Booking.ConfirmedAt)
		require.Equal(t, &staff.ID, result.Booking.BookedByID)
		require.Equal(t, &staff.ID, result.Booking.UpdatedByID)
		require.Equal(t, "staff note", result.Booking.Notes)
		return nil
	}))
}

func TestBookingCoreClockSampledAfterResourceLock(t *testing.T) {
	for _, mode := range []string{"offer_expired", "license_expired"} {
		t.Run(mode, func(t *testing.T) {
			db, org, contact, event, now := bookingDBFixture(t)
			facts, err := factsForEvent(event)
			require.NoError(t, err)
			expiry := now.Add(time.Minute)
			in := ReserveInput{EventID: event.ID, ContactID: contact, Quantity: 1, Source: models.BookingSourceWhatsApp, IdempotencyKey: uuid.NewString(), Expected: &facts, OfferExpiresAt: expiry}
			if mode == "license_expired" {
				in.OfferExpiresAt = now.Add(2 * time.Minute)
				require.NoError(t, db.Model(&models.Subscription{}).Where("organization_id = ?", org).Update("current_period_end", expiry).Error)
			}
			lock := db.Begin()
			require.NoError(t, lock.Error)
			defer lock.Rollback()
			var resource models.BookingResource
			require.NoError(t, lock.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", event.ResourceID).First(&resource).Error)
			attempted := make(chan struct{}, 1)
			name := "test:ai_booking_clock_" + uuid.NewString()
			require.NoError(t, db.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
				if tx.Statement.Table == "booking_resources" {
					if _, ok := tx.Statement.Clauses["FOR"]; ok {
						select {
						case attempted <- struct{}{}:
						default:
						}
					}
				}
			}))
			defer db.Callback().Query().Remove(name)
			var clockNanos, reads atomic.Int64
			clockNanos.Store(now.UnixNano())
			done := make(chan error, 1)
			go func() {
				done <- db.Transaction(func(tx *gorm.DB) error {
					_, err := ReserveTx(tx, org, in, func() time.Time { reads.Add(1); return time.Unix(0, clockNanos.Load()) })
					return err
				})
			}()
			select {
			case <-attempted:
			case err := <-done:
				t.Fatalf("reservation returned before lock: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("reservation did not reach resource lock")
			}
			require.Zero(t, reads.Load())
			clockNanos.Store(expiry.UnixNano())
			require.NoError(t, lock.Commit().Error)
			select {
			case err := <-done:
				require.Error(t, err)
				if mode == "offer_expired" {
					require.True(t, IsOfferStale(err))
				} else {
					var domain *Error
					require.ErrorAs(t, err, &domain)
					require.Equal(t, "forbidden", domain.Kind)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("reservation did not complete after unlock")
			}
			require.EqualValues(t, 1, reads.Load())
			var count int64
			require.NoError(t, db.Model(&models.Booking{}).Where("organization_id = ?", org).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}

func TestPureBookingIntentStrictEnvelope(t *testing.T) {
	for _, raw := range []string{
		`{"action":"reply","reply":"Hello","service":"","date":""}`,
		`{"action":"offer","reply":"","service":"Consultation","date":"2026-09-14"}`,
		`{"action":"handoff","reply":"","service":"","date":""}`,
	} {
		_, err := ParseIntent(raw)
		require.NoError(t, err)
	}
	for _, raw := range []string{
		"yes", "BOOK invented", `{"action":"confirm","reply":"","service":"","date":""}`,
		`{"action":"reply","reply":"Your appointment is confirmed","service":"","date":""}`,
		`{"action":"reply","reply":"Tempahan anda disahkan","service":"","date":""}`,
		`{"action":"reply","reply":"BOOK forged-token","service":"","date":""}`,
		`{"action":"offer","service":"Consultation","event_id":"invented"}`,
		`{"action":"reply","action":"offer","reply":"yes","service":"Test"}`,
		`{"action":"offer","service":"11111111-1111-4111-8111-111111111111"}`,
		`{"action":"offer","service":"Test","date":"tomorrow"}`,
		`{"action":"offer","service":"Test","date":"2026-02-30"}`,
		`{"action":"reply","reply":"ok"} {}`,
		`{"action":"offer","service":"Test","confirm":true}`,
		strings.Repeat("x", 8193),
	} {
		_, err := ParseIntent(raw)
		require.Error(t, err, raw)
	}
	require.True(t, RequiresHandoff("Please let me speak to staff"))
	require.True(t, RequiresHandoff("I have chest pain"))
	require.True(t, RequiresHandoff("ubat apa sesuai?"))
	require.False(t, RequiresHandoff("I would like an appointment"))
}

func TestPureBookingOfferConfirmationAndTamper(t *testing.T) {
	offer, binding, now := pureOfferFixture(t)
	token := offer.Choices[0].Token
	require.Len(t, token, 43)
	parsed, ok := ConfirmationToken("BOOK " + token)
	require.True(t, ok)
	require.Equal(t, token, parsed)
	for _, text := range []string{"yes", token, "book " + token, " BOOK " + token, "BOOK " + token + " ", "BOOK " + token + "."} {
		_, ok := ConfirmationToken(text)
		require.False(t, ok)
	}
	choice, err := ValidateConfirmation(offer, binding, token, true, now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, offer.Choices[0], choice)
	_, err = ValidateConfirmation(offer, binding, token, false, now)
	require.Error(t, err)
	for _, at := range []time.Time{now.Add(-time.Nanosecond), offer.ExpiresAt, offer.ExpiresAt.Add(time.Nanosecond)} {
		_, err := ValidateConfirmation(offer, binding, token, true, at)
		require.Error(t, err)
	}
	other := binding
	other.Revision = "revision-2"
	_, err = ValidateConfirmation(offer, other, token, true, now)
	require.Error(t, err)
	other = binding
	other.ContactID = uuid.New()
	_, err = ValidateConfirmation(offer, other, token, true, now)
	require.Error(t, err)
	other = binding
	other.ScopeID = uuid.New()
	_, err = ValidateConfirmation(offer, other, token, true, now)
	require.Error(t, err)
	require.Contains(t, RenderOffer(offer), "Asia/Kuala_Lumpur")
	require.Contains(t, RenderOffer(offer), "BOOK "+token)
	require.Contains(t, RenderConfirmation(choice), "No payment was taken")
	blob, err := OfferJSON(offer)
	require.NoError(t, err)
	decoded, err := ParseOffer(blob)
	require.NoError(t, err)
	before, err := OfferFingerprint(offer)
	require.NoError(t, err)
	after, err := OfferFingerprint(decoded)
	require.NoError(t, err)
	require.Equal(t, before, after)
	decoded.Choices[0].Slot.Location = "Room B"
	after, err = OfferFingerprint(decoded)
	require.NoError(t, err)
	require.NotEqual(t, before, after)
	require.Equal(t, ReservationKey(offer.ID, token), ReservationKey(offer.ID, token))
	require.NotEqual(t, ReservationKey(offer.ID, token), ReservationKey(uuid.New(), token))
	blob["unknown"] = true
	_, err = ParseOffer(blob)
	require.Error(t, err)
	require.Error(t, strictJSON([]byte(`{"id":1,"id":2}`), &map[string]any{}))
	require.Error(t, strictJSON([]byte(`{"nested":{"x":1,"x":2}}`), &map[string]any{}))
}

func TestPureBookingOfferFactsAndLimits(t *testing.T) {
	offer, _, _ := pureOfferFixture(t)
	mutated := offer
	mutated.SchemaVersion = 2
	_, err := OfferJSON(mutated)
	require.Error(t, err)
	mutated = offer
	mutated.Choices = append(mutated.Choices, mutated.Choices[0])
	_, err = OfferJSON(mutated)
	require.Error(t, err)
	mutated = offer
	mutated.ExpiresAt = mutated.CreatedAt.Add(time.Hour)
	_, err = OfferJSON(mutated)
	require.Error(t, err)
	a := offer.Choices[0].Slot
	b := a
	b.StartsAt = b.StartsAt.In(time.FixedZone("other", 8*3600))
	require.True(t, sameFacts(a, b))
	b.ResourceVersion++
	require.False(t, sameFacts(a, b))
	require.True(t, IsOfferStale(unavailable("full")))
	require.False(t, IsOfferStale(conflict("wrong request")))
	require.False(t, IsOfferStale(errors.New("database unavailable")))
	// Invalid character counts fail before any database method is reached.
	for _, key := range []string{strings.Repeat("界", 256), strings.Repeat("a", 256), " \t "} {
		_, err := ReserveTx(&gorm.DB{}, uuid.New(), ReserveInput{EventID: uuid.New(), ContactID: uuid.New(), Quantity: 1, IdempotencyKey: key}, time.Now)
		var domain *Error
		require.ErrorAs(t, err, &domain)
		require.Equal(t, "invalid", domain.Kind)
	}
}

func TestPureBookingEntitlementLifecycleAndOverrides(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	before := now.Add(-time.Nanosecond)
	after := now.Add(time.Nanosecond)
	for _, status := range []models.SubscriptionStatus{models.SubscriptionStatusActive, models.SubscriptionStatusTrialing, models.SubscriptionStatusPastDue} {
		s := models.Subscription{Status: status, CurrentPeriodEnd: &after, TrialEndsAt: &after, GraceUntil: &after, EntitlementsSnapshot: models.JSONB{"bookings.enabled": true}}
		require.True(t, bookingEntitlementAllowed(&s, nil, now))
		s.CurrentPeriodEnd = &now
		s.TrialEndsAt = &now
		s.GraceUntil = &now
		override := models.EntitlementOverride{Key: "bookings.enabled", IsActive: true, StartsAt: before, Value: models.JSONB{"value": true}}
		require.False(t, bookingEntitlementAllowed(&s, &override, now))
	}
	for _, status := range []models.SubscriptionStatus{models.SubscriptionStatusIncomplete, models.SubscriptionStatusPaused, models.SubscriptionStatusCanceled} {
		s := models.Subscription{Status: status, CurrentPeriodEnd: &after, GraceUntil: &after, EntitlementsSnapshot: models.JSONB{"bookings.enabled": true}}
		require.False(t, bookingEntitlementAllowed(&s, nil, now))
	}
	s := models.Subscription{Status: models.SubscriptionStatusActive, CurrentPeriodEnd: &after, EntitlementsSnapshot: models.JSONB{"omnichannel.enabled": true}}
	require.False(t, bookingEntitlementAllowed(&s, nil, now))
	override := models.EntitlementOverride{Key: "bookings.enabled", IsActive: true, StartsAt: before, ExpiresAt: &after, Value: models.JSONB{"value": true}}
	require.True(t, bookingEntitlementAllowed(&s, &override, now))
	override.ExpiresAt = &now
	require.False(t, bookingEntitlementAllowed(&s, &override, now))
	override.ExpiresAt = &after
	override.Value = models.JSONB{"enabled": false}
	s.EntitlementsSnapshot["bookings.enabled"] = true
	require.False(t, bookingEntitlementAllowed(&s, &override, now))
	for _, value := range []any{false, 0, -1, float64(0), math.NaN(), "disabled", "false", "0", "none", nil, models.JSONB{}} {
		require.False(t, entitlementAllows(value))
	}
	for _, value := range []any{true, 1, float64(1), "enabled", models.JSONB{"enabled": true}, map[string]any{"value": true}} {
		require.True(t, entitlementAllows(value))
	}
}

func bookingDBFixture(t *testing.T) (*gorm.DB, uuid.UUID, uuid.UUID, models.BookingEvent, time.Time) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	contact := testutil.CreateTestContact(t, db, org.ID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	s := models.BookingService{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, Name: "Consultation", Kind: models.BookingServiceKindAppointment, DurationMinutes: 30, DefaultCapacity: 1, Currency: "MYR", IsActive: true, Version: 1}
	r := models.BookingResource{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, Name: "Staff A", Kind: models.BookingResourceKindPractitioner, Timezone: "Asia/Kuala_Lumpur", IsActive: true, Version: 1}
	require.NoError(t, db.Create(&s).Error)
	require.NoError(t, db.Create(&r).Error)
	e := models.BookingEvent{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, ServiceID: s.ID, ResourceID: r.ID, StartsAt: now.Add(time.Hour), EndsAt: now.Add(90 * time.Minute), Capacity: 1, Status: models.BookingEventStatusScheduled, Version: 1}
	require.NoError(t, db.Create(&e).Error)
	e.Service = &s
	e.Resource = &r
	plan := models.Plan{BaseModel: models.BaseModel{ID: uuid.New()}, ScopeKey: uuid.NewString(), Code: uuid.NewString(), Name: "Test", Status: models.CommercialPlanStatusActive}
	billing := models.BillingAccount{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, Provider: models.BillingProviderManual, Status: models.BillingAccountStatusActive, DefaultCurrency: "MYR"}
	require.NoError(t, db.Create(&plan).Error)
	require.NoError(t, db.Create(&billing).Error)
	end := now.Add(24 * time.Hour)
	sub := models.Subscription{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, PlanID: plan.ID, BillingAccountID: billing.ID, Status: models.SubscriptionStatusActive, CurrentPeriodEnd: &end, EntitlementsSnapshot: models.JSONB{"bookings.enabled": true}}
	require.NoError(t, db.Create(&sub).Error)
	return db, org.ID, contact.ID, e, now
}

func TestBookingCoreCallerTransactionAndReplay(t *testing.T) {
	t.Run("staff_unicode_255_characters_create_and_replay", func(t *testing.T) {
		db, org, contact, event, now := bookingDBFixture(t)
		staff := testutil.CreateTestUser(t, db, org)
		key := strings.Repeat("界", 254) + "a"
		require.Greater(t, len(key), 255, "regression must exceed 255 UTF-8 bytes")
		in := ReserveInput{EventID: event.ID, ContactID: contact, Quantity: 1, Source: models.BookingSourceAgent, ActorUserID: &staff.ID, IdempotencyKey: "  " + key + " \t"}
		var first ReserveResult
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			var err error
			first, err = ReserveTx(tx, org, in, func() time.Time { return now })
			return err
		}))
		require.True(t, first.Created)
		require.Equal(t, key, first.Booking.IdempotencyKey)
		require.Equal(t, &staff.ID, first.Booking.BookedByID)
		for _, replayKey := range []string{key, " \t" + key + " "} {
			in.IdempotencyKey = replayKey
			require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
				replay, err := ReserveTx(tx, org, in, func() time.Time { return now.Add(2 * time.Hour) })
				if err != nil {
					return err
				}
				require.False(t, replay.Created)
				require.Equal(t, first.Booking.ID, replay.Booking.ID)
				return nil
			}))
		}
		in.IdempotencyKey = key + "界"
		require.Error(t, db.Transaction(func(tx *gorm.DB) error {
			_, err := ReserveTx(tx, org, in, func() time.Time { return now })
			var domain *Error
			require.ErrorAs(t, err, &domain)
			require.Equal(t, "invalid", domain.Kind)
			return err
		}))
		var count int64
		require.NoError(t, db.Model(&models.Booking{}).Where("organization_id = ?", org).Count(&count).Error)
		require.EqualValues(t, 1, count)
	})
	db, org, contact, event, now := bookingDBFixture(t)
	facts, err := factsForEvent(event)
	require.NoError(t, err)
	in := ReserveInput{EventID: event.ID, ContactID: contact, Quantity: 1, Status: models.BookingStatusReserved, Source: models.BookingSourceWhatsApp, IdempotencyKey: "ai-booking:" + uuid.NewString(), Expected: &facts, OfferExpiresAt: now.Add(10 * time.Minute)}
	aborted := errors.New("force confirmation intent failure")
	require.ErrorIs(t, db.Transaction(func(tx *gorm.DB) error {
		result, err := ReserveTx(tx, org, in, func() time.Time { return now })
		require.NoError(t, err)
		require.True(t, result.Created)
		require.Nil(t, result.Booking.BookedByID)
		require.Nil(t, result.Booking.UpdatedByID)
		return aborted
	}), aborted)
	var count int64
	require.NoError(t, db.Model(&models.Booking{}).Where("organization_id = ?", org).Count(&count).Error)
	require.Zero(t, count)
	var result ReserveResult
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = ReserveTx(tx, org, in, func() time.Time { return now })
		return err
	}))
	require.True(t, result.Created)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		replay, err := ReserveTx(tx, org, in, func() time.Time { return now.Add(2 * time.Hour) })
		require.NoError(t, err)
		require.False(t, replay.Created)
		require.Equal(t, result.Booking.ID, replay.Booking.ID)
		return nil
	}))
	different := in
	different.ContactID = testutil.CreateTestContact(t, db, org).ID
	require.Error(t, db.Transaction(func(tx *gorm.DB) error {
		_, err := ReserveTx(tx, org, different, func() time.Time { return now })
		require.False(t, IsOfferStale(err))
		return err
	}))
}

func TestBookingCorePostLockOfferAndLicenseExpiry(t *testing.T) {
	for _, mode := range []string{"offer_expired", "license_expired", "material_changed", "foreign_contact", "merged_contact"} {
		t.Run(mode, func(t *testing.T) {
			db, org, contact, event, now := bookingDBFixture(t)
			facts, err := factsForEvent(event)
			require.NoError(t, err)
			in := ReserveInput{EventID: event.ID, ContactID: contact, Quantity: 1, Status: models.BookingStatusReserved, Source: models.BookingSourceWhatsApp, IdempotencyKey: uuid.NewString(), Expected: &facts, OfferExpiresAt: now.Add(time.Minute)}
			at := now
			switch mode {
			case "offer_expired":
				at = in.OfferExpiresAt
			case "license_expired":
				require.NoError(t, db.Model(&models.Subscription{}).Where("organization_id = ?", org).Update("current_period_end", now).Error)
			case "material_changed":
				require.NoError(t, db.Model(&models.BookingResource{}).Where("id = ?", event.ResourceID).Update("location", "Changed").Error)
			case "foreign_contact":
				other := testutil.CreateTestOrganization(t, db)
				in.ContactID = testutil.CreateTestContact(t, db, other.ID).ID
			case "merged_contact":
				target := testutil.CreateTestContact(t, db, org)
				require.NoError(t, db.Model(&models.Contact{}).Where("id = ?", contact).Update("merged_into_id", target.ID).Error)
			}
			require.Error(t, db.Transaction(func(tx *gorm.DB) error { _, err := ReserveTx(tx, org, in, func() time.Time { return at }); return err }))
			var count int64
			require.NoError(t, db.Model(&models.Booking{}).Where("organization_id = ?", org).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}
