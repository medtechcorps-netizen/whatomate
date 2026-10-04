package graphstub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestGraphEndpoints walks every Graph endpoint, in order, against one stub.
// Each read-only case runs with and without the /v{n}.{m} prefix.
func TestGraphEndpoints(t *testing.T) {
	h := newHarness(t, nil)
	_, uploaded := h.uploadMedia(testPhone, "image/png", []byte("synthetic png bytes"))
	mediaID, _ := uploaded["id"].(string)
	if mediaID == "" {
		t.Fatalf("media upload returned %v", uploaded)
	}
	templateComponents := []any{map[string]any{"type": "BODY", "text": "Hello {{1}}"}}

	type check func(t *testing.T, body map[string]any)
	has := func(path []any, want any) check {
		return func(t *testing.T, body map[string]any) {
			t.Helper()
			if got := dig(body, path...); got != want {
				t.Fatalf("%v = %v, want %v (body %v)", path, got, want, body)
			}
		}
	}
	graphCode := func(want int) check {
		return func(t *testing.T, body map[string]any) {
			t.Helper()
			if got := errorCode(t, body); got != want {
				t.Fatalf("Graph error code %d, want %d", got, want)
			}
		}
	}
	cases := []struct {
		name     string
		method   string
		path     string
		token    string
		body     any
		status   int
		readOnly bool
		check    check
	}{
		{"phone fields", http.MethodGet, "/" + testPhone + "?fields=display_phone_number,verified_name,code_verification_status,account_mode,platform_type", testToken, nil, 200, true,
			func(t *testing.T, body map[string]any) {
				if body["id"] != testPhone || body["display_phone_number"] != "+1 555-0101" || body["verified_name"] != "Stub Clinic" ||
					body["code_verification_status"] != "VERIFIED" || body["account_mode"] != "LIVE" || body["quality_rating"] != nil {
					t.Fatalf("phone %v", body)
				}
			}},
		{"phone default fields", http.MethodGet, "/" + testPhone, testToken2, nil, 200, true, has([]any{"throughput", "level"}, "STANDARD")},
		{"waba fields", http.MethodGet, "/" + testWABA + "?fields=id,name", testToken, nil, 200, true, has([]any{"name"}, "Graph Stub Business Account")},
		{"waba phone numbers page 1", http.MethodGet, "/" + testWABA + "/phone_numbers?fields=id,display_phone_number,verified_name,quality_rating&limit=1", testToken, nil, 200, true,
			func(t *testing.T, body map[string]any) {
				if dig(body, "data", 0, "id") != testPhone || dig(body, "data", 1) != nil || dig(body, "paging", "next") == nil {
					t.Fatalf("page 1 %v", body)
				}
				after, _ := dig(body, "paging", "cursors", "after").(string)
				_, page2 := h.graph(http.MethodGet, "/"+testWABA+"/phone_numbers?limit=1&after="+url.QueryEscape(after), testToken, nil)
				if dig(page2, "data", 0, "id") != testPhone2 || dig(page2, "paging", "next") != nil {
					t.Fatalf("page 2 %v", page2)
				}
			}},
		{"bad paging cursor", http.MethodGet, "/" + testWABA + "/phone_numbers?after=bogus", testToken, nil, 400, true, graphCode(100)},
		{"missing bearer", http.MethodGet, "/" + testPhone, "", nil, 401, true, graphCode(190)},
		{"unknown bearer", http.MethodGet, "/" + testPhone, "not-a-configured-token", nil, 401, true, graphCode(190)},
		{"unknown object", http.MethodGet, "/999999999999999", testToken, nil, 400, true, graphCode(100)},
		{"unsupported edge", http.MethodGet, "/" + testPhone + "/calls", testToken, nil, 400, true, graphCode(100)},
		{"edge on the wrong object", http.MethodPost, "/" + testWABA + "/messages", testToken, map[string]any{"messaging_product": "whatsapp"}, 400, false, graphCode(100)},
		{"unsupported method", http.MethodPut, "/" + testPhone + "/messages", testToken, nil, 400, false, graphCode(100)},

		{"send text", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"messaging_product": "whatsapp", "to": testCustomer, "type": "text", "text": map[string]any{"body": "hello"},
		}, 200, false, func(t *testing.T, body map[string]any) {
			id, _ := dig(body, "messages", 0, "id").(string)
			if !strings.HasPrefix(id, "wamid.") || dig(body, "contacts", 0, "wa_id") != testCustomer {
				t.Fatalf("send %v", body)
			}
		}},
		{"send to a BSUID recipient", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"messaging_product": "whatsapp", "recipient": "US.13491208655302741918", "type": "text", "text": map[string]any{"body": "hi"},
		}, 200, false, nil},
		{"send interactive", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"messaging_product": "whatsapp", "to": testCustomer, "type": "interactive", "interactive": map[string]any{"type": "button"},
		}, 200, false, nil},
		{"send a contact card", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"messaging_product": "whatsapp", "to": testCustomer, "type": "contacts",
			"contacts": []any{map[string]any{"name": map[string]any{"formatted_name": "Synthetic Contact"}}},
		}, 200, false, nil},
		{"send an empty contact list", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"messaging_product": "whatsapp", "to": testCustomer, "type": "contacts", "contacts": []any{},
		}, 400, false, graphCode(100)},
		{"send uploaded image", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"messaging_product": "whatsapp", "to": testCustomer, "type": "image", "image": map[string]any{"id": mediaID},
		}, 200, false, nil},
		{"send unknown media", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"messaging_product": "whatsapp", "to": testCustomer, "type": "image", "image": map[string]any{"id": "123456789012345"},
		}, 400, false, graphCode(131053)},
		{"send without messaging_product", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"to": testCustomer, "type": "text", "text": map[string]any{"body": "x"},
		}, 400, false, graphCode(100)},
		{"send without a recipient", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"messaging_product": "whatsapp", "type": "text", "text": map[string]any{"body": "x"},
		}, 400, false, graphCode(100)},
		{"send an empty text", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"messaging_product": "whatsapp", "to": testCustomer, "type": "text", "text": map[string]any{"body": ""},
		}, 400, false, graphCode(100)},
		{"send an unknown type", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"messaging_product": "whatsapp", "to": testCustomer, "type": "hologram", "hologram": map[string]any{},
		}, 400, false, graphCode(100)},
		{"send a malformed body", http.MethodPost, "/" + testPhone + "/messages", testToken, "{", 400, false, graphCode(100)},
		{"read receipt", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"messaging_product": "whatsapp", "status": "read", "message_id": "wamid.inbound",
		}, 200, false, has([]any{"success"}, true)},

		{"media metadata", http.MethodGet, "/" + mediaID, testToken, nil, 200, true, func(t *testing.T, body map[string]any) {
			if body["mime_type"] != "image/png" || body["file_size"] != float64(len("synthetic png bytes")) || body["url"] != h.url+"/_media/"+mediaID {
				t.Fatalf("media %v", body)
			}
		}},
		{"media download", http.MethodGet, "/_media/" + mediaID, testToken, nil, 200, true, nil},
		{"media download needs a bearer", http.MethodGet, "/_media/" + mediaID, "", nil, 401, true, graphCode(190)},
		{"media download of unknown media", http.MethodGet, "/_media/123456789012345", testToken, nil, 404, true, nil},

		{"create template", http.MethodPost, "/" + testWABA + "/message_templates", testToken, map[string]any{
			"name": "visit_reminder", "language": "en", "category": "utility", "components": templateComponents,
		}, 200, false, has([]any{"status"}, "PENDING")},
		{"create a duplicate template", http.MethodPost, "/" + testWABA + "/message_templates", testToken, map[string]any{
			"name": "visit_reminder", "language": "en", "category": "UTILITY", "components": templateComponents,
		}, 400, false, graphCode(100)},
		{"create a template with a bad name", http.MethodPost, "/" + testWABA + "/message_templates", testToken, map[string]any{
			"name": "Visit Reminder", "language": "en", "category": "UTILITY", "components": templateComponents,
		}, 400, false, graphCode(100)},
		{"create a template without components", http.MethodPost, "/" + testWABA + "/message_templates", testToken, map[string]any{
			"name": "empty", "language": "en", "category": "UTILITY", "components": []any{},
		}, 400, false, graphCode(100)},
		{"list templates", http.MethodGet, "/" + testWABA + "/message_templates?fields=id,name,language,category,status,components,quality_score&limit=100", testToken, nil, 200, true,
			func(t *testing.T, body map[string]any) {
				if dig(body, "data", 0, "name") != "visit_reminder" || dig(body, "data", 0, "category") != "UTILITY" ||
					dig(body, "data", 0, "components", 0, "type") != "BODY" || dig(body, "data", 0, "quality_score", "score") != "UNKNOWN" {
					t.Fatalf("templates %v", body)
				}
			}},
		{"templates of another WABA", http.MethodGet, "/" + testWABA2 + "/message_templates", testToken, nil, 200, true,
			func(t *testing.T, body map[string]any) {
				if data, ok := body["data"].([]any); !ok || len(data) != 0 {
					t.Fatalf("templates leaked across WABAs, or data is not an empty array: %v", body)
				}
			}},
		{"send an unapproved template", http.MethodPost, "/" + testPhone + "/messages", testToken, map[string]any{
			"messaging_product": "whatsapp", "to": testCustomer, "type": "template",
			"template": map[string]any{"name": "visit_reminder", "language": map[string]any{"code": "en"}},
		}, 404, false, graphCode(132001)},

		{"subscribed apps before subscribing", http.MethodGet, "/" + testWABA + "/subscribed_apps", testToken, nil, 200, false,
			func(t *testing.T, body map[string]any) {
				if data, _ := body["data"].([]any); len(data) != 0 {
					t.Fatalf("subscribed %v", body)
				}
			}},
		{"subscribe", http.MethodPost, "/" + testWABA + "/subscribed_apps", testToken, nil, 200, false, has([]any{"success"}, true)},
		{"subscribed apps", http.MethodGet, "/" + testWABA + "/subscribed_apps", testToken, nil, 200, true,
			has([]any{"data", 0, "whatsapp_business_api_data", "id"}, testAppID)},
		{"subscribed apps on a phone", http.MethodGet, "/" + testPhone + "/subscribed_apps", testToken, nil, 400, true, graphCode(100)},

		{"register", http.MethodPost, "/" + testPhone + "/register", testToken, map[string]any{"messaging_product": "whatsapp", "pin": "123456"}, 200, false,
			has([]any{"success"}, true)},
		{"register with a bad pin", http.MethodPost, "/" + testPhone + "/register", testToken, map[string]any{"messaging_product": "whatsapp", "pin": "12ab"}, 400, false,
			graphCode(100)},
		{"registered phone status", http.MethodGet, "/" + testPhone + "?fields=status", testToken, nil, 200, true, has([]any{"status"}, "CONNECTED")},

		{"set a webhook override", http.MethodPost, "/" + testPhone, testToken, map[string]any{
			"webhook_configuration": map[string]any{"override_callback_uri": "https://product.example.test/api/webhook?workspace=1", "verify_token": "verify-token-value"},
		}, 200, false, has([]any{"success"}, true)},
		{"read the webhook override", http.MethodGet, "/" + testPhone + "?fields=webhook_configuration", testToken, nil, 200, true,
			has([]any{"webhook_configuration", "phone_number"}, "https://product.example.test/api/webhook?workspace=1")},
		{"override on a production host", http.MethodPost, "/" + testPhone, testToken, map[string]any{
			"webhook_configuration": map[string]any{"override_callback_uri": "https://api.rereply.app/api/webhook", "verify_token": "verify-token-value"},
		}, 400, false, graphCode(100)},
		{"override over plain http", http.MethodPost, "/" + testPhone, testToken, map[string]any{
			"webhook_configuration": map[string]any{"override_callback_uri": "http://product.example.test/api/webhook", "verify_token": "verify-token-value"},
		}, 400, false, graphCode(100)},
		{"override without a verify token", http.MethodPost, "/" + testPhone, testToken, map[string]any{
			"webhook_configuration": map[string]any{"override_callback_uri": "https://product.example.test/api/webhook"},
		}, 400, false, graphCode(100)},

		{"update the business profile", http.MethodPost, "/" + testPhone + "/whatsapp_business_profile", testToken, map[string]any{
			"messaging_product": "whatsapp", "about": "Open daily", "websites": []string{"https://clinic.example.test"},
		}, 200, false, has([]any{"success"}, true)},
		{"business profile", http.MethodGet, "/" + testPhone + "/whatsapp_business_profile?fields=about,websites", testToken, nil, 200, true,
			func(t *testing.T, body map[string]any) {
				if dig(body, "data", 0, "about") != "Open daily" || dig(body, "data", 0, "websites", 0) != "https://clinic.example.test" ||
					dig(body, "data", 0, "messaging_product") != "whatsapp" {
					t.Fatalf("profile %v", body)
				}
			}},
		{"business profile with a long about", http.MethodPost, "/" + testPhone + "/whatsapp_business_profile", testToken, map[string]any{
			"messaging_product": "whatsapp", "about": strings.Repeat("a", 140),
		}, 400, false, graphCode(100)},

		{"delete media", http.MethodDelete, "/" + mediaID, testToken, nil, 200, false, has([]any{"success"}, true)},
		{"deleted media", http.MethodGet, "/" + mediaID, testToken, nil, 400, false, graphCode(100)},
		{"delete template", http.MethodDelete, "/" + testWABA + "/message_templates?name=visit_reminder", testToken, nil, 200, false, has([]any{"success"}, true)},
		{"delete a deleted template", http.MethodDelete, "/" + testWABA + "/message_templates?name=visit_reminder", testToken, nil, 400, false, graphCode(100)},
		{"unsubscribe", http.MethodDelete, "/" + testWABA + "/subscribed_apps", testToken, nil, 200, false, has([]any{"success"}, true)},
	}
	for _, tc := range cases {
		prefixes := []string{v}
		if tc.readOnly {
			prefixes = append(prefixes, "", "/v19.0")
		}
		for _, prefix := range prefixes {
			t.Run(tc.name+" "+prefix, func(t *testing.T) {
				status, body := h.graph(tc.method, prefix+tc.path, tc.token, tc.body)
				if status != tc.status {
					t.Fatalf("status %d, want %d (body %v)", status, tc.status, body)
				}
				if tc.check != nil {
					tc.check(t, body)
				}
			})
		}
	}

	routes := map[string]bool{}
	for _, entry := range h.journal() {
		routes[entry.Route] = true
	}
	for _, route := range []string{routePhone, routeWABA, routePhoneNumbers, routeMessages, routeMediaUpload, routeMedia,
		routeMediaDownload, routeTemplates, routeSubscribedApps, routeRegister, routeBusinessProfile, "unsupported"} {
		if !routes[route] {
			t.Errorf("the journal has no %q entry", route)
		}
	}
	// Calls without a valid token are kept out of the journal.
	if routes["auth"] {
		t.Error("a call without a valid token reached the journal")
	}
	rejected := false
	for _, entry := range h.rejected() {
		rejected = rejected || entry.Route == "auth"
	}
	if !rejected {
		t.Error("the rejected ring has no \"auth\" entry")
	}
}

func TestTemplateObjectAndEdit(t *testing.T) {
	h := newHarness(t, nil)
	status, created := h.graph(http.MethodPost, v+"/"+testWABA+"/message_templates", testToken, map[string]any{
		"name": "named_params", "language": "en_US", "category": "MARKETING", "parameter_format": "named",
		"components": []any{map[string]any{"type": "BODY", "text": "Hi {{name}}"}},
	})
	id, _ := created["id"].(string)
	if status != http.StatusOK || id == "" {
		t.Fatalf("create: %d %v", status, created)
	}
	// pkg/whatsapp edits a template at the unversioned /{template_id}.
	if status, body := h.graph(http.MethodPost, "/"+id, testToken, map[string]any{
		"components": []any{map[string]any{"type": "BODY", "text": "Hello {{name}}"}},
	}); status != http.StatusOK {
		t.Fatalf("edit: %d %v", status, body)
	}
	status, object := h.graph(http.MethodGet, v+"/"+id, testToken, nil)
	if status != http.StatusOK || object["parameter_format"] != "NAMED" || dig(object, "components", 0, "text") != "Hello {{name}}" || object["status"] != "PENDING" {
		t.Fatalf("template: %d %v", status, object)
	}
	if status, body := h.graph(http.MethodPost, "/"+id, testToken, map[string]any{"components": "nope"}); status != http.StatusBadRequest {
		t.Fatalf("bad edit: %d %v", status, body)
	}
}

func TestMediaRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	payload := bytes.Repeat([]byte{0x00, 0x7f, 0xff, '\r', '\n'}, 4096)
	status, uploaded := h.uploadMedia(testPhone, "application/pdf", payload)
	mediaID, _ := uploaded["id"].(string)
	if status != http.StatusOK || mediaID == "" {
		t.Fatalf("upload: %d %v", status, uploaded)
	}
	status, metadata := h.graph(http.MethodGet, v+"/"+mediaID, testToken, nil)
	location, _ := metadata["url"].(string)
	if status != http.StatusOK || !strings.HasPrefix(location, h.url+"/") {
		t.Fatalf("metadata: %d %v", status, metadata)
	}
	request, _ := http.NewRequest(http.MethodGet, location, nil)
	request.Header.Set("Authorization", "Bearer "+testToken2)
	response, err := h.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/pdf" || !bytes.Equal(downloaded, payload) {
		t.Fatalf("download: %d %q, %d bytes", response.StatusCode, response.Header.Get("Content-Type"), len(downloaded))
	}
	if metadata["sha256"] != newMedia("", "", payload).SHA256 {
		t.Fatalf("sha256 %v", metadata["sha256"])
	}

	// Uploads for an unknown phone, without a file or over the size limit fail.
	if status, body := h.uploadMedia("999999999999999", "image/png", []byte("x")); status != http.StatusBadRequest || errorCode(t, body) != 100 {
		t.Fatalf("unknown phone: %d %v", status, body)
	}
	if status, body := h.uploadMedia(testPhone, "not a type", []byte("x")); status != http.StatusBadRequest {
		t.Fatalf("bad type: %d %v", status, body)
	}
	if status, _ := h.uploadMedia(testPhone, "video/mp4", make([]byte, maxMediaBytes+65<<10)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d", status)
	}
	request, _ = http.NewRequest(http.MethodPost, h.url+v+"/"+testPhone+"/media", strings.NewReader("plain"))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "text/plain")
	if status, body := h.do(request); status != http.StatusBadRequest {
		t.Fatalf("non-multipart upload: %d %v", status, body)
	}
}

// TestURLsUseTheCallersOrigin: the download URL and paging.next carry the
// scheme and host the caller used, so pkg/whatsapp's same-origin check passes
// behind an https base too.
func TestURLsUseTheCallersOrigin(t *testing.T) {
	h := newHarness(t, nil)
	_, uploaded := h.uploadMedia(testPhone, "image/png", []byte("png"))
	mediaID, _ := uploaded["id"].(string)
	read := func(server *httptest.Server, path, forwarded string) map[string]any {
		request, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		request.Header.Set("Authorization", "Bearer "+testToken)
		if forwarded != "" {
			request.Header.Set("X-Forwarded-Proto", forwarded)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		var decoded map[string]any
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %v", path, response.StatusCode, err)
		}
		return decoded
	}
	host := strings.TrimPrefix(h.url, "http://")
	for forwarded, scheme := range map[string]string{"": "http", "https": "https", "HTTPS, http": "https", "http": "http", "wss": "http"} {
		if got := read(h.server, v+"/"+mediaID, forwarded)["url"]; got != scheme+"://"+host+"/_media/"+mediaID {
			t.Errorf("X-Forwarded-Proto %q: url %v", forwarded, got)
		}
		next, _ := dig(read(h.server, v+"/"+testWABA+"/phone_numbers?limit=1", forwarded), "paging", "next").(string)
		if !strings.HasPrefix(next, scheme+"://"+host+v+"/"+testWABA+"/phone_numbers?") {
			t.Errorf("X-Forwarded-Proto %q: paging.next %q", forwarded, next)
		}
	}
	secure := httptest.NewTLSServer(h.stub)
	defer secure.Close()
	if got := read(secure, v+"/"+mediaID, "")["url"]; got != secure.URL+"/_media/"+mediaID {
		t.Fatalf("over TLS: url %v", got)
	}
}

func TestOAuthAccessToken(t *testing.T) {
	h := newHarness(t, nil)
	post := func(path string, form url.Values) (int, map[string]any) {
		request, _ := http.NewRequest(http.MethodPost, h.url+path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return h.do(request)
	}
	credentials := url.Values{"client_id": {testAppID}, "client_secret": {testAppSecret}, "grant_type": {"client_credentials"}}

	// The Integration Center test posts to the unversioned endpoint.
	status, body := post("/oauth/access_token", credentials)
	appToken, _ := body["access_token"].(string)
	if status != http.StatusOK || !strings.HasPrefix(appToken, testAppID+"|") || strings.Contains(appToken, testAppSecret) {
		t.Fatalf("client_credentials: %d %v", status, body)
	}
	if status, body := post(v+"/oauth/access_token", credentials); status != http.StatusOK || body["access_token"] != appToken {
		t.Fatalf("versioned client_credentials: %d %v", status, body)
	}
	wrong := url.Values{"client_id": {testAppID}, "client_secret": {"another-secret-value"}, "grant_type": {"client_credentials"}}
	if status, body := post("/oauth/access_token", wrong); status != http.StatusBadRequest || errorCode(t, body) != 1 {
		t.Fatalf("wrong secret: %d %v", status, body)
	}
	otherApp := url.Values{"client_id": {"100000000000778"}, "client_secret": {testAppSecret}, "grant_type": {"client_credentials"}}
	if status, _ := post("/oauth/access_token", otherApp); status != http.StatusBadRequest {
		t.Fatalf("wrong app: %d", status)
	}

	// An Embedded Signup code exchanges once, for the token it was issued for.
	exchange := url.Values{"client_id": {testAppID}, "client_secret": {testAppSecret}, "code": {"synthetic-signup-code-1"}}
	if status, body := post(v+"/oauth/access_token", exchange); status != http.StatusBadRequest {
		t.Fatalf("unissued code: %d %v", status, body)
	}
	h.mustControl(http.MethodPost, "/_control/oauth/codes", map[string]any{"code": "synthetic-signup-code-1", "access_token": testToken2})
	if status, body := post(v+"/oauth/access_token", exchange); status != http.StatusOK || body["access_token"] != testToken2 {
		t.Fatalf("code exchange: %d %v", status, body)
	}
	if status, _ := post(v+"/oauth/access_token", exchange); status != http.StatusBadRequest {
		t.Fatalf("a code exchanged twice: %d", status)
	}
	if status, body := h.control(http.MethodPost, "/_control/oauth/codes", map[string]any{"code": "synthetic-signup-code-2", "access_token": "not-a-configured-token"}); status != http.StatusBadRequest {
		t.Fatalf("code for an unknown token: %d %v", status, body)
	}
	if status, _ := post("/oauth/access_token", url.Values{"client_id": {testAppID}, "client_secret": {testAppSecret}, "grant_type": {"fb_exchange_token"}}); status != http.StatusBadRequest {
		t.Fatalf("unsupported grant: %d", status)
	}
}

func TestDebugToken(t *testing.T) {
	h := newHarness(t, nil)
	appToken := testAppID + "|" + testAppSecret
	status, body := h.graph(http.MethodGet, "/debug_token?input_token="+url.QueryEscape(testToken), appToken, nil)
	if status != http.StatusOK || dig(body, "data", "is_valid") != true || dig(body, "data", "app_id") != testAppID {
		t.Fatalf("debug_token: %d %v", status, body)
	}
	for index, scope := range []string{"whatsapp_business_management", "whatsapp_business_messaging"} {
		if dig(body, "data", "granular_scopes", index, "scope") != scope ||
			dig(body, "data", "granular_scopes", index, "target_ids", 0) != testWABA ||
			dig(body, "data", "granular_scopes", index, "target_ids", 1) != testWABA2 {
			t.Fatalf("granular scope %d: %v", index, body)
		}
	}
	if expires, _ := dig(body, "data", "data_access_expires_at").(float64); expires <= 0 {
		t.Fatalf("data_access_expires_at %v", expires)
	}

	_, issued := h.graph(http.MethodPost, "/oauth/access_token?client_id="+testAppID+"&client_secret="+testAppSecret+"&grant_type=client_credentials", "", nil)
	if status, body := h.graph(http.MethodGet, v+"/debug_token?input_token=unknown-token-value", issued["access_token"].(string), nil); status != http.StatusOK || dig(body, "data", "is_valid") != false {
		t.Fatalf("invalid input token: %d %v", status, body)
	}
	if status, body := h.graph(http.MethodGet, "/debug_token?input_token="+testToken, testToken, nil); status != http.StatusUnauthorized || errorCode(t, body) != 190 {
		t.Fatalf("a user token as the app token: %d %v", status, body)
	}
	if status, body := h.graph(http.MethodGet, "/debug_token", appToken, nil); status != http.StatusBadRequest {
		t.Fatalf("missing input_token: %d %v", status, body)
	}
}

func TestRequestedFields(t *testing.T) {
	request, _ := http.NewRequest(http.MethodGet, "/x?fields="+url.QueryEscape("id, name,phone_numbers{id,display_phone_number},preview.invalidate(false),,status"), nil)
	got := strings.Join(requestedFields(request), ",")
	if got != "id,name,phone_numbers,preview,status" {
		t.Fatalf("fields %q", got)
	}
}

func TestGraphSegments(t *testing.T) {
	for path, want := range map[string]string{
		"/v21.0/123/messages":  "123/messages",
		"/123/messages":        "123/messages",
		"/v1.2":                "",
		"/v21/123":             "v21/123",
		"//123":                "123",
		"//v21.0/123/messages": "123/messages",
		"///debug_token":       "debug_token",
		"/123//messages":       "",
		"/123/messages/":       "",
		"//":                   "",
		"/":                    "",
	} {
		if got := strings.Join(graphSegments(path), "/"); got != want {
			t.Errorf("graphSegments(%q) = %q, want %q", path, got, want)
		}
	}
}
