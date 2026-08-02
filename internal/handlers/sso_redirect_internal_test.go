package handlers

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOAuthHTTPClientWithoutRedirectsClonesAndRefusesRedirects(t *testing.T) {
	originalRedirectCalls := 0
	original := &http.Client{
		Transport: http.DefaultTransport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			originalRedirectCalls++
			return nil
		},
	}

	hardened := oauthHTTPClientWithoutRedirects(original)
	require.NotSame(t, original, hardened)
	assert.Same(t, original.Transport, hardened.Transport)
	require.NotNil(t, hardened.CheckRedirect)
	assert.ErrorIs(t, hardened.CheckRedirect(nil, nil), http.ErrUseLastResponse)
	assert.Zero(t, originalRedirectCalls, "the caller's client must not be mutated or invoked")

	require.NotNil(t, original.CheckRedirect)
	assert.NoError(t, original.CheckRedirect(nil, nil))
	assert.Equal(t, 1, originalRedirectCalls)
}

func TestOAuthHTTPClientWithoutRedirectsClonesDefaultClient(t *testing.T) {
	hardened := oauthHTTPClientWithoutRedirects(nil)
	require.NotSame(t, http.DefaultClient, hardened)
	require.NotNil(t, hardened.CheckRedirect)
	assert.True(t, errors.Is(hardened.CheckRedirect(nil, nil), http.ErrUseLastResponse))
}

func TestNormalizeSSOOrganizationSelector(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		want      string
		wantValid bool
	}{
		{name: "normalizes case and whitespace", raw: "  Klinik-Relive-A1B2C3D4  ", want: "klinik-relive-a1b2c3d4", wantValid: true},
		{name: "legacy omission", raw: "", want: "", wantValid: true},
		{name: "rejects path syntax", raw: "../clinic", want: "", wantValid: false},
		{name: "rejects leading dash", raw: "-clinic", want: "", wantValid: false},
		{name: "rejects trailing dash", raw: "clinic-", want: "", wantValid: false},
		{name: "rejects oversized selector", raw: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want: "", wantValid: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, valid := normalizeSSOOrganizationSelector(test.raw)
			assert.Equal(t, test.want, got)
			assert.Equal(t, test.wantValid, valid)
		})
	}
}
