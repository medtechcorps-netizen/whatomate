package handlers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

var (
	errMetaIntegrationDisabled        = errors.New("Meta integration is disabled")
	errAccountEncryptionUnavailable   = errors.New("account credential encryption is unavailable")
	errMetaAppIDNotConfigured         = errors.New("Meta App ID is not configured")
	errMetaAppSecretNotConfigured     = errors.New("Meta App Secret is not configured")
	errMetaTokenValidationUnavailable = errors.New("Meta access token validation is unavailable")
)

// AccountRequest represents the request body for creating/updating an account
type AccountRequest struct {
	Name                   string `json:"name" validate:"required"`
	AppID                  string `json:"app_id"` // Deprecated: managed in the Integration Center.
	PhoneID                string `json:"phone_id" validate:"required"`
	BusinessID             string `json:"business_id" validate:"required"`
	AccessToken            string `json:"access_token" validate:"required"`
	AppSecret              string `json:"app_secret"`           // Deprecated: managed in the Integration Center.
	WebhookVerifyToken     string `json:"webhook_verify_token"` // Deprecated: managed in the Integration Center.
	APIVersion             string `json:"api_version"`
	IsDefaultIncoming      bool   `json:"is_default_incoming"`
	IsDefaultOutgoing      bool   `json:"is_default_outgoing"`
	AutoReadReceipt        bool   `json:"auto_read_receipt"`
	BusinessCallingEnabled bool   `json:"business_calling_enabled"`
}

// AccountResponse represents the response for an account (without sensitive data)
type AccountResponse struct {
	ID                     uuid.UUID  `json:"id"`
	Name                   string     `json:"name"`
	PhoneID                string     `json:"phone_id"`
	BusinessID             string     `json:"business_id"`
	APIVersion             string     `json:"api_version"`
	IsDefaultIncoming      bool       `json:"is_default_incoming"`
	IsDefaultOutgoing      bool       `json:"is_default_outgoing"`
	AutoReadReceipt        bool       `json:"auto_read_receipt"`
	BusinessCallingEnabled bool       `json:"business_calling_enabled"`
	Status                 string     `json:"status"`
	HasAccessToken         bool       `json:"has_access_token"`
	AccessTokenExpiresAt   *time.Time `json:"access_token_expires_at,omitempty"`
	PhoneNumber            string     `json:"phone_number,omitempty"`
	DisplayName            string     `json:"display_name,omitempty"`
	CreatedByID            *uuid.UUID `json:"created_by_id,omitempty"`
	CreatedByName          string     `json:"created_by_name,omitempty"`
	UpdatedByID            *uuid.UUID `json:"updated_by_id,omitempty"`
	UpdatedByName          string     `json:"updated_by_name,omitempty"`
	CreatedAt              string     `json:"created_at"`
	UpdatedAt              string     `json:"updated_at"`
	Warning                string     `json:"warning,omitempty"`
}

// ListAccounts returns all WhatsApp accounts for the organization
func (a *App) ListAccounts(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceAccounts, models.ActionRead)
	if err != nil {
		return nil
	}

	var accounts []models.WhatsAppAccount
	if err := a.DB.Where("organization_id = ?", orgID).Order("created_at DESC").Find(&accounts).Error; err != nil {
		a.Log.Error("Failed to list accounts", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list accounts", nil, "")
	}

	// Convert to response format (hide sensitive data)
	response := make([]AccountResponse, len(accounts))
	for i, acc := range accounts {
		response[i] = accountToResponse(acc)
	}

	return r.SendEnvelope(map[string]any{
		"accounts": response,
	})
}

// CreateAccount creates a new WhatsApp account
func (a *App) CreateAccount(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
	if err != nil {
		return nil
	}

	var req AccountRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	// Validate required fields
	if req.Name == "" || req.PhoneID == "" || req.BusinessID == "" || req.AccessToken == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Name, phone_id, business_id, and access_token are required", nil, "")
	}
	if strings.TrimSpace(req.AppID) != "" || strings.TrimSpace(req.AppSecret) != "" {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Meta App ID and App Secret are managed in Settings > Integrations",
			nil,
			"",
		)
	}
	if strings.TrimSpace(req.WebhookVerifyToken) != "" {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Meta Webhook Verify Token is managed in Settings > Integrations",
			nil,
			"",
		)
	}
	if !a.hasIntegrationEncryptionKey() {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
	}

	validationCtx, cancelValidation := context.WithTimeout(requestContext(r), 30*time.Second)
	defer cancelValidation()
	_, tokenExpiresAt, err := a.debugAndValidateMetaAccessToken(
		validationCtx,
		orgID,
		req.AccessToken,
	)
	if err != nil {
		return a.sendMetaTokenPreflightError(r, err)
	}

	// Set the effective API version before validating the account tuple. Meta's
	// phone, WABA, and membership lookups must all succeed with this token before
	// any row can be presented as usable.
	apiVersion := req.APIVersion
	if apiVersion == "" {
		apiVersion = a.defaultAPIVersion()
	}
	if _, err := a.validateWhatsAppAccountContract(
		validationCtx,
		req.PhoneID,
		req.BusinessID,
		req.AccessToken,
		apiVersion,
	); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	account := models.WhatsAppAccount{
		OrganizationID:         orgID,
		Name:                   req.Name,
		PhoneID:                req.PhoneID,
		BusinessID:             req.BusinessID,
		AccessToken:            req.AccessToken,
		AccessTokenExpiresAt:   tokenExpiresAt,
		APIVersion:             apiVersion,
		IsDefaultIncoming:      req.IsDefaultIncoming,
		IsDefaultOutgoing:      req.IsDefaultOutgoing,
		AutoReadReceipt:        req.AutoReadReceipt,
		BusinessCallingEnabled: req.BusinessCallingEnabled,
		Status:                 "pending_subscription",
		CreatedByID:            &userID,
		UpdatedByID:            &userID,
	}
	// Keep a request-scoped plaintext copy solely for the provider call. The
	// persisted model is encrypted before it reaches the database.
	subscriptionAccount := a.toWhatsAppAccount(&account)

	if err := a.encryptAccountSecrets(&account); err != nil {
		a.Log.Error("Failed to encrypt account secrets", "error", err)
		if errors.Is(err, errAccountEncryptionUnavailable) {
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create account", nil, "")
	}

	// If this is set as default, unset other defaults
	if req.IsDefaultIncoming {
		a.DB.Model(&models.WhatsAppAccount{}).
			Where("organization_id = ? AND is_default_incoming = ?", orgID, true).
			Update("is_default_incoming", false)
	}
	if req.IsDefaultOutgoing {
		a.DB.Model(&models.WhatsAppAccount{}).
			Where("organization_id = ? AND is_default_outgoing = ?", orgID, true).
			Update("is_default_outgoing", false)
	}

	if err := a.DB.Create(&account).Error; err != nil {
		a.Log.Error("Failed to create account", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create account", nil, "")
	}

	subscriptionErr, statusErr := a.subscribeAndPersistWhatsAppStatus(
		validationCtx,
		orgID,
		&account,
		subscriptionAccount,
		"active",
		"subscription_failed",
	)
	if statusErr != nil {
		a.Log.Error("Failed to persist WhatsApp subscription readiness", "error", statusErr, "account_id", account.ID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Account saved, but webhook subscription status could not be recorded", nil, "")
	}

	a.DB.Preload("CreatedBy").Preload("UpdatedBy").
		Where("id = ? AND organization_id = ?", account.ID, orgID).
		First(&account)
	a.logAudit(orgID, userID,
		"account", account.ID, models.AuditActionCreated, nil, &account)

	response := accountToResponse(account)
	if subscriptionErr != nil {
		response.Warning = "Webhook subscription failed; use the account Subscribe action after checking Meta permissions"
	}
	return r.SendEnvelope(response)
}

// GetAccount returns a single WhatsApp account
func (a *App) GetAccount(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceAccounts, models.ActionRead)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}

	account, err := findByIDAndOrg[models.WhatsAppAccount](
		a.DB.Preload("CreatedBy").Preload("UpdatedBy"), r, id, orgID, "Account")
	if err != nil {
		return nil
	}

	return r.SendEnvelope(accountToResponse(*account))
}

// UpdateAccount updates a WhatsApp account
func (a *App) UpdateAccount(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}

	account, err := a.resolveWhatsAppAccountByID(r, id, orgID)
	if err != nil {
		return nil
	}

	oldAccount := *account // value copy for audit

	var req AccountRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if strings.TrimSpace(req.AppID) != "" || strings.TrimSpace(req.AppSecret) != "" {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Meta App ID and App Secret are managed in Settings > Integrations",
			nil,
			"",
		)
	}
	if strings.TrimSpace(req.WebhookVerifyToken) != "" {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Meta Webhook Verify Token is managed in Settings > Integrations",
			nil,
			"",
		)
	}

	var tokenExpiresAt *time.Time
	if req.AccessToken != "" {
		if !a.hasIntegrationEncryptionKey() {
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
		}
		validationCtx, cancelValidation := context.WithTimeout(requestContext(r), 20*time.Second)
		defer cancelValidation()
		_, tokenExpiresAt, err = a.debugAndValidateMetaAccessToken(
			validationCtx,
			orgID,
			req.AccessToken,
		)
		if err != nil {
			return a.sendMetaTokenPreflightError(r, err)
		}
	}

	effectivePhoneID := account.PhoneID
	if req.PhoneID != "" {
		effectivePhoneID = req.PhoneID
	}
	effectiveBusinessID := account.BusinessID
	if req.BusinessID != "" {
		effectiveBusinessID = req.BusinessID
	}
	effectiveAccessToken := account.AccessToken
	if req.AccessToken != "" {
		effectiveAccessToken = req.AccessToken
	}
	effectiveAPIVersion := account.APIVersion
	if req.APIVersion != "" {
		effectiveAPIVersion = req.APIVersion
	}
	if effectiveAPIVersion == "" {
		effectiveAPIVersion = a.defaultAPIVersion()
	}
	accountContractChanged := effectivePhoneID != account.PhoneID ||
		effectiveBusinessID != account.BusinessID ||
		req.AccessToken != "" ||
		effectiveAPIVersion != account.APIVersion
	var subscriptionCtx context.Context
	var cancelSubscription context.CancelFunc
	if accountContractChanged {
		if !a.hasIntegrationEncryptionKey() {
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
		}
		subscriptionCtx, cancelSubscription = context.WithTimeout(requestContext(r), 30*time.Second)
		defer cancelSubscription()
		if _, err := a.validateWhatsAppAccountContract(
			subscriptionCtx,
			effectivePhoneID,
			effectiveBusinessID,
			effectiveAccessToken,
			effectiveAPIVersion,
		); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
		}
	}

	// Update fields if provided
	if req.Name != "" {
		account.Name = req.Name
	}
	if req.PhoneID != "" {
		account.PhoneID = req.PhoneID
	}
	if req.BusinessID != "" {
		account.BusinessID = req.BusinessID
	}
	tokenChanged := false
	if req.AccessToken != "" {
		account.AccessToken = req.AccessToken
		account.AccessTokenExpiresAt = tokenExpiresAt
		tokenChanged = true
	}
	if req.APIVersion != "" {
		account.APIVersion = req.APIVersion
	} else if account.APIVersion == "" {
		account.APIVersion = effectiveAPIVersion
	}
	account.AutoReadReceipt = req.AutoReadReceipt
	account.BusinessCallingEnabled = req.BusinessCallingEnabled

	// Handle default flags
	if req.IsDefaultIncoming && !account.IsDefaultIncoming {
		a.DB.Model(&models.WhatsAppAccount{}).
			Where("organization_id = ? AND is_default_incoming = ?", orgID, true).
			Update("is_default_incoming", false)
	}
	if req.IsDefaultOutgoing && !account.IsDefaultOutgoing {
		a.DB.Model(&models.WhatsAppAccount{}).
			Where("organization_id = ? AND is_default_outgoing = ?", orgID, true).
			Update("is_default_outgoing", false)
	}
	account.IsDefaultIncoming = req.IsDefaultIncoming
	account.IsDefaultOutgoing = req.IsDefaultOutgoing
	account.UpdatedByID = &userID
	var subscriptionAccount *whatsapp.Account
	if accountContractChanged {
		account.Status = "pending_subscription"
		subscriptionAccount = a.toWhatsAppAccount(account)
	}

	// resolveWhatsAppAccountByID returns decrypted legacy secrets. Re-encrypt
	// every secret before saving so an unrelated account edit can never write an
	// access token, legacy app secret, or PIN back in plaintext.
	if err := a.encryptAccountSecrets(account); err != nil {
		a.Log.Error("Failed to encrypt account secrets", "error", err)
		if errors.Is(err, errAccountEncryptionUnavailable) {
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update account", nil, "")
	}

	if err := a.DB.Save(account).Error; err != nil {
		a.Log.Error("Failed to update account", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update account", nil, "")
	}

	var subscriptionErr error
	if accountContractChanged {
		var statusErr error
		subscriptionErr, statusErr = a.subscribeAndPersistWhatsAppStatus(
			subscriptionCtx,
			orgID,
			account,
			subscriptionAccount,
			"active",
			"subscription_failed",
		)
		if statusErr != nil {
			a.Log.Error("Failed to persist WhatsApp subscription readiness", "error", statusErr, "account_id", account.ID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Account updated, but webhook subscription status could not be recorded", nil, "")
		}
	}

	// Invalidate cache
	if oldAccount.PhoneID != account.PhoneID {
		a.InvalidateWhatsAppAccountCache(oldAccount.PhoneID)
	}
	a.InvalidateWhatsAppAccountCache(account.PhoneID)

	a.DB.Preload("CreatedBy").Preload("UpdatedBy").
		Where("id = ? AND organization_id = ?", account.ID, orgID).
		First(account)

	var sensitiveChanges []map[string]any
	if tokenChanged {
		sensitiveChanges = append(sensitiveChanges, map[string]any{
			"field": "access_token", "old_value": "********", "new_value": "********",
		})
	}
	a.logAudit(orgID, userID,
		"account", account.ID, models.AuditActionUpdated, &oldAccount, account, sensitiveChanges...)

	response := accountToResponse(*account)
	if subscriptionErr != nil {
		response.Warning = "Webhook subscription failed; use the account Subscribe action after checking Meta permissions"
	}
	return r.SendEnvelope(response)
}

// DeleteAccount deletes a WhatsApp account
func (a *App) DeleteAccount(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceAccounts, models.ActionDelete)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}

	// Get account first for cache invalidation and audit
	account, err := findByIDAndOrg[models.WhatsAppAccount](a.DB, r, id, orgID, "Account")
	if err != nil {
		return nil
	}

	if err := a.DB.Delete(account).Error; err != nil {
		a.Log.Error("Failed to delete account", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete account", nil, "")
	}

	// Invalidate cache
	a.InvalidateWhatsAppAccountCache(account.PhoneID)

	a.logAudit(orgID, userID,
		"account", id, models.AuditActionDeleted, account, nil)

	return r.SendEnvelope(map[string]string{"message": "Account deleted successfully"})
}

// TestAccountConnection tests the WhatsApp API connection
// This validates both PhoneID and BusinessID to ensure all credentials are correct
func (a *App) TestAccountConnection(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceAccounts, models.ActionRead)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}

	account, err := a.resolveWhatsAppAccountByID(r, id, orgID)
	if err != nil {
		return nil
	}

	// Use the comprehensive validation function
	if err := a.validateAccountCredentials(account.PhoneID, account.BusinessID, account.AccessToken, account.APIVersion); err != nil {
		a.Log.Error("Account test failed", "error", err, "account", account.Name)
		return r.SendEnvelope(map[string]any{
			"success": false,
			"error":   fmt.Sprintf("Account credential validation failed: %s", err.Error()),
		})
	}

	// Fetch additional details for display
	phoneURL := fmt.Sprintf("%s/%s/%s?fields=display_phone_number,verified_name,code_verification_status,account_mode,quality_rating,messaging_limit_tier,whatsapp_business_manager_messaging_limit",
		a.Config.WhatsApp.BaseURL, account.APIVersion, account.PhoneID)

	result, status, err := a.fetchMetaJSON(phoneURL, account.AccessToken)
	if err != nil {
		a.Log.Error("Failed to connect to WhatsApp API", "error", err)
		return r.SendEnvelope(map[string]any{
			"success": false,
			"error":   "Failed to connect to WhatsApp API",
		})
	}
	if status != http.StatusOK {
		return r.SendEnvelope(map[string]any{
			"success": false,
			"error":   "API error",
			"details": result,
		})
	}

	// Check if this is a test/sandbox number
	accountMode, _ := result["account_mode"].(string)
	isTestNumber := accountMode == "SANDBOX"

	// Resolve messaging limit tier, falling back to newer portfolio-based field if deprecated field is missing/null
	messagingLimitTier := result["messaging_limit_tier"]
	if messagingLimitTier == nil || messagingLimitTier == "" {
		messagingLimitTier = result["whatsapp_business_manager_messaging_limit"]
	}

	// If still empty/null, query the WABA ID (BusinessID) as a fallback
	if (messagingLimitTier == nil || messagingLimitTier == "") && account.BusinessID != "" {
		wabaURL := fmt.Sprintf("%s/%s/%s?fields=whatsapp_business_manager_messaging_limit",
			a.Config.WhatsApp.BaseURL, account.APIVersion, account.BusinessID)
		wabaResult, wabaStatus, wabaErr := a.fetchMetaJSON(wabaURL, account.AccessToken)
		switch {
		case wabaErr != nil:
			a.Log.Warn("WABA fallback request failed", "waba_id", account.BusinessID, "error", wabaErr)
		case wabaStatus != http.StatusOK:
			a.Log.Warn("WABA fallback returned non-200", "waba_id", account.BusinessID, "status", wabaStatus)
		default:
			if val, ok := wabaResult["whatsapp_business_manager_messaging_limit"]; ok && val != nil && val != "" {
				messagingLimitTier = val
				a.Log.Info("Resolved messaging limit tier from WABA as fallback", "waba_id", account.BusinessID, "limit", val)
			}
		}
	}

	// Prepare response
	response := map[string]any{
		"success":                  true,
		"display_phone_number":     result["display_phone_number"],
		"verified_name":            result["verified_name"],
		"quality_rating":           result["quality_rating"],
		"messaging_limit_tier":     messagingLimitTier,
		"code_verification_status": result["code_verification_status"],
		"account_mode":             result["account_mode"],
		"is_test_number":           isTestNumber,
	}

	// Add warning for test/sandbox numbers or expired verification
	if isTestNumber {
		response["warning"] = "This is a test/sandbox number. Not suitable for production use."
	} else if verificationStatus, ok := result["code_verification_status"].(string); ok && verificationStatus == "EXPIRED" {
		response["warning"] = "Phone verification has expired. Consider re-verifying at: https://business.facebook.com/wa/manage/phone-numbers/"
	}

	return r.SendEnvelope(response)
}

// fetchMetaJSON performs a Bearer-authenticated GET against the Meta Graph API
// and decodes the JSON body into a generic map. The decoded body is returned
// regardless of HTTP status, so callers can surface error envelopes from Meta.
// Returns (nil, 0, err) only when the request itself fails (network/decode).
func (a *App) fetchMetaJSON(url, accessToken string) (map[string]any, int, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	var out map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, resp.StatusCode, err
		}
	}
	return out, resp.StatusCode, nil
}

// Helper functions

func accountToResponse(acc models.WhatsAppAccount) AccountResponse {
	resp := AccountResponse{
		ID:                     acc.ID,
		Name:                   acc.Name,
		PhoneID:                acc.PhoneID,
		BusinessID:             acc.BusinessID,
		APIVersion:             acc.APIVersion,
		IsDefaultIncoming:      acc.IsDefaultIncoming,
		IsDefaultOutgoing:      acc.IsDefaultOutgoing,
		AutoReadReceipt:        acc.AutoReadReceipt,
		BusinessCallingEnabled: acc.BusinessCallingEnabled,
		Status:                 acc.Status,
		HasAccessToken:         acc.AccessToken != "",
		AccessTokenExpiresAt:   acc.AccessTokenExpiresAt,
		CreatedByID:            acc.CreatedByID,
		UpdatedByID:            acc.UpdatedByID,
		CreatedAt:              acc.CreatedAt.Format("2006-01-02T15:04:05Z"),
		UpdatedAt:              acc.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}
	if acc.CreatedBy != nil {
		resp.CreatedByName = acc.CreatedBy.FullName
	}
	if acc.UpdatedBy != nil {
		resp.UpdatedByName = acc.UpdatedBy.FullName
	}
	return resp
}

// validateAccountCredentials validates WhatsApp account credentials with Meta API
func (a *App) validateAccountCredentials(phoneID, businessID, accessToken, apiVersion string) error {
	ctx := context.Background()
	_, err := a.validateWhatsAppAccountContract(ctx, phoneID, businessID, accessToken, apiVersion)
	if err != nil {
		return err
	}
	return nil
}

// validateWhatsAppAccountContract verifies the complete provider-side tuple:
// the token can read the phone and WABA, and the exact Phone ID is listed under
// the supplied WABA. It deliberately does not log or return the access token.
func (a *App) validateWhatsAppAccountContract(
	ctx context.Context,
	phoneID, businessID, accessToken, apiVersion string,
) (*whatsapp.CredentialsValidationResult, error) {
	if a.WhatsApp == nil {
		return nil, errors.New("WhatsApp account validation is unavailable")
	}
	result, err := a.WhatsApp.ValidateCredentials(
		ctx,
		strings.TrimSpace(phoneID),
		strings.TrimSpace(businessID),
		accessToken,
		strings.TrimSpace(apiVersion),
	)
	if err != nil {
		a.Log.Warn(
			"WhatsApp account contract validation failed",
			"phone_id", strings.TrimSpace(phoneID),
			"business_id", strings.TrimSpace(businessID),
		)
		return nil, fmt.Errorf("WhatsApp account validation failed: %w", err)
	}
	a.Log.Info(
		"WhatsApp account contract validated successfully",
		"phone_id", strings.TrimSpace(phoneID),
		"business_id", strings.TrimSpace(businessID),
	)
	return result, nil
}

// subscribeAndPersistWhatsAppStatus subscribes a row that has already been
// saved in a non-ready state, then records the true provider outcome. The
// runtimeAccount contains request-scoped plaintext credentials; account is the
// encrypted-at-rest model and is never used for the provider call.
func (a *App) subscribeAndPersistWhatsAppStatus(
	ctx context.Context,
	orgID uuid.UUID,
	account *models.WhatsAppAccount,
	runtimeAccount *whatsapp.Account,
	successStatus, failureStatus string,
) (subscriptionErr, persistenceErr error) {
	if a.WhatsApp == nil || runtimeAccount == nil {
		subscriptionErr = errors.New("WhatsApp webhook subscription is unavailable")
	} else {
		subscriptionErr = a.WhatsApp.SubscribeApp(ctx, runtimeAccount)
	}

	status := successStatus
	if subscriptionErr != nil {
		status = failureStatus
		a.Log.Warn(
			"WhatsApp webhook subscription failed",
			"account_id", account.ID,
			"business_id", account.BusinessID,
		)
	}
	if err := a.DB.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, orgID).
		Update("status", status).Error; err != nil {
		return subscriptionErr, err
	}
	account.Status = status
	a.InvalidateWhatsAppAccountCache(account.PhoneID)
	return subscriptionErr, nil
}

// SubscribeApp subscribes the app to webhooks for the WhatsApp Business Account.
// This is required after phone number registration to receive incoming messages from Meta.
func (a *App) SubscribeApp(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}

	account, err := a.resolveWhatsAppAccountByID(r, id, orgID)
	if err != nil {
		return nil
	}

	// Subscribe the app to webhooks
	ctx, cancel := context.WithTimeout(requestContext(r), 30*time.Second)
	defer cancel()
	if err := a.WhatsApp.SubscribeApp(ctx, a.toWhatsAppAccount(account)); err != nil {
		a.Log.Warn("Failed to subscribe app to webhooks", "account_id", account.ID)
		if statusErr := a.DB.Model(&models.WhatsAppAccount{}).
			Where("id = ? AND organization_id = ?", account.ID, orgID).
			Update("status", "subscription_failed").Error; statusErr != nil {
			a.Log.Error("Failed to record webhook subscription failure", "error", statusErr, "account_id", account.ID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Webhook subscription failed and its status could not be recorded", nil, "")
		}
		a.InvalidateWhatsAppAccountCache(account.PhoneID)
		return r.SendEnvelope(map[string]any{
			"success": false,
			"error":   "Failed to subscribe app to webhooks. Check your credentials.",
		})
	}

	a.Log.Info("App subscribed to webhooks successfully", "account", account.Name, "business_id", account.BusinessID)
	if account.Status == "subscription_failed" || account.Status == "pending_subscription" {
		if err := a.DB.Model(&models.WhatsAppAccount{}).
			Where("id = ? AND organization_id = ?", account.ID, orgID).
			Update("status", "active").Error; err != nil {
			a.Log.Error("Failed to restore account readiness after subscription", "error", err, "account", account.Name)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Subscription succeeded but account readiness could not be saved", nil, "")
		}
		a.InvalidateWhatsAppAccountCache(account.PhoneID)
	}
	return r.SendEnvelope(map[string]any{
		"success": true,
		"message": "App subscribed to webhooks successfully. You should now receive incoming messages.",
	})
}

// resolveMetaAppCreds resolves Meta app ID, App Secret, and Config ID for an organization,
// preferring organization-specific settings and falling back to global config defaults.
func (a *App) resolveMetaAppCreds(orgID uuid.UUID) (string, string, string, error) {
	// Once an Integration Center row exists, its disabled state is
	// authoritative. Legacy organizations without a row retain the existing
	// global/workspace fallback until an administrator manages this provider in
	// the new center.
	var integration models.ProviderIntegration
	if err := a.DB.Select("enabled").
		Where("organization_id = ? AND provider = ?", orgID, integrationProviderMeta).
		First(&integration).Error; err == nil {
		if !integration.Enabled {
			return "", "", "", errMetaIntegrationDisabled
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", "", "", err
	}

	var org models.Organization
	if err := a.DB.Where("id = ?", orgID).First(&org).Error; err != nil {
		return "", "", "", err
	}

	var appID, appSecret, configID, encryptionKey string
	if a.Config != nil {
		appID = a.Config.WhatsApp.AppID
		appSecret = a.Config.WhatsApp.AppSecret
		configID = a.Config.WhatsApp.ConfigID
		encryptionKey = a.Config.App.EncryptionKey
	}

	if org.Settings != nil {
		if v, ok := org.Settings["meta_app_id"].(string); ok && v != "" {
			appID = v
		}
		if v, ok := org.Settings["meta_config_id"].(string); ok && v != "" {
			configID = v
		}
		if v, ok := org.Settings["meta_app_secret_encrypted"].(string); ok && v != "" {
			if crypto.IsEncrypted(v) &&
				strings.TrimSpace(encryptionKey) == "" {
				return "", "", "", errors.New("Meta credential encryption key is not configured")
			}
			decrypted, err := crypto.Decrypt(v, encryptionKey)
			if err != nil {
				a.Log.Error("Failed to decrypt meta app secret from organization settings", "error", err)
				return "", "", "", errors.New("Meta credential could not be decrypted")
			}
			if strings.TrimSpace(decrypted) == "" {
				return "", "", "", errors.New("Meta credential is empty")
			}
			appSecret = decrypted
		}
	}

	return appID, appSecret, configID, nil
}

// resolveEffectiveMetaAppCreds resolves the live organization-level Meta app
// credentials for an account. Once an Integration Center row exists, that row
// is authoritative (including disabled or incomplete states) and legacy
// account columns are never consulted. Organizations that have never opted in
// retain a read-only fallback to their existing account values.
func (a *App) resolveEffectiveMetaAppCreds(account *models.WhatsAppAccount) (appID, appSecret, configID string, err error) {
	if account == nil || account.OrganizationID == uuid.Nil {
		return "", "", "", errors.New("WhatsApp account organization is required")
	}
	if a.rlsEnabled() && (!a.hasTenantScope() || a.tenantOrgID != account.OrganizationID) {
		err = a.WithTenantApp(account.OrganizationID, func(scoped *App) error {
			var scopedErr error
			appID, appSecret, configID, scopedErr = scoped.resolveEffectiveMetaAppCredsScoped(account)
			return scopedErr
		})
		return appID, appSecret, configID, err
	}
	return a.resolveEffectiveMetaAppCredsScoped(account)
}

func (a *App) resolveEffectiveMetaAppCredsScoped(account *models.WhatsAppAccount) (appID, appSecret, configID string, err error) {
	appID, appSecret, configID, err = a.resolveMetaAppCreds(account.OrganizationID)
	if err != nil {
		return "", "", "", err
	}
	managed, err := a.metaIntegrationManaged(account.OrganizationID)
	if err != nil {
		return "", "", "", err
	}
	if managed {
		return appID, appSecret, configID, nil
	}

	if strings.TrimSpace(appID) == "" {
		appID = strings.TrimSpace(account.AppID)
	}
	if strings.TrimSpace(appSecret) == "" {
		appSecret, err = a.decryptLegacyMetaAccountSecret(account.AppSecret)
		if err != nil {
			return "", "", "", err
		}
	}
	return appID, appSecret, configID, nil
}

func (a *App) metaIntegrationManaged(orgID uuid.UUID) (bool, error) {
	var count int64
	err := a.DB.Model(&models.ProviderIntegration{}).
		Where("organization_id = ? AND provider = ?", orgID, integrationProviderMeta).
		Count(&count).Error
	return count > 0, err
}

func (a *App) decryptLegacyMetaAccountSecret(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !crypto.IsEncrypted(value) {
		return value, nil
	}
	key := a.integrationEncryptionKey()
	if strings.TrimSpace(key) == "" {
		return "", errAccountEncryptionUnavailable
	}
	decrypted, err := crypto.Decrypt(value, key)
	if err != nil {
		return "", errors.New("legacy Meta app credential could not be decrypted")
	}
	return strings.TrimSpace(decrypted), nil
}

// ExchangeToken exchanges the temporary code for a permanent access token and creates the account
func (a *App) ExchangeToken(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
	if err != nil {
		return nil
	}

	var req struct {
		Code               string `json:"code" validate:"required"`
		PhoneID            string `json:"phone_id"` // Optional: Discovered via token if missing
		WABAID             string `json:"waba_id"`  // Optional: Discovered via token if missing
		Name               string `json:"name"`
		WebhookVerifyToken string `json:"webhook_verify_token"`
	}
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	a.Log.Info("Received embedded signup exchange token request",
		"phone_id", req.PhoneID,
		"waba_id", req.WABAID,
		"organization_id", orgID)

	if req.Code == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Code is required", nil, "")
	}
	if strings.TrimSpace(req.WebhookVerifyToken) != "" {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Meta Webhook Verify Token is managed in Settings > Integrations",
			nil,
			"",
		)
	}
	if !a.hasIntegrationEncryptionKey() {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
	}

	// 1. Resolve Meta credentials for this org
	appID, appSecret, _, err := a.resolveMetaAppCreds(orgID)
	if err != nil {
		if errors.Is(err, errMetaIntegrationDisabled) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Meta integration is disabled", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to resolve credentials", nil, "")
	}

	// 2. Exchange code for user access token using WhatsApp service
	ctx, cancel := context.WithTimeout(requestContext(r), 60*time.Second)
	defer cancel()
	a.Log.Info("Exchanging code for access token")

	accessToken, err := a.WhatsApp.ExchangeCodeForToken(ctx, req.Code,
		appID, appSecret, a.Config.WhatsApp.APIVersion)
	if err != nil {
		a.Log.Warn("Meta authorization code exchange was rejected")
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Meta authorization code exchange failed", nil, "")
	}

	// DISCOVERY: If IDs are missing, try to find them using the token
	phoneID, wabaID, name, tokenExpiresAt, err := a.discoverWABAAndPhone(ctx, orgID, accessToken, req.PhoneID, req.WABAID, req.Name)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	// 3. We can now create/update the account
	account, phoneInfo, existingAccount, oldAccount, err := a.createOrUpdateAccount(ctx, orgID, phoneID, wabaID, name, accessToken, tokenExpiresAt)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, err.Error(), nil, "")
	}

	// 4. Attempt Auto-Registration
	var priorStatus string
	if oldAccount != nil {
		priorStatus = oldAccount.Status
	}
	regErr := a.attemptAutoRegistration(ctx, account, phoneInfo, accessToken, priorStatus)

	// 5. Persist the account in a non-ready state before attempting the remote
	// subscription. This ensures a successful provider-side subscription always
	// has a local row that can be reconciled or retried.
	registrationStatus := account.Status
	subscriptionSuccessStatus := registrationStatus
	subscriptionFailureStatus := registrationStatus
	if regErr == nil && registrationStatus == "active" {
		account.Status = "pending_subscription"
		subscriptionSuccessStatus = "active"
		subscriptionFailureStatus = "subscription_failed"
	}
	subscriptionAccount := a.toWhatsAppAccount(account)

	// 6. Encrypt credentials at rest
	plaintextPin := account.Pin
	if err := a.encryptAccountSecrets(account); err != nil {
		a.Log.Error("Failed to encrypt account secrets", "error", err)
		if errors.Is(err, errAccountEncryptionUnavailable) {
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, err.Error(), nil, "")
	}

	if err := a.DB.Save(account).Error; err != nil {
		a.Log.Error("Failed to save account", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to save account", nil, "")
	}

	// 7. Subscribe and persist the true readiness outcome. Registration failures
	// remain pending_registration; otherwise subscription success/failure is
	// represented as active/subscription_failed.
	subscriptionErr, statusErr := a.subscribeAndPersistWhatsAppStatus(
		ctx,
		orgID,
		account,
		subscriptionAccount,
		subscriptionSuccessStatus,
		subscriptionFailureStatus,
	)
	if statusErr != nil {
		a.Log.Error("Failed to persist embedded signup subscription readiness", "error", statusErr, "account_id", account.ID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Account saved, but webhook subscription status could not be recorded", nil, "")
	}

	// Invalidate cache
	a.InvalidateWhatsAppAccountCache(account.PhoneID)

	a.Log.Info("WhatsApp account connected via embedded signup successfully",
		"account_id", account.ID,
		"phone_id", account.PhoneID,
		"status", account.Status)

	// Audit Logging
	a.DB.Preload("CreatedBy").Preload("UpdatedBy").
		Where("id = ? AND organization_id = ?", account.ID, orgID).
		First(account)
	auditAction := models.AuditActionCreated
	var auditOld any = nil
	if existingAccount {
		auditAction = models.AuditActionUpdated
		auditOld = oldAccount
	}
	a.logAudit(orgID, userID,
		"account", account.ID, auditAction, auditOld, account)

	// Construction of response map (reusing accountToResponse)
	out := map[string]any{
		"account": accountToResponse(*account),
	}
	if account.Status == "active" && plaintextPin != "" {
		out["pin"] = plaintextPin
	}
	warnings := make([]string, 0, 2)
	if regErr != nil {
		warnings = append(warnings, "Registration failed: "+regErr.Error())
	}
	if subscriptionErr != nil {
		warnings = append(warnings, "Webhook subscription failed; use the account Subscribe action after checking Meta permissions")
	}
	if len(warnings) > 0 {
		out["warning"] = strings.Join(warnings, "; ")
	}

	return r.SendEnvelope(out)
}

func (a *App) discoverWABAAndPhone(ctx context.Context, orgID uuid.UUID, accessToken, phoneID, wabaID, name string) (string, string, string, *time.Time, error) {
	a.Log.Info("Validating embedded signup token via debug_token")

	debugInfo, tokenExpiresAt, err := a.debugAndValidateMetaAccessToken(ctx, orgID, accessToken)
	if err != nil {
		return "", "", "", nil, err
	}
	if wabaID == "" {
		// 2. Find WABA ID from Granular Scopes only when Embedded Signup did
		// not already provide one. A supplied ID is still validated below.
		var discoveredWABAID string
		for _, scope := range debugInfo.GranularScopes {
			if scope.Scope == "whatsapp_business_management" {
				if len(scope.TargetIds) > 0 {
					discoveredWABAID = scope.TargetIds[0]
					break
				}
			}
		}

		if discoveredWABAID == "" {
			a.Log.Warn("No WABA ID found in granular scopes, falling back to /me/accounts strategy")
			sharedInfo, err := a.WhatsApp.GetSharedWABA(ctx, accessToken)
			if err == nil && len(sharedInfo.Data) > 0 {
				discoveredWABAID = sharedInfo.Data[0].ID
			}
		}

		if discoveredWABAID == "" {
			return "", "", "", nil, fmt.Errorf("could not discover WhatsApp Business Account ID from token")
		}

		wabaID = discoveredWABAID
		a.Log.Info("Discovered WABA ID", "waba_id", wabaID)
	}

	if phoneID == "" {
		phonesResp, err := a.WhatsApp.GetWABAPhoneNumbers(ctx, wabaID, accessToken)
		if err != nil {
			a.Log.Error("Failed to fetch phone numbers from Meta", "error", err)
			return "", "", "", nil, fmt.Errorf("failed to fetch phone numbers from WABA: %w", err)
		}

		if len(phonesResp.Data) == 0 {
			return "", "", "", nil, fmt.Errorf("no phone numbers found in this WhatsApp Business Account")
		}

		if len(phonesResp.Data) > 1 {
			a.Log.Warn("Multiple phone numbers discovered in WABA; picking the first one", "count", len(phonesResp.Data))
		}

		// User selects only ONE account in the flow, so we take the first one found.
		phone := phonesResp.Data[0]
		phoneID = phone.ID
		name = fmt.Sprintf("%s (%s)", phone.VerifiedName, phone.DisplayPhoneNumber)
		a.Log.Info("Discovered Phone ID", "phone_id", phoneID)
	}

	// Even when the browser supplied both IDs, verify the token can read each
	// object and that Meta lists this exact Phone ID under this exact WABA.
	// This prevents Embedded Signup payloads from bypassing the same contract
	// enforced by manual account creation and credential updates.
	if _, err := a.validateWhatsAppAccountContract(
		ctx,
		phoneID,
		wabaID,
		accessToken,
		a.defaultAPIVersion(),
	); err != nil {
		return "", "", "", nil, err
	}

	return phoneID, wabaID, name, tokenExpiresAt, nil
}

func (a *App) debugAndValidateMetaAccessToken(
	ctx context.Context,
	orgID uuid.UUID,
	accessToken string,
) (*whatsapp.TokenDebugInfo, *time.Time, error) {
	appID, appSecret, _, err := a.resolveMetaAppCreds(orgID)
	if err != nil {
		if errors.Is(err, errMetaIntegrationDisabled) {
			return nil, nil, err
		}
		a.Log.Error("Failed to resolve Meta integration for access token validation")
		return nil, nil, errMetaTokenValidationUnavailable
	}
	if strings.TrimSpace(appID) == "" {
		return nil, nil, errMetaAppIDNotConfigured
	}
	if strings.TrimSpace(appSecret) == "" {
		return nil, nil, errMetaAppSecretNotConfigured
	}
	if a.WhatsApp == nil {
		return nil, nil, errMetaTokenValidationUnavailable
	}

	debugInfo, err := a.WhatsApp.GetTokenDebugInfo(
		ctx,
		accessToken,
		fmt.Sprintf("%s|%s", appID, appSecret),
	)
	if err != nil {
		// GetTokenDebugInfo must place input_token in Meta's required query
		// parameter. Transport errors can echo that URL, so never attach the raw
		// error to logs or client responses.
		a.Log.Warn("Meta access token validation request failed")
		return nil, nil, errMetaTokenValidationUnavailable
	}
	tokenExpiresAt, err := validateMetaEmbeddedSignupToken(
		debugInfo,
		appID,
		time.Now().UTC(),
	)
	if err != nil {
		return nil, nil, err
	}
	return debugInfo, tokenExpiresAt, nil
}

func (a *App) sendMetaTokenPreflightError(r *fastglue.Request, err error) error {
	switch {
	case errors.Is(err, errMetaIntegrationDisabled):
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Meta integration is disabled", nil, "")
	case errors.Is(err, errMetaAppIDNotConfigured),
		errors.Is(err, errMetaAppSecretNotConfigured),
		errors.Is(err, errMetaTokenValidationUnavailable):
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Meta access token validation is unavailable; check Settings > Integrations",
			nil,
			"",
		)
	default:
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
}

func validateMetaEmbeddedSignupToken(
	info *whatsapp.TokenDebugInfo,
	expectedAppID string,
	now time.Time,
) (*time.Time, error) {
	if info == nil || !info.IsValid {
		return nil, errors.New("Meta returned an invalid embedded signup token")
	}
	if strings.TrimSpace(info.AppID) == "" || strings.TrimSpace(info.AppID) != strings.TrimSpace(expectedAppID) {
		return nil, errors.New("The embedded signup token was issued to a different Meta app")
	}
	scopes := make(map[string]struct{}, len(info.Scopes)+len(info.GranularScopes))
	for _, scope := range info.Scopes {
		scopes[strings.TrimSpace(scope)] = struct{}{}
	}
	for _, scope := range info.GranularScopes {
		scopes[strings.TrimSpace(scope.Scope)] = struct{}{}
	}
	for _, required := range []string{
		"business_management",
		"whatsapp_business_management",
		"whatsapp_business_messaging",
	} {
		if _, ok := scopes[required]; !ok {
			return nil, fmt.Errorf("The embedded signup token is missing the required %s permission", required)
		}
	}

	var earliest *time.Time
	for _, unixSeconds := range []int64{info.ExpiresAt, info.DataAccessExpiresAt} {
		if unixSeconds <= 0 {
			continue
		}
		candidate := time.Unix(unixSeconds, 0).UTC()
		if !candidate.After(now.UTC()) {
			return nil, errors.New("The embedded signup token or its data access has expired")
		}
		if earliest == nil || candidate.Before(*earliest) {
			value := candidate
			earliest = &value
		}
	}
	return earliest, nil
}

func (a *App) createOrUpdateAccount(ctx context.Context, orgID uuid.UUID, phoneID, wabaID, name, accessToken string, tokenExpiresAt *time.Time) (*models.WhatsAppAccount, *whatsapp.PhoneNumberInfo, bool, *models.WhatsAppAccount, error) {
	var account models.WhatsAppAccount
	var existingAccount bool
	var oldAccount *models.WhatsAppAccount

	// Use Unscoped to find even soft-deleted accounts to avoid unique constraint violations
	if err := a.DB.Unscoped().Where("phone_id = ? AND organization_id = ?", phoneID, orgID).First(&account).Error; err == nil {
		existingAccount = true
		temp := account
		oldAccount = &temp
	}

	// Fetch phone info from Meta using WhatsApp service unconditionally
	phoneInfo, err := a.WhatsApp.GetPhoneNumberInfo(ctx, phoneID, accessToken, a.Config.WhatsApp.APIVersion)
	if err != nil {
		a.Log.Warn("Failed to fetch phone info from Meta", "error", err)
	}

	if name == "" {
		if err == nil && phoneInfo != nil && phoneInfo.VerifiedName != "" {
			suffixPIN, err := generateNumericPIN(4)
			if err != nil {
				return nil, nil, false, nil, fmt.Errorf("failed to generate security identifier: %w", err)
			}
			name = fmt.Sprintf("%s %s", phoneInfo.VerifiedName, suffixPIN)
		} else {
			// Safe substring handling
			suffix := phoneID
			if len(phoneID) > 4 {
				suffix = phoneID[len(phoneID)-4:]
			}
			name = "WhatsApp Account " + suffix
		}
	}

	var isSMB bool
	if phoneInfo != nil {
		if phoneInfo.IsOnBizApp || phoneInfo.PlatformType == "SMB" || phoneInfo.PlatformType == "SMB_CLOUD_API" {
			isSMB = true
		}
	}

	account.OrganizationID = orgID
	account.Name = name
	account.PhoneID = phoneID
	account.BusinessID = wabaID
	account.AccessToken = accessToken
	account.AccessTokenExpiresAt = tokenExpiresAt
	if !existingAccount {
		account.Status = "pending_registration"
	}
	account.IsSMB = isSMB

	// App ID and App Secret remain organization-level Integration Center
	// settings. Existing legacy values are left untouched, while new Embedded
	// Signup rows deliberately keep both account columns empty.
	if account.APIVersion == "" {
		account.APIVersion = a.defaultAPIVersion()
	}

	if !existingAccount {
		account.IsDefaultIncoming = false
		account.IsDefaultOutgoing = false
		account.AutoReadReceipt = false
	}

	return &account, phoneInfo, existingAccount, oldAccount, nil
}

func (a *App) attemptAutoRegistration(ctx context.Context, account *models.WhatsAppAccount, phoneInfo *whatsapp.PhoneNumberInfo, accessToken, priorStatus string) error {
	if account.IsSMB {
		account.Status = "active"
		account.Pin = ""
		a.Log.Info("SMB account detected via Meta API, skipped registration, setting to active", "phone_id", account.PhoneID)
		return nil
	}

	generatedPin, err := generateNumericPIN(6)
	if err != nil {
		return fmt.Errorf("failed to generate secure random PIN: %w", err)
	}

	a.Log.Info("Attempting phone number auto-registration", "phone_id", account.PhoneID)
	regErr := a.WhatsApp.RegisterPhoneNumber(ctx, account.PhoneID, generatedPin, accessToken, account.APIVersion)

	if regErr == nil {
		account.Status = "active"
		account.Pin = generatedPin
		a.Log.Info("Phone number auto-registration successful", "phone_id", account.PhoneID)
	} else {
		a.Log.Warn("Phone number auto-registration failed",
			"error", regErr,
			"phone_id", account.PhoneID)
		if priorStatus != "" {
			account.Status = priorStatus
		} else {
			account.Status = "pending_registration"
		}
	}

	return regErr
}

// RegisterPhoneNumber registers the phone number with Two-Step Verification
func (a *App) RegisterPhoneNumber(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}

	var req struct {
		Pin string `json:"pin"` // Optional custom PIN
	}
	_ = r.Decode(&req, "json")

	account, err := a.resolveWhatsAppAccountByID(r, id, orgID)
	if err != nil {
		return nil
	}

	oldAccount := *account

	// If PIN is not provided, generate a random one
	pin := req.Pin
	if pin == "" {
		var err error
		pin, err = generateNumericPIN(6)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to generate secure random PIN", nil, "")
		}
	}

	ctx, cancel := context.WithTimeout(requestContext(r), 30*time.Second)
	defer cancel()

	// Check if this is an SMB phone — SMB numbers are already registered
	// via the Business App and don't support the two-step registration API.
	if account.IsSMB {
		a.Log.Info("Manual registration: SMB account detected, skipping registration", "phone_id", account.PhoneID)
		pin = ""
	} else {
		// Call Meta Register endpoint using WhatsApp service
		if err := a.WhatsApp.RegisterPhoneNumber(ctx, account.PhoneID, pin, account.AccessToken, account.APIVersion); err != nil {
			a.Log.Error("Manual registration failed", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
		}
	}

	// Success
	account.Status = "active"
	account.Pin = pin

	// Encrypt secrets before saving
	if err := a.encryptAccountSecrets(account); err != nil {
		a.Log.Error("Failed to encrypt account secrets", "error", err)
		if errors.Is(err, errAccountEncryptionUnavailable) {
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, err.Error(), nil, "")
	}

	if err := a.DB.Save(account).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update account status", nil, "")
	}

	// Invalidate cache
	a.InvalidateWhatsAppAccountCache(account.PhoneID)

	// Log audit!
	a.DB.Preload("CreatedBy").Preload("UpdatedBy").First(account, "id = ?", account.ID)
	a.logAudit(orgID, userID,
		"account", account.ID, models.AuditActionUpdated, &oldAccount, account)

	return r.SendEnvelope(map[string]interface{}{
		"success": true,
		"message": "Phone number registered successfully",
		"pin":     pin,
	})
}

func generateNumericPIN(length int) (string, error) {
	b := make([]byte, length)
	for i := 0; i < length; i++ {
		num, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b[i] = byte(num.Int64()) + '0'
	}
	return string(b), nil
}

func (a *App) defaultAPIVersion() string {
	if a.Config.WhatsApp.APIVersion != "" {
		return a.Config.WhatsApp.APIVersion
	}
	return "v21.0"
}

func (a *App) encryptAccountSecrets(account *models.WhatsAppAccount) error {
	if account == nil {
		return errors.New("account is required")
	}
	encryptionKey := a.integrationEncryptionKey()
	if strings.TrimSpace(encryptionKey) == "" {
		for _, value := range []string{account.AccessToken, account.AppSecret, account.Pin} {
			if strings.TrimSpace(value) != "" && !crypto.IsEncrypted(value) {
				return errAccountEncryptionUnavailable
			}
		}
	}
	return crypto.EncryptFields(encryptionKey,
		&account.AccessToken, &account.AppSecret, &account.Pin)
}
