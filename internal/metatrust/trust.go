// Package metatrust resolves deployment-controlled trust bindings for Meta
// relay accounts. It is shared by request handlers and background workers so a
// queued delivery cannot bypass the same protected origin and ownership checks
// required by the interactive Test flow.
package metatrust

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
)

type inventoryDocument struct {
	MessengerApp inventoryMessengerApp `json:"messenger_app"`
	Accounts     []inventoryAccount    `json:"accounts"`
}

type inventoryMessengerApp struct {
	AppID           string `json:"app_id"`
	OwnerBusinessID string `json:"owner_business_id"`
}

type inventoryAccount struct {
	OrganizationID    string         `json:"organization_id"`
	MetaBusinessID    string         `json:"meta_business_id"`
	Channel           models.Channel `json:"channel"`
	ExternalAccountID string         `json:"external_account_id"`
	ReReplyAccountID  string         `json:"rereply_account_id"`
}

type protectedInventory struct {
	MessengerApp inventoryMessengerApp
	Accounts     map[uuid.UUID]inventoryAccount
}

// Binding contains the protected facts needed by a Meta relay adapter.
type Binding struct {
	MetaBusinessID string
	RelayURL       string
}

const minimumProviderProofBytes = 32

// Resolve requires account identity, ownership, relay origin, and route to
// match the application process's protected inventory exactly.
func Resolve(settings config.MetaRelayConfig, environment string, account *models.ChannelAccount) (Binding, error) {
	if account == nil ||
		(account.Channel != models.ChannelMessenger && account.Channel != models.ChannelInstagram) ||
		!strings.EqualFold(strings.TrimSpace(account.Provider), "relay") {
		return Binding{}, errors.New("Meta relay account is required")
	}
	if len([]byte(settings.ProviderProofSecret)) < minimumProviderProofBytes ||
		strings.TrimSpace(settings.ProviderProofSecret) != settings.ProviderProofSecret {
		return Binding{}, errors.New("trusted Meta relay provider proof secret must be at least 32 bytes without surrounding whitespace")
	}
	base, err := baseURL(settings, environment)
	if err != nil {
		return Binding{}, err
	}
	inventory, err := parseInventory(settings.ExpectedAccountsJSON)
	if err != nil {
		return Binding{}, err
	}
	expected, ok := inventory.Accounts[account.ID]
	if !ok ||
		expected.OrganizationID != account.OrganizationID.String() ||
		expected.Channel != account.Channel ||
		expected.ExternalAccountID != account.ExternalAccountID {
		return Binding{}, errors.New("channel account is absent or mismatched in the trusted Meta relay inventory")
	}
	// Accounts created by the managed Facebook Login for Business flow carry
	// server-only owned_pages evidence. For those accounts the protected relay
	// inventory cannot silently substitute another Business Portfolio: both
	// control planes must agree on the exact business ID before runtime trust is
	// issued. Legacy explicitly governed rows do not have this management mode
	// and retain the existing inventory-only contract.
	if configString(account.Metadata, "management_mode") == "meta_messenger_oauth" {
		if account.Channel != models.ChannelMessenger {
			return Binding{}, errors.New("managed Messenger ownership evidence is only valid for a Messenger account")
		}
		metadataBusinessID := configString(account.Metadata, "meta_business_id")
		verifiedAt := configString(account.Metadata, "ownership_verified_at")
		if metadataBusinessID == "" || metadataBusinessID != expected.MetaBusinessID ||
			configString(account.Metadata, "ownership_evidence_version") != "owned_pages_v1" ||
			verifiedAt == "" {
			return Binding{}, errors.New("managed Messenger ownership evidence does not match the trusted Meta relay inventory")
		}
		if _, err := time.Parse(time.RFC3339Nano, verifiedAt); err != nil {
			return Binding{}, errors.New("managed Messenger ownership evidence timestamp is invalid")
		}

		// The protected deployment inventory, rather than tenant-editable Config,
		// pins the app that is allowed to manage this Page. meta_config_id is kept
		// only as an onboarding/session audit fingerprint and is deliberately not
		// part of the runtime trust decision.
		if !canonicalMetaID(inventory.MessengerApp.AppID) ||
			!canonicalMetaID(inventory.MessengerApp.OwnerBusinessID) {
			return Binding{}, errors.New("trusted Meta relay Messenger app binding is missing or invalid")
		}
		if configString(account.Metadata, "meta_app_id") != inventory.MessengerApp.AppID ||
			configString(account.Metadata, "meta_app_owner_business_id") != inventory.MessengerApp.OwnerBusinessID {
			return Binding{}, errors.New("managed Messenger app evidence does not match the trusted Meta relay inventory")
		}
	}

	expectedURL := *base
	expectedURL.Path = strings.TrimRight(base.Path, "/") +
		"/v1/accounts/" + url.PathEscape(string(account.Channel)) + "/" + url.PathEscape(account.ExternalAccountID)
	relayURL := expectedURL.String()
	if strings.TrimSpace(configString(account.Config, "relay_url")) != relayURL {
		return Binding{}, errors.New("config.relay_url does not match the trusted Meta relay endpoint")
	}
	if healthURL := strings.TrimSpace(configString(account.Config, "health_url")); healthURL != "" && healthURL != relayURL {
		return Binding{}, errors.New("config.health_url must be absent or match the trusted Meta relay endpoint")
	}
	return Binding{MetaBusinessID: expected.MetaBusinessID, RelayURL: relayURL}, nil
}

// ValidateOutbound is the shared, fail-closed Meta delivery gate used at
// request enqueue and immediately before background delivery. It deliberately
// does not grandfather historical outbound_enabled rows: every live send must
// retain an exact protected mapping, confirmed asset identity, current account
// health, and a newly persisted provider-proven customer message after Test.
func ValidateOutbound(
	settings config.MetaRelayConfig,
	environment string,
	account *models.ChannelAccount,
	now time.Time,
) (Binding, error) {
	binding, err := Resolve(settings, environment, account)
	if err != nil {
		return Binding{}, err
	}
	if account.Config == nil || configString(account.Config, "identity_confirmed_id") != account.ExternalAccountID {
		return Binding{}, errors.New("Meta account identity is not confirmed for outbound delivery")
	}
	if account.Metadata == nil ||
		configString(account.Metadata, channelapi.MetaProviderProofMetadataKey) != channelapi.MetaProviderProofVersion {
		return Binding{}, errors.New("Meta account has not completed the current provider-proof Test")
	}
	if account.Status != models.ChannelAccountStatusActive ||
		account.LastHealthCheckAt == nil ||
		account.LastErrorAt != nil ||
		strings.TrimSpace(account.LastError) != "" {
		return Binding{}, errors.New("Meta account does not have a successful current health check")
	}
	if account.LastInboundAt == nil || !account.LastInboundAt.After(*account.LastHealthCheckAt) {
		return Binding{}, errors.New("Meta account has no provider-proven customer message after its latest health check")
	}
	if !configBool(account.Config, "outbound_enabled") {
		return Binding{}, errors.New("Meta outbound delivery is not enabled")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	requiredKind, providerRequiresKind := channelapi.ProviderRequiredCredentialKind(
		account.Channel,
		account.Provider,
	)
	if !providerRequiresKind || requiredKind != models.ChannelCredentialKindWebhook ||
		channelapi.CurrentCredentialCountOfKind(
			account.Credentials,
			requiredKind,
			now,
		) != 1 {
		return Binding{}, errors.New("Meta account must have exactly one current relay webhook credential")
	}
	return binding, nil
}

func baseURL(settings config.MetaRelayConfig, environment string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(settings.BaseURL))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("trusted Meta relay base URL must be absolute and omit credentials, query, and fragment")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		developmentLoopback := !strings.EqualFold(strings.TrimSpace(environment), "production") &&
			strings.EqualFold(parsed.Scheme, "http") && isLoopbackHostname(parsed.Hostname())
		if !developmentLoopback {
			return nil, errors.New("trusted Meta relay base URL must use HTTPS")
		}
	}
	if parsed.RawPath != "" || strings.Contains(parsed.Path, "//") {
		return nil, errors.New("trusted Meta relay base URL path must be canonical")
	}
	cleanPath := path.Clean("/" + strings.TrimPrefix(parsed.Path, "/"))
	if cleanPath == "/" {
		cleanPath = ""
	}
	if parsed.Path != cleanPath && parsed.Path != cleanPath+"/" {
		return nil, errors.New("trusted Meta relay base URL path must be canonical")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = cleanPath
	return parsed, nil
}

func parseInventory(raw string) (protectedInventory, error) {
	if strings.TrimSpace(raw) == "" {
		return protectedInventory{}, errors.New("trusted Meta relay expected-account inventory is required")
	}
	var document inventoryDocument
	decoder := json.NewDecoder(strings.NewReader(raw))
	if err := decoder.Decode(&document); err != nil {
		return protectedInventory{}, errors.New("trusted Meta relay expected-account inventory is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return protectedInventory{}, errors.New("trusted Meta relay expected-account inventory contains trailing data")
	}
	if len(document.Accounts) == 0 {
		return protectedInventory{}, errors.New("trusted Meta relay expected-account inventory contains no accounts")
	}
	// messenger_app remains optional for legacy inventory-governed rows. When
	// present, however, malformed protected IDs invalidate the document rather
	// than being normalized or silently ignored.
	if document.MessengerApp.AppID != "" && !canonicalMetaID(document.MessengerApp.AppID) {
		return protectedInventory{}, errors.New("trusted Meta relay messenger_app.app_id must be a canonical numeric Meta ID")
	}
	if document.MessengerApp.OwnerBusinessID != "" && !canonicalMetaID(document.MessengerApp.OwnerBusinessID) {
		return protectedInventory{}, errors.New("trusted Meta relay messenger_app.owner_business_id must be a canonical numeric Meta ID")
	}

	byAccountID := make(map[uuid.UUID]inventoryAccount, len(document.Accounts))
	byExternal := make(map[string]uuid.UUID, len(document.Accounts))
	organizationBusinesses := make(map[string]string, len(document.Accounts))
	for index, expected := range document.Accounts {
		accountID, err := canonicalUUID(expected.ReReplyAccountID)
		if err != nil {
			return protectedInventory{}, fmt.Errorf("trusted Meta relay account %d rereply_account_id: %w", index, err)
		}
		organizationID, err := canonicalUUID(expected.OrganizationID)
		if err != nil {
			return protectedInventory{}, fmt.Errorf("trusted Meta relay account %d organization_id: %w", index, err)
		}
		if !canonicalMetaID(expected.MetaBusinessID) {
			return protectedInventory{}, fmt.Errorf("trusted Meta relay account %d meta_business_id must be a canonical numeric Meta ID", index)
		}
		if expected.Channel != models.ChannelMessenger && expected.Channel != models.ChannelInstagram {
			return protectedInventory{}, fmt.Errorf("trusted Meta relay account %d channel must be messenger or instagram", index)
		}
		if !canonicalMetaID(expected.ExternalAccountID) {
			return protectedInventory{}, fmt.Errorf("trusted Meta relay account %d external_account_id must be a canonical numeric Meta ID", index)
		}
		expected.ReReplyAccountID = accountID.String()
		expected.OrganizationID = organizationID.String()
		if _, duplicate := byAccountID[accountID]; duplicate {
			return protectedInventory{}, fmt.Errorf("trusted Meta relay inventory reuses channel account ID %s", accountID)
		}
		externalKey := string(expected.Channel) + "\x00" + expected.ExternalAccountID
		if prior, duplicate := byExternal[externalKey]; duplicate {
			return protectedInventory{}, fmt.Errorf("trusted Meta relay inventory maps one Meta asset to both %s and %s", prior, accountID)
		}
		if prior, exists := organizationBusinesses[expected.OrganizationID]; exists && prior != expected.MetaBusinessID {
			return protectedInventory{}, fmt.Errorf("trusted Meta relay organization %s is bound to multiple Meta Business IDs", expected.OrganizationID)
		}
		organizationBusinesses[expected.OrganizationID] = expected.MetaBusinessID
		byExternal[externalKey] = accountID
		byAccountID[accountID] = expected
	}
	return protectedInventory{
		MessengerApp: document.MessengerApp,
		Accounts:     byAccountID,
	}, nil
}

func canonicalUUID(raw string) (uuid.UUID, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return uuid.Nil, errors.New("must be a canonical non-zero UUID")
	}
	parsed, err := uuid.Parse(raw)
	if err != nil || parsed == uuid.Nil || parsed.String() != raw {
		return uuid.Nil, errors.New("must be a canonical non-zero UUID")
	}
	return parsed, nil
}

func canonicalMetaID(value string) bool {
	if value == "" || len(value) > 32 || value[0] == '0' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func configString(values models.JSONB, key string) string {
	value, _ := values[key].(string)
	return value
}

func configBool(values models.JSONB, key string) bool {
	value, ok := values[key]
	if !ok {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes":
			return true
		}
	}
	return false
}
