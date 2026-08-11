package metarelay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/metareview"
)

func TestReviewBrokerAuthenticatesAndOpensOnlyExactInboundBinding(t *testing.T) {
	config := newReviewTestConfig(t)
	now := time.Now().UTC().Truncate(time.Second)
	config.ReviewExpiresAt = now.Add(time.Hour).Format(time.RFC3339Nano)
	protocol, err := metareview.NewProtocol(
		config.ReviewBrokerAuthSecret,
		config.ReviewBundleEncryptionSecret,
	)
	if err != nil {
		t.Fatalf("new test protocol: %v", err)
	}
	binding := validReviewBinding(config, now)
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodPost || request.URL.Path != metareview.ProvisionPath {
			t.Errorf("unexpected broker request: %s %s", request.Method, request.URL.Path)
		}
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Errorf("read request: %v", readErr)
		}
		unix, parseErr := strconv.ParseInt(request.Header.Get(metareview.TimestampHeader), 10, 64)
		if parseErr != nil {
			t.Errorf("parse timestamp: %v", parseErr)
		}
		nonce := request.Header.Get(metareview.NonceHeader)
		if verifyErr := protocol.VerifyProvisionRequest(
			unix,
			nonce,
			body,
			request.Header.Get(metareview.SignatureHeader),
			now,
		); verifyErr != nil {
			t.Errorf("verify provision request: %v", verifyErr)
		}
		decoded, decodeErr := metareview.DecodeProvisionRequest(body, now)
		if decodeErr != nil {
			t.Errorf("decode request: %v", decodeErr)
		}
		if decoded.Tuple != binding.Tuple {
			t.Errorf("request tuple = %+v, want %+v", decoded.Tuple, binding.Tuple)
		}
		if decoded.MessengerAppSecretKeyID != binding.MessengerAppSecretKeyID ||
			decoded.ProviderProofSecretKeyID != binding.ProviderProofSecretKeyID {
			t.Errorf("request shared-secret attestations differ: %+v", decoded)
		}
		response, sealErr := protocol.SealCredentialBundle(
			body,
			unix,
			nonce,
			metareview.CredentialBundle{
				MessengerAppSecretKeyID:  binding.MessengerAppSecretKeyID,
				ProviderProofSecretKeyID: binding.ProviderProofSecretKeyID,
				CredentialID:             binding.CredentialID,
				CredentialVersion:        binding.CredentialVersion,
				ReReplyWebhookURL:        binding.ReReplyWebhookURL,
				InboundSecret:            binding.InboundSecret,
			},
			now,
			binding.ExpiresAt,
		)
		if sealErr != nil {
			t.Errorf("seal response: %v", sealErr)
		}
		encoded, marshalErr := json.Marshal(response)
		if marshalErr != nil {
			t.Errorf("encode response: %v", marshalErr)
		}
		if strings.Contains(string(encoded), binding.InboundSecret) {
			t.Error("broker response exposed the inbound secret as plaintext")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(string(encoded))),
		}, nil
	})}
	broker, err := NewReviewBrokerClient(
		config,
		config.ReviewBindingCacheTTL,
		withReviewBrokerHTTPClient(client),
		withReviewBrokerNow(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatalf("new broker client: %v", err)
	}
	resolved, err := broker.ResolveReviewBinding(context.Background())
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	if resolved.Tuple != binding.Tuple ||
		resolved.MessengerAppSecretKeyID != binding.MessengerAppSecretKeyID ||
		resolved.ProviderProofSecretKeyID != binding.ProviderProofSecretKeyID ||
		resolved.CredentialID != binding.CredentialID ||
		resolved.CredentialVersion != binding.CredentialVersion ||
		resolved.InboundSecret != binding.InboundSecret {
		t.Fatalf("resolved binding differs: %+v", resolved)
	}
	if _, err := broker.ResolveReviewBinding(context.Background()); err != nil {
		t.Fatalf("resolve cached binding: %v", err)
	}
	if calls != 1 {
		t.Fatalf("broker calls = %d, want one cached ingress call", calls)
	}
}

func TestReviewBrokerZeroCacheRevalidatesAndClassifiesFailures(t *testing.T) {
	config := newReviewTestConfig(t)
	for _, testCase := range []struct {
		name      string
		status    int
		transport error
		want      error
	}{
		{name: "rejected", status: http.StatusForbidden, want: ErrReviewBindingRejected},
		{name: "rate limited", status: http.StatusTooManyRequests, want: ErrReviewBindingUnavailable},
		{name: "server failure", status: http.StatusBadGateway, want: ErrReviewBindingUnavailable},
		{name: "transport failure", transport: errTestTransport, want: ErrReviewBindingUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if testCase.transport != nil {
					return nil, testCase.transport
				}
				return &http.Response{
					StatusCode: testCase.status,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{}`)),
				}, nil
			})}
			broker, err := NewReviewBrokerClient(
				config,
				0,
				withReviewBrokerHTTPClient(client),
			)
			if err != nil {
				t.Fatalf("new broker: %v", err)
			}
			_, err = broker.ResolveReviewBinding(context.Background())
			if !errors.Is(err, testCase.want) {
				t.Fatalf("resolve error = %v, want %v", err, testCase.want)
			}
		})
	}
}
