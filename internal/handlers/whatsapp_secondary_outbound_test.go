package handlers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
)

type secondaryOutboundCountingTransport struct {
	calls atomic.Int32
}

func (t *secondaryOutboundCountingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"messages":[{"id":"wamid.unexpected"}]}`)),
	}, nil
}

func TestSecondaryOutboundGuardsRejectDisconnectedAccountWithoutGraph(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	account := testutil.CreateTestWhatsAppAccount(t, db, organization.ID)
	activeSnapshot := *account
	require.NoError(t, db.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", account.ID, organization.ID).
		Update("status", "disconnected").Error)

	transport := &secondaryOutboundCountingTransport{}
	httpClient := &http.Client{Transport: transport}
	log := testutil.NopLogger()
	waClient := whatsapp.NewWithBaseURL(log, "https://graph.example.test")
	waClient.HTTPClient = httpClient
	app := &App{
		Config: &config.Config{
			WhatsApp: config.WhatsAppConfig{BaseURL: "https://graph.example.test"},
		},
		DB:         db,
		Log:        log,
		WhatsApp:   waClient,
		HTTPClient: httpClient,
	}

	t.Run("reaction", func(t *testing.T) {
		err := app.sendWhatsAppReactionGuarded(
			&activeSnapshot,
			&models.Contact{PhoneNumber: "+15550001111"},
			&models.Message{WhatsAppMessageID: "wamid.source"},
			"U0001F44D",
		)
		require.ErrorIs(t, err, whatsappaccount.ErrOutboundInactive)
		require.Zero(t, transport.calls.Load())
	})

	t.Run("read receipts", func(t *testing.T) {
		err := app.sendWhatsAppReadReceipts(context.Background(), &activeSnapshot, []models.Message{
			{WhatsAppMessageID: "wamid.incoming"},
		})
		require.ErrorIs(t, err, whatsappaccount.ErrOutboundInactive)
		require.Zero(t, transport.calls.Load())
	})

	t.Run("call permission message", func(t *testing.T) {
		_, err := app.sendCallPermissionRequestGuarded(
			context.Background(),
			&activeSnapshot,
			whatsapp.Recipient{Phone: "+15550001111"},
		)
		require.ErrorIs(t, err, whatsappaccount.ErrOutboundInactive)
		require.Zero(t, transport.calls.Load())
	})
}
