package calling

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
)

const (
	cloudflareTURNURL        = "turns:turn.cloudflare.com:443?transport=tcp"
	cloudflareCredentialTTL  = 24 * time.Hour
	cloudflareCacheLifetime  = time.Hour
	cloudflareRequestTimeout = 5 * time.Second
	cloudflareResponseLimit  = 64 * 1024
)

// ICEServer contains only the short-lived client credentials, never the token
// used to obtain them. Static and coturn credentials retain their existing form.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type ICEConfiguration struct {
	Servers         []ICEServer `json:"ice_servers"`
	ExpiresAt       *time.Time  `json:"expires_at,omitempty"`
	TransportPolicy string      `json:"ice_transport_policy"`
}

type turnBundle struct {
	server    ICEServer
	issuedAt  time.Time
	expiresAt time.Time
}

type turnRefresh struct {
	done   chan struct{}
	bundle turnBundle
	err    error
}

// cloudflareTURNProvider shares one refresh between concurrent new calls. It
// does not change already-running peer connections or persist credentials.
type cloudflareTURNProvider struct {
	config  config.CloudflareTURNConfig
	client  *http.Client
	now     func() time.Time
	mu      sync.Mutex
	cached  turnBundle
	refresh *turnRefresh
}

func newCloudflareTURNProvider(cfg config.CloudflareTURNConfig, client *http.Client) *cloudflareTURNProvider {
	var bounded *http.Client
	if client != nil {
		// Reuse the application's SSRF-safe transport, but not cookies, redirect
		// behavior, or its longer timeout. The endpoint is never configurable.
		bounded = &http.Client{
			Transport:     client.Transport,
			Timeout:       cloudflareRequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &cloudflareTURNProvider{config: cfg, client: bounded, now: time.Now}
}

func (p *cloudflareTURNProvider) resolve(ctx context.Context) (turnBundle, error) {
	if err := ctx.Err(); err != nil {
		return turnBundle{}, err
	}
	p.mu.Lock()
	now := p.now()
	if !p.cached.issuedAt.IsZero() && !now.Before(p.cached.issuedAt) && now.Before(p.cached.issuedAt.Add(cloudflareCacheLifetime)) && now.Before(p.cached.expiresAt) {
		bundle := cloneTurnBundle(p.cached)
		p.mu.Unlock()
		return bundle, nil
	}
	pending := p.refresh
	if pending == nil {
		pending = &turnRefresh{done: make(chan struct{})}
		p.refresh = pending
		// A canceled caller must not cancel credentials needed by another call.
		// The refresh owns its five-second deadline; each caller can stop waiting.
		go p.completeRefresh(pending)
	}
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return turnBundle{}, ctx.Err()
	case <-pending.done:
		if err := ctx.Err(); err != nil {
			return turnBundle{}, err
		}
		return cloneTurnBundle(pending.bundle), pending.err
	}
}

func (p *cloudflareTURNProvider) completeRefresh(pending *turnRefresh) {
	bundle, err := p.fetch(context.Background())
	p.mu.Lock()
	if err == nil {
		p.cached = bundle
	}
	pending.bundle, pending.err = bundle, err
	p.refresh = nil
	close(pending.done)
	p.mu.Unlock()
}

func cloneTurnBundle(bundle turnBundle) turnBundle {
	bundle.server.URLs = append([]string(nil), bundle.server.URLs...)
	return bundle
}

func (p *cloudflareTURNProvider) fetch(ctx context.Context) (turnBundle, error) {
	if err := p.config.Validate(); err != nil {
		return turnBundle{}, err
	}
	if p.client == nil {
		return turnBundle{}, errors.New("TURN credential HTTP client is unavailable")
	}
	issued := p.now()
	ctx, cancel := context.WithTimeout(ctx, cloudflareRequestTimeout)
	defer cancel()
	endpoint := "https://rtc.live.cloudflare.com/v1/turn/keys/" + p.config.KeyID + "/credentials/generate-ice-servers"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(`{"ttl":86400}`))
	if err != nil {
		return turnBundle{}, errors.New("TURN credential request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+p.config.APIToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ReReply-TURN/1.0")
	response, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return turnBundle{}, ctx.Err()
		}
		// Transport errors may contain URLs or request details. Never propagate
		// them to the call log, public error envelope, or application logger.
		return turnBundle{}, errors.New("TURN credential service request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return turnBundle{}, errors.New("TURN credential service did not create credentials")
	}
	if strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]) != "application/json" {
		return turnBundle{}, errors.New("TURN credential service returned an invalid content type")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, cloudflareResponseLimit+1))
	if err != nil || len(raw) > cloudflareResponseLimit {
		return turnBundle{}, errors.New("TURN credential service returned an invalid response size")
	}
	var payload struct {
		Servers []ICEServer `json:"iceServers"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return turnBundle{}, errors.New("TURN credential service returned invalid JSON")
	}
	var selected *ICEServer
	for _, server := range payload.Servers {
		for _, address := range server.URLs {
			if address != cloudflareTURNURL {
				continue
			}
			if selected != nil || !validTURNCredential(server.Username) || !validTURNCredential(server.Credential) {
				return turnBundle{}, errors.New("TURN credential service returned invalid credentials")
			}
			selected = &ICEServer{URLs: []string{cloudflareTURNURL}, Username: server.Username, Credential: server.Credential}
		}
	}
	if selected == nil {
		return turnBundle{}, errors.New("TURN credential service omitted the required relay")
	}
	if err := ctx.Err(); err != nil {
		return turnBundle{}, err
	}
	return turnBundle{server: *selected, issuedAt: issued, expiresAt: issued.Add(cloudflareCredentialTTL)}, nil
}

func validTURNCredential(value string) bool {
	if len(value) < 1 || len(value) > 4096 {
		return false
	}
	for _, char := range value {
		if char < 33 || char > 126 {
			return false
		}
	}
	return true
}

// ResolveICEConfiguration is shared by Pion and the authenticated browser
// endpoint. A fresh bundle is required for every new connection; existing calls
// are not renegotiated when a cached bundle is replaced.
func (m *Manager) ResolveICEConfiguration(ctx context.Context) (ICEConfiguration, error) {
	if err := ctx.Err(); err != nil {
		return ICEConfiguration{}, err
	}
	if m.config == nil {
		return ICEConfiguration{}, errors.New("Calling is not configured")
	}
	result := ICEConfiguration{Servers: make([]ICEServer, 0), TransportPolicy: "all"}
	if m.config.RelayOnly {
		result.TransportPolicy = "relay"
	}
	if m.config.CloudflareTURN.Configured() {
		m.iceProviderOnce.Do(func() { m.iceProvider = newCloudflareTURNProvider(m.config.CloudflareTURN, m.httpClient) })
		bundle, err := m.iceProvider.resolve(ctx)
		if err != nil {
			return ICEConfiguration{}, err
		}
		result.Servers = append(result.Servers, bundle.server)
		result.ExpiresAt = &bundle.expiresAt
		return result, nil
	}
	now := time.Now()
	for _, server := range m.config.ICEServers {
		username, credential := server.ResolveCredentials(now)
		result.Servers = append(result.Servers, ICEServer{URLs: append([]string(nil), server.URLs...), Username: username, Credential: credential})
	}
	return result, nil
}
