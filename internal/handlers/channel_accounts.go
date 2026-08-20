package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrChannelAdapterUnavailable = errors.New("channel provider adapter is not available")
var ErrLegacyMetaAccountManaged = errors.New("legacy Meta channel accounts are managed by WhatsApp setup")
var ErrThreadsAccountManaged = errors.New("threads accounts using OAuth are managed in Settings > Integrations")
var ErrMetaRegistryAccountManaged = errors.New("OAuth-managed Meta account routing and credentials are managed by Meta onboarding")
var ErrChannelAccountChangedDuringValidation = errors.New("channel account changed during validation")
var ErrChannelAccountValidationSuperseded = errors.New("channel account validation was superseded")

var channelProviderIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var channelExternalAccountIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+-]{0,254}$`)

const (
	threadsPublicEngagementMode            = "public_replies_mentions"
	channelAccountHealthValidationTokenKey = "health_validation_token"
	metaRegistryHealthApprovalMaxAge       = 15 * time.Minute
)

type ChannelAccountRequest struct {
	Channel           models.Channel `json:"channel"`
	Provider          string         `json:"provider"`
	Name              string         `json:"name"`
	ExternalAccountID string         `json:"external_account_id"`
	Config            models.JSONB   `json:"config"`
	Capabilities      models.JSONB   `json:"capabilities"`
	OutboundSecret    string         `json:"outbound_secret"`
	IsDefaultIncoming bool           `json:"is_default_incoming"`
	IsDefaultOutgoing bool           `json:"is_default_outgoing"`
}

type UpdateChannelAccountRequest struct {
	Name              *string       `json:"name,omitempty"`
	Config            *models.JSONB `json:"config,omitempty"`
	Capabilities      *models.JSONB `json:"capabilities,omitempty"`
	OutboundSecret    string        `json:"outbound_secret,omitempty"`
	OutboundEnabled   *bool         `json:"outbound_enabled,omitempty"`
	AIReplyEnabled    *bool         `json:"ai_reply_enabled,omitempty"`
	IsDefaultIncoming *bool         `json:"is_default_incoming,omitempty"`
	IsDefaultOutgoing *bool         `json:"is_default_outgoing,omitempty"`
}

type ChannelAccountResponse struct {
	ID                                     uuid.UUID                   `json:"id"`
	OrganizationID                         uuid.UUID                   `json:"organization_id"`
	Channel                                models.Channel              `json:"channel"`
	Provider                               string                      `json:"provider"`
	Name                                   string                      `json:"name"`
	ExternalAccountID                      string                      `json:"external_account_id"`
	Status                                 models.ChannelAccountStatus `json:"status"`
	Capabilities                           models.JSONB                `json:"capabilities"`
	Config                                 models.JSONB                `json:"config"`
	IsDefaultIncoming                      bool                        `json:"is_default_incoming"`
	IsDefaultOutgoing                      bool                        `json:"is_default_outgoing"`
	HasCredentials                         bool                        `json:"has_credentials"`
	MetaSubscriptionReconciliationRequired bool                        `json:"meta_subscription_reconciliation_required"`
	MetaSubscriptionReconciliationDesired  string                      `json:"meta_subscription_reconciliation_desired_state,omitempty"`
	ConnectedAt                            *time.Time                  `json:"connected_at,omitempty"`
	LastHealthCheckAt                      *time.Time                  `json:"last_health_check_at,omitempty"`
	LastInboundAt                          *time.Time                  `json:"last_inbound_at,omitempty"`
	LastOutboundAt                         *time.Time                  `json:"last_outbound_at,omitempty"`
	LastErrorAt                            *time.Time                  `json:"last_error_at,omitempty"`
	LastError                              string                      `json:"last_error,omitempty"`
	OutboxPending                          int64                       `json:"outbox_pending"`
	OutboxFailed                           int64                       `json:"outbox_failed"`
	CreatedByID                            *uuid.UUID                  `json:"created_by_id,omitempty"`
	UpdatedByID                            *uuid.UUID                  `json:"updated_by_id,omitempty"`
	CreatedAt                              time.Time                   `json:"created_at"`
	UpdatedAt                              time.Time                   `json:"updated_at"`
}

type CreateChannelAccountResponse struct {
	Account       ChannelAccountResponse `json:"account"`
	InboundSecret string                 `json:"inbound_secret"`
	WebhookPath   string                 `json:"webhook_path"`
}

func (a *App) ListChannelAccounts(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceChannelAccounts, models.ActionRead)
	if err != nil {
		return nil
	}

	var accounts []models.ChannelAccount
	query := a.DB.
		Preload("Credentials", "organization_id = ?", orgID).
		Where("organization_id = ?", orgID).
		Order("created_at DESC")
	if channelName := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("channel"))); channelName != "" {
		query = query.Where("channel = ?", strings.ToLower(channelName))
	}
	if status := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("status"))); status != "" {
		query = query.Where("status = ?", strings.ToLower(status))
	}
	if err := query.Find(&accounts).Error; err != nil {
		a.Log.Error("Failed to list channel accounts", "error", err, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list channel accounts", nil, "")
	}

	response := make([]ChannelAccountResponse, len(accounts))
	for i := range accounts {
		response[i] = channelAccountToResponse(&accounts[i])
	}
	type outboxCount struct {
		ChannelAccountID uuid.UUID
		Status           models.OutboxJobStatus
		Count            int64
	}
	var outboxCounts []outboxCount
	if err := a.DB.Model(&models.OutboxJob{}).
		Select("channel_account_id, status, COUNT(*) AS count").
		Where(
			"organization_id = ? AND status IN ?",
			orgID,
			[]models.OutboxJobStatus{
				models.OutboxJobStatusPending,
				models.OutboxJobStatusProcessing,
				models.OutboxJobStatusDispatching,
				models.OutboxJobStatusRetrying,
				models.OutboxJobStatusFailed,
			},
		).
		Group("channel_account_id, status").
		Scan(&outboxCounts).Error; err != nil {
		a.Log.Error("Failed to summarize channel outbox", "error", err, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list channel accounts", nil, "")
	}
	responseByID := make(map[uuid.UUID]*ChannelAccountResponse, len(response))
	for i := range response {
		responseByID[response[i].ID] = &response[i]
	}
	for _, count := range outboxCounts {
		item := responseByID[count.ChannelAccountID]
		if item == nil {
			continue
		}
		if count.Status == models.OutboxJobStatusFailed {
			item.OutboxFailed += count.Count
		} else {
			item.OutboxPending += count.Count
		}
	}
	return r.SendEnvelope(map[string]any{"accounts": response})
}

func (a *App) CreateChannelAccount(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceChannelAccounts, models.ActionWrite)
	if err != nil {
		return nil
	}
	if a.Config == nil || strings.TrimSpace(a.Config.App.EncryptionKey) == "" {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Channel credential encryption is not configured", nil, "")
	}

	var request ChannelAccountRequest
	if err := a.decodeRequest(r, &request); err != nil {
		return nil
	}
	request.Channel = models.Channel(strings.ToLower(strings.TrimSpace(string(request.Channel))))
	request.Provider = strings.ToLower(strings.TrimSpace(request.Provider))
	request.Name = strings.TrimSpace(request.Name)
	request.ExternalAccountID = strings.TrimSpace(request.ExternalAccountID)
	if request.Channel == "" || request.Provider == "" || request.Name == "" || request.ExternalAccountID == "" {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"channel, provider, name, and external_account_id are required",
			nil,
			"",
		)
	}
	if !validChannelIdentifier(request.Channel) || !channelProviderIdentifier.MatchString(request.Provider) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Unsupported channel or invalid provider identifier", nil, "")
	}
	if request.Provider == channelapi.LegacyMetaProvider {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"meta_legacy is reserved for the managed WhatsApp inbox bridge",
			nil,
			"",
		)
	}
	if hasReservedMetaRegistryMarker(request.Config) {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"OAuth-managed Meta accounts must be created through Meta onboarding",
			nil,
			"",
		)
	}
	if request.Channel == models.ChannelInstagram && request.Provider == channelapi.RelayProvider &&
		a.Config.MetaInstagram.Enabled &&
		a.Config.MetaInstagram.OrganizationManaged(orgID.String()) {
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"Instagram accounts in this workspace must be connected through managed Meta onboarding",
			nil,
			"",
		)
	}
	if err := validateChannelCreationPolicy(request); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if utf8.RuneCountInString(request.Name) > 100 || !channelExternalAccountIdentifier.MatchString(request.ExternalAccountID) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid channel account name or external account identifier", nil, "")
	}
	if unsafeConfigKey(request.Config) != "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Config contains a restricted credential or network-policy key", nil, "")
	}
	if request.Provider == channelapi.RelayProvider && stringConfigValue(request.Config, "relay_url") == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "config.relay_url is required for relay accounts", nil, "")
	}
	if request.Provider == channelapi.RelayProvider {
		if err := validateRelayAccountConfig(request.Config); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
		}
	}
	if entitlementKey, required := additionalChannelCreationEntitlement(request.Channel); required {
		allowed, entitlementErr := a.HasProductEntitlement(userID, orgID, entitlementKey)
		if entitlementErr != nil {
			a.Log.Error(
				"Failed to evaluate channel-specific product entitlement",
				"error",
				entitlementErr,
				"organization_id",
				orgID,
				"channel",
				request.Channel,
				"entitlement",
				entitlementKey,
			)
			return r.SendErrorEnvelope(
				fasthttp.StatusInternalServerError,
				"Threads public engagement entitlement could not be evaluated",
				nil,
				"",
			)
		}
		if !allowed {
			return r.SendErrorEnvelope(
				fasthttp.StatusPaymentRequired,
				"Threads public replies are not included in the organization's active plan",
				nil,
				"",
			)
		}
	}

	if a.Config == nil || strings.TrimSpace(a.Config.App.EncryptionKey) == "" {
		return r.SendErrorEnvelope(
			fasthttp.StatusServiceUnavailable,
			"Channel credential storage is unavailable",
			nil,
			"",
		)
	}
	inboundSecret, err := generateChannelSecret()
	if err != nil {
		a.Log.Error("Failed to generate channel webhook secret", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create channel account", nil, "")
	}
	encryptedInbound, err := appcrypto.Encrypt(inboundSecret, a.Config.App.EncryptionKey)
	if err != nil || !appcrypto.IsEncrypted(encryptedInbound) {
		a.Log.Error("Failed to encrypt channel webhook secret", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create channel account", nil, "")
	}
	credentialBlob := models.JSONB{"inbound_secret": encryptedInbound}
	if request.OutboundSecret != "" {
		encryptedOutbound, encryptErr := appcrypto.Encrypt(request.OutboundSecret, a.Config.App.EncryptionKey)
		if encryptErr != nil || !appcrypto.IsEncrypted(encryptedOutbound) {
			a.Log.Error("Failed to encrypt channel outbound secret", "error", encryptErr)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create channel account", nil, "")
		}
		credentialBlob["outbound_secret"] = encryptedOutbound
	}

	config := cloneJSONB(request.Config)
	capabilities := cloneJSONB(request.Capabilities)
	if request.Channel == models.ChannelThreads {
		config = threadsPublicEngagementConfig(config)
		capabilities = threadsPublicEngagementCapabilities(capabilities)
	}
	// Activation and outbound approval happen only after TestChannelAccount.
	config["outbound_enabled"] = false
	config["ai_reply_enabled"] = false
	account := models.ChannelAccount{
		OrganizationID:    orgID,
		Channel:           request.Channel,
		Provider:          request.Provider,
		Name:              request.Name,
		ExternalAccountID: request.ExternalAccountID,
		Status:            models.ChannelAccountStatusPending,
		Capabilities:      capabilities,
		Config:            config,
		Metadata:          models.JSONB{},
		IsDefaultIncoming: request.IsDefaultIncoming,
		IsDefaultOutgoing: request.IsDefaultOutgoing,
		CreatedByID:       &userID,
		UpdatedByID:       &userID,
	}
	credential := models.ChannelCredential{
		OrganizationID:  orgID,
		Kind:            models.ChannelCredentialKindWebhook,
		Version:         1,
		CredentialBlob:  credentialBlob,
		Status:          models.ChannelCredentialStatusActive,
		KeyVersion:      "app:v1",
		Metadata:        models.JSONB{},
		LastValidatedAt: nil,
	}

	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if request.IsDefaultIncoming {
			if err := tx.Model(&models.ChannelAccount{}).
				Where("organization_id = ? AND is_default_incoming = ?", orgID, true).
				Update("is_default_incoming", false).Error; err != nil {
				return err
			}
		}
		if request.IsDefaultOutgoing {
			if err := tx.Model(&models.ChannelAccount{}).
				Where("organization_id = ? AND is_default_outgoing = ?", orgID, true).
				Update("is_default_outgoing", false).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&account).Error; err != nil {
			return err
		}
		credential.ChannelAccountID = account.ID
		if err := tx.Create(&credential).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			"channel_account",
			account.ID,
			models.AuditActionCreated,
			nil,
			&account,
			map[string]any{
				"field":     "credentials",
				"old_value": nil,
				"new_value": "********",
			},
		)
	})
	if err != nil {
		a.Log.Error("Failed to create channel account", "error", err, "organization_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Failed to create channel account", nil, "")
	}
	account.Credentials = []models.ChannelCredential{credential}

	return r.SendEnvelope(CreateChannelAccountResponse{
		Account:       channelAccountToResponse(&account),
		InboundSecret: inboundSecret,
		WebhookPath: fmt.Sprintf(
			"/api/webhooks/channels/%s",
			account.ID,
		),
	})
}

func (a *App) UpdateChannelAccount(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceChannelAccounts, models.ActionWrite)
	if err != nil {
		return nil
	}
	id, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}

	var request UpdateChannelAccountRequest
	if err := a.decodeRequest(r, &request); err != nil {
		return nil
	}
	if request.Config != nil && unsafeConfigKey(*request.Config) != "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Config contains a restricted credential or network-policy key", nil, "")
	}
	if request.Config != nil && hasReservedMetaRegistryMarker(*request.Config) {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Meta onboarding control markers cannot be changed through the generic channel API",
			nil,
			"",
		)
	}

	var response ChannelAccountResponse
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockChannelAIOrganizationScopeTx(tx, orgID); err != nil {
			return err
		}
		account, findErr := loadChannelAccount(tx, orgID, id, true)
		if findErr != nil {
			return findErr
		}
		if account.Provider == channelapi.LegacyMetaProvider {
			return ErrLegacyMetaAccountManaged
		}
		if account.Channel == models.ChannelThreads && strings.EqualFold(account.Provider, channelapi.ThreadsProvider) {
			return ErrThreadsAccountManaged
		}
		if managedMetaControlPlaneIntent(account) &&
			(request.Config != nil || request.Capabilities != nil || request.OutboundSecret != "") {
			return ErrMetaRegistryAccountManaged
		}
		oldAccount := *account
		// Channel account JSONB values are maps. Keep detached audit snapshots
		// so in-place delivery/AI toggles cannot mutate both old and new values.
		oldAccount.Config = cloneJSONB(account.Config)
		oldAccount.Capabilities = cloneJSONB(account.Capabilities)
		oldAccount.Metadata = cloneJSONB(account.Metadata)
		credentialChanged := false
		requiresRetest := false
		profileKeyChanged := false

		if request.Name != nil {
			name := strings.TrimSpace(*request.Name)
			if name == "" || utf8.RuneCountInString(name) > 100 {
				return errors.New("channel account name must be between 1 and 100 characters")
			}
			profileKeyChanged = account.Name != name
			account.Name = name
		}
		if request.Config != nil {
			outboundEnabled := boolConfigValue(account.Config, "outbound_enabled")
			aiReplyEnabled := boolConfigValue(account.Config, "ai_reply_enabled")
			previousRelayURL, _ := account.Config["relay_url"].(string)
			account.Config = cloneJSONB(*request.Config)
			account.Config["outbound_enabled"] = outboundEnabled
			account.Config["ai_reply_enabled"] = aiReplyEnabled
			if account.Channel == models.ChannelThreads {
				account.Config = threadsPublicEngagementConfig(account.Config)
			}
			if account.Provider == channelapi.RelayProvider {
				if err := validateRelayAccountConfig(account.Config); err != nil {
					return err
				}
				nextRelayURL, _ := account.Config["relay_url"].(string)
				requiresRetest = strings.TrimSpace(previousRelayURL) != strings.TrimSpace(nextRelayURL)
			}
		}
		if request.Capabilities != nil {
			account.Capabilities = cloneJSONB(*request.Capabilities)
			if account.Channel == models.ChannelThreads {
				account.Capabilities = threadsPublicEngagementCapabilities(account.Capabilities)
			}
		}
		managedInstagramEnableRequested := managedInstagramControlPlaneIntent(account) &&
			((request.OutboundEnabled != nil && *request.OutboundEnabled) ||
				(request.AIReplyEnabled != nil && *request.AIReplyEnabled))
		if managedInstagramEnableRequested &&
			a.metaInstagramCurrentRuntimeBindingReason(account, orgID, time.Now().UTC()) != "" {
			return ErrMetaRegistryAccountManaged
		}
		if request.OutboundEnabled != nil {
			if *request.OutboundEnabled && account.Status != models.ChannelAccountStatusActive {
				return errors.New("test and activate the channel account before enabling outbound delivery")
			}
			if *request.OutboundEnabled && managedMetaControlPlaneIntent(account) {
				if err := requireFreshMetaRegistryHealthApproval(account, time.Now().UTC()); err != nil {
					return err
				}
			}
			account.Config["outbound_enabled"] = *request.OutboundEnabled
		}
		if request.AIReplyEnabled != nil && *request.AIReplyEnabled &&
			managedMetaControlPlaneIntent(account) {
			if err := requireFreshMetaRegistryHealthApproval(account, time.Now().UTC()); err != nil {
				return err
			}
		}
		if err := applyChannelAIReplyOptIn(account, request.AIReplyEnabled); err != nil {
			return err
		}
		if request.IsDefaultIncoming != nil {
			if *request.IsDefaultIncoming {
				if err := tx.Model(&models.ChannelAccount{}).
					Where("organization_id = ? AND id <> ? AND is_default_incoming = ?", orgID, id, true).
					Update("is_default_incoming", false).Error; err != nil {
					return err
				}
			}
			account.IsDefaultIncoming = *request.IsDefaultIncoming
		}
		if request.IsDefaultOutgoing != nil {
			if account.Channel == models.ChannelThreads && *request.IsDefaultOutgoing {
				return errors.New("threads public engagement cannot be a default outbound channel because direct messages are not supported")
			}
			if *request.IsDefaultOutgoing {
				if err := tx.Model(&models.ChannelAccount{}).
					Where("organization_id = ? AND id <> ? AND is_default_outgoing = ?", orgID, id, true).
					Update("is_default_outgoing", false).Error; err != nil {
					return err
				}
			}
			account.IsDefaultOutgoing = *request.IsDefaultOutgoing
		}
		if account.Channel == models.ChannelThreads {
			account.Config = threadsPublicEngagementConfig(account.Config)
			account.Capabilities = threadsPublicEngagementCapabilities(account.Capabilities)
			account.IsDefaultOutgoing = false
		}
		if request.OutboundSecret != "" {
			if a.Config == nil || strings.TrimSpace(a.Config.App.EncryptionKey) == "" {
				return errors.New("channel credential encryption is not configured")
			}
			encrypted, encryptErr := appcrypto.Encrypt(request.OutboundSecret, a.Config.App.EncryptionKey)
			if encryptErr != nil {
				return encryptErr
			}
			credential := currentChannelCredential(account)
			if credential == nil {
				return errors.New("channel credential is missing")
			}
			if credential.CredentialBlob == nil {
				credential.CredentialBlob = models.JSONB{}
			}
			credential.CredentialBlob["outbound_secret"] = encrypted
			if err := tx.Model(&models.ChannelCredential{}).
				Where("id = ? AND organization_id = ? AND channel_account_id = ?", credential.ID, orgID, account.ID).
				Update("credential_blob", credential.CredentialBlob).Error; err != nil {
				return err
			}
			credentialChanged = true
			requiresRetest = true
		}
		if requiresRetest {
			disableChannelDeliveryForRetest(account)
			account.Status = models.ChannelAccountStatusPending
			account.LastHealthCheckAt = nil
			account.LastError = ""
			account.LastErrorAt = nil
		}
		cancelAIJobs := requiresRetest || profileKeyChanged ||
			(request.AIReplyEnabled != nil && !*request.AIReplyEnabled) ||
			(request.OutboundEnabled != nil && !*request.OutboundEnabled)
		if cancelAIJobs {
			cancelReason := "channel_ai_disabled"
			if requiresRetest {
				cancelReason = "channel_account_retest_required"
			} else if profileKeyChanged {
				cancelReason = "channel_account_profile_changed"
			} else if request.OutboundEnabled != nil && !*request.OutboundEnabled {
				cancelReason = "channel_account_outbound_disabled"
			}
			cancel := cancelChannelAIReplyJobsForAccountTx
			if managedMetaControlPlaneIntent(account) &&
				request.OutboundEnabled != nil && !*request.OutboundEnabled {
				cancel = cancelManagedMetaQueuedWorkForAccountTx
			}
			if err := cancel(tx, orgID, account.ID, cancelReason); err != nil {
				return err
			}
		}

		account.UpdatedByID = &userID
		now := time.Now().UTC()
		update := tx.Model(&models.ChannelAccount{}).
			Where("id = ? AND organization_id = ?", account.ID, orgID).
			Updates(map[string]any{
				"name":                 account.Name,
				"status":               account.Status,
				"capabilities":         account.Capabilities,
				"config":               account.Config,
				"is_default_incoming":  account.IsDefaultIncoming,
				"is_default_outgoing":  account.IsDefaultOutgoing,
				"last_health_check_at": account.LastHealthCheckAt,
				"last_error":           account.LastError,
				"last_error_at":        account.LastErrorAt,
				"updated_by_id":        userID,
				"updated_at":           now,
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		account.UpdatedAt = now
		var sensitiveChanges []map[string]any
		if credentialChanged {
			sensitiveChanges = append(sensitiveChanges, map[string]any{
				"field":     "outbound_secret",
				"old_value": "********",
				"new_value": "********",
			})
		}
		if err := audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			"channel_account",
			account.ID,
			models.AuditActionUpdated,
			&oldAccount,
			account,
			sensitiveChanges...,
		); err != nil {
			return err
		}
		response = channelAccountToResponse(account)
		return nil
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel account not found", nil, "")
		}
		if errors.Is(err, ErrLegacyMetaAccountManaged) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, err.Error(), nil, "")
		}
		if errors.Is(err, ErrThreadsAccountManaged) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, err.Error(), nil, "")
		}
		if errors.Is(err, ErrMetaRegistryAccountManaged) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, err.Error(), nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	return r.SendEnvelope(response)
}

func disableChannelDeliveryForRetest(account *models.ChannelAccount) {
	if account == nil {
		return
	}
	if account.Config == nil {
		account.Config = models.JSONB{}
	}
	account.Config["outbound_enabled"] = false
	// Changing relay routing or credentials invalidates the previous explicit
	// AI approval too. A successful retest and outbound approval must not
	// silently reactivate automatic replies without a fresh opt-in.
	account.Config["ai_reply_enabled"] = false
}

func quarantineManagedInstagramHealthPreflightTx(
	tx *gorm.DB,
	organizationID uuid.UUID,
	account *models.ChannelAccount,
	reason string,
	now time.Time,
) error {
	if tx == nil || account == nil || strings.TrimSpace(reason) == "" {
		return errors.New("managed Instagram health quarantine requires an exact account and reason")
	}
	if account.Status == models.ChannelAccountStatusDisconnected ||
		metaInstagramDeletionReconciliationPending(account.Metadata) ||
		metaDeauthorizationReconciliationPending(account.Metadata) ||
		stringConfigValue(
			account.Metadata, metaMessengerSubscriptionDesiredStateKey,
		) == metaMessengerSubscriptionDesiredUnsubscribed {
		// Callback/disconnect state already owns its exact lifecycle metadata and
		// has canceled queued work. Health must not overwrite that evidence.
		return nil
	}
	checkedAt := now.UTC()
	if previous, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(account.Metadata, "meta_ownership_checked_at"),
	); err == nil && !checkedAt.After(previous) {
		checkedAt = previous.Add(time.Nanosecond)
	}
	metadata := cloneJSONB(account.Metadata)
	metadata["meta_ownership_state"] = metaregistry.OwnershipStale
	metadata["meta_ownership_checked_at"] = checkedAt.Format(time.RFC3339Nano)
	metadata["meta_ownership_reason"] = reason
	metadata["meta_activation_state"] = "quarantined"
	for _, key := range []string{
		"meta_health_checked_at", "meta_health_oauth_credential_id",
		"meta_health_oauth_version", "meta_health_webhook_credential_id",
		"meta_health_webhook_version",
	} {
		delete(metadata, key)
	}
	config := cloneJSONB(account.Config)
	config["outbound_enabled"] = false
	config["ai_reply_enabled"] = false
	result := tx.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?", account.ID, organizationID,
	).Updates(map[string]any{
		"status": models.ChannelAccountStatusDegraded,
		"config": config, "metadata": metadata,
		"last_health_check_at": nil,
		"last_error":           "Managed Instagram health validation is fenced",
		"last_error_at":        checkedAt,
		"updated_at":           checkedAt,
	})
	if result.Error != nil || result.RowsAffected != 1 {
		if result.Error != nil {
			return result.Error
		}
		return gorm.ErrRecordNotFound
	}
	account.Status = models.ChannelAccountStatusDegraded
	account.Config = config
	account.Metadata = metadata
	return cancelManagedMetaQueuedWorkForAccountTx(
		tx, organizationID, account.ID, "managed_instagram_health_preflight_fenced",
	)
}

func applyChannelAIReplyOptIn(
	account *models.ChannelAccount,
	enabled *bool,
) error {
	if enabled == nil {
		return nil
	}
	if account == nil {
		return errors.New("channel account is required")
	}
	if account.Channel != models.ChannelInstagram &&
		account.Channel != models.ChannelMessenger {
		return errors.New("automatic AI replies are supported only for Instagram and Messenger")
	}
	if *enabled &&
		(account.Status != models.ChannelAccountStatusActive ||
			!boolConfigValue(account.Config, "outbound_enabled")) {
		return errors.New("activate and approve outbound delivery before enabling automatic AI replies")
	}
	if account.Config == nil {
		account.Config = models.JSONB{}
	}
	account.Config["ai_reply_enabled"] = *enabled
	return nil
}

func (a *App) TestChannelAccount(r *fastglue.Request) error {
	orgID, userID, err := a.requireChannelAccountTestAuth(r)
	if err != nil {
		return nil
	}
	id, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}

	// Reserve a per-account validation generation before making the network
	// call. Only the newest reservation may apply a result, so a slow older
	// success cannot overwrite a newer failure (or vice versa).
	validationToken := uuid.NewString()
	var account *models.ChannelAccount
	var adapter channelapi.Adapter
	var adapterErr error
	var managedInstagramPreflightReason string
	var managedInstagramValidationResult channelapi.AccountValidationResult
	var managedInstagramValidationErr error
	managedInstagramValidationPerformed := false
	if err := database.WithTenantReadCommitted(
		a.rootApp().DB,
		orgID,
		func(tx *gorm.DB) error {
			if err := lockChannelAIOrganizationScopeTx(tx, orgID); err != nil {
				return err
			}
			currentAccount, err := loadChannelAccount(tx, orgID, id, true)
			if err != nil {
				return err
			}
			account = currentAccount
			if account.Provider == channelapi.LegacyMetaProvider {
				return nil
			}
			managedInstagram := managedInstagramControlPlaneIntent(account)
			if account.Channel == models.ChannelThreads {
				if bindingErr := validateThreadsWebhookAccountBinding(
					a.Config,
					tx,
					account,
					stringConfigValue(account.Metadata, "app_id"),
				); bindingErr != nil {
					adapterErr = bindingErr
					return nil
				}
			}
			if managedInstagram {
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
					"id = ? AND organization_id = ?", account.ID, orgID,
				).First(account).Error; err != nil {
					return err
				}
				managedInstagramPreflightReason = a.metaInstagramCurrentRuntimeBindingReason(
					account, orgID, time.Now().UTC(),
				)
				if managedInstagramPreflightReason != "" {
					return quarantineManagedInstagramHealthPreflightTx(
						tx, orgID, account, managedInstagramPreflightReason, time.Now().UTC(),
					)
				}
			}
			adapter, adapterErr = a.channelAdapter(account)
			if adapterErr != nil {
				return nil
			}
			metadata := cloneJSONB(account.Metadata)
			metadata[channelAccountHealthValidationTokenKey] = validationToken
			reserved := tx.Model(&models.ChannelAccount{}).
				Where("id = ? AND organization_id = ?", account.ID, orgID).
				UpdateColumn("metadata", metadata)
			if reserved.Error != nil {
				return reserved.Error
			}
			if reserved.RowsAffected != 1 {
				return gorm.ErrRecordNotFound
			}
			account.Metadata = metadata
			if managedInstagram {
				// The organization mutex remains held through the signed relay health
				// request. Whichever of health or a control-plane downgrade owns it
				// first determines whether the request runs or is fenced.
				managedInstagramValidationResult, managedInstagramValidationErr =
					adapter.ValidateAccount(requestContext(r), account)
				managedInstagramValidationPerformed = true
			}
			return nil
		},
	); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel account not found", nil, "")
		}
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to prepare channel account validation",
			nil,
			"",
		)
	}
	if managedInstagramPreflightReason != "" {
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"Managed Instagram health validation is fenced by the current binding",
			nil, "",
		)
	}
	if account.Provider == channelapi.LegacyMetaProvider {
		return r.SendEnvelope(map[string]any{
			"success":    true,
			"status":     account.Status,
			"managed_by": "whatsapp_setup",
			"warnings": []string{
				"Delivery and credentials remain managed by the established WhatsApp account",
			},
		})
	}
	if adapterErr != nil {
		return r.SendEnvelope(map[string]any{
			"success": false,
			"status":  account.Status,
			"error":   "No approved adapter is available for this provider",
		})
	}

	validatedFingerprint, err := channelAccountValidationFingerprint(account)
	if err != nil {
		a.Log.Error("Failed to fingerprint channel account validation input", "error", err)
		return r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to prepare channel account validation",
			nil,
			"",
		)
	}
	validatedStatus := account.Status
	result, validationErr := managedInstagramValidationResult, managedInstagramValidationErr
	if !managedInstagramValidationPerformed {
		result, validationErr = adapter.ValidateAccount(requestContext(r), account)
	}
	now := time.Now().UTC()
	var oldAccount models.ChannelAccount

	if err := database.WithTenantReadCommitted(
		a.rootApp().DB,
		orgID,
		func(tx *gorm.DB) error {
			if err := lockChannelAIOrganizationScopeTx(tx, orgID); err != nil {
				return err
			}
			currentAccount, err := loadChannelAccount(tx, orgID, account.ID, true)
			if err != nil {
				return err
			}
			if currentAccount.Channel == models.ChannelThreads {
				if err := validateThreadsWebhookAccountBinding(
					a.Config,
					tx,
					currentAccount,
					stringConfigValue(currentAccount.Metadata, "app_id"),
				); err != nil {
					return ErrChannelAccountChangedDuringValidation
				}
			}
			if !channelAccountValidationTokenMatches(
				currentAccount,
				validationToken,
			) {
				return ErrChannelAccountValidationSuperseded
			}
			currentFingerprint, err := channelAccountValidationFingerprint(currentAccount)
			if err != nil {
				return err
			}
			if currentFingerprint != validatedFingerprint {
				return ErrChannelAccountChangedDuringValidation
			}
			managedInstagramBindingDriftReason := ""
			managedInstagram := managedInstagramControlPlaneIntent(currentAccount)
			if managedInstagram &&
				(currentAccount.Status == models.ChannelAccountStatusDisconnected ||
					metaInstagramDeletionReconciliationPending(currentAccount.Metadata) ||
					metaDeauthorizationReconciliationPending(currentAccount.Metadata) ||
					stringConfigValue(
						currentAccount.Metadata, metaMessengerSubscriptionDesiredStateKey,
					) == metaMessengerSubscriptionDesiredUnsubscribed) {
				// A disconnect or signed callback that acquired the organization
				// mutex owns the final state. Never let an older provider health
				// response overwrite its status, credentials, or reconciliation
				// evidence.
				return ErrChannelAccountValidationSuperseded
			}
			if validationErr == nil && result.Valid && managedInstagram {
				if currentAccount.Status != validatedStatus {
					managedInstagramBindingDriftReason = "managed_instagram_status_changed"
				} else {
					managedInstagramBindingDriftReason = a.metaInstagramCurrentRuntimeBindingReason(
						currentAccount, orgID, now,
					)
				}
				if managedInstagramBindingDriftReason != "" {
					validationErr = errors.New("managed Instagram binding changed during validation")
				}
			}
			oldAccount = *currentAccount
			oldAccount.Config = cloneJSONB(currentAccount.Config)
			oldAccount.Capabilities = cloneJSONB(currentAccount.Capabilities)
			oldAccount.Metadata = cloneJSONB(currentAccount.Metadata)
			currentAccount.LastHealthCheckAt = &now
			managedMetaHealthFailure := false
			if validationErr != nil || !result.Valid {
				currentAccount.LastErrorAt = &now
				if validationErr != nil {
					currentAccount.LastError = validationErr.Error()
				} else {
					currentAccount.LastError = "Provider rejected the channel account"
				}
				if currentAccount.Status == models.ChannelAccountStatusActive ||
					(managedInstagramBindingDriftReason != "" &&
						currentAccount.Status != models.ChannelAccountStatusDisconnected) {
					currentAccount.Status = models.ChannelAccountStatusDegraded
				}
				if managedMetaControlPlaneIntent(currentAccount) {
					managedMetaHealthFailure = true
					currentAccount.Config = cloneJSONB(currentAccount.Config)
					currentAccount.Config["outbound_enabled"] = false
					currentAccount.Config["ai_reply_enabled"] = false
				}
				if managedInstagramBindingDriftReason != "" {
					metadata := cloneJSONB(currentAccount.Metadata)
					metadata["meta_activation_state"] = "quarantined"
					metadata["meta_ownership_reason"] = managedInstagramBindingDriftReason
					for _, key := range []string{
						"meta_health_checked_at", "meta_health_oauth_credential_id",
						"meta_health_oauth_version", "meta_health_webhook_credential_id",
						"meta_health_webhook_version",
					} {
						delete(metadata, key)
					}
					currentAccount.Metadata = metadata
				}
			} else {
				managedMeta := managedMetaControlPlaneIntent(currentAccount)
				if !managedMeta {
					currentAccount.Status = models.ChannelAccountStatusActive
				}
				currentAccount.Capabilities = capabilitiesToJSONB(result.Capabilities)
				if currentAccount.Channel == models.ChannelThreads {
					currentAccount.Capabilities = threadsPublicEngagementCapabilities(
						currentAccount.Capabilities,
					)
				}
				currentAccount.LastError = ""
				currentAccount.LastErrorAt = nil
				if currentAccount.ConnectedAt == nil {
					if !managedMeta || currentAccount.Status == models.ChannelAccountStatusActive {
						currentAccount.ConnectedAt = &now
					}
				}
				if managedMeta {
					oauth := currentMetaRegistryCredential(currentAccount.Credentials, models.ChannelCredentialKindOAuth, now)
					webhook := currentMetaRegistryCredential(currentAccount.Credentials, models.ChannelCredentialKindWebhook, now)
					if oauth == nil || webhook == nil ||
						stringConfigValue(currentAccount.Metadata, "meta_ownership_state") != metaregistry.OwnershipVerified ||
						stringConfigValue(currentAccount.Metadata, "meta_subscription_state") != "verified" {
						return errors.New("managed Meta health validation lost its credential or ownership fence")
					}
					metadata := cloneJSONB(currentAccount.Metadata)
					metadata["meta_health_checked_at"] = now.Format(time.RFC3339Nano)
					metadata["meta_health_oauth_credential_id"] = oauth.ID.String()
					metadata["meta_health_oauth_version"] = oauth.Version
					metadata["meta_health_webhook_credential_id"] = webhook.ID.String()
					metadata["meta_health_webhook_version"] = webhook.Version
					if currentAccount.Status == models.ChannelAccountStatusPending {
						metadata["meta_activation_state"] = "awaiting_admin_approval"
					}
					currentAccount.Metadata = metadata
				}
			}
			currentAccount.UpdatedByID = &userID
			account = currentAccount

			// A health transition in either direction starts a fresh delivery
			// epoch. Cancel old AI work before saving the new account status so a
			// later recovery cannot resurrect an inbound message from the prior
			// state.
			if managedMetaHealthFailure {
				if err := cancelManagedMetaQueuedWorkForAccountTx(
					tx,
					orgID,
					account.ID,
					"managed_meta_health_downgraded",
				); err != nil {
					return err
				}
			} else if oldAccount.Status != account.Status {
				if err := cancelChannelAIReplyJobsForAccountTx(
					tx,
					orgID,
					account.ID,
					"channel_account_health_changed",
				); err != nil {
					return err
				}
			}
			if err := tx.Model(&models.ChannelAccount{}).
				Where("id = ? AND organization_id = ?", account.ID, orgID).
				Updates(map[string]any{
					"status":               account.Status,
					"config":               account.Config,
					"capabilities":         account.Capabilities,
					"last_health_check_at": account.LastHealthCheckAt,
					"last_error":           account.LastError,
					"last_error_at":        account.LastErrorAt,
					"connected_at":         account.ConnectedAt,
					"metadata":             account.Metadata,
					"updated_by_id":        userID,
					"updated_at":           now,
				}).Error; err != nil {
				return err
			}
			return audit.LogAudit(
				tx,
				orgID,
				userID,
				audit.GetUserName(tx, userID),
				"channel_account",
				account.ID,
				models.AuditActionUpdated,
				&oldAccount,
				account,
			)
		},
	); err != nil {
		if errors.Is(err, ErrChannelAccountValidationSuperseded) {
			return r.SendErrorEnvelope(
				fasthttp.StatusConflict,
				"A newer channel account validation superseded this result",
				nil,
				"",
			)
		}
		if errors.Is(err, ErrChannelAccountChangedDuringValidation) {
			return r.SendErrorEnvelope(
				fasthttp.StatusConflict,
				"Channel account changed during validation; run the test again",
				nil,
				"",
			)
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel account not found", nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update channel account health", nil, "")
	}

	if validationErr != nil || !result.Valid {
		return r.SendEnvelope(map[string]any{
			"success": false,
			"status":  account.Status,
			"error":   account.LastError,
		})
	}
	return r.SendEnvelope(map[string]any{
		"success":      true,
		"status":       account.Status,
		"capabilities": account.Capabilities,
		"warnings":     result.Warnings,
		"checked_at":   result.CheckedAt,
	})
}

func (a *App) requireChannelAccountTestAuth(
	r *fastglue.Request,
) (uuid.UUID, uuid.UUID, error) {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil || orgID == uuid.Nil || userID == uuid.Nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
		return uuid.Nil, uuid.Nil, errEnvelopeSent
	}

	var authErr error
	root := a.rootApp()
	if err := database.WithTenantReadCommitted(
		root.DB,
		orgID,
		func(tx *gorm.DB) error {
			scoped := root.scopedApp(tx, orgID)
			_, _, authErr = scoped.requireAuth(
				r,
				models.ResourceChannelAccounts,
				models.ActionWrite,
			)
			// requireAuth already sends the specific authorization or
			// entitlement response. Keep this short read transaction separate
			// from the provider network call.
			return nil
		},
	); err != nil {
		a.Log.Error(
			"Failed to authorize channel account validation",
			"error",
			err,
			"organization_id",
			orgID,
		)
		_ = r.SendErrorEnvelope(
			fasthttp.StatusInternalServerError,
			"Failed to authorize channel account validation",
			nil,
			"",
		)
		return uuid.Nil, uuid.Nil, errEnvelopeSent
	}
	if authErr != nil {
		return uuid.Nil, uuid.Nil, authErr
	}
	return orgID, userID, nil
}

func (a *App) DeleteChannelAccount(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceChannelAccounts, models.ActionDelete)
	if err != nil {
		return nil
	}
	id, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}

	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockChannelAIOrganizationScopeTx(tx, orgID); err != nil {
			return err
		}
		account, findErr := loadChannelAccount(tx, orgID, id, false)
		if findErr != nil {
			return findErr
		}
		if account.Provider == channelapi.LegacyMetaProvider {
			return ErrLegacyMetaAccountManaged
		}
		if account.Channel == models.ChannelThreads && strings.EqualFold(account.Provider, channelapi.ThreadsProvider) {
			return ErrThreadsAccountManaged
		}
		if managedMetaControlPlaneIntent(account) {
			return ErrMetaRegistryAccountManaged
		}
		if err := cancelChannelAIReplyJobsForAccountTx(
			tx,
			orgID,
			account.ID,
			"channel_account_deleted",
		); err != nil {
			return err
		}
		deleted := tx.
			Where("id = ? AND organization_id = ?", id, orgID).
			Delete(&models.ChannelAccount{})
		if deleted.Error != nil {
			return deleted.Error
		}
		if deleted.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			"channel_account",
			account.ID,
			models.AuditActionDeleted,
			account,
			nil,
		)
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel account not found", nil, "")
		}
		if errors.Is(err, ErrLegacyMetaAccountManaged) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, err.Error(), nil, "")
		}
		if errors.Is(err, ErrThreadsAccountManaged) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, err.Error(), nil, "")
		}
		if errors.Is(err, ErrMetaRegistryAccountManaged) {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, err.Error(), nil, "")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete channel account", nil, "")
	}
	return r.SendEnvelope(map[string]string{"message": "Channel account deleted successfully"})
}

func (a *App) channelAdapter(account *models.ChannelAccount) (channelapi.Adapter, error) {
	if account == nil {
		return nil, ErrChannelAdapterUnavailable
	}
	if a.channelAdapterFactory != nil {
		return a.channelAdapterFactory(account)
	}
	if account.Channel == models.ChannelThreads {
		if !strings.EqualFold(account.Provider, channelapi.ThreadsProvider) {
			return nil, fmt.Errorf("%w: Threads account is not OAuth-managed", ErrChannelAdapterUnavailable)
		}
		appSecret, verifyToken, err := a.threadsWebhookCredentials(
			account.OrganizationID,
			account.ExternalAccountID,
		)
		if err != nil {
			return nil, fmt.Errorf("%w: Threads app credentials are unavailable", ErrChannelAdapterUnavailable)
		}
		encryptionKey := ""
		if a.Config != nil {
			encryptionKey = a.Config.App.EncryptionKey
		}
		return channelapi.NewThreadsWebhookAdapter(a.HTTPClient, encryptionKey, appSecret, verifyToken), nil
	}
	if account.Channel == models.ChannelTikTok {
		return nil, fmt.Errorf("%w: TikTok connections are not yet approved", ErrChannelAdapterUnavailable)
	}
	if strings.EqualFold(account.Provider, channelapi.RelayProvider) {
		encryptionKey := ""
		if a.Config != nil {
			encryptionKey = a.Config.App.EncryptionKey
		}
		edgeSecret := ""
		if a.Config != nil {
			edgeSecret = a.Config.MetaRegistry.RelayEdgeSecret
		}
		return channelapi.NewRelayAdapter(account.Channel, a.HTTPClient, encryptionKey).
			WithServiceToken(edgeSecret), nil
	}
	return nil, fmt.Errorf("%w: channel=%s provider=%s", ErrChannelAdapterUnavailable, account.Channel, account.Provider)
}

func validChannelIdentifier(channel models.Channel) bool {
	switch channel {
	case models.ChannelWhatsApp,
		models.ChannelInstagram,
		models.ChannelMessenger,
		models.ChannelThreads,
		models.ChannelWebChat,
		models.ChannelEmail,
		models.ChannelSMS,
		models.ChannelTelegram,
		models.ChannelTikTok:
		return true
	default:
		return false
	}
}

func additionalChannelCreationEntitlement(channel models.Channel) (string, bool) {
	if channel == models.ChannelThreads {
		return channelapi.ThreadsPublicEngagementEntitlementKey, true
	}
	return "", false
}

func validateChannelCreationPolicy(request ChannelAccountRequest) error {
	switch request.Channel {
	case models.ChannelMessenger:
		return errors.New("messenger accounts must be connected through managed Meta onboarding and cannot be created manually")
	case models.ChannelThreads:
		return errors.New("threads accounts are OAuth-managed from Settings > Integrations and cannot be created manually")
	case models.ChannelTikTok:
		return errors.New("tiktok is preparation-only until an approved messaging adapter is installed; channel accounts cannot be created yet")
	default:
		return nil
	}
}

func threadsPublicEngagementConfig(config models.JSONB) models.JSONB {
	config = cloneJSONB(config)
	config["engagement_mode"] = threadsPublicEngagementMode
	config["direct_messages_supported"] = false
	config["beta"] = false
	config["activation_available"] = true
	return config
}

func threadsPublicEngagementCapabilities(capabilities models.JSONB) models.JSONB {
	capabilities = cloneJSONB(capabilities)
	capabilities["business_initiation"] = false
	capabilities["direct_messages"] = false
	capabilities["public_replies"] = true
	capabilities["mentions"] = true
	capabilities["reply_target_required"] = true
	return capabilities
}

func loadChannelAccount(db *gorm.DB, orgID, accountID uuid.UUID, credentials bool) (*models.ChannelAccount, error) {
	query := db
	if credentials {
		now := time.Now().UTC()
		query = query.Preload("Credentials", func(credentials *gorm.DB) *gorm.DB {
			return credentials.
				Where(
					"organization_id = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
					orgID,
					[]models.ChannelCredentialStatus{
						models.ChannelCredentialStatusActive,
						models.ChannelCredentialStatusExpiring,
					},
					now,
				).
				Order("version DESC").
				Order("id ASC")
		})
	}
	var account models.ChannelAccount
	if err := query.
		Where("id = ? AND organization_id = ?", accountID, orgID).
		First(&account).Error; err != nil {
		return nil, err
	}
	return &account, nil
}

func channelAccountToResponse(account *models.ChannelAccount) ChannelAccountResponse {
	return ChannelAccountResponse{
		ID:                                     account.ID,
		OrganizationID:                         account.OrganizationID,
		Channel:                                account.Channel,
		Provider:                               account.Provider,
		Name:                                   account.Name,
		ExternalAccountID:                      account.ExternalAccountID,
		Status:                                 account.Status,
		Capabilities:                           cloneJSONB(account.Capabilities),
		Config:                                 sanitizeChannelConfig(account.Config),
		IsDefaultIncoming:                      account.IsDefaultIncoming,
		IsDefaultOutgoing:                      account.IsDefaultOutgoing,
		HasCredentials:                         currentChannelCredential(account) != nil,
		MetaSubscriptionReconciliationRequired: metaMessengerSubscriptionReconciliationRequired(account),
		MetaSubscriptionReconciliationDesired:  metaSubscriptionReconciliationDesiredState(account),
		ConnectedAt:                            account.ConnectedAt,
		LastHealthCheckAt:                      account.LastHealthCheckAt,
		LastInboundAt:                          account.LastInboundAt,
		LastOutboundAt:                         account.LastOutboundAt,
		LastErrorAt:                            account.LastErrorAt,
		LastError:                              account.LastError,
		CreatedByID:                            account.CreatedByID,
		UpdatedByID:                            account.UpdatedByID,
		CreatedAt:                              account.CreatedAt,
		UpdatedAt:                              account.UpdatedAt,
	}
}

func metaMessengerSubscriptionReconciliationRequired(account *models.ChannelAccount) bool {
	if account == nil ||
		(account.Channel != models.ChannelMessenger && account.Channel != models.ChannelInstagram) ||
		!managedMetaControlPlaneIntent(account) {
		return false
	}
	if account.Channel == models.ChannelInstagram &&
		stringConfigValue(account.Config, "instagram_api_mode") != "instagram_login" {
		return false
	}
	if stringConfigValue(account.Metadata, metaMessengerSubscriptionFencedOperationIDKey) != "" {
		return true
	}
	switch stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationStateKey) {
	case metaMessengerSubscriptionSubscribePending,
		metaMessengerSubscriptionSubscribeFailed,
		metaMessengerSubscriptionUnsubscribePending,
		metaMessengerSubscriptionUnsubscribeFailed:
		return true
	default:
		return false
	}
}

func metaSubscriptionReconciliationDesiredState(account *models.ChannelAccount) string {
	if !metaMessengerSubscriptionReconciliationRequired(account) {
		return ""
	}
	switch desired := stringConfigValue(account.Metadata, metaMessengerSubscriptionDesiredStateKey); desired {
	case metaMessengerSubscriptionDesiredSubscribed, metaMessengerSubscriptionDesiredUnsubscribed:
		return desired
	default:
		return ""
	}
}

func generateChannelSecret() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}

func currentChannelCredential(account *models.ChannelAccount) *models.ChannelCredential {
	if account == nil {
		return nil
	}
	now := time.Now().UTC()
	for _, index := range channelapi.CredentialIndexesByPriority(account.Credentials) {
		credential := &account.Credentials[index]
		if channelapi.CredentialIsCurrent(credential, now) {
			return credential
		}
	}
	return nil
}

func cloneJSONB(value models.JSONB) models.JSONB {
	if value == nil {
		return models.JSONB{}
	}
	cloned := make(models.JSONB, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

// channelAccountValidationFingerprint hashes only provider-adapter inputs. It
// deliberately excludes operational timestamps so normal inbound/outbound
// activity does not invalidate an in-flight health check.
func channelAccountValidationFingerprint(
	account *models.ChannelAccount,
) ([sha256.Size]byte, error) {
	type credentialInput struct {
		ID             uuid.UUID                      `json:"id"`
		Kind           models.ChannelCredentialKind   `json:"kind"`
		Version        int                            `json:"version"`
		CredentialBlob models.JSONB                   `json:"credential_blob"`
		Status         models.ChannelCredentialStatus `json:"status"`
		KeyVersion     string                         `json:"key_version"`
		ExpiresAt      *time.Time                     `json:"expires_at,omitempty"`
	}
	type validationInput struct {
		Channel           models.Channel    `json:"channel"`
		Provider          string            `json:"provider"`
		ExternalAccountID string            `json:"external_account_id"`
		Config            models.JSONB      `json:"config"`
		Capabilities      models.JSONB      `json:"capabilities"`
		Credentials       []credentialInput `json:"credentials"`
	}

	var zero [sha256.Size]byte
	if account == nil {
		return zero, errors.New("channel account is required")
	}

	credentials := make([]credentialInput, 0, len(account.Credentials))
	for _, index := range channelapi.CredentialIndexesByPriority(account.Credentials) {
		credential := &account.Credentials[index]
		credentials = append(credentials, credentialInput{
			ID:             credential.ID,
			Kind:           credential.Kind,
			Version:        credential.Version,
			CredentialBlob: cloneJSONB(credential.CredentialBlob),
			Status:         credential.Status,
			KeyVersion:     credential.KeyVersion,
			ExpiresAt:      credential.ExpiresAt,
		})
	}
	encoded, err := json.Marshal(validationInput{
		Channel:           account.Channel,
		Provider:          account.Provider,
		ExternalAccountID: account.ExternalAccountID,
		Config:            cloneJSONB(account.Config),
		Capabilities:      cloneJSONB(account.Capabilities),
		Credentials:       credentials,
	})
	if err != nil {
		return zero, fmt.Errorf("encode channel account validation input: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

func channelAccountValidationTokenMatches(
	account *models.ChannelAccount,
	validationToken string,
) bool {
	if account == nil || strings.TrimSpace(validationToken) == "" {
		return false
	}
	return stringConfigValue(
		account.Metadata,
		channelAccountHealthValidationTokenKey,
	) == validationToken
}

func unsafeConfigKey(config models.JSONB) string {
	return unsafeNestedConfigKey(map[string]any(config), "")
}

func unsafeNestedConfigKey(config map[string]any, prefix string) string {
	for key, value := range config {
		normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if normalized == "allow_localhost_dev" {
			return path
		}
		for _, fragment := range []string{"secret", "password", "token", "api_key", "authorization", "credential"} {
			if strings.Contains(normalized, fragment) {
				return path
			}
		}
		switch nested := value.(type) {
		case map[string]any:
			if unsafe := unsafeNestedConfigKey(nested, path); unsafe != "" {
				return unsafe
			}
		case models.JSONB:
			if unsafe := unsafeNestedConfigKey(map[string]any(nested), path); unsafe != "" {
				return unsafe
			}
		case []any:
			for index, item := range nested {
				if object, ok := item.(map[string]any); ok {
					if unsafe := unsafeNestedConfigKey(object, fmt.Sprintf("%s[%d]", path, index)); unsafe != "" {
						return unsafe
					}
				}
			}
		}
	}
	return ""
}

func sanitizeChannelConfig(config models.JSONB) models.JSONB {
	return models.JSONB(sanitizeNestedChannelConfig(map[string]any(config)))
}

func sanitizeNestedChannelConfig(config map[string]any) map[string]any {
	sanitized := make(map[string]any, len(config))
	for key, value := range config {
		normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
		if normalized == "allow_localhost_dev" {
			continue
		}
		restricted := false
		for _, fragment := range []string{"secret", "password", "token", "api_key", "authorization", "credential"} {
			if strings.Contains(normalized, fragment) {
				restricted = true
				break
			}
		}
		if restricted {
			continue
		}
		switch nested := value.(type) {
		case map[string]any:
			sanitized[key] = sanitizeNestedChannelConfig(nested)
		case models.JSONB:
			sanitized[key] = sanitizeNestedChannelConfig(map[string]any(nested))
		case []any:
			items := make([]any, len(nested))
			for i, item := range nested {
				if object, ok := item.(map[string]any); ok {
					items[i] = sanitizeNestedChannelConfig(object)
				} else {
					items[i] = item
				}
			}
			sanitized[key] = items
		default:
			sanitized[key] = value
		}
	}
	return sanitized
}

func validateRelayAccountConfig(config models.JSONB) error {
	relayURL := stringConfigValue(config, "relay_url")
	if relayURL == "" {
		return errors.New("config.relay_url is required for relay accounts")
	}
	if err := channelapi.ValidateRelayEndpoint(relayURL); err != nil {
		return errors.New("config.relay_url must be a public HTTPS URL without credentials or query parameters")
	}
	if healthURL := stringConfigValue(config, "health_url"); healthURL != "" {
		if err := channelapi.ValidateRelayEndpoint(healthURL); err != nil {
			return errors.New("config.health_url must be a public HTTPS URL without credentials or query parameters")
		}
	}
	return nil
}

func stringConfigValue(config models.JSONB, key string) string {
	value, _ := config[key].(string)
	return strings.TrimSpace(value)
}

func boolConfigValue(config models.JSONB, key string) bool {
	value, _ := config[key].(bool)
	return value
}

func metaRegistryControlPlaneConfig(config models.JSONB) bool {
	return boolConfigValue(config, "meta_registry_managed") ||
		stringConfigValue(config, "meta_management_mode") == metaregistry.ManagementModePlatformOAuth
}

// metaRegistryControlPlaneConfig detects managed-control-plane intent using
// either reserved marker so partial rollback/seeded rows are never mistaken
// for static relay accounts. Exact runtime bindings must require both markers.
func exactMetaRegistryControlPlaneConfig(config models.JSONB) bool {
	return boolConfigValue(config, "meta_registry_managed") &&
		stringConfigValue(config, "meta_management_mode") == metaregistry.ManagementModePlatformOAuth
}

// managedInstagramControlPlaneIntent is deliberately broader than an exact
// authorization binding. Either reserved marker identifies managed intent;
// server-owned Instagram Login metadata, operation, or URL residue also
// identifies a rollback row after an older editor strips both markers. Exact
// provider calls still require both markers.
func managedInstagramControlPlaneIntent(account *models.ChannelAccount) bool {
	if account == nil || account.Channel != models.ChannelInstagram ||
		account.Provider != channelapi.RelayProvider {
		return false
	}
	if metaRegistryControlPlaneConfig(account.Config) {
		return true
	}
	platformAppID := stringConfigValue(account.Metadata, "meta_platform_app_id")
	if !validCanonicalMetaID(platformAppID) || !validCanonicalMetaID(account.ExternalAccountID) {
		return false
	}
	instagramIdentityResidue := stringConfigValue(account.Config, "instagram_api_mode") == "instagram_login" &&
		stringConfigValue(account.Metadata, "meta_webhook_app") == "instagram_login"
	subscriptionResidue := stringConfigValue(
		account.Metadata, metaMessengerSubscriptionOperationIDKey,
	) != ""
	webhookSuffix := "/api/webhooks/channels/" + account.ID.String()
	relaySuffix := "/v1/accounts/instagram/" + account.ExternalAccountID
	managedURLResidue := strings.HasSuffix(
		stringConfigValue(account.Config, "rereply_webhook_url"), webhookSuffix,
	) && strings.HasSuffix(stringConfigValue(account.Config, "relay_url"), relaySuffix)
	return instagramIdentityResidue || subscriptionResidue || managedURLResidue
}

func managedMetaControlPlaneIntent(account *models.ChannelAccount) bool {
	if account == nil {
		return false
	}
	if account.Channel == models.ChannelInstagram {
		return managedInstagramControlPlaneIntent(account)
	}
	return account.Channel == models.ChannelMessenger &&
		account.Provider == channelapi.RelayProvider &&
		metaRegistryControlPlaneConfig(account.Config)
}

func hasReservedMetaRegistryMarker(config models.JSONB) bool {
	for key := range config {
		normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
		if strings.HasPrefix(normalized, "meta_registry_") ||
			strings.HasPrefix(normalized, "meta_management_") ||
			strings.HasPrefix(normalized, "meta_activation_") {
			return true
		}
	}
	return false
}

func requireFreshMetaRegistryHealthApproval(account *models.ChannelAccount, now time.Time) error {
	if account == nil || account.Status != models.ChannelAccountStatusActive ||
		account.LastHealthCheckAt == nil || strings.TrimSpace(account.LastError) != "" ||
		stringConfigValue(account.Metadata, "meta_ownership_state") != metaregistry.OwnershipVerified {
		return errors.New("run a successful Meta relay health test before approving outbound delivery")
	}
	checkedAt := account.LastHealthCheckAt.UTC()
	if checkedAt.After(now.Add(time.Minute)) || now.Sub(checkedAt) > metaRegistryHealthApprovalMaxAge {
		return errors.New("the successful Meta relay health test is no longer fresh; run it again before approving outbound delivery")
	}
	return nil
}

func capabilitiesToJSONB(capabilities channelapi.Capabilities) models.JSONB {
	return models.JSONB{
		string(channelapi.CapabilityText):                capabilities.Text,
		string(channelapi.CapabilityMedia):               capabilities.Media,
		string(channelapi.CapabilityMultipleAttachments): capabilities.MultipleAttachments,
		string(channelapi.CapabilityReplies):             capabilities.Replies,
		string(channelapi.CapabilityReactions):           capabilities.Reactions,
		string(channelapi.CapabilityReadReceipts):        capabilities.ReadReceipts,
		string(channelapi.CapabilityButtons):             capabilities.Buttons,
		string(channelapi.CapabilityTemplates):           capabilities.Templates,
		string(channelapi.CapabilityBusinessInitiation):  capabilities.BusinessInitiation,
		string(channelapi.CapabilityServiceWindow):       capabilities.ServiceWindow,
		string(channelapi.CapabilityTyping):              capabilities.Typing,
		string(channelapi.CapabilitySubjectAndCC):        capabilities.SubjectAndCC,
		"max_attachments":                                capabilities.MaxAttachments,
		"max_media_bytes":                                capabilities.MaxMediaBytes,
		"supported_media_types":                          capabilities.SupportedMediaTypes,
	}
}

func requestContext(r *fastglue.Request) context.Context {
	if r == nil || r.RequestCtx == nil {
		return context.Background()
	}
	// fasthttp's RequestCtx only becomes cancellable after it is attached to a
	// server. Keep request values while giving downstream timeouts a safe parent
	// for direct handler calls (including tests and other in-process callers).
	return context.WithoutCancel(r.RequestCtx)
}
