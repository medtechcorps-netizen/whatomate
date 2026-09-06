package handlers

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const defaultBookingAvailabilityDays = 30

// BookingAvailabilitySlotResponse is the authoritative, bookable projection
// consumed by staff and automated booking clients. Local timestamps are
// derived from the resource timezone; capacity is always computed from live
// booking rows rather than a cached counter.
type BookingAvailabilitySlotResponse struct {
	BookingEventResponse
	RemainingCapacity int    `json:"remaining_capacity"`
	Timezone          string `json:"timezone"`
	LocalStartsAt     string `json:"local_starts_at"`
	LocalEndsAt       string `json:"local_ends_at"`
}

// bookingAvailabilityEventRow keeps the capacity used to qualify a slot in
// the same database statement as the paginated event row. A separate capacity
// read after pagination can legitimately observe a newer booking and drop a
// row, producing a short page that clients mistake for the end of the result
// set. CreateBooking still rechecks capacity under the event lock.
type bookingAvailabilityEventRow struct {
	models.BookingEvent
	BookedQuantity int `gorm:"column:booked_quantity"`
}

// ListBookingAvailability lists only concrete events that can currently
// accept a reservation. The default window begins now and spans 30 days.
func (a *App) ListBookingAvailability(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceBookings, models.ActionRead)
	if err != nil {
		return nil
	}

	now := time.Now().UTC()
	from, err := bookingCommerceQueryTime(r, "from")
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "from must be RFC3339", nil, "")
	}
	if from == nil || from.Before(now) {
		from = &now
	}
	to, err := bookingCommerceQueryTime(r, "to")
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "to must be RFC3339", nil, "")
	}
	if to == nil {
		defaultTo := from.AddDate(0, 0, defaultBookingAvailabilityDays)
		to = &defaultTo
	}
	if !to.After(*from) {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"to must be later than the effective from time",
			nil,
			"",
		)
	}

	pg := parsePaginationWithDefaults(r, 100, 250)
	occupiedCapacity := a.DB.Model(&models.Booking{}).
		Select("bookings.event_id, COALESCE(SUM(bookings.quantity), 0) AS booked_quantity").
		Where(
			"bookings.organization_id = ? AND bookings.status IN ?",
			orgID,
			bookingCapacityStatuses(),
		).
		Group("bookings.event_id")
	query := a.DB.Model(&models.BookingEvent{}).
		Joins(
			"JOIN booking_services AS available_services ON "+
				"available_services.id = booking_events.service_id AND "+
				"available_services.organization_id = booking_events.organization_id AND "+
				"available_services.is_active = ? AND available_services.deleted_at IS NULL",
			true,
		).
		Joins(
			"JOIN booking_resources AS available_resources ON "+
				"available_resources.id = booking_events.resource_id AND "+
				"available_resources.organization_id = booking_events.organization_id AND "+
				"available_resources.is_active = ? AND available_resources.deleted_at IS NULL",
			true,
		).
		Joins(
			"LEFT JOIN (?) AS available_capacity ON available_capacity.event_id = booking_events.id",
			occupiedCapacity,
		).
		Where(
			"booking_events.organization_id = ? AND booking_events.status = ?",
			orgID,
			models.BookingEventStatusScheduled,
		).
		Where("booking_events.starts_at >= ? AND booking_events.starts_at <= ?", *from, *to).
		Where("booking_events.capacity > COALESCE(available_capacity.booked_quantity, 0)")

	for _, filter := range []struct {
		param  string
		column string
		label  string
	}{
		{param: "service_id", column: "booking_events.service_id", label: "service"},
		{param: "resource_id", column: "booking_events.resource_id", label: "resource"},
	} {
		value, parseErr := productCRMQueryUUID(r, filter.param)
		if parseErr != nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"Invalid "+filter.label+" ID",
				nil,
				"",
			)
		}
		if value != nil {
			query = query.Where(filter.column+" = ?", *value)
		}
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		a.Log.Error("Failed to count booking availability", "error", err)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to list booking availability",
			nil,
			"",
		)
	}
	var rows []bookingAvailabilityEventRow
	if err := pg.Apply(query.
		Select(
			"booking_events.*, COALESCE(available_capacity.booked_quantity, 0) AS booked_quantity",
		).
		Preload("Service", "organization_id = ? AND is_active = ?", orgID, true).
		Preload("Resource", "organization_id = ? AND is_active = ?", orgID, true).
		Order("booking_events.starts_at ASC, booking_events.id ASC")).
		Find(&rows).Error; err != nil {
		a.Log.Error("Failed to load booking availability", "error", err)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to list booking availability",
			nil,
			"",
		)
	}

	slots := make([]BookingAvailabilitySlotResponse, 0, len(rows))
	for i := range rows {
		event := rows[i].BookingEvent
		bookedQuantity := rows[i].BookedQuantity
		remainingCapacity := event.Capacity - bookedQuantity
		if event.Resource == nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusInternalServerError,
				"Failed to resolve booking slot resource",
				nil,
				"",
			)
		}
		location, err := time.LoadLocation(event.Resource.Timezone)
		if err != nil {
			a.Log.Error(
				"Booking resource has invalid timezone",
				"resource_id", event.ResourceID,
				"timezone", event.Resource.Timezone,
				"error", err,
			)
			return r.SendErrorEnvelope(
				fasthttp.StatusInternalServerError,
				"Failed to resolve booking slot timezone",
				nil,
				"",
			)
		}
		if strings.TrimSpace(event.Location) == "" {
			event.Location = strings.TrimSpace(event.Resource.Location)
		}
		slots = append(slots, BookingAvailabilitySlotResponse{
			BookingEventResponse: BookingEventResponse{
				BookingEvent:   event,
				BookedQuantity: bookedQuantity,
			},
			RemainingCapacity: remainingCapacity,
			Timezone:          event.Resource.Timezone,
			LocalStartsAt:     event.StartsAt.In(location).Format(time.RFC3339),
			LocalEndsAt:       event.EndsAt.In(location).Format(time.RFC3339),
		})
	}

	return r.SendEnvelope(listEnvelope("slots", slots, total, pg))
}

// lockBookableEventForReservation is the final authority for manual and
// automated reservations. It deliberately runs inside the existing booking
// transaction after idempotency has been checked. The start-time boundary is
// checked by CreateBooking only after this lock set and its post-lock
// idempotency lookup, so a completed identical request can still be replayed.
func lockBookableEventForReservation(
	tx *gorm.DB,
	orgID, eventID uuid.UUID,
) (*models.BookingEvent, error) {
	var event models.BookingEvent
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", eventID, orgID).
		First(&event).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		return nil, newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Booking slot is no longer available; refresh available slots and choose another",
		)
	}
	if event.Status != models.BookingEventStatusScheduled {
		return nil, newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Booking slot is no longer scheduled; refresh available slots and choose another",
		)
	}
	var service models.BookingService
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ? AND is_active = ?", event.ServiceID, orgID, true).
		First(&service).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		return nil, newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Booking service is inactive; refresh available slots and choose another",
		)
	}
	var resource models.BookingResource
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ? AND is_active = ?", event.ResourceID, orgID, true).
		First(&resource).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		return nil, newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Booking practitioner or resource is inactive; refresh available slots and choose another",
		)
	}
	return &event, nil
}

func ensureBookingEventHasNotStarted(event *models.BookingEvent, now time.Time) error {
	if event.StartsAt.After(now.UTC()) {
		return nil
	}
	return newBookingCommerceClientError(
		fasthttp.StatusConflict,
		"Booking slot has already started; refresh available slots and choose another",
	)
}
