package handlers_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApp_CallbackSSORejectsProviderDisabledAfterStateIssued(t *testing.T) {
	app := newSSOApp(t)
	fake := newFakeOAuth(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	provider := createCustomSSOProvider(t, app, org.ID, fake)

	nonce := "provider-disabled-after-init"
	state := handlers.SSOState{
		OrgID:     org.ID.String(),
		Provider:  "custom",
		Nonce:     nonce,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	stored, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, app.Redis.Set(
		context.Background(),
		"sso:state:"+nonce,
		stored,
		5*time.Minute,
	).Err())
	require.NoError(t, app.DB.Model(provider).Update("is_enabled", false).Error)

	req := testutil.NewGETRequest(t)
	testutil.SetPathParam(req, "provider", "custom")
	testutil.SetQueryParam(req, "code", "must-not-be-exchanged")
	testutil.SetQueryParam(req, "state", nonce)
	require.NoError(t, app.CallbackSSO(req))

	location := string(req.RequestCtx.Response.Header.Peek("Location"))
	assert.Contains(t, location, "SSO+provider+not+configured")
	assert.Empty(t, fake.LastTokenCode, "disabled provider must not exchange the OAuth code")
}
