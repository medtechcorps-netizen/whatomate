package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	googleSearchConsoleReadOnlyScope                = "https://www.googleapis.com/auth/webmasters.readonly"
	googleSearchConsoleRefreshTokenCredential       = "refresh_token"
	googleSearchConsoleOAuthStatePrefix             = "integration:gsc:oauth:state:"
	googleSearchConsoleOAuthStateTTL                = 10 * time.Minute
	googleSearchConsoleHTTPTimeout                  = 30 * time.Second
	googleSearchConsoleMaxResponseBytes       int64 = 2 * 1024 * 1024
	googleSearchConsoleMaxProperties                = 1000
	googleSearchConsoleDefaultTopLimit              = 10
	googleSearchConsoleMaxTopLimit                  = 100
)

var (
	errGoogleSearchConsoleNotConnected = errors.New("google search console is not connected")
	errGoogleSearchConsoleStale        = errors.New("google search console settings changed")
	errGoogleSearchConsoleForbidden    = errors.New("google search console authorization is no longer permitted")
)

type googleSearchConsoleOAuthState struct {
	OrganizationID string    `json:"organization_id"`
	UserID         string    `json:"user_id"`
	Nonce          string    `json:"nonce"`
	CodeVerifier   string    `json:"code_verifier"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type googleSearchConsoleSite struct {
	SiteURL         string `json:"siteUrl"`
	PermissionLevel string `json:"permissionLevel"`
	PropertyType    string `json:"-"`
}

type googleSearchConsolePropertyResponse struct {
	ID              uuid.UUID  `json:"id"`
	SiteURL         string     `json:"site_url"`
	DisplayName     string     `json:"display_name"`
	PropertyType    string     `json:"property_type"`
	PermissionLevel string     `json:"permission_level"`
	Selected        bool       `json:"selected"`
	LastSyncedAt    *time.Time `json:"last_synced_at,omitempty"`
}

type googleSearchConsolePropertiesResponse struct {
	Properties    []googleSearchConsolePropertyResponse `json:"properties"`
	SelectedCount int                                   `json:"selected_count"`
}

type googleSearchVisibilitySetupResponse struct {
	Status     string                                `json:"status"`
	Message    string                                `json:"message,omitempty"`
	LastError  string                                `json:"last_error,omitempty"`
	Properties []googleSearchConsolePropertyResponse `json:"properties"`
}

type googleSearchConsoleValidationSnapshot struct {
	OrganizationID  uuid.UUID
	UserID          uuid.UUID
	ValidationToken string
	RefreshToken    string
}

type googleSearchMetric struct {
	Clicks      float64 `json:"clicks"`
	Impressions float64 `json:"impressions"`
	CTR         float64 `json:"ctr"`
	Position    float64 `json:"position"`
}

type googleSearchTrendPoint struct {
	Date string `json:"date"`
	googleSearchMetric
}

type googleSearchQueryMetric struct {
	Query string `json:"query"`
	googleSearchMetric
}

type googleSearchPageMetric struct {
	Page string `json:"page"`
	googleSearchMetric
}

type googleSearchVisibilityProperty struct {
	ID           uuid.UUID `json:"id"`
	SiteURL      string    `json:"site_url"`
	DisplayName  string    `json:"display_name"`
	PropertyType string    `json:"property_type"`
}

type googleSearchVisibilityResponse struct {
	Property   googleSearchVisibilityProperty `json:"property"`
	StartDate  string                         `json:"start_date"`
	EndDate    string                         `json:"end_date"`
	SearchType string                         `json:"search_type"`
	Page       string                         `json:"page,omitempty"`
	DataState  string                         `json:"data_state"`
	Summary    googleSearchMetric             `json:"summary"`
	Trend      []googleSearchTrendPoint       `json:"trend"`
	TopQueries []googleSearchQueryMetric      `json:"top_queries"`
	TopPages   []googleSearchPageMetric       `json:"top_pages"`
}

type googleSearchAnalyticsRequest struct {
	StartDate             string                             `json:"startDate"`
	EndDate               string                             `json:"endDate"`
	Dimensions            []string                           `json:"dimensions,omitempty"`
	Type                  string                             `json:"type"`
	DataState             string                             `json:"dataState,omitempty"`
	DimensionFilterGroups []googleSearchDimensionFilterGroup `json:"dimensionFilterGroups,omitempty"`
	RowLimit              int                                `json:"rowLimit,omitempty"`
	StartRow              int                                `json:"startRow,omitempty"`
}

type googleSearchDimensionFilterGroup struct {
	GroupType string                        `json:"groupType,omitempty"`
	Filters   []googleSearchDimensionFilter `json:"filters"`
}

type googleSearchDimensionFilter struct {
	Dimension  string `json:"dimension"`
	Operator   string `json:"operator"`
	Expression string `json:"expression"`
}

type googleSearchAnalyticsRow struct {
	Keys        []string `json:"keys"`
	Clicks      float64  `json:"clicks"`
	Impressions float64  `json:"impressions"`
	CTR         float64  `json:"ctr"`
	Position    float64  `json:"position"`
}

type googleSearchAnalyticsResponse struct {
	Rows                    []googleSearchAnalyticsRow `json:"rows"`
	ResponseAggregationType string                     `json:"responseAggregationType"`
}

type googleSearchConsoleAPIError struct {
	StatusCode int
}

func (e *googleSearchConsoleAPIError) Error() string {
	return fmt.Sprintf("google search console returned HTTP %d", e.StatusCode)
}

func (a *App) googleSearchConsoleOAuthConfig() (*oauth2.Config, error) {
	if a == nil || a.Config == nil {
		return nil, errors.New("google search console configuration is unavailable")
	}
	cfg := a.Config.GoogleSearchConsole
	clientID := strings.TrimSpace(cfg.ClientID)
	clientSecret := strings.TrimSpace(cfg.ClientSecret)
	redirectURL := strings.TrimSpace(cfg.RedirectURL)
	if clientID == "" || clientSecret == "" || redirectURL == "" {
		return nil, errors.New("google search console OAuth credentials are incomplete")
	}
	if len(clientID) > 1024 || len(clientSecret) > 8192 || len(redirectURL) > 2048 || validateIntegrationRedirectURI(redirectURL) != nil {
		return nil, errors.New("google search console OAuth configuration is invalid")
	}
	authURL, err := validatedGoogleEndpoint(cfg.AuthURL)
	if err != nil {
		return nil, errors.New("google search console auth endpoint is invalid")
	}
	tokenURL, err := validatedGoogleEndpoint(cfg.TokenURL)
	if err != nil {
		return nil, errors.New("google search console token endpoint is invalid")
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{googleSearchConsoleReadOnlyScope},
		Endpoint: oauth2.Endpoint{
			AuthURL:  authURL,
			TokenURL: tokenURL,
		},
	}, nil
}

func validatedGoogleEndpoint(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("invalid endpoint")
	}
	if parsed.Scheme != "https" {
		hostname := strings.ToLower(parsed.Hostname())
		if parsed.Scheme != "http" || (hostname != "localhost" && hostname != "127.0.0.1" && hostname != "::1") {
			return "", errors.New("endpoint must use HTTPS")
		}
	}
	return strings.TrimRight(value, "/"), nil
}

func (a *App) googleSearchConsoleAPIBaseURL() (string, error) {
	if a == nil || a.Config == nil {
		return "", errors.New("google search console configuration is unavailable")
	}
	return validatedGoogleEndpoint(a.Config.GoogleSearchConsole.APIBaseURL)
}

func (a *App) googleSearchConsolePlatformConfigured() bool {
	if _, err := a.googleSearchConsoleOAuthConfig(); err != nil {
		return false
	}
	_, err := a.googleSearchConsoleAPIBaseURL()
	return err == nil
}

func (a *App) googleSearchConsoleOAuthAvailable() bool {
	return a.googleSearchConsolePlatformConfigured() && a.hasIntegrationEncryptionKey() && a.Redis != nil
}

// startGoogleSearchConsoleOAuth creates a one-time, tenant/user-bound state and
// returns an authorization URL. It intentionally works before a tenant has a
// configured or enabled integration row.
func (a *App) startGoogleSearchConsoleOAuth(r *fastglue.Request, orgID, userID uuid.UUID) error {
	if !a.googleSearchConsolePlatformConfigured() {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Google Search Console OAuth is not configured on this server", nil, "")
	}
	if !a.hasIntegrationEncryptionKey() {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Server-side credential encryption is not configured", nil, "")
	}
	if a.Redis == nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "OAuth state storage is unavailable", nil, "")
	}
	oauthConfig, err := a.googleSearchConsoleOAuthConfig()
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Google Search Console OAuth is unavailable", nil, "")
	}

	nonce := generateRandomString(48)
	codeVerifier := oauth2.GenerateVerifier()
	state := googleSearchConsoleOAuthState{
		OrganizationID: orgID.String(),
		UserID:         userID.String(),
		Nonce:          nonce,
		CodeVerifier:   codeVerifier,
		ExpiresAt:      time.Now().UTC().Add(googleSearchConsoleOAuthStateTTL),
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare Google authorization", nil, "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Redis.Set(ctx, googleSearchConsoleOAuthStatePrefix+nonce, payload, googleSearchConsoleOAuthStateTTL).Err(); err != nil {
		a.Log.Error("Failed to store Google Search Console OAuth state", "error", err, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "OAuth state storage is unavailable", nil, "")
	}

	authOptions := []oauth2.AuthCodeOption{
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(codeVerifier),
		oauth2.SetAuthURLParam("prompt", "consent select_account"),
	}
	authorizationURL := oauthConfig.AuthCodeURL(nonce, authOptions...)
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	return r.SendEnvelope(map[string]any{
		"provider":          integrationProviderGoogleSearchConsole,
		"ready":             true,
		"mode":              "oauth",
		"authorization_url": authorizationURL,
	})
}

// CallbackGoogleSearchConsole completes OAuth without normal request auth. The
// Redis state is consumed atomically and re-authorizes the initiating admin at
// callback time before any tenant credential is written.
func (a *App) CallbackGoogleSearchConsole(r *fastglue.Request) error {
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	stateNonce := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("state")))
	if stateNonce == "" || len(stateNonce) > 128 || a.Redis == nil {
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	stateJSON, err := a.Redis.GetDel(ctx, googleSearchConsoleOAuthStatePrefix+stateNonce).Bytes()
	cancel()
	if err != nil {
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}
	var state googleSearchConsoleOAuthState
	if err := json.Unmarshal(stateJSON, &state); err != nil ||
		state.Nonce != stateNonce ||
		len(state.CodeVerifier) < 43 || len(state.CodeVerifier) > 128 ||
		time.Now().UTC().After(state.ExpiresAt) {
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}
	orgID, orgErr := uuid.Parse(state.OrganizationID)
	userID, userErr := uuid.Parse(state.UserID)
	if orgErr != nil || userErr != nil || orgID == uuid.Nil || userID == uuid.Nil {
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}
	if providerError := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("error"))); providerError != "" {
		a.redirectGoogleSearchConsoleCallback(r, "cancelled")
		return nil
	}
	code := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("code")))
	if code == "" || len(code) > 8192 {
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}

	err = a.WithTenantApp(orgID, func(scoped *App) error {
		if !scoped.HasPermission(userID, models.ResourceSettingsIntegrations, models.ActionWrite, orgID) {
			return errGoogleSearchConsoleForbidden
		}
		return nil
	})
	if err != nil {
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}

	oauthConfig, err := a.googleSearchConsoleOAuthConfig()
	if err != nil || !a.hasIntegrationEncryptionKey() {
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}
	baseClient := oauthHTTPClientWithoutRedirects(a.HTTPClient)
	exchangeCtx, exchangeCancel := context.WithTimeout(context.Background(), googleSearchConsoleHTTPTimeout)
	defer exchangeCancel()
	exchangeCtx = context.WithValue(exchangeCtx, oauth2.HTTPClient, baseClient)
	token, err := oauthConfig.Exchange(exchangeCtx, code, oauth2.VerifierOption(state.CodeVerifier))
	if err != nil {
		a.Log.Error("Google Search Console OAuth exchange failed", "organization_id", orgID)
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}
	refreshToken := strings.TrimSpace(token.RefreshToken)
	if refreshToken == "" {
		// Never combine a new access token/property list with a refresh token
		// retained from a potentially different Google account.
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}

	apiClient, err := a.googleSearchConsoleClient(exchangeCtx, token)
	if err != nil {
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}
	sites, err := a.fetchGoogleSearchConsoleSites(exchangeCtx, apiClient)
	if err != nil {
		a.Log.Error("Google Search Console property discovery failed", "organization_id", orgID)
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}
	encryptedRefreshToken, err := appcrypto.Encrypt(refreshToken, a.integrationEncryptionKey())
	if err != nil || !appcrypto.IsEncrypted(encryptedRefreshToken) {
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}

	now := time.Now().UTC()
	err = a.WithTenantApp(orgID, func(scoped *App) error {
		return scoped.DB.Transaction(func(tx *gorm.DB) error {
			txApp := scoped.scopedApp(tx, orgID)
			if !txApp.HasPermission(userID, models.ResourceSettingsIntegrations, models.ActionWrite, orgID) {
				return errGoogleSearchConsoleForbidden
			}
			row, _, err := lockOrCreateIntegrationRow(tx, orgID, userID, integrationProviderGoogleSearchConsole)
			if err != nil {
				return err
			}
			if row.CredentialData == nil {
				row.CredentialData = models.JSONB{}
			}
			previouslyConnected := encryptedCredentialFlag(row, googleSearchConsoleRefreshTokenCredential).Configured
			row.CredentialData[googleSearchConsoleRefreshTokenCredential] = encryptedRefreshToken
			row.CredentialsUpdatedAt = &now
			row.LastTestedAt = &now
			row.LastSuccessfulAt = &now
			row.LastErrorCode = ""
			row.LastErrorMessage = ""
			row.ValidationToken = ""
			row.UpdatedByID = &userID
			if row.CreatedByID == nil {
				row.CreatedByID = &userID
			}
			if err := tx.Save(row).Error; err != nil {
				return err
			}
			if err := persistGoogleSearchConsoleSites(tx, orgID, row.ID, sites, now); err != nil {
				return err
			}
			var selectedCount int64
			if err := tx.Model(&models.GoogleSearchConsoleProperty{}).
				Where("organization_id = ? AND available = ? AND selected = ?", orgID, true, true).
				Count(&selectedCount).Error; err != nil {
				return err
			}
			row.Enabled = selectedCount > 0
			if err := tx.Save(row).Error; err != nil {
				return err
			}
			return audit.LogAudit(
				tx,
				orgID,
				userID,
				audit.GetUserName(tx, userID),
				models.ResourceSettingsIntegrations,
				row.ID,
				models.AuditActionUpdated,
				map[string]any{"provider": integrationProviderGoogleSearchConsole, "connected": previouslyConnected},
				map[string]any{"provider": integrationProviderGoogleSearchConsole, "connected": true, "property_count": len(sites)},
				map[string]any{"field": "refresh_token", "old_value": "********", "new_value": "********"},
			)
		})
	})
	if err != nil {
		a.Log.Error("Failed to persist Google Search Console authorization", "error", err, "organization_id", orgID)
		a.redirectGoogleSearchConsoleCallback(r, "error")
		return nil
	}
	a.redirectGoogleSearchConsoleCallback(r, "connected")
	return nil
}

func (a *App) redirectGoogleSearchConsoleCallback(r *fastglue.Request, status string) {
	basePath := ""
	if a != nil && a.Config != nil {
		basePath = strings.TrimRight(sanitizeRedirectPath(strings.TrimSpace(a.Config.Server.BasePath)), "/")
	}
	destination := basePath + "/settings/integrations?google_search_console=" + url.QueryEscape(status)
	r.RequestCtx.Response.Header.Set("Cache-Control", "no-store")
	// Keep the callback target relative so an untrusted or absent Host header
	// cannot influence the redirect origin. RequestCtx.Redirect normalizes a
	// relative target into an absolute URL using the inbound request host.
	r.RequestCtx.Response.Header.Set("Location", destination)
	r.RequestCtx.Response.SetStatusCode(fasthttp.StatusSeeOther)
}

// ListGoogleSearchConsoleProperties returns only currently verified/supported
// website properties for the current tenant.
func (a *App) ListGoogleSearchConsoleProperties(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceSettingsIntegrations, models.ActionRead)
	if err != nil {
		return nil
	}
	response, err := a.googleSearchConsoleProperties(orgID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load Google Search Console properties", nil, "")
	}
	return r.SendEnvelope(response)
}

// GetGoogleSearchVisibilitySetup exposes only the connection state and
// tenant-selected properties needed by analytics users. Integration settings
// and credential metadata remain behind settings.integrations permissions.
func (a *App) GetGoogleSearchVisibilitySetup(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceAnalytics, models.ActionRead)
	if err != nil {
		return nil
	}
	integration, err := a.integrationResponse(orgID, integrationProviderGoogleSearchConsole)
	if err != nil {
		a.Log.Error("Failed to load Google Search Console analytics setup", "error", err, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load Search Visibility setup", nil, "")
	}

	var rows []models.GoogleSearchConsoleProperty
	if err := a.DB.Where("organization_id = ? AND available = ? AND selected = ?", orgID, true, true).
		Order("display_name ASC, id ASC").Find(&rows).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load Search Visibility properties", nil, "")
	}
	response := googleSearchVisibilitySetupResponse{
		Status:     integration.Status,
		Message:    integration.Message,
		LastError:  integration.Connection.LastError,
		Properties: make([]googleSearchConsolePropertyResponse, 0, len(rows)),
	}
	for _, row := range rows {
		response.Properties = append(response.Properties, googleSearchConsolePropertyResponse{
			ID:              row.ID,
			SiteURL:         row.SiteURL,
			DisplayName:     row.DisplayName,
			PropertyType:    row.PropertyType,
			PermissionLevel: row.PermissionLevel,
			Selected:        true,
			LastSyncedAt:    row.LastSyncedAt,
		})
	}
	return r.SendEnvelope(response)
}

type updateGoogleSearchConsolePropertiesRequest struct {
	PropertyIDs []uuid.UUID `json:"property_ids"`
}

// UpdateGoogleSearchConsoleProperties persists an explicit property selection.
// Raw site URLs are never accepted as selectors.
func (a *App) UpdateGoogleSearchConsoleProperties(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceSettingsIntegrations, models.ActionWrite)
	if err != nil {
		return nil
	}
	var request updateGoogleSearchConsolePropertiesRequest
	if err := decodeStrictGoogleJSON(r.RequestCtx.PostBody(), &request); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}
	if len(request.PropertyIDs) == 0 || len(request.PropertyIDs) > googleSearchConsoleMaxProperties {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Select at least one available website property", nil, "")
	}
	uniqueIDs := make([]uuid.UUID, 0, len(request.PropertyIDs))
	seen := make(map[uuid.UUID]struct{}, len(request.PropertyIDs))
	for _, propertyID := range request.PropertyIDs {
		if propertyID == uuid.Nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "property_ids contains an invalid ID", nil, "")
		}
		if _, exists := seen[propertyID]; exists {
			continue
		}
		seen[propertyID] = struct{}{}
		uniqueIDs = append(uniqueIDs, propertyID)
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var row models.ProviderIntegration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND provider = ?", orgID, integrationProviderGoogleSearchConsole).
			First(&row).Error; err != nil {
			return errGoogleSearchConsoleNotConnected
		}
		if _, err := a.decryptGoogleSearchConsoleRefreshToken(&row); err != nil {
			return errGoogleSearchConsoleNotConnected
		}
		var availableCount int64
		if err := tx.Model(&models.GoogleSearchConsoleProperty{}).
			Where("organization_id = ? AND available = ? AND id IN ?", orgID, true, uniqueIDs).
			Count(&availableCount).Error; err != nil {
			return err
		}
		if availableCount != int64(len(uniqueIDs)) {
			return &integrationClientError{status: fasthttp.StatusBadRequest, message: "One or more properties are unavailable for this workspace"}
		}
		if err := tx.Model(&models.GoogleSearchConsoleProperty{}).
			Where("organization_id = ?", orgID).
			Update("selected", false).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.GoogleSearchConsoleProperty{}).
			Where("organization_id = ? AND available = ? AND id IN ?", orgID, true, uniqueIDs).
			Update("selected", true).Error; err != nil {
			return err
		}
		row.Enabled = true
		row.UpdatedByID = &userID
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			models.ResourceSettingsIntegrations,
			row.ID,
			models.AuditActionUpdated,
			map[string]any{"provider": integrationProviderGoogleSearchConsole},
			map[string]any{"provider": integrationProviderGoogleSearchConsole, "selected_property_count": len(uniqueIDs)},
		)
	})
	if err != nil {
		var clientErr *integrationClientError
		if errors.As(err, &clientErr) {
			return r.SendErrorEnvelope(clientErr.status, clientErr.message, nil, "")
		}
		if errors.Is(err, errGoogleSearchConsoleNotConnected) || errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Connect Google Search Console before selecting properties", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to save Google Search Console properties", nil, "")
	}
	response, err := a.googleSearchConsoleProperties(orgID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Properties were saved but could not be reloaded", nil, "")
	}
	return r.SendEnvelope(response)
}

// RefreshGoogleSearchConsoleProperties runs provider I/O between short tenant
// phases, so it must not be registered through App.Tenant.
func (a *App) RefreshGoogleSearchConsoleProperties(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceSettingsIntegrations, models.ActionWrite)
	if err != nil {
		return nil
	}
	if err := a.runGoogleSearchConsoleValidation(r, orgID, userID); err != nil {
		return a.sendGoogleSearchConsoleOperationError(r, err, "Failed to refresh Google Search Console properties")
	}
	var response googleSearchConsolePropertiesResponse
	err = a.WithTenantApp(orgID, func(scoped *App) error {
		var loadErr error
		response, loadErr = scoped.googleSearchConsoleProperties(orgID)
		return loadErr
	})
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Properties were refreshed but could not be reloaded", nil, "")
	}
	return r.SendEnvelope(response)
}

func (a *App) googleSearchConsoleProperties(orgID uuid.UUID) (googleSearchConsolePropertiesResponse, error) {
	var rows []models.GoogleSearchConsoleProperty
	if err := a.DB.Where("organization_id = ? AND available = ?", orgID, true).
		Order("selected DESC, display_name ASC, id ASC").Find(&rows).Error; err != nil {
		return googleSearchConsolePropertiesResponse{}, err
	}
	response := googleSearchConsolePropertiesResponse{
		Properties: make([]googleSearchConsolePropertyResponse, 0, len(rows)),
	}
	for _, row := range rows {
		response.Properties = append(response.Properties, googleSearchConsolePropertyResponse{
			ID:              row.ID,
			SiteURL:         row.SiteURL,
			DisplayName:     row.DisplayName,
			PropertyType:    row.PropertyType,
			PermissionLevel: row.PermissionLevel,
			Selected:        row.Selected,
			LastSyncedAt:    row.LastSyncedAt,
		})
		if row.Selected {
			response.SelectedCount++
		}
	}
	return response, nil
}

func (a *App) testGoogleSearchConsole(r *fastglue.Request, orgID, userID uuid.UUID) error {
	testedAt := time.Now().UTC()
	err := a.runGoogleSearchConsoleValidation(r, orgID, userID)
	if err != nil {
		status := integrationStatusDegraded
		message := "Google Search Console rejected the authorization or could not be reached"
		if errors.Is(err, errGoogleSearchConsoleNotConnected) {
			status = integrationStatusNotConfigured
			message = "Connect Google Search Console before testing"
		}
		return r.SendEnvelope(map[string]any{
			"provider":  integrationProviderGoogleSearchConsole,
			"success":   false,
			"status":    status,
			"message":   message,
			"tested_at": testedAt,
		})
	}
	return r.SendEnvelope(map[string]any{
		"provider":  integrationProviderGoogleSearchConsole,
		"success":   true,
		"status":    integrationStatusConnected,
		"message":   "Connection test succeeded",
		"tested_at": testedAt,
	})
}

func (a *App) runGoogleSearchConsoleValidation(r *fastglue.Request, orgID, userID uuid.UUID) error {
	if !a.googleSearchConsolePlatformConfigured() || !a.hasIntegrationEncryptionKey() {
		return &integrationClientError{status: fasthttp.StatusServiceUnavailable, message: "Google Search Console is unavailable on this server"}
	}
	snapshot := googleSearchConsoleValidationSnapshot{
		OrganizationID:  orgID,
		UserID:          userID,
		ValidationToken: uuid.NewString(),
	}
	if err := a.WithTenantApp(orgID, func(scoped *App) error {
		return scoped.prepareGoogleSearchConsoleValidation(&snapshot)
	}); err != nil {
		return err
	}

	oauthToken := &oauth2.Token{RefreshToken: snapshot.RefreshToken}
	ctx, cancel := context.WithTimeout(requestContext(r), googleSearchConsoleHTTPTimeout)
	defer cancel()
	apiClient, err := a.googleSearchConsoleClient(ctx, oauthToken)
	if err == nil {
		var sites []googleSearchConsoleSite
		sites, err = a.fetchGoogleSearchConsoleSites(ctx, apiClient)
		finishErr := a.WithTenantApp(orgID, func(scoped *App) error {
			return scoped.finishGoogleSearchConsoleValidation(snapshot, sites, err)
		})
		if finishErr != nil {
			return finishErr
		}
		return err
	}
	_ = a.WithTenantApp(orgID, func(scoped *App) error {
		return scoped.finishGoogleSearchConsoleValidation(snapshot, nil, err)
	})
	return err
}

func (a *App) prepareGoogleSearchConsoleValidation(snapshot *googleSearchConsoleValidationSnapshot) error {
	if snapshot == nil {
		return errors.New("google search console validation snapshot is required")
	}
	return a.DB.Transaction(func(tx *gorm.DB) error {
		var row models.ProviderIntegration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND provider = ?", snapshot.OrganizationID, integrationProviderGoogleSearchConsole).
			First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errGoogleSearchConsoleNotConnected
			}
			return err
		}
		refreshToken, err := a.decryptGoogleSearchConsoleRefreshToken(&row)
		if err != nil || refreshToken == "" {
			return errGoogleSearchConsoleNotConnected
		}
		row.ValidationToken = snapshot.ValidationToken
		row.UpdatedByID = &snapshot.UserID
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		snapshot.RefreshToken = refreshToken
		return nil
	})
}

func (a *App) finishGoogleSearchConsoleValidation(snapshot googleSearchConsoleValidationSnapshot, sites []googleSearchConsoleSite, validationErr error) error {
	now := time.Now().UTC()
	return a.DB.Transaction(func(tx *gorm.DB) error {
		var row models.ProviderIntegration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND provider = ? AND validation_token = ?", snapshot.OrganizationID, integrationProviderGoogleSearchConsole, snapshot.ValidationToken).
			First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errGoogleSearchConsoleStale
			}
			return err
		}
		row.ValidationToken = ""
		row.LastTestedAt = &now
		row.UpdatedByID = &snapshot.UserID
		if validationErr == nil {
			if err := persistGoogleSearchConsoleSites(tx, snapshot.OrganizationID, row.ID, sites, now); err != nil {
				return err
			}
			var selectedCount int64
			if err := tx.Model(&models.GoogleSearchConsoleProperty{}).
				Where("organization_id = ? AND available = ? AND selected = ?", snapshot.OrganizationID, true, true).
				Count(&selectedCount).Error; err != nil {
				return err
			}
			row.Enabled = selectedCount > 0
			row.LastSuccessfulAt = &now
			row.LastErrorCode = ""
			row.LastErrorMessage = ""
		} else {
			row.LastErrorCode = integrationValidationFailedCode
			row.LastErrorMessage = "Provider validation failed"
		}
		return tx.Save(&row).Error
	})
}

func persistGoogleSearchConsoleSites(tx *gorm.DB, orgID, integrationID uuid.UUID, sites []googleSearchConsoleSite, syncedAt time.Time) error {
	if tx == nil {
		return errors.New("database is required")
	}
	if err := tx.Model(&models.GoogleSearchConsoleProperty{}).
		Where("organization_id = ?", orgID).
		Update("available", false).Error; err != nil {
		return err
	}
	for _, site := range sites {
		propertyType, supported := googleSearchConsoleWebsitePropertyType(site.SiteURL)
		if !supported || !googleSearchConsoleVerifiedPermission(site.PermissionLevel) {
			continue
		}
		digest := sha256.Sum256([]byte(site.SiteURL))
		row := models.GoogleSearchConsoleProperty{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  orgID,
			IntegrationID:   integrationID,
			SiteURL:         site.SiteURL,
			SiteURLHash:     hex.EncodeToString(digest[:]),
			DisplayName:     site.SiteURL,
			PropertyType:    propertyType,
			PermissionLevel: site.PermissionLevel,
			Available:       true,
			LastSyncedAt:    &syncedAt,
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "organization_id"}, {Name: "site_url_hash"}},
			DoUpdates: clause.Assignments(map[string]any{
				"integration_id":   integrationID,
				"site_url":         site.SiteURL,
				"display_name":     site.SiteURL,
				"property_type":    propertyType,
				"permission_level": site.PermissionLevel,
				"available":        true,
				"last_synced_at":   syncedAt,
				"updated_at":       syncedAt,
			}),
		}).Create(&row).Error; err != nil {
			return err
		}
	}
	return tx.Model(&models.GoogleSearchConsoleProperty{}).
		Where("organization_id = ? AND available = ?", orgID, false).
		Update("selected", false).Error
}

func googleSearchConsoleWebsitePropertyType(siteURL string) (string, bool) {
	if strings.HasPrefix(siteURL, "sc-domain:") {
		domain := strings.TrimPrefix(siteURL, "sc-domain:")
		if !validGoogleSearchConsoleDomain(domain) || googleSearchConsoleSocialHost(domain) {
			return "", false
		}
		return "domain", true
	}
	parsed, err := url.Parse(siteURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || len(siteURL) > 2048 {
		return "", false
	}
	if googleSearchConsoleSocialHost(parsed.Hostname()) {
		return "", false
	}
	return "url_prefix", true
}

func validGoogleSearchConsoleDomain(domain string) bool {
	if domain == "" || len(domain) > 253 || strings.ContainsAny(domain, "/:@?# \\") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') &&
				(character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') &&
				character != '-' {
				return false
			}
		}
	}
	return true
}

func googleSearchConsoleSocialHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	for _, domain := range []string{
		"instagram.com",
		"tiktok.com",
		"twitter.com",
		"x.com",
		"youtube.com",
		"youtu.be",
	} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func googleSearchConsoleVerifiedPermission(permission string) bool {
	switch strings.TrimSpace(permission) {
	case "siteOwner", "siteFullUser", "siteRestrictedUser":
		return true
	default:
		return false
	}
}

func (a *App) fetchGoogleSearchConsoleSites(ctx context.Context, client *http.Client) ([]googleSearchConsoleSite, error) {
	baseURL, err := a.googleSearchConsoleAPIBaseURL()
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/sites", nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		SiteEntry []googleSearchConsoleSite `json:"siteEntry"`
	}
	if err := doGoogleSearchConsoleJSON(client, request, &response); err != nil {
		return nil, err
	}
	result := make([]googleSearchConsoleSite, 0, len(response.SiteEntry))
	seen := make(map[string]struct{}, len(response.SiteEntry))
	for _, site := range response.SiteEntry {
		site.SiteURL = strings.TrimSpace(site.SiteURL)
		site.PermissionLevel = strings.TrimSpace(site.PermissionLevel)
		propertyType, supported := googleSearchConsoleWebsitePropertyType(site.SiteURL)
		if !supported || !googleSearchConsoleVerifiedPermission(site.PermissionLevel) {
			continue
		}
		if _, exists := seen[site.SiteURL]; exists {
			continue
		}
		seen[site.SiteURL] = struct{}{}
		site.PropertyType = propertyType
		result = append(result, site)
		if len(result) >= googleSearchConsoleMaxProperties {
			break
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SiteURL < result[j].SiteURL })
	return result, nil
}

func (a *App) googleSearchConsoleClient(ctx context.Context, token *oauth2.Token) (*http.Client, error) {
	oauthConfig, err := a.googleSearchConsoleOAuthConfig()
	if err != nil {
		return nil, err
	}
	if token == nil || (strings.TrimSpace(token.AccessToken) == "" && strings.TrimSpace(token.RefreshToken) == "") {
		return nil, errGoogleSearchConsoleNotConnected
	}
	baseClient := oauthHTTPClientWithoutRedirects(a.HTTPClient)
	oauthCtx := context.WithValue(ctx, oauth2.HTTPClient, baseClient)
	tokenSource := oauthConfig.TokenSource(oauthCtx, token)
	return &http.Client{
		Transport: &oauth2.Transport{Source: tokenSource, Base: baseClient.Transport},
		Timeout:   googleSearchConsoleHTTPTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func doGoogleSearchConsoleJSON(client *http.Client, request *http.Request, destination any) error {
	if client == nil || request == nil {
		return errors.New("google search console request is unavailable")
	}
	request.Header.Set("Accept", "application/json")
	if request.Body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("google search console request failed")
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, googleSearchConsoleMaxResponseBytes+1))
	if err != nil || int64(len(body)) > googleSearchConsoleMaxResponseBytes {
		return errors.New("google search console response was invalid")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &googleSearchConsoleAPIError{StatusCode: response.StatusCode}
	}
	if destination == nil {
		return nil
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return errors.New("google search console response was invalid")
	}
	return nil
}

func (a *App) decryptGoogleSearchConsoleRefreshToken(row *models.ProviderIntegration) (string, error) {
	if row == nil || row.CredentialData == nil || !a.hasIntegrationEncryptionKey() {
		return "", errGoogleSearchConsoleNotConnected
	}
	encrypted, _ := row.CredentialData[googleSearchConsoleRefreshTokenCredential].(string)
	if !appcrypto.IsEncrypted(strings.TrimSpace(encrypted)) {
		return "", errGoogleSearchConsoleNotConnected
	}
	decrypted, err := appcrypto.Decrypt(encrypted, a.integrationEncryptionKey())
	if err != nil || strings.TrimSpace(decrypted) == "" {
		return "", errGoogleSearchConsoleNotConnected
	}
	return strings.TrimSpace(decrypted), nil
}

// DisconnectGoogleSearchConsole destroys only this tenant's local credential.
// Revoking Google's account-level grant here could invalidate another CRM
// tenant that intentionally connected the same Google account and OAuth app.
func (a *App) DisconnectGoogleSearchConsole(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceSettingsIntegrations, models.ActionWrite)
	if err != nil {
		return nil
	}
	err = a.WithTenantApp(orgID, func(scoped *App) error {
		return scoped.disconnectGoogleSearchConsoleLocally(orgID, userID)
	})
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to disconnect Google Search Console", nil, "")
	}

	var integration IntegrationResponse
	err = a.WithTenantApp(orgID, func(scoped *App) error {
		var loadErr error
		integration, loadErr = scoped.integrationResponse(orgID, integrationProviderGoogleSearchConsole)
		return loadErr
	})
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Google Search Console was disconnected but the integration could not be reloaded", nil, "")
	}
	return r.SendEnvelope(integration)
}

func (a *App) disconnectGoogleSearchConsoleLocally(orgID, userID uuid.UUID) error {
	return a.DB.Transaction(func(tx *gorm.DB) error {
		var row models.ProviderIntegration
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND provider = ?", orgID, integrationProviderGoogleSearchConsole).
			First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.CredentialData == nil {
			row.CredentialData = models.JSONB{}
		}
		delete(row.CredentialData, googleSearchConsoleRefreshTokenCredential)
		now := time.Now().UTC()
		row.Enabled = false
		row.CredentialsUpdatedAt = &now
		row.LastTestedAt = nil
		row.LastSuccessfulAt = nil
		row.LastErrorCode = ""
		row.LastErrorMessage = ""
		row.ValidationToken = ""
		row.UpdatedByID = &userID
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.GoogleSearchConsoleProperty{}).
			Where("organization_id = ?", orgID).
			Updates(map[string]any{"selected": false, "available": false}).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			models.ResourceSettingsIntegrations,
			row.ID,
			models.AuditActionUpdated,
			map[string]any{"provider": integrationProviderGoogleSearchConsole, "connected": true},
			map[string]any{"provider": integrationProviderGoogleSearchConsole, "connected": false},
			map[string]any{"field": "refresh_token", "old_value": "********", "new_value": nil},
		)
	})
}

func (a *App) sendGoogleSearchConsoleOperationError(r *fastglue.Request, err error, fallback string) error {
	var clientErr *integrationClientError
	if errors.As(err, &clientErr) {
		return r.SendErrorEnvelope(clientErr.status, clientErr.message, nil, "")
	}
	if errors.Is(err, errGoogleSearchConsoleNotConnected) {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Connect Google Search Console first", nil, "")
	}
	if errors.Is(err, errGoogleSearchConsoleStale) {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Google Search Console settings changed; try again", nil, "")
	}
	var providerErr *googleSearchConsoleAPIError
	if errors.As(err, &providerErr) {
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Google Search Console rejected the request", nil, "")
	}
	a.Log.Error(fallback, "error", err)
	return r.SendErrorEnvelope(fasthttp.StatusBadGateway, fallback, nil, "")
}

// GetGoogleSearchVisibility queries live Search Console metrics. It must not be
// registered through App.Tenant because provider I/O happens between short
// tenant-scoped database phases.
func (a *App) GetGoogleSearchVisibility(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceAnalytics, models.ActionRead)
	if err != nil {
		return nil
	}
	parameters, err := parseGoogleSearchVisibilityParameters(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var property models.GoogleSearchConsoleProperty
	var refreshToken string
	err = a.WithTenantApp(orgID, func(scoped *App) error {
		var row models.ProviderIntegration
		if err := scoped.DB.Where("organization_id = ? AND provider = ? AND enabled = ?", orgID, integrationProviderGoogleSearchConsole, true).
			First(&row).Error; err != nil {
			return errGoogleSearchConsoleNotConnected
		}
		var err error
		refreshToken, err = scoped.decryptGoogleSearchConsoleRefreshToken(&row)
		if err != nil {
			return err
		}
		query := scoped.DB.Where("organization_id = ? AND available = ? AND selected = ?", orgID, true, true)
		if parameters.PropertyID != uuid.Nil {
			query = query.Where("id = ?", parameters.PropertyID)
			return query.First(&property).Error
		}
		var selected []models.GoogleSearchConsoleProperty
		if err := query.Order("id ASC").Limit(2).Find(&selected).Error; err != nil {
			return err
		}
		if len(selected) == 0 {
			return &integrationClientError{status: fasthttp.StatusConflict, message: "No Search Console property is selected for analytics"}
		}
		if len(selected) > 1 {
			return &integrationClientError{status: fasthttp.StatusBadRequest, message: "property_id is required when multiple properties are selected"}
		}
		property = selected[0]
		return nil
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Selected Search Console property was not found", nil, "")
		}
		return a.sendGoogleSearchConsoleOperationError(r, err, "Failed to load Google Search Console connection")
	}
	if parameters.Page != "" && !googleSearchConsolePageBelongsToProperty(parameters.Page, property.SiteURL, property.PropertyType) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "page must belong to the selected Search Console property", nil, "")
	}

	ctx, cancel := context.WithTimeout(requestContext(r), googleSearchConsoleHTTPTimeout)
	defer cancel()
	apiClient, err := a.googleSearchConsoleClient(ctx, &oauth2.Token{RefreshToken: refreshToken})
	if err != nil {
		return a.sendGoogleSearchConsoleOperationError(r, err, "Google Search Console is unavailable")
	}
	response, err := a.fetchGoogleSearchVisibility(ctx, apiClient, property, parameters)
	if err != nil {
		a.recordGoogleSearchConsoleAnalyticsResult(orgID, property.ID, false)
		return a.sendGoogleSearchConsoleOperationError(r, err, "Google Search Console analytics could not be loaded")
	}
	a.recordGoogleSearchConsoleAnalyticsResult(orgID, property.ID, true)
	return r.SendEnvelope(response)
}

type googleSearchVisibilityParameters struct {
	PropertyID uuid.UUID
	StartDate  time.Time
	EndDate    time.Time
	SearchType string
	Page       string
	Limit      int
}

func parseGoogleSearchVisibilityParameters(r *fastglue.Request) (googleSearchVisibilityParameters, error) {
	now := time.Now().UTC()
	args := r.RequestCtx.QueryArgs()
	startRaw := strings.TrimSpace(string(args.Peek("start_date")))
	endRaw := strings.TrimSpace(string(args.Peek("end_date")))
	if startRaw == "" && endRaw == "" {
		end := now.AddDate(0, 0, -2)
		startRaw = end.AddDate(0, 0, -27).Format("2006-01-02")
		endRaw = end.Format("2006-01-02")
	} else if startRaw == "" || endRaw == "" {
		return googleSearchVisibilityParameters{}, errors.New("start_date and end_date must be provided together")
	}
	start, err := time.Parse("2006-01-02", startRaw)
	if err != nil {
		return googleSearchVisibilityParameters{}, errors.New("start_date must use YYYY-MM-DD")
	}
	end, err := time.Parse("2006-01-02", endRaw)
	if err != nil {
		return googleSearchVisibilityParameters{}, errors.New("end_date must use YYYY-MM-DD")
	}
	finalDataCutoff, _ := time.Parse("2006-01-02", now.AddDate(0, 0, -2).Format("2006-01-02"))
	oldest, _ := time.Parse("2006-01-02", now.AddDate(0, -16, 0).Format("2006-01-02"))
	if start.After(end) {
		return googleSearchVisibilityParameters{}, errors.New("start_date must not be after end_date")
	}
	if end.After(finalDataCutoff) {
		return googleSearchVisibilityParameters{}, errors.New("end_date must be at least 2 days before today for final Search Console data")
	}
	if start.Before(oldest) {
		return googleSearchVisibilityParameters{}, errors.New("start_date must be within the last 16 months")
	}

	searchType := strings.TrimSpace(string(args.Peek("search_type")))
	if searchType == "" {
		searchType = "web"
	}
	switch strings.ToLower(searchType) {
	case "web", "image", "video", "news", "discover":
		searchType = strings.ToLower(searchType)
	case "googlenews":
		searchType = "googleNews"
	default:
		return googleSearchVisibilityParameters{}, errors.New("search_type must be web, image, video, news, discover, or googleNews")
	}

	page := strings.TrimSpace(string(args.Peek("page")))
	if page != "" {
		if len(page) > 2048 {
			return googleSearchVisibilityParameters{}, errors.New("page is too long")
		}
		if strings.Contains(page, "#") {
			return googleSearchVisibilityParameters{}, errors.New("page must not contain a fragment")
		}
		parsed, err := url.ParseRequestURI(page)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" {
			return googleSearchVisibilityParameters{}, errors.New("page must be an absolute HTTP or HTTPS URL without credentials or a fragment")
		}
	}

	propertyID := uuid.Nil
	if value := strings.TrimSpace(string(args.Peek("property_id"))); value != "" {
		propertyID, err = uuid.Parse(value)
		if err != nil || propertyID == uuid.Nil {
			return googleSearchVisibilityParameters{}, errors.New("property_id must be a valid UUID")
		}
	}
	limit := googleSearchConsoleDefaultTopLimit
	if value := strings.TrimSpace(string(args.Peek("limit"))); value != "" {
		parsedLimit, parseErr := parseStrictPositiveInt(value)
		if parseErr != nil || parsedLimit < 1 || parsedLimit > googleSearchConsoleMaxTopLimit {
			return googleSearchVisibilityParameters{}, fmt.Errorf("limit must be between 1 and %d", googleSearchConsoleMaxTopLimit)
		}
		limit = parsedLimit
	}
	return googleSearchVisibilityParameters{
		PropertyID: propertyID,
		StartDate:  start,
		EndDate:    end,
		SearchType: searchType,
		Page:       page,
		Limit:      limit,
	}, nil
}

func parseStrictPositiveInt(value string) (int, error) {
	result := 0
	if value == "" {
		return 0, errors.New("empty integer")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, errors.New("invalid integer")
		}
		result = result*10 + int(character-'0')
		if result > googleSearchConsoleMaxTopLimit {
			return result, nil
		}
	}
	return result, nil
}

func googleSearchConsolePageBelongsToProperty(page, siteURL, propertyType string) bool {
	pageURL, err := url.Parse(page)
	if err != nil || pageURL.Hostname() == "" {
		return false
	}
	if propertyType == "domain" && strings.HasPrefix(siteURL, "sc-domain:") {
		domain := strings.ToLower(strings.TrimPrefix(siteURL, "sc-domain:"))
		host := strings.ToLower(pageURL.Hostname())
		return host == domain || strings.HasSuffix(host, "."+domain)
	}
	baseURL, err := url.Parse(siteURL)
	if err != nil || !strings.EqualFold(baseURL.Scheme, pageURL.Scheme) || !strings.EqualFold(baseURL.Host, pageURL.Host) {
		return false
	}
	basePath := baseURL.EscapedPath()
	if basePath == "" {
		basePath = "/"
	}
	return strings.HasPrefix(pageURL.EscapedPath(), basePath)
}

func (a *App) fetchGoogleSearchVisibility(ctx context.Context, client *http.Client, property models.GoogleSearchConsoleProperty, parameters googleSearchVisibilityParameters) (googleSearchVisibilityResponse, error) {
	baseRequest := googleSearchAnalyticsRequest{
		StartDate: parameters.StartDate.Format("2006-01-02"),
		EndDate:   parameters.EndDate.Format("2006-01-02"),
		Type:      parameters.SearchType,
		DataState: "final",
	}
	if parameters.Page != "" {
		baseRequest.DimensionFilterGroups = []googleSearchDimensionFilterGroup{{
			GroupType: "and",
			Filters: []googleSearchDimensionFilter{{
				Dimension:  "page",
				Operator:   "equals",
				Expression: parameters.Page,
			}},
		}}
	}

	var summaryRows, trendRows, queryRows, pageRows []googleSearchAnalyticsRow
	type searchJob struct {
		dimensions  []string
		rowLimit    int
		destination *[]googleSearchAnalyticsRow
	}
	jobs := []searchJob{
		{rowLimit: 1, destination: &summaryRows},
		{dimensions: []string{"date"}, rowLimit: 500, destination: &trendRows},
		{dimensions: []string{"page"}, rowLimit: parameters.Limit, destination: &pageRows},
	}
	// Discover and Google News do not expose query dimensions.
	if parameters.SearchType != "discover" && parameters.SearchType != "googleNews" {
		jobs = append(jobs, searchJob{dimensions: []string{"query"}, rowLimit: parameters.Limit, destination: &queryRows})
	}
	errChannel := make(chan error, len(jobs))
	var waitGroup sync.WaitGroup
	for _, job := range jobs {
		job := job
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			request := baseRequest
			request.Dimensions = job.dimensions
			request.RowLimit = job.rowLimit
			rows, err := a.queryGoogleSearchAnalytics(ctx, client, property.SiteURL, request)
			if err == nil {
				*job.destination = rows
			}
			errChannel <- err
		}()
	}
	waitGroup.Wait()
	close(errChannel)
	for err := range errChannel {
		if err != nil {
			return googleSearchVisibilityResponse{}, err
		}
	}

	response := googleSearchVisibilityResponse{
		Property: googleSearchVisibilityProperty{
			ID:           property.ID,
			SiteURL:      property.SiteURL,
			DisplayName:  property.DisplayName,
			PropertyType: property.PropertyType,
		},
		StartDate:  baseRequest.StartDate,
		EndDate:    baseRequest.EndDate,
		SearchType: parameters.SearchType,
		Page:       parameters.Page,
		DataState:  "final",
		Trend:      make([]googleSearchTrendPoint, 0),
		TopQueries: make([]googleSearchQueryMetric, 0, len(queryRows)),
		TopPages:   make([]googleSearchPageMetric, 0, len(pageRows)),
	}
	if len(summaryRows) > 0 {
		response.Summary = googleSearchMetricFromRow(summaryRows[0])
	}
	trendByDate := make(map[string]googleSearchMetric, len(trendRows))
	for _, row := range trendRows {
		if len(row.Keys) > 0 {
			trendByDate[row.Keys[0]] = googleSearchMetricFromRow(row)
		}
	}
	for date := parameters.StartDate; !date.After(parameters.EndDate); date = date.AddDate(0, 0, 1) {
		dateString := date.Format("2006-01-02")
		response.Trend = append(response.Trend, googleSearchTrendPoint{Date: dateString, googleSearchMetric: trendByDate[dateString]})
	}
	for _, row := range queryRows {
		if len(row.Keys) > 0 {
			response.TopQueries = append(response.TopQueries, googleSearchQueryMetric{Query: row.Keys[0], googleSearchMetric: googleSearchMetricFromRow(row)})
		}
	}
	for _, row := range pageRows {
		if len(row.Keys) > 0 {
			response.TopPages = append(response.TopPages, googleSearchPageMetric{Page: row.Keys[0], googleSearchMetric: googleSearchMetricFromRow(row)})
		}
	}
	return response, nil
}

func googleSearchMetricFromRow(row googleSearchAnalyticsRow) googleSearchMetric {
	return googleSearchMetric{
		Clicks:      row.Clicks,
		Impressions: row.Impressions,
		CTR:         row.CTR,
		Position:    row.Position,
	}
}

func (a *App) queryGoogleSearchAnalytics(ctx context.Context, client *http.Client, siteURL string, query googleSearchAnalyticsRequest) ([]googleSearchAnalyticsRow, error) {
	baseURL, err := a.googleSearchConsoleAPIBaseURL()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(query)
	if err != nil {
		return nil, err
	}
	endpoint := baseURL + "/sites/" + url.PathEscape(siteURL) + "/searchAnalytics/query"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var response googleSearchAnalyticsResponse
	if err := doGoogleSearchConsoleJSON(client, request, &response); err != nil {
		return nil, err
	}
	return response.Rows, nil
}

func (a *App) recordGoogleSearchConsoleAnalyticsResult(orgID, propertyID uuid.UUID, success bool) {
	now := time.Now().UTC()
	_ = a.WithTenantApp(orgID, func(scoped *App) error {
		return scoped.DB.Transaction(func(tx *gorm.DB) error {
			if success {
				if err := tx.Model(&models.GoogleSearchConsoleProperty{}).
					Where("organization_id = ? AND id = ?", orgID, propertyID).
					Update("last_synced_at", now).Error; err != nil {
					return err
				}
				return tx.Model(&models.ProviderIntegration{}).
					Where("organization_id = ? AND provider = ?", orgID, integrationProviderGoogleSearchConsole).
					Updates(map[string]any{"last_successful_at": now, "last_error_code": "", "last_error_message": ""}).Error
			}
			return tx.Model(&models.ProviderIntegration{}).
				Where("organization_id = ? AND provider = ?", orgID, integrationProviderGoogleSearchConsole).
				Updates(map[string]any{"last_error_code": integrationAnalyticsFailedCode, "last_error_message": "Provider analytics request failed"}).Error
		})
	})
}

func decodeStrictGoogleJSON(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}
