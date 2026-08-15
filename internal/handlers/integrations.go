package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	qwenapi "github.com/shridarpatil/whatomate/internal/qwen"
	"github.com/shridarpatil/whatomate/internal/threadsreview"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	integrationProviderMeta                = "meta"
	integrationProviderThreads             = "threads"
	integrationProviderTikTok              = "tiktok"
	integrationProviderQwen                = "qwen"
	integrationProviderGoogleSearchConsole = "google_search_console"
	integrationProviderEmail               = "email"
	integrationProviderWebchat             = "webchat"

	integrationStatusNotConfigured   = "not_configured"
	integrationStatusConfigured      = "configured"
	integrationStatusConnected       = "connected"
	integrationStatusDegraded        = "degraded"
	integrationStatusDisabled        = "disabled"
	integrationStatusApprovalNeeded  = "approval_required"
	integrationStatusAdapterMissing  = "adapter_unavailable"
	integrationValidationFailedCode  = "provider_validation_failed"
	integrationAnalyticsFailedCode   = "provider_analytics_failed"
	integrationValidationSuccessCode = ""

	metaAppSecretSetting                 = "meta_app_secret_encrypted"
	metaWebhookVerifyTokenSetting        = "meta_webhook_verify_token_encrypted"
	metaWebhookCallbackWorkspaceQueryKey = "workspace"
)

var integrationIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)

type integrationCredentialResponse struct {
	Configured bool       `json:"configured"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	Source     string     `json:"source,omitempty"`
}

type integrationConnectionResponse struct {
	AccountCount      int        `json:"account_count"`
	ActiveCount       int        `json:"active_count"`
	PendingCount      int        `json:"pending_count"`
	LastHealthCheckAt *time.Time `json:"last_health_check_at,omitempty"`
	LastInboundAt     *time.Time `json:"last_inbound_at,omitempty"`
	LastOutboundAt    *time.Time `json:"last_outbound_at,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
}

type integrationOAuthResponse struct {
	Supported bool   `json:"supported"`
	Available bool   `json:"available"`
	Mode      string `json:"mode,omitempty"`
}

// IntegrationResponse is deliberately composed from safe fields rather than
// serializing persistence models. Credential values never enter this type.
type IntegrationResponse struct {
	Provider           string                                   `json:"provider"`
	DisplayName        string                                   `json:"display_name"`
	Status             string                                   `json:"status"`
	Enabled            bool                                     `json:"enabled"`
	Configured         bool                                     `json:"configured"`
	ReadOnly           bool                                     `json:"read_only"`
	Config             models.JSONB                             `json:"config"`
	Credentials        map[string]integrationCredentialResponse `json:"credentials"`
	Connection         integrationConnectionResponse            `json:"connection"`
	ChannelConnections map[string]integrationConnectionResponse `json:"channel_connections,omitempty"`
	IntendedChannels   *[]string                                `json:"intended_channels,omitempty"`
	OAuth              integrationOAuthResponse                 `json:"oauth"`
	TestSupported      bool                                     `json:"test_supported"`
	Message            string                                   `json:"message,omitempty"`
	RequiredScopes     []string                                 `json:"required_scopes,omitempty"`
	LastTestedAt       *time.Time                               `json:"last_tested_at,omitempty"`
	credentialUsable   bool
}

type updateIntegrationRequest struct {
	Enabled          *bool             `json:"enabled"`
	Config           models.JSONB      `json:"config"`
	Credentials      map[string]string `json:"credentials"`
	ClearCredentials []string          `json:"clear_credentials"`
}

type integrationClientError struct {
	status  int
	message string
}

func (e *integrationClientError) Error() string { return e.message }

type integrationSources struct {
	organization           models.Organization
	rows                   map[string]*models.ProviderIntegration
	copilot                *models.CopilotSettings
	connections            map[string]integrationConnectionResponse
	channelConnections     map[string]integrationConnectionResponse
	threadsWebhookAccount  *uuid.UUID
	intendedChannels       *[]string
	metaLegacyWebhookToken bool
}

// GetIntegrations returns the complete admin Integration Center catalogue.
func (a *App) GetIntegrations(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(
		r,
		models.ResourceSettingsIntegrations,
		models.ActionRead,
	)
	if err != nil {
		return nil
	}
	integrations, err := a.integrationResponses(orgID)
	if err != nil {
		a.Log.Error("Failed to load integration settings", "error", err, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load integrations", nil, "")
	}
	return r.SendEnvelope(map[string]any{"integrations": integrations})
}

// UpdateIntegration applies partial, provider-specific settings. Empty or
// omitted credential values preserve the existing secret. Clearing requires a
// separately allowlisted clear_credentials entry.
func (a *App) UpdateIntegration(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(
		r,
		models.ResourceSettingsIntegrations,
		models.ActionWrite,
	)
	if err != nil {
		return nil
	}
	provider, err := integrationProviderFromRequest(r, false)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	var request updateIntegrationRequest
	decoder := json.NewDecoder(bytes.NewReader(r.RequestCtx.PostBody()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}
	if err := a.updateIntegration(orgID, userID, provider, request); err != nil {
		return a.sendIntegrationMutationError(r, err)
	}
	if provider == integrationProviderQwen {
		a.InvalidateChatbotSettingsCache(orgID)
	}
	integration, err := a.integrationResponse(orgID, provider)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Integration was saved but could not be reloaded", nil, "")
	}
	return r.SendEnvelope(integration)
}

// DeleteIntegrationCredentials is an explicit clear-all operation for one
// provider. It preserves account history and revokes provider credentials when
// the provider owns durable channel accounts.
func (a *App) DeleteIntegrationCredentials(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(
		r,
		models.ResourceSettingsIntegrations,
		models.ActionWrite,
	)
	if err != nil {
		return nil
	}
	provider, err := integrationProviderFromRequest(r, false)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if provider == integrationProviderGoogleSearchConsole {
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"Use the Google Search Console disconnect action to remove this workspace's stored authorization safely",
			nil,
			"",
		)
	}
	disabled := false
	request := updateIntegrationRequest{
		Enabled:          &disabled,
		ClearCredentials: integrationCredentialNames(provider),
	}
	if err := a.updateIntegration(orgID, userID, provider, request); err != nil {
		return a.sendIntegrationMutationError(r, err)
	}
	if provider == integrationProviderQwen {
		a.InvalidateChatbotSettingsCache(orgID)
	}
	integration, err := a.integrationResponse(orgID, provider)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Credentials were cleared but the integration could not be reloaded", nil, "")
	}
	return r.SendEnvelope(integration)
}

// ConnectIntegration returns the next provider connection action.
func (a *App) ConnectIntegration(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(
		r,
		models.ResourceSettingsIntegrations,
		models.ActionWrite,
	)
	if err != nil {
		return nil
	}
	provider, err := integrationProviderFromRequest(r, false)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	integration, err := a.integrationResponse(orgID, provider)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare the integration", nil, "")
	}
	switch provider {
	case integrationProviderGoogleSearchConsole:
		return a.startGoogleSearchConsoleOAuth(r, orgID, userID)
	case integrationProviderMeta:
		if !integration.Enabled {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Enable the Meta integration first", nil, "")
		}
		if !integration.Configured {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Configure the Meta App ID, Config ID, App Secret, and Webhook Verify Token first", nil, "")
		}
		if !integration.credentialUsable {
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "The stored integration credential is unavailable to this server", nil, "")
		}
		return r.SendEnvelope(map[string]any{
			"provider": integrationProviderMeta,
			"ready":    true,
			"mode":     "embedded_signup",
			"message":  "Launch Meta Embedded Signup with this public configuration",
			"public_config": map[string]any{
				"whatsapp_app_id":      integration.Config["app_id"],
				"whatsapp_config_id":   integration.Config["config_id"],
				"whatsapp_api_version": integration.Config["api_version"],
			},
		})
	case integrationProviderQwen:
		if !integration.Enabled {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Enable Qwen Copilot first", nil, "")
		}
		if !integration.Configured {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Configure a Qwen API key first", nil, "")
		}
		if !integration.credentialUsable {
			return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "The stored integration credential is unavailable to this server", nil, "")
		}
		return r.SendEnvelope(map[string]any{
			"provider": integrationProviderQwen,
			"ready":    true,
			"mode":     "api_key",
			"message":  "Qwen uses the configured server-side API key and does not require OAuth",
		})
	case integrationProviderThreads:
		return a.startThreadsOAuth(r, orgID, userID)
	case integrationProviderTikTok:
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"TikTok Business Messaging approval and an approved adapter are required before OAuth can start",
			nil,
			"",
		)
	default:
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "This integration does not have a connect action", nil, "")
	}
}

type integrationTestSnapshot struct {
	provider        string
	validationToken string
	appID           string
	appSecret       string
	qwenAPIKey      string
	qwenModel       string
	qwenBaseURL     string
	qwenMaxTokens   int
	qwenTemperature float64
}

// TestIntegration runs provider I/O outside a tenant database transaction.
// Configuration is loaded and the result is committed in short, independent
// tenant phases guarded by a validation token, preventing stale tests from
// overwriting newer settings.
func (a *App) TestIntegration(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(
		r,
		models.ResourceSettingsIntegrations,
		models.ActionWrite,
	)
	if err != nil {
		return nil
	}
	provider, err := integrationProviderFromRequest(r, false)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if provider == integrationProviderTikTok {
		return r.SendEnvelope(map[string]any{
			"provider": provider,
			"success":  false,
			"status":   integrationStatusApprovalNeeded,
			"message":  "TikTok Business Messaging approval and an approved adapter are required",
		})
	}
	if provider == integrationProviderGoogleSearchConsole {
		return a.testGoogleSearchConsole(r, orgID, userID)
	}
	if provider != integrationProviderMeta && provider != integrationProviderQwen {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Testing is not supported for this integration", nil, "")
	}

	snapshot := integrationTestSnapshot{provider: provider, validationToken: uuid.NewString()}
	err = a.WithTenantApp(orgID, func(scoped *App) error {
		return scoped.prepareIntegrationTest(orgID, userID, &snapshot)
	})
	if err != nil {
		var clientErr *integrationClientError
		if errors.As(err, &clientErr) {
			return r.SendErrorEnvelope(clientErr.status, clientErr.message, nil, "")
		}
		a.Log.Error("Failed to prepare integration test", "error", err, "provider", provider, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to prepare integration test", nil, "")
	}

	var validationErr error
	switch provider {
	case integrationProviderMeta:
		validationErr = a.testMetaApplication(r, snapshot.appID, snapshot.appSecret)
	case integrationProviderQwen:
		validationErr = a.testQwenProvider(r, snapshot)
	}
	now := time.Now().UTC()
	if err := a.WithTenantApp(orgID, func(scoped *App) error {
		return scoped.finishIntegrationTest(orgID, userID, snapshot, now, validationErr)
	}); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "Integration settings changed while the test was running; test again", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to save integration test result", nil, "")
	}
	if validationErr != nil {
		return r.SendEnvelope(map[string]any{
			"provider":  provider,
			"success":   false,
			"status":    integrationStatusDegraded,
			"message":   "The provider rejected the configured credentials or could not be reached",
			"tested_at": now,
		})
	}
	return r.SendEnvelope(map[string]any{
		"provider":  provider,
		"success":   true,
		"status":    integrationStatusConnected,
		"message":   "Connection test succeeded",
		"tested_at": now,
	})
}

func (a *App) integrationResponses(orgID uuid.UUID) ([]IntegrationResponse, error) {
	sources, err := a.loadIntegrationSources(orgID)
	if err != nil {
		return nil, err
	}
	providers := []string{
		integrationProviderMeta,
		integrationProviderThreads,
		integrationProviderTikTok,
		integrationProviderQwen,
		integrationProviderGoogleSearchConsole,
		integrationProviderEmail,
		integrationProviderWebchat,
	}
	result := make([]IntegrationResponse, 0, len(providers))
	for _, provider := range providers {
		result = append(result, a.composeIntegrationResponse(provider, sources))
	}
	return result, nil
}

func (a *App) integrationResponse(orgID uuid.UUID, provider string) (IntegrationResponse, error) {
	sources, err := a.loadIntegrationSources(orgID)
	if err != nil {
		return IntegrationResponse{}, err
	}
	return a.composeIntegrationResponse(provider, sources), nil
}

func (a *App) loadIntegrationSources(orgID uuid.UUID) (*integrationSources, error) {
	var organization models.Organization
	if err := a.DB.Where("id = ?", orgID).First(&organization).Error; err != nil {
		return nil, err
	}
	var stored []models.ProviderIntegration
	if err := a.DB.Where("organization_id = ?", orgID).Find(&stored).Error; err != nil {
		return nil, err
	}
	rows := make(map[string]*models.ProviderIntegration, len(stored))
	for index := range stored {
		row := stored[index]
		rows[row.Provider] = &row
	}

	var copilot *models.CopilotSettings
	var dedicated models.CopilotSettings
	if err := a.DB.Where("organization_id = ? AND whats_app_account = ''", orgID).
		Order("updated_at DESC, id ASC").First(&dedicated).Error; err == nil {
		copilot = &dedicated
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	connections, channelConnections, err := a.integrationConnectionSummaries(orgID)
	if err != nil {
		return nil, err
	}
	intended, declared, err := productCommercialWorkspaceIntendedChannels(a.DB, orgID)
	if err != nil {
		return nil, err
	}
	var intendedChannels *[]string
	if declared {
		values := make([]string, len(intended))
		for index := range intended {
			values[index] = string(intended[index])
		}
		intendedChannels = &values
	}
	var legacyWebhookTokenCount int64
	if err := a.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND webhook_verify_token <> ''", orgID).
		Count(&legacyWebhookTokenCount).Error; err != nil {
		return nil, err
	}
	var threadsWebhookAccount *uuid.UUID
	threadsRow := rows[integrationProviderThreads]
	if threadsRow != nil && threadsRow.ThreadsAppID != nil {
		expectedAppID := strings.TrimSpace(*threadsRow.ThreadsAppID)
		var threadsAccounts []models.ChannelAccount
		if err := a.DB.Select("id", "metadata").Where(
			"organization_id = ? AND channel = ? AND provider = ? AND status = ?",
			orgID,
			models.ChannelThreads,
			channelapi.ThreadsProvider,
			models.ChannelAccountStatusActive,
		).Order("updated_at DESC, id ASC").Find(&threadsAccounts).Error; err != nil {
			return nil, err
		}
		for index := range threadsAccounts {
			if expectedAppID != "" &&
				stringJSONValue(threadsAccounts[index].Metadata, "app_id") == expectedAppID {
				accountID := threadsAccounts[index].ID
				threadsWebhookAccount = &accountID
				break
			}
		}
	}
	return &integrationSources{
		organization:           organization,
		rows:                   rows,
		copilot:                copilot,
		connections:            connections,
		channelConnections:     channelConnections,
		threadsWebhookAccount:  threadsWebhookAccount,
		intendedChannels:       intendedChannels,
		metaLegacyWebhookToken: legacyWebhookTokenCount > 0,
	}, nil
}

func (a *App) composeIntegrationResponse(provider string, sources *integrationSources) IntegrationResponse {
	row := sources.rows[provider]
	response := IntegrationResponse{
		Provider:         provider,
		DisplayName:      integrationDisplayName(provider),
		Status:           integrationStatusNotConfigured,
		Config:           models.JSONB{},
		Credentials:      map[string]integrationCredentialResponse{},
		Connection:       sources.connections[provider],
		IntendedChannels: sources.intendedChannels,
		OAuth:            integrationOAuthResponse{},
	}
	if row != nil {
		response.Enabled = row.Enabled
		response.LastTestedAt = row.LastTestedAt
	}

	switch provider {
	case integrationProviderMeta:
		response.ChannelConnections = map[string]integrationConnectionResponse{
			string(models.ChannelWhatsApp):  sources.channelConnections[string(models.ChannelWhatsApp)],
			string(models.ChannelInstagram): sources.channelConnections[string(models.ChannelInstagram)],
			string(models.ChannelMessenger): sources.channelConnections[string(models.ChannelMessenger)],
		}
		workspaceManaged := metaWorkspaceAppManaged(&sources.organization)
		appID, configID, secretConfigured, secretSource := a.metaIntegrationValues(&sources.organization)
		webhookCredential, webhookCredentialUsable := a.metaWebhookIntegrationCredential(
			&sources.organization,
			sources.metaLegacyWebhookToken,
			row,
			workspaceManaged,
		)
		response.credentialUsable = a.metaIntegrationCredentialUsable(&sources.organization) && webhookCredentialUsable
		response.Config = models.JSONB{
			"app_id":                appID,
			"config_id":             configID,
			"api_version":           a.metaAPIVersion(),
			"management_mode":       map[bool]string{true: "workspace", false: "platform"}[workspaceManaged],
			"webhook_callback_path": metaWebhookCallbackPath(sources.organization.ID, workspaceManaged),
		}
		response.Credentials["app_secret"] = integrationCredentialResponse{
			Configured: secretConfigured,
			UpdatedAt:  integrationCredentialUpdatedAt(row),
			Source:     secretSource,
		}
		response.Credentials["webhook_verify_token"] = webhookCredential
		response.Configured = appID != "" && configID != "" && secretConfigured && webhookCredential.Configured
		if row == nil {
			response.Enabled = response.Configured
		}
		response.OAuth = integrationOAuthResponse{Supported: true, Available: response.Configured && response.credentialUsable, Mode: "embedded_signup"}
		response.TestSupported = true
		response.RequiredScopes = []string{
			"whatsapp_business_management",
			"whatsapp_business_messaging",
		}
		response.Status = integrationOperationalStatus(response, row)
		if response.Configured && !response.credentialUsable {
			response.Status = integrationStatusDegraded
			response.Message = "The stored integration credential is unavailable to this server."
		} else if appID != "" && configID != "" && secretConfigured && !webhookCredential.Configured {
			response.Message = "Add the workspace webhook verify token before connecting WhatsApp accounts."
		}
	case integrationProviderQwen:
		model, maxTokens, temperature, configured, source, enabled := qwenIntegrationValues(sources.copilot)
		response.credentialUsable = a.qwenIntegrationCredentialUsable(sources.copilot)
		response.Config = models.JSONB{
			"model":           model,
			"max_tokens":      maxTokens,
			"temperature":     temperature,
			"endpoint_region": qwenEndpointRegion(row),
			"base_url":        a.qwenPublicBaseURLForRow(row),
		}
		response.Credentials["api_key"] = integrationCredentialResponse{
			Configured: configured,
			UpdatedAt:  integrationCredentialUpdatedAt(row),
			Source:     source,
		}
		response.Configured = configured && model != ""
		response.Enabled = enabled
		response.TestSupported = true
		response.Status = integrationOperationalStatus(response, row)
		if response.Configured && !response.credentialUsable {
			response.Status = integrationStatusDegraded
			response.Message = "The stored integration credential is unavailable to this server."
		}
	case integrationProviderThreads:
		threadsAccessMode := threadsreview.ModeBlocked
		if row != nil {
			threadsAccessMode = a.threadsAppReviewAccessMode(sources.organization.ID, row.Config, "")
		}
		response.Enabled = row != nil && row.Enabled && threadsAccessMode != threadsreview.ModeBlocked
		response.Config = safeIntegrationConfig(provider, integrationRowConfig(row))
		response.Config["app_review_access_mode"] = threadsAccessMode
		for key, value := range a.threadsPublicSetupURLs(
			sources.organization.ID,
			stringJSONValue(response.Config, "redirect_uri"),
			sources.threadsWebhookAccount,
		) {
			response.Config[key] = value
		}
		response.Credentials["app_secret"] = encryptedCredentialFlag(row, "app_secret")
		response.Credentials["webhook_verify_token"] = encryptedCredentialFlag(row, "webhook_verify_token")
		response.Configured = stringJSONValue(response.Config, "app_id") != "" &&
			stringJSONValue(response.Config, "redirect_uri") != "" &&
			response.Credentials["app_secret"].Configured &&
			response.Credentials["webhook_verify_token"].Configured
		response.credentialUsable = row != nil &&
			a.storedIntegrationCredentialUsable(stringJSONValue(row.CredentialData, "app_secret")) &&
			a.storedIntegrationCredentialUsable(stringJSONValue(row.CredentialData, "webhook_verify_token"))
		response.OAuth = integrationOAuthResponse{
			Supported: true,
			Available: response.Configured && response.credentialUsable &&
				threadsAccessMode != threadsreview.ModeBlocked && a.threadsOAuthAvailable(),
			Mode: "oauth",
		}
		response.TestSupported = false
		response.Status = integrationOperationalStatus(response, row)
		response.Message = "Public replies and mentions only. Direct messages and standalone posts are not supported."
		if threadsAccessMode == threadsreview.ModeBlocked {
			response.Status = integrationStatusApprovalNeeded
			response.Message = "Meta App Review approval is required before Threads can be enabled or authorized."
		} else if threadsAccessMode == threadsreview.ModeDevelopmentTesting {
			response.Message = "Non-production Threads App Review testing is restricted to the deployment-allowlisted app-role profile."
		} else if response.Configured && !response.credentialUsable {
			response.Status = integrationStatusDegraded
			response.Message = "The stored Threads app or webhook credential is unavailable to this server."
		}
		response.RequiredScopes = append([]string(nil), threadsRequiredScopes...)
	case integrationProviderTikTok:
		response.Enabled = false
		response.Config = safeIntegrationConfig(provider, integrationRowConfig(row))
		response.Credentials["client_secret"] = encryptedCredentialFlag(row, "client_secret")
		response.Configured = stringJSONValue(response.Config, "client_id") != "" &&
			stringJSONValue(response.Config, "redirect_uri") != "" &&
			response.Credentials["client_secret"].Configured
		response.OAuth = integrationOAuthResponse{Supported: true, Available: false, Mode: "oauth"}
		response.TestSupported = false
		if stringJSONValue(response.Config, "approval_status") == "approved" {
			response.Status = integrationStatusAdapterMissing
			response.Message = "TikTok credentials are prepared, but the Business Messaging adapter is not installed."
		} else {
			response.Status = integrationStatusApprovalNeeded
			response.Message = "TikTok Business Messaging approval is required before activation."
		}
		response.RequiredScopes = []string{
			"message.list.read",
			"message.list.send",
			"message.list.manage",
		}
	case integrationProviderGoogleSearchConsole:
		credential := encryptedCredentialFlag(row, googleSearchConsoleRefreshTokenCredential)
		platformConfigured := a.googleSearchConsolePlatformConfigured()
		response.Config = models.JSONB{
			"platform_configured":     platformConfigured,
			"property_count":          response.Connection.AccountCount,
			"selected_property_count": response.Connection.ActiveCount,
		}
		response.Credentials[googleSearchConsoleRefreshTokenCredential] = credential
		response.Configured = credential.Configured
		if credential.Configured && a.hasIntegrationEncryptionKey() && platformConfigured {
			_, decryptErr := a.decryptGoogleSearchConsoleRefreshToken(row)
			response.credentialUsable = decryptErr == nil
		}
		response.Config["operations_available"] = response.credentialUsable
		if row != nil && row.LastErrorMessage != "" {
			response.Connection.LastError = row.LastErrorMessage
		}
		response.OAuth = integrationOAuthResponse{
			Supported: true,
			Available: a.googleSearchConsoleOAuthAvailable(),
			Mode:      "oauth",
		}
		response.TestSupported = true
		response.RequiredScopes = []string{googleSearchConsoleReadOnlyScope}
		response.Status = integrationOperationalStatus(response, row)
		blockingProviderError := row != nil && row.LastErrorCode != "" && row.LastErrorCode != integrationAnalyticsFailedCode
		if blockingProviderError {
			response.Status = integrationStatusDegraded
		} else if response.credentialUsable {
			response.Status = integrationStatusConnected
		}
		if response.Configured && response.Connection.ActiveCount == 0 {
			response.Message = "Select at least one verified website property to enable Search Visibility analytics."
		}
		if !platformConfigured {
			if response.Configured {
				response.Status = integrationStatusDegraded
			}
			response.Message = "Google Search Console OAuth is not configured on this server."
		} else if !a.hasIntegrationEncryptionKey() {
			if response.Configured {
				response.Status = integrationStatusDegraded
			}
			response.Message = "Server-side credential encryption is unavailable."
		} else if response.Configured && !response.credentialUsable {
			response.Status = integrationStatusDegraded
			response.Message = "The stored Google authorization is unavailable to this server. Reauthorize Google Search Console."
		}
	case integrationProviderEmail, integrationProviderWebchat:
		response.ReadOnly = true
		response.Enabled = response.Connection.AccountCount > 0
		response.Configured = response.Connection.AccountCount > 0
		response.Status = connectionOnlyStatus(response.Connection)
		response.Message = "Connections for this channel are managed in the Omnichannel Inbox."
	}
	return response
}

func integrationOperationalStatus(response IntegrationResponse, row *models.ProviderIntegration) string {
	if !response.Enabled {
		if row != nil || response.Configured {
			return integrationStatusDisabled
		}
		return integrationStatusNotConfigured
	}
	if !response.Configured {
		return integrationStatusNotConfigured
	}
	if row != nil && row.LastErrorCode != "" {
		return integrationStatusDegraded
	}
	if response.Connection.ActiveCount > 0 || (row != nil && row.LastSuccessfulAt != nil) {
		return integrationStatusConnected
	}
	return integrationStatusConfigured
}

func connectionOnlyStatus(connection integrationConnectionResponse) string {
	if connection.AccountCount == 0 {
		return integrationStatusNotConfigured
	}
	if connection.LastError != "" || connection.ActiveCount == 0 {
		return integrationStatusDegraded
	}
	return integrationStatusConnected
}

func (a *App) integrationConnectionSummaries(
	orgID uuid.UUID,
) (
	map[string]integrationConnectionResponse,
	map[string]integrationConnectionResponse,
	error,
) {
	result := map[string]integrationConnectionResponse{
		integrationProviderMeta:                {},
		integrationProviderThreads:             {},
		integrationProviderTikTok:              {},
		integrationProviderQwen:                {},
		integrationProviderGoogleSearchConsole: {},
		integrationProviderEmail:               {},
		integrationProviderWebchat:             {},
	}
	byChannel := map[string]integrationConnectionResponse{
		string(models.ChannelWhatsApp):  {},
		string(models.ChannelInstagram): {},
		string(models.ChannelMessenger): {},
		string(models.ChannelThreads):   {},
		string(models.ChannelEmail):     {},
		string(models.ChannelWebChat):   {},
	}
	var whatsAppAccounts []models.WhatsAppAccount
	if err := a.DB.Where("organization_id = ?", orgID).Find(&whatsAppAccounts).Error; err != nil {
		return nil, nil, err
	}
	meta := result[integrationProviderMeta]
	whatsApp := byChannel[string(models.ChannelWhatsApp)]
	for index := range whatsAppAccounts {
		meta.AccountCount++
		whatsApp.AccountCount++
		if strings.EqualFold(whatsAppAccounts[index].Status, "active") {
			meta.ActiveCount++
			whatsApp.ActiveCount++
		} else {
			meta.PendingCount++
			whatsApp.PendingCount++
		}
	}
	result[integrationProviderMeta] = meta
	byChannel[string(models.ChannelWhatsApp)] = whatsApp

	var googlePropertyCount int64
	if err := a.DB.Model(&models.GoogleSearchConsoleProperty{}).
		Where("organization_id = ? AND available = ?", orgID, true).
		Count(&googlePropertyCount).Error; err != nil {
		return nil, nil, err
	}
	var selectedGooglePropertyCount int64
	if err := a.DB.Model(&models.GoogleSearchConsoleProperty{}).
		Where("organization_id = ? AND available = ? AND selected = ?", orgID, true, true).
		Count(&selectedGooglePropertyCount).Error; err != nil {
		return nil, nil, err
	}
	googleConnection := result[integrationProviderGoogleSearchConsole]
	googleConnection.AccountCount = int(googlePropertyCount)
	googleConnection.ActiveCount = int(selectedGooglePropertyCount)
	googleConnection.PendingCount = int(googlePropertyCount - selectedGooglePropertyCount)
	result[integrationProviderGoogleSearchConsole] = googleConnection

	var accounts []models.ChannelAccount
	if err := a.DB.Where("organization_id = ?", orgID).Find(&accounts).Error; err != nil {
		return nil, nil, err
	}
	for index := range accounts {
		account := &accounts[index]
		// WhatsApp meta_legacy rows mirror the established WhatsApp account and
		// must not be counted as a second channel or provider connection.
		if account.Provider == channelapi.LegacyMetaProvider {
			continue
		}
		channelKey := string(account.Channel)
		channelConnection, exists := byChannel[channelKey]
		if exists {
			channelConnection = summarizeChannelAccountConnection(channelConnection, account)
			byChannel[channelKey] = channelConnection
		}
		provider := string(account.Channel)
		if account.Channel == models.ChannelInstagram || account.Channel == models.ChannelMessenger {
			provider = integrationProviderMeta
		}
		connection, exists := result[provider]
		if !exists {
			continue
		}
		connection = summarizeChannelAccountConnection(connection, account)
		result[provider] = connection
	}
	return result, byChannel, nil
}

func summarizeChannelAccountConnection(
	connection integrationConnectionResponse,
	account *models.ChannelAccount,
) integrationConnectionResponse {
	connection.AccountCount++
	if account.Status == models.ChannelAccountStatusActive {
		connection.ActiveCount++
	} else {
		connection.PendingCount++
	}
	connection.LastHealthCheckAt = newestTime(connection.LastHealthCheckAt, account.LastHealthCheckAt)
	connection.LastInboundAt = newestTime(connection.LastInboundAt, account.LastInboundAt)
	connection.LastOutboundAt = newestTime(connection.LastOutboundAt, account.LastOutboundAt)
	if account.LastError != "" {
		connection.LastError = "Connection needs attention"
	}
	return connection
}

func newestTime(current, candidate *time.Time) *time.Time {
	if candidate == nil {
		return current
	}
	if current == nil || candidate.After(*current) {
		value := candidate.UTC()
		return &value
	}
	return current
}

func (a *App) updateIntegration(orgID, userID uuid.UUID, provider string, request updateIntegrationRequest) error {
	if provider == integrationProviderEmail || provider == integrationProviderWebchat {
		return &integrationClientError{status: fasthttp.StatusBadRequest, message: "This integration is managed from the Omnichannel Inbox"}
	}
	if provider == integrationProviderGoogleSearchConsole {
		return &integrationClientError{status: fasthttp.StatusBadRequest, message: "Use Connect Google or Disconnect for Google Search Console"}
	}
	if request.Enabled != nil && *request.Enabled && provider == integrationProviderTikTok {
		return &integrationClientError{
			status:  fasthttp.StatusConflict,
			message: "This provider cannot be enabled until its approved adapter is installed",
		}
	}
	config, err := validateIntegrationConfig(provider, request.Config)
	if err != nil {
		return &integrationClientError{status: fasthttp.StatusBadRequest, message: err.Error()}
	}
	credentialValues, clearSet, err := validateIntegrationCredentials(provider, request.Credentials, request.ClearCredentials)
	if err != nil {
		return &integrationClientError{status: fasthttp.StatusBadRequest, message: err.Error()}
	}
	if len(credentialValues) > 0 && !a.hasIntegrationEncryptionKey() {
		return &integrationClientError{
			status:  fasthttp.StatusServiceUnavailable,
			message: "Credential storage is unavailable because server-side encryption is not configured",
		}
	}

	userName := audit.GetUserName(a.DB, userID)
	return a.DB.Transaction(func(tx *gorm.DB) error {
		var organization models.Organization
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", orgID).First(&organization).Error; err != nil {
			return err
		}
		row, isNew, err := lockOrCreateIntegrationRow(tx, orgID, userID, provider)
		if err != nil {
			return err
		}
		oldSnapshot := integrationAuditSnapshot(provider, &organization, row, nil)
		credentialsChanged := false
		configChanged := len(config) > 0
		oldThreadsAppID := ""
		oldThreadsApproved := false
		legacyThreadsEnabledWithoutAccess := false
		threadsReviewStatus, threadsReviewStatusProvided := config[threadsreview.StatusKey]
		if provider == integrationProviderThreads {
			oldThreadsAppID = strings.TrimSpace(stringJSONValue(row.Config, "app_id"))
			oldThreadsApproved = threadsAppReviewApproved(row.Config)
			legacyThreadsEnabledWithoutAccess = row.Enabled &&
				a.threadsAppReviewAccessMode(orgID, row.Config, "") == threadsreview.ModeBlocked
			if threadsReviewStatusProvided && strings.TrimSpace(fmt.Sprint(threadsReviewStatus)) == threadsAppReviewApprovedStatus &&
				!oldThreadsApproved {
				return &integrationClientError{
					status:  fasthttp.StatusForbidden,
					message: "Threads App Review approval must be recorded by the platform owner with approval evidence",
				}
			}
		}
		_, threadsAppSecretCleared := clearSet["app_secret"]
		threadsAppSecretReplaced := strings.TrimSpace(credentialValues["app_secret"]) != ""

		switch provider {
		case integrationProviderMeta:
			if organization.Settings == nil {
				organization.Settings = models.JSONB{}
			}
			for key, value := range config {
				switch key {
				case "app_id":
					organization.Settings["meta_app_id"] = value
				case "config_id":
					organization.Settings["meta_config_id"] = value
				}
			}
			if _, clear := clearSet["app_secret"]; clear {
				delete(organization.Settings, metaAppSecretSetting)
				credentialsChanged = true
			}
			if value := credentialValues["app_secret"]; value != "" {
				encrypted, err := appcrypto.Encrypt(value, a.Config.App.EncryptionKey)
				if err != nil || !appcrypto.IsEncrypted(encrypted) {
					return errors.New("encrypt Meta credential")
				}
				organization.Settings[metaAppSecretSetting] = encrypted
				credentialsChanged = true
			}
			workspaceManaged := metaWorkspaceAppManaged(&organization)
			if _, clear := clearSet["webhook_verify_token"]; clear {
				delete(organization.Settings, metaWebhookVerifyTokenSetting)
				credentialsChanged = true
			}
			if value := credentialValues["webhook_verify_token"]; value != "" {
				if !workspaceManaged {
					return &integrationClientError{
						status:  fasthttp.StatusBadRequest,
						message: "The shared platform Meta app uses its deployment-managed webhook verify token; add workspace Meta app credentials before setting a workspace token",
					}
				}
				encrypted, err := appcrypto.Encrypt(value, a.Config.App.EncryptionKey)
				if err != nil || !appcrypto.IsEncrypted(encrypted) {
					return errors.New("encrypt Meta webhook verify token")
				}
				organization.Settings[metaWebhookVerifyTokenSetting] = encrypted
				credentialsChanged = true
			}
			if err := tx.Model(&models.Organization{}).Where("id = ?", orgID).Update("settings", organization.Settings).Error; err != nil {
				return err
			}
		case integrationProviderQwen:
			var copilot models.CopilotSettings
			result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("organization_id = ? AND whats_app_account = ''", orgID).
				Order("updated_at DESC, id ASC").First(&copilot)
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				copilot = models.CopilotSettings{
					BaseModel:      models.BaseModel{ID: uuid.New()},
					OrganizationID: orgID,
					Provider:       models.AIProviderQwen,
					Model:          qwenapi.DefaultModel,
					MaxTokens:      700,
					Temperature:    0.3,
					RetentionDays:  30,
					Version:        1,
				}
			} else if result.Error != nil {
				return result.Error
			}
			for key, value := range config {
				switch key {
				case "model":
					copilot.Model, _ = value.(string)
				case "max_tokens":
					copilot.MaxTokens, _ = numericJSONInt(value)
				case "temperature":
					copilot.Temperature, _ = numericJSONFloat(value)
				case "endpoint_region":
					if row.Config == nil {
						row.Config = models.JSONB{}
					}
					row.Config["endpoint_region"] = value
				}
			}
			if _, clear := clearSet["api_key"]; clear {
				copilot.APIKeyEncrypted = ""
				credentialsChanged = true
			}
			if value := credentialValues["api_key"]; value != "" {
				encrypted, err := appcrypto.Encrypt(value, a.Config.App.EncryptionKey)
				if err != nil || !appcrypto.IsEncrypted(encrypted) {
					return errors.New("encrypt Qwen credential")
				}
				copilot.APIKeyEncrypted = encrypted
				credentialsChanged = true
			}
			copilot.Provider = models.AIProviderQwen
			if request.Enabled != nil {
				copilot.IsEnabled = *request.Enabled
			} else if result.Error != nil && copilot.APIKeyEncrypted != "" {
				copilot.IsEnabled = true
			}
			copilot.UpdatedByID = &userID
			if copilot.CreatedByID == nil {
				copilot.CreatedByID = &userID
			}
			copilot.Version++
			if err := tx.Save(&copilot).Error; err != nil {
				return err
			}
		case integrationProviderThreads, integrationProviderTikTok:
			if row.Config == nil {
				row.Config = models.JSONB{}
			}
			for key, value := range config {
				row.Config[key] = value
			}
			if provider == integrationProviderThreads {
				if threadsReviewStatusProvided && stringJSONValue(row.Config, threadsreview.StatusKey) != threadsAppReviewApprovedStatus {
					row.Config = threadsreview.RemoveApproval(
						row.Config,
						stringJSONValue(row.Config, threadsreview.StatusKey),
					)
				}
				appID := stringJSONValue(row.Config, "app_id")
				if appID == "" {
					row.ThreadsAppID = nil
				} else {
					row.ThreadsAppID = &appID
				}
			}
			if row.CredentialData == nil {
				row.CredentialData = models.JSONB{}
			}
			for name := range clearSet {
				delete(row.CredentialData, name)
				credentialsChanged = true
			}
			for name, value := range credentialValues {
				encrypted, err := appcrypto.Encrypt(value, a.Config.App.EncryptionKey)
				if err != nil || !appcrypto.IsEncrypted(encrypted) {
					return fmt.Errorf("encrypt %s credential", provider)
				}
				row.CredentialData[name] = encrypted
				credentialsChanged = true
			}
		}

		if request.Enabled != nil {
			row.Enabled = *request.Enabled
		}
		if provider == integrationProviderTikTok {
			row.Enabled = false
		}
		if isNew && request.Enabled == nil {
			row.Enabled = false
		}
		threadsAuthorizationChanged := provider == integrationProviderThreads && !isNew &&
			(oldThreadsAppID != strings.TrimSpace(stringJSONValue(row.Config, "app_id")) ||
				threadsAppSecretCleared || threadsAppSecretReplaced)
		threadsReviewDowngraded := provider == integrationProviderThreads && oldThreadsApproved &&
			threadsReviewStatusProvided && stringJSONValue(row.Config, threadsreview.StatusKey) != threadsAppReviewApprovedStatus
		if provider == integrationProviderThreads && oldThreadsApproved && oldThreadsAppID != strings.TrimSpace(stringJSONValue(row.Config, "app_id")) {
			row.Config = threadsreview.RemoveApproval(row.Config, "not_submitted")
			threadsReviewDowngraded = true
		}
		threadsMustAutoDisable := provider == integrationProviderThreads &&
			(threadsAuthorizationChanged || threadsReviewDowngraded || legacyThreadsEnabledWithoutAccess)
		if threadsMustAutoDisable {
			row.Enabled = false
		}
		if provider == integrationProviderThreads &&
			((request.Enabled != nil && !*request.Enabled) || threadsMustAutoDisable) {
			if err := disconnectThreadsChannelAccounts(tx, orgID, userID, time.Now().UTC()); err != nil {
				return err
			}
		}
		if credentialsChanged {
			now := time.Now().UTC()
			row.CredentialsUpdatedAt = &now
		}
		threadsExplicitlyDisabled := provider == integrationProviderThreads &&
			request.Enabled != nil && !*request.Enabled
		if credentialsChanged || configChanged || threadsExplicitlyDisabled {
			row.LastTestedAt = nil
			row.LastSuccessfulAt = nil
			row.LastErrorCode = ""
			row.LastErrorMessage = ""
			row.ValidationToken = ""
		}
		row.UpdatedByID = &userID
		if row.CreatedByID == nil {
			row.CreatedByID = &userID
		}

		if row.Enabled {
			if err := a.validateEnabledIntegration(provider, &organization, row, tx, orgID); err != nil {
				status := fasthttp.StatusBadRequest
				if errors.Is(err, errThreadsAppReviewApprovalRequired) {
					status = fasthttp.StatusConflict
				}
				return &integrationClientError{status: status, message: err.Error()}
			}
		}
		if err := tx.Save(row).Error; err != nil {
			if provider == integrationProviderThreads && isUniqueViolation(err) {
				return &integrationClientError{
					status:  fasthttp.StatusConflict,
					message: "This Threads App ID is already bound to another ReReply workspace",
				}
			}
			return err
		}
		newSnapshot := integrationAuditSnapshot(provider, &organization, row, tx)
		var sensitive []map[string]any
		if credentialsChanged {
			sensitive = append(sensitive, map[string]any{
				"field":     "credentials",
				"old_value": "********",
				"new_value": "********",
			})
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			userName,
			models.ResourceSettingsIntegrations,
			row.ID,
			models.AuditActionUpdated,
			oldSnapshot,
			newSnapshot,
			sensitive...,
		)
	})
}

func disconnectThreadsChannelAccounts(tx *gorm.DB, orgID, userID uuid.UUID, now time.Time) error {
	return disconnectThreadsChannelAccountsWithActor(tx, orgID, &userID, now)
}

func disconnectThreadsChannelAccountsWithActor(
	tx *gorm.DB,
	orgID uuid.UUID,
	userID *uuid.UUID,
	now time.Time,
) error {
	var accounts []models.ChannelAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"organization_id = ? AND channel = ? AND provider = ?",
			orgID,
			models.ChannelThreads,
			channelapi.ThreadsProvider,
		).
		Find(&accounts).Error; err != nil {
		return err
	}
	return disconnectLockedThreadsChannelAccounts(tx, orgID, userID, now, accounts, nil)
}

// disconnectLockedThreadsChannelAccounts applies the disconnect fence to an
// already locked account set. A nil credentialKinds slice revokes every kind;
// lifecycle callbacks pass OAuth explicitly so unrelated future credentials
// are left alone.
func disconnectLockedThreadsChannelAccounts(
	tx *gorm.DB,
	orgID uuid.UUID,
	userID *uuid.UUID,
	now time.Time,
	accounts []models.ChannelAccount,
	credentialKinds []models.ChannelCredentialKind,
) error {
	accountIDs := make([]uuid.UUID, 0, len(accounts))
	for index := range accounts {
		account := &accounts[index]
		accountIDs = append(accountIDs, account.ID)
		if account.Config == nil {
			account.Config = models.JSONB{}
		}
		account.Config["outbound_enabled"] = false
		account.Status = models.ChannelAccountStatusDisconnected
		account.IsDefaultIncoming = false
		account.IsDefaultOutgoing = false
		account.UpdatedByID = userID
		if err := tx.Save(account).Error; err != nil {
			return err
		}
	}
	if len(accountIDs) == 0 {
		return nil
	}
	queuedStatuses := []models.OutboxJobStatus{
		models.OutboxJobStatusPending,
		models.OutboxJobStatusRetrying,
		models.OutboxJobStatusProcessing,
	}
	var cancelledJobs []models.OutboxJob
	if err := tx.Model(&cancelledJobs).
		Clauses(clause.Returning{Columns: []clause.Column{{Name: "message_id"}}}).
		Where(
			"organization_id = ? AND channel_account_id IN ? AND status IN ?",
			orgID,
			accountIDs,
			queuedStatuses,
		).
		Updates(map[string]any{
			"status":          models.OutboxJobStatusCancelled,
			"failed_at":       now,
			"last_error_code": "threads_disconnected",
			"last_error":      "Threads integration disconnected before delivery",
			"locked_at":       nil,
			"locked_by":       "",
			"updated_at":      now,
		}).Error; err != nil {
		return err
	}
	messageIDs := make([]uuid.UUID, 0, len(cancelledJobs))
	for index := range cancelledJobs {
		if cancelledJobs[index].MessageID != nil {
			messageIDs = append(messageIDs, *cancelledJobs[index].MessageID)
		}
	}
	if len(messageIDs) > 0 {
		if err := tx.Model(&models.Message{}).
			Where(
				"organization_id = ? AND id IN ? AND status = ?",
				orgID,
				messageIDs,
				models.MessageStatusPending,
			).
			Updates(map[string]any{
				"status":        models.MessageStatusFailed,
				"error_message": "Threads integration disconnected before delivery",
				"updated_at":    now,
			}).Error; err != nil {
			return err
		}
	}

	credentialQuery := tx.Model(&models.ChannelCredential{}).
		Where(
			"organization_id = ? AND channel_account_id IN ? AND status IN ?",
			orgID,
			accountIDs,
			[]models.ChannelCredentialStatus{
				models.ChannelCredentialStatusActive,
				models.ChannelCredentialStatusExpiring,
			},
		)
	if len(credentialKinds) > 0 {
		credentialQuery = credentialQuery.Where("kind IN ?", credentialKinds)
	}
	return credentialQuery.Updates(map[string]any{
		"status":     models.ChannelCredentialStatusRevoked,
		"revoked_at": now,
		"rotated_at": now,
		"updated_at": now,
	}).Error
}

func lockOrCreateIntegrationRow(tx *gorm.DB, orgID, userID uuid.UUID, provider string) (*models.ProviderIntegration, bool, error) {
	var row models.ProviderIntegration
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("organization_id = ? AND provider = ?", orgID, provider).
		First(&row).Error
	if err == nil {
		return &row, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}
	row = models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		Provider:       provider,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{},
		CreatedByID:    &userID,
		UpdatedByID:    &userID,
	}
	return &row, true, nil
}

func (a *App) validateEnabledIntegration(provider string, organization *models.Organization, row *models.ProviderIntegration, tx *gorm.DB, orgID uuid.UUID) error {
	switch provider {
	case integrationProviderMeta:
		appID, configID, hasSecret, _ := a.metaIntegrationValues(organization)
		workspaceManaged := metaWorkspaceAppManaged(organization)
		webhookCredential, _ := a.metaWebhookIntegrationCredential(
			organization,
			hasLegacyMetaWebhookToken(tx, orgID),
			row,
			workspaceManaged,
		)
		if strings.TrimSpace(appID) == "" || strings.TrimSpace(configID) == "" || !hasSecret || !webhookCredential.Configured {
			return errors.New("meta app ID, config ID, app secret, and webhook verify token are required before enabling")
		}
	case integrationProviderQwen:
		var settings models.CopilotSettings
		if err := tx.Where("organization_id = ? AND whats_app_account = ''", orgID).
			Order("updated_at DESC, id ASC").First(&settings).Error; err != nil {
			return errors.New("qwen API key and model are required before enabling")
		}
		if strings.TrimSpace(settings.APIKeyEncrypted) == "" || strings.TrimSpace(settings.Model) == "" {
			return errors.New("qwen API key and model are required before enabling")
		}
		if strings.TrimSpace(a.qwenBaseURLForRow(row)) == "" {
			return errors.New("qwen endpoint region is invalid")
		}
	case integrationProviderThreads:
		if _, err := a.requireThreadsAppReviewAccess(orgID, row.Config, ""); err != nil {
			return err
		}
		if stringJSONValue(row.Config, "app_id") == "" || stringJSONValue(row.Config, "redirect_uri") == "" ||
			!encryptedCredentialFlag(row, "app_secret").Configured ||
			!encryptedCredentialFlag(row, "webhook_verify_token").Configured {
			return errors.New("threads app ID, redirect URI, app secret, and webhook verify token are required before enabling")
		}
	case integrationProviderTikTok:
		if stringJSONValue(row.Config, "client_id") == "" || stringJSONValue(row.Config, "redirect_uri") == "" || !encryptedCredentialFlag(row, "client_secret").Configured {
			return errors.New("tiktok client ID, redirect URI, and client secret are required before enabling")
		}
	}
	return nil
}

func integrationAuditSnapshot(provider string, organization *models.Organization, row *models.ProviderIntegration, tx *gorm.DB) map[string]any {
	snapshot := map[string]any{
		"provider": provider,
		"enabled":  row != nil && row.Enabled,
	}
	switch provider {
	case integrationProviderMeta:
		if organization != nil {
			snapshot["app_id"] = organization.Settings["meta_app_id"]
			snapshot["config_id"] = organization.Settings["meta_config_id"]
			secret, _ := organization.Settings[metaAppSecretSetting].(string)
			snapshot["credentials_configured"] = secret != ""
			webhookToken, _ := organization.Settings[metaWebhookVerifyTokenSetting].(string)
			snapshot["webhook_verify_token_configured"] = webhookToken != ""
		}
	case integrationProviderQwen:
		if row != nil {
			snapshot["endpoint_region"] = qwenEndpointRegion(row)
		}
		if tx != nil && organization != nil {
			var settings models.CopilotSettings
			if err := tx.Where("organization_id = ? AND whats_app_account = ''", organization.ID).
				Order("updated_at DESC, id ASC").First(&settings).Error; err == nil {
				snapshot["model"] = settings.Model
				snapshot["max_tokens"] = settings.MaxTokens
				snapshot["temperature"] = settings.Temperature
				snapshot["credentials_configured"] = settings.APIKeyEncrypted != ""
			}
		}
	default:
		if row != nil {
			snapshot["config"] = safeIntegrationConfig(provider, row.Config)
			for _, name := range integrationCredentialNames(provider) {
				snapshot[name+"_configured"] = encryptedCredentialFlag(row, name).Configured
			}
		}
	}
	return snapshot
}

func validateIntegrationConfig(provider string, input models.JSONB) (models.JSONB, error) {
	if input == nil {
		return models.JSONB{}, nil
	}
	var allowed map[string]bool
	switch provider {
	case integrationProviderMeta:
		allowed = map[string]bool{"app_id": true, "config_id": true}
	case integrationProviderThreads:
		allowed = map[string]bool{"app_id": true, "redirect_uri": true, "app_review_status": true}
	case integrationProviderTikTok:
		allowed = map[string]bool{"client_id": true, "redirect_uri": true, "approval_status": true}
	case integrationProviderQwen:
		allowed = map[string]bool{"model": true, "max_tokens": true, "temperature": true, "endpoint_region": true}
	default:
		return nil, errors.New("unsupported integration provider")
	}
	result := models.JSONB{}
	for key, value := range input {
		if !allowed[key] {
			return nil, fmt.Errorf("config.%s is not supported for %s", key, provider)
		}
		switch key {
		case "app_id", "config_id", "client_id":
			text, ok := value.(string)
			text = strings.TrimSpace(text)
			if !ok || len(text) > 255 || (text != "" && !integrationIdentifierPattern.MatchString(text)) {
				return nil, fmt.Errorf("config.%s is invalid", key)
			}
			if provider == integrationProviderThreads && key == "app_id" && text != "" && !validThreadsOAuthID(text) {
				return nil, errors.New("config.app_id must be a numeric Threads App ID")
			}
			result[key] = text
		case "redirect_uri":
			text, ok := value.(string)
			text = strings.TrimSpace(text)
			if !ok || len(text) > 2048 || validateIntegrationRedirectURI(text) != nil {
				return nil, errors.New("config.redirect_uri must be an HTTPS URL without credentials or a fragment")
			}
			if provider == integrationProviderThreads && validateThreadsRedirectURI(text) != nil {
				return nil, errors.New("config.redirect_uri must use HTTPS and the exact /api/integrations/threads/callback path without a query")
			}
			result[key] = text
		case "app_review_status", "approval_status":
			text, ok := value.(string)
			text = strings.ToLower(strings.TrimSpace(text))
			if !ok || !stringInSet(text, "not_submitted", "pending", "approved", "rejected") {
				return nil, fmt.Errorf("config.%s must be not_submitted, pending, approved, or rejected", key)
			}
			result[key] = text
		case "model":
			text, ok := value.(string)
			text = strings.TrimSpace(text)
			if !ok || text == "" || len(text) > 100 || !integrationIdentifierPattern.MatchString(text) {
				return nil, errors.New("config.model is invalid")
			}
			result[key] = text
		case "max_tokens":
			number, ok := numericJSONInt(value)
			if !ok || number < 64 || number > 4000 {
				return nil, errors.New("config.max_tokens must be between 64 and 4000")
			}
			result[key] = number
		case "temperature":
			number, ok := numericJSONFloat(value)
			if !ok || number < 0 || number > 2 {
				return nil, errors.New("config.temperature must be between 0 and 2")
			}
			result[key] = number
		case "endpoint_region":
			text, ok := value.(string)
			text = strings.ToLower(strings.TrimSpace(text))
			if !ok || !stringInSet(text, "platform", qwenapi.RegionSingapore, qwenapi.RegionUS, qwenapi.RegionBeijing) {
				return nil, errors.New("config.endpoint_region must be platform, singapore, us, or china_beijing")
			}
			result[key] = text
		}
	}
	return result, nil
}

func validateIntegrationCredentials(provider string, values map[string]string, clear []string) (map[string]string, map[string]struct{}, error) {
	allowedNames := integrationCredentialNames(provider)
	allowed := make(map[string]struct{}, len(allowedNames))
	for _, name := range allowedNames {
		allowed[name] = struct{}{}
	}
	result := map[string]string{}
	for name, value := range values {
		if _, ok := allowed[name]; !ok {
			return nil, nil, fmt.Errorf("credentials.%s is not supported for %s", name, provider)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) < 8 || len(value) > 8192 {
			return nil, nil, fmt.Errorf("credentials.%s has an invalid length", name)
		}
		if provider == integrationProviderThreads && name == "webhook_verify_token" && len(value) < 16 {
			return nil, nil, errors.New("credentials.webhook_verify_token must be at least 16 characters")
		}
		result[name] = value
	}
	clearSet := map[string]struct{}{}
	for _, name := range clear {
		name = strings.TrimSpace(name)
		if _, ok := allowed[name]; !ok {
			return nil, nil, fmt.Errorf("clear_credentials contains unsupported field %s", name)
		}
		if _, supplied := result[name]; supplied {
			return nil, nil, fmt.Errorf("credentials.%s cannot be set and cleared in the same request", name)
		}
		clearSet[name] = struct{}{}
	}
	return result, clearSet, nil
}

func validateIntegrationRedirectURI(value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("invalid redirect URI")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	hostname := strings.ToLower(parsed.Hostname())
	if parsed.Scheme == "http" && (hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1") {
		return nil
	}
	return errors.New("redirect URI must use HTTPS")
}

func validateThreadsRedirectURI(value string) error {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.Fragment != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Opaque != "" ||
		parsed.RawPath != "" || parsed.EscapedPath() != parsed.Path ||
		!strings.HasSuffix(parsed.Path, threadsOAuthCallbackPath) {
		return errors.New("invalid Threads redirect URI")
	}
	basePath := strings.TrimSuffix(parsed.Path, threadsOAuthCallbackPath)
	if basePath != "" && (sanitizeRedirectPath(basePath) != basePath || strings.Contains(basePath, "\\")) {
		return errors.New("invalid Threads redirect URI base path")
	}
	for _, segment := range strings.Split(basePath, "/") {
		if segment == "." || segment == ".." {
			return errors.New("invalid Threads redirect URI base path")
		}
	}
	return nil
}

func integrationCredentialNames(provider string) []string {
	switch provider {
	case integrationProviderMeta:
		return []string{"app_secret", "webhook_verify_token"}
	case integrationProviderThreads:
		return []string{"app_secret", "webhook_verify_token"}
	case integrationProviderTikTok:
		return []string{"client_secret"}
	case integrationProviderQwen:
		return []string{"api_key"}
	default:
		return nil
	}
}

func integrationProviderFromRequest(r *fastglue.Request, allowReadOnly bool) (string, error) {
	provider := strings.ToLower(strings.TrimSpace(stringPathValue(r, "provider")))
	switch provider {
	case integrationProviderMeta, integrationProviderThreads, integrationProviderTikTok, integrationProviderQwen, integrationProviderGoogleSearchConsole:
		return provider, nil
	case integrationProviderEmail, integrationProviderWebchat:
		if allowReadOnly {
			return provider, nil
		}
		return provider, nil
	default:
		return "", errors.New("unsupported integration provider")
	}
}

func integrationDisplayName(provider string) string {
	switch provider {
	case integrationProviderMeta:
		return "Meta / WhatsApp"
	case integrationProviderThreads:
		return "Threads"
	case integrationProviderTikTok:
		return "TikTok Business"
	case integrationProviderQwen:
		return "Qwen Copilot"
	case integrationProviderGoogleSearchConsole:
		return "Google Search Console"
	case integrationProviderEmail:
		return "Email"
	case integrationProviderWebchat:
		return "Webchat"
	default:
		return provider
	}
}

func integrationRowConfig(row *models.ProviderIntegration) models.JSONB {
	if row == nil {
		return models.JSONB{}
	}
	return row.Config
}

func safeIntegrationConfig(provider string, config models.JSONB) models.JSONB {
	result := models.JSONB{}
	var allowed []string
	switch provider {
	case integrationProviderThreads:
		allowed = []string{"app_id", "redirect_uri", "app_review_status"}
	case integrationProviderTikTok:
		allowed = []string{"client_id", "redirect_uri", "approval_status"}
	}
	for _, key := range allowed {
		if value, exists := config[key]; exists {
			result[key] = value
		}
	}
	return result
}

func encryptedCredentialFlag(row *models.ProviderIntegration, name string) integrationCredentialResponse {
	response := integrationCredentialResponse{UpdatedAt: integrationCredentialUpdatedAt(row), Source: "workspace"}
	if row == nil || row.CredentialData == nil {
		return response
	}
	value, _ := row.CredentialData[name].(string)
	response.Configured = appcrypto.IsEncrypted(strings.TrimSpace(value))
	return response
}

func integrationCredentialUpdatedAt(row *models.ProviderIntegration) *time.Time {
	if row == nil {
		return nil
	}
	return row.CredentialsUpdatedAt
}

func metaWorkspaceAppManaged(organization *models.Organization) bool {
	if organization == nil || organization.Settings == nil {
		return false
	}
	for _, key := range []string{"meta_app_id", "meta_config_id", metaAppSecretSetting} {
		if value, ok := organization.Settings[key].(string); ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func (a *App) metaIntegrationValues(organization *models.Organization) (appID, configID string, hasSecret bool, source string) {
	if metaWorkspaceAppManaged(organization) {
		appID, _ = organization.Settings["meta_app_id"].(string)
		configID, _ = organization.Settings["meta_config_id"].(string)
		secret, _ := organization.Settings[metaAppSecretSetting].(string)
		return strings.TrimSpace(appID), strings.TrimSpace(configID), strings.TrimSpace(secret) != "", "workspace"
	}
	if a.Config != nil {
		appID = strings.TrimSpace(a.Config.WhatsApp.AppID)
		configID = strings.TrimSpace(a.Config.WhatsApp.ConfigID)
		hasSecret = strings.TrimSpace(a.Config.WhatsApp.AppSecret) != ""
		if appID != "" || configID != "" || hasSecret {
			source = "platform"
		}
	}
	return appID, configID, hasSecret, source
}

func metaWebhookCallbackPath(orgID uuid.UUID, workspaceManaged bool) string {
	if !workspaceManaged {
		return "/api/webhook"
	}
	return "/api/webhook?" + metaWebhookCallbackWorkspaceQueryKey + "=" + orgID.String()
}

func hasLegacyMetaWebhookToken(db *gorm.DB, orgID uuid.UUID) bool {
	if db == nil || orgID == uuid.Nil {
		return false
	}
	var count int64
	return db.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND webhook_verify_token <> ''", orgID).
		Limit(1).
		Count(&count).Error == nil && count > 0
}

// metaWebhookIntegrationCredential exposes only configuration state. The
// workspace token itself remains encrypted and is never included in an API or
// audit response. Account-column tokens are acknowledged only as a legacy
// fallback so administrators can transition without rotating a live token.
func (a *App) metaWebhookIntegrationCredential(
	organization *models.Organization,
	hasLegacyAccountToken bool,
	row *models.ProviderIntegration,
	workspaceManaged bool,
) (integrationCredentialResponse, bool) {
	response := integrationCredentialResponse{UpdatedAt: integrationCredentialUpdatedAt(row)}
	if workspaceManaged && organization != nil && organization.Settings != nil {
		if value, ok := organization.Settings[metaWebhookVerifyTokenSetting].(string); ok && strings.TrimSpace(value) != "" {
			value = strings.TrimSpace(value)
			response.Configured = true
			response.Source = "workspace"
			if !appcrypto.IsEncrypted(value) || !a.hasIntegrationEncryptionKey() {
				return response, false
			}
			decrypted, err := appcrypto.Decrypt(value, a.integrationEncryptionKey())
			return response, err == nil && strings.TrimSpace(decrypted) != ""
		}
	}
	if !workspaceManaged && a != nil && a.Config != nil && strings.TrimSpace(a.Config.WhatsApp.WebhookVerifyToken) != "" {
		response.Configured = true
		response.Source = "platform"
		return response, true
	}
	if hasLegacyAccountToken {
		response.Configured = true
		response.Source = "legacy_account"
		return response, true
	}
	return response, false
}

// metaIntegrationCredentialUsable reports whether the effective Meta secret
// can be consumed by this process. A workspace override is authoritative over
// a platform secret, so an encrypted override without the server key must fail
// closed rather than silently falling back to another credential.
func (a *App) metaIntegrationCredentialUsable(organization *models.Organization) bool {
	if metaWorkspaceAppManaged(organization) && organization != nil && organization.Settings != nil {
		if value, ok := organization.Settings[metaAppSecretSetting].(string); ok && strings.TrimSpace(value) != "" {
			return a.storedIntegrationCredentialUsable(value)
		}
		return false
	}
	return a != nil && a.Config != nil && strings.TrimSpace(a.Config.WhatsApp.AppSecret) != ""
}

func (a *App) qwenIntegrationCredentialUsable(copilot *models.CopilotSettings) bool {
	if copilot != nil {
		return a.storedIntegrationCredentialUsable(copilot.APIKeyEncrypted)
	}
	return false
}

func (a *App) storedIntegrationCredentialUsable(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if !appcrypto.IsEncrypted(value) {
		return true
	}
	if !a.hasIntegrationEncryptionKey() {
		return false
	}
	decrypted, err := appcrypto.Decrypt(value, a.integrationEncryptionKey())
	return err == nil && strings.TrimSpace(decrypted) != ""
}

func qwenIntegrationValues(copilot *models.CopilotSettings) (model string, maxTokens int, temperature float64, configured bool, source string, enabled bool) {
	if copilot != nil {
		model = strings.TrimSpace(copilot.Model)
		maxTokens = copilot.MaxTokens
		temperature = copilot.Temperature
		configured = strings.TrimSpace(copilot.APIKeyEncrypted) != ""
		source = "copilot"
		enabled = copilot.IsEnabled
		if model == "" {
			model = qwenapi.DefaultModel
		}
	}
	if model == "" {
		model = qwenapi.DefaultModel
	}
	if maxTokens < 64 || maxTokens > 4000 {
		maxTokens = 700
	}
	if temperature < 0 || temperature > 2 {
		temperature = 0.3
	}
	return
}

func (a *App) prepareIntegrationTest(orgID, userID uuid.UUID, snapshot *integrationTestSnapshot) error {
	if snapshot == nil {
		return errors.New("integration test snapshot is required")
	}
	return a.DB.Transaction(func(tx *gorm.DB) error {
		return a.scopedApp(tx, orgID).prepareIntegrationTestTx(orgID, userID, snapshot)
	})
}

func (a *App) prepareIntegrationTestTx(orgID, userID uuid.UUID, snapshot *integrationTestSnapshot) error {
	var organization models.Organization
	if err := a.DB.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", orgID).First(&organization).Error; err != nil {
		return err
	}
	switch snapshot.provider {
	case integrationProviderMeta:
		appID, appSecret, _, err := a.resolveMetaAppCreds(orgID)
		if err != nil {
			return &integrationClientError{status: fasthttp.StatusServiceUnavailable, message: "Meta credentials could not be decrypted safely"}
		}
		if strings.TrimSpace(appID) == "" || strings.TrimSpace(appSecret) == "" {
			return &integrationClientError{status: fasthttp.StatusConflict, message: "Configure Meta credentials before testing"}
		}
		snapshot.appID = appID
		snapshot.appSecret = appSecret
	case integrationProviderQwen:
		var settings models.CopilotSettings
		if err := a.DB.Where("organization_id = ? AND whats_app_account = ''", orgID).
			Order("updated_at DESC, id ASC").First(&settings).Error; err != nil {
			return &integrationClientError{status: fasthttp.StatusConflict, message: "Configure Qwen Copilot before testing"}
		}
		if appcrypto.IsEncrypted(settings.APIKeyEncrypted) && !a.hasIntegrationEncryptionKey() {
			return &integrationClientError{status: fasthttp.StatusServiceUnavailable, message: "Qwen credentials cannot be decrypted because server-side encryption is not configured"}
		}
		apiKey, err := appcrypto.Decrypt(settings.APIKeyEncrypted, a.integrationEncryptionKey())
		if err != nil || strings.TrimSpace(apiKey) == "" || !settings.IsEnabled {
			return &integrationClientError{status: fasthttp.StatusConflict, message: "Enable Qwen Copilot and configure an API key before testing"}
		}
		snapshot.qwenAPIKey = apiKey
		snapshot.qwenModel = settings.Model
		snapshot.qwenMaxTokens = settings.MaxTokens
		snapshot.qwenTemperature = settings.Temperature
	}
	row, isNew, err := lockOrCreateIntegrationRow(a.DB, orgID, userID, snapshot.provider)
	if err != nil {
		return err
	}
	if isNew {
		row.Enabled = true
	}
	if snapshot.provider == integrationProviderQwen {
		snapshot.qwenBaseURL = a.qwenBaseURLForRow(row)
		if strings.TrimSpace(snapshot.qwenBaseURL) == "" {
			return &integrationClientError{status: fasthttp.StatusConflict, message: "Configure a valid Qwen endpoint region before testing"}
		}
	}
	row.ValidationToken = snapshot.validationToken
	row.UpdatedByID = &userID
	if err := a.DB.Save(row).Error; err != nil {
		return err
	}
	return nil
}

func (a *App) finishIntegrationTest(orgID, userID uuid.UUID, snapshot integrationTestSnapshot, testedAt time.Time, validationErr error) error {
	updates := map[string]any{
		"last_tested_at":   testedAt,
		"updated_by_id":    userID,
		"updated_at":       testedAt,
		"validation_token": "",
	}
	if validationErr == nil {
		updates["last_successful_at"] = testedAt
		updates["last_error_code"] = integrationValidationSuccessCode
		updates["last_error_message"] = ""
	} else {
		updates["last_error_code"] = integrationValidationFailedCode
		updates["last_error_message"] = "Provider validation failed"
	}
	result := a.DB.Model(&models.ProviderIntegration{}).
		Where("organization_id = ? AND provider = ? AND validation_token = ?", orgID, snapshot.provider, snapshot.validationToken).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (a *App) testMetaApplication(r *fastglue.Request, appID, appSecret string) error {
	baseURL := whatsapp.BaseURL
	if a.Config != nil && strings.TrimSpace(a.Config.WhatsApp.BaseURL) != "" {
		baseURL = strings.TrimRight(strings.TrimSpace(a.Config.WhatsApp.BaseURL), "/")
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/oauth/access_token"
	form := url.Values{
		"client_id":     []string{appID},
		"client_secret": []string{appSecret},
		"grant_type":    []string{"client_credentials"},
	}
	ctx, cancel := context.WithTimeout(requestContext(r), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("create Meta validation request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	requestClient := *client
	requestClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := requestClient.Do(request)
	if err != nil {
		return errors.New("meta validation request failed")
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 || response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("meta rejected application credentials")
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.AccessToken) == "" {
		return errors.New("meta validation response was invalid")
	}
	return nil
}

func (a *App) testQwenProvider(r *fastglue.Request, snapshot integrationTestSnapshot) error {
	maxTokens := snapshot.qwenMaxTokens
	if maxTokens < 8 || maxTokens > 32 {
		maxTokens = 8
	}
	ctx, cancel := context.WithTimeout(requestContext(r), 20*time.Second)
	defer cancel()
	_, err := qwenapi.Generate(ctx, a.HTTPClient, qwenapi.Options{
		APIKey:      snapshot.qwenAPIKey,
		BaseURL:     snapshot.qwenBaseURL,
		Model:       snapshot.qwenModel,
		MaxTokens:   maxTokens,
		Temperature: snapshot.qwenTemperature,
		Messages: []qwenapi.Message{{
			Role:    "user",
			Content: "Reply with OK.",
		}},
	})
	return err
}

func (a *App) hasIntegrationEncryptionKey() bool {
	return strings.TrimSpace(a.integrationEncryptionKey()) != ""
}

func (a *App) integrationEncryptionKey() string {
	if a == nil || a.Config == nil {
		return ""
	}
	return a.Config.App.EncryptionKey
}

func (a *App) metaAPIVersion() string {
	if a != nil && a.Config != nil && strings.TrimSpace(a.Config.WhatsApp.APIVersion) != "" {
		return strings.TrimSpace(a.Config.WhatsApp.APIVersion)
	}
	return "v21.0"
}

func (a *App) qwenBaseURL() string {
	if a != nil && a.Config != nil && strings.TrimSpace(a.Config.AI.QwenBaseURL) != "" {
		return strings.TrimSpace(a.Config.AI.QwenBaseURL)
	}
	return qwenapi.DefaultBaseURL
}

func (a *App) qwenPublicBaseURL() string {
	parsed, err := url.Parse(a.qwenBaseURL())
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func qwenEndpointRegion(row *models.ProviderIntegration) string {
	if row == nil || row.Config == nil {
		return "platform"
	}
	region, _ := row.Config["endpoint_region"].(string)
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		return "platform"
	}
	return region
}

func (a *App) qwenBaseURLForRow(row *models.ProviderIntegration) string {
	region := qwenEndpointRegion(row)
	if region == "platform" {
		return a.qwenBaseURL()
	}
	baseURL, err := qwenapi.BaseURLForRegion(region)
	if err != nil {
		return ""
	}
	return baseURL
}

func (a *App) qwenPublicBaseURLForRow(row *models.ProviderIntegration) string {
	parsed, err := url.Parse(a.qwenBaseURLForRow(row))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func (a *App) sendIntegrationMutationError(r *fastglue.Request, err error) error {
	var clientErr *integrationClientError
	if errors.As(err, &clientErr) {
		return r.SendErrorEnvelope(clientErr.status, clientErr.message, nil, "")
	}
	a.Log.Error("Failed to update integration settings", "error", err)
	return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update integration settings", nil, "")
}

func stringJSONValue(config models.JSONB, key string) string {
	value, _ := config[key].(string)
	return strings.TrimSpace(value)
}

func numericJSONInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed != float64(int(typed)) {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		parsed, err := strconv.Atoi(typed.String())
		return parsed, err == nil
	default:
		return 0, false
	}
}

func numericJSONFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func stringInSet(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}
