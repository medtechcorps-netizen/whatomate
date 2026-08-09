package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelAdapterLocalhostRelayIsDevelopmentOnly(t *testing.T) {
	var requestCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestCount.Add(1)
		assert.Equal(t, http.MethodHead, request.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	organizationID := uuid.New()
	account := &models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organizationID,
		Channel:           models.ChannelWebChat,
		Provider:          channelapi.RelayProvider,
		ExternalAccountID: "webchat-local-relay",
		Status:            models.ChannelAccountStatusPending,
		Config: models.JSONB{
			"relay_url":           server.URL,
			"allow_localhost_dev": true,
		},
		Credentials: []models.ChannelCredential{{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   organizationID,
			ChannelAccountID: uuid.New(),
			Kind:             models.ChannelCredentialKindWebhook,
			Version:          1,
			CredentialBlob:   models.JSONB{"inbound_secret": "local-test-secret"},
			Status:           models.ChannelCredentialStatusActive,
		}},
	}
	account.Credentials[0].ChannelAccountID = account.ID

	for _, testCase := range []struct {
		name        string
		environment string
		wantValid   bool
	}{
		{name: "development", environment: "development", wantValid: true},
		{name: "trimmed mixed-case development", environment: " Development ", wantValid: true},
		{name: "staging", environment: "staging", wantValid: false},
		{name: "production", environment: "production", wantValid: false},
		{name: "test", environment: "test", wantValid: false},
		{name: "near match", environment: "development-local", wantValid: false},
		{name: "unset", environment: "", wantValid: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			requestsBefore := requestCount.Load()
			app := &App{
				Config: &configpkg.Config{App: configpkg.AppConfig{
					Environment:   testCase.environment,
					EncryptionKey: "development-relay-test-encryption-key",
				}},
				HTTPClient: server.Client(),
			}
			adapter, err := app.channelAdapter(account)
			require.NoError(t, err)

			result, validationErr := adapter.ValidateAccount(context.Background(), account)
			if testCase.wantValid {
				require.NoError(t, validationErr)
				assert.True(t, result.Valid)
				assert.Equal(t, requestsBefore+1, requestCount.Load())
				return
			}
			require.Error(t, validationErr)
			assert.False(t, result.Valid)
			assert.ErrorIs(t, validationErr, channelapi.ErrRelayURLInvalid)
			assert.ErrorContains(t, validationErr, "localhost requires explicit development approval")
			assert.Equal(t, requestsBefore, requestCount.Load())
		})
	}
}
