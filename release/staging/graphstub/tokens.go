package graphstub

import (
	"net/http"
	"net/url"
	"sort"
	"time"
)

const dataAccessLifetime = 90 * 24 * time.Hour

// oauthAccessToken serves /oauth/access_token for the two grants the product
// uses: client_credentials, which the Integration Center's Meta test sends
// (internal/handlers/integrations.go), and an Embedded Signup code exchange
// (pkg/whatsapp ExchangeCodeForToken). Codes exist only once the control API
// has issued them and exchange at most once.
func (s *Server) oauthAccessToken(c *call) {
	if c.r.Method != http.MethodPost && c.r.Method != http.MethodGet {
		unsupported(c)
		return
	}
	params := c.r.URL.Query()
	if c.r.Method == http.MethodPost {
		body, ok := readBody(c, 16<<10)
		if !ok {
			return
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			invalidParameter(c, "The request body is not a valid form")
			return
		}
		// Graph reads parameters from the query and the form; the form wins.
		for key, values := range form {
			params[key] = values
		}
	}
	if !oneOf(params.Get("client_id"), s.cfg.AppID) || !oneOf(params.Get("client_secret"), s.cfg.AppSecret) {
		graphError(c, http.StatusBadRequest, 1, 0, "OAuthException", "Error validating client secret.", false)
		return
	}
	c.trusted = true
	switch grant := params.Get("grant_type"); {
	case grant == "client_credentials":
		writeJSON(c.w, http.StatusOK, map[string]string{"access_token": s.appToken, "token_type": "bearer"})
	case (grant == "" || grant == "authorization_code") && params.Get("code") != "":
		s.mu.Lock()
		token, ok := s.state.takeCode(params.Get("code"), s.now())
		s.mu.Unlock()
		if !ok {
			graphError(c, http.StatusBadRequest, 100, 36007, "OAuthException", "Invalid verification code format.", false)
			return
		}
		writeJSON(c.w, http.StatusOK, map[string]any{"access_token": token, "token_type": "bearer", "expires_in": 5183999})
	default:
		invalidParameter(c, "Param grant_type is not supported by the Graph stub")
	}
}

// debugToken serves /debug_token as Embedded Signup uses it
// (internal/handlers/accounts.go): the caller authenticates with the app
// token "app_id|app_secret" (or the client_credentials token), and a valid
// input token grants both WhatsApp scopes on every synthetic WABA.
func (s *Server) debugToken(c *call) {
	if c.r.Method != http.MethodGet {
		unsupported(c)
		return
	}
	if !oneOf(bearer(c.r), s.cfg.AppID+"|"+s.cfg.AppSecret, s.appToken) {
		graphError(c, http.StatusUnauthorized, 190, 0, "OAuthException", "Invalid OAuth access token - Cannot parse access token", false)
		return
	}
	c.trusted = true
	input := c.r.URL.Query().Get("input_token")
	if input == "" {
		invalidParameter(c, "The parameter input_token is required")
		return
	}
	if !oneOf(input, s.cfg.AccessTokens...) {
		writeJSON(c.w, http.StatusOK, map[string]any{"data": map[string]any{
			"app_id":   s.cfg.AppID,
			"is_valid": false,
			"scopes":   []string{},
			"error":    map[string]any{"code": 190, "message": "Invalid OAuth access token."},
		}})
		return
	}
	s.mu.Lock()
	wabas := make([]string, 0, len(s.state.wabas))
	for id := range s.state.wabas {
		wabas = append(wabas, id)
	}
	s.mu.Unlock()
	sort.Strings(wabas)
	now := s.now()
	writeJSON(c.w, http.StatusOK, map[string]any{"data": map[string]any{
		"app_id":                 s.cfg.AppID,
		"type":                   "SYSTEM_USER",
		"application":            "Graph Stub App",
		"data_access_expires_at": now.Add(dataAccessLifetime).Unix(),
		"expires_at":             0,
		"is_valid":               true,
		"issued_at":              now.Add(-time.Hour).Unix(),
		"scopes":                 []string{"business_management", "whatsapp_business_management", "whatsapp_business_messaging"},
		"granular_scopes": []any{
			map[string]any{"scope": "whatsapp_business_management", "target_ids": wabas},
			map[string]any{"scope": "whatsapp_business_messaging", "target_ids": wabas},
		},
		"user_id": "100000000000001",
	}})
}
