package metarelay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const registryClientTestSecret = "synthetic-registry-client-service-secret-32-bytes"

type panicRegistryResolver struct{}

func (panicRegistryResolver) Resolve(context.Context, models.Channel, string, bool) (*AccountConfig, error) {
	panic("registry lookup occurred before outer service authentication")
}

func TestRegistryClientAuthenticatesResponseCachesOnlyWithinLeaseAndSupportsUncachedFence(t *testing.T) {
	now := time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	credentialVersion := atomic.Int32{}
	credentialVersion.Store(4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		require.NoError(t, metaregistry.VerifyRequest(
			registryClientTestSecret, request.Method, request.URL.Path,
			request.Header.Get(metaregistry.TimestampHeader), request.Header.Get(metaregistry.NonceHeader),
			request.Header.Get(metaregistry.SignatureHeader), body, now,
		))
		binding := metaregistry.Binding{
			SchemaVersion: metaregistry.SchemaVersion, LeaseID: uuid.New(), LeaseExpiresAt: now.Add(30 * time.Second),
			OrganizationID: uuid.New(), ChannelAccountID: uuid.MustParse("f9e915c1-0b31-48e4-8380-cd9eef312276"),
			Channel: models.ChannelMessenger, ExternalAccountID: "page-1",
			ReReplyWebhookURL: "https://app.example.test/api/webhooks/channels/account",
			AccessToken:       "token", InboundSecret: "inbound", OutboundSecret: "outbound",
			CredentialID:             uuid.MustParse("d34fa16d-8dad-41d8-a145-8246e2cc6ccc"),
			CredentialVersion:        int(credentialVersion.Load()),
			WebhookCredentialID:      uuid.MustParse("16a5109b-f346-4d2f-96f9-a02e9cf77fee"),
			WebhookCredentialVersion: 7, OwnershipCheckedAt: now,
		}
		encoded, err := json.Marshal(binding)
		require.NoError(t, err)
		w.Header().Set(metaregistry.ResponseHeader, metaregistry.SignResponse(
			registryClientTestSecret, request.Header.Get(metaregistry.NonceHeader), http.StatusOK, encoded,
		))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded)
	}))
	defer server.Close()

	client, err := NewRegistryClient(&Config{
		RegistryURL: server.URL + metaregistry.ResolvePath, RegistrySecret: registryClientTestSecret,
		RegistryCacheTTL: 10 * time.Second, RegistryTimeout: time.Second,
		allowInsecureTestEndpoints: true, registryLifecycleTestMode: true,
	}, server.Client())
	require.NoError(t, err)
	client.now = func() time.Time { return now }

	first, err := client.Resolve(t.Context(), models.ChannelMessenger, "page-1", true)
	require.NoError(t, err)
	second, err := client.Resolve(t.Context(), models.ChannelMessenger, "page-1", true)
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, registryFence(first), registryFence(second))

	credentialVersion.Store(5)
	uncached, err := client.Resolve(t.Context(), models.ChannelMessenger, "page-1", false)
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Load())
	assert.NotEqual(t, registryFence(first), registryFence(uncached))
}

func TestRegistryClientFailsClosedOnUnsignedOrMismatchedBinding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body := []byte(`{"schema_version":1}`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client, err := NewRegistryClient(&Config{
		RegistryURL: server.URL + metaregistry.ResolvePath, RegistrySecret: registryClientTestSecret,
		RegistryCacheTTL: time.Second, RegistryTimeout: time.Second,
		allowInsecureTestEndpoints: true, registryLifecycleTestMode: true,
	}, server.Client())
	require.NoError(t, err)
	_, err = client.Resolve(t.Context(), models.ChannelInstagram, "ig-1", false)
	require.ErrorIs(t, err, ErrRegistryUnavailable)
}

func TestRegistryClientDistinguishesSignedStaleBinding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body := []byte(`{"error":"binding_stale"}`)
		w.Header().Set(metaregistry.ResponseHeader, metaregistry.SignResponse(
			registryClientTestSecret, request.Header.Get(metaregistry.NonceHeader), http.StatusConflict, body,
		))
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client, err := NewRegistryClient(&Config{
		RegistryURL: server.URL + metaregistry.ResolvePath, RegistrySecret: registryClientTestSecret,
		RegistryCacheTTL: time.Second, RegistryTimeout: time.Second,
		allowInsecureTestEndpoints: true, registryLifecycleTestMode: true,
	}, server.Client())
	require.NoError(t, err)
	_, err = client.Resolve(t.Context(), models.ChannelMessenger, "page-stale", false)
	require.ErrorIs(t, err, ErrRegistryStale)
}

func TestRegistryClientRejectsNonExactOrProductionInsecureEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"http://registry.example.test" + metaregistry.ResolvePath,
		"https://registry.example.test" + metaregistry.ResolvePath + "?",
		"https://registry.example.test/internal/meta-registry/v1/%72esolve",
		"https://user@registry.example.test" + metaregistry.ResolvePath,
	} {
		client, err := NewRegistryClient(&Config{
			RegistryURL: endpoint, RegistrySecret: registryClientTestSecret,
			RegistryCacheTTL: time.Second, RegistryTimeout: time.Second,
			registryLifecycleTestMode: true,
		}, nil)
		require.Error(t, err, endpoint)
		require.Nil(t, client, endpoint)
	}
}

func TestDynamicPublicRouteRequiresOuterServiceCredentialBeforeRegistryLookup(t *testing.T) {
	config := newTestConfig(t)
	config.RegistryEdgeSecret = "synthetic-registry-edge-secret-at-least-32-bytes"
	server := &Server{config: config, registry: panicRegistryResolver{}}
	request := httptest.NewRequest(http.MethodHead, "/v1/accounts/messenger/page-dynamic", nil)
	request.SetPathValue("channel", "messenger")
	request.SetPathValue("externalID", "page-dynamic")
	_, err := server.accountFromPath(request, false)
	require.ErrorIs(t, err, ErrRegistryUnauthorized)

	// Static fallback remains first and does not acquire the new dynamic edge
	// dependency during the split-A rollout.
	static, ok := config.account(models.ChannelMessenger, "page-1")
	require.True(t, ok)
	staticRequest := httptest.NewRequest(http.MethodHead, "/v1/accounts/messenger/page-1", nil)
	staticRequest.SetPathValue("channel", "messenger")
	staticRequest.SetPathValue("externalID", "page-1")
	resolved, err := server.accountFromPath(staticRequest, false)
	require.NoError(t, err)
	require.Same(t, static, resolved)
}
