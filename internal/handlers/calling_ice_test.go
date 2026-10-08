package handlers_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shridarpatil/whatomate/internal/calling"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

type iceEndpointTransport func(*http.Request) (*http.Response, error)

func (f iceEndpointTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestGetICEServersRenewalAuthorizationNoStoreAndSecretBoundary(t *testing.T) {
	for _, tc := range []struct {
		name                string
		authorized, enabled bool
		upstream, expected  int
	}{
		{"success", true, true, 201, 200},
		{"upstream failure", true, true, 403, 503},
		{"calling disabled", true, false, 201, 503},
		{"unauthenticated", false, true, 201, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t)
			org := testutil.CreateTestOrganization(t, app.DB)
			org.Settings = models.JSONB{"calling_enabled": tc.enabled}
			require.NoError(t, app.DB.Save(org).Error)
			user := createAdminUser(t, app, org.ID)
			token := "synthetic-endpoint-private-token"
			app.Config.Calling = config.CallingConfig{RelayOnly: true, CloudflareTURN: config.CloudflareTURNConfig{KeyID: strings.Repeat("a", 32), APIToken: token}}
			var calls atomic.Int32
			client := &http.Client{Transport: iceEndpointTransport(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				body := `{"iceServers":[{"urls":["turns:turn.cloudflare.com:443?transport=tcp"],"username":"temporary-user","credential":"temporary-password"}]}`
				if tc.upstream != 201 {
					body = token
				}
				return &http.Response{StatusCode: tc.upstream, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			app.CallManager = calling.NewManager(&app.Config.Calling, nil, app.DB, app.Redis, nil, nil, nil, client, "", testutil.NopLogger())
			req := testutil.NewGETRequest(t)
			if tc.authorized {
				testutil.SetAuthContext(req, org.ID, user.ID)
			}
			require.NoError(t, app.GetICEServers(req))
			require.Equal(t, tc.expected, testutil.GetResponseStatusCode(req))
			require.Equal(t, "no-store", string(req.RequestCtx.Response.Header.Peek("Cache-Control")))
			body := string(testutil.GetResponseBody(req))
			require.NotContains(t, body, token)
			if tc.expected == fasthttp.StatusOK {
				var response struct {
					Data calling.ICEConfiguration `json:"data"`
				}
				require.NoError(t, json.Unmarshal([]byte(body), &response))
				require.Equal(t, "relay", response.Data.TransportPolicy)
				require.NotNil(t, response.Data.ExpiresAt)
				require.Equal(t, "temporary-password", response.Data.Servers[0].Credential)
			} else {
				require.NotContains(t, body, "temporary-password")
			}
			if !tc.authorized || !tc.enabled {
				require.Zero(t, calls.Load())
			}
		})
	}
}

func TestGetICEServersIncomingOnlyRoleAndUnrelatedRole(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	org.Settings = models.JSONB{"calling_enabled": true}
	require.NoError(t, app.DB.Save(org).Error)
	app.Config.Calling.ICEServers = []config.ICEServerConfig{{URLs: []string{"turn:example.invalid:3478"}, Username: "static-user", Credential: "static-password"}}
	app.CallManager = calling.NewManager(&app.Config.Calling, nil, app.DB, app.Redis, nil, nil, nil, nil, "", testutil.NopLogger())
	for _, tc := range []struct {
		name, resource, action string
		expected               int
	}{
		{"incoming-only", models.ResourceCallTransfers, models.ActionWrite, 200},
		{"outgoing-only", models.ResourceOutgoingCalls, models.ActionRead, 200},
		{"unrelated", models.ResourceContacts, models.ActionRead, 403},
		{"transfer-viewer", models.ResourceCallTransfers, models.ActionRead, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, tc.name, []string{tc.resource + ":" + tc.action})
			user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
			req := testutil.NewGETRequest(t)
			testutil.SetAuthContext(req, org.ID, user.ID)
			require.NoError(t, app.GetICEServers(req))
			require.Equal(t, tc.expected, testutil.GetResponseStatusCode(req))
			if tc.expected != 200 {
				require.NotContains(t, string(testutil.GetResponseBody(req)), "static-password")
			}
		})
	}
}
