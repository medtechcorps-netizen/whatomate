package whatsapp_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testAccount returns a test WhatsApp account configured to use the test server.
func testAccount(serverURL string) *whatsapp.Account {
	return &whatsapp.Account{
		PhoneID:     "123456789",
		BusinessID:  "987654321",
		APIVersion:  "v21.0",
		AccessToken: "test-access-token",
	}
}

func TestClient_SendTextMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		phone           string
		text            string
		serverResponse  func(t *testing.T, w http.ResponseWriter, r *http.Request)
		wantMessageID   string
		wantErr         bool
		wantErrContains string
	}{
		{
			name:  "successful send",
			phone: "1234567890",
			text:  "Hello, World!",
			serverResponse: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				// Verify request method and path
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Contains(t, r.URL.Path, "/messages")

				// Verify headers
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
				assert.Equal(t, "Bearer test-access-token", r.Header.Get("Authorization"))

				// Verify body
				var body map[string]any
				err := json.NewDecoder(r.Body).Decode(&body)
				require.NoError(t, err)
				assert.Equal(t, "whatsapp", body["messaging_product"])
				assert.Equal(t, "1234567890", body["to"])
				assert.Equal(t, "text", body["type"])

				textContent := body["text"].(map[string]any)
				assert.Equal(t, "Hello, World!", textContent["body"])

				// Return success
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"messages": []map[string]string{{"id": "wamid.test123"}},
				})
			},
			wantMessageID: "wamid.test123",
			wantErr:       false,
		},
		{
			name:  "API error - invalid phone",
			phone: "invalid",
			text:  "Hello",
			serverResponse: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
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
						Message: "Invalid phone number format",
						Code:    100,
					},
				})
			},
			wantErr:         true,
			wantErrContains: "Invalid phone number format",
		},
		{
			name:  "API error - unauthorized",
			phone: "1234567890",
			text:  "Hello",
			serverResponse: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
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
			},
			wantErr:         true,
			wantErrContains: "Invalid access token",
		},
		{
			name:  "empty message ID in response",
			phone: "1234567890",
			text:  "Hello",
			serverResponse: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"messages": []map[string]string{}, // Empty
				})
			},
			wantErr:         true,
			wantErrContains: "no message ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Create test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tt.serverResponse(t, w, r)
			}))
			defer server.Close()

			// Create client with custom HTTP client that redirects to test server
			log := testutil.NopLogger()
			client := whatsapp.NewWithTimeout(log, 5*time.Second)

			// Override HTTP client to use test server
			client.HTTPClient = &http.Client{
				Transport: &testServerTransport{serverURL: server.URL},
			}

			account := testAccount(server.URL)
			ctx := testutil.TestContext(t)

			msgID, err := client.SendTextMessage(ctx, account, whatsapp.Recipient{Phone: tt.phone}, tt.text)

			if tt.wantErr {
				require.Error(t, err)
				if tt.wantErrContains != "" {
					assert.Contains(t, err.Error(), tt.wantErrContains)
				}
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantMessageID, msgID)
		})
	}
}

func TestClient_ConfigurePhoneWebhookOverride_UsesPhoneEndpointAndVerifiesReadback(t *testing.T) {
	t.Parallel()

	const callbackURL = "https://app.rereply.app/api/webhook"
	const verifyToken = "test-verify-token"
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, "/v21.0/123456789", r.URL.Path)
		assert.Equal(t, "Bearer test-access-token", r.Header.Get("Authorization"))

		switch r.Method {
		case http.MethodPost:
			assert.Empty(t, r.URL.RawQuery)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			configuration, ok := body["webhook_configuration"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, callbackURL, configuration["override_callback_uri"])
			assert.Equal(t, verifyToken, configuration["verify_token"])
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
		case http.MethodGet:
			assert.Equal(t, "webhook_configuration", r.URL.Query().Get("fields"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"webhook_configuration": map[string]string{"phone_number": callbackURL},
			})
		default:
			t.Fatalf("unexpected request method %s", r.Method)
		}
	}))
	defer server.Close()

	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
	err := client.ConfigurePhoneWebhookOverride(
		testutil.TestContext(t),
		testAccount(server.URL),
		callbackURL,
		verifyToken,
	)
	require.NoError(t, err)
	assert.Equal(t, 2, requests)
}

func TestClient_ConfigurePhoneWebhookOverride_FailsClosedOnInvalidOrUnverifiedInput(t *testing.T) {
	t.Parallel()

	const callbackURL = "https://app.rereply.app/api/webhook"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"webhook_configuration": map[string]string{"phone_number": "https://unexpected.example/api/webhook"},
		})
	}))
	defer server.Close()

	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
	ctx := testutil.TestContext(t)

	invalidPhone := testAccount(server.URL)
	invalidPhone.PhoneID = "123/override"
	err := client.ConfigurePhoneWebhookOverride(ctx, invalidPhone, callbackURL, "verify-token")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid WhatsApp phone ID")

	invalidVersion := testAccount(server.URL)
	invalidVersion.APIVersion = "v21.0/anything"
	err = client.ConfigurePhoneWebhookOverride(ctx, invalidVersion, callbackURL, "verify-token")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid WhatsApp API version")

	err = client.ConfigurePhoneWebhookOverride(ctx, testAccount(server.URL), callbackURL, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verify token")

	err = client.ConfigurePhoneWebhookOverride(ctx, testAccount(server.URL), "http://not-https.example/api/webhook", "verify-token")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid webhook callback URL")

	err = client.ConfigurePhoneWebhookOverride(ctx, testAccount(server.URL), callbackURL, "verify-token")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not report the expected callback")
}

func TestClient_GetMediaURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		mediaID         string
		serverResponse  func(t *testing.T, w http.ResponseWriter, r *http.Request)
		wantURL         string
		wantErr         bool
		wantErrContains string
	}{
		{
			name:    "successful get",
			mediaID: "media123",
			serverResponse: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Contains(t, r.URL.Path, "media123")

				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(whatsapp.MediaURLResponse{
					URL:      "https://cdn.whatsapp.net/media/test123",
					MimeType: "image/jpeg",
				})
			},
			wantURL: "https://cdn.whatsapp.net/media/test123",
			wantErr: false,
		},
		{
			name:    "media not found",
			mediaID: "nonexistent",
			serverResponse: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
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
						Message: "Media not found",
						Code:    100,
					},
				})
			},
			wantErr:         true,
			wantErrContains: "Media not found",
		},
		{
			name:    "empty URL in response",
			mediaID: "media123",
			serverResponse: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(whatsapp.MediaURLResponse{
					URL: "", // Empty URL
				})
			},
			wantErr:         true,
			wantErrContains: "no URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tt.serverResponse(t, w, r)
			}))
			defer server.Close()

			log := testutil.NopLogger()
			client := whatsapp.NewWithTimeout(log, 5*time.Second)
			client.HTTPClient = &http.Client{
				Transport: &testServerTransport{serverURL: server.URL},
			}

			account := testAccount(server.URL)
			ctx := testutil.TestContext(t)

			url, err := client.GetMediaURL(ctx, tt.mediaID, account)

			if tt.wantErr {
				require.Error(t, err)
				if tt.wantErrContains != "" {
					assert.Contains(t, err.Error(), tt.wantErrContains)
				}
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantURL, url)
		})
	}
}

func TestClient_DownloadMedia(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		serverResponse func(t *testing.T, w http.ResponseWriter, r *http.Request)
		wantData       []byte
		wantErr        bool
	}{
		{
			name: "successful download",
			serverResponse: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "Bearer test-access-token", r.Header.Get("Authorization"))

				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("fake image data"))
			},
			wantData: []byte("fake image data"),
			wantErr:  false,
		},
		{
			name: "download failed",
			serverResponse: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tt.serverResponse(t, w, r)
			}))
			defer server.Close()

			log := testutil.NopLogger()
			client := whatsapp.NewWithTimeout(log, 5*time.Second)

			ctx := testutil.TestContext(t)

			data, err := client.DownloadMedia(ctx, server.URL+"/media/test", "test-access-token")

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantData, data)
		})
	}
}

func TestClient_MarkMessageRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		messageID      string
		serverResponse func(t *testing.T, w http.ResponseWriter, r *http.Request)
		wantErr        bool
	}{
		{
			name:      "successful mark read",
			messageID: "wamid.test123",
			serverResponse: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)

				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				assert.Equal(t, "read", body["status"])
				assert.Equal(t, "wamid.test123", body["message_id"])

				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
			},
			wantErr: false,
		},
		{
			name:      "message not found",
			messageID: "wamid.invalid",
			serverResponse: func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tt.serverResponse(t, w, r)
			}))
			defer server.Close()

			log := testutil.NopLogger()
			client := whatsapp.NewWithTimeout(log, 5*time.Second)
			client.HTTPClient = &http.Client{
				Transport: &testServerTransport{serverURL: server.URL},
			}

			account := testAccount(server.URL)
			ctx := testutil.TestContext(t)

			err := client.MarkMessageRead(ctx, account, tt.messageID)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
		})
	}
}

func TestClient_SendImageMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		assert.Equal(t, "image", body["type"])
		image := body["image"].(map[string]any)
		assert.Equal(t, "media123", image["id"])
		assert.Equal(t, "Test caption", image["caption"])

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": "wamid.img123"}},
		})
	}))
	defer server.Close()

	log := testutil.NopLogger()
	client := whatsapp.NewWithTimeout(log, 5*time.Second)
	client.HTTPClient = &http.Client{
		Transport: &testServerTransport{serverURL: server.URL},
	}

	account := testAccount(server.URL)
	ctx := testutil.TestContext(t)

	msgID, err := client.SendImageMessage(ctx, account, whatsapp.Recipient{Phone: "1234567890"}, "media123", "Test caption")

	require.NoError(t, err)
	assert.Equal(t, "wamid.img123", msgID)
}

func TestClient_SendDocumentMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		assert.Equal(t, "document", body["type"])
		doc := body["document"].(map[string]any)
		assert.Equal(t, "media456", doc["id"])
		assert.Equal(t, "report.pdf", doc["filename"])
		assert.Equal(t, "Monthly report", doc["caption"])

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": "wamid.doc123"}},
		})
	}))
	defer server.Close()

	log := testutil.NopLogger()
	client := whatsapp.NewWithTimeout(log, 5*time.Second)
	client.HTTPClient = &http.Client{
		Transport: &testServerTransport{serverURL: server.URL},
	}

	account := testAccount(server.URL)
	ctx := testutil.TestContext(t)

	msgID, err := client.SendDocumentMessage(ctx, account, whatsapp.Recipient{Phone: "1234567890"}, "media456", "report.pdf", "Monthly report")

	require.NoError(t, err)
	assert.Equal(t, "wamid.doc123", msgID)
}

func TestClient_GetWABAPhoneNumbersPaginatesWithTrustedCursorAndDeduplicates(t *testing.T) {
	t.Parallel()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/v21.0/220000000000101/phone_numbers", r.URL.Path)
		assert.Equal(t, "Bearer pagination-token", r.Header.Get("Authorization"))
		assert.Equal(t, "100", r.URL.Query().Get("limit"))
		assert.Equal(t, "id,display_phone_number,verified_name,quality_rating", r.URL.Query().Get("fields"))
		switch calls {
		case 1:
			assert.Empty(t, r.URL.Query().Get("after"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"id": "110000000000101", "verified_name": "One"}},
				"paging": map[string]any{
					"cursors": map[string]string{"after": "cursor one&next=https://attacker.invalid"},
					"next":    "https://attacker.invalid/steal?access_token=must-not-leak",
				},
			})
		case 2:
			assert.Equal(t, "cursor one&next=https://attacker.invalid", r.URL.Query().Get("after"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{
					{"id": " 110000000000101 ", "verified_name": "Duplicate"},
					{"id": "110000000000102", "verified_name": "Two"},
				},
			})
		default:
			t.Fatalf("unexpected pagination request %d", calls)
		}
	}))
	defer server.Close()

	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
	result, err := client.GetWABAPhoneNumbers(
		testutil.TestContext(t),
		"220000000000101",
		"pagination-token",
		"v21.0",
	)
	require.NoError(t, err)
	require.Len(t, result.Data, 2)
	assert.Equal(t, "110000000000101", result.Data[0].ID)
	assert.Equal(t, "110000000000102", result.Data[1].ID)
	assert.Equal(t, 2, calls)
}

func TestClient_ValidateCredentialsFindsSelectedPhoneOnSecondPage(t *testing.T) {
	t.Parallel()

	var phonePageCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v21.0/110000000000201":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"display_phone_number":     "+60123456789",
				"verified_name":            "Selected Clinic",
				"code_verification_status": "VERIFIED",
				"account_mode":             "LIVE",
				"quality_rating":           "GREEN",
				"platform_type":            "CLOUD_API",
			})
		case "/v21.0/220000000000201":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "220000000000201", "name": "Selected WABA"})
		case "/v21.0/220000000000201/phone_numbers":
			phonePageCalls++
			if phonePageCalls == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": []map[string]string{{"id": "110000000000202"}},
					"paging": map[string]any{
						"cursors": map[string]string{"after": "second-page"},
						"next":    "https://graph.facebook.com/ignored",
					},
				})
				return
			}
			assert.Equal(t, "second-page", r.URL.Query().Get("after"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"id": "110000000000201"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
	result, err := client.ValidateCredentials(
		testutil.TestContext(t),
		"110000000000201",
		"220000000000201",
		"selected-token",
		"v21.0",
	)
	require.NoError(t, err)
	assert.Equal(t, "Selected Clinic", result.VerifiedName)
	assert.Equal(t, 2, phonePageCalls)
}

func TestClient_GetWABAPhoneNumbersFailsClosedOnInvalidOrUnboundedPagination(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name          string
		response      func(int, *http.Request) map[string]any
		wantError     string
		wantCallCount int
	}{
		{
			name: "missing cursor",
			response: func(_ int, _ *http.Request) map[string]any {
				return map[string]any{
					"data":   []map[string]string{{"id": "110000000000301"}},
					"paging": map[string]any{"next": "https://graph.facebook.com/next"},
				}
			},
			wantError:     "omitted its continuation cursor",
			wantCallCount: 1,
		},
		{
			name: "repeated cursor",
			response: func(_ int, _ *http.Request) map[string]any {
				return map[string]any{
					"data": []map[string]string{{"id": "110000000000301"}},
					"paging": map[string]any{
						"cursors": map[string]string{"after": "same-cursor"},
						"next":    "https://graph.facebook.com/next",
					},
				}
			},
			wantError:     "repeated a continuation cursor",
			wantCallCount: 2,
		},
		{
			name: "page cap",
			response: func(call int, _ *http.Request) map[string]any {
				return map[string]any{
					"data": []map[string]string{{"id": fmt.Sprintf("110000000%06d", call)}},
					"paging": map[string]any{
						"cursors": map[string]string{"after": fmt.Sprintf("cursor-%d", call)},
						"next":    "https://graph.facebook.com/next",
					},
				}
			},
			wantError:     "exceeded 100 pages",
			wantCallCount: 100,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				_ = json.NewEncoder(w).Encode(testCase.response(calls, r))
			}))
			defer server.Close()
			client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
			_, err := client.GetWABAPhoneNumbers(
				testutil.TestContext(t),
				"220000000000301",
				"bounded-token",
				"v21.0",
			)
			require.ErrorContains(t, err, testCase.wantError)
			assert.Equal(t, testCase.wantCallCount, calls)
		})
	}
}

func TestClient_GraphIdentifiersRejectPathOrQueryInjectionBeforeHTTP(t *testing.T) {
	t.Parallel()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "must not be reached", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
	ctx := testutil.TestContext(t)

	for _, testCase := range []struct {
		name string
		call func() error
	}{
		{
			name: "phone path injection",
			call: func() error {
				_, err := client.ValidateCredentials(ctx, "110/evil", "220000000000501", "token", "v21.0")
				return err
			},
		},
		{
			name: "WABA query injection",
			call: func() error {
				_, err := client.ValidateCredentials(ctx, "110000000000501", "220?fields=secrets", "token", "v21.0")
				return err
			},
		},
		{
			name: "API version injection",
			call: func() error {
				_, err := client.ValidateCredentials(ctx, "110000000000501", "220000000000501", "token", "v21.0/evil")
				return err
			},
		},
		{
			name: "provider WABA path injection",
			call: func() error {
				_, err := client.GetWABAPhoneNumbers(ctx, "220000000000501/phone_numbers", "token", "v21.0")
				return err
			},
		},
		{
			name: "subscription app query injection",
			call: func() error {
				_, err := client.IsAppSubscribed(ctx, "220000000000501", "1717?access_token=evil", "token", "v21.0")
				return err
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			require.Error(t, testCase.call())
			assert.Zero(t, calls)
		})
	}
}

func TestClient_IsAppSubscribedUsesReadOnlyTrustedPagination(t *testing.T) {
	t.Parallel()

	const (
		wabaID = "220000000000601"
		appID  = "990000000000001"
		token  = "subscription-read-token"
	)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/v21.0/"+wabaID+"/subscribed_apps", r.URL.Path)
		assert.Equal(t, "Bearer "+token, r.Header.Get("Authorization"))
		assert.Equal(t, "100", r.URL.Query().Get("limit"))
		switch calls {
		case 1:
			assert.Empty(t, r.URL.Query().Get("after"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"whatsapp_business_api_data": map[string]string{"id": "990000000000000"},
				}},
				"paging": map[string]any{
					"cursors": map[string]string{"after": "safe-cursor"},
					"next":    "https://attacker.invalid/steal-token",
				},
			})
		case 2:
			assert.Equal(t, "safe-cursor", r.URL.Query().Get("after"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"whatsapp_business_api_data": map[string]string{"id": appID},
				}},
			})
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
	subscribed, err := client.IsAppSubscribed(
		testutil.TestContext(t),
		wabaID,
		appID,
		token,
		"v21.0",
	)
	require.NoError(t, err)
	assert.True(t, subscribed)
	assert.Equal(t, 2, calls)
}

func TestClient_IsAppSubscribedFailsClosedOnMalformedProviderData(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"whatsapp_business_api_data": map[string]string{"id": "not-a-graph-id"},
			}},
		})
	}))
	defer server.Close()

	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
	subscribed, err := client.IsAppSubscribed(
		testutil.TestContext(t),
		"220000000000602",
		"990000000000001",
		"subscription-read-token",
		"v21.0",
	)
	require.ErrorContains(t, err, "invalid subscribed app_id")
	assert.False(t, subscribed)
}

func TestIsDefiniteProviderRejectionClassifiesNonIdempotentFailuresConservatively(t *testing.T) {
	t.Parallel()

	definite := whatsapp.ParseMetaAPIError(http.StatusBadRequest, []byte(`{
		"error":{"message":"PIN rejected","code":33,"is_transient":false}
	}`))
	assert.True(t, whatsapp.IsDefiniteProviderRejection(fmt.Errorf("register: %w", definite)))

	for _, ambiguous := range []error{
		whatsapp.ParseMetaAPIError(http.StatusBadRequest, []byte(`{
			"error":{"message":"transience omitted","code":33}
		}`)),
		whatsapp.ParseMetaAPIError(http.StatusBadRequest, []byte(`{
			"error":{"message":"temporary provider condition","code":33,"is_transient":true}
		}`)),
		whatsapp.ParseMetaAPIError(http.StatusRequestTimeout, []byte(`{
			"error":{"message":"provider timeout","code":33,"is_transient":false}
		}`)),
		whatsapp.ParseMetaAPIError(http.StatusTooEarly, []byte(`{
			"error":{"message":"too early","code":33,"is_transient":false}
		}`)),
		whatsapp.ParseMetaAPIError(http.StatusTooManyRequests, []byte(`{
			"error":{"message":"rate limited","code":33,"is_transient":false}
		}`)),
		whatsapp.ParseMetaAPIError(http.StatusBadRequest, []byte(`not-json`)),
		whatsapp.ParseMetaAPIError(http.StatusInternalServerError, []byte(`{
			"error":{"message":"server failed","code":33,"is_transient":false}
		}`)),
		whatsapp.ParseMetaAPIError(http.StatusBadRequest, []byte(`{
			"error":{"message":"temporary Meta code","code":2,"is_transient":false}
		}`)),
		errors.New("transport timeout"),
	} {
		assert.False(t, whatsapp.IsDefiniteProviderRejection(ambiguous), ambiguous.Error())
	}
}

// testServerTransport redirects all requests to the test server
type testServerTransport struct {
	serverURL string
}

func (t *testServerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Replace the host with test server
	testReq := req.Clone(req.Context())
	testReq.URL.Scheme = "http"
	testReq.URL.Host = t.serverURL[7:] // Remove "http://"
	return http.DefaultTransport.RoundTrip(testReq)
}
