package handlers

import (
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validEmbeddedSignupToken(now time.Time) *whatsapp.TokenDebugInfo {
	return &whatsapp.TokenDebugInfo{
		AppID:   "meta-app-123",
		IsValid: true,
		Scopes: []string{
			"business_management",
			"whatsapp_business_management",
			"whatsapp_business_messaging",
		},
		ExpiresAt:           now.Add(2 * time.Hour).Unix(),
		DataAccessExpiresAt: now.Add(time.Hour).Unix(),
	}
}

func TestValidateMetaEmbeddedSignupTokenRequiresAppScopesAndTracksEarliestExpiry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)
	info := validEmbeddedSignupToken(now)

	expiresAt, err := validateMetaEmbeddedSignupToken(info, "meta-app-123", now)
	require.NoError(t, err)
	require.NotNil(t, expiresAt)
	assert.Equal(t, now.Add(time.Hour), *expiresAt)

	for _, missing := range []string{
		"business_management",
		"whatsapp_business_management",
		"whatsapp_business_messaging",
	} {
		t.Run("missing "+missing, func(t *testing.T) {
			candidate := validEmbeddedSignupToken(now)
			candidate.Scopes = nil
			for _, scope := range info.Scopes {
				if scope != missing {
					candidate.Scopes = append(candidate.Scopes, scope)
				}
			}
			_, err := validateMetaEmbeddedSignupToken(candidate, "meta-app-123", now)
			require.ErrorContains(t, err, missing)
		})
	}
}

func TestValidateMetaEmbeddedSignupTokenRejectsWrongAppInvalidAndExpired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)

	wrongApp := validEmbeddedSignupToken(now)
	_, err := validateMetaEmbeddedSignupToken(wrongApp, "another-app", now)
	require.ErrorContains(t, err, "different Meta app")

	invalid := validEmbeddedSignupToken(now)
	invalid.IsValid = false
	_, err = validateMetaEmbeddedSignupToken(invalid, "meta-app-123", now)
	require.ErrorContains(t, err, "invalid embedded signup token")

	expired := validEmbeddedSignupToken(now)
	expired.ExpiresAt = now.Add(-time.Minute).Unix()
	_, err = validateMetaEmbeddedSignupToken(expired, "meta-app-123", now)
	require.ErrorContains(t, err, "expired")
}
