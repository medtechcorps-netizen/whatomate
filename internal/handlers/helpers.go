package handlers

import (
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"

	"github.com/shridarpatil/whatomate/internal/access"
	"github.com/shridarpatil/whatomate/internal/audit"
	"github.com/shridarpatil/whatomate/internal/models"
)

// errEnvelopeSent is a sentinel returned by helpers after they have already
// written an error envelope to the response. Callers should return nil to the framework.
var errEnvelopeSent = errors.New("error envelope sent")

// parsePathUUID extracts a UUID from a path parameter. On failure, it sends a
// 400 error envelope and returns uuid.Nil plus an error.
func parsePathUUID(r *fastglue.Request, param, label string) (uuid.UUID, error) {
	idStr, _ := r.RequestCtx.UserValue(param).(string)
	id, err := uuid.Parse(idStr)
	if err != nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid "+label+" ID", nil, "")
		return uuid.Nil, errEnvelopeSent
	}
	return id, nil
}

// Pagination holds parsed pagination parameters.
type Pagination struct {
	Page   int
	Limit  int
	Offset int
}

// Apply adds Offset and Limit to a GORM query.
func (pg Pagination) Apply(query *gorm.DB) *gorm.DB {
	return query.Offset(pg.Offset).Limit(pg.Limit)
}

// parsePagination extracts page-based pagination from query params with
// default limit=50 and max limit=100.
func parsePagination(r *fastglue.Request) Pagination {
	return parsePaginationWithDefaults(r, 50, 100)
}

// parsePaginationWithDefaults extracts page-based pagination with custom defaults.
func parsePaginationWithDefaults(r *fastglue.Request, defaultLimit, maxLimit int) Pagination {
	page, _ := strconv.Atoi(string(r.RequestCtx.QueryArgs().Peek("page")))
	limit, _ := strconv.Atoi(string(r.RequestCtx.QueryArgs().Peek("limit")))

	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > maxLimit {
		limit = defaultLimit
	}
	return Pagination{
		Page:   page,
		Limit:  limit,
		Offset: (page - 1) * limit,
	}
}

// parseDateParam parses a YYYY-MM-DD date from the named query parameter.
// Returns the parsed time and true on success, or zero time and false if the
// parameter is missing or malformed.
func parseDateParam(r *fastglue.Request, param string) (time.Time, bool) {
	s := string(r.RequestCtx.QueryArgs().Peek(param))
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// endOfDay returns the last nanosecond of the given day.
func endOfDay(t time.Time) time.Time {
	return t.Add(24*time.Hour - time.Nanosecond)
}

// findByIDAndOrg fetches a single record scoped by ID and organization.
// Sends a 404 error envelope on failure and returns the error.
func findByIDAndOrg[T any](db *gorm.DB, r *fastglue.Request, id, orgID uuid.UUID, label string) (*T, error) {
	var model T
	if err := db.Where("id = ? AND organization_id = ?", id, orgID).First(&model).Error; err != nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusNotFound, label+" not found", nil, "")
		return nil, errEnvelopeSent
	}
	return &model, nil
}

// orgMemberUserCondition restricts a users query to accounts that belong to an
// organization, either through the legacy home column (users.organization_id)
// or through a live user_organizations row. It takes the organization ID twice.
// EXISTS is used instead of a JOIN so a user never appears twice.
const orgMemberUserCondition = `(users.organization_id = ? OR EXISTS (
	SELECT 1 FROM user_organizations uo
	WHERE uo.user_id = users.id AND uo.organization_id = ? AND uo.deleted_at IS NULL))`

// loadOrgMembershipRows returns every user_organizations row for orgID and
// the given users, soft-deleted rows included, grouped by user and ordered by
// id. Soft-deleted rows matter: they tell a removed member apart from a legacy
// account that never had a membership row.
func loadOrgMembershipRows(db *gorm.DB, userIDs []uuid.UUID, orgID uuid.UUID) (map[uuid.UUID][]models.UserOrganization, error) {
	byUser := make(map[uuid.UUID][]models.UserOrganization, len(userIDs))
	if len(userIDs) == 0 {
		return byUser, nil
	}
	var rows []models.UserOrganization
	if err := db.Unscoped().
		Where("organization_id = ? AND user_id IN ?", orgID, userIDs).
		Order("id").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		byUser[row.UserID] = append(byUser[row.UserID], row)
	}
	return byUser, nil
}

// orgMembershipRowsValid decides membership from a user's user_organizations
// rows for orgID (as loaded by loadOrgMembershipRows). It mirrors the auth
// layer (middleware setAuthenticatedContext): a live row must exist and, when
// reseller-derived, its reseller assignment must still be active. Only a user
// with no row at all, live or soft-deleted, falls back to the legacy home
// organization (users.organization_id), as findByIDAndOrg did before. A user
// whose membership was removed (row soft-deleted) is rejected even when
// orgID is their home organization. Super admins get no bypass.
func orgMembershipRowsValid(db *gorm.DB, user *models.User, orgID uuid.UUID, rows []models.UserOrganization) bool {
	if user == nil || orgID == uuid.Nil {
		return false
	}
	if len(rows) == 0 {
		return user.OrganizationID == orgID
	}
	for i := range rows {
		if rows[i].DeletedAt.Valid {
			continue
		}
		// The live row is unique (idx_user_org_unique ... WHERE deleted_at IS NULL).
		return access.ResellerDerivedMembershipActive(db, &rows[i])
	}
	return false
}

// orgMemberMembershipValid reports whether user is a valid member of orgID
// under orgMembershipRowsValid. It fails closed: a nil database or a failed
// membership query rejects the user.
func orgMemberMembershipValid(db *gorm.DB, user *models.User, orgID uuid.UUID) bool {
	if db == nil || user == nil || user.ID == uuid.Nil || orgID == uuid.Nil {
		return false
	}
	byUser, err := loadOrgMembershipRows(db, []uuid.UUID{user.ID}, orgID)
	if err != nil {
		return false
	}
	return orgMembershipRowsValid(db, user, orgID, byUser[user.ID])
}

// orgMemberUserQuery scopes a users query to one user who belongs to orgID.
// GORM adds users.deleted_at IS NULL for models.User automatically.
func orgMemberUserQuery(db *gorm.DB, userID, orgID uuid.UUID, activeOnly bool) *gorm.DB {
	query := db.Where("users.id = ?", userID).Where(orgMemberUserCondition, orgID, orgID)
	if activeOnly {
		query = query.Where("users.is_active = ?", true)
	}
	return query
}

// lookupOrgMemberUser returns a non-deleted user who is a valid member of
// orgID (home organization or user_organizations membership). With
// activeOnly, deactivated users are rejected as well. It writes no response.
func lookupOrgMemberUser(db *gorm.DB, userID, orgID uuid.UUID, activeOnly bool) (*models.User, bool) {
	if db == nil || userID == uuid.Nil || orgID == uuid.Nil {
		return nil, false
	}
	var user models.User
	if err := orgMemberUserQuery(db, userID, orgID, activeOnly).First(&user).Error; err != nil {
		return nil, false
	}
	if !orgMemberMembershipValid(db, &user, orgID) {
		return nil, false
	}
	return &user, true
}

// findOrgMemberUser is the membership-aware counterpart of
// findByIDAndOrg[models.User]. users.organization_id is only a user's home
// organization; members working in another organization are recorded in
// user_organizations, so a home-column lookup wrongly 404s them. On failure it
// sends the same 404 "<label> not found" envelope and returns errEnvelopeSent.
func findOrgMemberUser(db *gorm.DB, r *fastglue.Request, userID, orgID uuid.UUID, label string, activeOnly bool) (*models.User, error) {
	user, ok := lookupOrgMemberUser(db, userID, orgID, activeOnly)
	if !ok {
		_ = r.SendErrorEnvelope(fasthttp.StatusNotFound, label+" not found", nil, "")
		return nil, errEnvelopeSent
	}
	return user, nil
}

// logAudit records an audit-log entry for a resource mutation, resolving the
// actor's display name automatically. It wraps audit.LogAudit to remove the
// repeated a.DB + GetUserName boilerplate at call sites.
func (a *App) logAudit(orgID, userID uuid.UUID, resourceType string, resourceID uuid.UUID, action models.AuditAction, oldData, newData any, extraChanges ...map[string]any) {
	if err := audit.LogAudit(a.DB, orgID, userID, audit.GetUserName(a.DB, userID), resourceType, resourceID, action, oldData, newData, extraChanges...); err != nil {
		a.Log.Error("Failed to create audit log", "error", err, "resource_type", resourceType, "resource_id", resourceID)
	}
}

// listEnvelope builds the standard paginated list response payload used across
// list handlers: {<key>: items, total, page, limit}.
func listEnvelope(key string, items, total any, pg Pagination) map[string]any {
	return map[string]any{
		key:     items,
		"total": total,
		"page":  pg.Page,
		"limit": pg.Limit,
	}
}

// parseDateRange parses start and end date strings in YYYY-MM-DD format.
// Applies end-of-day to the end date. Returns an error message suitable for
// display if parsing fails.
func parseDateRange(startStr, endStr string) (start, end time.Time, errMsg string) {
	var err error
	start, err = time.Parse("2006-01-02", startStr)
	if err != nil {
		return time.Time{}, time.Time{}, "Invalid start date format. Use YYYY-MM-DD"
	}
	end, err = time.Parse("2006-01-02", endStr)
	if err != nil {
		return time.Time{}, time.Time{}, "Invalid end date format. Use YYYY-MM-DD"
	}
	end = endOfDay(end)
	return start, end, ""
}
