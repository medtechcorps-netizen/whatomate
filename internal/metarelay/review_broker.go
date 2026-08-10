package metarelay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/shridarpatil/whatomate/internal/metareview"
	"github.com/shridarpatil/whatomate/internal/models"
)

const reviewBrokerTimeout = 2 * time.Second

// ReviewBrokerClient obtains one encrypted inbound-only binding from ReReply.
// A client constructed with a zero cache TTL always revalidates over the
// server-to-server broker boundary and is intended for queue workers.
type ReviewBrokerClient struct {
	config   *Config
	protocol *metareview.Protocol
	endpoint string
	client   *http.Client
	now      func() time.Time
	cacheTTL time.Duration

	cacheMu      sync.Mutex
	cached       ReviewBinding
	cacheExpires time.Time
}

type ReviewBrokerOption func(*ReviewBrokerClient)

func withReviewBrokerHTTPClient(client *http.Client) ReviewBrokerOption {
	return func(broker *ReviewBrokerClient) {
		if client != nil {
			broker.client = client
		}
	}
}

func withReviewBrokerNow(now func() time.Time) ReviewBrokerOption {
	return func(broker *ReviewBrokerClient) {
		if now != nil {
			broker.now = now
		}
	}
}

func NewReviewBrokerClient(
	config *Config,
	cacheTTL time.Duration,
	options ...ReviewBrokerOption,
) (*ReviewBrokerClient, error) {
	if config == nil || !config.stagingMessengerReview() ||
		config.DeploymentEnvironment != "staging" {
		return nil, errors.New("staging Messenger review config is required")
	}
	if cacheTTL < 0 || cacheTTL > maximumReviewCacheTTL {
		return nil, errors.New("review broker cache TTL is invalid")
	}
	protocol, err := metareview.NewProtocol(
		config.ReviewBrokerAuthSecret,
		config.ReviewBundleEncryptionSecret,
	)
	if err != nil {
		return nil, errors.New("review broker protocol configuration is invalid")
	}
	base, err := validateReReplyBaseURL(
		config.ReReplyBaseURL,
		config.allowInsecureTestEndpoints,
	)
	if err != nil {
		return nil, errors.New("review broker ReReply origin is invalid")
	}
	endpoint := *base
	endpoint.Path = metareview.ProvisionPath
	broker := &ReviewBrokerClient{
		config:   config,
		protocol: protocol,
		endpoint: endpoint.String(),
		client: newReviewEndpointHTTPClient(
			reviewBrokerTimeout,
			config.allowInsecureTestEndpoints,
		),
		now:      time.Now,
		cacheTTL: cacheTTL,
	}
	for _, option := range options {
		option(broker)
	}
	return broker, nil
}

func (b *ReviewBrokerClient) ResolveReviewBinding(ctx context.Context) (ReviewBinding, error) {
	if b == nil {
		return ReviewBinding{}, ErrReviewBindingUnavailable
	}
	now := b.now().UTC()
	if binding, ok := b.loadCached(now); ok {
		return binding, nil
	}
	binding, err := b.fetch(ctx, now)
	if err != nil {
		return ReviewBinding{}, err
	}
	b.storeCached(binding, now)
	return binding, nil
}

// resolveReviewBindingFresh bypasses and replaces the ingress cache for the
// operator-facing binding attestation. A deprovision or broker rejection must
// make /reviewz fail immediately rather than remain hidden by the short ingress
// cache.
func (b *ReviewBrokerClient) resolveReviewBindingFresh(ctx context.Context) (ReviewBinding, error) {
	if b == nil {
		return ReviewBinding{}, ErrReviewBindingUnavailable
	}
	now := b.now().UTC()
	b.clearCached()
	binding, err := b.fetch(ctx, now)
	if err != nil {
		return ReviewBinding{}, err
	}
	b.storeCached(binding, now)
	return binding, nil
}

func (b *ReviewBrokerClient) fetch(ctx context.Context, now time.Time) (ReviewBinding, error) {
	tuple := reviewTupleFromConfig(b.config)
	messengerAppSecretKeyID, providerProofSecretKeyID, err := reviewSharedSecretKeyIDs(b.config)
	if err != nil {
		return ReviewBinding{}, ErrReviewBindingRejected
	}
	requestBody, err := metareview.EncodeProvisionRequest(metareview.ProvisionRequest{
		Version:                  metareview.Version,
		Mode:                     metareview.Mode,
		Tuple:                    tuple,
		MessengerAppSecretKeyID:  messengerAppSecretKeyID,
		ProviderProofSecretKeyID: providerProofSecretKeyID,
	}, now)
	if err != nil {
		return ReviewBinding{}, ErrReviewBindingRejected
	}
	nonce, err := metareview.NewNonce()
	if err != nil {
		return ReviewBinding{}, ErrReviewBindingUnavailable
	}
	timestamp := now.Unix()
	signature, err := b.protocol.SignProvisionRequest(timestamp, nonce, requestBody)
	if err != nil {
		return ReviewBinding{}, ErrReviewBindingRejected
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		b.endpoint,
		bytes.NewReader(requestBody),
	)
	if err != nil {
		return ReviewBinding{}, ErrReviewBindingRejected
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-store")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Pragma", "no-cache")
	request.Header.Set("User-Agent", "ReReply-Meta-Relay-Review/1.0")
	request.Header.Set(metareview.TimestampHeader, strconv.FormatInt(timestamp, 10))
	request.Header.Set(metareview.NonceHeader, nonce)
	request.Header.Set(metareview.SignatureHeader, signature)

	response, err := b.client.Do(request)
	if err != nil {
		return ReviewBinding{}, ErrReviewBindingUnavailable
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode == http.StatusTooManyRequests ||
		response.StatusCode >= http.StatusInternalServerError {
		return ReviewBinding{}, ErrReviewBindingUnavailable
	}
	if response.StatusCode != http.StatusOK {
		return ReviewBinding{}, ErrReviewBindingRejected
	}
	encoded, err := io.ReadAll(io.LimitReader(
		response.Body,
		metareview.MaximumResponseBodyBytes+1,
	))
	if err != nil || len(encoded) > metareview.MaximumResponseBodyBytes {
		return ReviewBinding{}, ErrReviewBindingUnavailable
	}
	provisionResponse, err := metareview.DecodeProvisionResponse(encoded, now)
	if err != nil {
		return ReviewBinding{}, ErrReviewBindingUnavailable
	}
	bundle, err := b.protocol.OpenCredentialBundle(
		requestBody,
		timestamp,
		nonce,
		provisionResponse,
		now,
	)
	if err != nil {
		return ReviewBinding{}, ErrReviewBindingUnavailable
	}
	if bundle.Tuple != tuple {
		return ReviewBinding{}, ErrReviewBindingRejected
	}
	base, err := validateReReplyBaseURL(
		b.config.ReReplyBaseURL,
		b.config.allowInsecureTestEndpoints,
	)
	if err != nil || bundle.ReReplyWebhookURL != exactReReplyWebhookURL(base, tuple.ChannelAccountID) {
		return ReviewBinding{}, ErrReviewBindingRejected
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, bundle.ExpiresAt)
	if err != nil || !expiresAt.After(now) {
		return ReviewBinding{}, ErrReviewBindingRejected
	}
	return ReviewBinding{
		Tuple:                    bundle.Tuple,
		MessengerAppSecretKeyID:  bundle.MessengerAppSecretKeyID,
		ProviderProofSecretKeyID: bundle.ProviderProofSecretKeyID,
		CredentialID:             bundle.CredentialID,
		CredentialVersion:        bundle.CredentialVersion,
		ReReplyWebhookURL:        bundle.ReReplyWebhookURL,
		InboundSecret:            bundle.InboundSecret,
		ExpiresAt:                expiresAt.UTC(),
	}, nil
}

func reviewSharedSecretKeyIDs(config *Config) (string, string, error) {
	if config == nil {
		return "", "", ErrReviewBindingRejected
	}
	messengerAppSecretKeyID, err := metareview.MessengerAppSecretKeyID(config.MessengerAppSecret)
	if err != nil {
		return "", "", ErrReviewBindingRejected
	}
	providerProofSecretKeyID, err := metareview.ProviderProofSecretKeyID(
		config.ReReplyProviderProofSecret,
	)
	if err != nil {
		return "", "", ErrReviewBindingRejected
	}
	return messengerAppSecretKeyID, providerProofSecretKeyID, nil
}

func reviewTupleFromConfig(config *Config) metareview.ProvisionTuple {
	if config == nil {
		return metareview.ProvisionTuple{}
	}
	return metareview.ProvisionTuple{
		OrganizationID:   config.ReviewOrganizationID,
		MetaBusinessID:   config.ReviewMetaBusinessID,
		PageID:           config.ReviewPageID,
		MetaAppID:        config.MessengerAppID,
		ChannelAccountID: config.ReviewChannelAccountID,
		Generation:       config.ReviewGeneration,
		ExpiresAt:        config.ReviewExpiresAt,
	}
}

func (b *ReviewBrokerClient) loadCached(now time.Time) (ReviewBinding, bool) {
	if b.cacheTTL <= 0 {
		return ReviewBinding{}, false
	}
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	if b.cacheExpires.IsZero() || !now.Before(b.cacheExpires) {
		b.cached = ReviewBinding{}
		b.cacheExpires = time.Time{}
		return ReviewBinding{}, false
	}
	return b.cached, true
}

func (b *ReviewBrokerClient) storeCached(binding ReviewBinding, now time.Time) {
	if b.cacheTTL <= 0 {
		return
	}
	expiresAt := now.Add(b.cacheTTL)
	if binding.ExpiresAt.Before(expiresAt) {
		expiresAt = binding.ExpiresAt
	}
	b.cacheMu.Lock()
	b.cached = binding
	b.cacheExpires = expiresAt
	b.cacheMu.Unlock()
}

func (b *ReviewBrokerClient) clearCached() {
	b.cacheMu.Lock()
	b.cached = ReviewBinding{}
	b.cacheExpires = time.Time{}
	b.cacheMu.Unlock()
}

func (b ReviewBinding) account() (*AccountConfig, error) {
	if b.Tuple.ChannelAccountID == "" || b.Tuple.PageID == "" ||
		b.ReReplyWebhookURL == "" || b.InboundSecret == "" ||
		b.MessengerAppSecretKeyID == "" || b.ProviderProofSecretKeyID == "" ||
		b.CredentialID == "" || b.CredentialVersion <= 0 {
		return nil, ErrReviewBindingRejected
	}
	return &AccountConfig{
		Key:                     b.Tuple.ChannelAccountID,
		OrganizationID:          b.Tuple.OrganizationID,
		MetaBusinessID:          b.Tuple.MetaBusinessID,
		Channel:                 models.ChannelMessenger,
		ExternalAccountID:       b.Tuple.PageID,
		ReReplyWebhookURL:       b.ReReplyWebhookURL,
		reReplyInboundSecret:    b.InboundSecret,
		reReplyChannelAccountID: b.Tuple.ChannelAccountID,
	}, nil
}

func reviewBindingMatchesJob(binding ReviewBinding, job InboundJob) bool {
	return binding.Tuple.ChannelAccountID == job.AccountKey &&
		binding.Tuple.Generation == job.ReviewGeneration &&
		binding.CredentialID == job.ReviewCredentialID &&
		binding.CredentialVersion == job.ReviewCredentialVersion
}

func validateReviewBinding(config *Config, binding ReviewBinding, now time.Time) error {
	if config == nil || !config.stagingMessengerReview() ||
		config.DeploymentEnvironment != "staging" ||
		binding.Tuple != reviewTupleFromConfig(config) {
		return ErrReviewBindingRejected
	}
	messengerAppSecretKeyID, providerProofSecretKeyID, err := reviewSharedSecretKeyIDs(config)
	if err != nil || binding.MessengerAppSecretKeyID != messengerAppSecretKeyID ||
		binding.ProviderProofSecretKeyID != providerProofSecretKeyID {
		return ErrReviewBindingRejected
	}
	if _, err := canonicalOrganizationID(binding.CredentialID); err != nil ||
		binding.CredentialVersion <= 0 ||
		!binding.ExpiresAt.After(now.UTC()) ||
		binding.ExpiresAt.Sub(now.UTC()) > metareview.MaximumBundleTTL {
		return ErrReviewBindingRejected
	}
	authorityExpiry, err := time.Parse(time.RFC3339Nano, binding.Tuple.ExpiresAt)
	if err != nil || binding.ExpiresAt.After(authorityExpiry) {
		return ErrReviewBindingRejected
	}
	if len([]byte(binding.InboundSecret)) > 4<<10 ||
		validateReviewRootSecret("review inbound HMAC secret", binding.InboundSecret) != nil {
		return ErrReviewBindingRejected
	}
	base, err := validateReReplyBaseURL(
		config.ReReplyBaseURL,
		config.allowInsecureTestEndpoints,
	)
	if err != nil || binding.ReReplyWebhookURL != exactReReplyWebhookURL(
		base,
		binding.Tuple.ChannelAccountID,
	) {
		return ErrReviewBindingRejected
	}
	return nil
}

func reviewBindingErrorStatus(err error) (int, string) {
	if reviewBindingUnavailable(err) {
		return http.StatusServiceUnavailable, "review_binding_unavailable"
	}
	return http.StatusNotFound, "review_binding_rejected"
}

func reviewBindingUnavailable(err error) bool {
	return errors.Is(err, ErrReviewBindingUnavailable) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}
