package handlers

import (
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

var (
	resellerDomainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}\.)+[a-z]{2,63}$`)
	resellerColorPattern  = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

const platformResellerSlug = "platform-direct"

type resellerResponse struct {
	models.Reseller
	OrganizationCount int64 `json:"organization_count"`
	MemberCount       int64 `json:"member_count"`
}

type createResellerRequest struct {
	Name             string `json:"name"`
	BrandName        string `json:"brand_name"`
	Plan             string `json:"plan"`
	MaxOrganizations int    `json:"max_organizations"`
	WorkspaceName    string `json:"workspace_name"`
	SupportEmail     string `json:"support_email"`
}

type updateResellerRequest struct {
	Name             *string `json:"name"`
	BrandName        *string `json:"brand_name"`
	LogoURL          *string `json:"logo_url"`
	PrimaryColor     *string `json:"primary_color"`
	AccentColor      *string `json:"accent_color"`
	SupportEmail     *string `json:"support_email"`
	CustomDomain     *string `json:"custom_domain"`
	Status           *string `json:"status"`
	Plan             *string `json:"plan"`
	MaxOrganizations *int    `json:"max_organizations"`
}

type resellerMemberResponse struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"user_id"`
	Email     string    `json:"email"`
	FullName  string    `json:"full_name"`
	Role      string    `json:"role"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

type addResellerMemberRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type resellerUsageResponse struct {
	ResellerID        uuid.UUID              `json:"reseller_id"`
	Plan              string                 `json:"plan"`
	MaxOrganizations  int                    `json:"max_organizations"`
	Organizations     []OrganizationResponse `json:"organizations"`
	OrganizationCount int64                  `json:"organization_count"`
	UserCount         int64                  `json:"user_count"`
	WhatsAppAccounts  int64                  `json:"whatsapp_accounts"`
	Contacts          int64                  `json:"contacts"`
	Messages          int64                  `json:"messages"`
}

func resellerRoleCanManage(role string) bool {
	return role == models.ResellerRoleOwner || role == models.ResellerRoleAdmin
}

func validResellerPlan(plan string) bool {
	switch plan {
	case models.ResellerPlanStarter, models.ResellerPlanGrowth, models.ResellerPlanEnterprise:
		return true
	default:
		return false
	}
}

func defaultResellerLimit(plan string) int {
	switch plan {
	case models.ResellerPlanGrowth:
		return 50
	case models.ResellerPlanEnterprise:
		return 1000
	default:
		return 10
	}
}

func (a *App) canManageReseller(userID, resellerID uuid.UUID) bool {
	if userID == uuid.Nil || resellerID == uuid.Nil {
		return false
	}
	if a.IsSuperAdmin(userID) {
		return true
	}

	var count int64
	err := a.DB.Table("reseller_members").
		Joins("JOIN resellers ON resellers.id = reseller_members.reseller_id AND resellers.deleted_at IS NULL").
		Where(`reseller_members.user_id = ? AND reseller_members.reseller_id = ?
			AND reseller_members.is_active = ? AND reseller_members.deleted_at IS NULL
			AND reseller_members.role IN ? AND resellers.status = ?`,
			userID, resellerID, true,
			[]string{models.ResellerRoleOwner, models.ResellerRoleAdmin},
			models.ResellerStatusActive).
		Count(&count).Error
	return err == nil && count > 0
}

func (a *App) resellerIDsForUser(userID uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := a.DB.Table("reseller_members").
		Joins("JOIN resellers ON resellers.id = reseller_members.reseller_id AND resellers.deleted_at IS NULL").
		Where(`reseller_members.user_id = ? AND reseller_members.is_active = ?
			AND reseller_members.deleted_at IS NULL AND reseller_members.role IN ?
			AND resellers.status = ?`,
			userID, true,
			[]string{models.ResellerRoleOwner, models.ResellerRoleAdmin},
			models.ResellerStatusActive).
		Distinct("reseller_members.reseller_id").
		Pluck("reseller_members.reseller_id", &ids).Error
	return ids, err
}

func (a *App) resolveOrganizationReseller(userID uuid.UUID, requested *uuid.UUID) (*models.Reseller, error) {
	var reseller models.Reseller
	if a.IsSuperAdmin(userID) {
		if requested != nil && *requested != uuid.Nil {
			if err := a.DB.Where("id = ? AND status = ?", *requested, models.ResellerStatusActive).First(&reseller).Error; err != nil {
				return nil, err
			}
			return &reseller, nil
		}
		if err := a.DB.Where("slug = ? AND status = ?", platformResellerSlug, models.ResellerStatusActive).First(&reseller).Error; err != nil {
			return nil, err
		}
		return &reseller, nil
	}

	ids, err := a.resellerIDsForUser(userID)
	if err != nil {
		return nil, err
	}
	if requested != nil && *requested != uuid.Nil {
		if !containsUUID(ids, *requested) {
			return nil, gorm.ErrRecordNotFound
		}
		if err := a.DB.Where("id = ? AND status = ?", *requested, models.ResellerStatusActive).First(&reseller).Error; err != nil {
			return nil, err
		}
		return &reseller, nil
	}
	if len(ids) != 1 {
		return nil, errors.New("reseller_id is required")
	}
	if err := a.DB.Where("id = ? AND status = ?", ids[0], models.ResellerStatusActive).First(&reseller).Error; err != nil {
		return nil, err
	}
	return &reseller, nil
}

func containsUUID(ids []uuid.UUID, target uuid.UUID) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func (a *App) uniqueControlPlaneSlug(tx *gorm.DB, base string, table string) string {
	slug := generateSlug(base)
	if slug == "" {
		slug = uuid.NewString()[:8]
	}
	candidate := slug
	for suffix := 2; suffix < 10000; suffix++ {
		var count int64
		if err := tx.Table(table).Where("slug = ?", candidate).Count(&count).Error; err == nil && count == 0 {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", slug, suffix)
	}
	return slug + "-" + uuid.NewString()[:8]
}

func (a *App) createTenantOrganization(
	tx *gorm.DB,
	name string,
	resellerID uuid.UUID,
	creatorID uuid.UUID,
) (*models.Organization, error) {
	org := models.Organization{
		BaseModel:  models.BaseModel{ID: uuid.New()},
		ResellerID: &resellerID,
		Name:       strings.TrimSpace(name),
		Slug:       a.uniqueControlPlaneSlug(tx, name, "organizations"),
		Settings:   models.JSONB{},
	}
	if err := tx.Create(&org).Error; err != nil {
		return nil, fmt.Errorf("create organization: %w", err)
	}
	if a.rlsEnabled() {
		if err := database.SetTenantContext(tx, org.ID); err != nil {
			return nil, fmt.Errorf("bind organization transaction: %w", err)
		}
	}
	if err := database.SeedSystemRolesForOrg(tx, org.ID); err != nil {
		return nil, fmt.Errorf("seed organization roles: %w", err)
	}

	chatbotSettings := models.ChatbotSettings{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     org.ID,
		IsEnabled:          false,
		SessionTimeoutMins: 30,
	}
	if err := tx.Create(&chatbotSettings).Error; err != nil {
		return nil, fmt.Errorf("create chatbot settings: %w", err)
	}

	var adminRole models.CustomRole
	if err := tx.Where(
		"organization_id = ? AND name = ? AND is_system = ?",
		org.ID, "admin", true,
	).First(&adminRole).Error; err != nil {
		return nil, fmt.Errorf("find organization admin role: %w", err)
	}

	creatorMembership := models.UserOrganization{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		UserID:         creatorID,
		OrganizationID: org.ID,
		RoleID:         &adminRole.ID,
		IsDefault:      false,
		Source:         models.MembershipSourceDirect,
	}
	if err := tx.Create(&creatorMembership).Error; err != nil {
		return nil, fmt.Errorf("add organization creator: %w", err)
	}

	if err := a.syncResellerOrganizationAdmins(tx, resellerID, org.ID, adminRole.ID); err != nil {
		return nil, err
	}
	if err := database.SeedDefaultWidgetsForOrg(tx, org.ID, creatorID); err != nil {
		return nil, fmt.Errorf("seed organization widgets: %w", err)
	}
	return &org, nil
}

func (a *App) syncResellerOrganizationAdmins(
	tx *gorm.DB,
	resellerID, orgID, adminRoleID uuid.UUID,
) error {
	var members []models.ResellerMember
	if err := tx.Where(
		"reseller_id = ? AND is_active = ? AND role IN ?",
		resellerID, true, []string{models.ResellerRoleOwner, models.ResellerRoleAdmin},
	).Find(&members).Error; err != nil {
		return fmt.Errorf("list reseller administrators: %w", err)
	}
	for i := range members {
		if err := a.ensureResellerOrganizationMembership(tx, &members[i], orgID, adminRoleID); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) ensureResellerOrganizationMembership(
	tx *gorm.DB,
	member *models.ResellerMember,
	orgID, adminRoleID uuid.UUID,
) error {
	var existing models.UserOrganization
	err := tx.Unscoped().
		Where("user_id = ? AND organization_id = ?", member.UserID, orgID).
		First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		existing = models.UserOrganization{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			UserID:           member.UserID,
			OrganizationID:   orgID,
			RoleID:           &adminRoleID,
			IsDefault:        false,
			Source:           models.MembershipSourceReseller,
			ResellerMemberID: &member.ID,
		}
		if err := tx.Create(&existing).Error; err != nil {
			return fmt.Errorf("create reseller organization membership: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("find reseller organization membership: %w", err)
	}

	// Direct organization membership is authoritative and must never be
	// converted into a reseller-derived membership.
	if existing.Source != models.MembershipSourceReseller {
		return nil
	}
	if existing.ResellerMemberID != nil && *existing.ResellerMemberID != member.ID {
		return nil
	}
	if err := tx.Unscoped().Model(&existing).Updates(map[string]any{
		"deleted_at":         nil,
		"role_id":            adminRoleID,
		"source":             models.MembershipSourceReseller,
		"reseller_member_id": member.ID,
	}).Error; err != nil {
		return fmt.Errorf("restore reseller organization membership: %w", err)
	}
	return nil
}

func (a *App) syncResellerMemberOrganizations(tx *gorm.DB, member *models.ResellerMember) error {
	var orgs []models.Organization
	if err := tx.Where("reseller_id = ?", member.ResellerID).Find(&orgs).Error; err != nil {
		return fmt.Errorf("list reseller organizations: %w", err)
	}
	for _, org := range orgs {
		var adminRole models.CustomRole
		if err := tx.Where(
			"organization_id = ? AND name = ? AND is_system = ?",
			org.ID, "admin", true,
		).First(&adminRole).Error; err != nil {
			return fmt.Errorf("find admin role for %s: %w", org.ID, err)
		}
		if err := a.ensureResellerOrganizationMembership(tx, member, org.ID, adminRole.ID); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) resellerSummary(reseller models.Reseller) resellerResponse {
	var organizationCount, memberCount int64
	a.DB.Model(&models.Organization{}).Where("reseller_id = ?", reseller.ID).Count(&organizationCount)
	a.DB.Model(&models.ResellerMember{}).Where(
		"reseller_id = ? AND is_active = ?", reseller.ID, true,
	).Count(&memberCount)
	return resellerResponse{
		Reseller:          reseller,
		OrganizationCount: organizationCount,
		MemberCount:       memberCount,
	}
}

// ListResellers returns every reseller to platform owners and only the
// caller's active portfolios to reseller administrators.
func (a *App) ListResellers(r *fastglue.Request) error {
	userID, ok := r.RequestCtx.UserValue("user_id").(uuid.UUID)
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	query := a.DB.Model(&models.Reseller{})
	if !a.IsSuperAdmin(userID) {
		ids, err := a.resellerIDsForUser(userID)
		if err != nil || len(ids) == 0 {
			return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Reseller access required", nil, "")
		}
		query = query.Where("id IN ?", ids)
	}

	var resellers []models.Reseller
	if err := query.Order("name ASC").Find(&resellers).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list resellers", nil, "")
	}
	response := make([]resellerResponse, 0, len(resellers))
	for _, reseller := range resellers {
		response = append(response, a.resellerSummary(reseller))
	}
	return r.SendEnvelope(map[string]any{"resellers": response})
}

// CreateReseller creates a reseller and its private workspace organization.
func (a *App) CreateReseller(r *fastglue.Request) error {
	userID, ok := r.RequestCtx.UserValue("user_id").(uuid.UUID)
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.IsSuperAdmin(userID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Platform owner access required", nil, "")
	}

	var req createResellerRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	req.Name = strings.TrimSpace(req.Name)
	req.BrandName = strings.TrimSpace(req.BrandName)
	if req.Name == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Reseller name is required", nil, "")
	}
	if req.BrandName == "" {
		req.BrandName = req.Name
	}
	if req.Plan == "" {
		req.Plan = models.ResellerPlanStarter
	}
	if !validResellerPlan(req.Plan) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid reseller plan", nil, "")
	}
	if req.MaxOrganizations <= 0 {
		req.MaxOrganizations = defaultResellerLimit(req.Plan)
	}
	if req.WorkspaceName == "" {
		req.WorkspaceName = req.BrandName + " Workspace"
	}
	if req.SupportEmail != "" {
		if _, err := mail.ParseAddress(req.SupportEmail); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid support email", nil, "")
		}
	}

	tx := a.DB.Begin()
	if tx.Error != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create reseller", nil, "")
	}
	reseller := models.Reseller{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		Name:             req.Name,
		Slug:             a.uniqueControlPlaneSlug(tx, req.Name, "resellers"),
		Status:           models.ResellerStatusActive,
		Plan:             req.Plan,
		MaxOrganizations: req.MaxOrganizations,
		BrandName:        req.BrandName,
		PrimaryColor:     "#0f766e",
		AccentColor:      "#f59e0b",
		SupportEmail:     strings.ToLower(strings.TrimSpace(req.SupportEmail)),
		Settings:         models.JSONB{},
		CreatedByID:      &userID,
	}
	if err := tx.Create(&reseller).Error; err != nil {
		tx.Rollback()
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "A reseller with this name already exists", nil, "")
	}
	workspace, err := a.createTenantOrganization(tx, req.WorkspaceName, reseller.ID, userID)
	if err != nil {
		tx.Rollback()
		a.Log.Error("Failed to create reseller workspace", "error", err, "reseller_id", reseller.ID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create reseller workspace", nil, "")
	}
	if err := tx.Commit().Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create reseller", nil, "")
	}

	response := a.resellerSummary(reseller)
	return r.SendEnvelope(map[string]any{
		"reseller": response,
		"workspace": OrganizationResponse{
			ID:         workspace.ID,
			ResellerID: workspace.ResellerID,
			Name:       workspace.Name,
			Slug:       workspace.Slug,
			CreatedAt:  workspace.CreatedAt.Format(time.RFC3339),
		},
	})
}

func (a *App) GetReseller(r *fastglue.Request) error {
	userID, ok := r.RequestCtx.UserValue("user_id").(uuid.UUID)
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	resellerID, err := parsePathUUID(r, "id", "reseller")
	if err != nil {
		return nil
	}
	if !a.canManageReseller(userID, resellerID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Reseller access denied", nil, "")
	}
	var reseller models.Reseller
	if err := a.DB.Where("id = ?", resellerID).First(&reseller).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Reseller not found", nil, "")
	}
	return r.SendEnvelope(a.resellerSummary(reseller))
}

func (a *App) UpdateReseller(r *fastglue.Request) error {
	userID, ok := r.RequestCtx.UserValue("user_id").(uuid.UUID)
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	resellerID, err := parsePathUUID(r, "id", "reseller")
	if err != nil {
		return nil
	}
	if !a.canManageReseller(userID, resellerID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Reseller access denied", nil, "")
	}

	var req updateResellerRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	updates := map[string]any{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Reseller name cannot be empty", nil, "")
		}
		updates["name"] = name
	}
	if req.BrandName != nil {
		updates["brand_name"] = strings.TrimSpace(*req.BrandName)
	}
	if req.LogoURL != nil {
		updates["logo_url"] = strings.TrimSpace(*req.LogoURL)
	}
	for field, value := range map[string]*string{
		"primary_color": req.PrimaryColor,
		"accent_color":  req.AccentColor,
	} {
		if value != nil {
			color := strings.TrimSpace(*value)
			if !resellerColorPattern.MatchString(color) {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Brand colors must use #RRGGBB format", nil, "")
			}
			updates[field] = strings.ToLower(color)
		}
	}
	if req.SupportEmail != nil {
		email := strings.ToLower(strings.TrimSpace(*req.SupportEmail))
		if email != "" {
			if _, err := mail.ParseAddress(email); err != nil {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid support email", nil, "")
			}
		}
		updates["support_email"] = email
	}
	if req.CustomDomain != nil {
		domain := strings.ToLower(strings.TrimSpace(*req.CustomDomain))
		if domain != "" && !resellerDomainPattern.MatchString(domain) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Custom domain must be a hostname without a protocol or path", nil, "")
		}
		if domain != "" {
			var count int64
			a.DB.Model(&models.Reseller{}).
				Where("custom_domain = ? AND id <> ?", domain, resellerID).
				Count(&count)
			if count > 0 {
				return r.SendErrorEnvelope(fasthttp.StatusConflict, "Custom domain is already assigned", nil, "")
			}
		}
		updates["custom_domain"] = domain
	}
	if req.Status != nil || req.Plan != nil || req.MaxOrganizations != nil {
		if !a.IsSuperAdmin(userID) {
			return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Only a platform owner can change reseller status or plan", nil, "")
		}
	}
	if req.Status != nil {
		if *req.Status != models.ResellerStatusActive && *req.Status != models.ResellerStatusSuspended {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid reseller status", nil, "")
		}
		updates["status"] = *req.Status
	}
	if req.Plan != nil {
		if !validResellerPlan(*req.Plan) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid reseller plan", nil, "")
		}
		updates["plan"] = *req.Plan
	}
	if req.MaxOrganizations != nil {
		if *req.MaxOrganizations < 1 {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Organization limit must be at least 1", nil, "")
		}
		updates["max_organizations"] = *req.MaxOrganizations
	}
	if len(updates) == 0 {
		return r.SendEnvelope(map[string]string{"message": "No changes"})
	}
	if err := a.DB.Model(&models.Reseller{}).Where("id = ?", resellerID).Updates(updates).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update reseller", nil, "")
	}
	var reseller models.Reseller
	if err := a.DB.Where("id = ?", resellerID).First(&reseller).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Reseller not found", nil, "")
	}
	return r.SendEnvelope(a.resellerSummary(reseller))
}

func (a *App) ListResellerMembers(r *fastglue.Request) error {
	userID, ok := r.RequestCtx.UserValue("user_id").(uuid.UUID)
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	resellerID, err := parsePathUUID(r, "id", "reseller")
	if err != nil {
		return nil
	}
	if !a.canManageReseller(userID, resellerID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Reseller access denied", nil, "")
	}
	var members []resellerMemberResponse
	if err := a.DB.Table("reseller_members").
		Select(`reseller_members.id, reseller_members.user_id, users.email,
			users.full_name, reseller_members.role, reseller_members.is_active,
			reseller_members.created_at`).
		Joins("JOIN users ON users.id = reseller_members.user_id AND users.deleted_at IS NULL").
		Where("reseller_members.reseller_id = ? AND reseller_members.deleted_at IS NULL", resellerID).
		Order("reseller_members.created_at ASC").
		Scan(&members).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list reseller members", nil, "")
	}
	return r.SendEnvelope(map[string]any{"members": members})
}

func (a *App) AddResellerMember(r *fastglue.Request) error {
	currentUserID, ok := r.RequestCtx.UserValue("user_id").(uuid.UUID)
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	resellerID, err := parsePathUUID(r, "id", "reseller")
	if err != nil {
		return nil
	}
	if !a.canManageReseller(currentUserID, resellerID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Reseller access denied", nil, "")
	}
	var req addResellerMemberRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if _, err := mail.ParseAddress(req.Email); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "A valid existing user email is required", nil, "")
	}
	if req.Role == "" {
		req.Role = models.ResellerRoleAdmin
	}
	if !resellerRoleCanManage(req.Role) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid reseller role", nil, "")
	}

	var user models.User
	if err := a.DB.Where("LOWER(email) = ? AND is_active = ?", req.Email, true).First(&user).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Create the user in the reseller workspace before assigning reseller access", nil, "")
	}

	tx := a.DB.Begin()
	if tx.Error != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to add reseller member", nil, "")
	}
	var member models.ResellerMember
	err = tx.Unscoped().Where("reseller_id = ? AND user_id = ?", resellerID, user.ID).First(&member).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		member = models.ResellerMember{
			BaseModel:  models.BaseModel{ID: uuid.New()},
			ResellerID: resellerID,
			UserID:     user.ID,
			Role:       req.Role,
			IsActive:   true,
		}
		if err := tx.Create(&member).Error; err != nil {
			tx.Rollback()
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to add reseller member", nil, "")
		}
	} else if err != nil {
		tx.Rollback()
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to add reseller member", nil, "")
	} else if err := tx.Unscoped().Model(&member).Updates(map[string]any{
		"deleted_at": nil,
		"role":       req.Role,
		"is_active":  true,
	}).Error; err != nil {
		tx.Rollback()
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to restore reseller member", nil, "")
	}
	member.Role = req.Role
	member.IsActive = true
	if err := a.syncResellerMemberOrganizations(tx, &member); err != nil {
		tx.Rollback()
		a.Log.Error("Failed to synchronize reseller access", "error", err, "reseller_id", resellerID, "user_id", user.ID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to synchronize reseller organization access", nil, "")
	}
	if err := tx.Commit().Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to add reseller member", nil, "")
	}
	a.InvalidateUserPermissionsCache(user.ID)
	return r.SendEnvelope(map[string]any{
		"member": resellerMemberResponse{
			ID:        member.ID,
			UserID:    user.ID,
			Email:     user.Email,
			FullName:  user.FullName,
			Role:      member.Role,
			IsActive:  true,
			CreatedAt: member.CreatedAt,
		},
	})
}

func (a *App) RemoveResellerMember(r *fastglue.Request) error {
	currentUserID, ok := r.RequestCtx.UserValue("user_id").(uuid.UUID)
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	resellerID, err := parsePathUUID(r, "id", "reseller")
	if err != nil {
		return nil
	}
	memberID, err := parsePathUUID(r, "member_id", "reseller member")
	if err != nil {
		return nil
	}
	if !a.canManageReseller(currentUserID, resellerID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Reseller access denied", nil, "")
	}
	var member models.ResellerMember
	if err := a.DB.Where("id = ? AND reseller_id = ?", memberID, resellerID).First(&member).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Reseller member not found", nil, "")
	}
	if member.UserID == currentUserID && !a.IsSuperAdmin(currentUserID) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "You cannot remove your own reseller access", nil, "")
	}
	if member.Role == models.ResellerRoleOwner {
		var ownerCount int64
		a.DB.Model(&models.ResellerMember{}).Where(
			"reseller_id = ? AND role = ? AND is_active = ?",
			resellerID, models.ResellerRoleOwner, true,
		).Count(&ownerCount)
		if ownerCount <= 1 {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "A reseller must retain at least one owner", nil, "")
		}
	}

	tx := a.DB.Begin()
	if tx.Error != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to remove reseller member", nil, "")
	}
	if err := tx.Where(
		"reseller_member_id = ? AND source = ?",
		member.ID, models.MembershipSourceReseller,
	).Delete(&models.UserOrganization{}).Error; err != nil {
		tx.Rollback()
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to revoke reseller organization access", nil, "")
	}
	if err := tx.Model(&member).Update("is_active", false).Error; err != nil {
		tx.Rollback()
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to remove reseller member", nil, "")
	}
	if err := tx.Delete(&member).Error; err != nil {
		tx.Rollback()
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to remove reseller member", nil, "")
	}
	if err := tx.Commit().Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to remove reseller member", nil, "")
	}
	a.InvalidateUserPermissionsCache(member.UserID)
	return r.SendEnvelope(map[string]string{"message": "Reseller access revoked"})
}

func (a *App) GetResellerUsage(r *fastglue.Request) error {
	userID, ok := r.RequestCtx.UserValue("user_id").(uuid.UUID)
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	resellerID, err := parsePathUUID(r, "id", "reseller")
	if err != nil {
		return nil
	}
	if !a.canManageReseller(userID, resellerID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Reseller access denied", nil, "")
	}
	var reseller models.Reseller
	if err := a.DB.Where("id = ?", resellerID).First(&reseller).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Reseller not found", nil, "")
	}
	var orgs []models.Organization
	if err := a.DB.Where("reseller_id = ?", resellerID).Order("name ASC").Find(&orgs).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load reseller organizations", nil, "")
	}

	response := resellerUsageResponse{
		ResellerID:        reseller.ID,
		Plan:              reseller.Plan,
		MaxOrganizations:  reseller.MaxOrganizations,
		Organizations:     make([]OrganizationResponse, 0, len(orgs)),
		OrganizationCount: int64(len(orgs)),
	}
	orgIDs := make([]uuid.UUID, 0, len(orgs))
	for _, org := range orgs {
		orgIDs = append(orgIDs, org.ID)
		response.Organizations = append(response.Organizations, OrganizationResponse{
			ID:         org.ID,
			ResellerID: org.ResellerID,
			Name:       org.Name,
			Slug:       org.Slug,
			CreatedAt:  org.CreatedAt.Format(time.RFC3339),
		})
		err := database.WithTenant(a.DB, org.ID, func(tx *gorm.DB) error {
			var whatsappAccounts, contacts, messages int64
			if err := tx.Model(&models.WhatsAppAccount{}).Count(&whatsappAccounts).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.Contact{}).Count(&contacts).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.Message{}).Count(&messages).Error; err != nil {
				return err
			}
			response.WhatsAppAccounts += whatsappAccounts
			response.Contacts += contacts
			response.Messages += messages
			return nil
		})
		if err != nil {
			a.Log.Error("Failed to aggregate reseller usage", "error", err, "reseller_id", resellerID, "org_id", org.ID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to calculate reseller usage", nil, "")
		}
	}
	if len(orgIDs) > 0 {
		if err := a.DB.Table("user_organizations").
			Where("organization_id IN ? AND deleted_at IS NULL", orgIDs).
			Distinct("user_id").
			Count(&response.UserCount).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to calculate reseller users", nil, "")
		}
	}
	return r.SendEnvelope(response)
}
