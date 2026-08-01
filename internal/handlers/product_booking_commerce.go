package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	bookingServiceAuditResource  = "booking_service"
	bookingResourceAuditResource = "booking_resource"
	bookingEventAuditResource    = "booking_event"
	bookingAuditResource         = "booking"
	packageAuditResource         = "package_definition"
	contactPackageAuditResource  = "contact_package"
	commerceInvoiceAuditResource = "commerce_invoice"
	paymentIntentAuditResource   = "payment_intent"
	paymentAuditResource         = "payment_transaction"
)

type bookingCommerceClientError struct {
	status  int
	message string
}

func (e *bookingCommerceClientError) Error() string {
	return e.message
}

func newBookingCommerceClientError(status int, message string) error {
	return &bookingCommerceClientError{status: status, message: message}
}

func (a *App) sendBookingCommerceError(
	r *fastglue.Request,
	operation string,
	err error,
) error {
	var clientErr *bookingCommerceClientError
	var crmClientErr *productCRMClientError
	switch {
	case errors.As(err, &clientErr):
		return r.SendErrorEnvelope(clientErr.status, clientErr.message, nil, "")
	case errors.As(err, &crmClientErr):
		return r.SendErrorEnvelope(crmClientErr.status, crmClientErr.message, nil, "")
	default:
		a.Log.Error("Booking/commerce operation failed", "operation", operation, "error", err)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to "+operation,
			nil,
			"",
		)
	}
}

// BookingServiceRequest is shared by create and full-field update contracts.
type BookingServiceRequest struct {
	Name                string                    `json:"name"`
	Description         string                    `json:"description,omitempty"`
	Kind                models.BookingServiceKind `json:"kind"`
	DurationMinutes     int                       `json:"duration_minutes"`
	BufferBeforeMinutes int                       `json:"buffer_before_minutes"`
	BufferAfterMinutes  int                       `json:"buffer_after_minutes"`
	DefaultCapacity     int                       `json:"default_capacity"`
	PriceMinor          int64                     `json:"price_minor"`
	Currency            string                    `json:"currency"`
	ReminderPolicy      models.JSONB              `json:"reminder_policy,omitempty"`
	IsActive            *bool                     `json:"is_active,omitempty"`
	Metadata            models.JSONB              `json:"metadata,omitempty"`
	ResourceIDs         []uuid.UUID               `json:"resource_ids,omitempty"`
	Version             int64                     `json:"version,omitempty"`
}

// BookingResourceRequest creates or updates a schedulable resource.
type BookingResourceRequest struct {
	UserID      *uuid.UUID                 `json:"user_id,omitempty"`
	ClearUserID bool                       `json:"clear_user_id,omitempty"`
	Name        string                     `json:"name"`
	Kind        models.BookingResourceKind `json:"kind"`
	Timezone    string                     `json:"timezone"`
	Location    string                     `json:"location,omitempty"`
	IsActive    *bool                      `json:"is_active,omitempty"`
	Metadata    models.JSONB               `json:"metadata,omitempty"`
	Version     int64                      `json:"version,omitempty"`
}

// BookingEventRequest creates or updates one concrete calendar slot.
type BookingEventRequest struct {
	ServiceID        uuid.UUID                 `json:"service_id"`
	ResourceID       uuid.UUID                 `json:"resource_id"`
	StartsAt         time.Time                 `json:"starts_at"`
	EndsAt           time.Time                 `json:"ends_at"`
	LocalStartsAt    string                    `json:"local_starts_at,omitempty"`
	LocalEndsAt      string                    `json:"local_ends_at,omitempty"`
	Timezone         string                    `json:"timezone,omitempty"`
	Capacity         int                       `json:"capacity"`
	Status           models.BookingEventStatus `json:"status,omitempty"`
	ExternalProvider string                    `json:"external_provider,omitempty"`
	ExternalEventID  string                    `json:"external_event_id,omitempty"`
	Location         string                    `json:"location,omitempty"`
	Metadata         models.JSONB              `json:"metadata,omitempty"`
	Version          int64                     `json:"version,omitempty"`
}

// CreateBookingRequest reserves capacity and, when supplied, package credit.
type CreateBookingRequest struct {
	EventID          uuid.UUID            `json:"event_id"`
	ContactID        uuid.UUID            `json:"contact_id"`
	Quantity         int                  `json:"quantity"`
	Status           models.BookingStatus `json:"status,omitempty"`
	Source           models.BookingSource `json:"source,omitempty"`
	Notes            string               `json:"notes,omitempty"`
	ContactPackageID *uuid.UUID           `json:"contact_package_id,omitempty"`
	AllowWaitlist    bool                 `json:"allow_waitlist,omitempty"`
	IdempotencyKey   string               `json:"idempotency_key"`
	Metadata         models.JSONB         `json:"metadata,omitempty"`
}

// TransitionBookingRequest is guarded by the booking's optimistic version.
type TransitionBookingRequest struct {
	Status  models.BookingStatus `json:"status"`
	Version int64                `json:"version"`
	Reason  string               `json:"reason,omitempty"`
}

// PackageEntitlementInput defines finite or unlimited service credit.
type PackageEntitlementInput struct {
	BookingServiceID uuid.UUID `json:"booking_service_id"`
	Credits          int       `json:"credits"`
	IsUnlimited      bool      `json:"is_unlimited"`
}

// PackageRequest creates or updates a package definition.
type PackageRequest struct {
	Name         string                    `json:"name"`
	Description  string                    `json:"description,omitempty"`
	PriceMinor   int64                     `json:"price_minor"`
	Currency     string                    `json:"currency"`
	ValidityDays int                       `json:"validity_days"`
	IsActive     *bool                     `json:"is_active,omitempty"`
	Metadata     models.JSONB              `json:"metadata,omitempty"`
	Entitlements []PackageEntitlementInput `json:"entitlements,omitempty"`
	Version      int64                     `json:"version,omitempty"`
}

// CreateContactPackageRequest purchases or grants a package to a contact.
type CreateContactPackageRequest struct {
	ContactID           uuid.UUID    `json:"contact_id"`
	PackageDefinitionID uuid.UUID    `json:"package_definition_id"`
	InvoiceID           *uuid.UUID   `json:"invoice_id,omitempty"`
	StartsAt            *time.Time   `json:"starts_at,omitempty"`
	Source              string       `json:"source,omitempty"`
	IdempotencyKey      string       `json:"idempotency_key"`
	Metadata            models.JSONB `json:"metadata,omitempty"`
}

// SellContactPackageRequest atomically invoices and assigns one package.
type SellContactPackageRequest struct {
	ContactID           uuid.UUID    `json:"contact_id"`
	PackageDefinitionID uuid.UUID    `json:"package_definition_id"`
	DueAt               *time.Time   `json:"due_at,omitempty"`
	IdempotencyKey      string       `json:"idempotency_key"`
	Metadata            models.JSONB `json:"metadata,omitempty"`
}

// SellContactPackageResponse returns both records committed by the sale.
type SellContactPackageResponse struct {
	Invoice        models.CommerceInvoice `json:"invoice"`
	ContactPackage models.ContactPackage  `json:"contact_package"`
}

// CommerceInvoiceLineInput supports one linked domain item or a custom line.
type CommerceInvoiceLineInput struct {
	BookingID           *uuid.UUID   `json:"booking_id,omitempty"`
	ContactPackageID    *uuid.UUID   `json:"contact_package_id,omitempty"`
	PackageDefinitionID *uuid.UUID   `json:"package_definition_id,omitempty"`
	Description         string       `json:"description,omitempty"`
	Quantity            int          `json:"quantity,omitempty"`
	UnitAmountMinor     *int64       `json:"unit_amount_minor,omitempty"`
	TaxMinor            int64        `json:"tax_minor,omitempty"`
	Metadata            models.JSONB `json:"metadata,omitempty"`
}

// CreateCommerceInvoiceRequest contains no client-computed totals.
type CreateCommerceInvoiceRequest struct {
	ContactID      uuid.UUID                  `json:"contact_id"`
	Currency       string                     `json:"currency"`
	DiscountMinor  int64                      `json:"discount_minor,omitempty"`
	DueAt          *time.Time                 `json:"due_at,omitempty"`
	Lines          []CommerceInvoiceLineInput `json:"lines"`
	IdempotencyKey string                     `json:"idempotency_key"`
	Metadata       models.JSONB               `json:"metadata,omitempty"`
}

// CreateInvoicePaymentIntentRequest records provider-neutral collection intent.
type CreateInvoicePaymentIntentRequest struct {
	ProviderAccountID uuid.UUID    `json:"provider_account_id"`
	AmountMinor       *int64       `json:"amount_minor,omitempty"`
	IdempotencyKey    string       `json:"idempotency_key"`
	ExpiresAt         *time.Time   `json:"expires_at,omitempty"`
	Metadata          models.JSONB `json:"metadata,omitempty"`
}

// RecordManualInvoicePaymentRequest requires explicit human confirmation.
type RecordManualInvoicePaymentRequest struct {
	Version           int64        `json:"version"`
	AmountMinor       int64        `json:"amount_minor"`
	Currency          string       `json:"currency"`
	ProviderAccountID *uuid.UUID   `json:"provider_account_id,omitempty"`
	Reference         string       `json:"reference,omitempty"`
	Notes             string       `json:"notes,omitempty"`
	IdempotencyKey    string       `json:"idempotency_key"`
	OccurredAt        *time.Time   `json:"occurred_at,omitempty"`
	ConfirmManual     bool         `json:"confirm_manual"`
	Metadata          models.JSONB `json:"metadata,omitempty"`
}

// PaymentIntentResponse makes it explicit that no external adapter was called.
type PaymentIntentResponse struct {
	Intent           models.PaymentIntent `json:"intent"`
	CollectionStatus string               `json:"collection_status"`
	Message          string               `json:"message"`
}

type CurrencyAmountSummary struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
}

type CommerceSummaryResponse struct {
	ActivePackages   int64                   `json:"active_packages"`
	PackagesVisible  bool                    `json:"packages_visible"`
	Outstanding      []CurrencyAmountSummary `json:"outstanding"`
	CollectedCharges []CurrencyAmountSummary `json:"collected_charges"`
}

// BookingServiceResponse includes the eligible resource IDs.
type BookingServiceResponse struct {
	models.BookingService
	ResourceIDs []uuid.UUID `json:"resource_ids"`
}

// BookingEventResponse exposes current occupied capacity without trusting a
// separately maintained counter.
type BookingEventResponse struct {
	models.BookingEvent
	BookedQuantity int `json:"booked_quantity"`
}

// ListBookingServices lists tenant booking services.
func (a *App) ListBookingServices(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.BookingService{}).Where("organization_id = ?", orgID)
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("active"))); raw != "" {
		active, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "active must be true or false", nil, "")
		}
		query = query.Where("is_active = ?", active)
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("kind"))); raw != "" {
		kind := models.BookingServiceKind(raw)
		if !validBookingServiceKind(kind) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid booking service kind", nil, "")
		}
		query = query.Where("kind = ?", kind)
	}
	if search := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("search"))); search != "" {
		query = query.Where("name ILIKE ?", productCRMSearchPattern(search))
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list booking services", nil, "")
	}
	var services []models.BookingService
	if err := pg.Apply(query.Order("name ASC")).Find(&services).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list booking services", nil, "")
	}
	resources, err := loadBookingServiceResourceIDs(a.DB, orgID, services)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list booking services", nil, "")
	}
	response := make([]BookingServiceResponse, len(services))
	for i := range services {
		response[i] = BookingServiceResponse{
			BookingService: services[i],
			ResourceIDs:    resources[services[i].ID],
		}
		if response[i].ResourceIDs == nil {
			response[i].ResourceIDs = []uuid.UUID{}
		}
	}
	return r.SendEnvelope(listEnvelope("services", response, total, pg))
}

// CreateBookingService creates a service and optional resource links atomically.
func (a *App) CreateBookingService(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionWrite)
	if err != nil {
		return nil
	}
	var req BookingServiceRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateBookingServiceRequest(&req, false); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}
	service := models.BookingService{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgID,
		Name:             strings.TrimSpace(req.Name),
		Description:      strings.TrimSpace(req.Description),
		Kind:             req.Kind,
		DurationMinutes:  req.DurationMinutes,
		BufferBeforeMins: req.BufferBeforeMinutes,
		BufferAfterMins:  req.BufferAfterMinutes,
		DefaultCapacity:  req.DefaultCapacity,
		PriceMinor:       req.PriceMinor,
		Currency:         strings.ToUpper(strings.TrimSpace(req.Currency)),
		ReminderPolicy:   bookingCommerceJSON(req.ReminderPolicy),
		IsActive:         active,
		Metadata:         bookingCommerceJSON(req.Metadata),
		Version:          1,
		CreatedByID:      &userID,
		UpdatedByID:      &userID,
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := validateBookingResourceIDs(tx, orgID, req.ResourceIDs); err != nil {
			return err
		}
		if err := ensureBookingCommerceUniqueName(
			tx,
			&models.BookingService{},
			orgID,
			service.Name,
			uuid.Nil,
			"booking service",
		); err != nil {
			return err
		}
		if err := tx.Create(&service).Error; err != nil {
			return err
		}
		if err := replaceBookingServiceResources(tx, orgID, service.ID, req.ResourceIDs); err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			bookingServiceAuditResource,
			service.ID,
			models.AuditActionCreated,
			nil,
			&service,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "create booking service", err)
	}
	return r.SendEnvelope(BookingServiceResponse{
		BookingService: service,
		ResourceIDs:    req.ResourceIDs,
	})
}

// UpdateBookingService replaces service fields and resource links with a
// version-guarded transaction.
func (a *App) UpdateBookingService(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionWrite)
	if err != nil {
		return nil
	}
	serviceID, err := parsePathUUID(r, "id", "booking service")
	if err != nil {
		return nil
	}
	var req BookingServiceRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateBookingServiceRequest(&req, true); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var updated models.BookingService
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var service models.BookingService
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", serviceID, orgID).
			First(&service).Error; err != nil {
			return newBookingCommerceClientError(fasthttp.StatusNotFound, "Booking service not found")
		}
		if service.Version != req.Version {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Booking service was modified; refresh and retry",
			)
		}
		if err := validateBookingResourceIDs(tx, orgID, req.ResourceIDs); err != nil {
			return err
		}
		if err := ensureBookingCommerceUniqueName(
			tx,
			&models.BookingService{},
			orgID,
			strings.TrimSpace(req.Name),
			serviceID,
			"booking service",
		); err != nil {
			return err
		}
		old := service
		active := service.IsActive
		if req.IsActive != nil {
			active = *req.IsActive
		}
		result := tx.Model(&models.BookingService{}).
			Where("id = ? AND organization_id = ? AND version = ?", serviceID, orgID, req.Version).
			Updates(map[string]any{
				"name":               strings.TrimSpace(req.Name),
				"description":        strings.TrimSpace(req.Description),
				"kind":               req.Kind,
				"duration_minutes":   req.DurationMinutes,
				"buffer_before_mins": req.BufferBeforeMinutes,
				"buffer_after_mins":  req.BufferAfterMinutes,
				"default_capacity":   req.DefaultCapacity,
				"price_minor":        req.PriceMinor,
				"currency":           strings.ToUpper(strings.TrimSpace(req.Currency)),
				"reminder_policy":    bookingCommerceJSON(req.ReminderPolicy),
				"is_active":          active,
				"metadata":           bookingCommerceJSON(req.Metadata),
				"updated_by_id":      userID,
				"version":            gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Booking service was modified; refresh and retry",
			)
		}
		if err := replaceBookingServiceResources(tx, orgID, serviceID, req.ResourceIDs); err != nil {
			return err
		}
		if err := tx.Where("id = ? AND organization_id = ?", serviceID, orgID).
			First(&updated).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			bookingServiceAuditResource,
			serviceID,
			models.AuditActionUpdated,
			&old,
			&updated,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "update booking service", err)
	}
	return r.SendEnvelope(BookingServiceResponse{
		BookingService: updated,
		ResourceIDs:    req.ResourceIDs,
	})
}

// ListBookingResources lists schedulable tenant resources.
func (a *App) ListBookingResources(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.BookingResource{}).Where("organization_id = ?", orgID)
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("active"))); raw != "" {
		active, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "active must be true or false", nil, "")
		}
		query = query.Where("is_active = ?", active)
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("kind"))); raw != "" {
		kind := models.BookingResourceKind(raw)
		if !validBookingResourceKind(kind) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid booking resource kind", nil, "")
		}
		query = query.Where("kind = ?", kind)
	}
	if search := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("search"))); search != "" {
		query = query.Where("(name ILIKE ? OR location ILIKE ?)",
			productCRMSearchPattern(search), productCRMSearchPattern(search))
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list booking resources", nil, "")
	}
	var resources []models.BookingResource
	if err := pg.Apply(query.Order("name ASC")).Find(&resources).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list booking resources", nil, "")
	}
	return r.SendEnvelope(listEnvelope("resources", resources, total, pg))
}

// CreateBookingResource creates a tenant resource after timezone/member checks.
func (a *App) CreateBookingResource(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionWrite)
	if err != nil {
		return nil
	}
	var req BookingResourceRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateBookingResourceRequest(&req, false); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}
	resource := models.BookingResource{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		UserID:         req.UserID,
		Name:           strings.TrimSpace(req.Name),
		Kind:           req.Kind,
		Timezone:       strings.TrimSpace(req.Timezone),
		Location:       strings.TrimSpace(req.Location),
		IsActive:       active,
		Metadata:       bookingCommerceJSON(req.Metadata),
		Version:        1,
		CreatedByID:    &userID,
		UpdatedByID:    &userID,
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if resource.UserID != nil {
			if err := ensureProductCRMUserMembership(
				tx,
				orgID,
				*resource.UserID,
				"user_id",
			); err != nil {
				return err
			}
		}
		if err := ensureBookingCommerceUniqueName(
			tx,
			&models.BookingResource{},
			orgID,
			resource.Name,
			uuid.Nil,
			"booking resource",
		); err != nil {
			return err
		}
		if err := tx.Create(&resource).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			bookingResourceAuditResource, resource.ID,
			models.AuditActionCreated, nil, &resource,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "create booking resource", err)
	}
	return r.SendEnvelope(resource)
}

// UpdateBookingResource updates a resource with optimistic concurrency.
func (a *App) UpdateBookingResource(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionWrite)
	if err != nil {
		return nil
	}
	resourceID, err := parsePathUUID(r, "id", "booking resource")
	if err != nil {
		return nil
	}
	var req BookingResourceRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateBookingResourceRequest(&req, true); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	var updated models.BookingResource
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var resource models.BookingResource
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", resourceID, orgID).
			First(&resource).Error; err != nil {
			return newBookingCommerceClientError(fasthttp.StatusNotFound, "Booking resource not found")
		}
		if resource.Version != req.Version {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Booking resource was modified; refresh and retry",
			)
		}
		if req.UserID != nil {
			if err := ensureProductCRMUserMembership(tx, orgID, *req.UserID, "user_id"); err != nil {
				return err
			}
		}
		if err := ensureBookingCommerceUniqueName(
			tx,
			&models.BookingResource{},
			orgID,
			strings.TrimSpace(req.Name),
			resourceID,
			"booking resource",
		); err != nil {
			return err
		}
		old := resource
		userRef := resource.UserID
		if req.ClearUserID {
			userRef = nil
		} else if req.UserID != nil {
			userRef = req.UserID
		}
		active := resource.IsActive
		if req.IsActive != nil {
			active = *req.IsActive
		}
		result := tx.Model(&models.BookingResource{}).
			Where("id = ? AND organization_id = ? AND version = ?", resourceID, orgID, req.Version).
			Updates(map[string]any{
				"user_id":       userRef,
				"name":          strings.TrimSpace(req.Name),
				"kind":          req.Kind,
				"timezone":      strings.TrimSpace(req.Timezone),
				"location":      strings.TrimSpace(req.Location),
				"is_active":     active,
				"metadata":      bookingCommerceJSON(req.Metadata),
				"updated_by_id": userID,
				"version":       gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Booking resource was modified; refresh and retry",
			)
		}
		if err := tx.Where("id = ? AND organization_id = ?", resourceID, orgID).
			First(&updated).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			bookingResourceAuditResource, resourceID,
			models.AuditActionUpdated, &old, &updated,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "update booking resource", err)
	}
	return r.SendEnvelope(updated)
}

// ListBookingEvents lists concrete calendar slots with date/resource filters.
func (a *App) ListBookingEvents(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceBookings, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.BookingEvent{}).Where("booking_events.organization_id = ?", orgID)
	for _, filter := range []struct {
		param  string
		column string
		label  string
	}{
		{"service_id", "service_id", "service"},
		{"resource_id", "resource_id", "resource"},
	} {
		value, parseErr := productCRMQueryUUID(r, filter.param)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid "+filter.label+" ID", nil, "")
		}
		if value != nil {
			query = query.Where(filter.column+" = ?", *value)
		}
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("status"))); raw != "" {
		status := models.BookingEventStatus(raw)
		if !validBookingEventStatus(status) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid booking event status", nil, "")
		}
		query = query.Where("status = ?", status)
	}
	for _, filter := range []struct {
		param    string
		column   string
		operator string
	}{
		{"from", "starts_at", ">="},
		{"to", "starts_at", "<="},
	} {
		value, parseErr := bookingCommerceQueryTime(r, filter.param)
		if parseErr != nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				filter.param+" must be RFC3339",
				nil,
				"",
			)
		}
		if value != nil {
			query = query.Where(filter.column+" "+filter.operator+" ?", *value)
		}
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list booking events", nil, "")
	}
	var events []models.BookingEvent
	if err := pg.Apply(query.
		Preload("Service", "organization_id = ?", orgID).
		Preload("Resource", "organization_id = ?", orgID).
		Order("starts_at ASC")).
		Find(&events).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list booking events", nil, "")
	}
	occupied := make(map[uuid.UUID]int)
	if len(events) > 0 {
		eventIDs := make([]uuid.UUID, len(events))
		for i := range events {
			eventIDs[i] = events[i].ID
		}
		type eventQuantity struct {
			EventID  uuid.UUID
			Quantity int
		}
		var rows []eventQuantity
		if err := a.DB.Model(&models.Booking{}).
			Select("event_id, COALESCE(SUM(quantity), 0) AS quantity").
			Where(
				"organization_id = ? AND event_id IN ? AND status IN ?",
				orgID,
				eventIDs,
				bookingCapacityStatuses(),
			).
			Group("event_id").
			Scan(&rows).Error; err != nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusInternalServerError,
				"Failed to calculate booking capacity",
				nil,
				"",
			)
		}
		for _, row := range rows {
			occupied[row.EventID] = row.Quantity
		}
	}
	response := make([]BookingEventResponse, len(events))
	for i := range events {
		response[i] = BookingEventResponse{
			BookingEvent:   events[i],
			BookedQuantity: occupied[events[i].ID],
		}
	}
	return r.SendEnvelope(listEnvelope("events", response, total, pg))
}

// CreateBookingEvent creates a validated, non-overlapping event.
func (a *App) CreateBookingEvent(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookings, models.ActionWrite)
	if err != nil {
		return nil
	}
	var req BookingEventRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := normalizeBookingEventRequestTimes(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if err := validateBookingEventRequest(&req, false); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if req.Status == "" {
		req.Status = models.BookingEventStatusScheduled
	}
	event := models.BookingEvent{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgID,
		ServiceID:        req.ServiceID,
		ResourceID:       req.ResourceID,
		StartsAt:         req.StartsAt.UTC(),
		EndsAt:           req.EndsAt.UTC(),
		Capacity:         req.Capacity,
		Status:           req.Status,
		ExternalProvider: strings.TrimSpace(req.ExternalProvider),
		ExternalEventID:  strings.TrimSpace(req.ExternalEventID),
		Location:         strings.TrimSpace(req.Location),
		Metadata:         bookingCommerceJSON(req.Metadata),
		Version:          1,
		CreatedByID:      &userID,
		UpdatedByID:      &userID,
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		service, resource, err := validateBookingEventReferences(
			tx, orgID, event.ServiceID, event.ResourceID,
		)
		if err != nil {
			return err
		}
		if req.Timezone != "" && req.Timezone != resource.Timezone {
			return newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"timezone must match the selected booking resource",
			)
		}
		if event.EndsAt.Sub(event.StartsAt) < time.Duration(service.DurationMinutes)*time.Minute {
			return newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"Event duration cannot be shorter than the booking service duration",
			)
		}
		if event.Status == models.BookingEventStatusScheduled {
			if err := ensureBookingEventWithinRecurringAvailability(
				tx,
				orgID,
				event.ResourceID,
				event.StartsAt,
				event.EndsAt,
			); err != nil {
				return err
			}
		}
		if err := ensureBookingEventAvailable(
			tx, orgID, event.ResourceID, event.StartsAt, event.EndsAt, uuid.Nil,
		); err != nil {
			return err
		}
		if err := tx.Create(&event).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			bookingEventAuditResource, event.ID,
			models.AuditActionCreated, nil, &event,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "create booking event", err)
	}
	return r.SendEnvelope(event)
}

// UpdateBookingEvent updates a calendar slot with version and capacity checks.
func (a *App) UpdateBookingEvent(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookings, models.ActionWrite)
	if err != nil {
		return nil
	}
	eventID, err := parsePathUUID(r, "id", "booking event")
	if err != nil {
		return nil
	}
	var req BookingEventRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := normalizeBookingEventRequestTimes(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if err := validateBookingEventRequest(&req, true); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	var updated models.BookingEvent
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var event models.BookingEvent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", eventID, orgID).
			First(&event).Error; err != nil {
			return newBookingCommerceClientError(fasthttp.StatusNotFound, "Booking event not found")
		}
		if event.Version != req.Version {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Booking event was modified; refresh and retry",
			)
		}
		service, resource, err := validateBookingEventReferences(
			tx, orgID, req.ServiceID, req.ResourceID,
		)
		if err != nil {
			return err
		}
		if req.Timezone != "" && req.Timezone != resource.Timezone {
			return newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"timezone must match the selected booking resource",
			)
		}
		if req.EndsAt.Sub(req.StartsAt) < time.Duration(service.DurationMinutes)*time.Minute {
			return newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"Event duration cannot be shorter than the booking service duration",
			)
		}
		if req.Status == models.BookingEventStatusScheduled {
			if err := ensureBookingEventWithinRecurringAvailability(
				tx,
				orgID,
				req.ResourceID,
				req.StartsAt.UTC(),
				req.EndsAt.UTC(),
			); err != nil {
				return err
			}
		}
		if err := ensureBookingEventAvailable(
			tx, orgID, req.ResourceID, req.StartsAt.UTC(), req.EndsAt.UTC(), eventID,
		); err != nil {
			return err
		}
		occupied, err := bookingEventOccupiedQuantity(tx, orgID, eventID, uuid.Nil)
		if err != nil {
			return err
		}
		if req.Capacity < occupied {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				fmt.Sprintf("capacity cannot be below the current occupied quantity of %d", occupied),
			)
		}
		activeBookings, err := bookingEventActiveQuantity(tx, orgID, eventID)
		if err != nil {
			return err
		}
		if req.Status != models.BookingEventStatusScheduled && activeBookings > 0 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Transition or cancel active bookings before closing the event",
			)
		}
		old := event
		result := tx.Model(&models.BookingEvent{}).
			Where("id = ? AND organization_id = ? AND version = ?", eventID, orgID, req.Version).
			Updates(map[string]any{
				"service_id":        req.ServiceID,
				"resource_id":       req.ResourceID,
				"starts_at":         req.StartsAt.UTC(),
				"ends_at":           req.EndsAt.UTC(),
				"capacity":          req.Capacity,
				"status":            req.Status,
				"external_provider": strings.TrimSpace(req.ExternalProvider),
				"external_event_id": strings.TrimSpace(req.ExternalEventID),
				"location":          strings.TrimSpace(req.Location),
				"metadata":          bookingCommerceJSON(req.Metadata),
				"updated_by_id":     userID,
				"version":           gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Booking event was modified; refresh and retry",
			)
		}
		if err := tx.Where("id = ? AND organization_id = ?", eventID, orgID).
			First(&updated).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			bookingEventAuditResource, eventID,
			models.AuditActionUpdated, &old, &updated,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "update booking event", err)
	}
	return r.SendEnvelope(updated)
}

// ListBookings lists tenant bookings with event/contact filters.
func (a *App) ListBookings(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceBookings, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.Booking{}).Where("bookings.organization_id = ?", orgID)
	for _, filter := range []struct {
		param  string
		column string
		label  string
	}{
		{"event_id", "event_id", "event"},
		{"contact_id", "contact_id", "contact"},
		{"contact_package_id", "contact_package_id", "contact package"},
	} {
		value, parseErr := productCRMQueryUUID(r, filter.param)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid "+filter.label+" ID", nil, "")
		}
		if value != nil {
			query = query.Where(filter.column+" = ?", *value)
		}
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("status"))); raw != "" {
		status := models.BookingStatus(raw)
		if !validBookingStatus(status) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid booking status", nil, "")
		}
		query = query.Where("status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list bookings", nil, "")
	}
	var bookings []models.Booking
	if err := pg.Apply(query.
		Preload("Event", "organization_id = ?", orgID).
		Preload("Event.Service", "organization_id = ?", orgID).
		Preload("Event.Resource", "organization_id = ?", orgID).
		Preload("Contact", "organization_id = ?", orgID).
		Preload("ContactPackage", "organization_id = ?", orgID).
		Order("bookings.created_at DESC")).
		Find(&bookings).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list bookings", nil, "")
	}
	return r.SendEnvelope(listEnvelope("bookings", bookings, total, pg))
}

// CreateBooking locks the event before checking capacity and reserving credit.
func (a *App) CreateBooking(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookings, models.ActionWrite)
	if err != nil {
		return nil
	}
	eventID, err := parsePathUUID(r, "id", "booking event")
	if err != nil {
		return nil
	}
	var req CreateBookingRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.EventID != uuid.Nil && req.EventID != eventID {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"event_id must match the booking event in the request path",
			nil,
			"",
		)
	}
	req.EventID = eventID
	if err := validateCreateBookingRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if req.Status == "" {
		req.Status = models.BookingStatusReserved
	}
	if req.Source == "" {
		req.Source = models.BookingSourceAgent
	}
	var booking models.Booking
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Where(
			"organization_id = ? AND idempotency_key = ?",
			orgID,
			req.IdempotencyKey,
		).First(&booking).Error
		if err == nil {
			if booking.EventID != req.EventID ||
				booking.ContactID != req.ContactID ||
				booking.Quantity != req.Quantity ||
				!bookingCommerceUUIDPointersEqual(
					booking.ContactPackageID,
					req.ContactPackageID,
				) {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Idempotency key was already used for a different booking",
				)
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := ensureProductCRMTenantRecord(
			tx, &models.Contact{}, orgID, req.ContactID, "contact_id",
		); err != nil {
			return err
		}
		var event models.BookingEvent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"id = ? AND organization_id = ? AND status = ?",
				req.EventID, orgID, models.BookingEventStatusScheduled,
			).
			First(&event).Error; err != nil {
			return newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"event_id does not belong to an active scheduled event",
			)
		}
		err = tx.Where(
			"organization_id = ? AND idempotency_key = ?",
			orgID,
			req.IdempotencyKey,
		).First(&booking).Error
		if err == nil {
			if booking.EventID != req.EventID ||
				booking.ContactID != req.ContactID ||
				booking.Quantity != req.Quantity ||
				!bookingCommerceUUIDPointersEqual(
					booking.ContactPackageID,
					req.ContactPackageID,
				) {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Idempotency key was already used for a different booking",
				)
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		status := req.Status
		occupied, err := bookingEventOccupiedQuantity(tx, orgID, event.ID, uuid.Nil)
		if err != nil {
			return err
		}
		if bookingStatusOccupiesCapacity(status) && occupied+req.Quantity > event.Capacity {
			if !req.AllowWaitlist {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Booking event does not have enough remaining capacity",
				)
			}
			status = models.BookingStatusWaitlisted
		}

		now := time.Now().UTC()
		booking = models.Booking{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   orgID,
			EventID:          event.ID,
			ContactID:        req.ContactID,
			Status:           status,
			Quantity:         req.Quantity,
			Source:           req.Source,
			Notes:            strings.TrimSpace(req.Notes),
			ContactPackageID: req.ContactPackageID,
			BookedByID:       &userID,
			IdempotencyKey:   strings.TrimSpace(req.IdempotencyKey),
			Metadata:         bookingCommerceJSON(req.Metadata),
			Version:          1,
			UpdatedByID:      &userID,
		}
		if status == models.BookingStatusConfirmed {
			booking.ConfirmedAt = &now
		}
		if err := tx.Create(&booking).Error; err != nil {
			return err
		}
		if bookingStatusOccupiesCapacity(status) && booking.ContactPackageID != nil {
			if err := reserveBookingPackageCredit(
				tx, orgID, &booking, event.ServiceID, userID, now,
			); err != nil {
				return err
			}
		} else if booking.ContactPackageID != nil {
			if err := validateBookingContactPackage(
				tx, orgID, &booking, event.ServiceID, false,
			); err != nil {
				return err
			}
		}
		if bookingStatusOccupiesCapacity(status) {
			if err := bumpBookingEventVersion(tx, orgID, event.ID); err != nil {
				return err
			}
		}
		if _, err := recordCustomerActivity(tx, orgID, customerActivityInput{
			ContactID:        booking.ContactID,
			EventType:        models.CustomerActivityBookingCreated,
			Category:         models.CustomerActivityCategoryBooking,
			Title:            "Booking created",
			Summary:          event.StartsAt.Format(time.RFC3339),
			ActorType:        models.CustomerActivityActorUser,
			ActorUserID:      &userID,
			SourceObjectType: bookingAuditResource,
			SourceObjectID:   &booking.ID,
			OccurredAt:       now,
			Metadata: models.JSONB{
				"event_id": event.ID.String(),
				"status":   string(booking.Status),
				"quantity": booking.Quantity,
			},
			IdempotencyKey: "booking-created:" + booking.ID.String(),
		}); err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			bookingAuditResource, booking.ID,
			models.AuditActionCreated, nil, &booking,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "create booking", err)
	}
	if err := loadBookingForResponse(a.DB, orgID, &booking); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load booking", nil, "")
	}
	return r.SendEnvelope(booking)
}

// TransitionBooking applies the state machine with optimistic concurrency.
func (a *App) TransitionBooking(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookings, models.ActionWrite)
	if err != nil {
		return nil
	}
	bookingID, err := parsePathUUID(r, "id", "booking")
	if err != nil {
		return nil
	}
	var req TransitionBookingRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	transition, err := bookingStatusFromTransition(
		strings.TrimSpace(fmt.Sprint(r.RequestCtx.UserValue("transition"))),
	)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if req.Status != "" && req.Status != transition {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"status must match the transition in the request path",
			nil,
			"",
		)
	}
	req.Status = transition
	if err := validateTransitionBookingRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var updated models.Booking
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var booking models.Booking
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", bookingID, orgID).
			First(&booking).Error; err != nil {
			return newBookingCommerceClientError(fasthttp.StatusNotFound, "Booking not found")
		}
		if booking.Version != req.Version {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Booking was modified; refresh and retry",
			)
		}
		if !bookingTransitionAllowed(booking.Status, req.Status) {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				fmt.Sprintf("Cannot transition booking from %s to %s", booking.Status, req.Status),
			)
		}
		var event models.BookingEvent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", booking.EventID, orgID).
			First(&event).Error; err != nil {
			return newBookingCommerceClientError(fasthttp.StatusConflict, "Booking event is unavailable")
		}

		oldOccupies := bookingStatusOccupiesCapacity(booking.Status)
		newOccupies := bookingStatusOccupiesCapacity(req.Status)
		if !oldOccupies && newOccupies {
			occupied, err := bookingEventOccupiedQuantity(tx, orgID, event.ID, booking.ID)
			if err != nil {
				return err
			}
			if occupied+booking.Quantity > event.Capacity {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Booking event does not have enough remaining capacity",
				)
			}
		}

		now := time.Now().UTC()
		if !oldOccupies && newOccupies && booking.ContactPackageID != nil {
			if err := reserveBookingPackageCredit(
				tx, orgID, &booking, event.ServiceID, userID, now,
			); err != nil {
				return err
			}
		}
		if oldOccupies && req.Status == models.BookingStatusCancelled &&
			booking.ContactPackageID != nil {
			if err := releaseBookingPackageCredit(
				tx, orgID, &booking, event.ServiceID, userID, now,
			); err != nil {
				return err
			}
		}
		if oldOccupies &&
			(req.Status == models.BookingStatusCompleted ||
				req.Status == models.BookingStatusNoShow) &&
			booking.ContactPackageID != nil {
			if err := consumeBookingPackageCredit(
				tx, orgID, &booking, event.ServiceID, userID, now,
			); err != nil {
				return err
			}
		}

		old := booking
		updates := map[string]any{
			"status":        req.Status,
			"updated_by_id": userID,
			"version":       gorm.Expr("version + 1"),
		}
		switch req.Status {
		case models.BookingStatusConfirmed:
			updates["confirmed_at"] = now
		case models.BookingStatusCheckedIn:
			updates["checked_in_at"] = now
		case models.BookingStatusCompleted:
			updates["completed_at"] = now
		case models.BookingStatusNoShow:
			updates["no_show_at"] = now
		case models.BookingStatusCancelled:
			updates["cancelled_at"] = now
			updates["cancelled_by_id"] = userID
			updates["cancellation_reason"] = strings.TrimSpace(req.Reason)
		}
		result := tx.Model(&models.Booking{}).
			Where("id = ? AND organization_id = ? AND version = ?", bookingID, orgID, req.Version).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Booking was modified; refresh and retry",
			)
		}
		if oldOccupies != newOccupies {
			if err := bumpBookingEventVersion(tx, orgID, event.ID); err != nil {
				return err
			}
		}
		if err := tx.Where("id = ? AND organization_id = ?", bookingID, orgID).
			First(&updated).Error; err != nil {
			return err
		}
		if _, err := recordCustomerActivity(tx, orgID, customerActivityInput{
			ContactID:        updated.ContactID,
			EventType:        models.CustomerActivityBookingStatus,
			Category:         models.CustomerActivityCategoryBooking,
			Title:            "Booking status changed",
			Summary:          fmt.Sprintf("%s → %s", old.Status, updated.Status),
			ActorType:        models.CustomerActivityActorUser,
			ActorUserID:      &userID,
			SourceObjectType: bookingAuditResource,
			SourceObjectID:   &updated.ID,
			OccurredAt:       now,
			Metadata: models.JSONB{
				"event_id":    updated.EventID.String(),
				"from_status": string(old.Status),
				"to_status":   string(updated.Status),
				"version":     updated.Version,
				"reason":      strings.TrimSpace(req.Reason),
			},
			IdempotencyKey: fmt.Sprintf("booking-status:%s:%d", updated.ID, updated.Version),
		}); err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			bookingAuditResource, bookingID,
			models.AuditActionUpdated, &old, &updated,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "transition booking", err)
	}
	if err := loadBookingForResponse(a.DB, orgID, &updated); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load booking", nil, "")
	}
	return r.SendEnvelope(updated)
}

// ListPackages lists sellable package definitions and service entitlements.
func (a *App) ListPackages(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourcePackages, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.PackageDefinition{}).Where("organization_id = ?", orgID)
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("active"))); raw != "" {
		active, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "active must be true or false", nil, "")
		}
		query = query.Where("is_active = ?", active)
	}
	if search := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("search"))); search != "" {
		query = query.Where("name ILIKE ?", productCRMSearchPattern(search))
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list packages", nil, "")
	}
	var packages []models.PackageDefinition
	if err := pg.Apply(query.
		Preload("Entitlements", func(db *gorm.DB) *gorm.DB {
			return db.Where("organization_id = ?", orgID).Order("created_at ASC")
		}).
		Preload("Entitlements.BookingService", "organization_id = ?", orgID).
		Order("name ASC")).
		Find(&packages).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list packages", nil, "")
	}
	return r.SendEnvelope(listEnvelope("packages", packages, total, pg))
}

// CreatePackage creates a package and its entitlements atomically.
func (a *App) CreatePackage(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourcePackages, models.ActionWrite)
	if err != nil {
		return nil
	}
	var req PackageRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validatePackageRequest(&req, false); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}
	pkg := models.PackageDefinition{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		Name:           strings.TrimSpace(req.Name),
		Description:    strings.TrimSpace(req.Description),
		PriceMinor:     req.PriceMinor,
		Currency:       strings.ToUpper(strings.TrimSpace(req.Currency)),
		ValidityDays:   req.ValidityDays,
		IsActive:       active,
		Metadata:       bookingCommerceJSON(req.Metadata),
		Version:        1,
		CreatedByID:    &userID,
		UpdatedByID:    &userID,
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := validatePackageEntitlementReferences(tx, orgID, req.Entitlements); err != nil {
			return err
		}
		if err := ensureBookingCommerceUniqueName(
			tx, &models.PackageDefinition{}, orgID, pkg.Name, uuid.Nil, "package",
		); err != nil {
			return err
		}
		if err := tx.Create(&pkg).Error; err != nil {
			return err
		}
		pkg.Entitlements = packageEntitlementsFromInput(orgID, pkg.ID, req.Entitlements)
		if err := tx.Create(&pkg.Entitlements).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			packageAuditResource, pkg.ID,
			models.AuditActionCreated, nil, &pkg,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "create package", err)
	}
	return r.SendEnvelope(pkg)
}

// UpdatePackage updates commercial terms and, before any sale exists, may
// replace entitlements.
func (a *App) UpdatePackage(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourcePackages, models.ActionWrite)
	if err != nil {
		return nil
	}
	packageID, err := parsePathUUID(r, "id", "package")
	if err != nil {
		return nil
	}
	var req PackageRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validatePackageRequest(&req, true); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	var updated models.PackageDefinition
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var pkg models.PackageDefinition
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", packageID, orgID).
			First(&pkg).Error; err != nil {
			return newBookingCommerceClientError(fasthttp.StatusNotFound, "Package not found")
		}
		if pkg.Version != req.Version {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Package was modified; refresh and retry",
			)
		}
		if err := ensureBookingCommerceUniqueName(
			tx, &models.PackageDefinition{}, orgID,
			strings.TrimSpace(req.Name), packageID, "package",
		); err != nil {
			return err
		}
		if req.Entitlements != nil {
			var purchaseCount int64
			if err := tx.Model(&models.ContactPackage{}).
				Where("organization_id = ? AND package_definition_id = ?", orgID, packageID).
				Count(&purchaseCount).Error; err != nil {
				return err
			}
			if purchaseCount > 0 {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Package entitlements cannot change after the package has been purchased",
				)
			}
			if err := validatePackageEntitlementReferences(tx, orgID, req.Entitlements); err != nil {
				return err
			}
		}
		old := pkg
		active := pkg.IsActive
		if req.IsActive != nil {
			active = *req.IsActive
		}
		result := tx.Model(&models.PackageDefinition{}).
			Where("id = ? AND organization_id = ? AND version = ?", packageID, orgID, req.Version).
			Updates(map[string]any{
				"name":          strings.TrimSpace(req.Name),
				"description":   strings.TrimSpace(req.Description),
				"price_minor":   req.PriceMinor,
				"currency":      strings.ToUpper(strings.TrimSpace(req.Currency)),
				"validity_days": req.ValidityDays,
				"is_active":     active,
				"metadata":      bookingCommerceJSON(req.Metadata),
				"updated_by_id": userID,
				"version":       gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Package was modified; refresh and retry",
			)
		}
		if req.Entitlements != nil {
			if err := tx.Unscoped().
				Where("organization_id = ? AND package_definition_id = ?", orgID, packageID).
				Delete(&models.PackageEntitlement{}).Error; err != nil {
				return err
			}
			entitlements := packageEntitlementsFromInput(orgID, packageID, req.Entitlements)
			if err := tx.Create(&entitlements).Error; err != nil {
				return err
			}
		}
		if err := tx.Preload("Entitlements", "organization_id = ?", orgID).
			Where("id = ? AND organization_id = ?", packageID, orgID).
			First(&updated).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			packageAuditResource, packageID,
			models.AuditActionUpdated, &old, &updated,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "update package", err)
	}
	return r.SendEnvelope(updated)
}

// ListContactPackages lists packages owned by contacts and their balances.
func (a *App) ListContactPackages(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourcePackages, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.ContactPackage{}).Where("contact_packages.organization_id = ?", orgID)
	for _, filter := range []struct {
		param  string
		column string
		label  string
	}{
		{"contact_id", "contact_id", "contact"},
		{"package_definition_id", "package_definition_id", "package definition"},
		{"invoice_id", "invoice_id", "invoice"},
	} {
		value, parseErr := productCRMQueryUUID(r, filter.param)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid "+filter.label+" ID", nil, "")
		}
		if value != nil {
			query = query.Where(filter.column+" = ?", *value)
		}
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("status"))); raw != "" {
		status := models.ContactPackageStatus(raw)
		if !validContactPackageStatus(status) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact package status", nil, "")
		}
		query = query.Where("status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list contact packages", nil, "")
	}
	var packages []models.ContactPackage
	if err := pg.Apply(query.
		Preload("Contact", "organization_id = ?", orgID).
		Preload("PackageDefinition", "organization_id = ?", orgID).
		Preload("Balances", "organization_id = ?", orgID).
		Order("contact_packages.created_at DESC")).
		Find(&packages).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list contact packages", nil, "")
	}
	return r.SendEnvelope(listEnvelope("contact_packages", packages, total, pg))
}

// CreateContactPackage creates an idempotent purchase/grant and grants credits
// immediately only when no unpaid invoice is attached.
func (a *App) CreateContactPackage(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourcePackages, models.ActionWrite)
	if err != nil {
		return nil
	}
	var req CreateContactPackageRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateCreateContactPackageRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	var contactPackage models.ContactPackage
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Where(
			"organization_id = ? AND idempotency_key = ?",
			orgID, req.IdempotencyKey,
		).First(&contactPackage).Error
		if err == nil {
			if contactPackage.ContactID != req.ContactID ||
				contactPackage.PackageDefinitionID != req.PackageDefinitionID ||
				!bookingCommerceUUIDPointersEqual(contactPackage.InvoiceID, req.InvoiceID) {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Idempotency key was already used for a different package purchase",
				)
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := ensureProductCRMTenantRecord(
			tx, &models.Contact{}, orgID, req.ContactID, "contact_id",
		); err != nil {
			return err
		}
		var definition models.PackageDefinition
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND is_active = ?",
			req.PackageDefinitionID, orgID, true,
		).First(&definition).Error; err != nil {
			return newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"package_definition_id does not belong to an active package",
			)
		}
		err = tx.Where(
			"organization_id = ? AND idempotency_key = ?",
			orgID, req.IdempotencyKey,
		).First(&contactPackage).Error
		if err == nil {
			if contactPackage.ContactID != req.ContactID ||
				contactPackage.PackageDefinitionID != req.PackageDefinitionID ||
				!bookingCommerceUUIDPointersEqual(contactPackage.InvoiceID, req.InvoiceID) {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Idempotency key was already used for a different package purchase",
				)
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		status := models.ContactPackageStatusActive
		if req.InvoiceID != nil {
			var invoice models.CommerceInvoice
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
				"id = ? AND organization_id = ? AND contact_id = ?",
				*req.InvoiceID, orgID, req.ContactID,
			).First(&invoice).Error; err != nil {
				return newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					"invoice_id does not belong to the contact",
				)
			}
			if invoice.Currency != definition.Currency {
				return newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					"Invoice currency does not match the package currency",
				)
			}
			if invoice.Status != models.CommerceInvoiceStatusOpen &&
				invoice.Status != models.CommerceInvoiceStatusPaid {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Only an open or paid invoice can fund a package purchase",
				)
			}
			var purchasedQuantity int64
			if err := tx.Model(&models.InvoiceLine{}).
				Where(
					"organization_id = ? AND invoice_id = ? AND package_definition_id = ?",
					orgID, invoice.ID, definition.ID,
				).
				Select("COALESCE(SUM(quantity), 0)").
				Scan(&purchasedQuantity).Error; err != nil {
				return err
			}
			var allocated int64
			if err := tx.Model(&models.ContactPackage{}).
				Where(
					"organization_id = ? AND invoice_id = ? AND package_definition_id = ?",
					orgID, invoice.ID, definition.ID,
				).
				Count(&allocated).Error; err != nil {
				return err
			}
			if purchasedQuantity <= allocated {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Invoice has no unallocated line for this package definition",
				)
			}
			if invoice.Status != models.CommerceInvoiceStatusPaid {
				status = models.ContactPackageStatusPending
			}
		}
		now := time.Now().UTC()
		startsAt := now
		if req.StartsAt != nil {
			startsAt = req.StartsAt.UTC()
		}
		var expiresAt *time.Time
		if definition.ValidityDays > 0 {
			expires := startsAt.AddDate(0, 0, definition.ValidityDays)
			expiresAt = &expires
		}
		contactPackage = models.ContactPackage{
			BaseModel:           models.BaseModel{ID: uuid.New()},
			OrganizationID:      orgID,
			ContactID:           req.ContactID,
			PackageDefinitionID: definition.ID,
			InvoiceID:           req.InvoiceID,
			Status:              status,
			StartsAt:            &startsAt,
			ExpiresAt:           expiresAt,
			PurchaseAmountMinor: definition.PriceMinor,
			Currency:            definition.Currency,
			Source:              strings.TrimSpace(req.Source),
			IdempotencyKey:      strings.TrimSpace(req.IdempotencyKey),
			Metadata:            bookingCommerceJSON(req.Metadata),
			Version:             1,
			CreatedByID:         &userID,
			UpdatedByID:         &userID,
		}
		result := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "organization_id"},
				{Name: "idempotency_key"},
			},
			DoNothing: true,
		}).Create(&contactPackage)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var existing models.ContactPackage
			if err := tx.Where(
				"organization_id = ? AND idempotency_key = ?",
				orgID, strings.TrimSpace(req.IdempotencyKey),
			).First(&existing).Error; err != nil {
				return err
			}
			if existing.ContactID != req.ContactID ||
				existing.PackageDefinitionID != req.PackageDefinitionID ||
				!bookingCommerceUUIDPointersEqual(existing.InvoiceID, req.InvoiceID) {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Idempotency key was already used for a different package purchase",
				)
			}
			contactPackage = existing
			return nil
		}
		if status == models.ContactPackageStatusActive {
			if err := grantContactPackageCredits(
				tx, orgID, &contactPackage, userID, now,
				"contact-package-grant:"+contactPackage.ID.String(),
			); err != nil {
				return err
			}
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			contactPackageAuditResource, contactPackage.ID,
			models.AuditActionCreated, nil, &contactPackage,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "create contact package", err)
	}
	if err := a.DB.Preload("PackageDefinition", "organization_id = ?", orgID).
		Preload("Balances", "organization_id = ?", orgID).
		Where("id = ? AND organization_id = ?", contactPackage.ID, orgID).
		First(&contactPackage).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load contact package", nil, "")
	}
	return r.SendEnvelope(contactPackage)
}

// SellContactPackage creates the package invoice and customer ownership record
// in one idempotent transaction. A failed request can never leave an orphan
// invoice behind.
func (a *App) SellContactPackage(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourcePackages, models.ActionWrite)
	if err != nil {
		return nil
	}
	if err := a.requirePermission(r, userID, models.ResourcePayments, models.ActionWrite); err != nil {
		return nil
	}
	if err := a.requirePermission(r, userID, models.ResourceContacts, models.ActionRead); err != nil {
		return nil
	}

	var req SellContactPackageRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateSellContactPackageRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	fingerprint, err := sellContactPackageRequestFingerprint(&req)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid package sale request", nil, "")
	}

	var response SellContactPackageResponse
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var transactionErr error
		response, transactionErr = sellContactPackageTransaction(
			tx,
			orgID,
			userID,
			&req,
			fingerprint,
		)
		return transactionErr
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "sell contact package", err)
	}

	if err := a.DB.
		Preload("Lines", "organization_id = ?", orgID).
		Where("id = ? AND organization_id = ?", response.Invoice.ID, orgID).
		First(&response.Invoice).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load package invoice", nil, "")
	}
	if err := a.DB.
		Preload("Contact", "organization_id = ?", orgID).
		Preload("PackageDefinition", "organization_id = ?", orgID).
		Preload("Balances", "organization_id = ?", orgID).
		Where("id = ? AND organization_id = ?", response.ContactPackage.ID, orgID).
		First(&response.ContactPackage).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load contact package", nil, "")
	}
	return r.SendEnvelope(response)
}

func sellContactPackageTransaction(
	tx *gorm.DB,
	orgID uuid.UUID,
	userID uuid.UUID,
	req *SellContactPackageRequest,
	saleFingerprint string,
) (SellContactPackageResponse, error) {
	var response SellContactPackageResponse
	var definition models.PackageDefinition
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", req.PackageDefinitionID, orgID).
		First(&definition).Error; err != nil {
		return response, newBookingCommerceClientError(
			fasthttp.StatusBadRequest,
			"package_definition_id does not belong to this organization",
		)
	}

	var existingPackage models.ContactPackage
	findPackageErr := tx.Where(
		"organization_id = ? AND idempotency_key = ?",
		orgID,
		req.IdempotencyKey,
	).First(&existingPackage).Error
	if findPackageErr == nil {
		storedFingerprint, _ := existingPackage.Metadata["package_sale_fingerprint"].(string)
		if existingPackage.ContactID != req.ContactID ||
			existingPackage.PackageDefinitionID != req.PackageDefinitionID ||
			existingPackage.InvoiceID == nil ||
			storedFingerprint != saleFingerprint {
			return response, newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Idempotency key was already used for a different package operation",
			)
		}
		var invoice models.CommerceInvoice
		if err := tx.Where(
			"id = ? AND organization_id = ? AND contact_id = ?",
			*existingPackage.InvoiceID,
			orgID,
			req.ContactID,
		).First(&invoice).Error; err != nil {
			return response, err
		}
		response.Invoice = invoice
		response.ContactPackage = existingPackage
		return response, nil
	}
	if !errors.Is(findPackageErr, gorm.ErrRecordNotFound) {
		return response, findPackageErr
	}
	if !definition.IsActive {
		return response, newBookingCommerceClientError(
			fasthttp.StatusBadRequest,
			"package_definition_id does not belong to an active package",
		)
	}
	if err := ensureProductCRMTenantRecord(
		tx,
		&models.Contact{},
		orgID,
		req.ContactID,
		"contact_id",
	); err != nil {
		return response, err
	}

	invoiceRequest := CreateCommerceInvoiceRequest{
		ContactID: req.ContactID,
		Currency:  definition.Currency,
		DueAt:     normalizeOptionalTime(req.DueAt),
		Lines: []CommerceInvoiceLineInput{{
			PackageDefinitionID: &definition.ID,
			Quantity:            1,
		}},
		IdempotencyKey: req.IdempotencyKey,
		Metadata:       bookingCommerceJSON(req.Metadata),
	}
	invoiceFingerprint, err := commerceInvoiceRequestFingerprint(&invoiceRequest)
	if err != nil {
		return response, err
	}

	var invoice models.CommerceInvoice
	findInvoiceErr := tx.Where(
		"organization_id = ? AND idempotency_key = ?",
		orgID,
		req.IdempotencyKey,
	).First(&invoice).Error
	if findInvoiceErr == nil {
		storedSaleFingerprint, _ := invoice.Metadata["package_sale_fingerprint"].(string)
		if storedSaleFingerprint != saleFingerprint ||
			invoice.ContactID != req.ContactID ||
			invoice.Currency != definition.Currency {
			return response, newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Idempotency key was already used for a different invoice",
			)
		}
	} else {
		if !errors.Is(findInvoiceErr, gorm.ErrRecordNotFound) {
			return response, findInvoiceErr
		}
		if err := validateCreateCommerceInvoiceRequest(&invoiceRequest); err != nil {
			return response, newBookingCommerceClientError(fasthttp.StatusBadRequest, err.Error())
		}
		now := time.Now().UTC()
		invoiceMetadata := bookingCommerceJSON(req.Metadata)
		invoiceMetadata["workflow"] = "package_sale"
		invoiceMetadata["package_sale_fingerprint"] = saleFingerprint
		invoiceMetadata["idempotency_fingerprint"] = invoiceFingerprint
		invoice = models.CommerceInvoice{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgID,
			ContactID:      req.ContactID,
			InvoiceNumber:  newCommerceInvoiceNumber(now),
			IdempotencyKey: req.IdempotencyKey,
			Status:         models.CommerceInvoiceStatusOpen,
			Currency:       definition.Currency,
			IssuedAt:       &now,
			DueAt:          normalizeOptionalTime(req.DueAt),
			Metadata:       invoiceMetadata,
			Version:        1,
			CreatedByID:    &userID,
			UpdatedByID:    &userID,
		}
		lines, subtotal, tax, total, buildErr := buildCommerceInvoiceLines(
			tx,
			orgID,
			&invoice,
			invoiceRequest.Lines,
		)
		if buildErr != nil {
			return response, buildErr
		}
		invoice.SubtotalMinor = subtotal
		invoice.TaxMinor = tax
		invoice.TotalMinor = total
		invoice.DueMinor = total
		if total == 0 {
			invoice.Status = models.CommerceInvoiceStatusPaid
			invoice.PaidAt = &now
		}
		result := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "organization_id"},
				{Name: "idempotency_key"},
			},
			DoNothing: true,
		}).Create(&invoice)
		if result.Error != nil {
			return response, result.Error
		}
		if result.RowsAffected == 0 {
			if err := tx.Where(
				"organization_id = ? AND idempotency_key = ?",
				orgID,
				req.IdempotencyKey,
			).First(&invoice).Error; err != nil {
				return response, err
			}
			storedSaleFingerprint, _ := invoice.Metadata["package_sale_fingerprint"].(string)
			if storedSaleFingerprint != saleFingerprint {
				return response, newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Idempotency key was already used for a different invoice",
				)
			}
		} else {
			if err := tx.Create(&lines).Error; err != nil {
				return response, err
			}
			invoice.Lines = lines
			if err := audit.LogAudit(
				tx,
				orgID,
				userID,
				audit.GetUserName(tx, userID),
				commerceInvoiceAuditResource,
				invoice.ID,
				models.AuditActionCreated,
				nil,
				&invoice,
			); err != nil {
				return response, err
			}
			if _, err := recordCustomerActivity(tx, orgID, customerActivityInput{
				ContactID:        invoice.ContactID,
				EventType:        models.CustomerActivityInvoiceCreated,
				Category:         models.CustomerActivityCategoryInvoice,
				Title:            "Invoice created",
				Summary:          invoice.InvoiceNumber,
				ActorType:        models.CustomerActivityActorUser,
				ActorUserID:      &userID,
				SourceObjectType: commerceInvoiceAuditResource,
				SourceObjectID:   &invoice.ID,
				OccurredAt:       now,
				Metadata: models.JSONB{
					"status":      string(invoice.Status),
					"currency":    invoice.Currency,
					"total_minor": invoice.TotalMinor,
					"due_minor":   invoice.DueMinor,
					"workflow":    "package_sale",
				},
				IdempotencyKey: "invoice-created:" + invoice.ID.String(),
			}); err != nil {
				return response, err
			}
		}
	}

	now := time.Now().UTC()
	startsAt := now
	var expiresAt *time.Time
	if definition.ValidityDays > 0 {
		expires := startsAt.AddDate(0, 0, definition.ValidityDays)
		expiresAt = &expires
	}
	status := models.ContactPackageStatusPending
	if invoice.Status == models.CommerceInvoiceStatusPaid {
		status = models.ContactPackageStatusActive
	}
	packageMetadata := bookingCommerceJSON(req.Metadata)
	packageMetadata["workflow"] = "package_sale"
	packageMetadata["package_sale_fingerprint"] = saleFingerprint
	contactPackage := models.ContactPackage{
		BaseModel:           models.BaseModel{ID: uuid.New()},
		OrganizationID:      orgID,
		ContactID:           req.ContactID,
		PackageDefinitionID: definition.ID,
		InvoiceID:           &invoice.ID,
		Status:              status,
		StartsAt:            &startsAt,
		ExpiresAt:           expiresAt,
		PurchaseAmountMinor: definition.PriceMinor,
		Currency:            definition.Currency,
		Source:              "staff_sale",
		IdempotencyKey:      req.IdempotencyKey,
		Metadata:            packageMetadata,
		Version:             1,
		CreatedByID:         &userID,
		UpdatedByID:         &userID,
	}
	result := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "idempotency_key"},
		},
		DoNothing: true,
	}).Create(&contactPackage)
	if result.Error != nil {
		return response, result.Error
	}
	if result.RowsAffected == 0 {
		var existing models.ContactPackage
		if err := tx.Where(
			"organization_id = ? AND idempotency_key = ?",
			orgID,
			req.IdempotencyKey,
		).First(&existing).Error; err != nil {
			return response, err
		}
		storedFingerprint, _ := existing.Metadata["package_sale_fingerprint"].(string)
		if existing.ContactID != req.ContactID ||
			existing.PackageDefinitionID != req.PackageDefinitionID ||
			existing.InvoiceID == nil ||
			*existing.InvoiceID != invoice.ID ||
			storedFingerprint != saleFingerprint {
			return response, newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Idempotency key was already used for a different package operation",
			)
		}
		contactPackage = existing
	} else {
		if status == models.ContactPackageStatusActive {
			if err := grantContactPackageCredits(
				tx,
				orgID,
				&contactPackage,
				userID,
				now,
				"contact-package-sale:"+contactPackage.ID.String(),
			); err != nil {
				return response, err
			}
		}
		if err := audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			contactPackageAuditResource,
			contactPackage.ID,
			models.AuditActionCreated,
			nil,
			&contactPackage,
		); err != nil {
			return response, err
		}
		if _, err := recordCustomerActivity(tx, orgID, customerActivityInput{
			ContactID:        contactPackage.ContactID,
			EventType:        models.CustomerActivityPackageSold,
			Category:         models.CustomerActivityCategoryPackage,
			Title:            "Package sold",
			Summary:          definition.Name,
			ActorType:        models.CustomerActivityActorUser,
			ActorUserID:      &userID,
			SourceObjectType: contactPackageAuditResource,
			SourceObjectID:   &contactPackage.ID,
			OccurredAt:       now,
			Metadata: models.JSONB{
				"package_definition_id": definition.ID.String(),
				"invoice_id":            invoice.ID.String(),
				"status":                string(contactPackage.Status),
				"currency":              contactPackage.Currency,
				"amount_minor":          contactPackage.PurchaseAmountMinor,
				"expires_at":            contactPackage.ExpiresAt,
			},
			IdempotencyKey: "package-sold:" + contactPackage.ID.String(),
		}); err != nil {
			return response, err
		}
	}
	response.Invoice = invoice
	response.ContactPackage = contactPackage
	return response, nil
}

// ListCommerceInvoices lists tenant invoices with their server-computed lines.
func (a *App) ListCommerceInvoices(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourcePayments, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.CommerceInvoice{}).
		Where("commerce_invoices.organization_id = ?", orgID)
	if contactID, parseErr := productCRMQueryUUID(r, "contact_id"); parseErr != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact ID", nil, "")
	} else if contactID != nil {
		query = query.Where("commerce_invoices.contact_id = ?", *contactID)
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("status"))); raw != "" {
		status := models.CommerceInvoiceStatus(raw)
		if !validCommerceInvoiceStatus(status) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid invoice status", nil, "")
		}
		query = query.Where("commerce_invoices.status = ?", status)
	}
	if search := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("search"))); search != "" {
		query = query.Where("commerce_invoices.invoice_number ILIKE ?", productCRMSearchPattern(search))
	}
	for _, filter := range []struct {
		param    string
		column   string
		operator string
	}{
		{"from", "commerce_invoices.created_at", ">="},
		{"to", "commerce_invoices.created_at", "<="},
	} {
		value, parseErr := bookingCommerceQueryTime(r, filter.param)
		if parseErr != nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				filter.param+" must be RFC3339",
				nil,
				"",
			)
		}
		if value != nil {
			query = query.Where(filter.column+" "+filter.operator+" ?", *value)
		}
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list invoices", nil, "")
	}
	var invoices []models.CommerceInvoice
	if err := pg.Apply(query.
		Preload("Contact", "organization_id = ?", orgID).
		Preload("Lines", "organization_id = ?", orgID).
		Order("commerce_invoices.created_at DESC")).
		Find(&invoices).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list invoices", nil, "")
	}
	return r.SendEnvelope(listEnvelope("invoices", invoices, total, pg))
}

// CreateCommerceInvoice validates domain-linked rows and computes every total
// on the server before persisting the invoice and its lines atomically.
func (a *App) CreateCommerceInvoice(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourcePayments, models.ActionWrite)
	if err != nil {
		return nil
	}
	var req CreateCommerceInvoiceRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateCreateCommerceInvoiceRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	fingerprint, err := commerceInvoiceRequestFingerprint(&req)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid invoice request", nil, "")
	}

	now := time.Now().UTC()
	metadata := bookingCommerceJSON(req.Metadata)
	metadata["idempotency_fingerprint"] = fingerprint
	invoice := models.CommerceInvoice{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		ContactID:      req.ContactID,
		InvoiceNumber:  newCommerceInvoiceNumber(now),
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		Status:         models.CommerceInvoiceStatusOpen,
		Currency:       strings.ToUpper(strings.TrimSpace(req.Currency)),
		DiscountMinor:  req.DiscountMinor,
		IssuedAt:       &now,
		DueAt:          normalizeOptionalTime(req.DueAt),
		Metadata:       metadata,
		Version:        1,
		CreatedByID:    &userID,
		UpdatedByID:    &userID,
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var existing models.CommerceInvoice
		findErr := tx.Preload("Lines", "organization_id = ?", orgID).
			Where(
				"organization_id = ? AND idempotency_key = ?",
				orgID, strings.TrimSpace(req.IdempotencyKey),
			).
			First(&existing).Error
		if findErr == nil {
			if existingFingerprint, _ := existing.Metadata["idempotency_fingerprint"].(string); existingFingerprint != fingerprint {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Idempotency key was already used for a different invoice",
				)
			}
			invoice = existing
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		if err := ensureProductCRMTenantRecord(
			tx, &models.Contact{}, orgID, req.ContactID, "contact_id",
		); err != nil {
			return err
		}
		lines, subtotal, tax, total, err := buildCommerceInvoiceLines(
			tx, orgID, &invoice, req.Lines,
		)
		if err != nil {
			return err
		}
		if req.DiscountMinor > subtotal {
			return newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"discount_minor cannot exceed the invoice subtotal",
			)
		}
		total -= req.DiscountMinor
		invoice.SubtotalMinor = subtotal
		invoice.TaxMinor = tax
		invoice.TotalMinor = total
		invoice.DueMinor = total
		if total == 0 {
			invoice.Status = models.CommerceInvoiceStatusPaid
			invoice.PaidAt = &now
		}
		result := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "organization_id"},
				{Name: "idempotency_key"},
			},
			DoNothing: true,
		}).Create(&invoice)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			if err := tx.Preload("Lines", "organization_id = ?", orgID).
				Where(
					"organization_id = ? AND idempotency_key = ?",
					orgID, strings.TrimSpace(req.IdempotencyKey),
				).
				First(&existing).Error; err != nil {
				return err
			}
			if existingFingerprint, _ := existing.Metadata["idempotency_fingerprint"].(string); existingFingerprint != fingerprint {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Idempotency key was already used for a different invoice",
				)
			}
			invoice = existing
			return nil
		}
		if err := tx.Create(&lines).Error; err != nil {
			return err
		}
		invoice.Lines = lines
		if _, err := recordCustomerActivity(tx, orgID, customerActivityInput{
			ContactID:        invoice.ContactID,
			EventType:        models.CustomerActivityInvoiceCreated,
			Category:         models.CustomerActivityCategoryInvoice,
			Title:            "Invoice created",
			Summary:          invoice.InvoiceNumber,
			ActorType:        models.CustomerActivityActorUser,
			ActorUserID:      &userID,
			SourceObjectType: commerceInvoiceAuditResource,
			SourceObjectID:   &invoice.ID,
			OccurredAt:       now,
			Metadata: models.JSONB{
				"status":      string(invoice.Status),
				"currency":    invoice.Currency,
				"total_minor": invoice.TotalMinor,
				"due_minor":   invoice.DueMinor,
			},
			IdempotencyKey: "invoice-created:" + invoice.ID.String(),
		}); err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			commerceInvoiceAuditResource, invoice.ID,
			models.AuditActionCreated, nil, &invoice,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "create invoice", err)
	}
	return r.SendEnvelope(invoice)
}

// CreateInvoicePaymentIntent records a provider-neutral pending intent. No
// provider call is made and the response explicitly says an adapter is still
// required to attempt collection.
func (a *App) CreateInvoicePaymentIntent(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourcePayments, models.ActionWrite)
	if err != nil {
		return nil
	}
	invoiceID, err := parsePathUUID(r, "id", "invoice")
	if err != nil {
		return nil
	}
	var req CreateInvoicePaymentIntentRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateCreateInvoicePaymentIntentRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var intent models.PaymentIntent
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Where(
			"organization_id = ? AND idempotency_key = ?",
			orgID, strings.TrimSpace(req.IdempotencyKey),
		).First(&intent).Error
		if err == nil {
			if intent.InvoiceID != invoiceID ||
				intent.ProviderAccountID != req.ProviderAccountID ||
				(req.AmountMinor != nil && intent.AmountMinor != *req.AmountMinor) {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Idempotency key was already used for a different payment intent",
				)
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var invoice models.CommerceInvoice
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", invoiceID, orgID).
			First(&invoice).Error; err != nil {
			return newBookingCommerceClientError(fasthttp.StatusNotFound, "Invoice not found")
		}
		if invoice.Status != models.CommerceInvoiceStatusOpen || invoice.DueMinor <= 0 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Only an open invoice with an outstanding balance can receive a payment intent",
			)
		}
		var account models.PaymentProviderAccount
		if err := tx.Where(
			"id = ? AND organization_id = ? AND is_active = ?",
			req.ProviderAccountID, orgID, true,
		).First(&account).Error; err != nil {
			return newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"provider_account_id does not belong to an active payment account",
			)
		}
		if strings.EqualFold(strings.TrimSpace(account.Provider), "manual") {
			return newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"Manual payment accounts do not create provider payment intents",
			)
		}
		amount := invoice.DueMinor
		if req.AmountMinor != nil {
			amount = *req.AmountMinor
		}
		if amount > invoice.DueMinor {
			return newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"amount_minor cannot exceed the outstanding invoice balance",
			)
		}
		metadata := bookingCommerceJSON(req.Metadata)
		metadata["adapter_dispatched"] = false
		metadata["collection_attempted"] = false
		intent = models.PaymentIntent{
			BaseModel:         models.BaseModel{ID: uuid.New()},
			OrganizationID:    orgID,
			ProviderAccountID: account.ID,
			InvoiceID:         invoice.ID,
			ContactID:         invoice.ContactID,
			IdempotencyKey:    strings.TrimSpace(req.IdempotencyKey),
			AmountMinor:       amount,
			Currency:          invoice.Currency,
			Status:            models.PaymentIntentStatusPending,
			ExpiresAt:         normalizeOptionalTime(req.ExpiresAt),
			Metadata:          metadata,
			Version:           1,
			CreatedByID:       &userID,
		}
		result := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "organization_id"},
				{Name: "idempotency_key"},
			},
			DoNothing: true,
		}).Create(&intent)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var existing models.PaymentIntent
			if err := tx.Where(
				"organization_id = ? AND idempotency_key = ?",
				orgID, strings.TrimSpace(req.IdempotencyKey),
			).First(&existing).Error; err != nil {
				return err
			}
			if existing.InvoiceID != invoiceID ||
				existing.ProviderAccountID != req.ProviderAccountID ||
				(req.AmountMinor != nil && existing.AmountMinor != *req.AmountMinor) {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Idempotency key was already used for a different payment intent",
				)
			}
			intent = existing
			return nil
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			paymentIntentAuditResource, intent.ID,
			models.AuditActionCreated, nil, &intent,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "create payment intent", err)
	}
	return r.SendEnvelope(PaymentIntentResponse{
		Intent:           intent,
		CollectionStatus: "requires_adapter",
		Message: "Payment intent recorded. No external collection was attempted; " +
			"a configured provider adapter must process it.",
	})
}

// ListPaymentTransactions lists the append-only tenant payment ledger.
func (a *App) ListPaymentTransactions(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourcePayments, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.PaymentTransaction{}).
		Where("payment_transactions.organization_id = ?", orgID)
	for _, filter := range []struct {
		param  string
		column string
		label  string
	}{
		{"invoice_id", "invoice_id", "invoice"},
		{"provider_account_id", "provider_account_id", "provider account"},
		{"intent_id", "intent_id", "payment intent"},
	} {
		value, parseErr := productCRMQueryUUID(r, filter.param)
		if parseErr != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid "+filter.label+" ID", nil, "")
		}
		if value != nil {
			query = query.Where(filter.column+" = ?", *value)
		}
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("status"))); raw != "" {
		status := models.PaymentTransactionStatus(raw)
		if !validPaymentTransactionStatus(status) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid payment status", nil, "")
		}
		query = query.Where("status = ?", status)
	}
	if raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("type"))); raw != "" {
		transactionType := models.PaymentTransactionType(raw)
		if !validPaymentTransactionType(transactionType) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid payment type", nil, "")
		}
		query = query.Where("type = ?", transactionType)
	}
	for _, filter := range []struct {
		param    string
		column   string
		operator string
	}{
		{"from", "occurred_at", ">="},
		{"to", "occurred_at", "<="},
	} {
		value, parseErr := bookingCommerceQueryTime(r, filter.param)
		if parseErr != nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				filter.param+" must be RFC3339",
				nil,
				"",
			)
		}
		if value != nil {
			query = query.Where(filter.column+" "+filter.operator+" ?", *value)
		}
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list payments", nil, "")
	}
	var payments []models.PaymentTransaction
	if err := pg.Apply(query.
		Preload("ProviderAccount", "organization_id = ?", orgID).
		Preload("Invoice", "organization_id = ?", orgID).
		Order("occurred_at DESC, created_at DESC")).
		Find(&payments).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list payments", nil, "")
	}
	return r.SendEnvelope(listEnvelope("payments", payments, total, pg))
}

// GetCommerceSummary returns tenant-wide currency aggregates without requiring
// the client to download the financial ledger.
func (a *App) GetCommerceSummary(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourcePayments, models.ActionRead)
	if err != nil {
		return nil
	}
	response := CommerceSummaryResponse{
		Outstanding:      []CurrencyAmountSummary{},
		CollectedCharges: []CurrencyAmountSummary{},
		PackagesVisible:  a.HasPermission(userID, models.ResourcePackages, models.ActionRead, orgID),
	}
	if response.PackagesVisible {
		if err := a.DB.Model(&models.PackageDefinition{}).
			Where("organization_id = ? AND is_active = ?", orgID, true).
			Count(&response.ActivePackages).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to summarize packages", nil, "")
		}
	}
	if err := a.DB.Model(&models.CommerceInvoice{}).
		Select("currency, COALESCE(SUM(due_minor), 0) AS amount_minor").
		Where("organization_id = ? AND due_minor > 0", orgID).
		Group("currency").
		Order("currency ASC").
		Scan(&response.Outstanding).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to summarize invoices", nil, "")
	}
	if err := a.DB.Model(&models.PaymentTransaction{}).
		Select("currency, COALESCE(SUM(amount_minor), 0) AS amount_minor").
		Where(
			"organization_id = ? AND status = ? AND type = ?",
			orgID,
			models.PaymentTransactionStatusSucceeded,
			models.PaymentTransactionTypeCharge,
		).
		Group("currency").
		Order("currency ASC").
		Scan(&response.CollectedCharges).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to summarize payments", nil, "")
	}
	return r.SendEnvelope(response)
}

// ListPayments is kept as a compatibility alias for route registries that use
// the shorter handler name.
func (a *App) ListPayments(r *fastglue.Request) error {
	return a.ListPaymentTransactions(r)
}

// RecordManualInvoicePayment explicitly records a human-confirmed, externally
// received payment. It never invokes or impersonates a provider adapter.
func (a *App) RecordManualInvoicePayment(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourcePayments, models.ActionWrite)
	if err != nil {
		return nil
	}
	invoiceID, err := parsePathUUID(r, "id", "invoice")
	if err != nil {
		return nil
	}
	var req RecordManualInvoicePaymentRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateRecordManualInvoicePaymentRequest(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var invoice models.CommerceInvoice
	var payment models.PaymentTransaction
	var idempotent bool
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		key := strings.TrimSpace(req.IdempotencyKey)
		err := tx.Where("organization_id = ? AND idempotency_key = ?", orgID, key).
			First(&payment).Error
		if err == nil {
			if payment.InvoiceID == nil || *payment.InvoiceID != invoiceID ||
				payment.AmountMinor != req.AmountMinor ||
				payment.Currency != strings.ToUpper(strings.TrimSpace(req.Currency)) ||
				!strings.EqualFold(payment.ProviderTransactionID, strings.TrimSpace(req.Reference)) {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Idempotency key was already used for a different payment",
				)
			}
			if err := tx.Where("id = ? AND organization_id = ?", invoiceID, orgID).
				First(&invoice).Error; err != nil {
				return newBookingCommerceClientError(fasthttp.StatusNotFound, "Invoice not found")
			}
			idempotent = true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", invoiceID, orgID).
			First(&invoice).Error; err != nil {
			return newBookingCommerceClientError(fasthttp.StatusNotFound, "Invoice not found")
		}
		err = tx.Where(
			"organization_id = ? AND idempotency_key = ?",
			orgID, strings.TrimSpace(req.IdempotencyKey),
		).First(&payment).Error
		if err == nil {
			if payment.InvoiceID == nil || *payment.InvoiceID != invoiceID ||
				payment.AmountMinor != req.AmountMinor ||
				payment.Currency != strings.ToUpper(strings.TrimSpace(req.Currency)) ||
				!strings.EqualFold(
					payment.ProviderTransactionID,
					strings.TrimSpace(req.Reference),
				) {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Idempotency key was already used for a different payment",
				)
			}
			idempotent = true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if invoice.Version != req.Version {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Invoice was modified; refresh and retry",
			)
		}
		if invoice.Status != models.CommerceInvoiceStatusOpen || invoice.DueMinor <= 0 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Only an open invoice with an outstanding balance can receive a manual payment",
			)
		}
		currency := strings.ToUpper(strings.TrimSpace(req.Currency))
		if currency != invoice.Currency {
			return newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"Payment currency must match the invoice currency",
			)
		}
		if req.AmountMinor > invoice.DueMinor {
			return newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"amount_minor cannot exceed the outstanding invoice balance",
			)
		}
		account, err := resolveManualPaymentAccount(tx, orgID, userID, req.ProviderAccountID)
		if err != nil {
			return err
		}
		reference := strings.TrimSpace(req.Reference)
		var referenceCount int64
		if err := tx.Model(&models.PaymentTransaction{}).
			Where(
				"organization_id = ? AND provider_account_id = ? AND LOWER(provider_transaction_id) = LOWER(?)",
				orgID, account.ID, reference,
			).
			Count(&referenceCount).Error; err != nil {
			return err
		}
		if referenceCount > 0 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Manual payment reference has already been recorded",
			)
		}

		occurredAt := time.Now().UTC()
		if req.OccurredAt != nil {
			occurredAt = req.OccurredAt.UTC()
		}
		metadata := bookingCommerceJSON(req.Metadata)
		metadata["manual"] = true
		metadata["provider_collection_attempted"] = false
		if notes := strings.TrimSpace(req.Notes); notes != "" {
			metadata["notes"] = notes
		}
		payment = models.PaymentTransaction{
			ID:                    uuid.New(),
			OrganizationID:        orgID,
			ProviderAccountID:     account.ID,
			InvoiceID:             &invoice.ID,
			Type:                  models.PaymentTransactionTypeCharge,
			ProviderTransactionID: reference,
			IdempotencyKey:        key,
			AmountMinor:           req.AmountMinor,
			Currency:              invoice.Currency,
			Status:                models.PaymentTransactionStatusSucceeded,
			Metadata:              metadata,
			OccurredAt:            occurredAt,
		}
		if err := tx.Create(&payment).Error; err != nil {
			if bookingCommerceUniqueConstraintError(err) {
				return newBookingCommerceClientError(
					fasthttp.StatusConflict,
					"Manual payment reference or idempotency key has already been recorded",
				)
			}
			return err
		}

		oldInvoice := invoice
		invoice.PaidMinor += req.AmountMinor
		invoice.DueMinor -= req.AmountMinor
		invoice.Version++
		invoice.UpdatedByID = &userID
		if invoice.DueMinor == 0 {
			now := time.Now().UTC()
			invoice.Status = models.CommerceInvoiceStatusPaid
			invoice.PaidAt = &now
		}
		result := tx.Model(&models.CommerceInvoice{}).
			Where("id = ? AND organization_id = ? AND version = ?", invoice.ID, orgID, req.Version).
			Updates(map[string]any{
				"paid_minor":    invoice.PaidMinor,
				"due_minor":     invoice.DueMinor,
				"status":        invoice.Status,
				"paid_at":       invoice.PaidAt,
				"updated_by_id": userID,
				"version":       invoice.Version,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Invoice was modified; refresh and retry",
			)
		}
		if invoice.Status == models.CommerceInvoiceStatusPaid {
			if err := activatePaidInvoiceContactPackages(
				tx, orgID, &invoice, userID, payment.ID,
			); err != nil {
				return err
			}
		}
		if _, err := recordCustomerActivity(tx, orgID, customerActivityInput{
			ContactID:        invoice.ContactID,
			EventType:        models.CustomerActivityPaymentRecorded,
			Category:         models.CustomerActivityCategoryPayment,
			Title:            "Payment recorded",
			Summary:          invoice.InvoiceNumber,
			ActorType:        models.CustomerActivityActorUser,
			ActorUserID:      &userID,
			SourceObjectType: paymentAuditResource,
			SourceObjectID:   &payment.ID,
			OccurredAt:       payment.OccurredAt,
			Metadata: models.JSONB{
				"invoice_id":   invoice.ID.String(),
				"amount_minor": payment.AmountMinor,
				"currency":     payment.Currency,
				"status":       string(payment.Status),
				"manual":       true,
			},
			IdempotencyKey: "payment-recorded:" + payment.ID.String(),
		}); err != nil {
			return err
		}
		if invoice.Status == models.CommerceInvoiceStatusPaid {
			if _, err := recordCustomerActivity(tx, orgID, customerActivityInput{
				ContactID:        invoice.ContactID,
				EventType:        models.CustomerActivityInvoicePaid,
				Category:         models.CustomerActivityCategoryInvoice,
				Title:            "Invoice paid",
				Summary:          invoice.InvoiceNumber,
				ActorType:        models.CustomerActivityActorUser,
				ActorUserID:      &userID,
				SourceObjectType: commerceInvoiceAuditResource,
				SourceObjectID:   &invoice.ID,
				OccurredAt:       payment.OccurredAt,
				Metadata: models.JSONB{
					"currency":   invoice.Currency,
					"paid_minor": invoice.PaidMinor,
					"payment_id": payment.ID.String(),
					"version":    invoice.Version,
				},
				IdempotencyKey: fmt.Sprintf("invoice-paid:%s:%d", invoice.ID, invoice.Version),
			}); err != nil {
				return err
			}
		}
		if err := audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			paymentAuditResource, payment.ID,
			models.AuditActionCreated, nil, &payment,
		); err != nil {
			return err
		}
		return audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			commerceInvoiceAuditResource, invoice.ID,
			models.AuditActionUpdated, &oldInvoice, &invoice,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "record manual payment", err)
	}
	return r.SendEnvelope(map[string]any{
		"invoice":                       invoice,
		"payment":                       payment,
		"idempotent":                    idempotent,
		"collection_status":             "manual_payment_recorded",
		"provider_collection_attempted": false,
	})
}

func bookingCommerceUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate key") ||
		strings.Contains(message, "unique constraint")
}

func bookingCommerceJSON(value models.JSONB) models.JSONB {
	if value == nil {
		return models.JSONB{}
	}
	return value
}

func bookingCommerceUUIDPointersEqual(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func normalizeOptionalTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := value.UTC()
	return &normalized
}

func validCurrency(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 3 {
		return false
	}
	for _, character := range value {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

func validBookingServiceKind(value models.BookingServiceKind) bool {
	return value == models.BookingServiceKindAppointment ||
		value == models.BookingServiceKindClass
}

func validBookingResourceKind(value models.BookingResourceKind) bool {
	switch value {
	case models.BookingResourceKindPractitioner,
		models.BookingResourceKindInstructor,
		models.BookingResourceKindRoom,
		models.BookingResourceKindEquipment:
		return true
	default:
		return false
	}
}

func validBookingEventStatus(value models.BookingEventStatus) bool {
	switch value {
	case models.BookingEventStatusScheduled,
		models.BookingEventStatusCompleted,
		models.BookingEventStatusCancelled:
		return true
	default:
		return false
	}
}

func validBookingStatus(value models.BookingStatus) bool {
	switch value {
	case models.BookingStatusReserved,
		models.BookingStatusConfirmed,
		models.BookingStatusWaitlisted,
		models.BookingStatusCheckedIn,
		models.BookingStatusCompleted,
		models.BookingStatusNoShow,
		models.BookingStatusCancelled:
		return true
	default:
		return false
	}
}

func validBookingSource(value models.BookingSource) bool {
	switch value {
	case models.BookingSourceAgent,
		models.BookingSourceWhatsApp,
		models.BookingSourceAPI,
		models.BookingSourceImport:
		return true
	default:
		return false
	}
}

func validContactPackageStatus(value models.ContactPackageStatus) bool {
	switch value {
	case models.ContactPackageStatusPending,
		models.ContactPackageStatusActive,
		models.ContactPackageStatusExhausted,
		models.ContactPackageStatusExpired,
		models.ContactPackageStatusCancelled,
		models.ContactPackageStatusRefunded:
		return true
	default:
		return false
	}
}

func validCommerceInvoiceStatus(value models.CommerceInvoiceStatus) bool {
	switch value {
	case models.CommerceInvoiceStatusDraft,
		models.CommerceInvoiceStatusOpen,
		models.CommerceInvoiceStatusPaid,
		models.CommerceInvoiceStatusVoid,
		models.CommerceInvoiceStatusPartiallyRefunded,
		models.CommerceInvoiceStatusRefunded:
		return true
	default:
		return false
	}
}

func validPaymentTransactionStatus(value models.PaymentTransactionStatus) bool {
	switch value {
	case models.PaymentTransactionStatusPending,
		models.PaymentTransactionStatusSucceeded,
		models.PaymentTransactionStatusFailed,
		models.PaymentTransactionStatusReversed:
		return true
	default:
		return false
	}
}

func validPaymentTransactionType(value models.PaymentTransactionType) bool {
	switch value {
	case models.PaymentTransactionTypeCharge,
		models.PaymentTransactionTypeRefund,
		models.PaymentTransactionTypeFee,
		models.PaymentTransactionTypeAdjustment:
		return true
	default:
		return false
	}
}

func validateBookingServiceRequest(req *BookingServiceRequest, updating bool) error {
	req.Name = strings.TrimSpace(req.Name)
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 255 {
		return errors.New("name is required and must not exceed 255 characters")
	}
	if utf8.RuneCountInString(req.Description) > 10000 {
		return errors.New("description must not exceed 10000 characters")
	}
	if !validBookingServiceKind(req.Kind) {
		return errors.New("invalid booking service kind")
	}
	if req.DurationMinutes < 1 || req.DurationMinutes > 1440 {
		return errors.New("duration_minutes must be between 1 and 1440")
	}
	if req.BufferBeforeMinutes < 0 || req.BufferBeforeMinutes > 1440 ||
		req.BufferAfterMinutes < 0 || req.BufferAfterMinutes > 1440 {
		return errors.New("booking buffers must be between 0 and 1440 minutes")
	}
	if req.DefaultCapacity < 1 || req.DefaultCapacity > 10000 {
		return errors.New("default_capacity must be between 1 and 10000")
	}
	if req.PriceMinor < 0 {
		return errors.New("price_minor must be zero or greater")
	}
	if !validCurrency(req.Currency) {
		return errors.New("currency must be a three-letter uppercase ISO code")
	}
	if updating && req.Version < 1 {
		return errors.New("version must be at least 1")
	}
	seen := make(map[uuid.UUID]struct{}, len(req.ResourceIDs))
	for _, resourceID := range req.ResourceIDs {
		if resourceID == uuid.Nil {
			return errors.New("resource_ids must contain valid UUIDs")
		}
		if _, exists := seen[resourceID]; exists {
			return errors.New("resource_ids must not contain duplicates")
		}
		seen[resourceID] = struct{}{}
	}
	return nil
}

func validateBookingResourceRequest(req *BookingResourceRequest, updating bool) error {
	req.Name = strings.TrimSpace(req.Name)
	req.Timezone = strings.TrimSpace(req.Timezone)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 255 {
		return errors.New("name is required and must not exceed 255 characters")
	}
	if !validBookingResourceKind(req.Kind) {
		return errors.New("invalid booking resource kind")
	}
	if req.Timezone == "" || utf8.RuneCountInString(req.Timezone) > 100 {
		return errors.New("timezone is required and must not exceed 100 characters")
	}
	if _, err := time.LoadLocation(req.Timezone); err != nil {
		return errors.New("timezone must be a valid IANA timezone")
	}
	if utf8.RuneCountInString(req.Location) > 255 {
		return errors.New("location must not exceed 255 characters")
	}
	if req.ClearUserID && req.UserID != nil {
		return errors.New("user_id and clear_user_id cannot both be set")
	}
	if req.UserID != nil && *req.UserID == uuid.Nil {
		return errors.New("user_id must be a valid UUID")
	}
	if updating && req.Version < 1 {
		return errors.New("version must be at least 1")
	}
	return nil
}

func normalizeBookingEventRequestTimes(req *BookingEventRequest) error {
	req.LocalStartsAt = strings.TrimSpace(req.LocalStartsAt)
	req.LocalEndsAt = strings.TrimSpace(req.LocalEndsAt)
	req.Timezone = strings.TrimSpace(req.Timezone)
	hasLocalInput := req.LocalStartsAt != "" || req.LocalEndsAt != "" || req.Timezone != ""
	if !hasLocalInput {
		return nil
	}
	if req.LocalStartsAt == "" || req.LocalEndsAt == "" || req.Timezone == "" {
		return errors.New("local_starts_at, local_ends_at, and timezone must be provided together")
	}
	if !req.StartsAt.IsZero() || !req.EndsAt.IsZero() {
		return errors.New("provide either UTC timestamps or local wall times, not both")
	}
	if utf8.RuneCountInString(req.Timezone) > 100 {
		return errors.New("timezone must not exceed 100 characters")
	}
	location, err := time.LoadLocation(req.Timezone)
	if err != nil {
		return errors.New("timezone must be a valid IANA timezone")
	}
	parseLocal := func(raw, field string) (time.Time, error) {
		for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05"} {
			parsed, parseErr := time.ParseInLocation(layout, raw, location)
			if parseErr == nil {
				if parsed.In(location).Format(layout) != raw {
					return time.Time{}, fmt.Errorf("%s is not a valid wall time in %s", field, req.Timezone)
				}

				// Build candidates from the timezone's actual nearby UTC offsets. This
				// handles non-hour and historical rollbacks (for example Casey's
				// three-hour transition) without assuming a fixed DST shift size.
				wallTime, wallErr := time.Parse(layout, raw)
				if wallErr != nil {
					return time.Time{}, fmt.Errorf("%s must use YYYY-MM-DDTHH:MM[:SS]", field)
				}
				offsets := make(map[int]struct{})
				cursor := parsed.Add(-7 * 24 * time.Hour)
				windowEnd := parsed.Add(7 * 24 * time.Hour)
				for !cursor.After(windowEnd) {
					_, offset := cursor.In(location).Zone()
					offsets[offset] = struct{}{}
					_, zoneEnd := cursor.In(location).ZoneBounds()
					if zoneEnd.IsZero() || zoneEnd.After(windowEnd) {
						break
					}
					if !zoneEnd.After(cursor) {
						cursor = cursor.Add(time.Second)
						continue
					}
					cursor = zoneEnd
				}

				matches := make(map[int64]time.Time)
				for offset := range offsets {
					candidate := wallTime.Add(-time.Duration(offset) * time.Second)
					if candidate.In(location).Format(layout) == raw {
						matches[candidate.UnixNano()] = candidate
					}
				}
				if len(matches) > 1 {
					return time.Time{}, fmt.Errorf(
						"%s is ambiguous in %s; provide an RFC3339 timestamp with an explicit offset",
						field,
						req.Timezone,
					)
				}
				for _, match := range matches {
					return match.UTC(), nil
				}
				return time.Time{}, fmt.Errorf("%s is not a valid wall time in %s", field, req.Timezone)
			}
		}
		return time.Time{}, fmt.Errorf("%s must use YYYY-MM-DDTHH:MM[:SS]", field)
	}
	startsAt, err := parseLocal(req.LocalStartsAt, "local_starts_at")
	if err != nil {
		return err
	}
	endsAt, err := parseLocal(req.LocalEndsAt, "local_ends_at")
	if err != nil {
		return err
	}
	req.StartsAt = startsAt
	req.EndsAt = endsAt
	return nil
}

func validateBookingEventRequest(req *BookingEventRequest, updating bool) error {
	if req.ServiceID == uuid.Nil || req.ResourceID == uuid.Nil {
		return errors.New("service_id and resource_id are required")
	}
	if req.StartsAt.IsZero() || req.EndsAt.IsZero() || !req.EndsAt.After(req.StartsAt) {
		return errors.New("starts_at and ends_at must define a valid increasing interval")
	}
	if req.Capacity < 1 || req.Capacity > 10000 {
		return errors.New("capacity must be between 1 and 10000")
	}
	if req.Status != "" && !validBookingEventStatus(req.Status) {
		return errors.New("invalid booking event status")
	}
	if utf8.RuneCountInString(req.ExternalProvider) > 50 {
		return errors.New("external_provider must not exceed 50 characters")
	}
	if utf8.RuneCountInString(req.ExternalEventID) > 255 ||
		utf8.RuneCountInString(req.Location) > 255 {
		return errors.New("external_event_id and location must not exceed 255 characters")
	}
	if updating && req.Version < 1 {
		return errors.New("version must be at least 1")
	}
	return nil
}

func validateCreateBookingRequest(req *CreateBookingRequest) error {
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.EventID == uuid.Nil || req.ContactID == uuid.Nil {
		return errors.New("event_id and contact_id are required")
	}
	if req.Quantity < 1 || req.Quantity > 10000 {
		return errors.New("quantity must be between 1 and 10000")
	}
	if req.Status != "" &&
		req.Status != models.BookingStatusReserved &&
		req.Status != models.BookingStatusConfirmed &&
		req.Status != models.BookingStatusWaitlisted {
		return errors.New("initial status must be reserved, confirmed, or waitlisted")
	}
	if req.Source != "" && !validBookingSource(req.Source) {
		return errors.New("invalid booking source")
	}
	if req.ContactPackageID != nil && *req.ContactPackageID == uuid.Nil {
		return errors.New("contact_package_id must be a valid UUID")
	}
	if req.IdempotencyKey == "" || utf8.RuneCountInString(req.IdempotencyKey) > 255 {
		return errors.New("idempotency_key is required and must not exceed 255 characters")
	}
	if utf8.RuneCountInString(req.Notes) > 10000 {
		return errors.New("notes must not exceed 10000 characters")
	}
	return nil
}

func validateTransitionBookingRequest(req *TransitionBookingRequest) error {
	if !validBookingStatus(req.Status) {
		return errors.New("invalid booking transition")
	}
	if req.Version < 1 {
		return errors.New("version must be at least 1")
	}
	if utf8.RuneCountInString(req.Reason) > 10000 {
		return errors.New("reason must not exceed 10000 characters")
	}
	return nil
}

func bookingCommerceQueryTime(r *fastglue.Request, param string) (*time.Time, error) {
	raw := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek(param)))
	if raw == "" {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	value = value.UTC()
	return &value, nil
}

func bookingStatusFromTransition(raw string) (models.BookingStatus, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "reserve", "reserved":
		return models.BookingStatusReserved, nil
	case "confirm", "confirmed":
		return models.BookingStatusConfirmed, nil
	case "waitlist", "waitlisted":
		return models.BookingStatusWaitlisted, nil
	case "check_in", "checked_in", "checkin":
		return models.BookingStatusCheckedIn, nil
	case "complete", "completed":
		return models.BookingStatusCompleted, nil
	case "no_show", "noshow":
		return models.BookingStatusNoShow, nil
	case "cancel", "cancelled", "canceled":
		return models.BookingStatusCancelled, nil
	default:
		return "", fmt.Errorf("invalid booking transition %q", raw)
	}
}

func loadBookingServiceResourceIDs(
	db *gorm.DB,
	orgID uuid.UUID,
	services []models.BookingService,
) (map[uuid.UUID][]uuid.UUID, error) {
	result := make(map[uuid.UUID][]uuid.UUID, len(services))
	if len(services) == 0 {
		return result, nil
	}
	ids := make([]uuid.UUID, len(services))
	for i := range services {
		ids[i] = services[i].ID
	}
	var links []models.BookingServiceResource
	if err := db.Select("service_id", "resource_id").
		Where(
			"organization_id = ? AND service_id IN ? AND is_active = ?",
			orgID, ids, true,
		).
		Order("created_at ASC").
		Find(&links).Error; err != nil {
		return nil, err
	}
	for _, link := range links {
		result[link.ServiceID] = append(result[link.ServiceID], link.ResourceID)
	}
	return result, nil
}

func validateBookingResourceIDs(
	tx *gorm.DB,
	orgID uuid.UUID,
	resourceIDs []uuid.UUID,
) error {
	if len(resourceIDs) == 0 {
		return nil
	}
	var count int64
	if err := tx.Model(&models.BookingResource{}).
		Where("organization_id = ? AND id IN ?", orgID, resourceIDs).
		Count(&count).Error; err != nil {
		return err
	}
	if int(count) != len(resourceIDs) {
		return newBookingCommerceClientError(
			fasthttp.StatusBadRequest,
			"One or more resource_ids do not belong to the organization",
		)
	}
	return nil
}

func ensureBookingCommerceUniqueName(
	tx *gorm.DB,
	model any,
	orgID uuid.UUID,
	name string,
	excludeID uuid.UUID,
	label string,
) error {
	query := tx.Model(model).
		Where("organization_id = ? AND LOWER(name) = LOWER(?)", orgID, strings.TrimSpace(name))
	if excludeID != uuid.Nil {
		query = query.Where("id <> ?", excludeID)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"A "+label+" with this name already exists",
		)
	}
	return nil
}

func replaceBookingServiceResources(
	tx *gorm.DB,
	orgID, serviceID uuid.UUID,
	resourceIDs []uuid.UUID,
) error {
	if err := tx.Unscoped().
		Where("organization_id = ? AND service_id = ?", orgID, serviceID).
		Delete(&models.BookingServiceResource{}).Error; err != nil {
		return err
	}
	if len(resourceIDs) == 0 {
		return nil
	}
	links := make([]models.BookingServiceResource, len(resourceIDs))
	for i, resourceID := range resourceIDs {
		links[i] = models.BookingServiceResource{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgID,
			ServiceID:      serviceID,
			ResourceID:     resourceID,
			IsActive:       true,
			Version:        1,
		}
	}
	return tx.Create(&links).Error
}

func validateBookingEventReferences(
	tx *gorm.DB,
	orgID, serviceID, resourceID uuid.UUID,
) (*models.BookingService, *models.BookingResource, error) {
	var service models.BookingService
	if err := tx.Where(
		"id = ? AND organization_id = ? AND is_active = ?",
		serviceID, orgID, true,
	).First(&service).Error; err != nil {
		return nil, nil, newBookingCommerceClientError(
			fasthttp.StatusBadRequest,
			"service_id does not belong to an active booking service",
		)
	}
	var resource models.BookingResource
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"id = ? AND organization_id = ? AND is_active = ?",
		resourceID, orgID, true,
	).First(&resource).Error; err != nil {
		return nil, nil, newBookingCommerceClientError(
			fasthttp.StatusBadRequest,
			"resource_id does not belong to an active booking resource",
		)
	}
	var configured int64
	if err := tx.Model(&models.BookingServiceResource{}).
		Where("organization_id = ? AND service_id = ? AND is_active = ?", orgID, serviceID, true).
		Count(&configured).Error; err != nil {
		return nil, nil, err
	}
	if configured > 0 {
		var linked int64
		if err := tx.Model(&models.BookingServiceResource{}).
			Where(
				"organization_id = ? AND service_id = ? AND resource_id = ? AND is_active = ?",
				orgID, serviceID, resourceID, true,
			).
			Count(&linked).Error; err != nil {
			return nil, nil, err
		}
		if linked != 1 {
			return nil, nil, newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"resource_id is not eligible for the selected booking service",
			)
		}
	}
	return &service, &resource, nil
}

func ensureBookingEventAvailable(
	tx *gorm.DB,
	orgID, resourceID uuid.UUID,
	startsAt, endsAt time.Time,
	excludeEventID uuid.UUID,
) error {
	var timeOffCount int64
	if err := tx.Model(&models.ResourceTimeOff{}).
		Where(
			"organization_id = ? AND resource_id = ? AND starts_at < ? AND ends_at > ?",
			orgID, resourceID, endsAt, startsAt,
		).
		Count(&timeOffCount).Error; err != nil {
		return err
	}
	if timeOffCount > 0 {
		return newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Booking resource is unavailable during this interval",
		)
	}
	overlap := tx.Model(&models.BookingEvent{}).
		Where(
			"organization_id = ? AND resource_id = ? AND status = ? AND starts_at < ? AND ends_at > ?",
			orgID,
			resourceID,
			models.BookingEventStatusScheduled,
			endsAt,
			startsAt,
		)
	if excludeEventID != uuid.Nil {
		overlap = overlap.Where("id <> ?", excludeEventID)
	}
	var overlapCount int64
	if err := overlap.Count(&overlapCount).Error; err != nil {
		return err
	}
	if overlapCount > 0 {
		return newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Booking resource already has an overlapping event",
		)
	}
	return nil
}

func bookingCapacityStatuses() []models.BookingStatus {
	return []models.BookingStatus{
		models.BookingStatusReserved,
		models.BookingStatusConfirmed,
		models.BookingStatusCheckedIn,
		models.BookingStatusCompleted,
		models.BookingStatusNoShow,
	}
}

func bookingStatusOccupiesCapacity(status models.BookingStatus) bool {
	switch status {
	case models.BookingStatusReserved,
		models.BookingStatusConfirmed,
		models.BookingStatusCheckedIn,
		models.BookingStatusCompleted,
		models.BookingStatusNoShow:
		return true
	default:
		return false
	}
}

func bookingEventOccupiedQuantity(
	tx *gorm.DB,
	orgID, eventID, excludeBookingID uuid.UUID,
) (int, error) {
	query := tx.Model(&models.Booking{}).
		Where(
			"organization_id = ? AND event_id = ? AND status IN ?",
			orgID, eventID, bookingCapacityStatuses(),
		)
	if excludeBookingID != uuid.Nil {
		query = query.Where("id <> ?", excludeBookingID)
	}
	var quantity int64
	if err := query.Select("COALESCE(SUM(quantity), 0)").Scan(&quantity).Error; err != nil {
		return 0, err
	}
	if quantity > int64(math.MaxInt) {
		return 0, errors.New("occupied booking quantity overflow")
	}
	return int(quantity), nil
}

func bookingEventActiveQuantity(
	tx *gorm.DB,
	orgID, eventID uuid.UUID,
) (int, error) {
	var quantity int64
	if err := tx.Model(&models.Booking{}).
		Where(
			"organization_id = ? AND event_id = ? AND status IN ?",
			orgID,
			eventID,
			[]models.BookingStatus{
				models.BookingStatusReserved,
				models.BookingStatusConfirmed,
				models.BookingStatusCheckedIn,
			},
		).
		Select("COALESCE(SUM(quantity), 0)").
		Scan(&quantity).Error; err != nil {
		return 0, err
	}
	if quantity > int64(math.MaxInt) {
		return 0, errors.New("active booking quantity overflow")
	}
	return int(quantity), nil
}

func bumpBookingEventVersion(tx *gorm.DB, orgID, eventID uuid.UUID) error {
	result := tx.Model(&models.BookingEvent{}).
		Where("id = ? AND organization_id = ?", eventID, orgID).
		UpdateColumn("version", gorm.Expr("version + 1"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Booking event is unavailable",
		)
	}
	return nil
}

func loadBookingForResponse(db *gorm.DB, orgID uuid.UUID, booking *models.Booking) error {
	return db.
		Preload("Event", "organization_id = ?", orgID).
		Preload("Event.Service", "organization_id = ?", orgID).
		Preload("Event.Resource", "organization_id = ?", orgID).
		Preload("Contact", "organization_id = ?", orgID).
		Preload("ContactPackage", "organization_id = ?", orgID).
		Where("id = ? AND organization_id = ?", booking.ID, orgID).
		First(booking).Error
}

func bookingTransitionAllowed(from, to models.BookingStatus) bool {
	switch from {
	case models.BookingStatusReserved:
		return to == models.BookingStatusConfirmed ||
			to == models.BookingStatusCancelled
	case models.BookingStatusConfirmed:
		return to == models.BookingStatusCheckedIn ||
			to == models.BookingStatusNoShow ||
			to == models.BookingStatusCancelled
	case models.BookingStatusWaitlisted:
		return to == models.BookingStatusReserved ||
			to == models.BookingStatusConfirmed ||
			to == models.BookingStatusCancelled
	case models.BookingStatusCheckedIn:
		return to == models.BookingStatusCompleted ||
			to == models.BookingStatusNoShow
	default:
		return false
	}
}

func validatePackageRequest(req *PackageRequest, updating bool) error {
	req.Name = strings.TrimSpace(req.Name)
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 255 {
		return errors.New("name is required and must not exceed 255 characters")
	}
	if utf8.RuneCountInString(req.Description) > 10000 {
		return errors.New("description must not exceed 10000 characters")
	}
	if req.PriceMinor < 0 {
		return errors.New("price_minor must be zero or greater")
	}
	if !validCurrency(req.Currency) {
		return errors.New("currency must be a three-letter uppercase ISO code")
	}
	if req.ValidityDays < 1 || req.ValidityDays > 3650 {
		return errors.New("validity_days must be between 1 and 3650")
	}
	if updating && req.Version < 1 {
		return errors.New("version must be at least 1")
	}
	if !updating && len(req.Entitlements) == 0 {
		return errors.New("at least one entitlement is required")
	}
	if req.Entitlements != nil && len(req.Entitlements) == 0 {
		return errors.New("entitlements cannot be empty")
	}
	seen := make(map[uuid.UUID]struct{}, len(req.Entitlements))
	for _, entitlement := range req.Entitlements {
		if entitlement.BookingServiceID == uuid.Nil {
			return errors.New("entitlement booking_service_id is required")
		}
		if _, exists := seen[entitlement.BookingServiceID]; exists {
			return errors.New("entitlements must not repeat a booking service")
		}
		seen[entitlement.BookingServiceID] = struct{}{}
		if entitlement.IsUnlimited {
			if entitlement.Credits != 0 {
				return errors.New("unlimited entitlements must set credits to zero")
			}
		} else if entitlement.Credits < 1 || entitlement.Credits > 1000000 {
			return errors.New("finite entitlement credits must be between 1 and 1000000")
		}
	}
	return nil
}

func validatePackageEntitlementReferences(
	tx *gorm.DB,
	orgID uuid.UUID,
	inputs []PackageEntitlementInput,
) error {
	if len(inputs) == 0 {
		return nil
	}
	serviceIDs := make([]uuid.UUID, len(inputs))
	for i := range inputs {
		serviceIDs[i] = inputs[i].BookingServiceID
	}
	var count int64
	if err := tx.Model(&models.BookingService{}).
		Where(
			"organization_id = ? AND id IN ? AND is_active = ?",
			orgID, serviceIDs, true,
		).
		Count(&count).Error; err != nil {
		return err
	}
	if int(count) != len(serviceIDs) {
		return newBookingCommerceClientError(
			fasthttp.StatusBadRequest,
			"One or more entitlement services are inactive or outside the organization",
		)
	}
	return nil
}

func packageEntitlementsFromInput(
	orgID, packageID uuid.UUID,
	inputs []PackageEntitlementInput,
) []models.PackageEntitlement {
	entitlements := make([]models.PackageEntitlement, len(inputs))
	for i, input := range inputs {
		entitlements[i] = models.PackageEntitlement{
			BaseModel:           models.BaseModel{ID: uuid.New()},
			OrganizationID:      orgID,
			PackageDefinitionID: packageID,
			BookingServiceID:    input.BookingServiceID,
			Credits:             input.Credits,
			IsUnlimited:         input.IsUnlimited,
			Version:             1,
		}
	}
	return entitlements
}

func validateCreateContactPackageRequest(req *CreateContactPackageRequest) error {
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	req.Source = strings.TrimSpace(req.Source)
	if req.ContactID == uuid.Nil || req.PackageDefinitionID == uuid.Nil {
		return errors.New("contact_id and package_definition_id are required")
	}
	if req.InvoiceID != nil && *req.InvoiceID == uuid.Nil {
		return errors.New("invoice_id must be a valid UUID")
	}
	if req.IdempotencyKey == "" || utf8.RuneCountInString(req.IdempotencyKey) > 255 {
		return errors.New("idempotency_key is required and must not exceed 255 characters")
	}
	if utf8.RuneCountInString(req.Source) > 50 {
		return errors.New("source must not exceed 50 characters")
	}
	return nil
}

func validateSellContactPackageRequest(req *SellContactPackageRequest) error {
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.ContactID == uuid.Nil || req.PackageDefinitionID == uuid.Nil {
		return errors.New("contact_id and package_definition_id are required")
	}
	if req.IdempotencyKey == "" || utf8.RuneCountInString(req.IdempotencyKey) > 255 {
		return errors.New("idempotency_key is required and must not exceed 255 characters")
	}
	if req.DueAt != nil && req.DueAt.IsZero() {
		return errors.New("due_at must be a valid timestamp")
	}
	return nil
}

func sellContactPackageRequestFingerprint(req *SellContactPackageRequest) (string, error) {
	normalized := *req
	normalized.IdempotencyKey = ""
	if normalized.DueAt != nil {
		dueAt := normalized.DueAt.UTC()
		normalized.DueAt = &dueAt
	}
	normalized.Metadata = bookingCommerceJSON(normalized.Metadata)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func grantContactPackageCredits(
	tx *gorm.DB,
	orgID uuid.UUID,
	contactPackage *models.ContactPackage,
	userID uuid.UUID,
	now time.Time,
	idempotencyPrefix string,
) error {
	var entitlements []models.PackageEntitlement
	if err := tx.Where(
		"organization_id = ? AND package_definition_id = ?",
		orgID, contactPackage.PackageDefinitionID,
	).Order("created_at ASC").Find(&entitlements).Error; err != nil {
		return err
	}
	if len(entitlements) == 0 {
		return newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Package definition has no entitlements",
		)
	}
	for _, entitlement := range entitlements {
		key := idempotencyPrefix + ":" + contactPackage.ID.String() + ":" + entitlement.ID.String()
		var existing int64
		if err := tx.Model(&models.CreditLedgerEntry{}).
			Where("organization_id = ? AND idempotency_key = ?", orgID, key).
			Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			continue
		}
		granted := entitlement.Credits
		available := entitlement.Credits
		if entitlement.IsUnlimited {
			granted = 0
			available = 0
		}
		balance := models.CreditBalance{
			BaseModel:            models.BaseModel{ID: uuid.New()},
			OrganizationID:       orgID,
			ContactPackageID:     contactPackage.ID,
			PackageEntitlementID: entitlement.ID,
			Granted:              granted,
			Available:            available,
			Version:              1,
		}
		if err := tx.Create(&balance).Error; err != nil {
			return err
		}
		grantDelta := available
		if entitlement.IsUnlimited {
			// The ledger database invariant requires a non-zero movement. One
			// is an activation marker only; unlimited balances remain zeroed.
			grantDelta = 1
		}
		entry := models.CreditLedgerEntry{
			ID:                   uuid.New(),
			OrganizationID:       orgID,
			ContactPackageID:     contactPackage.ID,
			PackageEntitlementID: entitlement.ID,
			Type:                 models.CreditLedgerEntryTypeGrant,
			Delta:                grantDelta,
			BalanceAfter:         available,
			IdempotencyKey:       key,
			Reason:               "Package activated",
			ActorUserID:          &userID,
			Metadata: models.JSONB{
				"is_unlimited": entitlement.IsUnlimited,
			},
			OccurredAt: now.UTC(),
		}
		if err := tx.Create(&entry).Error; err != nil {
			return err
		}
	}
	return nil
}

func validateBookingContactPackage(
	tx *gorm.DB,
	orgID uuid.UUID,
	booking *models.Booking,
	serviceID uuid.UUID,
	lockBalance bool,
) error {
	_, _, _, err := loadBookingCreditBalance(
		tx, orgID, booking, serviceID, lockBalance,
	)
	return err
}

func loadBookingCreditBalance(
	tx *gorm.DB,
	orgID uuid.UUID,
	booking *models.Booking,
	serviceID uuid.UUID,
	lockBalance bool,
) (*models.ContactPackage, *models.PackageEntitlement, *models.CreditBalance, error) {
	if booking.ContactPackageID == nil {
		return nil, nil, nil, newBookingCommerceClientError(
			fasthttp.StatusBadRequest,
			"contact_package_id is required",
		)
	}
	now := time.Now().UTC()
	var contactPackage models.ContactPackage
	packageQuery := tx
	if lockBalance {
		packageQuery = packageQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := packageQuery.Where(
		"id = ? AND organization_id = ? AND contact_id = ? AND status = ?",
		*booking.ContactPackageID,
		orgID,
		booking.ContactID,
		models.ContactPackageStatusActive,
	).First(&contactPackage).Error; err != nil {
		return nil, nil, nil, newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Contact package is not active for this contact",
		)
	}
	if contactPackage.StartsAt != nil && contactPackage.StartsAt.After(now) {
		return nil, nil, nil, newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Contact package has not started",
		)
	}
	if contactPackage.ExpiresAt != nil && !contactPackage.ExpiresAt.After(now) {
		return nil, nil, nil, newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Contact package has expired",
		)
	}
	var entitlement models.PackageEntitlement
	if err := tx.Where(
		"organization_id = ? AND package_definition_id = ? AND booking_service_id = ?",
		orgID, contactPackage.PackageDefinitionID, serviceID,
	).First(&entitlement).Error; err != nil {
		return nil, nil, nil, newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Contact package does not include this booking service",
		)
	}
	balanceQuery := tx
	if lockBalance {
		balanceQuery = balanceQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var balance models.CreditBalance
	if err := balanceQuery.Where(
		"organization_id = ? AND contact_package_id = ? AND package_entitlement_id = ?",
		orgID, contactPackage.ID, entitlement.ID,
	).First(&balance).Error; err != nil {
		return nil, nil, nil, newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Contact package credit balance is unavailable",
		)
	}
	return &contactPackage, &entitlement, &balance, nil
}

func reserveBookingPackageCredit(
	tx *gorm.DB,
	orgID uuid.UUID,
	booking *models.Booking,
	serviceID, userID uuid.UUID,
	now time.Time,
) error {
	key := "booking-credit-reserve:" + booking.ID.String()
	var existing int64
	if err := tx.Model(&models.CreditLedgerEntry{}).
		Where("organization_id = ? AND idempotency_key = ?", orgID, key).
		Count(&existing).Error; err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	contactPackage, entitlement, balance, err := loadBookingCreditBalance(
		tx, orgID, booking, serviceID, true,
	)
	if err != nil {
		return err
	}
	if !entitlement.IsUnlimited && balance.Available < booking.Quantity {
		return newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Contact package does not have enough available credits",
		)
	}
	if !entitlement.IsUnlimited {
		balance.Available -= booking.Quantity
		balance.Reserved += booking.Quantity
		balance.Version++
		if err := tx.Model(&models.CreditBalance{}).
			Where("id = ? AND organization_id = ?", balance.ID, orgID).
			Updates(map[string]any{
				"available": balance.Available,
				"reserved":  balance.Reserved,
				"version":   balance.Version,
			}).Error; err != nil {
			return err
		}
	}
	entry := models.CreditLedgerEntry{
		ID:                   uuid.New(),
		OrganizationID:       orgID,
		ContactPackageID:     contactPackage.ID,
		PackageEntitlementID: entitlement.ID,
		BookingID:            &booking.ID,
		Type:                 models.CreditLedgerEntryTypeReserve,
		Delta:                -booking.Quantity,
		BalanceAfter:         balance.Available,
		IdempotencyKey:       key,
		Reason:               "Booking credit reserved",
		ActorUserID:          &userID,
		Metadata: models.JSONB{
			"is_unlimited": entitlement.IsUnlimited,
		},
		OccurredAt: now.UTC(),
	}
	return tx.Create(&entry).Error
}

func releaseBookingPackageCredit(
	tx *gorm.DB,
	orgID uuid.UUID,
	booking *models.Booking,
	serviceID, userID uuid.UUID,
	now time.Time,
) error {
	key := "booking-credit-release:" + booking.ID.String()
	var existing int64
	if err := tx.Model(&models.CreditLedgerEntry{}).
		Where("organization_id = ? AND idempotency_key = ?", orgID, key).
		Count(&existing).Error; err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	contactPackage, entitlement, balance, err := loadBookingCreditBalance(
		tx, orgID, booking, serviceID, true,
	)
	if err != nil {
		return err
	}
	if !entitlement.IsUnlimited && balance.Reserved < booking.Quantity {
		return newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Reserved package credits are inconsistent",
		)
	}
	if !entitlement.IsUnlimited {
		balance.Reserved -= booking.Quantity
		balance.Available += booking.Quantity
		balance.Version++
		if err := tx.Model(&models.CreditBalance{}).
			Where("id = ? AND organization_id = ?", balance.ID, orgID).
			Updates(map[string]any{
				"available": balance.Available,
				"reserved":  balance.Reserved,
				"version":   balance.Version,
			}).Error; err != nil {
			return err
		}
	}
	entry := models.CreditLedgerEntry{
		ID:                   uuid.New(),
		OrganizationID:       orgID,
		ContactPackageID:     contactPackage.ID,
		PackageEntitlementID: entitlement.ID,
		BookingID:            &booking.ID,
		Type:                 models.CreditLedgerEntryTypeRelease,
		Delta:                booking.Quantity,
		BalanceAfter:         balance.Available,
		IdempotencyKey:       key,
		Reason:               "Booking credit released",
		ActorUserID:          &userID,
		Metadata: models.JSONB{
			"is_unlimited": entitlement.IsUnlimited,
		},
		OccurredAt: now.UTC(),
	}
	return tx.Create(&entry).Error
}

func consumeBookingPackageCredit(
	tx *gorm.DB,
	orgID uuid.UUID,
	booking *models.Booking,
	serviceID, userID uuid.UUID,
	now time.Time,
) error {
	key := "booking-credit-redeem:" + booking.ID.String()
	var existing int64
	if err := tx.Model(&models.CreditLedgerEntry{}).
		Where("organization_id = ? AND idempotency_key = ?", orgID, key).
		Count(&existing).Error; err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	contactPackage, entitlement, balance, err := loadBookingCreditBalance(
		tx, orgID, booking, serviceID, true,
	)
	if err != nil {
		return err
	}
	if !entitlement.IsUnlimited && balance.Reserved < booking.Quantity {
		return newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Reserved package credits are inconsistent",
		)
	}
	if !entitlement.IsUnlimited {
		balance.Reserved -= booking.Quantity
		balance.Consumed += booking.Quantity
		balance.Version++
		if err := tx.Model(&models.CreditBalance{}).
			Where("id = ? AND organization_id = ?", balance.ID, orgID).
			Updates(map[string]any{
				"reserved": balance.Reserved,
				"consumed": balance.Consumed,
				"version":  balance.Version,
			}).Error; err != nil {
			return err
		}
	}
	entry := models.CreditLedgerEntry{
		ID:                   uuid.New(),
		OrganizationID:       orgID,
		ContactPackageID:     contactPackage.ID,
		PackageEntitlementID: entitlement.ID,
		BookingID:            &booking.ID,
		Type:                 models.CreditLedgerEntryTypeRedeem,
		Delta:                -booking.Quantity,
		BalanceAfter:         balance.Available,
		IdempotencyKey:       key,
		Reason:               "Booking credit consumed",
		ActorUserID:          &userID,
		Metadata: models.JSONB{
			"is_unlimited": entitlement.IsUnlimited,
		},
		OccurredAt: now.UTC(),
	}
	return tx.Create(&entry).Error
}

func validateCreateCommerceInvoiceRequest(req *CreateCommerceInvoiceRequest) error {
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.ContactID == uuid.Nil {
		return errors.New("contact_id is required")
	}
	if !validCurrency(req.Currency) {
		return errors.New("currency must be a three-letter uppercase ISO code")
	}
	if req.DiscountMinor < 0 {
		return errors.New("discount_minor must be zero or greater")
	}
	if len(req.Lines) < 1 || len(req.Lines) > 100 {
		return errors.New("lines must contain between 1 and 100 items")
	}
	if req.IdempotencyKey == "" || utf8.RuneCountInString(req.IdempotencyKey) > 255 {
		return errors.New("idempotency_key is required and must not exceed 255 characters")
	}
	if req.DueAt != nil {
		if req.DueAt.IsZero() {
			return errors.New("due_at must be a valid timestamp")
		}
		if req.DueAt.Before(time.Now().UTC().Add(-5 * time.Minute)) {
			return errors.New("due_at cannot be in the past")
		}
	}
	return nil
}

func commerceInvoiceRequestFingerprint(req *CreateCommerceInvoiceRequest) (string, error) {
	normalized := *req
	normalized.Currency = strings.ToUpper(strings.TrimSpace(normalized.Currency))
	normalized.IdempotencyKey = ""
	if normalized.DueAt != nil {
		dueAt := normalized.DueAt.UTC()
		normalized.DueAt = &dueAt
	}
	normalized.Metadata = bookingCommerceJSON(normalized.Metadata)
	for index := range normalized.Lines {
		normalized.Lines[index].Description =
			strings.TrimSpace(normalized.Lines[index].Description)
		normalized.Lines[index].Metadata =
			bookingCommerceJSON(normalized.Lines[index].Metadata)
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func buildCommerceInvoiceLines(
	tx *gorm.DB,
	orgID uuid.UUID,
	invoice *models.CommerceInvoice,
	inputs []CommerceInvoiceLineInput,
) ([]models.InvoiceLine, int64, int64, int64, error) {
	lines := make([]models.InvoiceLine, 0, len(inputs))
	var subtotalTotal int64
	var taxTotal int64
	var total int64
	for index, input := range inputs {
		linkedCount := 0
		if input.BookingID != nil {
			linkedCount++
		}
		if input.ContactPackageID != nil {
			linkedCount++
		}
		if input.PackageDefinitionID != nil {
			linkedCount++
		}
		if linkedCount > 1 {
			return nil, 0, 0, 0, newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				fmt.Sprintf("line %d may link to only one domain item", index+1),
			)
		}
		if input.TaxMinor < 0 {
			return nil, 0, 0, 0, newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				fmt.Sprintf("line %d tax_minor must be zero or greater", index+1),
			)
		}

		description := strings.TrimSpace(input.Description)
		quantity := input.Quantity
		unitAmount := int64(0)
		switch {
		case input.BookingID != nil:
			if *input.BookingID == uuid.Nil {
				return nil, 0, 0, 0, newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					fmt.Sprintf("line %d booking_id must be a valid UUID", index+1),
				)
			}
			var booking models.Booking
			if err := tx.
				Preload("Event", "organization_id = ?", orgID).
				Preload("Event.Service", "organization_id = ?", orgID).
				Where(
					"id = ? AND organization_id = ? AND contact_id = ?",
					*input.BookingID, orgID, invoice.ContactID,
				).
				First(&booking).Error; err != nil ||
				booking.Event == nil ||
				booking.Event.Service == nil {
				return nil, 0, 0, 0, newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					fmt.Sprintf("line %d booking_id does not belong to the invoice contact", index+1),
				)
			}
			if booking.Status == models.BookingStatusCancelled {
				return nil, 0, 0, 0, newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					fmt.Sprintf("line %d cannot invoice a cancelled booking", index+1),
				)
			}
			if booking.Event.Service.Currency != invoice.Currency {
				return nil, 0, 0, 0, newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					fmt.Sprintf("line %d currency does not match the invoice", index+1),
				)
			}
			quantity = booking.Quantity
			unitAmount = booking.Event.Service.PriceMinor
			if description == "" {
				description = booking.Event.Service.Name
			}
		case input.ContactPackageID != nil:
			if *input.ContactPackageID == uuid.Nil {
				return nil, 0, 0, 0, newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					fmt.Sprintf("line %d contact_package_id must be a valid UUID", index+1),
				)
			}
			var contactPackage models.ContactPackage
			if err := tx.Preload("PackageDefinition", "organization_id = ?", orgID).
				Where(
					"id = ? AND organization_id = ? AND contact_id = ?",
					*input.ContactPackageID, orgID, invoice.ContactID,
				).
				First(&contactPackage).Error; err != nil {
				return nil, 0, 0, 0, newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					fmt.Sprintf("line %d contact_package_id does not belong to the invoice contact", index+1),
				)
			}
			if contactPackage.Currency != invoice.Currency {
				return nil, 0, 0, 0, newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					fmt.Sprintf("line %d currency does not match the invoice", index+1),
				)
			}
			quantity = 1
			unitAmount = contactPackage.PurchaseAmountMinor
			if description == "" && contactPackage.PackageDefinition != nil {
				description = contactPackage.PackageDefinition.Name
			}
			if description == "" {
				description = "Package purchase"
			}
		case input.PackageDefinitionID != nil:
			if *input.PackageDefinitionID == uuid.Nil {
				return nil, 0, 0, 0, newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					fmt.Sprintf("line %d package_definition_id must be a valid UUID", index+1),
				)
			}
			var definition models.PackageDefinition
			if err := tx.Where(
				"id = ? AND organization_id = ? AND is_active = ?",
				*input.PackageDefinitionID, orgID, true,
			).First(&definition).Error; err != nil {
				return nil, 0, 0, 0, newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					fmt.Sprintf("line %d package_definition_id is not an active tenant package", index+1),
				)
			}
			if definition.Currency != invoice.Currency {
				return nil, 0, 0, 0, newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					fmt.Sprintf("line %d currency does not match the invoice", index+1),
				)
			}
			if quantity == 0 {
				quantity = 1
			}
			unitAmount = definition.PriceMinor
			if description == "" {
				description = definition.Name
			}
		default:
			if input.UnitAmountMinor == nil {
				return nil, 0, 0, 0, newBookingCommerceClientError(
					fasthttp.StatusBadRequest,
					fmt.Sprintf("line %d unit_amount_minor is required for a custom line", index+1),
				)
			}
			unitAmount = *input.UnitAmountMinor
			if quantity == 0 {
				quantity = 1
			}
		}
		if description == "" || utf8.RuneCountInString(description) > 1000 {
			return nil, 0, 0, 0, newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				fmt.Sprintf("line %d description is required and must not exceed 1000 characters", index+1),
			)
		}
		if quantity < 1 || quantity > 1000000 {
			return nil, 0, 0, 0, newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				fmt.Sprintf("line %d quantity must be between 1 and 1000000", index+1),
			)
		}
		if unitAmount < 0 {
			return nil, 0, 0, 0, newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				fmt.Sprintf("line %d unit amount must be zero or greater", index+1),
			)
		}
		lineSubtotal, err := safeMoneyMultiply(unitAmount, quantity)
		if err != nil {
			return nil, 0, 0, 0, newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				fmt.Sprintf("line %d amount is too large", index+1),
			)
		}
		lineTotal, err := safeMoneyAdd(lineSubtotal, input.TaxMinor)
		if err != nil {
			return nil, 0, 0, 0, newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				fmt.Sprintf("line %d total is too large", index+1),
			)
		}
		subtotalTotal, err = safeMoneyAdd(subtotalTotal, lineSubtotal)
		if err != nil {
			return nil, 0, 0, 0, newBookingCommerceClientError(
				fasthttp.StatusBadRequest, "invoice subtotal is too large",
			)
		}
		taxTotal, err = safeMoneyAdd(taxTotal, input.TaxMinor)
		if err != nil {
			return nil, 0, 0, 0, newBookingCommerceClientError(
				fasthttp.StatusBadRequest, "invoice tax is too large",
			)
		}
		total, err = safeMoneyAdd(total, lineTotal)
		if err != nil {
			return nil, 0, 0, 0, newBookingCommerceClientError(
				fasthttp.StatusBadRequest, "invoice total is too large",
			)
		}
		lines = append(lines, models.InvoiceLine{
			BaseModel:           models.BaseModel{ID: uuid.New()},
			OrganizationID:      orgID,
			InvoiceID:           invoice.ID,
			BookingID:           input.BookingID,
			ContactPackageID:    input.ContactPackageID,
			PackageDefinitionID: input.PackageDefinitionID,
			Description:         description,
			Quantity:            quantity,
			UnitAmountMinor:     unitAmount,
			SubtotalMinor:       lineSubtotal,
			TaxMinor:            input.TaxMinor,
			TotalMinor:          lineTotal,
			Metadata:            bookingCommerceJSON(input.Metadata),
			Version:             1,
		})
	}
	return lines, subtotalTotal, taxTotal, total, nil
}

func safeMoneyMultiply(amount int64, quantity int) (int64, error) {
	if amount < 0 || quantity < 0 {
		return 0, errors.New("money values cannot be negative")
	}
	if quantity != 0 && amount > math.MaxInt64/int64(quantity) {
		return 0, errors.New("money multiplication overflow")
	}
	return amount * int64(quantity), nil
}

func safeMoneyAdd(left, right int64) (int64, error) {
	if left < 0 || right < 0 || left > math.MaxInt64-right {
		return 0, errors.New("money addition overflow")
	}
	return left + right, nil
}

func newCommerceInvoiceNumber(now time.Time) string {
	suffix := strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:10])
	return fmt.Sprintf("INV-%s-%s", now.UTC().Format("20060102"), suffix)
}

func validateCreateInvoicePaymentIntentRequest(
	req *CreateInvoicePaymentIntentRequest,
) error {
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.ProviderAccountID == uuid.Nil {
		return errors.New("provider_account_id is required")
	}
	if req.AmountMinor != nil && *req.AmountMinor <= 0 {
		return errors.New("amount_minor must be greater than zero")
	}
	if req.IdempotencyKey == "" || utf8.RuneCountInString(req.IdempotencyKey) > 255 {
		return errors.New("idempotency_key is required and must not exceed 255 characters")
	}
	if req.ExpiresAt != nil {
		if req.ExpiresAt.IsZero() || !req.ExpiresAt.After(time.Now().UTC()) {
			return errors.New("expires_at must be a future timestamp")
		}
	}
	return nil
}

func validateRecordManualInvoicePaymentRequest(
	req *RecordManualInvoicePaymentRequest,
) error {
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	req.Reference = strings.TrimSpace(req.Reference)
	if !req.ConfirmManual {
		return errors.New("confirm_manual must be true to record an external manual payment")
	}
	if req.Version < 1 {
		return errors.New("version must be at least 1")
	}
	if req.AmountMinor <= 0 {
		return errors.New("amount_minor must be greater than zero")
	}
	if !validCurrency(req.Currency) {
		return errors.New("currency must be a three-letter uppercase ISO code")
	}
	if req.ProviderAccountID != nil && *req.ProviderAccountID == uuid.Nil {
		return errors.New("provider_account_id must be a valid UUID")
	}
	if req.Reference == "" || utf8.RuneCountInString(req.Reference) > 255 {
		return errors.New("reference is required and must not exceed 255 characters")
	}
	if req.IdempotencyKey == "" || utf8.RuneCountInString(req.IdempotencyKey) > 255 {
		return errors.New("idempotency_key is required and must not exceed 255 characters")
	}
	if utf8.RuneCountInString(req.Notes) > 10000 {
		return errors.New("notes must not exceed 10000 characters")
	}
	if req.OccurredAt != nil {
		if req.OccurredAt.IsZero() ||
			req.OccurredAt.After(time.Now().UTC().Add(5*time.Minute)) {
			return errors.New("occurred_at must be a valid timestamp that is not in the future")
		}
	}
	return nil
}

func resolveManualPaymentAccount(
	tx *gorm.DB,
	orgID, userID uuid.UUID,
	requestedID *uuid.UUID,
) (*models.PaymentProviderAccount, error) {
	if requestedID != nil {
		var account models.PaymentProviderAccount
		if err := tx.Where(
			"id = ? AND organization_id = ? AND is_active = ?",
			*requestedID, orgID, true,
		).First(&account).Error; err != nil {
			return nil, newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"provider_account_id does not belong to an active payment account",
			)
		}
		if !strings.EqualFold(strings.TrimSpace(account.Provider), "manual") {
			return nil, newBookingCommerceClientError(
				fasthttp.StatusBadRequest,
				"Only a manual payment account can record manual payments",
			)
		}
		return &account, nil
	}
	var organization models.Organization
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("id = ?", orgID).
		First(&organization).Error; err != nil {
		return nil, err
	}
	externalID := "manual:" + orgID.String()
	var account models.PaymentProviderAccount
	err := tx.Where(
		"organization_id = ? AND provider = ? AND external_account_id = ?",
		orgID, "manual", externalID,
	).First(&account).Error
	if err == nil {
		if !account.IsActive {
			return nil, newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"The organization's manual payment account is inactive",
			)
		}
		return &account, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	account = models.PaymentProviderAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    orgID,
		Name:              "Manual payments",
		Provider:          "manual",
		ExternalAccountID: externalID,
		Environment:       models.PaymentEnvironmentLive,
		PublicConfig:      models.JSONB{},
		IsActive:          true,
		Metadata: models.JSONB{
			"system_managed": true,
		},
		Version:     1,
		CreatedByID: &userID,
		UpdatedByID: &userID,
	}
	if err := tx.Create(&account).Error; err != nil {
		return nil, err
	}
	return &account, nil
}

func activatePaidInvoiceContactPackages(
	tx *gorm.DB,
	orgID uuid.UUID,
	invoice *models.CommerceInvoice,
	userID, paymentTransactionID uuid.UUID,
) error {
	var packages []models.ContactPackage
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND invoice_id = ? AND status = ?",
			orgID, invoice.ID, models.ContactPackageStatusPending,
		).
		Find(&packages).Error; err != nil {
		return err
	}
	now := time.Now().UTC()
	for i := range packages {
		contactPackage := &packages[i]
		old := *contactPackage
		if contactPackage.StartsAt == nil {
			startsAt := now
			contactPackage.StartsAt = &startsAt
		}
		contactPackage.Status = models.ContactPackageStatusActive
		contactPackage.Version++
		contactPackage.UpdatedByID = &userID
		result := tx.Model(&models.ContactPackage{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND version = ?",
				contactPackage.ID,
				orgID,
				models.ContactPackageStatusPending,
				old.Version,
			).
			Updates(map[string]any{
				"status":        contactPackage.Status,
				"starts_at":     contactPackage.StartsAt,
				"updated_by_id": userID,
				"version":       contactPackage.Version,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Contact package was modified while applying the payment",
			)
		}
		if err := grantContactPackageCredits(
			tx,
			orgID,
			contactPackage,
			userID,
			now,
			"contact-package-payment:"+paymentTransactionID.String(),
		); err != nil {
			return err
		}
		if err := audit.LogAudit(
			tx, orgID, userID, audit.GetUserName(tx, userID),
			contactPackageAuditResource, contactPackage.ID,
			models.AuditActionUpdated, &old, contactPackage,
		); err != nil {
			return err
		}
	}
	return nil
}
