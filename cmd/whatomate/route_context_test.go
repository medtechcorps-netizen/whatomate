package main

import (
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/fastglue"
)

func TestRegisteredRoutesKeepPathParametersSeparateFromRequestContext(t *testing.T) {
	g := fastglue.NewGlue()
	setupRoutes(
		g,
		&handlers.App{},
		testutil.NopLogger(),
		"",
		nil,
		&config.Config{},
	)

	require.NoError(t, validateRouteContextKeyIsolation(g.Router.List()))
}

func TestRouteContextKeyIsolationRejectsReservedPathParameters(t *testing.T) {
	err := validateRouteContextKeyIsolation(map[string][]string{
		"PUT": {"/api/admin/organizations/{organization_id}/subscription"},
	})

	require.ErrorContains(t, err, "{organization_id}")
}
