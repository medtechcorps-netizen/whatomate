package whatsapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/stretchr/testify/require"
)

func TestClient_SubmitTemplate_FooterOrderingAndValidation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var payload struct {
			Components []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"components"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Len(t, payload.Components, 4)
		require.Equal(t, "HEADER", payload.Components[0].Type)
		require.Equal(t, "BODY", payload.Components[1].Type)
		require.Equal(t, "FOOTER", payload.Components[2].Type)
		require.Equal(t, strings.Repeat("界", 60), payload.Components[2].Text)
		require.Equal(t, "BUTTONS", payload.Components[3].Type)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "template-footer"})
	}))
	defer server.Close()
	client := newTestClient(t, server)
	template := &whatsapp.TemplateSubmission{Name: "footer_test", Language: "en", Category: "UTILITY", HeaderType: "TEXT", HeaderContent: "Order", BodyContent: "Your order is ready.", FooterContent: "  " + strings.Repeat("界", 60) + "  ", Buttons: []any{map[string]any{"type": "QUICK_REPLY", "text": "Thanks"}}}
	_, err := client.SubmitTemplate(context.Background(), testAccount(server.URL), template)
	require.NoError(t, err)
	for _, footer := range []string{strings.Repeat("界", 61), "Hello {{name}}"} {
		template.FooterContent = footer
		_, err = client.SubmitTemplate(context.Background(), testAccount(server.URL), template)
		require.Error(t, err)
	}
	require.Equal(t, 1, calls, "invalid footers must fail before calling Meta")
}
