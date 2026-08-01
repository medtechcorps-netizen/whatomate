package handlers

import (
	"errors"
	"fmt"
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
	availabilityRuleAuditResource = "availability_rule"
	resourceTimeOffAuditResource  = "resource_time_off"
)

// AvailabilityRuleRequest creates or replaces a same-day recurring window.
// Weekdays use Go's convention: Sunday=0 through Saturday=6. Effective dates
// are interpreted as inclusive calendar dates in the resource timezone.
type AvailabilityRuleRequest struct {
	Weekday        int     `json:"weekday"`
	StartLocalTime string  `json:"start_local_time"`
	EndLocalTime   string  `json:"end_local_time"`
	EffectiveFrom  *string `json:"effective_from,omitempty"`
	EffectiveUntil *string `json:"effective_until,omitempty"`
	IsActive       *bool   `json:"is_active,omitempty"`
	Version        int64   `json:"version,omitempty"`

	normalizedEffectiveFrom  *time.Time
	normalizedEffectiveUntil *time.Time
}

// ResourceTimeOffRequest creates or replaces an absolute unavailable window.
type ResourceTimeOffRequest struct {
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	Reason   string    `json:"reason,omitempty"`
	Version  int64     `json:"version,omitempty"`
}

// BookingVersionRequest guards destructive booking-settings changes.
type BookingVersionRequest struct {
	Version int64 `json:"version"`
}

// AvailabilityRuleResponse keeps the persisted timestamps for compatibility
// and also exposes canonical date-only fields so clients do not need to
// recover recurrence boundaries from timestamps.
type AvailabilityRuleResponse struct {
	models.AvailabilityRule
	EffectiveFromDate  *string `json:"effective_from_date,omitempty"`
	EffectiveUntilDate *string `json:"effective_until_date,omitempty"`
}

// ListAvailabilityRules lists recurring windows for one tenant resource.
func (a *App) ListAvailabilityRules(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionRead)
	if err != nil {
		return nil
	}
	resourceID, err := parsePathUUID(r, "resource_id", "booking resource")
	if err != nil {
		return nil
	}
	if err := ensureBookingResourceTenant(a.DB, orgID, resourceID, false); err != nil {
		return a.sendBookingCommerceError(r, "list availability rules", err)
	}
	if _, err := bookingResourceLocation(a.DB, orgID, resourceID); err != nil {
		return a.sendBookingCommerceError(r, "list availability rules", err)
	}

	pg := parsePagination(r)
	query := a.DB.Model(&models.AvailabilityRule{}).Where(
		"organization_id = ? AND resource_id = ?",
		orgID,
		resourceID,
	)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return a.sendBookingCommerceError(r, "list availability rules", err)
	}
	var rules []models.AvailabilityRule
	if err := pg.Apply(query.Order(
		"weekday ASC, start_local_time ASC, effective_from ASC NULLS FIRST, created_at ASC",
	)).Find(&rules).Error; err != nil {
		return a.sendBookingCommerceError(r, "list availability rules", err)
	}
	response := make([]AvailabilityRuleResponse, len(rules))
	for i := range rules {
		response[i] = availabilityRuleToResponse(&rules[i])
	}
	return r.SendEnvelope(listEnvelope("availability_rules", response, total, pg))
}

// CreateAvailabilityRule creates a versioned recurring window.
func (a *App) CreateAvailabilityRule(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionWrite)
	if err != nil {
		return nil
	}
	resourceID, err := parsePathUUID(r, "resource_id", "booking resource")
	if err != nil {
		return nil
	}
	var req AvailabilityRuleRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateAvailabilityRuleRequest(&req, false); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var rule models.AvailabilityRule
	var location *time.Location
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := ensureBookingResourceTenant(tx, orgID, resourceID, true); err != nil {
			return err
		}
		resourceLocation, err := bookingResourceLocation(tx, orgID, resourceID)
		if err != nil {
			return err
		}
		location = resourceLocation
		if err := normalizeAvailabilityRuleEffectiveDates(&req, location); err != nil {
			return err
		}
		active := true
		if req.IsActive != nil {
			active = *req.IsActive
		}
		rule = models.AvailabilityRule{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgID,
			ResourceID:     resourceID,
			Weekday:        req.Weekday,
			StartLocalTime: req.StartLocalTime,
			EndLocalTime:   req.EndLocalTime,
			EffectiveFrom:  req.normalizedEffectiveFrom,
			EffectiveUntil: req.normalizedEffectiveUntil,
			IsActive:       active,
			Version:        1,
			CreatedByID:    &userID,
			UpdatedByID:    &userID,
		}
		if err := ensureAvailabilityRuleDoesNotOverlap(
			tx,
			orgID,
			&rule,
			uuid.Nil,
		); err != nil {
			return err
		}
		if err := tx.Create(&rule).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			availabilityRuleAuditResource,
			rule.ID,
			models.AuditActionCreated,
			nil,
			&rule,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "create availability rule", err)
	}
	return r.SendEnvelope(availabilityRuleToResponse(&rule))
}

// UpdateAvailabilityRule replaces a recurring window with optimistic locking.
func (a *App) UpdateAvailabilityRule(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionWrite)
	if err != nil {
		return nil
	}
	resourceID, err := parsePathUUID(r, "resource_id", "booking resource")
	if err != nil {
		return nil
	}
	ruleID, err := parsePathUUID(r, "id", "availability rule")
	if err != nil {
		return nil
	}
	var req AvailabilityRuleRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateAvailabilityRuleRequest(&req, true); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var updated models.AvailabilityRule
	var location *time.Location
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := ensureBookingResourceTenant(tx, orgID, resourceID, true); err != nil {
			return err
		}
		var rule models.AvailabilityRule
		queryErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND resource_id = ?",
			ruleID,
			orgID,
			resourceID,
		).First(&rule).Error
		if queryErr != nil {
			if errors.Is(queryErr, gorm.ErrRecordNotFound) {
				return newBookingCommerceClientError(
					fasthttp.StatusNotFound,
					"Availability rule not found",
				)
			}
			return queryErr
		}
		if rule.Version != req.Version {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Availability rule was modified; refresh and retry",
			)
		}
		resourceLocation, err := bookingResourceLocation(tx, orgID, resourceID)
		if err != nil {
			return err
		}
		location = resourceLocation
		if err := normalizeAvailabilityRuleEffectiveDates(&req, location); err != nil {
			return err
		}
		active := rule.IsActive
		if req.IsActive != nil {
			active = *req.IsActive
		}
		candidate := models.AvailabilityRule{
			OrganizationID: orgID,
			ResourceID:     resourceID,
			Weekday:        req.Weekday,
			StartLocalTime: req.StartLocalTime,
			EndLocalTime:   req.EndLocalTime,
			EffectiveFrom:  req.normalizedEffectiveFrom,
			EffectiveUntil: req.normalizedEffectiveUntil,
			IsActive:       active,
		}
		if err := ensureAvailabilityRuleDoesNotOverlap(
			tx,
			orgID,
			&candidate,
			ruleID,
		); err != nil {
			return err
		}
		old := rule
		result := tx.Model(&models.AvailabilityRule{}).Where(
			"id = ? AND organization_id = ? AND resource_id = ? AND version = ?",
			ruleID,
			orgID,
			resourceID,
			req.Version,
		).Updates(map[string]any{
			"weekday":          req.Weekday,
			"start_local_time": req.StartLocalTime,
			"end_local_time":   req.EndLocalTime,
			"effective_from":   req.normalizedEffectiveFrom,
			"effective_until":  req.normalizedEffectiveUntil,
			"is_active":        active,
			"updated_by_id":    userID,
			"version":          gorm.Expr("version + 1"),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Availability rule was modified; refresh and retry",
			)
		}
		if err := tx.Where(
			"id = ? AND organization_id = ? AND resource_id = ?",
			ruleID,
			orgID,
			resourceID,
		).First(&updated).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			availabilityRuleAuditResource,
			ruleID,
			models.AuditActionUpdated,
			&old,
			&updated,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "update availability rule", err)
	}
	return r.SendEnvelope(availabilityRuleToResponse(&updated))
}

// DeleteAvailabilityRule soft-deletes a version-guarded recurring window.
func (a *App) DeleteAvailabilityRule(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionWrite)
	if err != nil {
		return nil
	}
	resourceID, err := parsePathUUID(r, "resource_id", "booking resource")
	if err != nil {
		return nil
	}
	ruleID, err := parsePathUUID(r, "id", "availability rule")
	if err != nil {
		return nil
	}
	var req BookingVersionRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.Version < 1 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "version must be at least 1", nil, "")
	}

	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := ensureBookingResourceTenant(tx, orgID, resourceID, true); err != nil {
			return err
		}
		var rule models.AvailabilityRule
		queryErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND resource_id = ?",
			ruleID,
			orgID,
			resourceID,
		).First(&rule).Error
		if queryErr != nil {
			if errors.Is(queryErr, gorm.ErrRecordNotFound) {
				return newBookingCommerceClientError(
					fasthttp.StatusNotFound,
					"Availability rule not found",
				)
			}
			return queryErr
		}
		if rule.Version != req.Version {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Availability rule was modified; refresh and retry",
			)
		}
		if err := tx.Delete(&rule).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			availabilityRuleAuditResource,
			ruleID,
			models.AuditActionDeleted,
			&rule,
			nil,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "delete availability rule", err)
	}
	return r.SendEnvelope(map[string]bool{"success": true})
}

// ListResourceTimeOff lists absolute unavailable windows for one resource.
func (a *App) ListResourceTimeOff(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionRead)
	if err != nil {
		return nil
	}
	resourceID, err := parsePathUUID(r, "resource_id", "booking resource")
	if err != nil {
		return nil
	}
	if err := ensureBookingResourceTenant(a.DB, orgID, resourceID, false); err != nil {
		return a.sendBookingCommerceError(r, "list resource time off", err)
	}

	pg := parsePagination(r)
	query := a.DB.Model(&models.ResourceTimeOff{}).Where(
		"organization_id = ? AND resource_id = ?",
		orgID,
		resourceID,
	)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return a.sendBookingCommerceError(r, "list resource time off", err)
	}
	var timeOff []models.ResourceTimeOff
	if err := pg.Apply(query.Order("starts_at ASC, created_at ASC")).Find(&timeOff).Error; err != nil {
		return a.sendBookingCommerceError(r, "list resource time off", err)
	}
	return r.SendEnvelope(listEnvelope("time_off", timeOff, total, pg))
}

// CreateResourceTimeOff blocks an absolute interval after checking for
// overlapping time-off and non-cancelled booking events.
func (a *App) CreateResourceTimeOff(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionWrite)
	if err != nil {
		return nil
	}
	resourceID, err := parsePathUUID(r, "resource_id", "booking resource")
	if err != nil {
		return nil
	}
	var req ResourceTimeOffRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateResourceTimeOffRequest(&req, false); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	timeOff := models.ResourceTimeOff{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		ResourceID:     resourceID,
		StartsAt:       req.StartsAt.UTC(),
		EndsAt:         req.EndsAt.UTC(),
		Reason:         strings.TrimSpace(req.Reason),
		Version:        1,
		CreatedByID:    &userID,
		UpdatedByID:    &userID,
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := ensureBookingResourceTenant(tx, orgID, resourceID, true); err != nil {
			return err
		}
		if err := ensureResourceTimeOffDoesNotConflict(
			tx,
			orgID,
			resourceID,
			timeOff.StartsAt,
			timeOff.EndsAt,
			uuid.Nil,
		); err != nil {
			return err
		}
		if err := tx.Create(&timeOff).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			resourceTimeOffAuditResource,
			timeOff.ID,
			models.AuditActionCreated,
			nil,
			&timeOff,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "create resource time off", err)
	}
	return r.SendEnvelope(timeOff)
}

// UpdateResourceTimeOff replaces a time-off interval with optimistic locking.
func (a *App) UpdateResourceTimeOff(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionWrite)
	if err != nil {
		return nil
	}
	resourceID, err := parsePathUUID(r, "resource_id", "booking resource")
	if err != nil {
		return nil
	}
	timeOffID, err := parsePathUUID(r, "id", "resource time off")
	if err != nil {
		return nil
	}
	var req ResourceTimeOffRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := validateResourceTimeOffRequest(&req, true); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var updated models.ResourceTimeOff
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := ensureBookingResourceTenant(tx, orgID, resourceID, true); err != nil {
			return err
		}
		var timeOff models.ResourceTimeOff
		queryErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND resource_id = ?",
			timeOffID,
			orgID,
			resourceID,
		).First(&timeOff).Error
		if queryErr != nil {
			if errors.Is(queryErr, gorm.ErrRecordNotFound) {
				return newBookingCommerceClientError(
					fasthttp.StatusNotFound,
					"Resource time off not found",
				)
			}
			return queryErr
		}
		if timeOff.Version != req.Version {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Resource time off was modified; refresh and retry",
			)
		}
		startsAt := req.StartsAt.UTC()
		endsAt := req.EndsAt.UTC()
		if err := ensureResourceTimeOffDoesNotConflict(
			tx,
			orgID,
			resourceID,
			startsAt,
			endsAt,
			timeOffID,
		); err != nil {
			return err
		}
		old := timeOff
		result := tx.Model(&models.ResourceTimeOff{}).Where(
			"id = ? AND organization_id = ? AND resource_id = ? AND version = ?",
			timeOffID,
			orgID,
			resourceID,
			req.Version,
		).Updates(map[string]any{
			"starts_at":     startsAt,
			"ends_at":       endsAt,
			"reason":        strings.TrimSpace(req.Reason),
			"updated_by_id": userID,
			"version":       gorm.Expr("version + 1"),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Resource time off was modified; refresh and retry",
			)
		}
		if err := tx.Where(
			"id = ? AND organization_id = ? AND resource_id = ?",
			timeOffID,
			orgID,
			resourceID,
		).First(&updated).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			resourceTimeOffAuditResource,
			timeOffID,
			models.AuditActionUpdated,
			&old,
			&updated,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "update resource time off", err)
	}
	return r.SendEnvelope(updated)
}

// DeleteResourceTimeOff soft-deletes a version-guarded time-off interval.
func (a *App) DeleteResourceTimeOff(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceBookingSettings, models.ActionWrite)
	if err != nil {
		return nil
	}
	resourceID, err := parsePathUUID(r, "resource_id", "booking resource")
	if err != nil {
		return nil
	}
	timeOffID, err := parsePathUUID(r, "id", "resource time off")
	if err != nil {
		return nil
	}
	var req BookingVersionRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.Version < 1 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "version must be at least 1", nil, "")
	}

	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := ensureBookingResourceTenant(tx, orgID, resourceID, true); err != nil {
			return err
		}
		var timeOff models.ResourceTimeOff
		queryErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"id = ? AND organization_id = ? AND resource_id = ?",
			timeOffID,
			orgID,
			resourceID,
		).First(&timeOff).Error
		if queryErr != nil {
			if errors.Is(queryErr, gorm.ErrRecordNotFound) {
				return newBookingCommerceClientError(
					fasthttp.StatusNotFound,
					"Resource time off not found",
				)
			}
			return queryErr
		}
		if timeOff.Version != req.Version {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Resource time off was modified; refresh and retry",
			)
		}
		if err := tx.Delete(&timeOff).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			resourceTimeOffAuditResource,
			timeOffID,
			models.AuditActionDeleted,
			&timeOff,
			nil,
		)
	})
	if err != nil {
		return a.sendBookingCommerceError(r, "delete resource time off", err)
	}
	return r.SendEnvelope(map[string]bool{"success": true})
}

func validateAvailabilityRuleRequest(req *AvailabilityRuleRequest, updating bool) error {
	if req.Weekday < int(time.Sunday) || req.Weekday > int(time.Saturday) {
		return errors.New("weekday must be between 0 (Sunday) and 6 (Saturday)")
	}
	req.StartLocalTime = strings.TrimSpace(req.StartLocalTime)
	req.EndLocalTime = strings.TrimSpace(req.EndLocalTime)
	startMinute, err := availabilityLocalMinute(req.StartLocalTime)
	if err != nil {
		return errors.New("start_local_time must use HH:MM in 24-hour time")
	}
	endMinute, err := availabilityLocalMinute(req.EndLocalTime)
	if err != nil {
		return errors.New("end_local_time must use HH:MM in 24-hour time")
	}
	if endMinute <= startMinute {
		return errors.New("availability rules must be same-day intervals with end_local_time after start_local_time")
	}
	if updating && req.Version < 1 {
		return errors.New("version must be at least 1")
	}
	return nil
}

func validateResourceTimeOffRequest(req *ResourceTimeOffRequest, updating bool) error {
	if req.StartsAt.IsZero() || req.EndsAt.IsZero() || !req.EndsAt.After(req.StartsAt) {
		return errors.New("starts_at and ends_at must define a valid increasing interval")
	}
	if utf8.RuneCountInString(req.Reason) > 10000 {
		return errors.New("reason must not exceed 10000 characters")
	}
	if updating && req.Version < 1 {
		return errors.New("version must be at least 1")
	}
	return nil
}

func availabilityLocalMinute(value string) (int, error) {
	if len(value) != len("00:00") || value[2] != ':' {
		return 0, errors.New("invalid local time")
	}
	parsed, err := time.Parse("15:04", value)
	if err != nil || parsed.Format("15:04") != value {
		return 0, errors.New("invalid local time")
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

func normalizeAvailabilityRuleEffectiveDates(
	req *AvailabilityRuleRequest,
	location *time.Location,
) error {
	from, err := normalizeAvailabilityEffectiveDate(req.EffectiveFrom, location)
	if err != nil {
		return newBookingCommerceClientError(fasthttp.StatusBadRequest, "effective_from is not a valid resource-local calendar date")
	}
	until, err := normalizeAvailabilityEffectiveDate(req.EffectiveUntil, location)
	if err != nil {
		return newBookingCommerceClientError(fasthttp.StatusBadRequest, "effective_until is not a valid resource-local calendar date")
	}
	req.normalizedEffectiveFrom = from
	req.normalizedEffectiveUntil = until
	if from != nil && until != nil && availabilityStoredDateKey(*until) < availabilityStoredDateKey(*from) {
		return newBookingCommerceClientError(
			fasthttp.StatusBadRequest,
			"effective_until must be on or after effective_from",
		)
	}
	return nil
}

func normalizeAvailabilityEffectiveDate(
	value *string,
	location *time.Location,
) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(*value)
	if raw == "" {
		return nil, errors.New("empty date")
	}
	var year int
	var month time.Month
	var day int
	if dateOnly, err := time.Parse("2006-01-02", raw); err == nil && dateOnly.Format("2006-01-02") == raw {
		year, month, day = dateOnly.Date()
	} else {
		instant, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, err
		}
		year, month, day = instant.In(location).Date()
	}
	// A date has no offset. Persisting UTC midnight as a sentinel keeps the
	// calendar date stable if the resource timezone changes later.
	normalized := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	return &normalized, nil
}

func availabilityLocalDateKey(value time.Time, location *time.Location) int {
	year, month, day := value.In(location).Date()
	return year*10000 + int(month)*100 + day
}

func availabilityStoredDateKey(value time.Time) int {
	year, month, day := value.UTC().Date()
	return year*10000 + int(month)*100 + day
}

func ensureBookingResourceTenant(
	tx *gorm.DB,
	orgID, resourceID uuid.UUID,
	lock bool,
) error {
	query := tx.Select("id").Where("id = ? AND organization_id = ?", resourceID, orgID)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var resource models.BookingResource
	if err := query.First(&resource).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return newBookingCommerceClientError(fasthttp.StatusNotFound, "Booking resource not found")
		}
		return err
	}
	return nil
}

func bookingResourceLocation(
	tx *gorm.DB,
	orgID, resourceID uuid.UUID,
) (*time.Location, error) {
	var resource models.BookingResource
	if err := tx.Select("id", "timezone").Where(
		"id = ? AND organization_id = ?",
		resourceID,
		orgID,
	).First(&resource).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, newBookingCommerceClientError(fasthttp.StatusNotFound, "Booking resource not found")
		}
		return nil, err
	}
	location, err := time.LoadLocation(strings.TrimSpace(resource.Timezone))
	if err != nil {
		return nil, newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Booking resource timezone is invalid; update the resource before managing availability",
		)
	}
	return location, nil
}

func ensureAvailabilityRuleDoesNotOverlap(
	tx *gorm.DB,
	orgID uuid.UUID,
	candidate *models.AvailabilityRule,
	excludeRuleID uuid.UUID,
) error {
	if !candidate.IsActive {
		return nil
	}
	query := tx.Where(
		"organization_id = ? AND resource_id = ? AND weekday = ? AND is_active = ?",
		orgID,
		candidate.ResourceID,
		candidate.Weekday,
		true,
	)
	if excludeRuleID != uuid.Nil {
		query = query.Where("id <> ?", excludeRuleID)
	}
	var existing []models.AvailabilityRule
	if err := query.Find(&existing).Error; err != nil {
		return err
	}
	candidateStart, _ := availabilityLocalMinute(candidate.StartLocalTime)
	candidateEnd, _ := availabilityLocalMinute(candidate.EndLocalTime)
	for i := range existing {
		otherStart, err := availabilityLocalMinute(existing[i].StartLocalTime)
		if err != nil {
			return fmt.Errorf("invalid persisted availability start time: %w", err)
		}
		otherEnd, err := availabilityLocalMinute(existing[i].EndLocalTime)
		if err != nil || otherEnd <= otherStart {
			return errors.New("invalid persisted availability interval")
		}
		if candidateStart >= otherEnd || candidateEnd <= otherStart {
			continue
		}
		if availabilityEffectiveRangesOverlap(candidate, &existing[i]) {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Availability rule overlaps an active rule for this resource",
			)
		}
	}
	return nil
}

func availabilityEffectiveRangesOverlap(
	left, right *models.AvailabilityRule,
) bool {
	if left.EffectiveUntil != nil && right.EffectiveFrom != nil &&
		availabilityStoredDateKey(*left.EffectiveUntil) < availabilityStoredDateKey(*right.EffectiveFrom) {
		return false
	}
	if right.EffectiveUntil != nil && left.EffectiveFrom != nil &&
		availabilityStoredDateKey(*right.EffectiveUntil) < availabilityStoredDateKey(*left.EffectiveFrom) {
		return false
	}
	return true
}

func ensureResourceTimeOffDoesNotConflict(
	tx *gorm.DB,
	orgID, resourceID uuid.UUID,
	startsAt, endsAt time.Time,
	excludeTimeOffID uuid.UUID,
) error {
	overlap := tx.Model(&models.ResourceTimeOff{}).Where(
		"organization_id = ? AND resource_id = ? AND starts_at < ? AND ends_at > ?",
		orgID,
		resourceID,
		endsAt,
		startsAt,
	)
	if excludeTimeOffID != uuid.Nil {
		overlap = overlap.Where("id <> ?", excludeTimeOffID)
	}
	var timeOffCount int64
	if err := overlap.Count(&timeOffCount).Error; err != nil {
		return err
	}
	if timeOffCount > 0 {
		return newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Resource time off overlaps an existing time-off interval",
		)
	}

	var eventCount int64
	if err := tx.Model(&models.BookingEvent{}).Where(
		"organization_id = ? AND resource_id = ? AND status <> ? AND starts_at < ? AND ends_at > ?",
		orgID,
		resourceID,
		models.BookingEventStatusCancelled,
		endsAt,
		startsAt,
	).Count(&eventCount).Error; err != nil {
		return err
	}
	if eventCount > 0 {
		return newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Resource time off conflicts with an existing non-cancelled booking event",
		)
	}
	return nil
}

// ensureBookingEventWithinRecurringAvailability applies active recurring
// windows in the resource timezone. Resources without any active rules
// retain the historical unrestricted behavior until a schedule is configured.
func ensureBookingEventWithinRecurringAvailability(
	tx *gorm.DB,
	orgID, resourceID uuid.UUID,
	startsAt, endsAt time.Time,
) error {
	var rules []models.AvailabilityRule
	if err := tx.Where(
		"organization_id = ? AND resource_id = ? AND is_active = ?",
		orgID,
		resourceID,
		true,
	).Find(&rules).Error; err != nil {
		return err
	}
	if len(rules) == 0 {
		return nil
	}
	location, err := bookingResourceLocation(tx, orgID, resourceID)
	if err != nil {
		return err
	}
	localStart := startsAt.In(location)
	localEnd := endsAt.In(location)
	if availabilityLocalDateKey(localStart, location) != availabilityLocalDateKey(localEnd, location) {
		return newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Scheduled events must remain within one resource-local calendar day",
		)
	}
	endProbe := endsAt.Add(-time.Nanosecond)
	if endProbe.Before(startsAt) {
		endProbe = startsAt
	}
	_, startOffset := localStart.Zone()
	_, endOffset := endProbe.In(location).Zone()
	if startOffset != endOffset {
		return newBookingCommerceClientError(
			fasthttp.StatusConflict,
			"Scheduled events cannot cross a daylight-saving timezone transition",
		)
	}

	eventDate := availabilityLocalDateKey(localStart, location)
	eventWeekday := int(localStart.Weekday())
	startOfDay := localClockNanoseconds(localStart)
	endOfDay := localClockNanoseconds(localEnd)
	for i := range rules {
		rule := &rules[i]
		if rule.Weekday != eventWeekday {
			continue
		}
		if rule.EffectiveFrom != nil && eventDate < availabilityStoredDateKey(*rule.EffectiveFrom) {
			continue
		}
		if rule.EffectiveUntil != nil && eventDate > availabilityStoredDateKey(*rule.EffectiveUntil) {
			continue
		}
		startMinute, parseErr := availabilityLocalMinute(rule.StartLocalTime)
		if parseErr != nil {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Booking resource has an invalid recurring availability configuration",
			)
		}
		endMinute, parseErr := availabilityLocalMinute(rule.EndLocalTime)
		if parseErr != nil || endMinute <= startMinute {
			return newBookingCommerceClientError(
				fasthttp.StatusConflict,
				"Booking resource has an unsupported overnight recurring availability rule",
			)
		}
		ruleStart := int64(startMinute) * int64(time.Minute)
		ruleEnd := int64(endMinute) * int64(time.Minute)
		if startOfDay >= ruleStart && endOfDay <= ruleEnd {
			return nil
		}
	}
	return newBookingCommerceClientError(
		fasthttp.StatusConflict,
		"Booking event falls outside the resource's recurring availability",
	)
}

func localClockNanoseconds(value time.Time) int64 {
	return int64(value.Hour())*int64(time.Hour) +
		int64(value.Minute())*int64(time.Minute) +
		int64(value.Second())*int64(time.Second) +
		int64(value.Nanosecond())
}

func availabilityRuleToResponse(rule *models.AvailabilityRule) AvailabilityRuleResponse {
	response := AvailabilityRuleResponse{AvailabilityRule: *rule}
	if rule.EffectiveFrom != nil {
		value := rule.EffectiveFrom.UTC().Format("2006-01-02")
		response.EffectiveFromDate = &value
	}
	if rule.EffectiveUntil != nil {
		value := rule.EffectiveUntil.UTC().Format("2006-01-02")
		response.EffectiveUntilDate = &value
	}
	return response
}
