package channel

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testAdapter struct {
	channel      models.Channel
	provider     string
	capabilities Capabilities
}

func (a *testAdapter) Channel() models.Channel {
	return a.channel
}

func (a *testAdapter) Provider() string {
	return a.provider
}

func (a *testAdapter) Capabilities(_ *models.ChannelAccount) Capabilities {
	return a.capabilities
}

func (a *testAdapter) RouteHint(_ http.Header, _ []byte) (WebhookRouteHint, error) {
	return WebhookRouteHint{}, nil
}

func (a *testAdapter) VerifyWebhook(_ *models.ChannelAccount, _ http.Header, _ []byte) error {
	return nil
}

func (a *testAdapter) NormalizeWebhook(_ context.Context, _ *models.ChannelAccount, _ []byte) ([]InboundEvent, error) {
	return nil, nil
}

func (a *testAdapter) Send(_ context.Context, _ *models.ChannelAccount, _ OutboundMessage) (SendResult, error) {
	return SendResult{}, nil
}

func (a *testAdapter) MarkRead(_ context.Context, _ *models.ChannelAccount, _ ConversationRef, _ []string) error {
	return nil
}

func (a *testAdapter) FetchMedia(_ context.Context, _ *models.ChannelAccount, _ MediaRef) (FetchedMedia, error) {
	return FetchedMedia{}, nil
}

func (a *testAdapter) ValidateAccount(_ context.Context, _ *models.ChannelAccount) (AccountValidationResult, error) {
	return AccountValidationResult{}, nil
}

func (a *testAdapter) Subscribe(_ context.Context, _ *models.ChannelAccount) error {
	return nil
}

func (a *testAdapter) RefreshCredentials(_ context.Context, _ *models.ChannelAccount) (CredentialRefreshResult, error) {
	return CredentialRefreshResult{}, nil
}

func TestRegistryRegisterGetAndList(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	meta := &testAdapter{channel: models.ChannelWhatsApp, provider: "meta"}
	smtp := &testAdapter{channel: models.ChannelEmail, provider: "smtp"}
	graph := &testAdapter{channel: models.ChannelMessenger, provider: "graph"}

	require.NoError(t, registry.Register(meta))
	require.NoError(t, registry.Register(graph))
	require.NoError(t, registry.Register(smtp))

	got, ok := registry.Get(models.Channel(" WHATSAPP "), " META ")
	require.True(t, ok)
	assert.Same(t, meta, got)

	_, ok = registry.Get(models.ChannelInstagram, "graph")
	assert.False(t, ok)

	list := registry.List()
	require.Len(t, list, 3)
	assert.Same(t, smtp, list[0], "email sorts before messenger and whatsapp")
	assert.Same(t, graph, list[1])
	assert.Same(t, meta, list[2])
}

func TestRegistryRejectsDuplicateAndInvalidAdapters(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	require.NoError(t, registry.Register(&testAdapter{
		channel:  models.ChannelWhatsApp,
		provider: "Meta",
	}))

	err := registry.Register(&testAdapter{
		channel:  models.Channel(" WHATSAPP "),
		provider: " meta ",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAdapterAlreadyRegistered)

	assert.ErrorIs(t, registry.Register(nil), ErrNilAdapter)

	var typedNil *testAdapter
	assert.ErrorIs(t, registry.Register(typedNil), ErrNilAdapter)

	assert.ErrorIs(t, registry.Register(&testAdapter{
		channel:  "",
		provider: "provider",
	}), ErrInvalidAdapterIdentity)
	assert.ErrorIs(t, registry.Register(&testAdapter{
		channel:  models.ChannelEmail,
		provider: " ",
	}), ErrInvalidAdapterIdentity)
}

func TestRegistryIsSafeForConcurrentRegistrationAndLookup(t *testing.T) {
	t.Parallel()

	const adapterCount = 64
	registry := NewRegistry()

	var wg sync.WaitGroup
	errs := make(chan error, adapterCount)
	for i := 0; i < adapterCount; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			provider := string(rune('a'+i%26)) + string(rune('a'+i/26))
			adapter := &testAdapter{
				channel:  models.ChannelWebChat,
				provider: provider,
			}
			if err := registry.Register(adapter); err != nil {
				errs <- err
				return
			}
			if _, ok := registry.Get(models.ChannelWebChat, provider); !ok {
				errs <- errors.New("registered adapter was not found")
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	assert.Len(t, registry.List(), adapterCount)
}

func TestCapabilitiesSupportsAndRegistryResolution(t *testing.T) {
	t.Parallel()

	capabilities := Capabilities{
		Text:                true,
		Media:               true,
		MultipleAttachments: true,
		Replies:             true,
		Reactions:           true,
		ReadReceipts:        true,
		Buttons:             true,
		Templates:           true,
		BusinessInitiation:  true,
		ServiceWindow:       true,
		Typing:              true,
		SubjectAndCC:        true,
		MaxAttachments:      10,
		MaxMediaBytes:       25 << 20,
		SupportedMediaTypes: []string{"image/jpeg", "application/pdf"},
	}

	known := []Capability{
		CapabilityText,
		CapabilityMedia,
		CapabilityMultipleAttachments,
		CapabilityReplies,
		CapabilityReactions,
		CapabilityReadReceipts,
		CapabilityButtons,
		CapabilityTemplates,
		CapabilityBusinessInitiation,
		CapabilityServiceWindow,
		CapabilityTyping,
		CapabilitySubjectAndCC,
	}
	for _, capability := range known {
		assert.True(t, capabilities.Supports(capability), capability)
	}
	assert.False(t, capabilities.Supports(Capability("unknown")))

	registry := NewRegistry()
	require.NoError(t, registry.Register(&testAdapter{
		channel:      models.ChannelEmail,
		provider:     "graph",
		capabilities: capabilities,
	}))

	account := &models.ChannelAccount{
		Channel:  models.ChannelEmail,
		Provider: "graph",
	}
	got, ok := registry.Capabilities(models.ChannelEmail, "GRAPH", account)
	require.True(t, ok)
	assert.Equal(t, capabilities, got)

	_, ok = registry.Capabilities(models.ChannelSMS, "missing", account)
	assert.False(t, ok)
}

func TestNilRegistryReadOperations(t *testing.T) {
	t.Parallel()

	var registry *Registry
	_, ok := registry.Get(models.ChannelWhatsApp, "meta")
	assert.False(t, ok)
	assert.Nil(t, registry.List())
	_, ok = registry.Capabilities(models.ChannelWhatsApp, "meta", nil)
	assert.False(t, ok)
}
