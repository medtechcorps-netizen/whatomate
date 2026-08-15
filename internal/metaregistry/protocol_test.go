package metaregistry

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
)

const testServiceSecret = "synthetic-meta-registry-service-secret-32-bytes"

// legacyBindingV1 is the exact pre-managed-Instagram response wire shape. It
// intentionally remains explicit so strict old/new decoder compatibility is a
// regression-tested rollout invariant.
type legacyBindingV1 struct {
	SchemaVersion            int            `json:"schema_version"`
	LeaseID                  uuid.UUID      `json:"lease_id"`
	LeaseExpiresAt           time.Time      `json:"lease_expires_at"`
	OrganizationID           uuid.UUID      `json:"organization_id"`
	ChannelAccountID         uuid.UUID      `json:"channel_account_id"`
	Channel                  models.Channel `json:"channel"`
	ExternalAccountID        string         `json:"external_account_id"`
	PlatformAppID            string         `json:"platform_app_id,omitempty"`
	InstagramAPIMode         string         `json:"instagram_api_mode,omitempty"`
	ReReplyWebhookURL        string         `json:"rereply_webhook_url"`
	AccessToken              string         `json:"access_token"`
	InboundSecret            string         `json:"inbound_secret"`
	OutboundSecret           string         `json:"outbound_secret"`
	CredentialID             uuid.UUID      `json:"credential_id"`
	CredentialVersion        int            `json:"credential_version"`
	WebhookCredentialID      uuid.UUID      `json:"webhook_credential_id"`
	WebhookCredentialVersion int            `json:"webhook_credential_version"`
	OwnershipCheckedAt       time.Time      `json:"ownership_checked_at"`
}

func TestRequestSignatureBindsMethodPathNonceTimestampAndBody(t *testing.T) {
	now := time.Date(2026, 8, 15, 1, 2, 3, 0, time.UTC)
	body := []byte(`{"channel":"messenger","external_account_id":"page-1"}`)
	signature := SignRequest(testServiceSecret, "POST", ResolvePath, now, "nonce-1", body)
	require.NoError(t, VerifyRequest(testServiceSecret, "POST", ResolvePath,
		"1786755723", "nonce-1", signature, body, now))
	require.Error(t, VerifyRequest(testServiceSecret, "POST", ResolvePath,
		"1786755723", "nonce-1", signature, append(body, 'x'), now))
	require.Error(t, VerifyRequest(testServiceSecret, "POST", RevokePath,
		"1786755723", "nonce-1", signature, body, now))
	require.Error(t, VerifyRequest(testServiceSecret, "POST", ResolvePath,
		"1786755723", "nonce-2", signature, body, now))
}

func TestRequestSignatureRejectsStaleTimestampAndWeakSecret(t *testing.T) {
	now := time.Now().UTC()
	body := []byte("{}")
	old := now.Add(-MaxClockSkew - time.Second)
	signature := SignRequest(testServiceSecret, "POST", ResolvePath, old, "nonce", body)
	require.Error(t, VerifyRequest(testServiceSecret, "POST", ResolvePath,
		strconv.FormatInt(old.Unix(), 10), "nonce", signature, body, now))
	require.Error(t, VerifyRequest("short", "POST", ResolvePath,
		"0", "nonce", "", body, now))
}

func TestResponseSignatureDetectsTampering(t *testing.T) {
	body := []byte(`{"schema_version":1}`)
	signature := SignResponse(testServiceSecret, "nonce", 200, body)
	require.NoError(t, VerifyResponse(testServiceSecret, "nonce", 200, body, signature))
	require.Error(t, VerifyResponse(testServiceSecret, "nonce", 404, body, signature))
}

func TestBindingValidationRequiresShortLivedCompleteLease(t *testing.T) {
	now := time.Now().UTC()
	binding := Binding{
		SchemaVersion: SchemaVersion, LeaseID: uuid.New(), LeaseExpiresAt: now.Add(time.Minute),
		OrganizationID: uuid.New(), ChannelAccountID: uuid.New(), Channel: models.ChannelMessenger,
		ExternalAccountID: "page-1", PlatformAppID: "123456",
		ReReplyWebhookURL: "https://app.example.test/api/webhooks/channels/a",
		AccessToken:       "token", InboundSecret: "inbound", OutboundSecret: "outbound",
		CredentialID: uuid.New(), CredentialVersion: 2, OwnershipCheckedAt: now,
		WebhookCredentialID: uuid.New(), WebhookCredentialVersion: 3,
	}
	require.NoError(t, binding.Validate(now))
	binding.LeaseExpiresAt = now
	require.ErrorIs(t, binding.Validate(now), ErrStaleBinding)
}

func TestInstagramBindingValidationRequiresAppAndAPIMode(t *testing.T) {
	now := time.Now().UTC()
	binding := Binding{
		SchemaVersion: SchemaVersion, LeaseID: uuid.New(), LeaseExpiresAt: now.Add(time.Minute),
		OrganizationID: uuid.New(), ChannelAccountID: uuid.New(), Channel: models.ChannelInstagram,
		ExternalAccountID: "profile-1",
		PlatformAppID:     "123456", InstagramAPIMode: "instagram_login",
		ReReplyWebhookURL: "https://app.example.test/api/webhooks/channels/a",
		AccessToken:       "token", InboundSecret: "inbound", OutboundSecret: "outbound",
		CredentialID: uuid.New(), CredentialVersion: 2, OwnershipCheckedAt: now,
		WebhookCredentialID: uuid.New(), WebhookCredentialVersion: 3,
	}
	require.NoError(t, binding.Validate(now))

	binding.PlatformAppID = ""
	require.ErrorIs(t, binding.Validate(now), ErrInvalidRequest)
	binding.PlatformAppID = "123456"
	binding.InstagramAPIMode = ""
	require.ErrorIs(t, binding.Validate(now), ErrInvalidRequest)
}

func TestBindingV1StrictOldAndNewReadersRemainWireCompatible(t *testing.T) {
	now := time.Date(2026, 8, 15, 4, 0, 0, 0, time.UTC)
	current := Binding{
		SchemaVersion: SchemaVersion, LeaseID: uuid.New(), LeaseExpiresAt: now.Add(time.Minute),
		OrganizationID: uuid.New(), ChannelAccountID: uuid.New(),
		Channel: models.ChannelMessenger, ExternalAccountID: "synthetic-page-1",
		PlatformAppID:     "123456",
		ReReplyWebhookURL: "https://app.example.test/api/webhooks/channels/synthetic",
		AccessToken:       "synthetic-token", InboundSecret: "synthetic-inbound", OutboundSecret: "synthetic-outbound",
		CredentialID: uuid.New(), CredentialVersion: 2,
		WebhookCredentialID: uuid.New(), WebhookCredentialVersion: 3,
		OwnershipCheckedAt: now,
	}
	currentWire, err := json.Marshal(current)
	require.NoError(t, err)
	require.NotContains(t, string(currentWire), `"purpose"`, "response purpose would break the strict v1 reader")
	var legacy legacyBindingV1
	legacyDecoder := json.NewDecoder(bytes.NewReader(currentWire))
	legacyDecoder.DisallowUnknownFields()
	require.NoError(t, legacyDecoder.Decode(&legacy), "old relay must accept a new broker response")

	legacyWire, err := json.Marshal(legacy)
	require.NoError(t, err)
	var decodedCurrent Binding
	currentDecoder := json.NewDecoder(bytes.NewReader(legacyWire))
	currentDecoder.DisallowUnknownFields()
	require.NoError(t, currentDecoder.Decode(&decodedCurrent), "new relay must accept an old broker response")
	require.NoError(t, decodedCurrent.Validate(now))
}
