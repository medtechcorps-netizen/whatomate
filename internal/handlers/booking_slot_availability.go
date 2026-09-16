package handlers

import (
	"errors"
	"time"

	"github.com/google/uuid"
	bookingcore "github.com/shridarpatil/whatomate/internal/booking"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
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
	filter := bookingcore.SlotFilter{From: *from, To: *to, Limit: pg.Limit, Offset: pg.Offset}
	filter.ServiceID, err = productCRMQueryUUID(r, "service_id")
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid service ID", nil, "")
	}
	filter.ResourceID, err = productCRMQueryUUID(r, "resource_id")
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid resource ID", nil, "")
	}
	page, err := bookingcore.ListSlots(a.DB, orgID, filter)
	if err != nil {
		return a.sendBookingCommerceError(r, "list booking availability", err)
	}
	slots := make([]BookingAvailabilitySlotResponse, 0, len(page.Slots))
	for _, slot := range page.Slots {
		slots = append(slots, BookingAvailabilitySlotResponse{
			BookingEventResponse: BookingEventResponse{BookingEvent: slot.Event, BookedQuantity: slot.BookedQuantity},
			RemainingCapacity:    slot.RemainingCapacity, Timezone: slot.Timezone,
			LocalStartsAt: slot.LocalStartsAt, LocalEndsAt: slot.LocalEndsAt,
		})
	}
	return r.SendEnvelope(listEnvelope("slots", slots, page.Total, pg))
}

// Compatibility wrappers retain staff/test typed errors while the shared core
// owns the authoritative locks and post-lock start rule.
func lockBookableEventForReservation(tx *gorm.DB, orgID, eventID uuid.UUID) (*models.BookingEvent, error) {
	event, err := bookingcore.LockEvent(tx, orgID, eventID)
	return event, bookingCoreClientError(err)
}
func ensureBookingEventHasNotStarted(event *models.BookingEvent, now time.Time) error {
	return bookingCoreClientError(bookingcore.EnsureNotStarted(event, now))
}
func bookingCoreClientError(err error) error {
	var domain *bookingcore.Error
	if errors.As(err, &domain) {
		status := fasthttp.StatusConflict
		if domain.Kind == "invalid" {
			status = fasthttp.StatusBadRequest
		}
		return newBookingCommerceClientError(status, domain.Message)
	}
	return err
}
