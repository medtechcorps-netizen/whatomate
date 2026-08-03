package channel

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const threadsTestEncryptionKey = "threads-test-encryption-key"

func TestThreadsAdapterUsesOfficialGraphHost(t *testing.T) {
	adapter := NewThreadsAdapter(http.DefaultClient, threadsTestEncryptionKey)
	assert.Equal(t, "https://graph.threads.net/v1.0", adapter.apiBaseURL)
}

func TestThreadsAdapterPublishesExactTargetTextReply(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "Bearer threads-token", r.Header.Get("Authorization"))
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Empty(t, r.URL.RawQuery, "Threads reply content must not be exposed in the request URL")
		assert.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		require.NoError(t, r.ParseForm())
		switch r.URL.Path {
		case "/me/threads":
			assert.Equal(t, "TEXT", r.Form.Get("media_type"))
			assert.Equal(t, "A careful public reply", r.Form.Get("text"))
			assert.Equal(t, "mention-123", r.Form.Get("reply_to_id"))
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "container-456"})
		case "/me/threads_publish":
			assert.Equal(t, "container-456", r.Form.Get("creation_id"))
			w.Header().Set("X-FB-Request-ID", "request-789")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "published-789"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := NewThreadsAdapter(server.Client(), threadsTestEncryptionKey)
	adapter.apiBaseURL = server.URL
	account := threadsTestAccount(t, "9876543210987654")
	result, err := adapter.Send(context.Background(), account, OutboundMessage{
		IdempotencyKey:    "outbox-1",
		ReplyToExternalID: "mention-123",
		Parts: []MessagePart{{
			Type: models.MessagePartTypeText,
			Text: " A careful public reply ",
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	assert.Equal(t, []string{"published-789"}, result.ProviderMessageIDs)
	assert.Equal(t, "mention-123", result.ExternalConversationID)
	assert.Equal(t, "request-789", result.ProviderRequestID)
	assert.Equal(t, "container-456", result.Raw["container_id"])
}

func TestThreadsAdapterRejectsStandalonePostBeforeProviderCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("provider must not be called for a standalone post")
	}))
	defer server.Close()
	adapter := NewThreadsAdapter(server.Client(), threadsTestEncryptionKey)
	adapter.apiBaseURL = server.URL

	_, err := adapter.Send(context.Background(), threadsTestAccount(t, "9876543210987654"), OutboundMessage{
		IdempotencyKey: "outbox-1",
		Parts:          []MessagePart{{Type: models.MessagePartTypeText, Text: "unsafe standalone"}},
	})
	require.Error(t, err)
	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, "reply_target_required", providerErr.Code)
	assert.False(t, providerErr.Retryable)
}

func TestThreadsAdapterValidatesAuthorizedAccountIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/me", r.URL.Path)
		assert.Equal(t, "id,username,name", r.URL.Query().Get("fields"))
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id":       "another-user",
			"username": "another",
			"name":     "Another",
		})
	}))
	defer server.Close()
	adapter := NewThreadsAdapter(server.Client(), threadsTestEncryptionKey)
	adapter.apiBaseURL = server.URL

	result, err := adapter.ValidateAccount(context.Background(), threadsTestAccount(t, "9876543210987654"))
	require.Error(t, err)
	assert.False(t, result.Valid)
	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, "account_mismatch", providerErr.Code)
}

func TestThreadsAdapterRefreshesAndEncryptsCredential(t *testing.T) {
	now := time.Date(2026, time.August, 3, 8, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/refresh_access_token":
			assert.Equal(t, "th_refresh_token", r.URL.Query().Get("grant_type"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "refreshed-token",
				"token_type":   "bearer",
				"expires_in":   3600,
			})
		case "/debug_token":
			assert.Equal(t, "refreshed-token", r.URL.Query().Get("access_token"))
			assert.Equal(t, "refreshed-token", r.URL.Query().Get("input_token"))
			_ = json.NewEncoder(w).Encode(threadsTestDebugTokenPayload("9876543210987654", now.Add(time.Hour)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter := NewThreadsAdapter(server.Client(), threadsTestEncryptionKey)
	adapter.apiBaseURL = server.URL
	adapter.now = func() time.Time { return now }

	result, err := adapter.RefreshCredentials(context.Background(), threadsTestAccount(t, "9876543210987654"))
	require.NoError(t, err)
	stored, _ := result.CredentialBlob["access_token"].(string)
	assert.True(t, appcrypto.IsEncrypted(stored))
	plaintext, err := appcrypto.Decrypt(stored, threadsTestEncryptionKey)
	require.NoError(t, err)
	assert.Equal(t, "refreshed-token", plaintext)
	require.NotNil(t, result.ExpiresAt)
	assert.Equal(t, now.Add(time.Hour), *result.ExpiresAt)
}

func TestThreadsAdapterRejectsMissingRequiredPermission(t *testing.T) {
	now := time.Date(2026, time.August, 3, 8, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := threadsTestDebugTokenPayload("9876543210987654", now.Add(time.Hour))
		data := payload["data"].(map[string]any)
		data["scopes"] = []string{"threads_basic"}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()
	adapter := NewThreadsAdapter(server.Client(), threadsTestEncryptionKey)
	adapter.apiBaseURL = server.URL
	adapter.now = func() time.Time { return now }

	_, err := adapter.InspectTokenPermissions(context.Background(), "threads-token", "9876543210987654")
	require.Error(t, err)
	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, "permissions_missing", providerErr.Code)
}

func TestThreadsAdapterVerifiesWebhookSignatureAndChallenge(t *testing.T) {
	adapter := NewThreadsWebhookAdapter(nil, threadsTestEncryptionKey, "threads-app-secret", "verify-token-at-least-sixteen")
	body := []byte(`{"app_id":"threads-app-1","topic":"moderate","target_id":"123","values":{"field":"replies","value":{"id":"456"}}}`)
	mac := hmac.New(sha256.New, []byte("threads-app-secret"))
	_, _ = mac.Write(body)
	headers := http.Header{"X-Hub-Signature-256": []string{"sha256=" + hex.EncodeToString(mac.Sum(nil))}}
	require.NoError(t, adapter.VerifyWebhook(nil, headers, body))
	require.NoError(t, adapter.VerifyChallenge("verify-token-at-least-sixteen"))

	headers.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(make([]byte, sha256.Size)))
	require.Error(t, adapter.VerifyWebhook(nil, headers, body))
	require.Error(t, adapter.VerifyChallenge("different-token-value"))
}

func TestThreadsAdapterRejectsMixedAppBatchAndMisdirectedMention(t *testing.T) {
	adapter := NewThreadsWebhookAdapter(nil, threadsTestEncryptionKey, "threads-app-secret", "verify-token-at-least-sixteen")
	mixed := []byte(`[
		{"app_id":"threads-app-1","topic":"interaction","target_id":"9876543210987654","subscription_id":"sub-1","values":{"field":"mentions","value":{"id":"1001","username":"patient_one"}}},
		{"app_id":"threads-app-2","topic":"interaction","target_id":"9876543210987654","subscription_id":"sub-2","values":{"field":"mentions","value":{"id":"1002","username":"patient_two"}}}
	]`)
	_, err := adapter.RouteHint(nil, mixed)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple apps")

	account := threadsTestAccount(t, "9876543210987654")
	account.Metadata = models.JSONB{"app_id": "threads-app-1", "username": "clinic_account"}
	wrongTarget := []byte(`{"app_id":"threads-app-1","topic":"interaction","target_id":"9988776655443322","subscription_id":"sub-1","values":{"field":"mentions","value":{"id":"1001","username":"patient_one"}}}`)
	_, err = adapter.NormalizeWebhook(context.Background(), account, wrongTarget)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "different connected account")
}

func TestThreadsAdapterRequiresReplyRootOwnerToMatchConnectedAccount(t *testing.T) {
	adapter := NewThreadsWebhookAdapter(nil, threadsTestEncryptionKey, "threads-app-secret", "verify-token-at-least-sixteen")
	account := threadsTestAccount(t, "9876543210987654")
	account.Metadata = models.JSONB{"app_id": "threads-app-1", "username": "clinic_account"}

	mismatched := []byte(`{"app_id":"threads-app-1","topic":"moderate","target_id":"1000","values":{"field":"replies","value":{"id":"1001","owner_id":"2001","username":"patient_one","text":"Can I book?","replied_to":{"id":"1000"},"root_post":{"id":"1000","owner_id":"9988776655443322","username":"another_business"}}}}`)
	require.ErrorContains(t, adapter.ValidateWebhookAccount(account, mismatched), "root post belongs to a different connected account")
	_, err := adapter.NormalizeWebhook(context.Background(), account, mismatched)
	require.ErrorContains(t, err, "root post belongs to a different connected account")

	matching := []byte(`{"app_id":"threads-app-1","topic":"moderate","target_id":"1000","values":{"field":"replies","value":{"id":"1001","owner_id":"2001","username":"patient_one","text":"Can I book?","replied_to":{"id":"1000"},"root_post":{"id":"1000","owner_id":"9876543210987654","username":"clinic_account"}}}}`)
	require.NoError(t, adapter.ValidateWebhookAccount(account, matching))
	events, err := adapter.NormalizeWebhook(context.Background(), account, matching)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "1001", events[0].ProviderEventID)
}

func TestThreadsAdapterNormalizesOnlyIncomingPublicReplyAndMention(t *testing.T) {
	adapter := NewThreadsWebhookAdapter(nil, threadsTestEncryptionKey, "threads-app-secret", "verify-token-at-least-sixteen")
	now := time.Date(2026, time.August, 3, 9, 0, 0, 0, time.UTC)
	adapter.now = func() time.Time { return now }
	account := threadsTestAccount(t, "9876543210987654")
	account.Metadata = models.JSONB{"app_id": "threads-app-1", "username": "clinic_account"}
	body := []byte(`[
		{"app_id":"threads-app-1","topic":"moderate","target_id":"1000","time":1785744000,"subscription_id":"sub-1","values":{"field":"replies","value":{"id":"1001","owner_id":"2001","username":"patient_one","text":"Can I book?","media_type":"TEXT_POST","permalink":"https://www.threads.com/@patient_one/post/1","timestamp":"2026-08-03T08:59:00+0000","replied_to":{"id":"1000"},"root_post":{"id":"1000","owner_id":"9876543210987654","username":"clinic_account"}}}},
		{"app_id":"threads-app-1","topic":"interaction","target_id":"9876543210987654","time":1785744000,"subscription_id":"sub-1","values":{"field":"mentions","value":{"id":"1002","username":"patient_two","text":"@clinic_account please help","timestamp":"2026-08-03T08:58:00+0000","root_post":{"id":"1002","owner_id":"2002","username":"patient_two"}}}},
		{"app_id":"threads-app-1","topic":"moderate","target_id":"1000","time":1785744000,"subscription_id":"sub-1","values":{"field":"replies","value":{"id":"1003","username":"clinic_account","text":"Our own echo","timestamp":"2026-08-03T08:57:00+0000","root_post":{"id":"1000","owner_id":"9876543210987654","username":"clinic_account"}}}}
	]`)

	events, err := adapter.NormalizeWebhook(context.Background(), account, body)
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, "1001", events[0].Message.Conversation.ExternalID)
	assert.Equal(t, "reply", events[0].Message.Conversation.Metadata["engagement_type"])
	assert.Equal(t, "1000", events[0].Message.ReplyToExternalID)
	assert.Equal(t, "2001", events[0].Message.Sender.ExternalID)
	assert.Equal(t, "1002", events[1].Message.Conversation.ExternalID)
	assert.Equal(t, "mention", events[1].Message.Conversation.Metadata["engagement_type"])
	assert.Equal(t, "threads:mentions:1002", events[1].DedupeKey)
}

func threadsTestAccount(t *testing.T, externalID string) *models.ChannelAccount {
	t.Helper()
	encrypted, err := appcrypto.Encrypt("threads-token", threadsTestEncryptionKey)
	require.NoError(t, err)
	return &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    uuid.New(),
		Channel:           models.ChannelThreads,
		Provider:          ThreadsProvider,
		ExternalAccountID: externalID,
		Status:            models.ChannelAccountStatusActive,
		Config: models.JSONB{
			"outbound_enabled": true,
			"engagement_mode":  "public_replies_mentions",
		},
		Credentials: []models.ChannelCredential{{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			Kind:           models.ChannelCredentialKindOAuth,
			Version:        1,
			CredentialBlob: models.JSONB{"access_token": encrypted},
			Status:         models.ChannelCredentialStatusActive,
			KeyVersion:     "app:v1",
		}},
	}
}

func threadsTestDebugTokenPayload(userID string, expiresAt time.Time) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"type":                   "USER",
			"is_valid":               true,
			"user_id":                userID,
			"scopes":                 RequiredThreadsScopes(),
			"expires_at":             expiresAt.Unix(),
			"data_access_expires_at": expiresAt.Add(24 * time.Hour).Unix(),
		},
	}
}
