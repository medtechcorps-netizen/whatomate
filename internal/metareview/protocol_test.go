package metareview

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAuthSecret         = "review-broker-auth-secret-that-is-at-least-thirty-two-bytes"
	testWrapSecret         = "review-broker-wrap-secret-that-is-at-least-thirty-two-bytes"
	testProofSecret        = "review-provider-proof-secret-that-is-at-least-thirty-two-bytes"
	testMessengerAppSecret = "review-messenger-app-secret-that-is-at-least-thirty-two-bytes"
)

func TestProtocolRoundTripBindsExactRequestAndContainsOnlyInboundCredential(t *testing.T) {
	now := time.Date(2026, 8, 10, 8, 30, 0, 0, time.UTC)
	protocol, err := NewProtocol(testAuthSecret, testWrapSecret)
	require.NoError(t, err)
	request := validProvisionRequest(now)
	body, err := EncodeProvisionRequest(request, now)
	require.NoError(t, err)
	assert.NotContains(t, string(body), testMessengerAppSecret)
	assert.NotContains(t, string(body), testProofSecret)
	nonce, err := NewNonce()
	require.NoError(t, err)
	signature, err := protocol.SignProvisionRequest(now.Unix(), nonce, body)
	require.NoError(t, err)
	require.NoError(t, protocol.VerifyProvisionRequest(
		now.Unix(), nonce, body, signature, now.Add(30*time.Second),
	))

	inboundSecret := "inbound-review-secret-material-that-never-leaves-the-envelope"
	bundle := CredentialBundle{
		MessengerAppSecretKeyID:  request.MessengerAppSecretKeyID,
		ProviderProofSecretKeyID: request.ProviderProofSecretKeyID,
		CredentialID:             uuid.NewString(),
		CredentialVersion:        7,
		ReReplyWebhookURL: "https://staging.rereply.example/api/webhooks/channels/" +
			request.Tuple.ChannelAccountID,
		InboundSecret: inboundSecret,
	}
	response, err := protocol.SealCredentialBundle(
		body,
		now.Unix(),
		nonce,
		bundle,
		now,
		now.Add(2*time.Minute),
	)
	require.NoError(t, err)

	encodedResponse, err := json.Marshal(response)
	require.NoError(t, err)
	assert.NotContains(t, string(encodedResponse), inboundSecret)
	assert.NotContains(t, string(encodedResponse), "page-access-token")
	assert.NotContains(t, string(encodedResponse), "outbound-secret")
	assert.Equal(t, protocol.KeyID(), response.KeyID)

	decodedResponse, err := DecodeProvisionResponse(encodedResponse, now.Add(time.Minute))
	require.NoError(t, err)
	opened, err := protocol.OpenCredentialBundle(
		body,
		now.Unix(),
		nonce,
		decodedResponse,
		now.Add(time.Minute),
	)
	require.NoError(t, err)
	assert.Equal(t, request.Tuple, opened.Tuple)
	assert.Equal(t, request.MessengerAppSecretKeyID, opened.MessengerAppSecretKeyID)
	assert.Equal(t, request.ProviderProofSecretKeyID, opened.ProviderProofSecretKeyID)
	assert.Equal(t, inboundSecret, opened.InboundSecret)
	assert.Equal(t, bundle.CredentialID+":7", opened.CredentialGeneration())
	assert.Equal(t, nonce, opened.RequestNonce)
}

func TestProvisionRequestAuthenticationRejectsTamperingReplayInputsAndStaleTime(t *testing.T) {
	now := time.Date(2026, 8, 10, 8, 30, 0, 0, time.UTC)
	protocol, err := NewProtocol(testAuthSecret, testWrapSecret)
	require.NoError(t, err)
	body, err := EncodeProvisionRequest(validProvisionRequest(now), now)
	require.NoError(t, err)
	nonce, err := NewNonce()
	require.NoError(t, err)
	signature, err := protocol.SignProvisionRequest(now.Unix(), nonce, body)
	require.NoError(t, err)

	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-2] ^= 1
	assert.ErrorIs(t, protocol.VerifyProvisionRequest(
		now.Unix(), nonce, tampered, signature, now,
	), ErrInvalidSignature)
	assert.ErrorIs(t, protocol.VerifyProvisionRequest(
		now.Unix(), nonce, body, "sha256="+strings.Repeat("0", 64), now,
	), ErrInvalidSignature)
	assert.ErrorIs(t, protocol.VerifyProvisionRequest(
		now.Unix(), nonce, body, signature, now.Add(MaximumRequestSkew+time.Second),
	), ErrRequestExpired)

	replayKey, err := ReplayKey(nonce)
	require.NoError(t, err)
	assert.NotContains(t, replayKey, nonce)
	assert.Equal(t, replayKey, mustReplayKey(t, nonce))
	assert.ErrorIs(t, protocol.VerifyProvisionRequest(
		now.Unix(), "not-a-canonical-nonce", body, signature, now,
	), ErrInvalidRequest)
}

func TestCredentialBundleAEADRejectsEveryAuthorityMutation(t *testing.T) {
	now := time.Date(2026, 8, 10, 8, 30, 0, 0, time.UTC)
	protocol, err := NewProtocol(testAuthSecret, testWrapSecret)
	require.NoError(t, err)
	request := validProvisionRequest(now)
	body, err := EncodeProvisionRequest(request, now)
	require.NoError(t, err)
	nonce, err := NewNonce()
	require.NoError(t, err)
	response, err := protocol.SealCredentialBundle(
		body,
		now.Unix(),
		nonce,
		CredentialBundle{
			MessengerAppSecretKeyID:  request.MessengerAppSecretKeyID,
			ProviderProofSecretKeyID: request.ProviderProofSecretKeyID,
			CredentialID:             uuid.NewString(),
			CredentialVersion:        1,
			ReReplyWebhookURL: "https://staging.rereply.example/api/webhooks/channels/" +
				request.Tuple.ChannelAccountID,
			InboundSecret: "one-exact-inbound-signing-secret-with-enough-bytes",
		},
		now,
		now.Add(time.Minute),
	)
	require.NoError(t, err)

	t.Run("request body", func(t *testing.T) {
		other := request
		other.Tuple.PageID = "999999999999999"
		otherBody, encodeErr := EncodeProvisionRequest(other, now)
		require.NoError(t, encodeErr)
		_, openErr := protocol.OpenCredentialBundle(
			otherBody, now.Unix(), nonce, response, now.Add(30*time.Second),
		)
		assert.ErrorIs(t, openErr, ErrInvalidResponse)
	})
	t.Run("shared secret key ID", func(t *testing.T) {
		other := request
		other.MessengerAppSecretKeyID = mustSharedSecretKeyID(
			MessengerAppSecretKeyID,
			testMessengerAppSecret+"-different",
		)
		otherBody, encodeErr := EncodeProvisionRequest(other, now)
		require.NoError(t, encodeErr)
		_, openErr := protocol.OpenCredentialBundle(
			otherBody, now.Unix(), nonce, response, now.Add(30*time.Second),
		)
		assert.ErrorIs(t, openErr, ErrInvalidResponse)
	})
	t.Run("request nonce", func(t *testing.T) {
		otherNonce, nonceErr := NewNonce()
		require.NoError(t, nonceErr)
		_, openErr := protocol.OpenCredentialBundle(
			body, now.Unix(), otherNonce, response, now.Add(30*time.Second),
		)
		assert.ErrorIs(t, openErr, ErrInvalidResponse)
	})
	t.Run("response expiry", func(t *testing.T) {
		changed := response
		changed.ExpiresAt = now.Add(90 * time.Second).Format(time.RFC3339Nano)
		_, openErr := protocol.OpenCredentialBundle(
			body, now.Unix(), nonce, changed, now.Add(30*time.Second),
		)
		assert.ErrorIs(t, openErr, ErrInvalidResponse)
	})
	t.Run("ciphertext", func(t *testing.T) {
		changed := response
		last := len(changed.Envelope) - 1
		if changed.Envelope[last] == 'A' {
			changed.Envelope = changed.Envelope[:last] + "B"
		} else {
			changed.Envelope = changed.Envelope[:last] + "A"
		}
		_, openErr := protocol.OpenCredentialBundle(
			body, now.Unix(), nonce, changed, now.Add(30*time.Second),
		)
		assert.ErrorIs(t, openErr, ErrInvalidResponse)
	})
	assert.ErrorIs(t, func() error {
		_, openErr := protocol.OpenCredentialBundle(
			body, now.Unix(), nonce, response, now.Add(2*time.Minute),
		)
		return openErr
	}(), ErrInvalidResponse)
}

func TestOpenCredentialBundleRejectsSharedSecretAttestationMismatch(t *testing.T) {
	now := time.Date(2026, 8, 10, 8, 30, 0, 0, time.UTC)
	protocol, err := NewProtocol(testAuthSecret, testWrapSecret)
	require.NoError(t, err)
	request := validProvisionRequest(now)
	body, err := EncodeProvisionRequest(request, now)
	require.NoError(t, err)

	for _, testCase := range []struct {
		name   string
		mutate func(*CredentialBundle)
	}{
		{
			name: "Messenger App Secret",
			mutate: func(bundle *CredentialBundle) {
				bundle.MessengerAppSecretKeyID = mustSharedSecretKeyID(
					MessengerAppSecretKeyID,
					testMessengerAppSecret+"-different",
				)
			},
		},
		{
			name: "provider-proof secret",
			mutate: func(bundle *CredentialBundle) {
				bundle.ProviderProofSecretKeyID = mustSharedSecretKeyID(
					ProviderProofSecretKeyID,
					testProofSecret+"-different",
				)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			nonce, err := NewNonce()
			require.NoError(t, err)
			bundle := CredentialBundle{
				MessengerAppSecretKeyID:  request.MessengerAppSecretKeyID,
				ProviderProofSecretKeyID: request.ProviderProofSecretKeyID,
				CredentialID:             uuid.NewString(),
				CredentialVersion:        1,
				ReReplyWebhookURL: "https://staging.rereply.example/api/webhooks/channels/" +
					request.Tuple.ChannelAccountID,
				InboundSecret: "one-exact-inbound-signing-secret-with-enough-bytes",
			}
			testCase.mutate(&bundle)
			response, err := protocol.SealCredentialBundle(
				body,
				now.Unix(),
				nonce,
				bundle,
				now,
				now.Add(time.Minute),
			)
			require.NoError(t, err)

			_, err = protocol.OpenCredentialBundle(
				body,
				now.Unix(),
				nonce,
				response,
				now.Add(30*time.Second),
			)
			assert.ErrorIs(t, err, ErrInvalidBundle)
		})
	}
}

func TestProvisionTupleAndBundleValidationFailClosed(t *testing.T) {
	now := time.Date(2026, 8, 10, 8, 30, 0, 0, time.UTC)
	valid := validProvisionRequest(now)

	tests := []struct {
		name   string
		mutate func(*ProvisionRequest)
	}{
		{name: "wrong mode", mutate: func(value *ProvisionRequest) { value.Mode = "production" }},
		{name: "missing Messenger app secret key ID", mutate: func(value *ProvisionRequest) {
			value.MessengerAppSecretKeyID = ""
		}},
		{name: "malformed provider proof secret key ID", mutate: func(value *ProvisionRequest) {
			value.ProviderProofSecretKeyID = "sha256=not-a-fingerprint"
		}},
		{name: "noncanonical organization", mutate: func(value *ProvisionRequest) {
			value.Tuple.OrganizationID = strings.ToUpper(value.Tuple.OrganizationID)
		}},
		{name: "zero account", mutate: func(value *ProvisionRequest) { value.Tuple.ChannelAccountID = uuid.Nil.String() }},
		{name: "missing generation", mutate: func(value *ProvisionRequest) { value.Tuple.Generation = "" }},
		{name: "leading-zero Meta ID", mutate: func(value *ProvisionRequest) { value.Tuple.PageID = "012345" }},
		{name: "non-UTC expiry", mutate: func(value *ProvisionRequest) {
			value.Tuple.ExpiresAt = now.Add(time.Hour).Format("2006-01-02T15:04:05+00:00")
		}},
		{name: "expired authority", mutate: func(value *ProvisionRequest) { value.Tuple.ExpiresAt = now.Add(-time.Second).Format(time.RFC3339Nano) }},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := valid
			testCase.mutate(&candidate)
			assert.ErrorIs(t, ValidateProvisionRequest(candidate, now), ErrInvalidRequest)
		})
	}

	protocol, err := NewProtocol(testAuthSecret, testWrapSecret)
	require.NoError(t, err)
	body, err := EncodeProvisionRequest(valid, now)
	require.NoError(t, err)
	nonce, err := NewNonce()
	require.NoError(t, err)
	base := CredentialBundle{
		MessengerAppSecretKeyID:  valid.MessengerAppSecretKeyID,
		ProviderProofSecretKeyID: valid.ProviderProofSecretKeyID,
		CredentialID:             uuid.NewString(),
		CredentialVersion:        1,
		ReReplyWebhookURL: "https://staging.rereply.example/api/webhooks/channels/" +
			valid.Tuple.ChannelAccountID,
		InboundSecret: "valid-inbound-signing-secret-with-at-least-32-bytes",
	}
	_, err = protocol.SealCredentialBundle(
		body, now.Unix(), nonce, base, now, now.Add(MaximumBundleTTL+time.Nanosecond),
	)
	assert.ErrorIs(t, err, ErrInvalidBundle)
	base.ReReplyWebhookURL = "https://attacker.invalid/api/webhooks/channels/" + uuid.NewString()
	_, err = protocol.SealCredentialBundle(
		body, now.Unix(), nonce, base, now, now.Add(time.Minute),
	)
	assert.ErrorIs(t, err, ErrInvalidBundle)
	base.ReReplyWebhookURL = "https://staging.rereply.example/api/webhooks/channels/" +
		valid.Tuple.ChannelAccountID
	base.InboundSecret = strings.Repeat("s", inboundSecretMaxBytes+1)
	_, err = protocol.SealCredentialBundle(
		body, now.Unix(), nonce, base, now, now.Add(time.Minute),
	)
	assert.ErrorIs(t, err, ErrInvalidBundle)
}

func TestSharedSecretKeyIDsAreStrictDomainSeparatedAndNonSecret(t *testing.T) {
	messengerID, err := MessengerAppSecretKeyID(testMessengerAppSecret)
	require.NoError(t, err)
	sameMessengerID, err := MessengerAppSecretKeyID(testMessengerAppSecret)
	require.NoError(t, err)
	providerID, err := ProviderProofSecretKeyID(testMessengerAppSecret)
	require.NoError(t, err)
	changedMessengerID, err := MessengerAppSecretKeyID(testMessengerAppSecret + "-different")
	require.NoError(t, err)

	assert.Regexp(t, `^sha256=[0-9a-f]{64}$`, messengerID)
	assert.Equal(t, messengerID, sameMessengerID)
	assert.NotEqual(t, messengerID, providerID)
	assert.NotEqual(t, messengerID, changedMessengerID)
	assert.NotContains(t, messengerID, testMessengerAppSecret)

	_, err = MessengerAppSecretKeyID("short")
	assert.ErrorIs(t, err, ErrInvalidRootSecret)
	_, err = ProviderProofSecretKeyID(testProofSecret + " ")
	assert.ErrorIs(t, err, ErrInvalidRootSecret)
}

func TestProtocolRootKeyDerivationIsStrictAndDomainSeparated(t *testing.T) {
	_, err := NewProtocol("short", testWrapSecret)
	assert.ErrorIs(t, err, ErrInvalidRootSecret)
	_, err = NewProtocol(" "+testAuthSecret, testWrapSecret)
	assert.ErrorIs(t, err, ErrInvalidRootSecret)
	_, err = NewProtocol(testAuthSecret[:20]+"\n"+testAuthSecret[20:], testWrapSecret)
	assert.ErrorIs(t, err, ErrInvalidRootSecret)
	_, err = NewProtocol(testAuthSecret, "short")
	assert.ErrorIs(t, err, ErrInvalidRootSecret)
	_, err = NewProtocol(testAuthSecret, testAuthSecret)
	assert.ErrorIs(t, err, ErrInvalidRootSecret)

	first, err := NewProtocol(testAuthSecret, testWrapSecret)
	require.NoError(t, err)
	second, err := NewProtocol(testAuthSecret, testWrapSecret)
	require.NoError(t, err)
	authChanged, err := NewProtocol(testAuthSecret+"-different", testWrapSecret)
	require.NoError(t, err)
	wrapChanged, err := NewProtocol(testAuthSecret, testWrapSecret+"-different")
	require.NoError(t, err)
	assert.Equal(t, first.KeyID(), second.KeyID())
	assert.Equal(t, first.KeyID(), authChanged.KeyID())
	assert.NotEqual(t, first.KeyID(), wrapChanged.KeyID())
	assert.False(t, errors.Is(ErrInvalidSignature, ErrInvalidResponse))
	assert.NotEqual(t, string(first.authKey), string(first.aeadKey))
	assert.NotEqual(t, string(first.authKey), string(first.keyIDKey))
	assert.NotEqual(t, string(first.aeadKey), string(first.keyIDKey))

	now := time.Date(2026, 8, 10, 8, 30, 0, 0, time.UTC)
	body, err := EncodeProvisionRequest(validProvisionRequest(now), now)
	require.NoError(t, err)
	nonce, err := NewNonce()
	require.NoError(t, err)
	firstSignature, err := first.SignProvisionRequest(now.Unix(), nonce, body)
	require.NoError(t, err)
	authChangedSignature, err := authChanged.SignProvisionRequest(now.Unix(), nonce, body)
	require.NoError(t, err)
	wrapChangedSignature, err := wrapChanged.SignProvisionRequest(now.Unix(), nonce, body)
	require.NoError(t, err)
	assert.NotEqual(t, firstSignature, authChangedSignature)
	assert.Equal(t, firstSignature, wrapChangedSignature)
}

func TestInboundProofBindsFullTupleCredentialGenerationAndBody(t *testing.T) {
	now := time.Date(2026, 8, 10, 8, 30, 0, 0, time.UTC)
	tuple := validProvisionRequest(now).Tuple
	credentialID := "2515cc3f-7166-4262-820c-415c1358b316"
	body := []byte(`{"external_account_id":"1038752885977372","events":[]}`)

	proof, err := SignInboundProof(testProofSecret, tuple, credentialID, 7, body)
	require.NoError(t, err)
	assert.Regexp(t, `^sha256=[0-9a-f]{64}$`, proof)
	require.NoError(t, VerifyInboundProof(
		testProofSecret,
		tuple,
		credentialID,
		7,
		body,
		proof,
	))

	tamperedTuple := tuple
	tamperedTuple.Generation = "fa16563b-930a-4bf5-bab7-c93eec99c048"
	assert.ErrorIs(t, VerifyInboundProof(
		testProofSecret, tamperedTuple, credentialID, 7, body, proof,
	), ErrInvalidProof)
	assert.ErrorIs(t, VerifyInboundProof(
		testProofSecret, tuple, "d5153063-c5f1-4348-a741-01f523cc2e83", 7, body, proof,
	), ErrInvalidProof)
	assert.ErrorIs(t, VerifyInboundProof(
		testProofSecret, tuple, credentialID, 8, body, proof,
	), ErrInvalidProof)
	assert.ErrorIs(t, VerifyInboundProof(
		testProofSecret, tuple, credentialID, 7, append(body, '\n'), proof,
	), ErrInvalidProof)
	assert.ErrorIs(t, VerifyInboundProof(
		testProofSecret+"-different", tuple, credentialID, 7, body, proof,
	), ErrInvalidProof)

	_, err = SignInboundProof("short", tuple, credentialID, 7, body)
	assert.ErrorIs(t, err, ErrInvalidProofSecret)
	_, err = SignInboundProof(testProofSecret+" ", tuple, credentialID, 7, body)
	assert.ErrorIs(t, err, ErrInvalidProofSecret)
	_, err = SignInboundProof(testProofSecret[:20]+" "+testProofSecret[20:], tuple, credentialID, 7, body)
	assert.ErrorIs(t, err, ErrInvalidProofSecret)
	_, err = SignInboundProof(testProofSecret, tuple, credentialID, 7, nil)
	assert.ErrorIs(t, err, ErrInvalidProof)
}

func TestStrictWireDecodingRejectsUnknownAndTrailingFields(t *testing.T) {
	now := time.Date(2026, 8, 10, 8, 30, 0, 0, time.UTC)
	request := validProvisionRequest(now)
	body, err := json.Marshal(request)
	require.NoError(t, err)
	_, err = DecodeProvisionRequest(append(body, []byte(` {}`)...), now)
	assert.ErrorIs(t, err, ErrInvalidRequest)

	unknown := strings.Replace(string(body), `"version":"v1"`, `"version":"v1","secret":"must-not-be-accepted"`, 1)
	_, err = DecodeProvisionRequest([]byte(unknown), now)
	assert.ErrorIs(t, err, ErrInvalidRequest)
	_, err = DecodeProvisionRequest(make([]byte, MaximumRequestBodyBytes+1), now)
	assert.ErrorIs(t, err, ErrInvalidRequest)

	oversizedResponse, err := json.Marshal(ProvisionResponse{
		Version:   Version,
		Mode:      Mode,
		KeyID:     "sha256=" + strings.Repeat("0", 64),
		IssuedAt:  now.Format(time.RFC3339Nano),
		ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
		Envelope:  strings.Repeat("A", MaximumResponseBodyBytes+1),
	})
	require.NoError(t, err)
	_, err = DecodeProvisionResponse(oversizedResponse, now)
	assert.ErrorIs(t, err, ErrInvalidResponse)
}

func validProvisionRequest(now time.Time) ProvisionRequest {
	return ProvisionRequest{
		Version:                  Version,
		Mode:                     Mode,
		MessengerAppSecretKeyID:  mustSharedSecretKeyID(MessengerAppSecretKeyID, testMessengerAppSecret),
		ProviderProofSecretKeyID: mustSharedSecretKeyID(ProviderProofSecretKeyID, testProofSecret),
		Tuple: ProvisionTuple{
			OrganizationID:   "c73f761f-5154-4fe1-9a13-06bae570277a",
			MetaBusinessID:   "3852210034910979",
			PageID:           "1038752885977372",
			MetaAppID:        "1035383549213572",
			ChannelAccountID: "88dadf4a-7ea5-4b42-9a8c-174b3db4a73c",
			Generation:       "9bd97a61-4388-4430-8076-1f60c76e44d7",
			ExpiresAt:        now.Add(24 * time.Hour).Format(time.RFC3339Nano),
		},
	}
}

func mustSharedSecretKeyID(keyID func(string) (string, error), secret string) string {
	value, err := keyID(secret)
	if err != nil {
		panic(err)
	}
	return value
}

func mustReplayKey(t *testing.T, nonce string) string {
	t.Helper()
	value, err := ReplayKey(nonce)
	require.NoError(t, err)
	return value
}
