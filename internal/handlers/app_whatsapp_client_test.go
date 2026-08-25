package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWhatsAppClientUsesConfiguredBaseURLWhenSharedClientIsMissing(t *testing.T) {
	t.Parallel()

	paths := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.handler-local"}]}`))
	}))
	t.Cleanup(server.Close)

	app := &App{
		Config: &config.Config{
			WhatsApp: config.WhatsAppConfig{BaseURL: "  " + server.URL + "/  "},
		},
		Log: testutil.NopLogger(),
	}

	messageID, err := app.whatsAppClient().SendTextMessage(
		context.Background(),
		&whatsapp.Account{
			PhoneID:     "987654321",
			APIVersion:  "v99.0",
			AccessToken: "synthetic-handler-token",
		},
		whatsapp.Recipient{Phone: "15550000001"},
		"handler routing check",
	)
	require.NoError(t, err)
	assert.Equal(t, "wamid.handler-local", messageID)
	assert.Equal(t, "/v99.0/987654321/messages", <-paths)
}

func TestWhatsAppClientPrefersInjectedSharedClient(t *testing.T) {
	t.Parallel()

	injected := whatsapp.NewWithBaseURL(testutil.NopLogger(), "http://127.0.0.1:1")
	app := &App{
		Config: &config.Config{
			WhatsApp: config.WhatsAppConfig{BaseURL: "http://127.0.0.1:2"},
		},
		Log:      testutil.NopLogger(),
		WhatsApp: injected,
	}

	assert.Same(t, injected, app.whatsAppClient())
}
