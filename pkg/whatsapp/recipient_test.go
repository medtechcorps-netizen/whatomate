package whatsapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsPlaceholderAddress(t *testing.T) {
	t.Parallel()

	placeholders := []string{
		"bsuid:0123456789abcdef0123456789abcdef01234567",
		"user:0123456789abcdef0123456789abcdef01234567",
		"event:0123456789abcdef0123456789abcdef01234567",
		"id:0123456789abcdef",
		"  BSUID:0123  ",
	}
	for _, phone := range placeholders {
		assert.True(t, whatsapp.IsPlaceholderAddress(phone), phone)
	}

	addresses := []string{
		"",
		"15550001111",
		"+15550001111",
		// Group JIDs are valid non-digit addresses and must keep working.
		"120363000000000000@g.us",
		"user-without-colon",
		"x-bsuid:not-a-prefix",
	}
	for _, phone := range addresses {
		assert.False(t, whatsapp.IsPlaceholderAddress(phone), phone)
	}
}

func TestRecipientSetOnPayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		recipient     whatsapp.Recipient
		wantTo        any
		wantRecipient any
	}{
		{
			name:      "phone only",
			recipient: whatsapp.Recipient{Phone: "15550001111"},
			wantTo:    "15550001111",
		},
		{
			name:          "phone and BSUID",
			recipient:     whatsapp.Recipient{Phone: "15550001111", BSUID: "US.1000000000000000001"},
			wantTo:        "15550001111",
			wantRecipient: "US.1000000000000000001",
		},
		{
			name:          "empty phone uses BSUID only",
			recipient:     whatsapp.Recipient{BSUID: "US.1000000000000000001"},
			wantRecipient: "US.1000000000000000001",
		},
		{
			name: "bsuid placeholder is never sent as to",
			recipient: whatsapp.Recipient{
				Phone: "bsuid:0123456789abcdef0123456789abcdef01234567",
				BSUID: "US.1000000000000000001",
			},
			wantRecipient: "US.1000000000000000001",
		},
		{
			name:          "user placeholder is never sent as to",
			recipient:     whatsapp.Recipient{Phone: "user:0123", BSUID: "US.1000000000000000002"},
			wantRecipient: "US.1000000000000000002",
		},
		{
			name:          "event placeholder is never sent as to",
			recipient:     whatsapp.Recipient{Phone: "event:0123", BSUID: "US.1000000000000000003"},
			wantRecipient: "US.1000000000000000003",
		},
		{
			name:          "id placeholder is never sent as to",
			recipient:     whatsapp.Recipient{Phone: "id:0123", BSUID: "US.1000000000000000004"},
			wantRecipient: "US.1000000000000000004",
		},
		{
			name:      "placeholder without BSUID sets neither field",
			recipient: whatsapp.Recipient{Phone: "bsuid:0123"},
		},
		{
			name:      "group JID is kept",
			recipient: whatsapp.Recipient{Phone: "120363000000000000@g.us"},
			wantTo:    "120363000000000000@g.us",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			payload := map[string]any{}
			tt.recipient.SetOnPayload(payload)

			to, hasTo := payload["to"]
			if tt.wantTo == nil {
				assert.False(t, hasTo, "unexpected to=%v", to)
			} else {
				assert.Equal(t, tt.wantTo, to)
			}
			recipient, hasRecipient := payload["recipient"]
			if tt.wantRecipient == nil {
				assert.False(t, hasRecipient, "unexpected recipient=%v", recipient)
			} else {
				assert.Equal(t, tt.wantRecipient, recipient)
			}
		})
	}
}

// The outbound request for a contact stored with a BSUID placeholder carries
// only the BSUID, never the placeholder as "to".
func TestSendTextMessageToPlaceholderContactOmitsTo(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": "wamid.placeholder-test"}},
		})
	}))
	defer server.Close()

	client := whatsapp.NewWithBaseURL(testutil.NopLogger(), server.URL)
	account := &whatsapp.Account{PhoneID: "123456789", BusinessID: "987654321", APIVersion: "v21.0", AccessToken: "test-token"}
	_, err := client.SendTextMessage(
		context.Background(),
		account,
		whatsapp.Recipient{
			Phone: "bsuid:0123456789abcdef0123456789abcdef01234567",
			BSUID: "US.1000000000000000001",
		},
		"hello",
	)
	require.NoError(t, err)
	require.NotNil(t, captured)
	_, hasTo := captured["to"]
	assert.False(t, hasTo)
	assert.Equal(t, "US.1000000000000000001", captured["recipient"])
}
