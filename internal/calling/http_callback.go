package calling

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	channelapi "github.com/shridarpatil/whatomate/internal/channel"
)

// HTTPCallbackResult holds the response from an HTTP callback.
type HTTPCallbackResult struct {
	StatusCode int
	Body       string
}

// ValidateHTTPCallbackURL requires a structurally valid public HTTPS endpoint.
// DNS is revalidated by the injected production client's SSRF-safe dialer at
// request time, including after an attacker-controlled DNS record changes.
func ValidateHTTPCallbackURL(rawURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.Fragment != "" || strings.ContainsAny(parsed.Hostname(), "{}") {
		return fmt.Errorf("HTTP callback URL must be a public HTTPS URL")
	}

	// Fixed query parameters are allowed for IVR integrations, but they do not
	// participate in the network destination decision.
	hostOnly := *parsed
	hostOnly.RawQuery = ""
	hostOnly.ForceQuery = false
	if err := channelapi.ValidateRelayEndpoint(hostOnly.String()); err != nil {
		return fmt.Errorf("HTTP callback URL must be a public HTTPS URL")
	}
	return nil
}

// ValidateHTTPCallbackTemplateURL applies the stored-template policy on top of
// runtime URL validation. Query values must be variable placeholders so a
// literal API token cannot be persisted in an IVR URL; fixed credentials belong
// in the encrypted callback-header vault.
func ValidateHTTPCallbackTemplateURL(rawURL string) error {
	if err := ValidateHTTPCallbackURL(rawURL); err != nil {
		return err
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("HTTP callback URL must be a public HTTPS URL")
	}
	if parsed.RawQuery == "" {
		return nil
	}
	values, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return fmt.Errorf("HTTP callback URL query parameters are invalid")
	}
	for name, candidates := range values {
		if strings.TrimSpace(name) == "" || len(candidates) == 0 {
			return fmt.Errorf("HTTP callback URL query parameters are invalid")
		}
		for _, value := range candidates {
			trimmed := strings.TrimSpace(value)
			if len(trimmed) < 5 || !strings.HasPrefix(trimmed, "{{") ||
				!strings.HasSuffix(trimmed, "}}") || strings.ContainsAny(trimmed, "\r\n") {
				return fmt.Errorf("HTTP callback URL query values must use {{variable}} placeholders; store credentials in encrypted headers")
			}
		}
	}
	return nil
}

// ValidateHTTPCallbackMethod keeps callback dispatch to the two methods
// exposed by the IVR editor. Rejecting tunnel and diagnostic methods avoids
// replaying stored credentials through an unintended HTTP semantic.
func ValidateHTTPCallbackMethod(method string) error {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet, http.MethodPost:
		return nil
	default:
		return fmt.Errorf("HTTP callback method must be GET or POST")
	}
}

// executeHTTPCallback performs an HTTP request with configurable method,
// headers, and body using the injected production HTTP client. It fails closed
// without that client and refuses redirects so endpoint credentials cannot be
// replayed to a second host.
func executeHTTPCallback(client *http.Client, rawURL, method string, headers map[string]string, body string, timeout time.Duration) (*HTTPCallbackResult, error) {
	if client == nil {
		return nil, fmt.Errorf("HTTP callback client is not configured")
	}
	if err := ValidateHTTPCallbackURL(rawURL); err != nil {
		return nil, err
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if err := ValidateHTTPCallbackMethod(method); err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, rawURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	requestClient := *client
	requestClient.Timeout = timeout
	requestClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := requestClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024)) // limit to 64KB
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return &HTTPCallbackResult{
		StatusCode: resp.StatusCode,
		Body:       string(respBody),
	}, nil
}

// interpolateTemplate replaces {{key}} placeholders with values from the variables map.
func interpolateTemplate(tpl string, vars map[string]string) string {
	for k, v := range vars {
		tpl = strings.ReplaceAll(tpl, "{{"+k+"}}", v)
	}
	return tpl
}
