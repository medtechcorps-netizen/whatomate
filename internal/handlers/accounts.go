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
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	errMetaIntegrationDisabled                    = errors.New("meta integration is disabled")
	errAccountEncryptionUnavailable               = errors.New("account credential encryption is unavailable")
	errMetaAppIDNotConfigured                     = errors.New("meta app ID is not configured")
	errMetaAppSecretNotConfigured                 = errors.New("meta app secret is not configured")
	errMetaTokenValidationUnavailable             = errors.New("meta access token validation is unavailable")
	errWhatsAppPhoneAlreadyClaimed                = errors.New("WhatsApp phone is already claimed")
	errEmbeddedSignupClaimSuperseded              = errors.New("embedded signup claim was superseded")
	errEmbeddedSignupConfigurationChanged         = errors.New("embedded signup configuration changed")
	errEmbeddedSignupClaimInProgress              = errors.New("embedded signup claim is already in progress")
	errEmbeddedSignupAppSubscriptionNotProven     = errors.New("embedded signup app subscription is not proven")
	errRegistrationRecoveryStatus                 = errors.New("account is not pending registration")
	errRegistrationRecoveryCredentials            = errors.New("registration recovery credentials are unavailable")
	errRegistrationRecoverySuperseded             = errors.New("registration recovery was superseded")
	errRegistrationReconciliationEvidenceMissing  = errors.New("recent inbound registration evidence is unavailable")
	errRegistrationReconciliationTokenMismatch    = errors.New("registration reconciliation token contract changed")
	errSubscriptionRecoveryStatus                 = errors.New("account is not pending subscription")
	errSubscriptionRecoveryCredentials            = errors.New("subscription recovery credentials are unavailable")
	errSubscriptionRecoverySuperseded             = errors.New("subscription recovery was superseded")
	errPhoneWebhookOverrideAccountInactive        = errors.New("phone webhook override account is inactive")
	errPhoneWebhookOverrideCredentialsUnavailable = errors.New("phone webhook override credentials are unavailable")
	errWhatsAppAccountUpdateSuperseded            = errors.New("WhatsApp account update was superseded")
)

// canonicalPhoneWebhookOrigin is intentionally a fixed production origin.
// Never derive this from Host, Origin, or X-Forwarded-* request values: those
// headers are caller-controlled and can route Meta webhooks to an attacker.
const canonicalPhoneWebhookOrigin = "https://app.rereply.app"

// A pending registration can be reconciled from provider evidence only while
// that evidence is operationally recent. The evidence must also post-date the
// exact pending claim, so an old message can never activate a newer token/PIN.
const registrationReconciliationEvidenceWindow = 24 * time.Hour

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
	req.APIVersion = strings.TrimSpace(req.APIVersion)

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
		apiVersion = strings.TrimSpace(a.defaultAPIVersion())
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
		account.AccessToken,
		"active",
		"subscription_failed",
	)
	if statusErr != nil {
		if errors.Is(statusErr, errEmbeddedSignupClaimSuperseded) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account was replaced by a newer request", nil, "")
		}
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

	account, err := findByIDAndOrg[models.WhatsAppAccount](a.DB, r, id, orgID, "Account")
	if err != nil {
		return nil
	}
	originalAccessTokenCiphertext := account.AccessToken
	originalPINCiphertext := account.Pin
	originalStatus := strings.TrimSpace(account.Status)
	originalUpdatedAt := account.UpdatedAt
	a.decryptAccountSecrets(account)
	oldAccount := *account // value copy for audit

	var req AccountRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	req.Name = strings.TrimSpace(req.Name)
	req.PhoneID = strings.TrimSpace(req.PhoneID)
	req.BusinessID = strings.TrimSpace(req.BusinessID)
	req.APIVersion = strings.TrimSpace(req.APIVersion)
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

	currentPhoneID := strings.TrimSpace(account.PhoneID)
	effectivePhoneID := currentPhoneID
	if req.PhoneID != "" {
		effectivePhoneID = req.PhoneID
	}
	currentBusinessID := strings.TrimSpace(account.BusinessID)
	effectiveBusinessID := currentBusinessID
	if req.BusinessID != "" {
		effectiveBusinessID = req.BusinessID
	}
	effectiveAccessToken := account.AccessToken
	if req.AccessToken != "" {
		effectiveAccessToken = req.AccessToken
	}
	currentAPIVersion := strings.TrimSpace(account.APIVersion)
	effectiveAPIVersion := currentAPIVersion
	if req.APIVersion != "" {
		effectiveAPIVersion = req.APIVersion
	}
	if effectiveAPIVersion == "" {
		effectiveAPIVersion = strings.TrimSpace(a.defaultAPIVersion())
	}
	accountContractChanged := effectivePhoneID != currentPhoneID ||
		effectiveBusinessID != currentBusinessID ||
		req.AccessToken != "" ||
		effectiveAPIVersion != currentAPIVersion
	accountStatus := originalStatus
	if accountContractChanged && embeddedSignupRecoveryLockedStatus(accountStatus) {
		// Registration/subscription recovery owns this provider tuple. A normal
		// PUT must never skip /register, replace its durable token/PIN claim, or
		// race the fenced recovery status transition.
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"Finish WhatsApp registration and subscription recovery before changing account credentials",
			nil,
			"",
		)
	}

	// Non-contract edits remain available while onboarding is pending, but use
	// a partial status-fenced update so they cannot write decrypted secrets or
	// a stale recovery status back over a concurrent Register/Subscribe action.
	if embeddedSignupRecoveryLockedStatus(accountStatus) {
		updatedName := strings.TrimSpace(account.Name)
		if req.Name != "" {
			updatedName = req.Name
		}
		updateErr := a.DB.Transaction(func(tx *gorm.DB) error {
			shadowPrepared, err := channelapi.StageLegacyMetaWhatsAppAccountRename(
				tx,
				orgID,
				account.ID,
				account.Name,
				updatedName,
			)
			if err != nil {
				return err
			}
			result := tx.Model(&models.WhatsAppAccount{}).
				Where(
					"id = ? AND organization_id = ? AND status = ? AND updated_at = ? AND access_token = ? AND pin = ?",
					account.ID,
					orgID,
					accountStatus,
					originalUpdatedAt,
					originalAccessTokenCiphertext,
					originalPINCiphertext,
				).
				Updates(map[string]any{
					"name":                     updatedName,
					"auto_read_receipt":        req.AutoReadReceipt,
					"business_calling_enabled": req.BusinessCallingEnabled,
					"is_default_incoming":      req.IsDefaultIncoming,
					"is_default_outgoing":      req.IsDefaultOutgoing,
					"updated_by_id":            userID,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errWhatsAppAccountUpdateSuperseded
			}
			if !shadowPrepared {
				return channelapi.FinalizeLegacyMetaWhatsAppAccountRename(
					tx,
					orgID,
					account.ID,
					account.Name,
					updatedName,
				)
			}
			return nil
		})
		if errors.Is(updateErr, errWhatsAppAccountUpdateSuperseded) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account recovery changed; reload and try again", nil, "")
		}
		if errors.Is(updateErr, channelapi.ErrLegacyMetaBridgeConflict) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account Omnichannel binding changed; reload and try again", nil, "")
		}
		if updateErr != nil {
			a.Log.Error("Failed to update pending WhatsApp account", "error", updateErr, "account_id", account.ID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update account", nil, "")
		}
		if req.IsDefaultIncoming && !account.IsDefaultIncoming {
			a.DB.Model(&models.WhatsAppAccount{}).
				Where("organization_id = ? AND id <> ? AND is_default_incoming = ?", orgID, account.ID, true).
				Update("is_default_incoming", false)
		}
		if req.IsDefaultOutgoing && !account.IsDefaultOutgoing {
			a.DB.Model(&models.WhatsAppAccount{}).
				Where("organization_id = ? AND id <> ? AND is_default_outgoing = ?", orgID, account.ID, true).
				Update("is_default_outgoing", false)
		}
		a.InvalidateWhatsAppAccountCache(account.PhoneID)
		var updated models.WhatsAppAccount
		if err := a.DB.Preload("CreatedBy").Preload("UpdatedBy").
			Where("id = ? AND organization_id = ?", account.ID, orgID).
			First(&updated).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to reload account", nil, "")
		}
		a.logAudit(orgID, userID,
			"account", updated.ID, models.AuditActionUpdated, &oldAccount, &updated)
		return r.SendEnvelope(accountToResponse(updated))
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
	account.Name = strings.TrimSpace(account.Name)
	if req.Name != "" {
		account.Name = req.Name
	}
	account.PhoneID = effectivePhoneID
	account.BusinessID = effectiveBusinessID
	tokenChanged := false
	if req.AccessToken != "" {
		account.AccessToken = req.AccessToken
		account.AccessTokenExpiresAt = tokenExpiresAt
		tokenChanged = true
	}
	account.APIVersion = effectiveAPIVersion
	account.AutoReadReceipt = req.AutoReadReceipt
	account.BusinessCallingEnabled = req.BusinessCallingEnabled

	account.IsDefaultIncoming = req.IsDefaultIncoming
	account.IsDefaultOutgoing = req.IsDefaultOutgoing
	account.UpdatedByID = &userID
	var subscriptionAccount *whatsapp.Account
	if accountContractChanged {
		account.Status = "pending_subscription"
		subscriptionAccount = a.toWhatsAppAccount(account)
	}

	updates := map[string]any{
		"name":                     account.Name,
		"phone_id":                 account.PhoneID,
		"business_id":              account.BusinessID,
		"api_version":              account.APIVersion,
		"auto_read_receipt":        account.AutoReadReceipt,
		"business_calling_enabled": account.BusinessCallingEnabled,
		"is_default_incoming":      account.IsDefaultIncoming,
		"is_default_outgoing":      account.IsDefaultOutgoing,
		"updated_by_id":            userID,
	}
	expectedSubscriptionAccessTokenCiphertext := originalAccessTokenCiphertext
	if tokenChanged {
		encryptedAccessToken, encryptErr := crypto.Encrypt(account.AccessToken, a.integrationEncryptionKey())
		if encryptErr != nil {
			a.Log.Error("Failed to encrypt account access token", "account_id", account.ID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update account", nil, "")
		}
		updates["access_token"] = encryptedAccessToken
		updates["access_token_expires_at"] = tokenExpiresAt
		expectedSubscriptionAccessTokenCiphertext = encryptedAccessToken
	}
	if accountContractChanged {
		updates["status"] = "pending_subscription"
	}
	updateErr := a.DB.Transaction(func(tx *gorm.DB) error {
		shadowPrepared, err := channelapi.StageLegacyMetaWhatsAppAccountRename(
			tx,
			orgID,
			account.ID,
			oldAccount.Name,
			account.Name,
		)
		if err != nil {
			return err
		}
		result := tx.Model(&models.WhatsAppAccount{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND updated_at = ? AND access_token = ? AND pin = ?",
				account.ID,
				orgID,
				originalStatus,
				originalUpdatedAt,
				originalAccessTokenCiphertext,
				originalPINCiphertext,
			).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errWhatsAppAccountUpdateSuperseded
		}
		if !shadowPrepared {
			return channelapi.FinalizeLegacyMetaWhatsAppAccountRename(
				tx,
				orgID,
				account.ID,
				oldAccount.Name,
				account.Name,
			)
		}
		return nil
	})
	if errors.Is(updateErr, errWhatsAppAccountUpdateSuperseded) {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account changed; reload and try again", nil, "")
	}
	if errors.Is(updateErr, channelapi.ErrLegacyMetaBridgeConflict) {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account Omnichannel binding changed; reload and try again", nil, "")
	}
	if updateErr != nil {
		a.Log.Error("Failed to update account", "error", updateErr)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update account", nil, "")
	}
	if req.IsDefaultIncoming && !oldAccount.IsDefaultIncoming {
		a.DB.Model(&models.WhatsAppAccount{}).
			Where("organization_id = ? AND id <> ? AND is_default_incoming = ?", orgID, account.ID, true).
			Update("is_default_incoming", false)
	}
	if req.IsDefaultOutgoing && !oldAccount.IsDefaultOutgoing {
		a.DB.Model(&models.WhatsAppAccount{}).
			Where("organization_id = ? AND id <> ? AND is_default_outgoing = ?", orgID, account.ID, true).
			Update("is_default_outgoing", false)
	}

	var subscriptionErr error
	if accountContractChanged {
		var statusErr error
		subscriptionErr, statusErr = a.subscribeAndPersistWhatsAppStatus(
			subscriptionCtx,
			orgID,
			account,
			subscriptionAccount,
			expectedSubscriptionAccessTokenCiphertext,
			"active",
			"subscription_failed",
		)
		if statusErr != nil {
			if errors.Is(statusErr, errEmbeddedSignupClaimSuperseded) {
				return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account was replaced by a newer request", nil, "")
			}
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

	deleteResult := a.DB.
		Where(
			"id = ? AND organization_id = ? AND status NOT IN ?",
			account.ID,
			orgID,
			[]string{"pending_registration", "pending_subscription"},
		).
		Delete(&models.WhatsAppAccount{})
	if deleteResult.Error != nil {
		a.Log.Error("Failed to delete account", "error", deleteResult.Error)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete account", nil, "")
	}
	if deleteResult.RowsAffected != 1 {
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"Finish WhatsApp registration and subscription recovery before deleting this account",
			nil,
			"",
		)
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
	expectedAccessTokenCiphertext string,
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
	// A manual Create/Update request may be waiting on Meta while the row is
	// deleted and reclaimed by Embedded Signup. Finalize only the exact durable
	// provider tuple and encrypted token that this Subscribe call observed.
	result := a.DB.Model(&models.WhatsAppAccount{}).
		Where(
			"id = ? AND organization_id = ? AND status = ? AND access_token = ? AND phone_id = ? AND business_id = ? AND api_version = ?",
			account.ID,
			orgID,
			"pending_subscription",
			expectedAccessTokenCiphertext,
			account.PhoneID,
			account.BusinessID,
			account.APIVersion,
		).
		Update("status", status)
	if result.Error != nil {
		return subscriptionErr, result.Error
	}
	if result.RowsAffected != 1 {
		return subscriptionErr, errEmbeddedSignupClaimSuperseded
	}
	account.Status = status
	a.InvalidateWhatsAppAccountCache(account.PhoneID)
	return subscriptionErr, nil
}

// SubscribeApp subscribes the app to webhooks for the WhatsApp Business Account.
// This is required after phone number registration to receive incoming messages from Meta.
func (a *App) SubscribeApp(r *fastglue.Request) error {
	orgID, err := a.requireExplicitOrganization(r)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}

	type subscriptionRecoverySnapshot struct {
		accountID             uuid.UUID
		phoneID               string
		businessID            string
		apiVersion            string
		accessToken           string
		accessTokenCiphertext string
		expectedStatus        string
	}
	var snapshot subscriptionRecoverySnapshot
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		resolvedOrgID, _, authErr := scoped.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
		if authErr != nil {
			return authErr
		}
		if resolvedOrgID != orgID {
			_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Selected organization is not available", nil, "")
			return errEnvelopeSent
		}

		var account models.WhatsAppAccount
		if loadErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", id, orgID).
			First(&account).Error; loadErr != nil {
			return loadErr
		}
		expectedStatus := strings.TrimSpace(account.Status)
		if expectedStatus != "pending_subscription" && expectedStatus != "subscription_failed" {
			return errSubscriptionRecoveryStatus
		}
		encryptionKey := strings.TrimSpace(scoped.integrationEncryptionKey())
		if encryptionKey == "" {
			return errAccountEncryptionUnavailable
		}
		accessToken, decryptErr := crypto.Decrypt(account.AccessToken, encryptionKey)
		if decryptErr != nil || strings.TrimSpace(accessToken) == "" || crypto.IsEncrypted(accessToken) {
			return errSubscriptionRecoveryCredentials
		}
		accessTokenCiphertext := account.AccessToken
		if !crypto.IsEncrypted(accessTokenCiphertext) {
			accessTokenCiphertext, decryptErr = crypto.Encrypt(accessToken, encryptionKey)
			if decryptErr != nil {
				return decryptErr
			}
			result := scoped.DB.Model(&models.WhatsAppAccount{}).
				Where("id = ? AND organization_id = ? AND status = ?", account.ID, orgID, expectedStatus).
				Update("access_token", accessTokenCiphertext)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errSubscriptionRecoverySuperseded
			}
		}

		snapshot = subscriptionRecoverySnapshot{
			accountID:             account.ID,
			phoneID:               strings.TrimSpace(account.PhoneID),
			businessID:            strings.TrimSpace(account.BusinessID),
			apiVersion:            strings.TrimSpace(account.APIVersion),
			accessToken:           strings.TrimSpace(accessToken),
			accessTokenCiphertext: accessTokenCiphertext,
			expectedStatus:        expectedStatus,
		}
		return nil
	})
	if errors.Is(err, errEnvelopeSent) {
		return nil
	}
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Account not found", nil, "")
		case errors.Is(err, errSubscriptionRecoveryStatus):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account is not pending webhook subscription", nil, "")
		case errors.Is(err, errSubscriptionRecoveryCredentials):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp subscription recovery is unavailable for this account", nil, "")
		case errors.Is(err, errSubscriptionRecoverySuperseded):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp subscription recovery changed; reload and try again", nil, "")
		case errors.Is(err, errAccountEncryptionUnavailable):
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
		default:
			a.Log.Error("Failed to prepare WhatsApp subscription recovery", "account_id", id, "organization_id", orgID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare WhatsApp subscription recovery", nil, "")
		}
	}
	if a.WhatsApp == nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "WhatsApp subscription recovery is unavailable", nil, "")
	}

	// Meta runs outside a tenant transaction. The exact expected status and
	// token ciphertext fence the committed result against concurrent reconnects.
	ctx, cancel := context.WithTimeout(requestContext(r), 30*time.Second)
	defer cancel()
	subscriptionErr := a.WhatsApp.SubscribeApp(ctx, &whatsapp.Account{
		PhoneID:     snapshot.phoneID,
		BusinessID:  snapshot.businessID,
		APIVersion:  snapshot.apiVersion,
		AccessToken: snapshot.accessToken,
	})
	nextStatus := "active"
	if subscriptionErr != nil {
		nextStatus = "subscription_failed"
		a.Log.Warn("WhatsApp subscription recovery was not confirmed", "account_id", snapshot.accountID)
	}
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		result := scoped.DB.Model(&models.WhatsAppAccount{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND access_token = ?",
				snapshot.accountID,
				orgID,
				snapshot.expectedStatus,
				snapshot.accessTokenCiphertext,
			).
			Update("status", nextStatus)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errSubscriptionRecoverySuperseded
		}
		return nil
	})
	a.InvalidateWhatsAppAccountCache(snapshot.phoneID)
	if err != nil {
		if errors.Is(err, errSubscriptionRecoverySuperseded) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp subscription recovery was replaced by a newer request", nil, "")
		}
		a.Log.Error("Failed to persist WhatsApp subscription recovery status", "account_id", snapshot.accountID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Webhook subscription status could not be saved", nil, "")
	}
	if subscriptionErr != nil {
		return r.SendEnvelope(map[string]any{
			"success": false,
			"status":  "subscription_failed",
			"error":   "Failed to subscribe app to webhooks. Check your credentials.",
		})
	}

	a.Log.Info("App subscribed to webhooks successfully", "account_id", snapshot.accountID, "business_id", snapshot.businessID)
	return r.SendEnvelope(map[string]any{
		"success": true,
		"status":  "active",
		"message": "App subscribed to webhooks successfully. You should now receive incoming messages.",
	})
}

// ConfigurePhoneWebhookOverride configures Meta's alternate callback for one
// stored WhatsApp phone number. The endpoint has no body so callers cannot
// choose a callback, token, phone ID, WABA, or Graph API version.
func (a *App) ConfigurePhoneWebhookOverride(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
	if err != nil {
		return nil
	}
	// Rerouting inbound provider events changes a workspace-wide security
	// boundary. Accounts write alone is intentionally insufficient.
	if err := a.requirePermission(r, userID, models.ResourceSettingsIntegrations, models.ActionWrite); err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}
	if a.WhatsApp == nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "WhatsApp integration is unavailable", nil, "")
	}

	// Meta verifies the configured callback synchronously. Do not retain a
	// tenant transaction while Meta performs that verification, otherwise the
	// inbound GET may be blocked behind this request's database connection.
	// Load only the provider inputs in a short, committed tenant phase.
	type overrideSnapshot struct {
		accountID        uuid.UUID
		credentials      *whatsapp.Account
		workspaceManaged bool
	}
	var snapshot overrideSnapshot
	err = a.WithTenantApp(orgID, func(scoped *App) error {
		var account models.WhatsAppAccount
		if err := scoped.DB.Where("id = ? AND organization_id = ?", id, orgID).First(&account).Error; err != nil {
			return err
		}
		if strings.TrimSpace(account.Status) != "active" {
			return errPhoneWebhookOverrideAccountInactive
		}
		accessToken, err := crypto.Decrypt(account.AccessToken, scoped.integrationEncryptionKey())
		if err != nil || strings.TrimSpace(accessToken) == "" || crypto.IsEncrypted(accessToken) {
			return errPhoneWebhookOverrideCredentialsUnavailable
		}
		var organization models.Organization
		if err := scoped.DB.Select("id", "settings").Where("id = ?", orgID).First(&organization).Error; err != nil {
			return err
		}
		snapshot = overrideSnapshot{
			accountID: account.ID,
			credentials: &whatsapp.Account{
				PhoneID:     account.PhoneID,
				APIVersion:  account.APIVersion,
				AccessToken: accessToken,
			},
			workspaceManaged: metaWorkspaceAppManaged(&organization),
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Account not found", nil, "")
		case errors.Is(err, errPhoneWebhookOverrideAccountInactive):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account must be active before configuring its webhook", nil, "")
		case errors.Is(err, errPhoneWebhookOverrideCredentialsUnavailable):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account credentials are unavailable", nil, "")
		default:
			a.Log.Error("Failed to prepare phone webhook override", "error", err, "account_id", id, "organization_id", orgID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare phone webhook override", nil, "")
		}
	}

	// New Embedded Signup accounts intentionally do not copy the webhook token
	// into a phone-account row. Resolve the authoritative Integration Center or
	// platform credential in a separate short tenant phase instead. This action
	// must never fall back to a legacy account token: a workspace-managed callback
	// must be verified with that workspace's central token.
	verifyToken, authoritative, err := a.resolveMetaWebhookVerifyToken(orgID)
	if err != nil {
		if errors.Is(err, errMetaIntegrationDisabled) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Meta integration must be enabled before configuring its webhook", nil, "")
		}
		a.Log.Error("Failed to resolve phone webhook verification", "error", err, "account_id", snapshot.accountID, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Meta webhook verification is unavailable", nil, "")
	}
	if !authoritative || strings.TrimSpace(verifyToken) == "" {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account webhook verification is not configured", nil, "")
	}
	callbackURL := canonicalPhoneWebhookOrigin + metaWebhookCallbackPath(orgID, snapshot.workspaceManaged)

	ctx, cancel := context.WithTimeout(requestContext(r), 30*time.Second)
	defer cancel()
	if err := a.WhatsApp.ConfigurePhoneWebhookOverride(
		ctx,
		snapshot.credentials,
		callbackURL,
		verifyToken,
	); err != nil {
		a.Log.Warn("Failed to configure phone webhook override", "account_id", snapshot.accountID, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Meta could not verify the phone webhook override", nil, "")
	}

	// Record the external mutation in a separate short tenant phase. Neither
	// token is included in the audit data, response, or logs.
	if err := a.WithTenantApp(orgID, func(scoped *App) error {
		return audit.LogAudit(
			scoped.DB,
			orgID,
			userID,
			audit.GetUserName(scoped.DB, userID),
			"account",
			snapshot.accountID,
			models.AuditActionUpdated,
			nil,
			nil,
			map[string]any{
				"field":     "phone_webhook_override",
				"old_value": "not recorded",
				"new_value": "configured",
			},
		)
	}); err != nil {
		a.Log.Error("Failed to audit phone webhook override", "error", err, "account_id", snapshot.accountID, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Phone webhook override was configured but could not be audited", nil, "")
	}

	return r.SendEnvelope(map[string]any{
		"success": true,
		"message": "Phone-specific webhook override configured and verified.",
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

type embeddedSignupMetaSnapshot struct {
	appID      string
	appSecret  string
	configID   string
	apiVersion string
}

type embeddedSignupClaim struct {
	account            *models.WhatsAppAccount
	existing           bool
	old                *models.WhatsAppAccount
	priorStatus        string
	priorPINCiphertext string
	refreshOnly        bool
}

// ExchangeToken exchanges a temporary code and advances one exact workspace
// through committed onboarding phases. Provider mutations never run before an
// encrypted local phone claim has committed.
func (a *App) ExchangeToken(r *fastglue.Request) error {
	orgID, err := a.requireExplicitOrganization(r)
	if err != nil {
		return nil
	}

	var userID uuid.UUID
	var metaSnapshot embeddedSignupMetaSnapshot
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		resolvedOrgID, resolvedUserID, authErr := scoped.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
		if authErr != nil {
			return authErr
		}
		if resolvedOrgID != orgID {
			_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Selected organization is not available", nil, "")
			return errEnvelopeSent
		}
		appID, appSecret, configID, resolveErr := scoped.resolveMetaAppCreds(orgID)
		if resolveErr != nil {
			return resolveErr
		}
		userID = resolvedUserID
		metaSnapshot = embeddedSignupMetaSnapshot{
			appID:      strings.TrimSpace(appID),
			appSecret:  strings.TrimSpace(appSecret),
			configID:   strings.TrimSpace(configID),
			apiVersion: scoped.metaAPIVersion(),
		}
		return nil
	})
	if errors.Is(err, errEnvelopeSent) {
		return nil
	}
	if err != nil {
		if errors.Is(err, errMetaIntegrationDisabled) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Meta integration is disabled", nil, "")
		}
		a.Log.Error("Failed to prepare embedded signup credentials", "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to resolve credentials", nil, "")
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

	if strings.TrimSpace(req.Code) == "" {
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
	if a.WhatsApp == nil || metaSnapshot.appID == "" || metaSnapshot.appSecret == "" || metaSnapshot.apiVersion == "" {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Meta Embedded Signup is unavailable", nil, "")
	}

	// Provider read-only phase: exchange, validate the issuing app/scopes, and
	// prove the exact WABA/phone relationship before claiming anything.
	ctx, cancel := context.WithTimeout(requestContext(r), 60*time.Second)
	defer cancel()
	a.Log.Info("Exchanging code for access token")

	accessToken, err := a.WhatsApp.ExchangeCodeForToken(
		ctx,
		strings.TrimSpace(req.Code),
		metaSnapshot.appID,
		metaSnapshot.appSecret,
		metaSnapshot.apiVersion,
	)
	if err != nil {
		a.Log.Warn("Meta authorization code exchange was rejected")
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Meta authorization code exchange failed", nil, "")
	}

	phoneID, wabaID, name, tokenExpiresAt, phoneInfo, err := a.discoverWABAAndPhone(
		ctx,
		accessToken,
		req.PhoneID,
		req.WABAID,
		req.Name,
		metaSnapshot,
	)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	// Reconnecting the exact active account is a credential rotation, not a
	// registration. Before permitting that zero-mutation path, prove with a
	// read-only Graph GET that the Integration Center app is already subscribed
	// to this exact WABA. Never POST subscribed_apps during a token rotation.
	activeRefreshCandidate, err := a.embeddedSignupActiveRefreshCandidate(
		orgID,
		phoneID,
		wabaID,
		metaSnapshot.apiVersion,
		metaSnapshot,
	)
	if err != nil {
		a.Log.Error("Failed to inspect active WhatsApp reconnect candidate", "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to inspect the existing WhatsApp account", nil, "")
	}
	activeAppSubscriptionProven := false
	if activeRefreshCandidate {
		activeAppSubscriptionProven, err = a.WhatsApp.IsAppSubscribed(
			ctx,
			wabaID,
			metaSnapshot.appID,
			accessToken,
			metaSnapshot.apiVersion,
		)
		if err != nil {
			a.Log.Warn("Failed to verify existing WABA app subscription", "organization_id", orgID)
			return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Meta could not confirm the existing app subscription; the active account was not changed", nil, "")
		}
		if !activeAppSubscriptionProven {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "The current Meta app is not subscribed to this WhatsApp Business Account; the active account was not changed", nil, "")
		}
	}

	// Generate the exact non-SMB registration PIN before the local claim is
	// committed. The claim encrypts it before any Meta mutation, so an
	// externally successful registration remains recoverable even if a later
	// status write or process restart fails.
	registrationPIN := ""
	if !embeddedSignupPhoneIsSMB(phoneInfo) {
		registrationPIN, err = generateNumericPIN(6)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare WhatsApp account", nil, "")
		}
	}

	claim, err := a.claimEmbeddedSignupAccount(
		orgID,
		userID,
		phoneID,
		wabaID,
		name,
		accessToken,
		tokenExpiresAt,
		metaSnapshot.apiVersion,
		phoneInfo,
		registrationPIN,
		metaSnapshot,
		activeAppSubscriptionProven,
	)
	if err != nil {
		switch {
		case errors.Is(err, errWhatsAppPhoneAlreadyClaimed):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "This WhatsApp phone number is already connected", nil, "")
		case errors.Is(err, errEmbeddedSignupConfigurationChanged), errors.Is(err, errMetaIntegrationDisabled):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Meta integration settings changed; restart the connection", nil, "")
		case errors.Is(err, errEmbeddedSignupClaimInProgress):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "A WhatsApp connection for this number is already pending reconciliation", nil, "")
		case errors.Is(err, errEmbeddedSignupAppSubscriptionNotProven):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "The existing WhatsApp app subscription could not be proven; the active account was not changed", nil, "")
		case errors.Is(err, errEmbeddedSignupClaimSuperseded):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp connection changed; reload and retry", nil, "")
		}
		a.Log.Error("Failed to persist embedded signup phone claim", "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to save WhatsApp account", nil, "")
	}
	account := claim.account
	claimCiphertext := account.AccessToken
	// The global phone-keyed cache may still contain the pre-claim credentials,
	// status, or tenant association. Evict it immediately after the durable
	// claim, before Meta can emit registration/subscription webhooks.
	a.InvalidateWhatsAppAccountCache(account.PhoneID)
	if account.IsSMB {
		registrationPIN = ""
	}

	var regErr error
	if !claim.refreshOnly {
		// Provider mutation phase 1. The committed pending_registration row above
		// owns the phone and its encrypted PIN before Meta registration can change
		// external state. An exact active-account token refresh never enters this
		// branch: /register is non-idempotent and is unnecessary for that case.
		regErr = a.attemptEmbeddedSignupRegistration(ctx, account, accessToken, registrationPIN)
		registrationStatus := "pending_registration"
		var registrationPINOverride *string
		if regErr == nil {
			registrationStatus = "pending_subscription"
		} else if claim.priorStatus != "" && whatsapp.IsDefiniteProviderRejection(regErr) {
			// A non-active legacy reconnect can remain usable when Meta definitively
			// rejects registration. Restore its prior readiness and accepted PIN.
			registrationStatus = claim.priorStatus
			priorPINCiphertext := claim.priorPINCiphertext
			registrationPINOverride = &priorPINCiphertext
		}
		account, err = a.persistEmbeddedSignupStatus(
			orgID,
			account.ID,
			claimCiphertext,
			"pending_registration",
			registrationStatus,
			registrationPINOverride,
		)
		if err != nil {
			if errors.Is(err, errEmbeddedSignupClaimSuperseded) {
				return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp connection was replaced by a newer request", nil, "")
			}
			a.Log.Error("Failed to persist embedded signup registration state", "account_id", account.ID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "WhatsApp account was claimed, but registration status could not be saved", nil, "")
		}
	}

	var subscriptionErr error
	if !claim.refreshOnly && regErr == nil {
		// Provider mutation phase 2. pending_subscription is already committed,
		// so a successful subscription always has a durable reconciliation row.
		runtimeAccount := &whatsapp.Account{
			PhoneID:     phoneID,
			BusinessID:  wabaID,
			APIVersion:  metaSnapshot.apiVersion,
			AccessToken: accessToken,
		}
		subscriptionErr = a.WhatsApp.SubscribeApp(ctx, runtimeAccount)
		finalStatus := "active"
		if subscriptionErr != nil {
			finalStatus = "subscription_failed"
			a.Log.Warn("WhatsApp webhook subscription failed", "account_id", account.ID, "business_id", wabaID)
		}
		account, err = a.persistEmbeddedSignupStatus(
			orgID,
			account.ID,
			claimCiphertext,
			"pending_subscription",
			finalStatus,
			nil,
		)
		if err != nil {
			if errors.Is(err, errEmbeddedSignupClaimSuperseded) {
				return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp connection was replaced by a newer request", nil, "")
			}
			a.Log.Error("Failed to persist embedded signup subscription state", "account_id", account.ID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "WhatsApp account was saved, but subscription status could not be recorded", nil, "")
		}
	}

	a.InvalidateWhatsAppAccountCache(account.PhoneID)

	a.Log.Info("WhatsApp account connected via embedded signup successfully",
		"account_id", account.ID,
		"phone_id", account.PhoneID,
		"status", account.Status)

	if auditErr := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		if err := scoped.DB.Preload("CreatedBy").Preload("UpdatedBy").
			Where("id = ? AND organization_id = ?", account.ID, orgID).
			First(account).Error; err != nil {
			return err
		}
		auditAction := models.AuditActionCreated
		var auditOld any
		if claim.existing {
			auditAction = models.AuditActionUpdated
			auditOld = claim.old
		}
		scoped.logAudit(orgID, userID, "account", account.ID, auditAction, auditOld, account)
		return nil
	}); auditErr != nil {
		a.Log.Error("Failed to reload or audit embedded signup account", "account_id", account.ID)
	}

	out := map[string]any{
		"account": accountToResponse(*account),
	}
	warnings := make([]string, 0, 2)
	if regErr != nil {
		warnings = append(warnings, "Registration could not be confirmed; keep this account pending and reconcile it before retrying")
	}
	if subscriptionErr != nil {
		warnings = append(warnings, "Webhook subscription failed; use the account Subscribe action after checking Meta permissions")
	}
	if len(warnings) > 0 {
		out["warning"] = strings.Join(warnings, "; ")
	}

	return r.SendEnvelope(out)
}

func embeddedSignupAccountName(name, phoneID string, phoneInfo *whatsapp.PhoneNumberInfo) (string, error) {
	name = strings.TrimSpace(name)
	if name != "" {
		return name, nil
	}
	if phoneInfo != nil && strings.TrimSpace(phoneInfo.VerifiedName) != "" {
		suffix, err := generateNumericPIN(4)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s %s", strings.TrimSpace(phoneInfo.VerifiedName), suffix), nil
	}
	suffix := strings.TrimSpace(phoneID)
	if len(suffix) > 4 {
		suffix = suffix[len(suffix)-4:]
	}
	return "WhatsApp Account " + suffix, nil
}

func embeddedSignupPhoneIsSMB(phoneInfo *whatsapp.PhoneNumberInfo) bool {
	return phoneInfo != nil &&
		(phoneInfo.IsOnBizApp || phoneInfo.PlatformType == "SMB" || phoneInfo.PlatformType == "SMB_CLOUD_API")
}

func embeddedSignupPhoneClaimUniqueViolation(err error) bool {
	if err == nil || !isUniqueViolation(err) {
		return false
	}
	// Only the global routing identity constraint is a phone-claim conflict.
	// A tenant-local name collision must not be mislabeled as another tenant
	// owning the phone number.
	return strings.Contains(
		strings.ToLower(err.Error()),
		"uq_whatsapp_accounts_live_phone_id",
	)
}

func (a *App) embeddedSignupActiveReconnectContractMatches(
	account *models.WhatsAppAccount,
	phoneID, wabaID, apiVersion string,
	metaSnapshot embeddedSignupMetaSnapshot,
) (bool, error) {
	if account == nil || account.DeletedAt.Valid || strings.TrimSpace(account.Status) != "active" {
		return false, nil
	}
	if strings.TrimSpace(account.PhoneID) != strings.TrimSpace(phoneID) ||
		strings.TrimSpace(account.BusinessID) != strings.TrimSpace(wabaID) ||
		strings.TrimSpace(account.APIVersion) != strings.TrimSpace(apiVersion) {
		return false, nil
	}

	// New managed accounts intentionally keep these deprecated columns empty;
	// their app contract is the fenced Integration Center snapshot. If a legacy
	// row still pins account-level app credentials, both values must exactly
	// match that same snapshot before it can use the refresh-only path.
	legacyAppID := strings.TrimSpace(account.AppID)
	legacyAppSecret := strings.TrimSpace(account.AppSecret)
	if legacyAppID == "" && legacyAppSecret == "" {
		return true, nil
	}
	if legacyAppID == "" || legacyAppSecret == "" || legacyAppID != metaSnapshot.appID {
		return false, nil
	}
	decryptedSecret, err := a.decryptLegacyMetaAccountSecret(legacyAppSecret)
	if err != nil {
		return false, err
	}
	return decryptedSecret == metaSnapshot.appSecret, nil
}

// lockMetaIntegrationContract serializes an Embedded Signup commit with
// Integration Center updates. Keep this lock order identical to
// updateIntegration: Organization first, then ProviderIntegration. Locking the
// organization also fences creation of the first provider row.
func lockMetaIntegrationContract(db *gorm.DB, orgID uuid.UUID) error {
	var organization models.Organization
	if err := db.Select("id").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", orgID).
		First(&organization).Error; err != nil {
		return err
	}

	var integration models.ProviderIntegration
	err := db.Select("id").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("organization_id = ? AND provider = ?", orgID, integrationProviderMeta).
		First(&integration).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	return err
}

// embeddedSignupActiveRefreshCandidate performs only a local read. The caller
// must separately prove the currently configured app is already subscribed to
// the WABA before the final locked refresh claim can replace credentials.
func (a *App) embeddedSignupActiveRefreshCandidate(
	orgID uuid.UUID,
	phoneID, wabaID, apiVersion string,
	metaSnapshot embeddedSignupMetaSnapshot,
) (bool, error) {
	var matches bool
	err := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var account models.WhatsAppAccount
		if err := scoped.DB.Where(
			"organization_id = ? AND BTRIM(phone_id) = BTRIM(?) AND deleted_at IS NULL AND status = ?",
			orgID,
			strings.TrimSpace(phoneID),
			"active",
		).First(&account).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		var matchErr error
		matches, matchErr = scoped.embeddedSignupActiveReconnectContractMatches(
			&account,
			phoneID,
			wabaID,
			apiVersion,
			metaSnapshot,
		)
		return matchErr
	})
	return matches, err
}

func (a *App) claimEmbeddedSignupAccount(
	orgID, userID uuid.UUID,
	phoneID, wabaID, name, accessToken string,
	tokenExpiresAt *time.Time,
	apiVersion string,
	phoneInfo *whatsapp.PhoneNumberInfo,
	registrationPIN string,
	metaSnapshot embeddedSignupMetaSnapshot,
	activeAppSubscriptionProven bool,
) (*embeddedSignupClaim, error) {
	phoneID = strings.TrimSpace(phoneID)
	wabaID = strings.TrimSpace(wabaID)
	name = strings.TrimSpace(name)
	registrationPIN = strings.TrimSpace(registrationPIN)
	if phoneID == "" || wabaID == "" || strings.TrimSpace(accessToken) == "" {
		return nil, errors.New("embedded signup account claim is incomplete")
	}

	claim := &embeddedSignupClaim{}
	err := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		// Fence the read-only Meta validation phase against a concurrent
		// Integration Center credential or API-version change. No local claim
		// and no provider mutation may proceed with a stale app snapshot.
		if lockErr := lockMetaIntegrationContract(scoped.DB, orgID); lockErr != nil {
			return lockErr
		}
		currentAppID, currentAppSecret, currentConfigID, resolveErr := scoped.resolveMetaAppCreds(orgID)
		if resolveErr != nil {
			return resolveErr
		}
		if strings.TrimSpace(currentAppID) != metaSnapshot.appID ||
			strings.TrimSpace(currentAppSecret) != metaSnapshot.appSecret ||
			strings.TrimSpace(currentConfigID) != metaSnapshot.configID ||
			scoped.metaAPIVersion() != metaSnapshot.apiVersion {
			return errEmbeddedSignupConfigurationChanged
		}

		var account models.WhatsAppAccount
		lookupErr := scoped.DB.Unscoped().
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND BTRIM(phone_id) = BTRIM(?)", orgID, phoneID).
			Order("CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END, updated_at DESC, created_at DESC, id DESC").
			First(&account).Error
		initializeOperationalFields := false
		switch {
		case lookupErr == nil:
			if !account.DeletedAt.Valid &&
				(account.Status == "pending_registration" || account.Status == "pending_subscription") {
				return errEmbeddedSignupClaimInProgress
			}
			claim.existing = true
			old := account
			claim.old = &old

			refreshOnly, contractErr := scoped.embeddedSignupActiveReconnectContractMatches(
				&account,
				phoneID,
				wabaID,
				apiVersion,
				metaSnapshot,
			)
			if contractErr != nil {
				return contractErr
			}
			if !account.DeletedAt.Valid && strings.TrimSpace(account.Status) == "active" {
				if !refreshOnly {
					// A live row with the same global phone identity but a different
					// WABA/API/app contract is not a token refresh. Never rewrite it
					// through Embedded Signup.
					return errWhatsAppPhoneAlreadyClaimed
				}
				if !activeAppSubscriptionProven {
					// A managed row does not persist its app ID. The read-only WABA
					// subscription proof binds it to the current Integration Center
					// app without changing Meta subscription configuration.
					return errEmbeddedSignupAppSubscriptionNotProven
				}
				if strings.TrimSpace(scoped.integrationEncryptionKey()) == "" {
					return errAccountEncryptionUnavailable
				}
				accessTokenCiphertext, encryptErr := crypto.Encrypt(
					strings.TrimSpace(accessToken),
					scoped.integrationEncryptionKey(),
				)
				if encryptErr != nil || !crypto.IsEncrypted(accessTokenCiphertext) {
					return errAccountEncryptionUnavailable
				}
				result := scoped.DB.Model(&models.WhatsAppAccount{}).
					Where(
						"id = ? AND organization_id = ? AND deleted_at IS NULL AND status = ? AND updated_at = ? AND phone_id = ? AND business_id = ? AND api_version = ? AND access_token = ? AND pin = ?",
						account.ID,
						orgID,
						"active",
						account.UpdatedAt,
						account.PhoneID,
						account.BusinessID,
						account.APIVersion,
						account.AccessToken,
						account.Pin,
					).
					Updates(map[string]any{
						"access_token":            accessTokenCiphertext,
						"access_token_expires_at": tokenExpiresAt,
						"updated_by_id":           userID,
					})
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return errEmbeddedSignupClaimSuperseded
				}
				if loadErr := scoped.DB.Where(
					"id = ? AND organization_id = ?",
					account.ID,
					orgID,
				).First(&account).Error; loadErr != nil {
					return loadErr
				}
				claim.account = &account
				claim.refreshOnly = true
				return nil
			}
			initializeOperationalFields = account.DeletedAt.Valid
			if !account.DeletedAt.Valid {
				claim.priorStatus = strings.TrimSpace(account.Status)
				claim.priorPINCiphertext = account.Pin
				if strings.TrimSpace(claim.priorPINCiphertext) != "" && !crypto.IsEncrypted(claim.priorPINCiphertext) {
					var encryptErr error
					claim.priorPINCiphertext, encryptErr = crypto.Encrypt(
						claim.priorPINCiphertext,
						scoped.integrationEncryptionKey(),
					)
					if encryptErr != nil || !crypto.IsEncrypted(claim.priorPINCiphertext) {
						return errAccountEncryptionUnavailable
					}
				}
			}
		case errors.Is(lookupErr, gorm.ErrRecordNotFound):
			account.ID = uuid.New()
			account.CreatedByID = &userID
			initializeOperationalFields = true
		default:
			return lookupErr
		}

		if !claim.existing || strings.TrimSpace(account.Name) == "" {
			preparedName, nameErr := embeddedSignupAccountName(name, phoneID, phoneInfo)
			if nameErr != nil {
				return nameErr
			}
			account.Name = preparedName
		}
		if initializeOperationalFields {
			account.IsDefaultIncoming = false
			account.IsDefaultOutgoing = false
			account.AutoReadReceipt = false
			account.BusinessCallingEnabled = false
			account.IsSMB = embeddedSignupPhoneIsSMB(phoneInfo)
		} else if phoneInfo != nil {
			// IsSMB is provider classification, not a user-controlled routing
			// preference. Refresh it only when Meta returned current phone info.
			account.IsSMB = embeddedSignupPhoneIsSMB(phoneInfo)
		}

		account.OrganizationID = orgID
		account.PhoneID = phoneID
		account.BusinessID = wabaID
		account.AccessToken = accessToken
		account.AccessTokenExpiresAt = tokenExpiresAt
		account.APIVersion = strings.TrimSpace(apiVersion)
		account.Status = "pending_registration"
		account.Pin = registrationPIN
		if account.IsSMB {
			account.Pin = ""
		}
		account.UpdatedByID = &userID
		account.DeletedAt = gorm.DeletedAt{}

		if err := scoped.encryptAccountSecrets(&account); err != nil {
			return err
		}
		var saveErr error
		if claim.existing {
			saveErr = scoped.DB.Unscoped().Save(&account).Error
		} else {
			saveErr = scoped.DB.Create(&account).Error
		}
		if saveErr != nil {
			if embeddedSignupPhoneClaimUniqueViolation(saveErr) {
				return errWhatsAppPhoneAlreadyClaimed
			}
			return saveErr
		}
		claim.account = &account
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claim, nil
}

func (a *App) persistEmbeddedSignupStatus(
	orgID, accountID uuid.UUID,
	claimCiphertext, expectedStatus, nextStatus string,
	pinCiphertextOverride *string,
) (*models.WhatsAppAccount, error) {
	var account models.WhatsAppAccount
	err := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		updates := map[string]any{"status": nextStatus}
		if pinCiphertextOverride != nil {
			pinCiphertext := strings.TrimSpace(*pinCiphertextOverride)
			if pinCiphertext != "" && !crypto.IsEncrypted(pinCiphertext) {
				return errAccountEncryptionUnavailable
			}
			updates["pin"] = pinCiphertext
		}
		result := scoped.DB.Model(&models.WhatsAppAccount{}).
			Where(
				"id = ? AND organization_id = ? AND access_token = ? AND status = ?",
				accountID,
				orgID,
				claimCiphertext,
				expectedStatus,
			).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errEmbeddedSignupClaimSuperseded
		}
		return scoped.DB.Where("id = ? AND organization_id = ?", accountID, orgID).First(&account).Error
	})
	if err != nil {
		return nil, err
	}
	return &account, nil
}

func (a *App) discoverWABAAndPhone(
	ctx context.Context,
	accessToken, phoneID, wabaID, name string,
	metaSnapshot embeddedSignupMetaSnapshot,
) (string, string, string, *time.Time, *whatsapp.PhoneNumberInfo, error) {
	a.Log.Info("Validating embedded signup token via debug_token")

	debugInfo, tokenExpiresAt, err := a.debugAndValidateMetaAccessTokenWithCredentials(
		ctx,
		accessToken,
		metaSnapshot.appID,
		metaSnapshot.appSecret,
	)
	if err != nil {
		return "", "", "", nil, nil, err
	}
	discoveredWABAIDs := make(map[string]struct{})
	messagingTargetIDs := make(map[string]struct{})
	for _, scope := range debugInfo.GranularScopes {
		scopeName := strings.TrimSpace(scope.Scope)
		if scopeName != "whatsapp_business_management" && scopeName != "whatsapp_business_messaging" {
			continue
		}
		for _, targetID := range scope.TargetIds {
			targetID = strings.TrimSpace(targetID)
			if targetID == "" {
				continue
			}
			if scopeName == "whatsapp_business_management" {
				discoveredWABAIDs[targetID] = struct{}{}
			} else {
				messagingTargetIDs[targetID] = struct{}{}
			}
		}
	}

	wabaID = strings.TrimSpace(wabaID)
	if wabaID == "" {
		// Find the WABA ID from the token's WhatsApp granular scope only when
		// Embedded Signup did not already provide one. The Graph /me/accounts
		// edge returns Facebook Pages, not WhatsApp Business Accounts, so it is
		// not a safe discovery fallback.
		switch len(discoveredWABAIDs) {
		case 0:
			return "", "", "", nil, nil, fmt.Errorf("embedded signup did not provide a WhatsApp Business Account ID; reconnect and complete the WhatsApp account selection")
		case 1:
			for discoveredWABAID := range discoveredWABAIDs {
				wabaID = discoveredWABAID
			}
		default:
			return "", "", "", nil, nil, fmt.Errorf("embedded signup token grants access to multiple WhatsApp Business Accounts; reconnect and select exactly one account")
		}

		a.Log.Info("Discovered WABA ID", "waba_id", wabaID)
	} else if _, granted := discoveredWABAIDs[wabaID]; !granted {
		return "", "", "", nil, nil, errors.New("the selected WhatsApp Business Account is not granted by the embedded signup token")
	}
	if len(messagingTargetIDs) > 0 {
		if _, granted := messagingTargetIDs[wabaID]; !granted {
			return "", "", "", nil, nil, errors.New("the selected WhatsApp Business Account is not granted for messaging by the embedded signup token")
		}
	}

	phoneID = strings.TrimSpace(phoneID)
	if phoneID == "" {
		phonesResp, err := a.WhatsApp.GetWABAPhoneNumbers(ctx, wabaID, accessToken, metaSnapshot.apiVersion)
		if err != nil {
			a.Log.Error("Failed to fetch phone numbers from Meta", "error", err)
			return "", "", "", nil, nil, fmt.Errorf("failed to fetch phone numbers from WABA: %w", err)
		}

		if len(phonesResp.Data) == 0 {
			return "", "", "", nil, nil, fmt.Errorf("no phone numbers found in this WhatsApp Business Account")
		}

		if len(phonesResp.Data) > 1 {
			return "", "", "", nil, nil, fmt.Errorf("multiple phone numbers found in this WhatsApp Business Account; reconnect and select exactly one phone number")
		}

		phone := phonesResp.Data[0]
		phoneID = strings.TrimSpace(phone.ID)
		if phoneID == "" {
			return "", "", "", nil, nil, fmt.Errorf("meta returned a phone number without an ID")
		}
		name = fmt.Sprintf("%s (%s)", phone.VerifiedName, phone.DisplayPhoneNumber)
		a.Log.Info("Discovered Phone ID", "phone_id", phoneID)
	}

	// Even when the browser supplied both IDs, verify the token can read each
	// object and that Meta lists this exact Phone ID under this exact WABA.
	// This prevents Embedded Signup payloads from bypassing the same contract
	// enforced by manual account creation and credential updates.
	validation, err := a.validateWhatsAppAccountContract(
		ctx,
		phoneID,
		wabaID,
		accessToken,
		metaSnapshot.apiVersion,
	)
	if err != nil {
		return "", "", "", nil, nil, err
	}

	phoneInfo := &whatsapp.PhoneNumberInfo{
		VerifiedName:       validation.VerifiedName,
		DisplayPhoneNumber: validation.PhoneNumber,
		QualityRating:      validation.QualityRating,
		IsOnBizApp:         validation.IsOnBizApp,
		PlatformType:       validation.PlatformType,
	}
	return phoneID, wabaID, name, tokenExpiresAt, phoneInfo, nil
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
	return a.debugAndValidateMetaAccessTokenWithCredentials(ctx, accessToken, appID, appSecret)
}

func (a *App) debugAndValidateMetaAccessTokenWithCredentials(
	ctx context.Context,
	accessToken, appID, appSecret string,
) (*whatsapp.TokenDebugInfo, *time.Time, error) {
	appID = strings.TrimSpace(appID)
	appSecret = strings.TrimSpace(appSecret)
	if appID == "" {
		return nil, nil, errMetaAppIDNotConfigured
	}
	if appSecret == "" {
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

func (a *App) attemptEmbeddedSignupRegistration(
	ctx context.Context,
	account *models.WhatsAppAccount,
	accessToken, registrationPIN string,
) error {
	if account.IsSMB {
		a.Log.Info("SMB account detected via Meta API; registration call skipped", "phone_id", account.PhoneID)
		return nil
	}
	if len(registrationPIN) != 6 {
		return errors.New("embedded signup registration PIN is unavailable")
	}

	a.Log.Info("Attempting phone number auto-registration", "phone_id", account.PhoneID)
	regErr := a.WhatsApp.RegisterPhoneNumber(ctx, account.PhoneID, registrationPIN, accessToken, account.APIVersion)

	if regErr == nil {
		a.Log.Info("Phone number auto-registration successful", "phone_id", account.PhoneID)
	} else {
		a.Log.Warn("Phone number auto-registration failed",
			"phone_id", account.PhoneID,
			"definite_provider_rejection", whatsapp.IsDefiniteProviderRejection(regErr))
	}

	return regErr
}

// RegisterPhoneNumber reconciles a durable Embedded Signup registration claim.
// The exact PIN was encrypted and committed before the original Meta mutation;
// this endpoint only reuses that candidate and never accepts or reveals a PIN.
func (a *App) RegisterPhoneNumber(r *fastglue.Request) error {
	orgID, err := a.requireExplicitOrganization(r)
	if err != nil {
		return nil
	}

	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}

	// An empty JSON object is tolerated for older clients, but caller-selected
	// PINs or any other recovery inputs are deliberately unsupported.
	if body := strings.TrimSpace(string(r.RequestCtx.PostBody())); body != "" && body != "null" {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &fields); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
		}
		if len(fields) != 0 {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Registration recovery does not accept request fields", nil, "")
		}
	}
	type registrationRecoverySnapshot struct {
		accountID             uuid.UUID
		phoneID               string
		apiVersion            string
		accessToken           string
		accessTokenCiphertext string
		pin                   string
		pinCiphertext         string
		attemptedAt           time.Time
		oldAccount            models.WhatsAppAccount
	}
	var snapshot registrationRecoverySnapshot
	var userID uuid.UUID

	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		resolvedOrgID, resolvedUserID, authErr := scoped.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
		if authErr != nil {
			return authErr
		}
		if resolvedOrgID != orgID {
			_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Selected organization is not available", nil, "")
			return errEnvelopeSent
		}
		userID = resolvedUserID

		var account models.WhatsAppAccount
		if loadErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", id, orgID).
			First(&account).Error; loadErr != nil {
			return loadErr
		}
		if strings.TrimSpace(account.Status) != "pending_registration" || account.IsSMB {
			return errRegistrationRecoveryStatus
		}
		oldAccount := account

		encryptionKey := strings.TrimSpace(scoped.integrationEncryptionKey())
		if encryptionKey == "" {
			return errAccountEncryptionUnavailable
		}
		accessToken, decryptErr := crypto.Decrypt(account.AccessToken, encryptionKey)
		if decryptErr != nil || strings.TrimSpace(accessToken) == "" || crypto.IsEncrypted(accessToken) {
			return errRegistrationRecoveryCredentials
		}
		pin, decryptErr := crypto.Decrypt(account.Pin, encryptionKey)
		if decryptErr != nil || !isSixDigitPIN(pin) {
			return errRegistrationRecoveryCredentials
		}

		// Normalize any legacy plaintext claim and always advance a durable
		// registration-attempt watermark before Meta sees the candidate. Inbound
		// evidence predating this exact attempt can therefore never reconcile a
		// later ambiguous /register result. No candidate is generated here.
		accessTokenCiphertext := account.AccessToken
		pinCiphertext := account.Pin
		if !crypto.IsEncrypted(accessTokenCiphertext) || !crypto.IsEncrypted(pinCiphertext) {
			accessTokenCiphertext, decryptErr = crypto.Encrypt(accessToken, encryptionKey)
			if decryptErr != nil {
				return decryptErr
			}
			pinCiphertext, decryptErr = crypto.Encrypt(pin, encryptionKey)
			if decryptErr != nil {
				return decryptErr
			}
		}
		attemptedAt := time.Now().UTC().Truncate(time.Microsecond)
		if !attemptedAt.After(account.UpdatedAt) {
			// PostgreSQL timestamps are microsecond-precision. Preserve strict
			// monotonicity even when an application clock moves backwards or the
			// stored row is slightly ahead of this replica's clock.
			attemptedAt = account.UpdatedAt.UTC().Truncate(time.Microsecond).Add(time.Microsecond)
		}
		result := scoped.DB.Model(&models.WhatsAppAccount{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND updated_at = ? AND access_token = ? AND pin = ?",
				account.ID,
				orgID,
				"pending_registration",
				account.UpdatedAt,
				account.AccessToken,
				account.Pin,
			).
			UpdateColumns(map[string]any{
				"access_token":  accessTokenCiphertext,
				"pin":           pinCiphertext,
				"updated_by_id": userID,
				"updated_at":    attemptedAt,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errRegistrationRecoverySuperseded
		}
		account.AccessToken = accessTokenCiphertext
		account.Pin = pinCiphertext
		account.UpdatedAt = attemptedAt
		account.UpdatedByID = &userID

		snapshot = registrationRecoverySnapshot{
			accountID:             account.ID,
			phoneID:               strings.TrimSpace(account.PhoneID),
			apiVersion:            strings.TrimSpace(account.APIVersion),
			accessToken:           strings.TrimSpace(accessToken),
			accessTokenCiphertext: accessTokenCiphertext,
			pin:                   pin,
			pinCiphertext:         pinCiphertext,
			attemptedAt:           attemptedAt,
			oldAccount:            oldAccount,
		}
		return nil
	})
	if errors.Is(err, errEnvelopeSent) {
		return nil
	}
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Account not found", nil, "")
		case errors.Is(err, errRegistrationRecoveryStatus):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account is not pending phone registration", nil, "")
		case errors.Is(err, errRegistrationRecoveryCredentials):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp registration recovery is unavailable for this account", nil, "")
		case errors.Is(err, errAccountEncryptionUnavailable):
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Account credential storage is unavailable", nil, "")
		case errors.Is(err, errRegistrationRecoverySuperseded):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp registration recovery changed; reload and retry", nil, "")
		default:
			a.Log.Error("Failed to prepare WhatsApp registration recovery", "account_id", id, "organization_id", orgID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare WhatsApp registration recovery", nil, "")
		}
	}
	if a.WhatsApp == nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "WhatsApp registration recovery is unavailable", nil, "")
	}

	// The non-idempotent provider mutation runs without a database transaction.
	// Any failure remains ambiguous and deliberately leaves the durable claim,
	// encrypted candidate, and pending status unchanged for another safe retry.
	a.InvalidateWhatsAppAccountCache(snapshot.phoneID)
	ctx, cancel := context.WithTimeout(requestContext(r), 30*time.Second)
	defer cancel()
	if regErr := a.WhatsApp.RegisterPhoneNumber(
		ctx,
		snapshot.phoneID,
		snapshot.pin,
		snapshot.accessToken,
		snapshot.apiVersion,
	); regErr != nil {
		a.Log.Warn("WhatsApp registration recovery was not confirmed", "account_id", snapshot.accountID)
		return r.SendErrorEnvelope(
			fasthttp.StatusBadGateway,
			"WhatsApp registration could not be confirmed; the account remains pending registration",
			nil,
			"",
		)
	}

	var updated models.WhatsAppAccount
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		result := scoped.DB.Model(&models.WhatsAppAccount{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND updated_at = ? AND access_token = ? AND pin = ?",
				snapshot.accountID,
				orgID,
				"pending_registration",
				snapshot.attemptedAt,
				snapshot.accessTokenCiphertext,
				snapshot.pinCiphertext,
			).
			Updates(map[string]any{
				"status":        "pending_subscription",
				"updated_by_id": userID,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errRegistrationRecoverySuperseded
		}
		if loadErr := scoped.DB.Preload("CreatedBy").Preload("UpdatedBy").
			Where("id = ? AND organization_id = ?", snapshot.accountID, orgID).
			First(&updated).Error; loadErr != nil {
			return loadErr
		}
		scoped.logAudit(
			orgID,
			userID,
			"account",
			updated.ID,
			models.AuditActionUpdated,
			&snapshot.oldAccount,
			&updated,
		)
		return nil
	})
	a.InvalidateWhatsAppAccountCache(snapshot.phoneID)
	if err != nil {
		if errors.Is(err, errRegistrationRecoverySuperseded) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp registration recovery was replaced by a newer request", nil, "")
		}
		a.Log.Error("Failed to persist WhatsApp registration recovery status", "account_id", snapshot.accountID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Registration succeeded but its status could not be saved", nil, "")
	}

	return r.SendEnvelope(map[string]any{
		"success": true,
		"message": "Phone registration confirmed. Subscribe the account to finish setup.",
		"status":  "pending_subscription",
		"account": accountToResponse(updated),
	})
}

type registrationReconciliationSnapshot struct {
	accountID             uuid.UUID
	name                  string
	phoneID               string
	rawPhoneID            string
	businessID            string
	rawBusinessID         string
	apiVersion            string
	rawAPIVersion         string
	accessToken           string
	accessTokenCiphertext string
	accessTokenExpiresAt  *time.Time
	pinCiphertext         string
	pendingSince          time.Time
	updatedAt             time.Time
	meta                  embeddedSignupMetaSnapshot
	oldAccount            models.WhatsAppAccount
}

func embeddedSignupTokenGrantsWABA(info *whatsapp.TokenDebugInfo, wabaID string) error {
	if info == nil || strings.TrimSpace(wabaID) == "" {
		return errRegistrationReconciliationTokenMismatch
	}
	wabaID = strings.TrimSpace(wabaID)
	managementTargets := make(map[string]struct{})
	messagingTargets := make(map[string]struct{})
	for _, scope := range info.GranularScopes {
		targets := managementTargets
		switch strings.TrimSpace(scope.Scope) {
		case "whatsapp_business_management":
		case "whatsapp_business_messaging":
			targets = messagingTargets
		default:
			continue
		}
		for _, targetID := range scope.TargetIds {
			targetID = strings.TrimSpace(targetID)
			if targetID != "" {
				targets[targetID] = struct{}{}
			}
		}
	}
	if _, granted := managementTargets[wabaID]; !granted {
		return errors.New("the stored token is not granted to this WhatsApp Business Account")
	}
	if len(messagingTargets) > 0 {
		if _, granted := messagingTargets[wabaID]; !granted {
			return errors.New("the stored token is not granted to message for this WhatsApp Business Account")
		}
	}
	return nil
}

func sameMetaTokenExpiry(stored, validated *time.Time) bool {
	if stored == nil || validated == nil {
		return stored == nil && validated == nil
	}
	return stored.UTC().Unix() == validated.UTC().Unix()
}

func recentInboundRegistrationEvidence(
	db *gorm.DB,
	orgID uuid.UUID,
	accountName, phoneID string,
	pendingSince, now time.Time,
) (*models.Message, error) {
	if db == nil || orgID == uuid.Nil || strings.TrimSpace(accountName) == "" ||
		strings.TrimSpace(phoneID) == "" || pendingSince.IsZero() {
		return nil, errRegistrationReconciliationEvidenceMissing
	}
	now = now.UTC()
	pendingSince = pendingSince.UTC()
	if pendingSince.After(now.Add(time.Minute)) {
		return nil, errRegistrationReconciliationEvidenceMissing
	}
	evidenceSince := pendingSince
	if recentCutoff := now.Add(-registrationReconciliationEvidenceWindow); evidenceSince.Before(recentCutoff) {
		evidenceSince = recentCutoff
	}

	// These jobs are created atomically with webhook-persisted inbound messages.
	// Their payload retains the exact Meta phone_number_id while AggregateID
	// links to the exact incoming message and account routing name.
	var jobs []models.ScheduledJob
	if err := db.Where(
		"organization_id = ? AND kind = ? AND created_at > ? AND created_at <= ?",
		orgID,
		inboundContinuationJobKind,
		evidenceSince,
		now,
	).Order("created_at DESC").Limit(500).Find(&jobs).Error; err != nil {
		return nil, err
	}
	for index := range jobs {
		job := &jobs[index]
		jobPhoneID, _ := job.Payload["phone_number_id"].(string)
		if strings.TrimSpace(jobPhoneID) != strings.TrimSpace(phoneID) || job.AggregateID == nil {
			continue
		}
		var message models.Message
		if err := db.Where(
			"id = ? AND organization_id = ? AND whats_app_account = ? AND direction = ? AND whats_app_message_id <> '' AND created_at > ? AND created_at <= ?",
			*job.AggregateID,
			orgID,
			accountName,
			models.DirectionIncoming,
			evidenceSince,
			now,
		).First(&message).Error; err == nil {
			return &message, nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	return nil, errRegistrationReconciliationEvidenceMissing
}

// ReconcilePhoneRegistration activates an ambiguous Embedded Signup claim only
// from read-only provider validation plus durable inbound webhook evidence. It
// never calls /register, changes the token/PIN, or edits operational flags.
func (a *App) ReconcilePhoneRegistration(r *fastglue.Request) error {
	orgID, err := a.requireExplicitOrganization(r)
	if err != nil {
		return nil
	}
	id, err := parsePathUUID(r, "id", "account")
	if err != nil {
		return nil
	}
	if body := strings.TrimSpace(string(r.RequestCtx.PostBody())); body != "" && body != "null" {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &fields); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
		}
		if len(fields) != 0 {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Registration reconciliation does not accept request fields", nil, "")
		}
	}

	var snapshot registrationReconciliationSnapshot
	var userID uuid.UUID
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		resolvedOrgID, resolvedUserID, authErr := scoped.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
		if authErr != nil {
			return authErr
		}
		if resolvedOrgID != orgID {
			_ = r.SendErrorEnvelope(fasthttp.StatusForbidden, "Selected organization is not available", nil, "")
			return errEnvelopeSent
		}
		userID = resolvedUserID

		var account models.WhatsAppAccount
		if loadErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", id, orgID).
			First(&account).Error; loadErr != nil {
			return loadErr
		}
		if strings.TrimSpace(account.Status) != "pending_registration" || account.IsSMB {
			return errRegistrationRecoveryStatus
		}
		if strings.TrimSpace(account.PhoneID) == "" || strings.TrimSpace(account.BusinessID) == "" ||
			strings.TrimSpace(account.APIVersion) == "" || account.UpdatedAt.IsZero() {
			return errRegistrationRecoveryCredentials
		}
		encryptionKey := strings.TrimSpace(scoped.integrationEncryptionKey())
		if encryptionKey == "" || !crypto.IsEncrypted(account.AccessToken) ||
			(strings.TrimSpace(account.Pin) != "" && !crypto.IsEncrypted(account.Pin)) {
			return errRegistrationRecoveryCredentials
		}
		accessToken, decryptErr := crypto.Decrypt(account.AccessToken, encryptionKey)
		if decryptErr != nil || strings.TrimSpace(accessToken) == "" || crypto.IsEncrypted(accessToken) {
			return errRegistrationRecoveryCredentials
		}
		pin, decryptErr := crypto.Decrypt(account.Pin, encryptionKey)
		if decryptErr != nil || !isSixDigitPIN(pin) {
			return errRegistrationRecoveryCredentials
		}
		appID, appSecret, configID, resolveErr := scoped.resolveMetaAppCreds(orgID)
		if resolveErr != nil {
			return resolveErr
		}
		meta := embeddedSignupMetaSnapshot{
			appID:      strings.TrimSpace(appID),
			appSecret:  strings.TrimSpace(appSecret),
			configID:   strings.TrimSpace(configID),
			apiVersion: scoped.metaAPIVersion(),
		}
		if strings.TrimSpace(account.APIVersion) != meta.apiVersion {
			return errRegistrationReconciliationTokenMismatch
		}
		snapshot = registrationReconciliationSnapshot{
			accountID:             account.ID,
			name:                  account.Name,
			phoneID:               strings.TrimSpace(account.PhoneID),
			rawPhoneID:            account.PhoneID,
			businessID:            strings.TrimSpace(account.BusinessID),
			rawBusinessID:         account.BusinessID,
			apiVersion:            strings.TrimSpace(account.APIVersion),
			rawAPIVersion:         account.APIVersion,
			accessToken:           strings.TrimSpace(accessToken),
			accessTokenCiphertext: account.AccessToken,
			accessTokenExpiresAt:  account.AccessTokenExpiresAt,
			pinCiphertext:         account.Pin,
			pendingSince:          account.UpdatedAt.UTC(),
			updatedAt:             account.UpdatedAt,
			meta:                  meta,
			oldAccount:            account,
		}
		return nil
	})
	if errors.Is(err, errEnvelopeSent) {
		return nil
	}
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Account not found", nil, "")
		case errors.Is(err, errRegistrationRecoveryStatus):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp account is not eligible for pending-registration reconciliation", nil, "")
		case errors.Is(err, errRegistrationRecoveryCredentials), errors.Is(err, errRegistrationReconciliationTokenMismatch):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp registration reconciliation contract is incomplete or changed", nil, "")
		case errors.Is(err, errMetaIntegrationDisabled):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Meta integration is disabled", nil, "")
		default:
			a.Log.Error("Failed to prepare WhatsApp registration reconciliation", "account_id", id, "organization_id", orgID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare WhatsApp registration reconciliation", nil, "")
		}
	}

	ctx, cancel := context.WithTimeout(requestContext(r), 30*time.Second)
	defer cancel()
	debugInfo, validatedExpiry, err := a.debugAndValidateMetaAccessTokenWithCredentials(
		ctx,
		snapshot.accessToken,
		snapshot.meta.appID,
		snapshot.meta.appSecret,
	)
	if err != nil {
		return a.sendMetaTokenPreflightError(r, err)
	}
	if !sameMetaTokenExpiry(snapshot.accessTokenExpiresAt, validatedExpiry) {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Stored Meta token metadata changed; reconnect before reconciling", nil, "")
	}
	if err := embeddedSignupTokenGrantsWABA(debugInfo, snapshot.businessID); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Stored Meta token is not granted to this WhatsApp account", nil, "")
	}
	if _, err := a.validateWhatsAppAccountContract(
		ctx,
		snapshot.phoneID,
		snapshot.businessID,
		snapshot.accessToken,
		snapshot.apiVersion,
	); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Stored Meta token no longer validates this exact WhatsApp phone and business account", nil, "")
	}
	appSubscribed, err := a.WhatsApp.IsAppSubscribed(
		ctx,
		snapshot.businessID,
		snapshot.meta.appID,
		snapshot.accessToken,
		snapshot.meta.apiVersion,
	)
	if err != nil {
		a.Log.Warn("Failed to verify WABA app subscription for registration reconciliation", "account_id", snapshot.accountID)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Meta could not confirm the current app subscription; the account remains pending registration", nil, "")
	}
	if !appSubscribed {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "The current Meta app is not subscribed to this WhatsApp Business Account; the account remains pending registration", nil, "")
	}

	var updated models.WhatsAppAccount
	var evidence *models.Message
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		resolvedOrgID, resolvedUserID, authErr := scoped.requireAuth(r, models.ResourceAccounts, models.ActionWrite)
		if authErr != nil {
			return authErr
		}
		if resolvedOrgID != orgID || resolvedUserID != userID {
			return errRegistrationRecoverySuperseded
		}
		if lockErr := lockMetaIntegrationContract(scoped.DB, orgID); lockErr != nil {
			return lockErr
		}
		appID, appSecret, configID, resolveErr := scoped.resolveMetaAppCreds(orgID)
		if resolveErr != nil {
			return resolveErr
		}
		if strings.TrimSpace(appID) != snapshot.meta.appID ||
			strings.TrimSpace(appSecret) != snapshot.meta.appSecret ||
			strings.TrimSpace(configID) != snapshot.meta.configID ||
			scoped.metaAPIVersion() != snapshot.meta.apiVersion {
			return errRegistrationReconciliationTokenMismatch
		}

		var evidenceErr error
		evidence, evidenceErr = recentInboundRegistrationEvidence(
			scoped.DB,
			orgID,
			snapshot.name,
			snapshot.phoneID,
			snapshot.pendingSince,
			time.Now().UTC(),
		)
		if evidenceErr != nil {
			return evidenceErr
		}
		query := scoped.DB.Model(&models.WhatsAppAccount{}).Where(
			"id = ? AND organization_id = ? AND deleted_at IS NULL AND status = ? AND updated_at = ? AND name = ? AND phone_id = ? AND business_id = ? AND api_version = ? AND access_token = ? AND pin = ?",
			snapshot.accountID,
			orgID,
			"pending_registration",
			snapshot.updatedAt,
			snapshot.name,
			snapshot.rawPhoneID,
			snapshot.rawBusinessID,
			snapshot.rawAPIVersion,
			snapshot.accessTokenCiphertext,
			snapshot.pinCiphertext,
		)
		if snapshot.accessTokenExpiresAt == nil {
			query = query.Where("access_token_expires_at IS NULL")
		} else {
			query = query.Where("access_token_expires_at = ?", *snapshot.accessTokenExpiresAt)
		}
		result := query.Updates(map[string]any{
			"status":        "active",
			"updated_by_id": userID,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errRegistrationRecoverySuperseded
		}
		if loadErr := scoped.DB.Preload("CreatedBy").Preload("UpdatedBy").
			Where("id = ? AND organization_id = ?", snapshot.accountID, orgID).
			First(&updated).Error; loadErr != nil {
			return loadErr
		}
		return audit.LogAudit(
			scoped.DB,
			orgID,
			userID,
			audit.GetUserName(scoped.DB, userID),
			"account",
			updated.ID,
			models.AuditActionUpdated,
			&snapshot.oldAccount,
			&updated,
			map[string]any{
				"field":     "registration_reconciliation_evidence",
				"old_value": nil,
				"new_value": evidence.WhatsAppMessageID,
			},
		)
	})
	a.InvalidateWhatsAppAccountCache(snapshot.phoneID)
	if err != nil {
		switch {
		case errors.Is(err, errRegistrationReconciliationEvidenceMissing):
			return r.SendErrorEnvelope(
				fasthttp.StatusConflict,
				"No recent inbound WhatsApp evidence was found after the latest pending-registration change or registration attempt. Send a new message to this number, then retry reconciliation.",
				nil,
				"",
			)
		case errors.Is(err, errRegistrationRecoverySuperseded),
			errors.Is(err, errRegistrationReconciliationTokenMismatch),
			errors.Is(err, errMetaIntegrationDisabled):
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "WhatsApp registration reconciliation changed; reload and retry", nil, "")
		default:
			a.Log.Error("Failed to reconcile WhatsApp registration", "account_id", snapshot.accountID, "organization_id", orgID)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to reconcile WhatsApp registration", nil, "")
		}
	}

	return r.SendEnvelope(map[string]any{
		"success":     true,
		"message":     "Registration reconciled from recent inbound WhatsApp evidence.",
		"status":      "active",
		"evidence_at": evidence.CreatedAt.UTC().Format(time.RFC3339),
		"account":     accountToResponse(updated),
	})
}

func isSixDigitPIN(pin string) bool {
	if len(pin) != 6 {
		return false
	}
	for _, digit := range pin {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func embeddedSignupRecoveryLockedStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "pending_registration", "pending_subscription", "subscription_failed":
		return true
	default:
		return false
	}
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
