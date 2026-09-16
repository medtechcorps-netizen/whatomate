// Package booking contains tenant-scoped booking primitives. The caller owns
// the transaction and authorization; this package never commits or sends.
package booking

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Error struct{ Kind, Message string }

func (e *Error) Error() string   { return e.Message }
func invalid(s string) error     { return &Error{"invalid", s} }
func conflict(s string) error    { return &Error{"conflict", s} }
func unavailable(s string) error { return &Error{"unavailable", s} }

// IsOfferStale never masks storage, authorization or conflicting-idempotency errors.
func IsOfferStale(err error) bool { var e *Error; return errors.As(err, &e) && e.Kind == "unavailable" }

type Slot struct {
	Event             models.BookingEvent `json:"event"`
	BookedQuantity    int                 `json:"booked_quantity"`
	RemainingCapacity int                 `json:"remaining_capacity"`
	Timezone          string              `json:"timezone"`
	LocalStartsAt     string              `json:"local_starts_at"`
	LocalEndsAt       string              `json:"local_ends_at"`
}
type SlotFilter struct {
	From, To              time.Time
	ServiceID, ResourceID *uuid.UUID
	Service, Date         string
	Limit, Offset         int
}
type SlotPage struct {
	Slots []Slot
	Total int64
}

func CapacityStatuses() []models.BookingStatus {
	return []models.BookingStatus{models.BookingStatusReserved, models.BookingStatusConfirmed, models.BookingStatusCheckedIn, models.BookingStatusCompleted, models.BookingStatusNoShow}
}
func OccupiesCapacity(status models.BookingStatus) bool {
	for _, candidate := range CapacityStatuses() {
		if status == candidate {
			return true
		}
	}
	return false
}

// ListSlots uses one capacity aggregate per query. Date, when present, is an
// exact YYYY-MM-DD in each resource's explicitly displayed timezone, not a
// guessed browser/customer timezone. Service is an exact case-insensitive name.
func ListSlots(tx *gorm.DB, orgID uuid.UUID, f SlotFilter) (SlotPage, error) {
	page := SlotPage{Slots: []Slot{}}
	if tx == nil || orgID == uuid.Nil || f.From.IsZero() || !f.To.After(f.From) || f.Limit < 1 || f.Limit > 250 || f.Offset < 0 {
		return page, invalid("invalid booking availability scope")
	}
	if f.Date != "" {
		if _, err := time.Parse("2006-01-02", f.Date); err != nil {
			return page, invalid("date must be YYYY-MM-DD")
		}
	}
	occupied := tx.Model(&models.Booking{}).Select("bookings.event_id, COALESCE(SUM(bookings.quantity), 0) AS booked_quantity").Where("bookings.organization_id = ? AND bookings.status IN ?", orgID, CapacityStatuses()).Group("bookings.event_id")
	query := tx.Model(&models.BookingEvent{}).
		Joins("JOIN booking_services AS available_services ON available_services.id = booking_events.service_id AND available_services.organization_id = booking_events.organization_id AND available_services.is_active = ? AND available_services.deleted_at IS NULL", true).
		Joins("JOIN booking_resources AS available_resources ON available_resources.id = booking_events.resource_id AND available_resources.organization_id = booking_events.organization_id AND available_resources.is_active = ? AND available_resources.deleted_at IS NULL", true).
		Joins("LEFT JOIN (?) AS available_capacity ON available_capacity.event_id = booking_events.id", occupied).
		Where("booking_events.organization_id = ? AND booking_events.status = ?", orgID, models.BookingEventStatusScheduled).
		Where("booking_events.starts_at >= ? AND booking_events.starts_at <= ?", f.From, f.To).
		Where("booking_events.capacity > COALESCE(available_capacity.booked_quantity, 0)")
	if f.ServiceID != nil {
		query = query.Where("booking_events.service_id = ?", *f.ServiceID)
	}
	if f.ResourceID != nil {
		query = query.Where("booking_events.resource_id = ?", *f.ResourceID)
	}
	if strings.TrimSpace(f.Service) != "" {
		query = query.Where("LOWER(BTRIM(available_services.name)) = LOWER(?)", strings.TrimSpace(f.Service))
	}
	if f.Date != "" {
		query = query.Where("TO_CHAR(booking_events.starts_at AT TIME ZONE available_resources.timezone, 'YYYY-MM-DD') = ?", f.Date)
	}
	if err := query.Count(&page.Total).Error; err != nil {
		return page, err
	}
	var rows []struct {
		models.BookingEvent
		BookedQuantity int
	}
	err := query.Select("booking_events.*, COALESCE(available_capacity.booked_quantity, 0) AS booked_quantity").Preload("Service", "organization_id = ? AND is_active = ?", orgID, true).Preload("Resource", "organization_id = ? AND is_active = ?", orgID, true).Order("booking_events.starts_at ASC, booking_events.id ASC").Limit(f.Limit).Offset(f.Offset).Find(&rows).Error
	if err != nil {
		return page, err
	}
	for _, row := range rows {
		if row.Service == nil || row.Resource == nil {
			return page, errors.New("booking slot definitions unavailable")
		}
		location, err := time.LoadLocation(row.Resource.Timezone)
		if err != nil {
			return page, err
		}
		event := row.BookingEvent
		if strings.TrimSpace(event.Location) == "" {
			event.Location = strings.TrimSpace(row.Resource.Location)
		}
		page.Slots = append(page.Slots, Slot{Event: event, BookedQuantity: row.BookedQuantity, RemainingCapacity: event.Capacity - row.BookedQuantity, Timezone: row.Resource.Timezone, LocalStartsAt: event.StartsAt.In(location).Format(time.RFC3339), LocalEndsAt: event.EndsAt.In(location).Format(time.RFC3339)})
	}
	return page, nil
}

// LockEvent follows the established Event -> Service -> Resource lock order.
func LockEvent(tx *gorm.DB, orgID, eventID uuid.UUID) (*models.BookingEvent, error) {
	if tx == nil || orgID == uuid.Nil || eventID == uuid.Nil {
		return nil, invalid("booking tenant and event are required")
	}
	var event models.BookingEvent
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", eventID, orgID).First(&event).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, unavailable("Booking slot is no longer available; refresh available slots and choose another")
		}
		return nil, err
	}
	if event.Status != models.BookingEventStatusScheduled {
		return nil, unavailable("Booking slot is no longer scheduled; refresh available slots and choose another")
	}
	var service models.BookingService
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ? AND is_active = ?", event.ServiceID, orgID, true).First(&service).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, unavailable("Booking service is inactive; refresh available slots and choose another")
		}
		return nil, err
	}
	var resource models.BookingResource
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ? AND is_active = ?", event.ResourceID, orgID, true).First(&resource).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, unavailable("Booking practitioner or resource is inactive; refresh available slots and choose another")
		}
		return nil, err
	}
	event.Service, event.Resource = &service, &resource
	return &event, nil
}
func EnsureNotStarted(event *models.BookingEvent, now time.Time) error {
	if event != nil && event.StartsAt.After(now.UTC()) {
		return nil
	}
	return unavailable("Booking slot has already started; refresh available slots and choose another")
}

type ReserveInput struct {
	EventID, ContactID uuid.UUID
	Quantity           int
	Status             models.BookingStatus
	Source             models.BookingSource
	Notes              string
	ContactPackageID   *uuid.UUID
	AllowWaitlist      bool
	IdempotencyKey     string
	Metadata           models.JSONB
	ActorUserID        *uuid.UUID
	Expected           *SlotFacts
	OfferExpiresAt     time.Time
}
type ReserveResult struct {
	Booking models.Booking
	Event   models.BookingEvent
	Created bool
	Now     time.Time
}

// ReserveTx deliberately does not write package credits, activity or user
// audit. Staff and AI callers compose those facts on this SAME transaction.
func ReserveTx(tx *gorm.DB, orgID uuid.UUID, in ReserveInput, clock func() time.Time) (ReserveResult, error) {
	var out ReserveResult
	// Match the staff DTO and PostgreSQL varchar character limit. Normalize
	// before both replay lookup and insertion; server-generated AI keys stay exact.
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if tx == nil || orgID == uuid.Nil || in.EventID == uuid.Nil || in.ContactID == uuid.Nil || in.Quantity < 1 || in.IdempotencyKey == "" || utf8.RuneCountInString(in.IdempotencyKey) > 255 || clock == nil {
		return out, invalid("invalid reservation scope")
	}
	if in.Status == "" {
		in.Status = models.BookingStatusReserved
	}
	if in.Source == "" {
		in.Source = models.BookingSourceAgent
	}
	if in.Status != models.BookingStatusReserved && in.Status != models.BookingStatusConfirmed && in.Status != models.BookingStatusWaitlisted {
		return out, invalid("invalid initial booking status")
	}
	if in.ActorUserID != nil && *in.ActorUserID == uuid.Nil {
		return out, invalid("zero staff actor is forbidden")
	}
	if in.Expected != nil && (in.Quantity != 1 || in.Status != models.BookingStatusReserved || in.AllowWaitlist || in.ContactPackageID != nil || in.ActorUserID != nil || in.OfferExpiresAt.IsZero()) {
		return out, invalid("AI reservation must be one reserved place without staff or financial authority and require offer expiry")
	}
	lookup := func() (bool, error) {
		err := tx.Where("organization_id = ? AND idempotency_key = ?", orgID, in.IdempotencyKey).First(&out.Booking).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if out.Booking.EventID != in.EventID || out.Booking.ContactID != in.ContactID || out.Booking.Quantity != in.Quantity || !sameUUID(out.Booking.ContactPackageID, in.ContactPackageID) {
			return false, conflict("Idempotency key was already used for a different booking")
		}
		return true, nil
	}
	if found, err := lookup(); found || err != nil {
		return out, err
	}
	var contact models.Contact
	if err := tx.Select("id", "merged_into_id").Where("id = ? AND organization_id = ?", in.ContactID, orgID).First(&contact).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return out, invalid("contact_id does not belong to this organization")
		}
		return out, err
	}
	// An AI offer binds one immutable canonical contact; never follow a later
	// merge to a different identity. Staff alias handling remains unchanged.
	if in.Expected != nil && contact.MergedIntoID != nil {
		return out, unavailable("Booking contact identity changed; request a fresh offer")
	}
	event, err := LockEvent(tx, orgID, in.EventID)
	if err != nil {
		return out, err
	}
	if found, err := lookup(); found || err != nil {
		return out, err
	}
	if in.Expected != nil {
		facts, err := factsForEvent(*event)
		if err != nil {
			return out, err
		}
		if !sameFacts(facts, *in.Expected) {
			return out, unavailable("Booking slot details changed; request a fresh offer")
		}
	}
	var occupied int64
	if err := tx.Model(&models.Booking{}).Select("COALESCE(SUM(quantity), 0)").Where("organization_id = ? AND event_id = ? AND status IN ?", orgID, event.ID, CapacityStatuses()).Scan(&occupied).Error; err != nil {
		return out, err
	}
	status := in.Status
	if OccupiesCapacity(status) && occupied > int64(event.Capacity)-int64(in.Quantity) {
		if !in.AllowWaitlist {
			return out, unavailable("Booking slot was just filled; refresh available slots and choose another")
		}
		status = models.BookingStatusWaitlisted
	}
	now := clock().UTC()
	if err := EnsureNotStarted(event, now); err != nil {
		return out, err
	}
	if in.Expected != nil && !now.Before(in.OfferExpiresAt) {
		return out, unavailable("Booking offer expired; request a fresh offer")
	}
	if in.Expected != nil {
		allowed, err := HasEntitlementTx(tx, orgID, now)
		if err != nil {
			return out, err
		}
		if !allowed {
			return out, &Error{"forbidden", "Booking entitlement is not available"}
		}
	}
	metadata := in.Metadata
	if metadata == nil {
		metadata = models.JSONB{}
	}
	out.Booking = models.Booking{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID, EventID: event.ID, ContactID: in.ContactID, Quantity: in.Quantity, Status: status, Source: in.Source, Notes: strings.TrimSpace(in.Notes), ContactPackageID: in.ContactPackageID, BookedByID: in.ActorUserID, UpdatedByID: in.ActorUserID, IdempotencyKey: strings.TrimSpace(in.IdempotencyKey), Metadata: metadata, Version: 1}
	if status == models.BookingStatusConfirmed {
		out.Booking.ConfirmedAt = &now
	}
	if err := tx.Create(&out.Booking).Error; err != nil {
		return out, err
	}
	if OccupiesCapacity(status) {
		update := tx.Model(&models.BookingEvent{}).Where("id = ? AND organization_id = ?", event.ID, orgID).UpdateColumn("version", gorm.Expr("version + 1"))
		if update.Error != nil {
			return out, update.Error
		}
		if update.RowsAffected != 1 {
			return out, gorm.ErrRecordNotFound
		}
	}
	out.Event = *event
	out.Created = true
	out.Now = now
	return out, nil
}
func sameUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// HasEntitlementTx is intentionally booking-only. Its lifecycle/override
// semantics mirror the existing commercial handler, including preferring the
// current live subscription over newer historical rows. No user bypass exists.
func HasEntitlementTx(tx *gorm.DB, orgID uuid.UUID, now time.Time) (bool, error) {
	if tx == nil || orgID == uuid.Nil || now.IsZero() {
		return false, invalid("booking entitlement scope is required")
	}
	var subscription models.Subscription
	if err := loadCurrentSubscription(tx, orgID, &subscription); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if !subscriptionPermitsFeatures(&subscription, now.UTC()) {
		return false, nil
	}
	override, err := activeOverride(tx, orgID, "bookings.enabled", now.UTC())
	if err != nil {
		return false, err
	}
	return bookingEntitlementAllowed(&subscription, override, now.UTC()), nil
}
func bookingEntitlementAllowed(s *models.Subscription, o *models.EntitlementOverride, now time.Time) bool {
	if !subscriptionPermitsFeatures(s, now) {
		return false
	}
	allowed := entitlementAllows(s.EntitlementsSnapshot["bookings.enabled"])
	if o != nil && o.Key == "bookings.enabled" && o.IsActive && !o.StartsAt.After(now) && (o.ExpiresAt == nil || o.ExpiresAt.After(now)) {
		return entitlementAllows(entitlementValue(o.Value))
	}
	return allowed
}
func entitlementAllows(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case int:
		return typed > 0
	case int32:
		return typed > 0
	case int64:
		return typed > 0
	case float64:
		return !math.IsNaN(typed) && typed > 0
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		return normalized != "" &&
			normalized != "0" &&
			normalized != "false" &&
			normalized != "disabled" &&
			normalized != "none"
	case models.JSONB:
		if nested, ok := typed["value"]; ok {
			return entitlementAllows(nested)
		}
		if enabled, ok := typed["enabled"]; ok {
			return entitlementAllows(enabled)
		}
		return len(typed) > 0
	case map[string]any:
		return entitlementAllows(models.JSONB(typed))
	default:
		return value != nil
	}
}

func subscriptionPermitsFeatures(
	subscription *models.Subscription,
	now time.Time,
) bool {
	if subscription == nil {
		return false
	}
	switch subscription.Status {
	case models.SubscriptionStatusActive:
		return (subscription.CurrentPeriodEnd != nil && subscription.CurrentPeriodEnd.After(now)) ||
			(subscription.GraceUntil != nil && subscription.GraceUntil.After(now))
	case models.SubscriptionStatusTrialing:
		return subscription.TrialEndsAt != nil && subscription.TrialEndsAt.After(now)
	case models.SubscriptionStatusPastDue:
		return subscription.GraceUntil != nil && subscription.GraceUntil.After(now)
	default:
		return false
	}
}

var liveSubscriptionStatuses = []models.SubscriptionStatus{
	models.SubscriptionStatusIncomplete,
	models.SubscriptionStatusTrialing,
	models.SubscriptionStatusActive,
	models.SubscriptionStatusPastDue,
	models.SubscriptionStatusPaused,
}

// loadCurrentSubscription prefers the single live lifecycle
// row guaranteed by idx_subscriptions_org_live. If no live row exists, it falls
// back to the newest historical row so cancellation and expiry stay visible.
func loadCurrentSubscription(
	db *gorm.DB,
	organizationID uuid.UUID,
	subscription *models.Subscription,
) error {
	err := db.Where(
		"organization_id = ? AND status IN ?",
		organizationID,
		liveSubscriptionStatuses,
	).Order("created_at DESC, id DESC").First(subscription).Error
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return db.Where("organization_id = ?", organizationID).
		Order("created_at DESC, id DESC").
		First(subscription).Error
}

func activeOverride(
	db *gorm.DB,
	orgID uuid.UUID,
	key string,
	now time.Time,
) (*models.EntitlementOverride, error) {
	var override models.EntitlementOverride
	err := db.Where(
		"organization_id = ? AND key = ? AND is_active = ? AND starts_at <= ? AND (expires_at IS NULL OR expires_at > ?)",
		orgID,
		key,
		true,
		now,
		now,
	).Order("starts_at DESC, created_at DESC").First(&override).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &override, nil
}

func entitlementValue(value models.JSONB) any {
	if len(value) == 1 {
		if scalar, ok := value["value"]; ok {
			return scalar
		}
	}
	return value
}

const IntentInstructions = `Return only one JSON object with keys action, reply, service, date. action is reply, offer, or handoff. reply is ordinary non-clinical text, service is an exact service-name preference, date is YYYY-MM-DD or empty. Never supply IDs, confirmation, booking authority, medical advice, payments or cancellation. Use handoff for staff requests, clinical/emergency questions, uncertainty or unsupported operations. Booking choices and confirmation are rendered by the server.`

type Intent struct {
	Action  string `json:"action"`
	Reply   string `json:"reply"`
	Service string `json:"service"`
	Date    string `json:"date"`
}

// strictJSON rejects repeated keys at every depth before decoding typed data.
func strictJSON(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > 32768 {
		return invalid("JSON body is empty or oversized")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func() error
	walk = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return invalid("duplicate JSON key")
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return invalid("unexpected JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid("trailing JSON data")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	return nil
}
func ParseIntent(raw string) (Intent, error) {
	var in Intent
	if len(raw) > 8192 {
		return in, invalid("intent is oversized")
	}
	if err := strictJSON([]byte(raw), &in); err != nil {
		return in, err
	}
	if len(in.Reply) > 2000 || len(in.Service) > 120 || len(in.Date) > 10 {
		return Intent{}, invalid("intent fields are oversized")
	}
	if _, err := uuid.Parse(in.Service); in.Service != "" && err == nil {
		return Intent{}, invalid("model-selected identifiers are forbidden")
	}
	if in.Date != "" {
		if _, err := time.Parse("2006-01-02", in.Date); err != nil {
			return Intent{}, invalid("date must be YYYY-MM-DD")
		}
	}
	switch in.Action {
	case "reply":
		if strings.TrimSpace(in.Reply) == "" || in.Service != "" || in.Date != "" {
			return Intent{}, invalid("invalid reply intent")
		}
		// Successful reservation wording and confirmation tokens are rendered
		// only from a committed server receipt, never supplied by the model.
		for _, claim := range []string{"confirmed", "reserved", "booked", "scheduled", "disahkan", "ditempah", "dijadualkan", "book "} {
			if strings.Contains(strings.ToLower(in.Reply), claim) {
				return Intent{}, invalid("model reply cannot claim a booking or issue confirmation syntax")
			}
		}
	case "offer":
		if in.Reply != "" || strings.TrimSpace(in.Service) == "" {
			return Intent{}, invalid("offer requires a service name only and optional exact date")
		}
	case "handoff":
		if in.Service != "" || in.Date != "" {
			return Intent{}, invalid("invalid handoff intent")
		}
	default:
		return Intent{}, invalid("unknown intent action")
	}
	return in, nil
}

// RequiresHandoff is a conservative routing guard, not a clinical classifier.
// Ambiguous/model-declared clinical input still has to take the handoff path.
func RequiresHandoff(text string) bool {
	text = strings.ToLower(text)
	for _, marker := range []string{"human", "speak to staff", "talk to staff", "speak to an agent", "talk to an agent", "receptionist", "manusia", "bercakap dengan staf", "emergency", "chest pain", "cannot breathe", "can't breathe", "suicide", "overdose", "unconscious", "severe bleeding", "kecemasan", "sakit dada", "sesak nafas", "diagnos", "symptom", "dosage", "side effect", "medical advice", "treatment advice", "what medicine", "which medicine", "should i take", "ubat apa", "kesan sampingan"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

type Binding struct {
	OrganizationID   uuid.UUID `json:"organization_id"`
	ChannelAccountID uuid.UUID `json:"channel_account_id"`
	ContactID        uuid.UUID `json:"contact_id"`
	ScopeID          uuid.UUID `json:"scope_id"`
	Channel          string    `json:"channel"`
	Revision         string    `json:"revision"`
	RoutingDigest    string    `json:"routing_digest"`
}
type SlotFacts struct {
	EventID         uuid.UUID `json:"event_id"`
	ServiceID       uuid.UUID `json:"service_id"`
	ResourceID      uuid.UUID `json:"resource_id"`
	EventVersion    int64     `json:"event_version"`
	ServiceVersion  int64     `json:"service_version"`
	ResourceVersion int64     `json:"resource_version"`
	StartsAt        time.Time `json:"starts_at"`
	EndsAt          time.Time `json:"ends_at"`
	Capacity        int       `json:"capacity"`
	ServiceName     string    `json:"service_name"`
	ResourceName    string    `json:"resource_name"`
	Timezone        string    `json:"timezone"`
	Location        string    `json:"location"`
}
type Choice struct {
	Token string    `json:"token"`
	Slot  SlotFacts `json:"slot"`
}

func sameFacts(a, b SlotFacts) bool {
	a.StartsAt = a.StartsAt.UTC()
	a.EndsAt = a.EndsAt.UTC()
	b.StartsAt = b.StartsAt.UTC()
	b.EndsAt = b.EndsAt.UTC()
	return a == b
}

type Offer struct {
	SchemaVersion   int       `json:"schema_version"`
	ID              uuid.UUID `json:"id"`
	Binding         Binding   `json:"binding"`
	OriginInboundID uuid.UUID `json:"origin_inbound_id"`
	OfferMessageID  uuid.UUID `json:"offer_message_id"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	Choices         []Choice  `json:"choices"`
}

func validBinding(b Binding) bool {
	return b.OrganizationID != uuid.Nil && b.ChannelAccountID != uuid.Nil && b.ContactID != uuid.Nil && b.ScopeID != uuid.Nil && strings.TrimSpace(b.Channel) != "" && strings.TrimSpace(b.Revision) != "" && len(b.Revision) <= 128 && len(b.RoutingDigest) == 64 && isHex(b.RoutingDigest)
}
func isHex(s string) bool { _, err := hex.DecodeString(s); return err == nil }
func factsForEvent(event models.BookingEvent) (SlotFacts, error) {
	if event.Service == nil || event.Resource == nil || event.ID == uuid.Nil || event.Service.ID != event.ServiceID || event.Resource.ID != event.ResourceID || event.Service.OrganizationID != event.OrganizationID || event.Resource.OrganizationID != event.OrganizationID {
		return SlotFacts{}, invalid("slot definitions do not match")
	}
	location := strings.TrimSpace(event.Location)
	if location == "" {
		location = strings.TrimSpace(event.Resource.Location)
	}
	return SlotFacts{EventID: event.ID, ServiceID: event.ServiceID, ResourceID: event.ResourceID, EventVersion: event.Version, ServiceVersion: event.Service.Version, ResourceVersion: event.Resource.Version, StartsAt: event.StartsAt.UTC(), EndsAt: event.EndsAt.UTC(), Capacity: event.Capacity, ServiceName: event.Service.Name, ResourceName: event.Resource.Name, Timezone: event.Resource.Timezone, Location: location}, nil
}
func NewOffer(binding Binding, originID, messageID uuid.UUID, slots []Slot, now time.Time, ttl time.Duration) (Offer, error) {
	o := Offer{SchemaVersion: 1, ID: uuid.New(), Binding: binding, OriginInboundID: originID, OfferMessageID: messageID, CreatedAt: now.UTC(), ExpiresAt: now.UTC().Add(ttl), Choices: []Choice{}}
	if !validBinding(binding) || originID == uuid.Nil || messageID == uuid.Nil || ttl <= 0 || ttl > 30*time.Minute || len(slots) < 1 || len(slots) > 3 {
		return Offer{}, invalid("invalid offer scope")
	}
	seen := map[uuid.UUID]bool{}
	for _, slot := range slots {
		if slot.Event.OrganizationID != binding.OrganizationID || slot.RemainingCapacity < 1 || !slot.Event.StartsAt.After(now) || seen[slot.Event.ID] {
			return Offer{}, invalid("offer contains unavailable or duplicate slot")
		}
		seen[slot.Event.ID] = true
		facts, err := factsForEvent(slot.Event)
		if err != nil {
			return Offer{}, err
		}
		token := make([]byte, 32)
		if _, err := rand.Read(token); err != nil {
			return Offer{}, err
		}
		o.Choices = append(o.Choices, Choice{Token: base64.RawURLEncoding.EncodeToString(token), Slot: facts})
	}
	if err := validateOffer(o); err != nil {
		return Offer{}, err
	}
	return o, nil
}
func validToken(token string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == token
}
func ConfirmationToken(text string) (string, bool) {
	if !strings.HasPrefix(text, "BOOK ") {
		return "", false
	}
	token := strings.TrimPrefix(text, "BOOK ")
	return token, validToken(token)
}
func validateOffer(o Offer) error {
	if o.SchemaVersion != 1 || o.ID == uuid.Nil || !validBinding(o.Binding) || o.OriginInboundID == uuid.Nil || o.OfferMessageID == uuid.Nil || o.CreatedAt.IsZero() || !o.ExpiresAt.After(o.CreatedAt) || o.ExpiresAt.Sub(o.CreatedAt) > 30*time.Minute || len(o.Choices) < 1 || len(o.Choices) > 3 {
		return invalid("invalid durable offer")
	}
	seen := map[string]bool{}
	events := map[uuid.UUID]bool{}
	for _, c := range o.Choices {
		s := c.Slot
		if !validToken(c.Token) || seen[c.Token] || events[s.EventID] || s.EventID == uuid.Nil || s.ServiceID == uuid.Nil || s.ResourceID == uuid.Nil || s.EventVersion < 1 || s.ServiceVersion < 1 || s.ResourceVersion < 1 || s.Capacity < 1 || !s.StartsAt.After(o.CreatedAt) || !s.EndsAt.After(s.StartsAt) || strings.TrimSpace(s.ServiceName) == "" || strings.TrimSpace(s.ResourceName) == "" {
			return invalid("invalid durable choice")
		}
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			return invalid("invalid slot timezone")
		}
		seen[c.Token] = true
		events[s.EventID] = true
	}
	return nil
}
func ValidateConfirmation(o Offer, b Binding, token string, offerSent bool, now time.Time) (Choice, error) {
	if err := validateOffer(o); err != nil {
		return Choice{}, err
	}
	if o.Binding != b || !offerSent || now.Before(o.CreatedAt) || !now.Before(o.ExpiresAt) || !validToken(token) {
		return Choice{}, conflict("Booking offer is unavailable or expired; request a fresh offer")
	}
	for _, c := range o.Choices {
		if subtle.ConstantTimeCompare([]byte(c.Token), []byte(token)) == 1 {
			return c, nil
		}
	}
	return Choice{}, invalid("confirmation does not belong to this offer")
}
func OfferJSON(o Offer) (models.JSONB, error) {
	if err := validateOffer(o); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	var out models.JSONB
	err = json.Unmarshal(raw, &out)
	return out, err
}
func ParseOffer(value any) (Offer, error) {
	var o Offer
	raw, err := json.Marshal(value)
	if err != nil {
		return o, err
	}
	if err := strictJSON(raw, &o); err != nil {
		return o, err
	}
	return o, validateOffer(o)
}
func OfferFingerprint(o Offer) (string, error) {
	if err := validateOffer(o); err != nil {
		return "", err
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("rereply:ai-booking:offer:v1\n"), raw...))
	return hex.EncodeToString(sum[:]), nil
}
func ReservationKey(offerID uuid.UUID, token string) string {
	sum := sha256.Sum256([]byte("rereply:ai-booking:reservation:v1\n" + offerID.String() + "\n" + token))
	return "ai-booking:" + hex.EncodeToString(sum[:])
}
func RenderOffer(o Offer) string {
	if validateOffer(o) != nil {
		return "Please ask staff to help arrange your booking."
	}
	var b strings.Builder
	b.WriteString("Available booking choices (one place; no payment taken):\n")
	for i, c := range o.Choices {
		location, _ := time.LoadLocation(c.Slot.Timezone)
		fmt.Fprintf(&b, "%d. %s — %s — %s (%s) — %s\nTo confirm this choice, send exactly: BOOK %s\n", i+1, c.Slot.ServiceName, c.Slot.ResourceName, c.Slot.StartsAt.In(location).Format("2006-01-02 15:04"), c.Slot.Timezone, c.Slot.Location, c.Token)
	}
	b.WriteString("Choices expire soon and capacity is checked again on confirmation.")
	return b.String()
}
func RenderConfirmation(c Choice) string {
	location, err := time.LoadLocation(c.Slot.Timezone)
	if err != nil {
		return "Your booking is reserved. Please ask staff for the appointment details."
	}
	return fmt.Sprintf("Your place is reserved: %s with %s on %s (%s), %s. No payment was taken.", c.Slot.ServiceName, c.Slot.ResourceName, c.Slot.StartsAt.In(location).Format("2006-01-02 15:04"), c.Slot.Timezone, c.Slot.Location)
}
