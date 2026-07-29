package handlers

import (
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

const (
	crmInsightsDefaultDays       = 30
	crmLowBalanceThreshold       = 2
	crmPackageExpiringWindowDays = 14
	crmDormantWindowDays         = 60
	crmNoShowRecoveryWindowDays  = 90
	crmInsightsMaxDays           = 366
)

// CRMInsightsMoney is a tenant currency total expressed in minor units.
type CRMInsightsMoney struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
}

// CRMInsightsResponse is the stable cross-product CRM reporting contract.
// Sections that the caller may not read remain present with zero values.
type CRMInsightsResponse struct {
	Range       CRMInsightsRange    `json:"range"`
	Pipeline    CRMInsightsPipeline `json:"pipeline"`
	Bookings    CRMInsightsBookings `json:"bookings"`
	Revenue     CRMInsightsRevenue  `json:"revenue"`
	Packages    CRMInsightsPackages `json:"packages"`
	Tasks       CRMInsightsTasks    `json:"tasks"`
	GeneratedAt string              `json:"generated_at"`
}

type CRMInsightsRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type CRMInsightsPipeline struct {
	OpenCount      int64              `json:"open_count"`
	WonCount       int64              `json:"won_count"`
	LostCount      int64              `json:"lost_count"`
	ConversionRate float64            `json:"conversion_rate"`
	OpenValue      []CRMInsightsMoney `json:"open_value"`
}

type CRMInsightsBookings struct {
	Total          int64   `json:"total"`
	Completed      int64   `json:"completed"`
	NoShow         int64   `json:"no_show"`
	Cancelled      int64   `json:"cancelled"`
	AttendanceRate float64 `json:"attendance_rate"`
	NoShowRate     float64 `json:"no_show_rate"`
}

type CRMInsightsRevenue struct {
	Collected       []CRMInsightsMoney `json:"collected"`
	Outstanding     []CRMInsightsMoney `json:"outstanding"`
	OverdueInvoices int64              `json:"overdue_invoices"`
}

type CRMInsightsPackages struct {
	Active       int64 `json:"active"`
	LowBalance   int64 `json:"low_balance"`
	ExpiringSoon int64 `json:"expiring_soon"`
}

type CRMInsightsTasks struct {
	Open    int64 `json:"open"`
	Overdue int64 `json:"overdue"`
}

type crmInsightsPermissions struct {
	leads    bool
	tasks    bool
	bookings bool
	packages bool
	payments bool
	contacts bool
}

func (p crmInsightsPermissions) anyInsights() bool {
	return p.leads || p.tasks || p.bookings || p.packages || p.payments
}

func (a *App) crmReadPermissions(userID, orgID uuid.UUID) crmInsightsPermissions {
	return crmInsightsPermissions{
		leads:    a.HasPermission(userID, models.ResourceCRMLeads, models.ActionRead, orgID),
		tasks:    a.HasPermission(userID, models.ResourceTasks, models.ActionRead, orgID),
		bookings: a.HasPermission(userID, models.ResourceBookings, models.ActionRead, orgID),
		packages: a.HasPermission(userID, models.ResourcePackages, models.ActionRead, orgID),
		payments: a.HasPermission(userID, models.ResourcePayments, models.ActionRead, orgID),
		contacts: a.HasPermission(userID, models.ResourceContacts, models.ActionRead, orgID),
	}
}

// crmEntitledPermissions applies the product catalog to an already-authorized
// permission set. CRM Insights itself is a CRM feature; booking and commerce
// sections additionally require their own product entitlements.
func (a *App) crmEntitledPermissions(
	userID, orgID uuid.UUID,
	permissions crmInsightsPermissions,
) (crmInsightsPermissions, bool, error) {
	crmEnabled, err := a.HasProductEntitlement(userID, orgID, "crm.enabled")
	if err != nil || !crmEnabled {
		return crmInsightsPermissions{}, crmEnabled, err
	}

	capabilities := crmInsightsPermissions{
		leads:    permissions.leads,
		tasks:    permissions.tasks,
		contacts: permissions.contacts,
	}
	if permissions.bookings {
		capabilities.bookings, err = a.HasProductEntitlement(
			userID,
			orgID,
			"bookings.enabled",
		)
		if err != nil {
			return crmInsightsPermissions{}, true, err
		}
	}
	if permissions.packages || permissions.payments {
		commerceEnabled, entitlementErr := a.HasProductEntitlement(
			userID,
			orgID,
			"commerce.enabled",
		)
		if entitlementErr != nil {
			return crmInsightsPermissions{}, true, entitlementErr
		}
		capabilities.packages = permissions.packages && commerceEnabled
		capabilities.payments = permissions.payments && commerceEnabled
	}
	return capabilities, true, nil
}

func (a *App) resolveCRMInsightsCapabilities(
	r *fastglue.Request,
	userID, orgID uuid.UUID,
	permissions crmInsightsPermissions,
) (crmInsightsPermissions, bool) {
	capabilities, crmEnabled, err := a.crmEntitledPermissions(userID, orgID, permissions)
	if err != nil {
		a.Log.Error(
			"Failed to evaluate CRM insights entitlements",
			"error", err,
			"organization_id", orgID,
		)
		_ = r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Product entitlement could not be evaluated",
			nil,
			"",
		)
		return crmInsightsPermissions{}, false
	}
	if !crmEnabled {
		_ = r.SendErrorEnvelope(
			fasthttp.StatusPaymentRequired,
			"CRM Insights is not included in the organization's active plan",
			nil,
			"",
		)
		return crmInsightsPermissions{}, false
	}
	return capabilities, true
}

// GetCRMInsights returns permission-aware, tenant-scoped CRM, booking, package,
// task, and commerce metrics for a bounded reporting range.
func (a *App) GetCRMInsights(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	permissions := a.crmReadPermissions(userID, orgID)
	if !permissions.anyInsights() {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"You don't have permission to view CRM insights",
			nil,
			"",
		)
	}
	permissions, ok := a.resolveCRMInsightsCapabilities(r, userID, orgID, permissions)
	if !ok {
		return nil
	}
	if !permissions.anyInsights() {
		return r.SendErrorEnvelope(
			fasthttp.StatusPaymentRequired,
			"Your active plan does not include any permitted CRM Insights data sources",
			nil,
			"",
		)
	}

	now := time.Now().UTC()
	location, locationErr := a.crmInsightsLocation(orgID)
	if locationErr != nil {
		return a.sendCRMInsightsQueryError(r, "organization timezone", locationErr)
	}
	from, to, rangeErr := parseCRMInsightsRange(r, now, location)
	if rangeErr != "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, rangeErr, nil, "")
	}

	response := CRMInsightsResponse{
		Range: CRMInsightsRange{
			From: from.Format(time.RFC3339),
			To:   to.Format(time.RFC3339),
		},
		Pipeline: CRMInsightsPipeline{
			OpenValue: make([]CRMInsightsMoney, 0),
		},
		Revenue: CRMInsightsRevenue{
			Collected:   make([]CRMInsightsMoney, 0),
			Outstanding: make([]CRMInsightsMoney, 0),
		},
		GeneratedAt: now.Format(time.RFC3339),
	}

	if permissions.leads {
		if err := a.populateCRMInsightsPipeline(orgID, from, to, &response.Pipeline); err != nil {
			return a.sendCRMInsightsQueryError(r, "pipeline", err)
		}
	}
	if permissions.bookings {
		if err := a.populateCRMInsightsBookings(orgID, from, to, &response.Bookings); err != nil {
			return a.sendCRMInsightsQueryError(r, "bookings", err)
		}
	}
	if permissions.payments {
		if err := a.populateCRMInsightsRevenue(orgID, from, to, now, &response.Revenue); err != nil {
			return a.sendCRMInsightsQueryError(r, "revenue", err)
		}
	}
	if permissions.packages {
		if err := a.populateCRMInsightsPackages(orgID, now, &response.Packages); err != nil {
			return a.sendCRMInsightsQueryError(r, "packages", err)
		}
	}
	if permissions.tasks {
		if err := a.populateCRMInsightsTasks(orgID, now, &response.Tasks); err != nil {
			return a.sendCRMInsightsQueryError(r, "tasks", err)
		}
	}

	return r.SendEnvelope(response)
}

func (a *App) crmInsightsLocation(orgID uuid.UUID) (*time.Location, error) {
	var organization models.Organization
	if err := a.DB.Select("id", "settings").
		Where("id = ?", orgID).
		First(&organization).Error; err != nil {
		return nil, err
	}
	timezone := "UTC"
	if configured, ok := organization.Settings["timezone"].(string); ok &&
		strings.TrimSpace(configured) != "" {
		timezone = strings.TrimSpace(configured)
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		a.Log.Warn(
			"Invalid organization timezone; CRM insights will use UTC",
			"organization_id", orgID,
			"timezone", timezone,
		)
		return time.UTC, nil
	}
	return location, nil
}

func parseCRMInsightsRange(
	r *fastglue.Request,
	now time.Time,
	location *time.Location,
) (time.Time, time.Time, string) {
	if location == nil {
		location = time.UTC
	}
	fromString := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("from")))
	toString := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("to")))
	if fromString == "" && toString == "" {
		to := now.In(location)
		from := time.Date(
			to.Year(),
			to.Month(),
			to.Day(),
			0,
			0,
			0,
			0,
			location,
		).AddDate(0, 0, -(crmInsightsDefaultDays - 1))
		return from.UTC(), to.UTC(), ""
	}
	if fromString == "" || toString == "" {
		return time.Time{}, time.Time{}, "Both from and to are required. Use YYYY-MM-DD"
	}
	from, err := time.ParseInLocation("2006-01-02", fromString, location)
	if err != nil {
		return time.Time{}, time.Time{}, "Invalid start date format. Use YYYY-MM-DD"
	}
	toDate, err := time.ParseInLocation("2006-01-02", toString, location)
	if err != nil {
		return time.Time{}, time.Time{}, "Invalid end date format. Use YYYY-MM-DD"
	}
	to := toDate.AddDate(0, 0, 1).Add(-time.Nanosecond)
	if from.After(to) {
		return time.Time{}, time.Time{}, "from must be on or before to"
	}
	if toDate.After(from.AddDate(0, 0, crmInsightsMaxDays-1)) {
		return time.Time{}, time.Time{}, "Reporting period cannot exceed 366 days"
	}
	return from.UTC(), to.UTC(), ""
}

func (a *App) sendCRMInsightsQueryError(
	r *fastglue.Request,
	section string,
	err error,
) error {
	a.Log.Error("Failed to query CRM insights", "section", section, "error", err)
	return r.SendErrorEnvelope(
		fasthttp.StatusInternalServerError,
		"Failed to load CRM insights",
		nil,
		"",
	)
}

func (a *App) populateCRMInsightsPipeline(
	orgID uuid.UUID,
	from, to time.Time,
	target *CRMInsightsPipeline,
) error {
	var counts struct {
		OpenCount int64
		WonCount  int64
		LostCount int64
	}
	if err := a.DB.Model(&models.CRMLead{}).
		Select(`
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS open_count,
			COALESCE(SUM(
				CASE WHEN status = ? AND won_at >= ? AND won_at <= ? THEN 1 ELSE 0 END
			), 0) AS won_count,
			COALESCE(SUM(
				CASE WHEN status = ? AND lost_at >= ? AND lost_at <= ? THEN 1 ELSE 0 END
			), 0) AS lost_count
		`,
			models.CRMLeadStatusOpen,
			models.CRMLeadStatusWon,
			from,
			to,
			models.CRMLeadStatusLost,
			from,
			to,
		).
		Where("organization_id = ?", orgID).
		Scan(&counts).Error; err != nil {
		return err
	}

	target.OpenCount = counts.OpenCount
	target.WonCount = counts.WonCount
	target.LostCount = counts.LostCount
	target.ConversionRate = crmPercentage(counts.WonCount, counts.WonCount+counts.LostCount)

	target.OpenValue = make([]CRMInsightsMoney, 0)
	if err := a.DB.Model(&models.CRMLead{}).
		Select("currency, COALESCE(SUM(value_minor), 0) AS amount_minor").
		Where(
			"organization_id = ? AND status = ?",
			orgID,
			models.CRMLeadStatusOpen,
		).
		Group("currency").
		Order("currency").
		Scan(&target.OpenValue).Error; err != nil {
		return err
	}
	return nil
}

func (a *App) populateCRMInsightsBookings(
	orgID uuid.UUID,
	from, to time.Time,
	target *CRMInsightsBookings,
) error {
	var counts struct {
		Total     int64
		Completed int64
		NoShow    int64
		Cancelled int64
	}
	if err := a.DB.Table("bookings AS b").
		Select(`
			COUNT(*) AS total,
			COALESCE(SUM(CASE WHEN b.status = ? THEN 1 ELSE 0 END), 0) AS completed,
			COALESCE(SUM(CASE WHEN b.status = ? THEN 1 ELSE 0 END), 0) AS no_show,
			COALESCE(SUM(CASE WHEN b.status = ? THEN 1 ELSE 0 END), 0) AS cancelled
		`,
			models.BookingStatusCompleted,
			models.BookingStatusNoShow,
			models.BookingStatusCancelled,
		).
		Joins(`
			JOIN booking_events AS be
			  ON be.id = b.event_id
			 AND be.organization_id = b.organization_id
			 AND be.deleted_at IS NULL
		`).
		Where(`
			b.organization_id = ?
			AND b.deleted_at IS NULL
			AND be.starts_at >= ?
			AND be.starts_at <= ?
		`, orgID, from, to).
		Scan(&counts).Error; err != nil {
		return err
	}

	target.Total = counts.Total
	target.Completed = counts.Completed
	target.NoShow = counts.NoShow
	target.Cancelled = counts.Cancelled
	attendanceDecisions := counts.Completed + counts.NoShow
	target.AttendanceRate = crmPercentage(counts.Completed, attendanceDecisions)
	target.NoShowRate = crmPercentage(counts.NoShow, attendanceDecisions)
	return nil
}

func (a *App) populateCRMInsightsRevenue(
	orgID uuid.UUID,
	from, to, now time.Time,
	target *CRMInsightsRevenue,
) error {
	target.Collected = make([]CRMInsightsMoney, 0)
	if err := a.DB.Table("payment_transactions").
		Select(`
			currency,
			COALESCE(SUM(
				CASE
					WHEN type = ? THEN -ABS(amount_minor)
					ELSE amount_minor
				END
			), 0) AS amount_minor
		`, models.PaymentTransactionTypeRefund).
		Where(`
			organization_id = ?
			AND status = ?
			AND type IN ?
			AND occurred_at >= ?
			AND occurred_at <= ?
		`,
			orgID,
			models.PaymentTransactionStatusSucceeded,
			[]models.PaymentTransactionType{
				models.PaymentTransactionTypeCharge,
				models.PaymentTransactionTypeRefund,
			},
			from,
			to,
		).
		Group("currency").
		Order("currency").
		Scan(&target.Collected).Error; err != nil {
		return err
	}

	target.Outstanding = make([]CRMInsightsMoney, 0)
	currentOpenInvoices := `
		organization_id = ?
		AND deleted_at IS NULL
		AND status = ?
		AND due_minor > 0
	`
	if err := a.DB.Table("commerce_invoices").
		Select("currency, COALESCE(SUM(due_minor), 0) AS amount_minor").
		Where(
			currentOpenInvoices,
			orgID,
			models.CommerceInvoiceStatusOpen,
		).
		Group("currency").
		Order("currency").
		Scan(&target.Outstanding).Error; err != nil {
		return err
	}

	if err := a.DB.Table("commerce_invoices").
		Where(
			currentOpenInvoices+" AND due_at IS NOT NULL AND due_at < ?",
			orgID,
			models.CommerceInvoiceStatusOpen,
			now,
		).
		Count(&target.OverdueInvoices).Error; err != nil {
		return err
	}
	return nil
}

func (a *App) populateCRMInsightsPackages(
	orgID uuid.UUID,
	now time.Time,
	target *CRMInsightsPackages,
) error {
	base := a.DB.Table("contact_packages AS cp").
		Where(`
			cp.organization_id = ?
			AND cp.deleted_at IS NULL
		`, orgID)

	if err := base.
		Where("cp.status = ?", models.ContactPackageStatusActive).
		Count(&target.Active).Error; err != nil {
		return err
	}

	lowBalanceSQL := `
		EXISTS (
			SELECT 1
			FROM credit_balances AS cb
			JOIN package_entitlements AS pe
			  ON pe.id = cb.package_entitlement_id
			 AND pe.organization_id = cb.organization_id
			 AND pe.deleted_at IS NULL
			WHERE cb.organization_id = cp.organization_id
			  AND cb.contact_package_id = cp.id
			  AND cb.deleted_at IS NULL
			  AND pe.is_unlimited = FALSE
			GROUP BY cb.contact_package_id
			HAVING COALESCE(SUM(cb.available), 0) <= ?
		)
	`
	if err := base.
		Where("cp.status = ?", models.ContactPackageStatusActive).
		Where(lowBalanceSQL, crmLowBalanceThreshold).
		Count(&target.LowBalance).Error; err != nil {
		return err
	}

	expiringBefore := now.AddDate(0, 0, crmPackageExpiringWindowDays)
	if err := base.
		Where(`
			cp.status = ?
			AND cp.expires_at IS NOT NULL
			AND cp.expires_at > ?
			AND cp.expires_at <= ?
		`, models.ContactPackageStatusActive, now, expiringBefore).
		Count(&target.ExpiringSoon).Error; err != nil {
		return err
	}
	return nil
}

func (a *App) populateCRMInsightsTasks(
	orgID uuid.UUID,
	now time.Time,
	target *CRMInsightsTasks,
) error {
	base := a.DB.Model(&models.FollowUpTask{}).
		Where(
			"organization_id = ? AND status IN ?",
			orgID,
			[]models.FollowUpTaskStatus{
				models.FollowUpTaskStatusOpen,
				models.FollowUpTaskStatusInProgress,
			},
		)
	if err := base.Count(&target.Open).Error; err != nil {
		return err
	}
	if err := base.
		Where("due_at IS NOT NULL AND due_at < ?", now).
		Count(&target.Overdue).Error; err != nil {
		return err
	}
	return nil
}

func crmPercentage(numerator, denominator int64) float64 {
	if numerator <= 0 || denominator <= 0 {
		return 0
	}
	value := (float64(numerator) / float64(denominator)) * 100
	return math.Min(100, math.Max(0, math.Round(value*100)/100))
}

// CRMSystemSegment is a live, system-defined contact segment. Membership is
// evaluated at request time so it cannot become stale.
type CRMSystemSegment struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Count       int64  `json:"count"`
}

// CRMSystemSegmentContact is the safe contact reference returned by segment
// membership endpoints.
type CRMSystemSegmentContact struct {
	ID             uuid.UUID  `json:"id"`
	ProfileName    string     `json:"profile_name"`
	PhoneNumber    string     `json:"phone_number"`
	AssignedUserID *uuid.UUID `json:"assigned_user_id,omitempty"`
	LastMessageAt  *time.Time `json:"last_message_at,omitempty"`
}

type crmSystemSegmentSpec struct {
	Key               string
	Label             string
	Description       string
	RequiredResources []string
	Where             func(time.Time) (string, []any)
}

var crmSystemSegmentOrder = []string{
	"needs_follow_up",
	"no_show_recovery",
	"overdue_invoice",
	"package_low",
	"package_expiring",
	"dormant",
	"open_opportunity",
}

func crmSystemSegments() map[string]crmSystemSegmentSpec {
	return map[string]crmSystemSegmentSpec{
		"needs_follow_up": {
			Key:               "needs_follow_up",
			Label:             "Needs follow-up",
			Description:       "Contacts with an open task that is due or has no due date.",
			RequiredResources: []string{models.ResourceTasks},
			Where: func(now time.Time) (string, []any) {
				return `
					EXISTS (
						SELECT 1
						FROM follow_up_tasks AS ft
						WHERE ft.organization_id = contacts.organization_id
						  AND ft.contact_id = contacts.id
						  AND ft.deleted_at IS NULL
						  AND ft.status IN ?
						  AND (ft.due_at IS NULL OR ft.due_at <= ?)
					)
				`, []any{
						[]models.FollowUpTaskStatus{
							models.FollowUpTaskStatusOpen,
							models.FollowUpTaskStatusInProgress,
						},
						now,
					}
			},
		},
		"no_show_recovery": {
			Key:               "no_show_recovery",
			Label:             "No-show recovery",
			Description:       "Recent no-shows without a later recovered booking.",
			RequiredResources: []string{models.ResourceBookings},
			Where: func(now time.Time) (string, []any) {
				return `
					EXISTS (
						SELECT 1
						FROM bookings AS missed
						JOIN booking_events AS missed_event
						  ON missed_event.id = missed.event_id
						 AND missed_event.organization_id = missed.organization_id
						 AND missed_event.deleted_at IS NULL
						WHERE missed.organization_id = contacts.organization_id
						  AND missed.contact_id = contacts.id
						  AND missed.deleted_at IS NULL
						  AND missed.status = ?
						  AND missed_event.starts_at >= ?
						  AND NOT EXISTS (
							  SELECT 1
							  FROM bookings AS recovered
							  JOIN booking_events AS recovered_event
							    ON recovered_event.id = recovered.event_id
							   AND recovered_event.organization_id = recovered.organization_id
							   AND recovered_event.deleted_at IS NULL
							  WHERE recovered.organization_id = missed.organization_id
							    AND recovered.contact_id = missed.contact_id
							    AND recovered.deleted_at IS NULL
							    AND recovered.status IN ?
							    AND recovered_event.starts_at > missed_event.starts_at
						  )
					)
				`, []any{
						models.BookingStatusNoShow,
						now.AddDate(0, 0, -crmNoShowRecoveryWindowDays),
						[]models.BookingStatus{
							models.BookingStatusReserved,
							models.BookingStatusConfirmed,
							models.BookingStatusWaitlisted,
							models.BookingStatusCheckedIn,
							models.BookingStatusCompleted,
						},
					}
			},
		},
		"overdue_invoice": {
			Key:               "overdue_invoice",
			Label:             "Overdue invoice",
			Description:       "Contacts with an unpaid invoice past its due date.",
			RequiredResources: []string{models.ResourcePayments},
			Where: func(now time.Time) (string, []any) {
				return `
					EXISTS (
						SELECT 1
						FROM commerce_invoices AS ci
						WHERE ci.organization_id = contacts.organization_id
						  AND ci.contact_id = contacts.id
						  AND ci.deleted_at IS NULL
						  AND ci.status = ?
						  AND ci.due_minor > 0
						  AND ci.due_at IS NOT NULL
						  AND ci.due_at < ?
					)
				`, []any{models.CommerceInvoiceStatusOpen, now}
			},
		},
		"package_low": {
			Key:               "package_low",
			Label:             "Package balance low",
			Description:       "Contacts with two or fewer finite package credits remaining.",
			RequiredResources: []string{models.ResourcePackages},
			Where: func(_ time.Time) (string, []any) {
				return `
					EXISTS (
						SELECT 1
						FROM contact_packages AS cp
						JOIN credit_balances AS cb
						  ON cb.contact_package_id = cp.id
						 AND cb.organization_id = cp.organization_id
						 AND cb.deleted_at IS NULL
						JOIN package_entitlements AS pe
						  ON pe.id = cb.package_entitlement_id
						 AND pe.organization_id = cb.organization_id
						 AND pe.deleted_at IS NULL
						WHERE cp.organization_id = contacts.organization_id
						  AND cp.contact_id = contacts.id
						  AND cp.deleted_at IS NULL
						  AND cp.status = ?
						  AND pe.is_unlimited = FALSE
						GROUP BY cp.id
						HAVING COALESCE(SUM(cb.available), 0) <= ?
					)
				`, []any{models.ContactPackageStatusActive, crmLowBalanceThreshold}
			},
		},
		"package_expiring": {
			Key:               "package_expiring",
			Label:             "Package expiring",
			Description:       "Contacts with an active package expiring in the next 14 days.",
			RequiredResources: []string{models.ResourcePackages},
			Where: func(now time.Time) (string, []any) {
				return `
					EXISTS (
						SELECT 1
						FROM contact_packages AS cp
						WHERE cp.organization_id = contacts.organization_id
						  AND cp.contact_id = contacts.id
						  AND cp.deleted_at IS NULL
						  AND cp.status = ?
						  AND cp.expires_at IS NOT NULL
						  AND cp.expires_at > ?
						  AND cp.expires_at <= ?
					)
				`, []any{
						models.ContactPackageStatusActive,
						now,
						now.AddDate(0, 0, crmPackageExpiringWindowDays),
					}
			},
		},
		"dormant": {
			Key:         "dormant",
			Label:       "Dormant",
			Description: "Contacts inactive for 60 days without an upcoming booking.",
			RequiredResources: []string{
				models.ResourceContacts,
				models.ResourceBookings,
			},
			Where: func(now time.Time) (string, []any) {
				return `
					COALESCE(contacts.last_message_at, contacts.created_at) < ?
					AND NOT EXISTS (
						SELECT 1
						FROM bookings AS future_booking
						JOIN booking_events AS future_event
						  ON future_event.id = future_booking.event_id
						 AND future_event.organization_id = future_booking.organization_id
						 AND future_event.deleted_at IS NULL
						WHERE future_booking.organization_id = contacts.organization_id
						  AND future_booking.contact_id = contacts.id
						  AND future_booking.deleted_at IS NULL
						  AND future_booking.status IN ?
						  AND future_event.starts_at >= ?
					)
				`, []any{
						now.AddDate(0, 0, -crmDormantWindowDays),
						[]models.BookingStatus{
							models.BookingStatusReserved,
							models.BookingStatusConfirmed,
							models.BookingStatusWaitlisted,
							models.BookingStatusCheckedIn,
						},
						now,
					}
			},
		},
		"open_opportunity": {
			Key:               "open_opportunity",
			Label:             "Open opportunity",
			Description:       "Contacts with at least one open CRM opportunity.",
			RequiredResources: []string{models.ResourceCRMLeads},
			Where: func(_ time.Time) (string, []any) {
				return `
					EXISTS (
						SELECT 1
						FROM crm_leads AS lead
						WHERE lead.organization_id = contacts.organization_id
						  AND lead.contact_id = contacts.id
						  AND lead.deleted_at IS NULL
						  AND lead.status = ?
					)
				`, []any{models.CRMLeadStatusOpen}
			},
		},
	}
}

// ListCRMSystemSegments returns the live system segment definitions and
// tenant-scoped counts the caller has permission to inspect.
func (a *App) ListCRMSystemSegments(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	permissions := a.crmReadPermissions(userID, orgID)
	if !permissions.contacts {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"You don't have permission to view contact segments",
			nil,
			"",
		)
	}
	permissions, ok := a.resolveCRMInsightsCapabilities(r, userID, orgID, permissions)
	if !ok {
		return nil
	}

	now := time.Now().UTC()
	specs := crmSystemSegments()
	segments := make([]CRMSystemSegment, 0, len(specs))
	for _, key := range crmSystemSegmentOrder {
		spec := specs[key]
		if !crmSegmentPermissionAllowed(spec, permissions) {
			continue
		}
		count, countErr := a.countCRMSystemSegment(orgID, spec, now)
		if countErr != nil {
			a.Log.Error(
				"Failed to count CRM system segment",
				"key", spec.Key,
				"error", countErr,
			)
			return r.SendErrorEnvelope(
				fasthttp.StatusInternalServerError,
				"Failed to load CRM segments",
				nil,
				"",
			)
		}
		segments = append(segments, CRMSystemSegment{
			Key:         spec.Key,
			Label:       spec.Label,
			Description: spec.Description,
			Count:       count,
		})
	}

	return r.SendEnvelope(map[string]any{"segments": segments})
}

// ListCRMSystemSegmentContacts returns one live segment's tenant-scoped,
// paginated contact references.
func (a *App) ListCRMSystemSegmentContacts(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	key := crmSegmentPathKey(r)
	spec, ok := crmSystemSegments()[key]
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "CRM segment not found", nil, "")
	}

	permissions := a.crmReadPermissions(userID, orgID)
	if !permissions.contacts || !crmSegmentPermissionAllowed(spec, permissions) {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"You don't have permission to view this contact segment",
			nil,
			"",
		)
	}
	permissions, entitled := a.resolveCRMInsightsCapabilities(r, userID, orgID, permissions)
	if !entitled {
		return nil
	}
	if !crmSegmentPermissionAllowed(spec, permissions) {
		return r.SendErrorEnvelope(
			fasthttp.StatusPaymentRequired,
			"This customer segment depends on a feature outside the organization's active plan",
			nil,
			"",
		)
	}

	now := time.Now().UTC()
	where, args := spec.Where(now)
	base := a.DB.Model(&models.Contact{}).
		Where("contacts.organization_id = ?", orgID).
		Where(where, args...)

	var total int64
	if err := base.Count(&total).Error; err != nil {
		a.Log.Error("Failed to count CRM segment contacts", "key", key, "error", err)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to load CRM segment",
			nil,
			"",
		)
	}

	pagination := parsePagination(r)
	contacts := make([]CRMSystemSegmentContact, 0)
	if err := pagination.Apply(base).
		Select(`
			contacts.id,
			contacts.profile_name,
			contacts.phone_number,
			contacts.assigned_user_id,
			contacts.last_message_at
		`).
		Order("COALESCE(contacts.last_message_at, contacts.created_at) DESC, contacts.id").
		Scan(&contacts).Error; err != nil {
		a.Log.Error("Failed to list CRM segment contacts", "key", key, "error", err)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to load CRM segment",
			nil,
			"",
		)
	}
	for i := range contacts {
		contacts[i].ProfileName, contacts[i].PhoneNumber = a.MaskContactFields(
			orgID,
			contacts[i].ProfileName,
			contacts[i].PhoneNumber,
		)
	}

	return r.SendEnvelope(map[string]any{
		"segment": CRMSystemSegment{
			Key:         spec.Key,
			Label:       spec.Label,
			Description: spec.Description,
			Count:       total,
		},
		"contacts": contacts,
		"total":    total,
		"page":     pagination.Page,
		"limit":    pagination.Limit,
	})
}

func crmSegmentPathKey(r *fastglue.Request) string {
	value := r.RequestCtx.UserValue("key")
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []byte:
		return strings.TrimSpace(string(typed))
	default:
		return ""
	}
}

func crmSegmentPermissionAllowed(
	spec crmSystemSegmentSpec,
	permissions crmInsightsPermissions,
) bool {
	if len(spec.RequiredResources) == 0 {
		return false
	}
	for _, resource := range spec.RequiredResources {
		var allowed bool
		switch resource {
		case models.ResourceContacts:
			allowed = permissions.contacts
		case models.ResourceTasks:
			allowed = permissions.tasks
		case models.ResourceBookings:
			allowed = permissions.bookings
		case models.ResourcePackages:
			allowed = permissions.packages
		case models.ResourcePayments:
			allowed = permissions.payments
		case models.ResourceCRMLeads:
			allowed = permissions.leads
		}
		if !allowed {
			return false
		}
	}
	return true
}

func (a *App) countCRMSystemSegment(
	orgID uuid.UUID,
	spec crmSystemSegmentSpec,
	now time.Time,
) (int64, error) {
	where, args := spec.Where(now)
	var count int64
	err := a.DB.Model(&models.Contact{}).
		Where("contacts.organization_id = ?", orgID).
		Where(where, args...).
		Count(&count).Error
	return count, err
}
