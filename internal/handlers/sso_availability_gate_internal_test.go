package handlers

import (
	"net/url"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestOptC_SSOAvailabilityByEnvironment(t *testing.T) {
	for environment, available := range map[string]bool{
		"development": true,
		"test":        true,
		"staging":     false,
		"production":  false,
		"":            false,
		"Production ": false,
		"prod":        false,
		"Development": false,
		" test":       false,
		"TEST":        false,
		"unknown":     false,
	} {
		assert.Equal(t, available, ssoAvailableInEnvironment(environment), "%q", environment)
		app := &App{Config: &config.Config{App: config.AppConfig{Environment: environment}}}
		assert.Equal(t, available, app.ssoAvailable(), "%q", environment)
	}

	assert.False(t, (&App{}).ssoAvailable(), "a missing configuration refuses")
	var noApp *App
	assert.False(t, noApp.ssoAvailable(), "a missing app refuses")
}

// With no configuration, database or Redis, every SSO entry point refuses
// without panicking: a gate reached only after a lookup would dereference one
// of them.
func TestOptC_SSOEntryPointsRefuseWithoutConfiguration(t *testing.T) {
	app := &App{Log: testutil.NopLogger()}
	const unavailable = "Single sign-on is temporarily unavailable."

	list := testutil.NewGETRequest(t)
	require.NotPanics(t, func() { require.NoError(t, app.GetPublicSSOProviders(list)) })
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(list))
	assert.Contains(t, string(testutil.GetResponseBody(list)), `"data":[]`)
	assert.Equal(t, "no-store", string(list.RequestCtx.Response.Header.Peek("Cache-Control")))

	start := testutil.NewGETRequest(t)
	testutil.SetPathParam(start, "provider", "custom")
	require.NotPanics(t, func() { require.NoError(t, app.InitSSO(start)) })
	testutil.AssertErrorResponse(t, start, fasthttp.StatusServiceUnavailable, unavailable)

	callback := testutil.NewGETRequest(t)
	testutil.SetPathParam(callback, "provider", "custom")
	testutil.SetQueryParam(callback, "code", "code")
	testutil.SetQueryParam(callback, "state", "state")
	require.NotPanics(t, func() { require.NoError(t, app.CallbackSSO(callback)) })
	require.Equal(t, fasthttp.StatusTemporaryRedirect, testutil.GetResponseStatusCode(callback))
	location, err := url.Parse(string(callback.RequestCtx.Response.Header.Peek("Location")))
	require.NoError(t, err)
	assert.Equal(t, "/login", location.Path)
	assert.Equal(t, unavailable, location.Query().Get("sso_error"))

	exchange := testutil.NewJSONRequest(t, map[string]any{"sso_exchange": "code"})
	require.NotPanics(t, func() { require.NoError(t, app.RefreshToken(exchange)) })
	testutil.AssertErrorResponse(t, exchange, fasthttp.StatusServiceUnavailable, unavailable)
}
