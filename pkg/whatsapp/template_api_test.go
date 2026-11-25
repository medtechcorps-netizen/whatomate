package whatsapp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, server *httptest.Server) *whatsapp.Client {
	t.Helper()
	log := testutil.NopLogger()
	client := whatsapp.NewWithTimeout(log, 5*time.Second)
	client.HTTPClient = &http.Client{
		Transport: &testServerTransport{serverURL: server.URL},
	}
	return client
}

// --- SubmitTemplate ---

func TestClient_SubmitTemplate_Success(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Contains(t, r.URL.Path, "/message_templates")
		assert.Equal(t, "Bearer test-access-token", r.Header.Get("Authorization"))

		var body map[string]any
		err := json.NewDecoder(r.Body).Decode(&body)
		require.NoError(t, err)
		assert.Equal(t, "hello_world", body["name"])
		assert.Equal(t, "en", body["language"])
		assert.Equal(t, "MARKETING", body["category"])

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "tmpl-123"})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)
	ctx := context.Background()

	tmpl := &whatsapp.TemplateSubmission{
		Name:        "hello_world",
		Language:    "en",
		Category:    "MARKETING",
		BodyContent: "Hello! Welcome to our service.",
	}

	id, err := client.SubmitTemplate(ctx, account, tmpl)
	require.NoError(t, err)
	assert.Equal(t, "tmpl-123", id)
}

func TestClient_SubmitTemplate_WithVariables(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		components := body["components"].([]any)
		// Should have BODY component
		var bodyComp map[string]any
		for _, c := range components {
			comp := c.(map[string]any)
			if comp["type"] == "BODY" {
				bodyComp = comp
			}
		}
		require.NotNil(t, bodyComp)
		assert.Equal(t, "Hello {{1}}! Your order {{2}} is ready.", bodyComp["text"])

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "tmpl-456"})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)
	ctx := context.Background()

	tmpl := &whatsapp.TemplateSubmission{
		Name:        "order_ready",
		Language:    "en",
		Category:    "UTILITY",
		BodyContent: "Hello {{1}}! Your order {{2}} is ready.",
		SampleValues: []any{
			map[string]any{"component": "body", "index": 1, "value": "John"},
			map[string]any{"component": "body", "index": 2, "value": "ORD-123"},
		},
	}

	id, err := client.SubmitTemplate(ctx, account, tmpl)
	require.NoError(t, err)
	assert.Equal(t, "tmpl-456", id)
}

func TestClient_SubmitTemplate_APIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(whatsapp.MetaAPIError{
			Error: struct {
				Message      string `json:"message"`
				Type         string `json:"type"`
				Code         int    `json:"code"`
				ErrorSubcode int    `json:"error_subcode"`
				ErrorUserMsg string `json:"error_user_msg"`
				ErrorData    struct {
					Details string `json:"details"`
				} `json:"error_data"`
				FBTraceID string `json:"fbtrace_id"`
			}{
				Message: "Invalid template name",
				Code:    100,
			},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)
	ctx := context.Background()

	tmpl := &whatsapp.TemplateSubmission{
		Name:        "",
		Language:    "en",
		Category:    "MARKETING",
		BodyContent: "Hello",
	}

	_, err := client.SubmitTemplate(ctx, account, tmpl)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Invalid template name")
}

func TestClient_SubmitTemplate_MissingVariableSamples(t *testing.T) {
	t.Parallel()

	// SubmitTemplate should fail if body has variables but no samples
	log := testutil.NopLogger()
	client := whatsapp.NewWithTimeout(log, 5*time.Second)

	account := &whatsapp.Account{
		PhoneID:     "123",
		BusinessID:  "456",
		APIVersion:  "v21.0",
		AccessToken: "token",
	}

	tmpl := &whatsapp.TemplateSubmission{
		Name:         "test",
		Language:     "en",
		Category:     "UTILITY",
		BodyContent:  "Hello {{1}}!",
		SampleValues: []any{},
	}

	_, err := client.SubmitTemplate(context.Background(), account, tmpl)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sample values are required")
}

// --- FetchTemplate ---

func TestClient_FetchTemplate_Success(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/v21.0/1234567890", r.URL.Path)
		assert.Equal(t, "id,name,language,status", r.URL.Query().Get("fields"))
		assert.Equal(t, "Bearer test-access-token", r.Header.Get("Authorization"))

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "1234567890", "name": "order_update", "language": "en_US", "status": "PAUSED",
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	template, err := client.FetchTemplate(context.Background(), testAccount(server.URL), " 1234567890 ")
	require.NoError(t, err)
	assert.Equal(t, "1234567890", template.ID)
	assert.Equal(t, "order_update", template.Name)
	assert.Equal(t, "en_US", template.Language)
	assert.Equal(t, "PAUSED", template.Status)
}

func TestClient_FetchTemplate_RejectsInvalidInput(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.FetchTemplate(context.Background(), testAccount(server.URL), "123/../456")
	require.Error(t, err)
	account := testAccount(server.URL)
	account.APIVersion = "latest"
	_, err = client.FetchTemplate(context.Background(), account, "1234567890")
	require.Error(t, err)
	_, err = client.FetchTemplate(context.Background(), nil, "1234567890")
	require.Error(t, err)
}

func TestClient_FetchTemplate_APIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Unsupported get request","type":"GraphMethodException","code":100}}`))
	}))
	defer server.Close()

	client := newTestClient(t, server)
	_, err := client.FetchTemplate(context.Background(), testAccount(server.URL), "1234567890")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Unsupported get request")
}

// --- FetchTemplates ---

func TestClient_FetchTemplates_Success(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Contains(t, r.URL.Path, "/message_templates")

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "1", "name": "hello", "language": "en", "category": "MARKETING", "status": "APPROVED", "quality_rating": "HIGH"},
				{"id": "2", "name": "goodbye", "language": "en", "category": "UTILITY", "status": "PENDING", "quality_rating": "UNKNOWN"},
			},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)

	templates, err := client.FetchTemplates(context.Background(), account)
	require.NoError(t, err)
	require.Len(t, templates, 2)
	assert.Equal(t, "hello", templates[0].Name)
	assert.Equal(t, "APPROVED", templates[0].Status)
	assert.Equal(t, "HIGH", templates[0].QualityRating)
	assert.Equal(t, "goodbye", templates[1].Name)
	assert.Equal(t, "UNKNOWN", templates[1].QualityRating)
}

func TestClient_FetchTemplates_PaginatesPinnedAccount(t *testing.T) {
	t.Parallel()

	account := testAccount("")
	const firstCursor = "opaque+/=a&v=%"
	const secondCursor = "another+opaque"
	pages := []struct {
		after string
		body  string
	}{
		{"", `{"data":[{"id":"1","name":"first"}],"paging":{"next":"/wrong-edge","cursors":{"after":"` + firstCursor + `"}}}`},
		{firstCursor, `{"data":[],"paging":{"next":"/wrong-waba","cursors":{"after":"` + secondCursor + `"}}}`},
		{secondCursor, `{"data":[{"id":"2","name":"last"}],"paging":{"cursors":{"after":"not-a-next-page"}}}`},
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := int(calls.Add(1)) - 1
		if page >= len(pages) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/"+account.APIVersion+"/"+account.BusinessID+"/message_templates", r.URL.Path)
		assert.Equal(t, "Bearer "+account.AccessToken, r.Header.Get("Authorization"))
		query := r.URL.Query()
		assert.Equal(t, "100", query.Get("limit"))
		assert.Equal(t, "id,name,language,category,status,components,quality_score,quality_rating", query.Get("fields"))
		if pages[page].after == "" {
			assert.False(t, query.Has("after"))
			assert.Len(t, query, 2)
		} else {
			assert.Equal(t, []string{pages[page].after}, query["after"])
			assert.Len(t, query, 3)
		}
		_, _ = w.Write([]byte(pages[page].body))
	}))
	defer server.Close()
	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)

	templates, err := client.FetchTemplates(context.Background(), account)
	require.NoError(t, err)
	require.Len(t, templates, 2)
	assert.Equal(t, "first", templates[0].Name)
	assert.Equal(t, "last", templates[1].Name)
	assert.EqualValues(t, 3, calls.Load())
}

func TestClient_FetchTemplates_RejectsIncompletePagination(t *testing.T) {
	t.Parallel()

	type response struct {
		status int
		body   string
	}
	const cursor = "private-opaque-token"
	continuation := func(after string) response {
		return response{http.StatusOK, fmt.Sprintf(`{"data":[{"id":"1"}],"paging":{"next":"/ignored","cursors":{"after":%q}}}`, after)}
	}
	pageLimit := make([]response, 100)
	for i := range pageLimit {
		pageLimit[i] = continuation(fmt.Sprintf("cursor-%d", i))
	}
	for _, tc := range []struct {
		name  string
		pages []response
		error string
	}{
		{"missing cursor", []response{continuation("")}, "invalid continuation cursor"},
		{"invalid cursor", []response{continuation("unsafe cursor")}, "invalid continuation cursor"},
		{"oversize cursor", []response{continuation(strings.Repeat("a", 4097))}, "invalid continuation cursor"},
		{"repeated cursor", []response{continuation(cursor), continuation(cursor)}, "repeated a continuation cursor"},
		{"later HTTP failure", []response{continuation(cursor), {http.StatusBadGateway, `{"error":{"message":"` + cursor + `"}}`}}, "HTTP status 502"},
		{"later missing data", []response{continuation(cursor), {http.StatusOK, `{}`}}, "invalid template page 2"},
		{"page limit", pageLimit, "pagination exceeded 100 pages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				i := int(calls.Add(1)) - 1
				if i >= len(tc.pages) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(tc.pages[i].status)
				_, _ = w.Write([]byte(tc.pages[i].body))
			}))
			defer server.Close()
			client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
			templates, err := client.FetchTemplates(context.Background(), testAccount(server.URL))
			require.ErrorContains(t, err, tc.error)
			assert.NotContains(t, err.Error(), cursor)
			assert.Nil(t, templates, "an unfinished list must not reach the caller")
			assert.EqualValues(t, len(tc.pages), calls.Load())
		})
	}
}

func TestClient_FetchTemplates_Empty(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)

	templates, err := client.FetchTemplates(context.Background(), account)
	require.NoError(t, err)
	assert.Empty(t, templates)
}

func TestClient_FetchTemplates_APIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(whatsapp.MetaAPIError{
			Error: struct {
				Message      string `json:"message"`
				Type         string `json:"type"`
				Code         int    `json:"code"`
				ErrorSubcode int    `json:"error_subcode"`
				ErrorUserMsg string `json:"error_user_msg"`
				ErrorData    struct {
					Details string `json:"details"`
				} `json:"error_data"`
				FBTraceID string `json:"fbtrace_id"`
			}{
				Message: "Invalid access token",
				Code:    190,
			},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)

	_, err := client.FetchTemplates(context.Background(), account)
	require.Error(t, err)
}

// --- DeleteTemplate ---

func TestClient_DeleteTemplate_Success(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Contains(t, r.URL.Path, "/message_templates")
		assert.Contains(t, r.URL.RawQuery, "name=hello_world")

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)

	err := client.DeleteTemplate(context.Background(), account, "hello_world")
	require.NoError(t, err)
}

func TestClient_DeleteTemplate_APIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(whatsapp.MetaAPIError{
			Error: struct {
				Message      string `json:"message"`
				Type         string `json:"type"`
				Code         int    `json:"code"`
				ErrorSubcode int    `json:"error_subcode"`
				ErrorUserMsg string `json:"error_user_msg"`
				ErrorData    struct {
					Details string `json:"details"`
				} `json:"error_data"`
				FBTraceID string `json:"fbtrace_id"`
			}{
				Message: "Template not found",
				Code:    100,
			},
		})
	}))
	defer server.Close()

	client := newTestClient(t, server)
	account := testAccount(server.URL)

	err := client.DeleteTemplate(context.Background(), account, "nonexistent")
	require.Error(t, err)
}
