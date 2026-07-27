package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

var ErrChannelAdapterUnavailable = errors.New("channel provider adapter is not available")
var ErrLegacyMetaAccountManaged = errors.New("legacy Meta channel accounts are managed by WhatsApp setup")

var channelProviderIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var channelExternalAccountIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+-]{0,254}$`)

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
	IsDefaultIncoming *bool         `json:"is_default_incoming,omitempty"`
	IsDefaultOutgoing *bool         `json:"is_default_outgoing,omitempty"`
}

type ChannelAccountResponse struct {
	ID                uuid.UUID                   `json:"id"`
	OrganizationID    uuid.UUID                   `json:"organization_id"`
	Channel           models.Channel              `json:"channel"`
	Provider          string                      `json:"provider"`
	Name              string                      `json:"name"`
	ExternalAccountID string                      `json:"external_account_id"`
	Status            models.ChannelAccountStatus `json:"status"`
	Capabilities      models.JSONB                `json:"capabilities"`
	Config            models.JSONB                `json:"config"`
	IsDefaultIncoming bool                        `json:"is_default_incoming"`
	IsDefaultOutgoing bool                        `json:"is_default_outgoing"`
	HasCredentials    bool                        `json:"has_credentials"`
	ConnectedAt       *time.Time                  `json:"connected_at,omitempty"`
	LastHealthCheckAt *time.Time                  `json:"last_health_check_at,omitempty"`
	LastInboundAt     *time.Time                  `json:"last_inbound_at,omitempty"`
	LastOutboundAt    *time.Time                  `json:"last_outbound_at,omitempty"`
	LastErrorAt       *time.Time                  `json:"last_error_at,omitempty"`
	LastError         string                      `json:"last_error,omitempty"`
	OutboxPending     int64                       `json:"outbox_pending"`
	OutboxFailed      int64                       `json:"outbox_failed"`
	CreatedByID       *uuid.UUID                  `json:"created_by_id,omitempty"`
	UpdatedByID       *uuid.UUID                  `json:"updated_by_id,omitempty"`
	CreatedAt         time.Time                   `json:"created_at"`
	UpdatedAt         time.Time                   `json:"updated_at"`
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

	inboundSecret, err := generateChannelSecret()
	if err != nil {
		a.Log.Error("Failed to generate channel webhook secret", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create channel account", nil, "")
	}
	encryptedInbound, err := appcrypto.Encrypt(inboundSecret, a.Config.App.EncryptionKey)
	if err != nil {
		a.Log.Error("Failed to encrypt channel webhook secret", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create channel account", nil, "")
	}
	credentialBlob := models.JSONB{"inbound_secret": encryptedInbound}
	if request.OutboundSecret != "" {
		encryptedOutbound, encryptErr := appcrypto.Encrypt(request.OutboundSecret, a.Config.App.EncryptionKey)
		if encryptErr != nil {
			a.Log.Error("Failed to encrypt channel outbound secret", "error", encryptErr)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create channel account", nil, "")
		}
		credentialBlob["outbound_secret"] = encryptedOutbound
	}

	config := cloneJSONB(request.Config)
	// Activation and outbound approval happen only after TestChannelAccount.
	config["outbound_enabled"] = false
	account := models.ChannelAccount{
		OrganizationID:    orgID,
		Channel:           request.Channel,
		Provider:          request.Provider,
		Name:              request.Name,
		ExternalAccountID: request.ExternalAccountID,
		Status:            models.ChannelAccountStatusPending,
		Capabilities:      cloneJSONB(request.Capabilities),
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

	var response ChannelAccountResponse
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		account, findErr := loadChannelAccount(tx, orgID, id, true)
		if findErr != nil {
			return findErr
		}
		if account.Provider == channelapi.LegacyMetaProvider {
			return ErrLegacyMetaAccountManaged
		}
		oldAccount := *account
		credentialChanged := false
		requiresRetest := false

		if request.Name != nil {
			name := strings.TrimSpace(*request.Name)
			if name == "" || utf8.RuneCountInString(name) > 100 {
				return errors.New("channel account name must be between 1 and 100 characters")
			}
			account.Name = name
		}
		if request.Config != nil {
			outboundEnabled := boolConfigValue(account.Config, "outbound_enabled")
			previousRelayURL, _ := account.Config["relay_url"].(string)
			account.Config = cloneJSONB(*request.Config)
			account.Config["outbound_enabled"] = outboundEnabled
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
		}
		if request.OutboundEnabled != nil {
			if *request.OutboundEnabled && account.Status != models.ChannelAccountStatusActive {
				return errors.New("test and activate the channel account before enabling outbound delivery")
			}
			account.Config["outbound_enabled"] = *request.OutboundEnabled
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
			if *request.IsDefaultOutgoing {
				if err := tx.Model(&models.ChannelAccount{}).
					Where("organization_id = ? AND id <> ? AND is_default_outgoing = ?", orgID, id, true).
					Update("is_default_outgoing", false).Error; err != nil {
					return err
				}
			}
			account.IsDefaultOutgoing = *request.IsDefaultOutgoing
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
			account.Config["outbound_enabled"] = false
			account.Status = models.ChannelAccountStatusPending
			account.LastHealthCheckAt = nil
			account.LastError = ""
			account.LastErrorAt = nil
		}

		account.UpdatedByID = &userID
		if err := tx.
			Where("id = ? AND organization_id = ?", account.ID, orgID).
			Save(account).Error; err != nil {
			return err
		}
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
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	return r.SendEnvelope(response)
}

func (a *App) TestChannelAccount(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceChannelAccounts, models.ActionWrite)
	if err != nil {
		return nil
	}
	id, err := parsePathUUID(r, "id", "channel account")
	if err != nil {
		return nil
	}
	account, err := loadChannelAccount(a.DB, orgID, id, true)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Channel account not found", nil, "")
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
	adapter, err := a.channelAdapter(account)
	if err != nil {
		return r.SendEnvelope(map[string]any{
			"success": false,
			"status":  account.Status,
			"error":   "No approved adapter is available for this provider",
		})
	}

	result, validationErr := adapter.ValidateAccount(requestContext(r), account)
	now := time.Now().UTC()
	oldAccount := *account
	account.LastHealthCheckAt = &now
	if validationErr != nil || !result.Valid {
		account.LastErrorAt = &now
		if validationErr != nil {
			account.LastError = validationErr.Error()
		} else {
			account.LastError = "Provider rejected the channel account"
		}
		if account.Status == models.ChannelAccountStatusActive {
			account.Status = models.ChannelAccountStatusDegraded
		}
	} else {
		account.Status = models.ChannelAccountStatusActive
		account.Capabilities = capabilitiesToJSONB(result.Capabilities)
		account.LastError = ""
		account.LastErrorAt = nil
		if account.ConnectedAt == nil {
			account.ConnectedAt = &now
		}
	}
	account.UpdatedByID = &userID

	if err := a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.ChannelAccount{}).
			Where("id = ? AND organization_id = ?", account.ID, orgID).
			Updates(map[string]any{
				"status":               account.Status,
				"capabilities":         account.Capabilities,
				"last_health_check_at": account.LastHealthCheckAt,
				"last_error":           account.LastError,
				"last_error_at":        account.LastErrorAt,
				"connected_at":         account.ConnectedAt,
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
	}); err != nil {
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
		account, findErr := loadChannelAccount(tx, orgID, id, false)
		if findErr != nil {
			return findErr
		}
		if account.Provider == channelapi.LegacyMetaProvider {
			return ErrLegacyMetaAccountManaged
		}
		if err := tx.
			Where("id = ? AND organization_id = ?", id, orgID).
			Delete(&models.ChannelAccount{}).Error; err != nil {
			return err
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
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete channel account", nil, "")
	}
	return r.SendEnvelope(map[string]string{"message": "Channel account deleted successfully"})
}

func (a *App) channelAdapter(account *models.ChannelAccount) (channelapi.Adapter, error) {
	if account == nil {
		return nil, ErrChannelAdapterUnavailable
	}
	if account.Channel == models.ChannelTikTok {
		return nil, fmt.Errorf("%w: TikTok connections are not yet approved", ErrChannelAdapterUnavailable)
	}
	if strings.EqualFold(account.Provider, channelapi.RelayProvider) {
		encryptionKey := ""
		if a.Config != nil {
			encryptionKey = a.Config.App.EncryptionKey
		}
		return channelapi.NewRelayAdapter(account.Channel, a.HTTPClient, encryptionKey), nil
	}
	return nil, fmt.Errorf("%w: channel=%s provider=%s", ErrChannelAdapterUnavailable, account.Channel, account.Provider)
}

func validChannelIdentifier(channel models.Channel) bool {
	switch channel {
	case models.ChannelWhatsApp,
		models.ChannelInstagram,
		models.ChannelMessenger,
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

func loadChannelAccount(db *gorm.DB, orgID, accountID uuid.UUID, credentials bool) (*models.ChannelAccount, error) {
	query := db
	if credentials {
		query = query.Preload(
			"Credentials",
			"organization_id = ? AND status IN ? AND (expires_at IS NULL OR expires_at > ?)",
			orgID,
			[]models.ChannelCredentialStatus{
				models.ChannelCredentialStatusActive,
				models.ChannelCredentialStatusExpiring,
			},
			time.Now().UTC(),
		)
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
		ID:                account.ID,
		OrganizationID:    account.OrganizationID,
		Channel:           account.Channel,
		Provider:          account.Provider,
		Name:              account.Name,
		ExternalAccountID: account.ExternalAccountID,
		Status:            account.Status,
		Capabilities:      cloneJSONB(account.Capabilities),
		Config:            sanitizeChannelConfig(account.Config),
		IsDefaultIncoming: account.IsDefaultIncoming,
		IsDefaultOutgoing: account.IsDefaultOutgoing,
		HasCredentials:    currentChannelCredential(account) != nil,
		ConnectedAt:       account.ConnectedAt,
		LastHealthCheckAt: account.LastHealthCheckAt,
		LastInboundAt:     account.LastInboundAt,
		LastOutboundAt:    account.LastOutboundAt,
		LastErrorAt:       account.LastErrorAt,
		LastError:         account.LastError,
		CreatedByID:       account.CreatedByID,
		UpdatedByID:       account.UpdatedByID,
		CreatedAt:         account.CreatedAt,
		UpdatedAt:         account.UpdatedAt,
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
	for i := range account.Credentials {
		credential := &account.Credentials[i]
		statusCurrent := credential.Status == models.ChannelCredentialStatusActive ||
			credential.Status == models.ChannelCredentialStatusExpiring
		if statusCurrent && (credential.ExpiresAt == nil || credential.ExpiresAt.After(now)) {
			return &account.Credentials[i]
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
	return r.RequestCtx
}
