package metaregistry

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
)

const testServiceSecret = "synthetic-meta-registry-service-secret-32-bytes"

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
		ExternalAccountID: "page-1", ReReplyWebhookURL: "https://app.example.test/api/webhooks/channels/a",
		AccessToken: "token", InboundSecret: "inbound", OutboundSecret: "outbound",
		CredentialID: uuid.New(), CredentialVersion: 2, OwnershipCheckedAt: now,
		WebhookCredentialID: uuid.New(), WebhookCredentialVersion: 3,
	}
	require.NoError(t, binding.Validate(now))
	binding.LeaseExpiresAt = now
	require.ErrorIs(t, binding.Validate(now), ErrStaleBinding)
}
