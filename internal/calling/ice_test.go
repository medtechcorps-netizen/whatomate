package calling

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/stretchr/testify/require"
)

type turnRoundTripFunc func(*http.Request) (*http.Response, error)

func (f turnRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func turnTestConfig() config.CloudflareTURNConfig {
	return config.CloudflareTURNConfig{KeyID: strings.Repeat("a", 32), APIToken: "synthetic-long-lived-turn-api-token"}
}

func turnTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func turnTestJSON(credential string) string {
	body, _ := json.Marshal(map[string]any{"iceServers": []ICEServer{{URLs: []string{"stun:stun.cloudflare.com:3478"}}, {URLs: []string{"turn:turn.cloudflare.com:3478?transport=udp", cloudflareTURNURL}, Username: "temporary-user", Credential: credential}}})
	return string(body)
}

func TestCloudflareTURNFreshnessExpiryAndSharedPublicResolver(t *testing.T) {
	now := time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC)
	var calls int
	client := &http.Client{Transport: turnRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "https://rtc.live.cloudflare.com/v1/turn/keys/"+strings.Repeat("a", 32)+"/credentials/generate-ice-servers", req.URL.String())
		require.Equal(t, http.MethodPost, req.Method)
		require.Equal(t, "Bearer "+turnTestConfig().APIToken, req.Header.Get("Authorization"))
		require.Equal(t, "ReReply-TURN/1.0", req.UserAgent())
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"ttl":86400}`, string(body))
		return turnTestResponse(201, turnTestJSON("temporary-password")), nil
	})}
	provider := newCloudflareTURNProvider(turnTestConfig(), client)
	provider.now = func() time.Time { return now }
	manager := &Manager{config: &config.CallingConfig{CloudflareTURN: turnTestConfig(), RelayOnly: true}}
	manager.iceProviderOnce.Do(func() { manager.iceProvider = provider })
	first, err := manager.ResolveICEConfiguration(context.Background())
	require.NoError(t, err)
	require.Equal(t, "relay", first.TransportPolicy)
	require.Equal(t, []string{cloudflareTURNURL}, first.Servers[0].URLs)
	require.Equal(t, now.Add(24*time.Hour), *first.ExpiresAt)
	serialized, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(serialized), turnTestConfig().APIToken)
	first.Servers[0].URLs[0] = "changed-by-caller"
	now = now.Add(time.Hour - time.Nanosecond)
	second, err := manager.ResolveICEConfiguration(context.Background())
	require.NoError(t, err)
	require.Equal(t, cloudflareTURNURL, second.Servers[0].URLs[0])
	require.Equal(t, 1, calls)
	now = now.Add(time.Nanosecond)
	_, err = manager.ResolveICEConfiguration(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, calls, "one-hour boundary must refresh")
	now = now.Add(24 * time.Hour)
	_, err = manager.ResolveICEConfiguration(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, calls, "expired bundles must never be reused")
}

func TestCloudflareTURNFailureDoesNotReturnStaleBundleAndCanRetry(t *testing.T) {
	now := time.Now()
	fail := false
	provider := newCloudflareTURNProvider(turnTestConfig(), &http.Client{Transport: turnRoundTripFunc(func(*http.Request) (*http.Response, error) {
		if fail {
			return nil, errors.New("raw private transport " + turnTestConfig().APIToken)
		}
		return turnTestResponse(201, turnTestJSON("temporary-password")), nil
	})})
	provider.now = func() time.Time { return now }
	_, err := provider.resolve(context.Background())
	require.NoError(t, err)
	now = now.Add(time.Hour)
	fail = true
	bundle, err := provider.resolve(context.Background())
	require.Error(t, err)
	require.Empty(t, bundle.server.Credential)
	require.NotContains(t, err.Error(), turnTestConfig().APIToken)
	fail = false
	_, err = provider.resolve(context.Background())
	require.NoError(t, err)
}

func TestCloudflareTURNRejectsInvalidResponsesAndRedirects(t *testing.T) {
	badServers := func(server ICEServer) string {
		raw, _ := json.Marshal(map[string]any{"iceServers": []ICEServer{server}})
		return string(raw)
	}
	for _, tc := range []struct {
		name              string
		status            int
		contentType, body string
	}{
		{"unauthorized", 401, "application/json", turnTestConfig().APIToken},
		{"redirect", 307, "application/json", ""},
		{"wrong type", 201, "text/html", turnTestJSON("temporary-password")},
		{"invalid json", 201, "application/json", "{"},
		{"oversized", 201, "application/json", strings.Repeat("x", cloudflareResponseLimit+1)},
		{"wrong endpoint", 201, "application/json", badServers(ICEServer{URLs: []string{"turns:other.invalid:443?transport=tcp"}, Username: "user", Credential: "password"})},
		{"missing password", 201, "application/json", badServers(ICEServer{URLs: []string{cloudflareTURNURL}, Username: "user"})},
		{"control character", 201, "application/json", badServers(ICEServer{URLs: []string{cloudflareTURNURL}, Username: "user\n", Credential: "password"})},
		{"duplicate relay", 201, "application/json", badServers(ICEServer{URLs: []string{cloudflareTURNURL, cloudflareTURNURL}, Username: "user", Credential: "password"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			provider := newCloudflareTURNProvider(turnTestConfig(), &http.Client{Transport: turnRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				response := turnTestResponse(tc.status, tc.body)
				response.Header.Set("Content-Type", tc.contentType)
				response.Header.Set("Location", "https://other.invalid/steal")
				return response, nil
			})})
			_, err := provider.resolve(context.Background())
			require.Error(t, err)
			require.NotContains(t, err.Error(), turnTestConfig().APIToken)
			require.Equal(t, 1, calls, "must not follow redirects")
		})
	}
}

func TestCloudflareTURNCoalescesRefreshAndWaitersCanCancel(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	provider := newCloudflareTURNProvider(turnTestConfig(), &http.Client{Transport: turnRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-release:
		}
		return turnTestResponse(201, turnTestJSON("temporary-password")), nil
	})})
	owner := make(chan error, 1)
	go func() { _, err := provider.resolve(context.Background()); owner <- err }()
	<-started
	waitCtx, cancelWait := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { _, err := provider.resolve(waitCtx); waiter <- err }()
	cancelWait()
	require.ErrorIs(t, <-waiter, context.Canceled)
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := provider.resolve(context.Background()); results <- err }()
	}
	close(release)
	require.NoError(t, <-owner)
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestCloudflareTURNRequestCancellationAndDeadline(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	provider := newCloudflareTURNProvider(turnTestConfig(), &http.Client{Transport: turnRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), cloudflareRequestTimeout)
		close(started)
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-release:
		}
		return turnTestResponse(201, turnTestJSON("temporary-password")), nil
	})})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := provider.resolve(ctx); done <- err }()
	<-started
	// The original caller may leave without canceling a shared renewal that
	// another live caller still needs.
	waiter := make(chan error, 1)
	go func() { _, err := provider.resolve(context.Background()); waiter <- err }()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	close(release)
	require.NoError(t, <-waiter)
}

func TestICEStaticAndCoturnRemainAvailableWithoutCloudflare(t *testing.T) {
	manager := &Manager{config: &config.CallingConfig{ICEServers: []config.ICEServerConfig{
		{URLs: []string{"stun:example.invalid:3478"}},
		{URLs: []string{"turn:example.invalid:3478"}, Username: "static-user", Credential: "static-password"},
		{URLs: []string{"turn:coturn.invalid:3478"}, Secret: "synthetic-shared-secret", CredentialTTL: 3600},
	}}}
	result, err := manager.ResolveICEConfiguration(context.Background())
	require.NoError(t, err)
	require.Nil(t, result.ExpiresAt)
	require.Equal(t, "all", result.TransportPolicy)
	require.Len(t, result.Servers, 3)
	require.Equal(t, "static-password", result.Servers[1].Credential)
	require.NotEmpty(t, result.Servers[2].Credential)
	require.NotEqual(t, "synthetic-shared-secret", result.Servers[2].Credential)
	public, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(public), "synthetic-shared-secret")
}
