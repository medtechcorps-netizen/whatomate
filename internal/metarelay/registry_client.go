package metarelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
)

const registryResponseLimit = 64 << 10

var (
	ErrRegistryNotFound     = errors.New("dynamic Meta registry binding not found")
	ErrRegistryStale        = errors.New("dynamic Meta registry binding is stale")
	ErrRegistryUnauthorized = errors.New("dynamic Meta registry service authentication failed")
	ErrRegistryUnavailable  = errors.New("dynamic Meta registry unavailable")
)

type RegistryResolver interface {
	Resolve(ctx context.Context, channel models.Channel, externalID, purpose string, allowCache bool) (*AccountConfig, error)
}

type registryCacheEntry struct {
	account   AccountConfig
	expiresAt time.Time
}

type RegistryClient struct {
	endpoint              string
	secret                string
	client                *http.Client
	cacheTTL              time.Duration
	managedMessengerAppID string
	now                   func() time.Time
	mu                    sync.Mutex
	cache                 map[string]registryCacheEntry
}

func NewRegistryClient(config *Config, client *http.Client) (*RegistryClient, error) {
	if config == nil || strings.TrimSpace(config.RegistryURL) == "" {
		return nil, nil
	}
	if !config.RegistryEnabled {
		return nil, ErrRegistryUnavailable
	}
	if len(config.RegistrySecret) < 32 {
		return nil, ErrRegistryUnavailable
	}
	if strings.TrimSpace(config.ManagedMessengerAppID) == "" {
		return nil, ErrRegistryUnavailable
	}
	parsed, err := url.Parse(strings.TrimSpace(config.RegistryURL))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Opaque != "" || parsed.RawPath != "" ||
		parsed.Path != metaregistry.ResolvePath || parsed.EscapedPath() != metaregistry.ResolvePath ||
		(parsed.Scheme != "https" && (!config.allowInsecureTestEndpoints || parsed.Scheme != "http")) {
		return nil, ErrRegistryUnavailable
	}
	if client == nil {
		client = &http.Client{
			Timeout:       config.RegistryTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &RegistryClient{
		endpoint: strings.TrimSpace(config.RegistryURL), secret: config.RegistrySecret,
		client: client, cacheTTL: config.RegistryCacheTTL,
		managedMessengerAppID: strings.TrimSpace(config.ManagedMessengerAppID), now: time.Now,
		cache: make(map[string]registryCacheEntry),
	}, nil
}

func (client *RegistryClient) Resolve(ctx context.Context, channel models.Channel, externalID, purpose string, allowCache bool) (*AccountConfig, error) {
	requestValue, err := metaregistry.NormalizeResolveRequest(metaregistry.ResolveRequest{
		Channel: channel, ExternalAccountID: externalID, Purpose: purpose,
	})
	if err != nil {
		return nil, ErrRegistryNotFound
	}
	key := accountIndexKey(requestValue.Channel, requestValue.ExternalAccountID) + "\x00" + requestValue.Purpose
	now := client.now().UTC()
	if allowCache {
		client.mu.Lock()
		entry, ok := client.cache[key]
		client.mu.Unlock()
		if ok && entry.expiresAt.After(now) && entry.account.registryLeaseExpiresAt.After(now) {
			copy := entry.account
			return &copy, nil
		}
	}
	body, err := json.Marshal(requestValue)
	if err != nil {
		return nil, ErrRegistryUnavailable
	}
	nonce, err := metaregistry.NewNonce()
	if err != nil {
		return nil, ErrRegistryUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(body))
	if err != nil || request.URL.Path != metaregistry.ResolvePath {
		return nil, ErrRegistryUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ReReply-Meta-Relay/1.0")
	metaregistry.ApplyRequestHeaders(request, client.secret, now, nonce, body)
	response, err := client.client.Do(request)
	if err != nil {
		return nil, ErrRegistryUnavailable
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, registryResponseLimit))
		_ = response.Body.Close()
	}()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, registryResponseLimit+1))
	if err != nil || len(responseBody) > registryResponseLimit ||
		metaregistry.VerifyResponse(client.secret, nonce, response.StatusCode, responseBody,
			response.Header.Get(metaregistry.ResponseHeader)) != nil {
		return nil, ErrRegistryUnavailable
	}
	if response.StatusCode == http.StatusNotFound {
		client.mu.Lock()
		delete(client.cache, key)
		client.mu.Unlock()
		return nil, ErrRegistryNotFound
	}
	if response.StatusCode == http.StatusConflict {
		client.mu.Lock()
		delete(client.cache, key)
		client.mu.Unlock()
		return nil, ErrRegistryStale
	}
	if response.StatusCode != http.StatusOK {
		return nil, ErrRegistryUnavailable
	}
	var binding metaregistry.Binding
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&binding) != nil || rejectTrailingJSON(decoder) != nil || binding.Validate(now) != nil ||
		binding.Channel != requestValue.Channel || binding.ExternalAccountID != requestValue.ExternalAccountID ||
		(binding.Channel == models.ChannelMessenger && binding.PlatformAppID != client.managedMessengerAppID) {
		return nil, ErrRegistryUnavailable
	}
	account := accountConfigFromBinding(binding)
	expiresAt := now.Add(client.cacheTTL)
	if binding.LeaseExpiresAt.Before(expiresAt) {
		expiresAt = binding.LeaseExpiresAt
	}
	client.mu.Lock()
	client.cache[key] = registryCacheEntry{account: account, expiresAt: expiresAt}
	client.mu.Unlock()
	return &account, nil
}

func accountConfigFromBinding(binding metaregistry.Binding) AccountConfig {
	return AccountConfig{
		Key:     "registry." + binding.ChannelAccountID.String(),
		Channel: binding.Channel, ExternalAccountID: binding.ExternalAccountID,
		PlatformAppID:     binding.PlatformAppID,
		InstagramAPIMode:  InstagramAPIMode(binding.InstagramAPIMode),
		ReReplyWebhookURL: binding.ReReplyWebhookURL,
		accessToken:       binding.AccessToken, reReplyInboundSecret: binding.InboundSecret,
		reReplyOutboundSecret:     binding.OutboundSecret,
		registryCredentialID:      binding.CredentialID.String(),
		registryCredentialVersion: binding.CredentialVersion,
		registryWebhookID:         binding.WebhookCredentialID.String(),
		registryWebhookVersion:    binding.WebhookCredentialVersion,
		registryLeaseExpiresAt:    binding.LeaseExpiresAt.UTC(), registryManaged: true,
	}
}

func registryFence(account *AccountConfig) string {
	if account == nil || !account.registryManaged {
		return ""
	}
	return fmt.Sprintf("%s:%d:%s:%d", account.registryCredentialID,
		account.registryCredentialVersion, account.registryWebhookID, account.registryWebhookVersion)
}
