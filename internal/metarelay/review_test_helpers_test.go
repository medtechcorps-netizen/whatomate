package metarelay

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/metareview"
)

func newReviewTestConfig(t *testing.T) *Config {
	t.Helper()
	config, err := loadTestEnvironment(validReviewConfigEnvironment())
	if err != nil {
		t.Fatalf("load review test config: %v", err)
	}
	return config
}

func validReviewBinding(config *Config, now time.Time) ReviewBinding {
	messengerAppSecretKeyID, err := metareview.MessengerAppSecretKeyID(config.MessengerAppSecret)
	if err != nil {
		panic(err)
	}
	providerProofSecretKeyID, err := metareview.ProviderProofSecretKeyID(
		config.ReReplyProviderProofSecret,
	)
	if err != nil {
		panic(err)
	}
	return ReviewBinding{
		Tuple:                    reviewTupleFromConfig(config),
		MessengerAppSecretKeyID:  messengerAppSecretKeyID,
		ProviderProofSecretKeyID: providerProofSecretKeyID,
		CredentialID:             "77777777-7777-4777-8777-777777777777",
		CredentialVersion:        3,
		ReReplyWebhookURL: "https://staging.rereply.example.test/api/webhooks/channels/" +
			config.ReviewChannelAccountID,
		InboundSecret: "review-inbound-secret-at-least-thirty-two-bytes",
		ExpiresAt:     now.UTC().Add(2 * time.Minute),
	}
}

type fakeReviewResolver struct {
	mu      sync.Mutex
	binding ReviewBinding
	err     error
	calls   int
}

func (r *fakeReviewResolver) ResolveReviewBinding(context.Context) (ReviewBinding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.binding, r.err
}

func (r *fakeReviewResolver) resolveReviewBindingFresh(context.Context) (ReviewBinding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.binding, r.err
}

func (r *fakeReviewResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}
