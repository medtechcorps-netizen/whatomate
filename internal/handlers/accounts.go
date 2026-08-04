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
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	errMetaIntegrationDisabled         = errors.New("meta integration is disabled")
	errAccountEncryptionUnavailable    = errors.New("account credential encryption is unavailable")
	errMetaAppIDNotConfigured          = errors.New("meta app ID is not configured")
	errMetaAppSecretNotConfigured      = errors.New("meta app secret is not configured")
	errMetaTokenValidationUnavailable  = errors.New("meta access token validation is unavailable")
	errEmbeddedSignupAlreadyInProgress = errors.New("a WhatsApp connection attempt is already in progress")
	errEmbeddedSignupAttemptSuperseded = errors.New("the WhatsApp connection attempt was superseded")
	errWhatsAppDeletePhoneChanged      = errors.New("WhatsApp phone changed while deletion was being fenced")
)

const (
	embeddedSignupEventFinish      = "FINISH"
	embeddedSignupEventCoexistence = "FINISH_WHATSAPP_BUSINESS_APP_ONBOARDING"

	globalWhatsAppPhoneIndex   = "uq_whatsapp_accounts_phone_id_active"
	embeddedSignupAttemptLease = 2 * time.Minute
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
	req.Name = strings.TrimSpace(req.Name)
	req.PhoneID = strings.TrimSpace(req.PhoneID)
	req.BusinessID = strings.TrimSpace(req.BusinessID)
	req.AccessToken = strings.TrimSpace(req.AccessToken)

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

	// Commit the non-ready row before subscribing at Meta. The HTTP tenant
	// wrapper may still own a request-wide transaction, so provider work must not
	// depend on that transaction committing after the remote mutation succeeds.
	attemptID := uuid.New()
	if err := a.createWhatsAppAccountWithAttempt(orgID, &account, attemptID, func(tx *gorm.DB) error {
		if req.IsDefaultIncoming {
			if err := tx.Model(&models.WhatsAppAccount{}).
				Where("organization_id = ? AND is_default_incoming = ?", orgID, true).
				Update("is_default_incoming", false).Error; err != nil {
				return err
			}
		}
		if req.IsDefaultOutgoing {
			if err := tx.Model(&models.WhatsAppAccount{}).
				Where("organization_id = ? AND is_default_outgoing = ?", orgID, true).
				Update("is_default_outgoing", false).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		a.Log.Error("Failed to create account", "error", err)
		if isWhatsAppPhoneOwnershipConflict(err) {
			return sendWhatsAppPhoneOwnershipConflict(r)
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create account", nil, "")
	}
	a.InvalidateWhatsAppAccountCache(account.PhoneID)

	subscriptionErr, statusErr := a.subscribeAndPersistWhatsAppStatus(
		validationCtx,
		orgID,
		&account,
		subscriptionAccount,
		attemptID,
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
	req.Name = strings.TrimSpace(req.Name)
	req.PhoneID = strings.TrimSpace(req.PhoneID)
	req.BusinessID = strings.TrimSpace(req.BusinessID)
	req.AccessToken = strings.TrimSpace(req.AccessToken)
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

	// Keep default flips and the account write atomic. Contract changes use an
	// independently committed transaction because they are followed by a Meta
	// subscription mutation.
	unsetDefaultIncoming := req.IsDefaultIncoming && !account.IsDefaultIncoming
	unsetDefaultOutgoing := req.IsDefaultOutgoing && !account.IsDefaultOutgoing
	account.IsDefaultIncoming = req.IsDefaultIncoming
	account.IsDefaultOutgoing = req.IsDefaultOutgoing
	account.UpdatedByID = &userID
	var subscriptionAccount *whatsapp.Account
	if accountContractChanged {
		account.Status = "pending_subscription"
		subscriptionAccount = a.toWhatsAppAccount(account)
	}

	// Contract changes persist credentials and therefore require encryption.
	// Safe profile/default edits use a field whitelist below and never rewrite
	// decrypted secrets or the provider-operation lease.
	if accountContractChanged {
		if err := a.encryptAccountSecrets(account); err != nil {
			a.Log.Error("Failed to encrypt account secrets", "error", err)
			if errors.Is(err, errAccountEncryptionUnavailable) {
				return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
			}
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update account", nil, "")
		}
	}

	persistDefaultFlips := func(tx *gorm.DB) error {
		if unsetDefaultIncoming {
			if err := tx.Model(&models.WhatsAppAccount{}).
				Where("organization_id = ? AND is_default_incoming = ?", orgID, true).
				Update("is_default_incoming", false).Error; err != nil {
				return err
			}
		}
		if unsetDefaultOutgoing {
			if err := tx.Model(&models.WhatsAppAccount{}).
				Where("organization_id = ? AND is_default_outgoing = ?", orgID, true).
				Update("is_default_outgoing", false).Error; err != nil {
				return err
			}
		}
		return nil
	}
	attemptID := uuid.Nil
	var persistErr error
	if accountContractChanged {
		attemptID = uuid.New()
		_, persistErr = a.beginWhatsAppAccountAttempt(
			orgID,
			account.ID,
			attemptID,
			whatsAppAccountAttemptOptions{
				ExpectedPhoneID:   oldAccount.PhoneID,
				ExpectedUpdatedAt: oldAccount.UpdatedAt,
				LockPhoneIDs:      []string{account.PhoneID},
				BeforeUpdate:      persistDefaultFlips,
				Updates: map[string]any{
					"name":                     account.Name,
					"phone_id":                 account.PhoneID,
					"business_id":              account.BusinessID,
					"access_token":             account.AccessToken,
					"access_token_expires_at":  account.AccessTokenExpiresAt,
					"api_version":              account.APIVersion,
					"is_default_incoming":      account.IsDefaultIncoming,
					"is_default_outgoing":      account.IsDefaultOutgoing,
					"auto_read_receipt":        account.AutoReadReceipt,
					"business_calling_enabled": account.BusinessCallingEnabled,
					"status":                   account.Status,
					"updated_by_id":            account.UpdatedByID,
				},
			},
		)
		if persistErr == nil {
			account.ConnectionAttemptID = &attemptID
			startedAt := time.Now().UTC()
			account.ConnectionAttemptStartedAt = &startedAt
		}
	} else {
		if persistErr = persistDefaultFlips(a.DB); persistErr == nil {
			result := a.DB.Model(&models.WhatsAppAccount{}).
				Where("id = ? AND organization_id = ? AND deleted_at IS NULL", account.ID, orgID).
				Updates(map[string]any{
					"name":                     account.Name,
					"is_default_incoming":      account.IsDefaultIncoming,
					"is_default_outgoing":      account.IsDefaultOutgoing,
					"auto_read_receipt":        account.AutoReadReceipt,
					"business_calling_enabled": account.BusinessCallingEnabled,
					"updated_by_id":            account.UpdatedByID,
				})
			persistErr = result.Error
			if persistErr == nil && result.RowsAffected != 1 {
				persistErr = gorm.ErrRecordNotFound
			}
		}
	}
	if persistErr != nil {
		a.Log.Error("Failed to update account", "error", persistErr)
		if errors.Is(persistErr, errEmbeddedSignupAlreadyInProgress) ||
			errors.Is(persistErr, errEmbeddedSignupAttemptSuperseded) {
			return sendWhatsAppAttemptConflict(r)
		}
		if isWhatsAppPhoneOwnershipConflict(persistErr) {
			return sendWhatsAppPhoneOwnershipConflict(r)
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update account", nil, "")
	}

	var subscriptionErr error
	if accountContractChanged {
		if oldAccount.PhoneID != account.PhoneID {
			a.InvalidateWhatsAppAccountCache(oldAccount.PhoneID)
		}
		a.InvalidateWhatsAppAccountCache(account.PhoneID)
		var statusErr error
		subscriptionErr, statusErr = a.subscribeAndPersistWhatsAppStatus(
			subscriptionCtx,
			orgID,
			account,
			subscriptionAccount,
			attemptID,
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

	deletedPhoneID, err := a.deleteWhatsAppAccountIndependent(orgID, account.ID, account.PhoneID)
	if err != nil {
		a.Log.Error("Failed to delete account", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete account", nil, "")
	}

	// Invalidate cache
	a.InvalidateWhatsAppAccountCache(account.PhoneID)
	if deletedPhoneID != account.PhoneID {
		a.InvalidateWhatsAppAccountCache(deletedPhoneID)
	}

	a.logAudit(orgID, userID,
		"account", id, models.AuditActionDeleted, account, nil)

	return r.SendEnvelope(map[string]string{"message": "Account deleted successfully"})
}

// deleteWhatsAppAccountIndependent atomically revokes any provider-mutation
// lease before soft-deleting the row. The update takes the row lock, so an
// older leased finalization either completes before the delete or loses its
// attempt predicate after this transaction commits.
func (a *App) deleteWhatsAppAccountIndependent(
	orgID, accountID uuid.UUID,
	expectedPhoneID string,
) (string, error) {
	phoneToLock := strings.TrimSpace(expectedPhoneID)
	for attempt := 0; attempt < 4; attempt++ {
		currentPhoneID := ""
		err := a.withIndependentAccountTenant(orgID, func(tx *gorm.DB) error {
			if err := lockWhatsAppPhoneAttempts(tx, phoneToLock); err != nil {
				return err
			}
			var current models.WhatsAppAccount
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Select("id", "phone_id").
				Where("id = ? AND organization_id = ?", accountID, orgID).
				First(&current).Error; err != nil {
				return err
			}
			currentPhoneID = current.PhoneID
			if phoneToLock != currentPhoneID {
				// Retry without holding the row lock so every account mutation keeps
				// the global ordering: phone advisory lock, then account row lock.
				return errWhatsAppDeletePhoneChanged
			}

			result := tx.Model(&models.WhatsAppAccount{}).
				Where("id = ? AND organization_id = ?", accountID, orgID).
				Updates(map[string]any{
					"connection_attempt_id":         nil,
					"connection_attempt_started_at": nil,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return gorm.ErrRecordNotFound
			}

			result = tx.Delete(
				&models.WhatsAppAccount{},
				"id = ? AND organization_id = ?",
				accountID,
				orgID,
			)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return gorm.ErrRecordNotFound
			}
			return nil
		})
		if errors.Is(err, errWhatsAppDeletePhoneChanged) {
			phoneToLock = currentPhoneID
			continue
		}
		return currentPhoneID, err
	}
	return "", errEmbeddedSignupAttemptSuperseded
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
	attemptID uuid.UUID,
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
	if err := a.finishWhatsAppAccountAttempt(
		orgID,
		account.ID,
		attemptID,
		map[string]any{"status": status},
	); err != nil {
		return subscriptionErr, err
	}
	account.Status = status
	account.ConnectionAttemptID = nil
	account.ConnectionAttemptStartedAt = nil
	a.InvalidateWhatsAppAccountCache(account.PhoneID)
	return subscriptionErr, nil
}

type whatsAppAccountAttemptOptions struct {
	ExpectedPhoneID   string
	ExpectedUpdatedAt time.Time
	LockPhoneIDs      []string
	Updates           map[string]any
	BeforeUpdate      func(*gorm.DB) error
}

func (a *App) createWhatsAppAccountWithAttempt(
	orgID uuid.UUID,
	account *models.WhatsAppAccount,
	attemptID uuid.UUID,
	beforeCreate func(*gorm.DB) error,
) error {
	if account == nil {
		return errors.New("WhatsApp account is required")
	}
	if attemptID == uuid.Nil {
		return errors.New("WhatsApp connection attempt ID is required")
	}
	now := time.Now().UTC()
	return a.withIndependentAccountTenant(orgID, func(tx *gorm.DB) error {
		if err := lockWhatsAppPhoneAttempts(tx, account.PhoneID); err != nil {
			return err
		}
		if beforeCreate != nil {
			if err := beforeCreate(tx); err != nil {
				return err
			}
		}
		account.ConnectionAttemptID = &attemptID
		account.ConnectionAttemptStartedAt = &now
		return tx.Create(account).Error
	})
}

// beginWhatsAppAccountAttempt locks the phone ownership and account row,
// rejects a live operation lease, then commits a whitelisted pending state.
// Every handler that will mutate Meta must call this before the provider call.
func (a *App) beginWhatsAppAccountAttempt(
	orgID, accountID, attemptID uuid.UUID,
	options whatsAppAccountAttemptOptions,
) (*models.WhatsAppAccount, error) {
	if accountID == uuid.Nil || attemptID == uuid.Nil {
		return nil, errors.New("WhatsApp account and attempt IDs are required")
	}
	now := time.Now().UTC()
	var previous models.WhatsAppAccount
	err := a.withIndependentAccountTenant(orgID, func(tx *gorm.DB) error {
		phoneIDs := append([]string(nil), options.LockPhoneIDs...)
		phoneIDs = append(phoneIDs, options.ExpectedPhoneID)
		if err := lockWhatsAppPhoneAttempts(tx, phoneIDs...); err != nil {
			return err
		}

		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", accountID, orgID).
			First(&previous).Error; err != nil {
			return err
		}
		if options.ExpectedPhoneID != "" && previous.PhoneID != options.ExpectedPhoneID {
			return errEmbeddedSignupAttemptSuperseded
		}
		if !options.ExpectedUpdatedAt.IsZero() &&
			!previous.UpdatedAt.Equal(options.ExpectedUpdatedAt) {
			return errEmbeddedSignupAttemptSuperseded
		}
		if hasLiveWhatsAppAccountAttempt(&previous, now) {
			return errEmbeddedSignupAlreadyInProgress
		}
		if options.BeforeUpdate != nil {
			if err := options.BeforeUpdate(tx); err != nil {
				return err
			}
		}

		updates := make(map[string]any, len(options.Updates)+2)
		for key, value := range options.Updates {
			updates[key] = value
		}
		updates["connection_attempt_id"] = attemptID
		updates["connection_attempt_started_at"] = now
		result := tx.Model(&models.WhatsAppAccount{}).
			Where("id = ? AND organization_id = ? AND deleted_at IS NULL", accountID, orgID).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errEmbeddedSignupAttemptSuperseded
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &previous, nil
}

func (a *App) finishWhatsAppAccountAttempt(
	orgID, accountID, attemptID uuid.UUID,
	fields map[string]any,
) error {
	if accountID == uuid.Nil || attemptID == uuid.Nil {
		return errors.New("WhatsApp account and attempt IDs are required")
	}
	return a.withIndependentAccountTenant(orgID, func(tx *gorm.DB) error {
		updates := make(map[string]any, len(fields)+2)
		for key, value := range fields {
			updates[key] = value
		}
		updates["connection_attempt_id"] = nil
		updates["connection_attempt_started_at"] = nil
		result := tx.Model(&models.WhatsAppAccount{}).
			Where(
				"id = ? AND organization_id = ? AND connection_attempt_id = ? AND deleted_at IS NULL",
				accountID,
				orgID,
				attemptID,
			).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errEmbeddedSignupAttemptSuperseded
		}
		return nil
	})
}

func hasLiveWhatsAppAccountAttempt(account *models.WhatsAppAccount, now time.Time) bool {
	if account == nil || account.ConnectionAttemptID == nil {
		return false
	}
	return account.ConnectionAttemptStartedAt == nil ||
		account.ConnectionAttemptStartedAt.After(now.Add(-embeddedSignupAttemptLease))
}

func lockWhatsAppPhoneAttempts(tx *gorm.DB, phoneIDs ...string) error {
	unique := make(map[string]struct{}, len(phoneIDs))
	for _, phoneID := range phoneIDs {
		phoneID = strings.TrimSpace(phoneID)
		if phoneID != "" {
			unique[phoneID] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(unique))
	for phoneID := range unique {
		ordered = append(ordered, phoneID)
	}
	sort.Strings(ordered)
	for _, phoneID := range ordered {
		if err := tx.Exec(
			"SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
			phoneID,
		).Error; err != nil {
			return fmt.Errorf("lock WhatsApp phone attempt: %w", err)
		}
	}
	return nil
}

func isWhatsAppPhoneOwnershipConflict(err error) bool {
	if !isUniqueViolation(err) {
		return false
	}
	errorText := strings.ToLower(err.Error())
	return strings.Contains(errorText, globalWhatsAppPhoneIndex) ||
		strings.Contains(errorText, "idx_whatsapp_accounts_org_phone")
}

func sendWhatsAppPhoneOwnershipConflict(r *fastglue.Request) error {
	return r.SendErrorEnvelope(
		fasthttp.StatusConflict,
		"This WhatsApp phone number is already connected to a ReReply workspace. Disconnect it there before connecting it to another workspace.",
		nil,
		"",
	)
}

func sendWhatsAppAttemptConflict(r *fastglue.Request) error {
	return r.SendErrorEnvelope(
		fasthttp.StatusConflict,
		"Another WhatsApp connection, registration, or subscription attempt is already in progress. Wait a moment and retry.",
		nil,
		"",
	)
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

	// Make the non-ready state durable before mutating the provider. This action
	// can run inside the request-wide tenant transaction, which must not own the
	// checkpoint that makes an interrupted subscription visible to recovery.
	runtimeAccount := a.toWhatsAppAccount(account)
	attemptID := uuid.New()
	_, err = a.beginWhatsAppAccountAttempt(
		orgID,
		account.ID,
		attemptID,
		whatsAppAccountAttemptOptions{
			ExpectedPhoneID:   account.PhoneID,
			ExpectedUpdatedAt: account.UpdatedAt,
			Updates:           map[string]any{"status": "pending_subscription"},
		},
	)
	if err != nil {
		a.Log.Error("Failed to persist pending webhook subscription", "error", err, "account_id", account.ID)
		if errors.Is(err, errEmbeddedSignupAlreadyInProgress) ||
			errors.Is(err, errEmbeddedSignupAttemptSuperseded) {
			return sendWhatsAppAttemptConflict(r)
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Webhook subscription could not be started", nil, "")
	}
	account.Status = "pending_subscription"
	account.ConnectionAttemptID = &attemptID
	a.InvalidateWhatsAppAccountCache(account.PhoneID)

	// Subscribe the app to webhooks, then commit the provider outcome in a
	// second independent tenant transaction.
	ctx, cancel := context.WithTimeout(requestContext(r), 30*time.Second)
	defer cancel()
	subscriptionErr, statusErr := a.subscribeAndPersistWhatsAppStatus(
		ctx,
		orgID,
		account,
		runtimeAccount,
		attemptID,
		"active",
		"subscription_failed",
	)
	if statusErr != nil {
		a.Log.Error("Failed to record webhook subscription outcome", "error", statusErr, "account_id", account.ID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Webhook subscription outcome could not be recorded", nil, "")
	}
	if subscriptionErr != nil {
		a.Log.Warn("Failed to subscribe app to webhooks", "account_id", account.ID)
		return r.SendEnvelope(map[string]any{
			"success": false,
			"error":   "Failed to subscribe app to webhooks. Check your credentials.",
		})
	}

	a.Log.Info("App subscribed to webhooks successfully", "account", account.Name, "business_id", account.BusinessID)
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
				return "", "", "", errors.New("meta credential encryption key is not configured")
			}
			decrypted, err := crypto.Decrypt(v, encryptionKey)
			if err != nil {
				a.Log.Error("Failed to decrypt meta app secret from organization settings", "error", err)
				return "", "", "", errors.New("meta credential could not be decrypted")
			}
			if strings.TrimSpace(decrypted) == "" {
				return "", "", "", errors.New("meta credential is empty")
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
		PhoneID            string `json:"phone_id"`
		PhoneNumberID      string `json:"phone_number_id"`
		WABAID             string `json:"waba_id"`
		SignupEvent        string `json:"signup_event"`
		Name               string `json:"name"`
		WebhookVerifyToken string `json:"webhook_verify_token"`
	}
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	req.Code = strings.TrimSpace(req.Code)
	req.PhoneID = strings.TrimSpace(req.PhoneID)
	req.PhoneNumberID = strings.TrimSpace(req.PhoneNumberID)
	req.WABAID = strings.TrimSpace(req.WABAID)
	req.SignupEvent = strings.ToUpper(strings.TrimSpace(req.SignupEvent))
	req.Name = strings.TrimSpace(req.Name)
	if req.PhoneID != "" && req.PhoneNumberID != "" && req.PhoneID != req.PhoneNumberID {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "phone_id and phone_number_id must match", nil, "")
	}
	if req.PhoneID == "" {
		req.PhoneID = req.PhoneNumberID
	}
	if req.SignupEvent == "" {
		// Backward compatibility for the earlier exact-ID client contract.
		req.SignupEvent = embeddedSignupEventFinish
	}

	a.Log.Info("Received embedded signup exchange token request",
		"phone_id", req.PhoneID,
		"waba_id", req.WABAID,
		"organization_id", orgID)

	if req.Code == "" || req.WABAID == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Embedded Signup code and waba_id are required", nil, "")
	}
	switch req.SignupEvent {
	case embeddedSignupEventFinish:
		if req.PhoneID == "" {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"Classic Embedded Signup must provide the selected phone_id",
				nil,
				"",
			)
		}
	case embeddedSignupEventCoexistence:
		// Meta's documented v3 coexistence result contains the WABA ID only.
		// The exact WhatsApp Business App phone is resolved and verified below.
	default:
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Unsupported Embedded Signup completion event",
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

	phoneID, wabaID, name, tokenExpiresAt, skipRegistration, err := a.validateEmbeddedSignupSelection(
		ctx,
		orgID,
		accessToken,
		req.PhoneID,
		req.WABAID,
		req.Name,
		req.SignupEvent,
	)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	// 3. Build the local account claim. The claim is encrypted and persisted
	// before any provider-side registration or subscription mutation. The global
	// active-phone index is authoritative under concurrent cross-workspace claims.
	account, _, candidateExistingAccount, preflightAccount, err := a.createOrUpdateAccount(ctx, orgID, phoneID, wabaID, name, accessToken, tokenExpiresAt)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, err.Error(), nil, "")
	}

	account.Status = "pending_registration"
	account.UpdatedByID = &userID
	if !candidateExistingAccount {
		account.CreatedByID = &userID
	}
	registrationPIN := ""
	if !skipRegistration && !account.IsSMB {
		registrationPIN, err = generateNumericPIN(6)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to generate secure registration PIN", nil, "")
		}
		// Persist the exact encrypted PIN before sending it to Meta. If the
		// process stops after registration, the durable pending claim retains the
		// PIN needed to reconcile the provider state.
		account.Pin = registrationPIN
	}

	// 4. Encrypt and persist the ownership claim before registering the number.
	if err := a.encryptAccountSecrets(account); err != nil {
		a.Log.Error("Failed to encrypt account secrets", "error", err)
		if errors.Is(err, errAccountEncryptionUnavailable) {
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, err.Error(), nil, "")
	}

	attemptID := uuid.New()
	existingAccount, oldAccount, err := a.claimEmbeddedSignupAccount(
		orgID,
		account,
		attemptID,
		preflightAccount,
	)
	if err != nil {
		a.Log.Error("Failed to save account", "error", err)
		if errors.Is(err, errEmbeddedSignupAlreadyInProgress) ||
			errors.Is(err, errEmbeddedSignupAttemptSuperseded) {
			return r.SendErrorEnvelope(
				fasthttp.StatusConflict,
				"A connection attempt for this WhatsApp phone is already in progress. Wait a moment and try again.",
				nil,
				"",
			)
		}
		if isWhatsAppPhoneOwnershipConflict(err) {
			return sendWhatsAppPhoneOwnershipConflict(r)
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to save account", nil, "")
	}
	// The ownership claim above is durable even when ExchangeToken is running
	// inside a request transaction. Remove any former-owner or old-credential
	// cache entry before the first provider-side mutation can run.
	a.InvalidateWhatsAppAccountCache(account.PhoneID)

	var priorStatus string
	if oldAccount != nil {
		priorStatus = oldAccount.Status
	}

	// 5. Attempt registration only after the local ownership claim succeeds.
	runtimeAccount := *account
	runtimeAccount.AccessToken = accessToken
	runtimeAccount.Pin = registrationPIN
	regErr := a.attemptAutoRegistration(
		ctx,
		&runtimeAccount,
		accessToken,
		priorStatus,
		skipRegistration,
	)
	plaintextPin := ""
	if regErr == nil {
		plaintextPin = runtimeAccount.Pin
	}
	account.Pin = runtimeAccount.Pin
	var metaRejection *whatsapp.MetaAPIRequestError
	if regErr != nil && oldAccount != nil && errors.As(regErr, &metaRejection) {
		// Meta definitively rejected the new PIN, so retain the prior known PIN.
		// For transport errors the remote outcome is unknown and the pre-persisted
		// new PIN is kept for reconciliation.
		account.Pin = oldAccount.Pin
	}
	if err := a.encryptAccountSecrets(account); err != nil {
		a.Log.Error("Failed to encrypt registered account secrets", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to secure registered account", nil, "")
	}

	var subscriptionErr error
	if regErr != nil {
		// Registration is the final provider mutation for this attempt. Record
		// its outcome and release the attempt lease. A definitive Meta rejection
		// may restore the prior known-good status and PIN; an ambiguous transport
		// result retains the newly persisted PIN for reconciliation.
		account.Status = runtimeAccount.Status
		if err := a.persistEmbeddedSignupAccount(orgID, account, attemptID, true); err != nil {
			a.Log.Error("Failed to persist registration outcome", "error", err, "account_id", account.ID)
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "The connection attempt was superseded; reload the account before retrying", nil, "")
		}
	} else {
		// 6. Persist a non-ready state under this attempt lease before the remote
		// subscription. Another reconnect cannot enter while this call is active.
		account.Status = "pending_subscription"
		if err := a.persistEmbeddedSignupAccount(orgID, account, attemptID, false); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "The connection attempt was superseded; reload the account before retrying", nil, "")
		}
		subscriptionAccount := a.toWhatsAppAccount(&runtimeAccount)

		// 7. Subscribe only after registration succeeded or was correctly skipped,
		// then commit the true readiness outcome independently of the request-wide
		// tenant transaction.
		if a.WhatsApp == nil {
			subscriptionErr = errors.New("WhatsApp webhook subscription is unavailable")
		} else {
			subscriptionErr = a.WhatsApp.SubscribeApp(ctx, subscriptionAccount)
		}
		account.Status = "active"
		if subscriptionErr != nil {
			account.Status = "subscription_failed"
			a.Log.Warn("WhatsApp webhook subscription failed", "account_id", account.ID, "business_id", account.BusinessID)
		}
		if statusErr := a.persistEmbeddedSignupAccount(orgID, account, attemptID, true); statusErr != nil {
			a.Log.Error("Failed to persist embedded signup subscription readiness", "error", statusErr, "account_id", account.ID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Account saved, but webhook subscription status could not be recorded", nil, "")
		}
		a.InvalidateWhatsAppAccountCache(account.PhoneID)
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

// claimEmbeddedSignupAccount serializes reconnects for the same phone and
// durably leases every later state transition to attemptID. The advisory lock
// covers the first-connect race where no row exists yet; SELECT FOR UPDATE
// covers reconnects to an existing row.
func (a *App) claimEmbeddedSignupAccount(
	orgID uuid.UUID,
	account *models.WhatsAppAccount,
	attemptID uuid.UUID,
	preflightAccount *models.WhatsAppAccount,
) (existingAccount bool, oldAccount *models.WhatsAppAccount, err error) {
	if account == nil {
		return false, nil, errors.New("WhatsApp account ownership claim is required")
	}
	if attemptID == uuid.Nil {
		return false, nil, errors.New("WhatsApp connection attempt ID is required")
	}
	if preflightAccount != nil && (preflightAccount.ID == uuid.Nil ||
		preflightAccount.OrganizationID != orgID ||
		strings.TrimSpace(preflightAccount.PhoneID) != strings.TrimSpace(account.PhoneID)) {
		return false, nil, errEmbeddedSignupAttemptSuperseded
	}

	now := time.Now().UTC()
	claim := func(tx *gorm.DB) error {
		// PostgreSQL advisory transaction locks are automatically released at
		// commit/rollback and also serialize two first-time claims that race
		// before either has inserted a row.
		if err := lockWhatsAppPhoneAttempts(tx, account.PhoneID); err != nil {
			return err
		}

		var current models.WhatsAppAccount
		currentQuery := tx.Unscoped().
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ?", orgID)
		if preflightAccount == nil {
			currentQuery = currentQuery.Where("phone_id = ?", account.PhoneID)
		} else {
			currentQuery = currentQuery.Where("id = ?", preflightAccount.ID)
		}
		findErr := currentQuery.First(&current).Error
		switch {
		case errors.Is(findErr, gorm.ErrRecordNotFound):
			if preflightAccount != nil {
				return errEmbeddedSignupAttemptSuperseded
			}
			account.ConnectionAttemptID = &attemptID
			account.ConnectionAttemptStartedAt = &now
			return tx.Create(account).Error
		case findErr != nil:
			return findErr
		}
		if preflightAccount == nil ||
			current.ID != preflightAccount.ID ||
			current.PhoneID != preflightAccount.PhoneID ||
			!current.UpdatedAt.Equal(preflightAccount.UpdatedAt) ||
			current.DeletedAt.Valid != preflightAccount.DeletedAt.Valid ||
			(current.DeletedAt.Valid && !current.DeletedAt.Time.Equal(preflightAccount.DeletedAt.Time)) {
			// The account changed after this request's preflight. This includes a
			// later delete, restore, replacement row, or a newer deletion generation.
			// Only the exact tombstone observed by preflight may be restored.
			return errEmbeddedSignupAttemptSuperseded
		}

		if hasLiveWhatsAppAccountAttempt(&current, now) {
			return errEmbeddedSignupAlreadyInProgress
		}

		existingAccount = true
		previous := current
		oldAccount = &previous

		// Start from the freshly locked row so a reconnect cannot erase unrelated
		// settings that changed after the preflight read. Overlay only the fields
		// established by this verified Embedded Signup result.
		current.Name = account.Name
		current.PhoneID = account.PhoneID
		current.BusinessID = account.BusinessID
		current.AccessToken = account.AccessToken
		current.AccessTokenExpiresAt = account.AccessTokenExpiresAt
		current.APIVersion = account.APIVersion
		current.IsSMB = account.IsSMB
		current.Status = account.Status
		current.Pin = account.Pin
		current.UpdatedByID = account.UpdatedByID
		current.DeletedAt = gorm.DeletedAt{}
		current.ConnectionAttemptID = &attemptID
		current.ConnectionAttemptStartedAt = &now
		*account = current

		result := tx.Unscoped().Select("*").Updates(account)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errEmbeddedSignupAttemptSuperseded
		}
		return nil
	}

	err = a.withIndependentAccountTenant(orgID, claim)
	return existingAccount, oldAccount, err
}

// persistEmbeddedSignupAccount commits a leased Embedded Signup state
// transition in its own tenant transaction. The attempt predicate is an
// optimistic fence: an old request cannot overwrite a newer reconnect.
func (a *App) persistEmbeddedSignupAccount(
	orgID uuid.UUID,
	account *models.WhatsAppAccount,
	attemptID uuid.UUID,
	clearAttempt bool,
) error {
	if account == nil {
		return errors.New("WhatsApp account ownership claim is required")
	}
	if attemptID == uuid.Nil {
		return errors.New("WhatsApp connection attempt ID is required")
	}

	persist := func(tx *gorm.DB) error {
		updates := map[string]any{
			"status": account.Status,
			"pin":    account.Pin,
		}
		if clearAttempt {
			account.ConnectionAttemptID = nil
			account.ConnectionAttemptStartedAt = nil
			updates["connection_attempt_id"] = nil
			updates["connection_attempt_started_at"] = nil
		} else {
			account.ConnectionAttemptID = &attemptID
			updates["connection_attempt_id"] = attemptID
		}
		result := tx.Model(&models.WhatsAppAccount{}).
			Where(
				"id = ? AND organization_id = ? AND connection_attempt_id = ? AND deleted_at IS NULL",
				account.ID,
				orgID,
				attemptID,
			).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errEmbeddedSignupAttemptSuperseded
		}
		return nil
	}
	return a.withIndependentAccountTenant(orgID, persist)
}

func (a *App) withIndependentAccountTenant(orgID uuid.UUID, fn func(*gorm.DB) error) error {
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return errors.New("database is unavailable")
	}
	if a.rlsEnabled() {
		return database.WithTenant(root.DB, orgID, fn)
	}
	return root.DB.Transaction(fn)
}

func (a *App) validateEmbeddedSignupSelection(
	ctx context.Context,
	orgID uuid.UUID,
	accessToken, phoneID, wabaID, name, signupEvent string,
) (string, string, string, *time.Time, bool, error) {
	a.Log.Info("Validating embedded signup token via debug_token")

	_, tokenExpiresAt, err := a.debugAndValidateMetaAccessToken(ctx, orgID, accessToken)
	if err != nil {
		return "", "", "", nil, false, err
	}

	phoneID = strings.TrimSpace(phoneID)
	wabaID = strings.TrimSpace(wabaID)
	name = strings.TrimSpace(name)
	signupEvent = strings.ToUpper(strings.TrimSpace(signupEvent))
	if wabaID == "" {
		return "", "", "", nil, false, errors.New("embedded signup did not provide a WABA ID")
	}

	skipRegistration := signupEvent == embeddedSignupEventCoexistence
	if skipRegistration && phoneID == "" {
		phonesResp, err := a.WhatsApp.GetWABAPhoneNumbersVersion(ctx, wabaID, accessToken, a.defaultAPIVersion())
		if err != nil {
			a.Log.Error("Failed to fetch phone numbers from Meta", "error", err)
			return "", "", "", nil, false, fmt.Errorf("failed to fetch coexistence phone numbers from WABA: %w", err)
		}

		coexistenceCandidates := make([]int, 0, 1)
		for i := range phonesResp.Data {
			if phonesResp.Data[i].IsOnBizApp &&
				strings.EqualFold(strings.TrimSpace(phonesResp.Data[i].PlatformType), "CLOUD_API") {
				coexistenceCandidates = append(coexistenceCandidates, i)
			}
		}
		if len(coexistenceCandidates) == 0 {
			return "", "", "", nil, false, errors.New("meta did not return a WhatsApp Business App phone number for this WABA")
		}
		if len(coexistenceCandidates) > 1 {
			return "", "", "", nil, false, errors.New("meta returned multiple WhatsApp Business App phone numbers for this WABA; reconnect using a WABA with one coexistence number")
		}

		phone := phonesResp.Data[coexistenceCandidates[0]]
		phoneID = strings.TrimSpace(phone.ID)
		if phoneID == "" {
			return "", "", "", nil, false, errors.New("meta returned a coexistence phone number without an ID")
		}
		if name == "" && (phone.VerifiedName != "" || phone.DisplayPhoneNumber != "") {
			name = strings.TrimSpace(fmt.Sprintf("%s (%s)", phone.VerifiedName, phone.DisplayPhoneNumber))
		}
	}
	if phoneID == "" {
		return "", "", "", nil, false, errors.New("embedded signup did not provide a phone number ID")
	}

	// Verify the token can read each exact object and that Meta lists this Phone
	// ID under this WABA. The browser event and coexistence lookup are untrusted.
	// This prevents Embedded Signup payloads from bypassing the same contract
	// enforced by manual account creation and credential updates.
	validation, err := a.validateWhatsAppAccountContract(
		ctx,
		phoneID,
		wabaID,
		accessToken,
		a.defaultAPIVersion(),
	)
	if err != nil {
		return "", "", "", nil, false, err
	}
	if skipRegistration && (!validation.IsOnBizApp ||
		!strings.EqualFold(strings.TrimSpace(validation.PlatformType), "CLOUD_API")) {
		return "", "", "", nil, false, errors.New("meta did not confirm that the selected phone is an active WhatsApp Business App coexistence number")
	}

	return phoneID, wabaID, name, tokenExpiresAt, skipRegistration, nil
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
		return nil, errors.New("meta returned an invalid embedded signup token")
	}
	if strings.TrimSpace(info.AppID) == "" || strings.TrimSpace(info.AppID) != strings.TrimSpace(expectedAppID) {
		return nil, errors.New("the embedded signup token was issued to a different Meta app")
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
			return nil, fmt.Errorf("the embedded signup token is missing the required %s permission", required)
		}
	}

	var earliest *time.Time
	for _, unixSeconds := range []int64{info.ExpiresAt, info.DataAccessExpiresAt} {
		if unixSeconds <= 0 {
			continue
		}
		candidate := time.Unix(unixSeconds, 0).UTC()
		if !candidate.After(now.UTC()) {
			return nil, errors.New("the embedded signup token or its data access has expired")
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
		account.DeletedAt = gorm.DeletedAt{}
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

func (a *App) attemptAutoRegistration(ctx context.Context, account *models.WhatsAppAccount, accessToken, priorStatus string, skipRegistration bool) error {
	if skipRegistration || account.IsSMB {
		account.Status = "active"
		account.Pin = ""
		a.Log.Info("WhatsApp Business App account detected; skipped phone registration", "phone_id", account.PhoneID)
		return nil
	}

	if strings.TrimSpace(account.Pin) == "" {
		return errors.New("registration PIN is unavailable")
	}

	a.Log.Info("Attempting phone number auto-registration", "phone_id", account.PhoneID)
	regErr := a.WhatsApp.RegisterPhoneNumber(ctx, account.PhoneID, account.Pin, accessToken, account.APIVersion)

	if regErr == nil {
		account.Status = "active"
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

	runtimeAccessToken := account.AccessToken
	attemptID := uuid.New()
	account.UpdatedByID = &userID
	finishRegistration := func() error {
		if err := a.encryptAccountSecrets(account); err != nil {
			return err
		}
		if err := a.finishWhatsAppAccountAttempt(
			orgID,
			account.ID,
			attemptID,
			map[string]any{
				"status":        account.Status,
				"pin":           account.Pin,
				"updated_by_id": account.UpdatedByID,
			},
		); err != nil {
			return err
		}
		account.ConnectionAttemptID = nil
		account.ConnectionAttemptStartedAt = nil
		return nil
	}

	// Check if this is an SMB phone — SMB numbers are already registered
	// via the Business App and don't support the two-step registration API.
	if account.IsSMB {
		a.Log.Info("Manual registration: SMB account detected, skipping registration", "phone_id", account.PhoneID)
		pin = ""
		account.Status = "active"
		account.Pin = ""
		_, err := a.beginWhatsAppAccountAttempt(
			orgID,
			account.ID,
			attemptID,
			whatsAppAccountAttemptOptions{
				ExpectedPhoneID:   account.PhoneID,
				ExpectedUpdatedAt: oldAccount.UpdatedAt,
				Updates: map[string]any{
					"status":        account.Status,
					"pin":           "",
					"updated_by_id": account.UpdatedByID,
				},
			},
		)
		if err == nil {
			err = finishRegistration()
		}
		if err != nil {
			a.Log.Error("Failed to persist SMB registration outcome", "error", err, "account_id", account.ID)
			if errors.Is(err, errEmbeddedSignupAlreadyInProgress) ||
				errors.Is(err, errEmbeddedSignupAttemptSuperseded) {
				return sendWhatsAppAttemptConflict(r)
			}
			if errors.Is(err, errAccountEncryptionUnavailable) {
				return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
			}
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update account status", nil, "")
		}
	} else {
		// Persist the exact encrypted PIN and non-ready status before calling Meta.
		// If the process stops after the provider accepts the request, recovery can
		// reconcile the durable pending state with the same PIN.
		account.Status = "pending_registration"
		account.Pin = pin
		if err := a.encryptAccountSecrets(account); err != nil {
			a.Log.Error("Failed to encrypt pending manual registration", "error", err, "account_id", account.ID)
			if errors.Is(err, errAccountEncryptionUnavailable) {
				return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
			}
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Phone registration could not be started", nil, "")
		}
		_, err := a.beginWhatsAppAccountAttempt(
			orgID,
			account.ID,
			attemptID,
			whatsAppAccountAttemptOptions{
				ExpectedPhoneID:   account.PhoneID,
				ExpectedUpdatedAt: oldAccount.UpdatedAt,
				Updates: map[string]any{
					"status":        account.Status,
					"pin":           account.Pin,
					"updated_by_id": account.UpdatedByID,
				},
			},
		)
		if err != nil {
			a.Log.Error("Failed to persist pending manual registration", "error", err, "account_id", account.ID)
			if errors.Is(err, errEmbeddedSignupAlreadyInProgress) ||
				errors.Is(err, errEmbeddedSignupAttemptSuperseded) {
				return sendWhatsAppAttemptConflict(r)
			}
			if errors.Is(err, errAccountEncryptionUnavailable) {
				return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
			}
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Phone registration could not be started", nil, "")
		}
		a.InvalidateWhatsAppAccountCache(account.PhoneID)

		ctx, cancel := context.WithTimeout(requestContext(r), 30*time.Second)
		defer cancel()
		registrationErr := a.WhatsApp.RegisterPhoneNumber(
			ctx,
			account.PhoneID,
			pin,
			runtimeAccessToken,
			account.APIVersion,
		)
		if registrationErr != nil {
			a.Log.Error("Manual registration failed", "error", registrationErr)
			var metaRejection *whatsapp.MetaAPIRequestError
			if errors.As(registrationErr, &metaRejection) {
				account.Status = oldAccount.Status
				if strings.TrimSpace(account.Status) == "" {
					account.Status = "pending_registration"
				}
				account.Pin = oldAccount.Pin
			} else {
				// A transport failure has an ambiguous remote outcome. Retain the
				// pending state and exact PIN committed before the call.
				account.Status = "pending_registration"
				account.Pin = pin
			}
			if err := finishRegistration(); err != nil {
				a.Log.Error("Failed to persist manual registration failure", "error", err, "account_id", account.ID)
				return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Phone registration failed and its outcome could not be recorded", nil, "")
			}
			a.InvalidateWhatsAppAccountCache(account.PhoneID)
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, registrationErr.Error(), nil, "")
		}

		account.Status = "active"
		account.Pin = pin
		if err := finishRegistration(); err != nil {
			a.Log.Error("Failed to persist manual registration success", "error", err, "account_id", account.ID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Phone registration succeeded but account readiness could not be saved", nil, "")
		}
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
