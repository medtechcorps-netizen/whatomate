package handlers_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type legacyReplyFixture struct {
	app            *handlers.App
	mock           *mockWhatsAppServer
	organization   models.Organization
	user           models.User
	selected       *models.WhatsAppAccount
	defaultAccount *models.WhatsAppAccount
	contact        *models.Contact
	conversation   models.InboxConversation
	shadow         models.ChannelAccount
}

func TestSendLegacyWhatsAppConversationReplyUsesExactBoundAccount(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	request := legacyReplyRequest(t, fixture, "Reply from Omnichannel")
	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	assert.Equal(t, 1, fixture.mock.sentMessageCount())

	var response struct {
		Data struct {
			ID                  uuid.UUID            `json:"id"`
			Content             map[string]string    `json:"content"`
			Status              models.MessageStatus `json:"status"`
			WAMID               string               `json:"wamid"`
			WhatsAppAccount     string               `json:"whatsapp_account"`
			InboxConversationID *uuid.UUID           `json:"inbox_conversation_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	assert.Equal(t, fixture.selected.Name, response.Data.WhatsAppAccount)
	assert.Equal(t, "Reply from Omnichannel", response.Data.Content["body"])
	assert.Equal(t, models.MessageStatusSent, response.Data.Status)
	assert.NotEmpty(t, response.Data.WAMID)
	require.NotNil(t, response.Data.InboxConversationID)
	assert.Equal(t, fixture.conversation.ID, *response.Data.InboxConversationID)
	paths, auth := fixture.mock.messageRequestSnapshot()
	require.Equal(t, []string{"/v18.0/" + fixture.selected.PhoneID + "/messages"}, paths)
	require.Equal(t, []string{"Bearer " + fixture.selected.AccessToken}, auth)
	assert.NotContains(t, paths, "/v18.0/"+fixture.defaultAccount.PhoneID+"/messages")

	var persisted models.Message
	require.NoError(t, fixture.app.DB.Where("id = ? AND organization_id = ?", response.Data.ID, fixture.organization.ID).First(&persisted).Error)
	assert.Equal(t, fixture.selected.Name, persisted.WhatsAppAccount)
	require.NotNil(t, persisted.InboxConversationID)
	assert.Equal(t, fixture.conversation.ID, *persisted.InboxConversationID)
	assert.Equal(t, models.MessageStatusSent, persisted.Status)
}

func TestSendLegacyWhatsAppConversationReplyBroadcastsOnlyFinalEnvelope(t *testing.T) {
	tests := []struct {
		name         string
		providerFail bool
		wantStatus   models.MessageStatus
	}{
		{name: "sent", wantStatus: models.MessageStatusSent},
		{name: "failed", providerFail: true, wantStatus: models.MessageStatusFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLegacyReplyFixture(t)
			fixture.mock.returnError = test.providerFail
			fixture.mock.errorMessage = "provider rejected reply"
			hub := appwebsocket.NewHub(testutil.NopLogger())
			go hub.Run()
			fixture.app.WSHub = hub
			client := appwebsocket.NewClient(hub, nil, uuid.New(), fixture.organization.ID)
			hub.Register(client)
			testutil.AssertEventually(t, func() bool { return hub.GetClientCount() == 1 }, 2*time.Second, "websocket client registered")

			request := legacyReplyRequest(t, fixture, "Final status envelope")
			require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
			assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))

			var httpResponse struct {
				Data struct {
					Status models.MessageStatus `json:"status"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &httpResponse))
			assert.Equal(t, test.wantStatus, httpResponse.Data.Status)

			statusEnvelope := receiveLegacyReplyWSEnvelope(t, client.SendChan())
			newMessageEnvelope := receiveLegacyReplyWSEnvelope(t, client.SendChan())
			assert.Equal(t, appwebsocket.TypeStatusUpdate, statusEnvelope.Type)
			assert.Equal(t, string(test.wantStatus), statusEnvelope.Payload["status"])
			assert.Equal(t, appwebsocket.TypeNewMessage, newMessageEnvelope.Type)
			assert.Equal(t, string(test.wantStatus), newMessageEnvelope.Payload["status"])
			assert.NotEqual(t, string(models.MessageStatusPending), newMessageEnvelope.Payload["status"])
		})
	}
}

type legacyReplyWSEnvelope struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

func receiveLegacyReplyWSEnvelope(t *testing.T, messages <-chan []byte) legacyReplyWSEnvelope {
	t.Helper()
	select {
	case data := <-messages:
		var envelope legacyReplyWSEnvelope
		require.NoError(t, json.Unmarshal(data, &envelope))
		return envelope
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for legacy reply WebSocket envelope")
		return legacyReplyWSEnvelope{}
	}
}

func TestSendLegacyWhatsAppConversationReplyIsReplaySafe(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	key := "reply-" + uuid.NewString()
	first := legacyReplyRequestWithKey(t, fixture, "One logical reply", key)
	second := legacyReplyRequestWithKey(t, fixture, "One logical reply", key)
	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(first))
	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(second))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(second))
	assert.Equal(t, legacyReplyResponseID(t, first), legacyReplyResponseID(t, second))
	assert.Equal(t, 1, fixture.mock.sentMessageCount())
	var count int64
	require.NoError(t, fixture.app.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND inbox_conversation_id = ? AND metadata ->> 'idempotency_key' = ?",
		fixture.organization.ID, fixture.conversation.ID, key,
	).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestSendLegacyWhatsAppConversationReplyConcurrentClaimSendsOnce(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	key := "reply-" + uuid.NewString()
	requests := []*fastglue.Request{
		legacyReplyRequestWithKey(t, fixture, "Concurrent payload", key),
		legacyReplyRequestWithKey(t, fixture, "Concurrent payload", key),
	}
	requestErrors := make([]error, len(requests))
	var group sync.WaitGroup
	for index := range requests {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			requestErrors[index] = fixture.app.SendLegacyWhatsAppConversationReply(requests[index])
		}(index)
	}
	group.Wait()
	for index, request := range requests {
		require.NoError(t, requestErrors[index])
		assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	}
	assert.Equal(t, legacyReplyResponseID(t, requests[0]), legacyReplyResponseID(t, requests[1]))
	assert.Equal(t, 1, fixture.mock.sentMessageCount())
}

func TestSendLegacyWhatsAppConversationReplyRejectsIdempotencyCollision(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	key := "reply-" + uuid.NewString()
	first := legacyReplyRequestWithKey(t, fixture, "Original payload", key)
	second := legacyReplyRequestWithKey(t, fixture, "Different payload", key)
	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(first))
	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(second))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))
	assert.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(second))
	assert.Equal(t, 1, fixture.mock.sentMessageCount())
}

func TestSendLegacyWhatsAppConversationReplyReplaysNonSentClaimWithoutProvider(t *testing.T) {
	tests := []struct {
		name      string
		status    models.MessageStatus
		errorText string
	}{
		{name: "pending provider state unknown", status: models.MessageStatusPending},
		{name: "failed terminal result", status: models.MessageStatusFailed, errorText: "terminal failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLegacyReplyFixture(t)
			hub := appwebsocket.NewHub(testutil.NopLogger())
			go hub.Run()
			fixture.app.WSHub = hub
			client := appwebsocket.NewClient(hub, nil, uuid.New(), fixture.organization.ID)
			hub.Register(client)
			testutil.AssertEventually(t, func() bool { return hub.GetClientCount() == 1 }, 2*time.Second, "websocket client registered")
			key := "reply-" + uuid.NewString()
			body := "Do not resend this operation"
			message := models.Message{
				BaseModel:           models.BaseModel{ID: uuid.New()},
				OrganizationID:      fixture.organization.ID,
				WhatsAppAccount:     fixture.selected.Name,
				ContactID:           fixture.contact.ID,
				InboxConversationID: &fixture.conversation.ID,
				Direction:           models.DirectionOutgoing,
				MessageType:         models.MessageTypeText,
				Content:             body,
				Status:              test.status,
				ErrorMessage:        test.errorText,
				Metadata: models.JSONB{
					"idempotency_key":            key,
					"payload_digest":             legacyReplyTestDigest(fixture, body),
					"legacy_whatsapp_account_id": fixture.selected.ID.String(),
					"inbox_conversation_id":      fixture.conversation.ID.String(),
					"send_surface":               "omnichannel_legacy_whatsapp",
				},
			}
			require.NoError(t, fixture.app.DB.Create(&message).Error)

			request := legacyReplyRequestWithKey(t, fixture, body, key)
			require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
			assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
			assert.Zero(t, fixture.mock.sentMessageCount(), "replay must never call Meta")
			var response struct {
				Data struct {
					ID     uuid.UUID            `json:"id"`
					Status models.MessageStatus `json:"status"`
					Error  string               `json:"error_message"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
			assert.Equal(t, message.ID, response.Data.ID)
			assert.Equal(t, test.status, response.Data.Status)
			assert.Equal(t, test.errorText, response.Data.Error)
			replayEnvelope := receiveLegacyReplyWSEnvelope(t, client.SendChan())
			assert.Equal(t, appwebsocket.TypeNewMessage, replayEnvelope.Type)
			assert.Equal(t, string(test.status), replayEnvelope.Payload["status"])
			assert.NotEqual(t, "", replayEnvelope.Payload["id"])
		})
	}
}

func TestSendLegacyWhatsAppConversationReplyDatabaseFailureCallsNoProvider(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	callbackName := "test:reject_strict_legacy_reply_" + uuid.NewString()
	require.NoError(t, fixture.app.DB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "messages" {
			_ = tx.AddError(errors.New("forced pending-message persistence failure"))
		}
	}))
	t.Cleanup(func() { _ = fixture.app.DB.Callback().Create().Remove(callbackName) })
	request := legacyReplyRequest(t, fixture, "Must remain local")
	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
	assert.Equal(t, fasthttp.StatusBadGateway, testutil.GetResponseStatusCode(request))
	assert.Zero(t, fixture.mock.sentMessageCount())
}

func TestSendLegacyWhatsAppConversationReplyRejectsInactiveDurableEntitlement(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *legacyReplyFixture)
	}{
		{"missing", func(t *testing.T, fixture *legacyReplyFixture) {
			require.NoError(t, fixture.app.DB.Unscoped().Where("organization_id = ?", fixture.organization.ID).Delete(&models.Subscription{}).Error)
		}},
		{"expired", func(t *testing.T, fixture *legacyReplyFixture) {
			expired := time.Now().UTC().Add(-time.Hour)
			require.NoError(t, fixture.app.DB.Model(&models.Subscription{}).Where("organization_id = ?", fixture.organization.ID).Updates(map[string]any{
				"current_period_end": expired, "grace_until": nil, "trial_ends_at": nil,
			}).Error)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLegacyReplyFixture(t)
			test.mutate(t, fixture)
			request := legacyReplyRequest(t, fixture, "Must not reach Meta")
			require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
			assert.Contains(t, []int{fasthttp.StatusPaymentRequired, fasthttp.StatusForbidden}, testutil.GetResponseStatusCode(request))
			assert.Zero(t, fixture.mock.sentMessageCount())
		})
	}
}

func TestSendLegacyWhatsAppConversationReplyRejectsUnsafeProjection(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *legacyReplyFixture)
	}{
		{"wrong reply route", func(t *testing.T, fixture *legacyReplyFixture) {
			value := cloneReplyJSON(fixture.shadow.Config)
			value["reply_route"] = "unsafe"
			updateLegacyReplyShadow(t, fixture, "config", value)
		}},
		{"missing text capability", removeLegacyReplyCapability("text")},
		{"missing replies capability", removeLegacyReplyCapability("replies")},
		{"missing service window capability", removeLegacyReplyCapability("service_window")},
		{"inconsistent immutable binding", func(t *testing.T, fixture *legacyReplyFixture) {
			value := cloneReplyJSON(fixture.shadow.Metadata)
			value["legacy_account_id"] = uuid.NewString()
			updateLegacyReplyShadow(t, fixture, "metadata", value)
		}},
		{"closed service window", func(t *testing.T, fixture *legacyReplyFixture) {
			closedAt := time.Now().UTC().Add(-time.Minute)
			require.NoError(t, fixture.app.DB.Model(&models.InboxConversation{}).Where(
				"id = ? AND organization_id = ?", fixture.conversation.ID, fixture.organization.ID,
			).Update("service_window_ends_at", closedAt).Error)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLegacyReplyFixture(t)
			test.mutate(t, fixture)
			request := legacyReplyRequest(t, fixture, "Must not reach Meta")
			require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
			assert.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(request))
			assert.Zero(t, fixture.mock.sentMessageCount())
		})
	}
}

func TestSendLegacyWhatsAppConversationReplyRequiresIdempotencyKey(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	request := legacyReplyRequestWithKey(t, fixture, "No key", "")
	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(request))
	assert.Zero(t, fixture.mock.sentMessageCount())
}

func TestSendLegacyWhatsAppConversationReplyRejectsUnsafeJSONShape(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "unknown root field",
			body: `{"idempotency_key":"key","type":"text","content":{"body":"hello"},"account_id":"attacker-selected"}`,
		},
		{
			name: "unknown nested field",
			body: `{"idempotency_key":"key","type":"text","content":{"body":"hello","media_id":"attacker-selected"}}`,
		},
		{
			name: "case variant root field",
			body: `{"idempotency_key":"key","Type":"text","content":{"body":"hello"}}`,
		},
		{
			name: "case variant nested field",
			body: `{"idempotency_key":"key","type":"text","content":{"Body":"hello"}}`,
		},
		{
			name: "duplicate idempotency key",
			body: `{"idempotency_key":"first","idempotency_key":"second","type":"text","content":{"body":"hello"}}`,
		},
		{
			name: "duplicate nested body",
			body: `{"idempotency_key":"key","type":"text","content":{"body":"first","body":"second"}}`,
		},
		{
			name: "trailing JSON value",
			body: `{"idempotency_key":"key","type":"text","content":{"body":"hello"}} {}`,
		},
		{
			name: "non object root",
			body: `[]`,
		},
		{
			name: "content array",
			body: `{"idempotency_key":"key","type":"text","content":[]}`,
		},
		{
			name: "nested body object",
			body: `{"idempotency_key":"key","type":"text","content":{"body":{}}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLegacyReplyFixture(t)
			request := legacyReplyRequest(t, fixture, "placeholder")
			request.RequestCtx.Request.SetBodyString(test.body)

			require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
			assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(request))
			assert.Zero(t, fixture.mock.sentMessageCount())
		})
	}
}

func TestSendLegacyWhatsAppConversationReplyRejectsInvalidUTF8(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	request := legacyReplyRequest(t, fixture, "placeholder")
	body := append(
		[]byte(`{"idempotency_key":"key","type":"text","content":{"body":"hello`),
		0xff,
	)
	body = append(body, []byte(`"}}`)...)
	request.RequestCtx.Request.SetBodyRaw(body)

	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(request))
	assert.Zero(t, fixture.mock.sentMessageCount())
}

func TestSendLegacyWhatsAppConversationReplyEnforcesRequestBodyBoundary(t *testing.T) {
	const maxRequestBytes = 64 << 10
	validJSON := `{"idempotency_key":"body-boundary","type":"text","content":{"body":"hello"}}`

	fixture := newLegacyReplyFixture(t)
	boundaryRequest := legacyReplyRequest(t, fixture, "placeholder")
	boundaryRequest.RequestCtx.Request.SetBodyString(
		validJSON + strings.Repeat(" ", maxRequestBytes-len(validJSON)),
	)
	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(boundaryRequest))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(boundaryRequest))
	assert.Equal(t, 1, fixture.mock.sentMessageCount())

	oversizedRequest := legacyReplyRequest(t, fixture, "placeholder")
	oversizedRequest.RequestCtx.Request.SetBodyString(
		validJSON + strings.Repeat(" ", maxRequestBytes-len(validJSON)+1),
	)
	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(oversizedRequest))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(oversizedRequest))
	assert.Equal(t, 1, fixture.mock.sentMessageCount())
}

func TestSendLegacyWhatsAppConversationReplyRejectsProviderOversizedText(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	request := legacyReplyRequest(
		t,
		fixture,
		strings.Repeat("界", 4097),
	)

	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(request))
	assert.Zero(t, fixture.mock.sentMessageCount())
}

func TestSendLegacyWhatsAppConversationReplyAcceptsProviderTextBoundary(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	request := legacyReplyRequest(
		t,
		fixture,
		strings.Repeat("界", 4096),
	)

	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	assert.Equal(t, 1, fixture.mock.sentMessageCount())
}

func TestSendLegacyWhatsAppConversationReplyRejectsNULText(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	request := legacyReplyRequest(t, fixture, "hello\x00world")

	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(request))
	assert.Zero(t, fixture.mock.sentMessageCount())
}

func TestSendLegacyWhatsAppConversationReplyRolloutGateFailsClosed(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	fixture.app.Config.LegacyWhatsAppReply.Enabled = false
	request := legacyReplyRequest(t, fixture, "Disabled rollout")
	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(request))
	assert.Zero(t, fixture.mock.sentMessageCount())

	fixture.app.Config.LegacyWhatsAppReply.Enabled = true
	fixture.app.Config.LegacyWhatsAppReply.AllowedOrganizationIDs = ""
	request = legacyReplyRequest(t, fixture, "Empty allowlist")
	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(request))
	assert.Zero(t, fixture.mock.sentMessageCount())

	fixture.app.Config.LegacyWhatsAppReply.Enabled = true
	fixture.app.Config.LegacyWhatsAppReply.AllowedOrganizationIDs = uuid.NewString()
	request = legacyReplyRequest(t, fixture, "Wrong tenant")
	require.NoError(t, fixture.app.SendLegacyWhatsAppConversationReply(request))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(request))
	assert.Zero(t, fixture.mock.sentMessageCount())
}

func newLegacyReplyFixture(t *testing.T) *legacyReplyFixture {
	t.Helper()
	mockServer := newMockWhatsAppServer()
	t.Cleanup(mockServer.close)
	app := newMsgTestApp(t, mockServer)
	organization := testutil.CreateTestOrganization(t, app.DB)
	app.Config.LegacyWhatsAppReply.Enabled = true
	app.Config.LegacyWhatsAppReply.AllowedOrganizationIDs = organization.ID.String()
	adminRole := testutil.CreateAdminRole(t, app.DB, organization.ID)
	user := testutil.CreateTestUser(t, app.DB, organization.ID, testutil.WithRoleID(&adminRole.ID))
	enableLegacyReplyOmnichannel(t, app.DB, organization.ID, user.ID)
	selected := createTestAccount(t, app, organization.ID)
	contactDefault := createTestAccount(t, app, organization.ID)
	contactDefault.AccessToken = "default-token-" + uuid.NewString()
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("id = ? AND organization_id = ?", contactDefault.ID, organization.ID).
		Update("access_token", contactDefault.AccessToken).Error)
	contact := testutil.CreateTestContactWith(t, app.DB, organization.ID, testutil.WithContactAccount(contactDefault.Name))
	inbound := models.Message{
		OrganizationID: organization.ID, ContactID: contact.ID, WhatsAppAccount: selected.Name,
		Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
		Content: "Customer opened the service window", Status: models.MessageStatusReceived,
		WhatsAppMessageID: "wamid.inbound." + uuid.NewString(), Metadata: models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&inbound).Error)
	bridge, err := channelapi.MirrorLegacyWhatsAppMessage(app.DB, channelapi.LegacyMetaAccountRef{
		ID: selected.ID, OrganizationID: organization.ID, Name: selected.Name, Status: selected.Status,
	}, inbound.ID)
	require.NoError(t, err)
	var conversation models.InboxConversation
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", bridge.ConversationID, organization.ID).First(&conversation).Error)
	var shadow models.ChannelAccount
	require.NoError(t, app.DB.Where("id = ? AND organization_id = ?", bridge.ChannelAccountID, organization.ID).First(&shadow).Error)
	return &legacyReplyFixture{
		app: app, mock: mockServer, organization: *organization, user: *user,
		selected: selected, defaultAccount: contactDefault, contact: contact,
		conversation: conversation, shadow: shadow,
	}
}

func legacyReplyRequest(t *testing.T, fixture *legacyReplyFixture, body string) *fastglue.Request {
	return legacyReplyRequestWithKey(t, fixture, body, "reply-"+uuid.NewString())
}

func legacyReplyRequestWithKey(t *testing.T, fixture *legacyReplyFixture, body, key string) *fastglue.Request {
	t.Helper()
	request := testutil.NewJSONRequest(t, map[string]any{
		"idempotency_key": key, "type": "text", "content": map[string]string{"body": body},
	})
	testutil.SetAuthContext(request, fixture.organization.ID, fixture.user.ID)
	testutil.SetHeader(request, "X-Organization-ID", fixture.organization.ID.String())
	testutil.SetPathParam(request, "id", fixture.conversation.ID.String())
	return request
}

func legacyReplyResponseID(t *testing.T, request *fastglue.Request) uuid.UUID {
	t.Helper()
	var response struct {
		Data struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &response))
	require.NotEqual(t, uuid.Nil, response.Data.ID)
	return response.Data.ID
}

func legacyReplyTestDigest(fixture *legacyReplyFixture, body string) string {
	payload := fixture.organization.ID.String() + "\x00" +
		fixture.selected.ID.String() + "\x00" +
		fixture.conversation.ID.String() + "\x00text\x00" +
		body + "\x00"
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

func removeLegacyReplyCapability(key string) func(*testing.T, *legacyReplyFixture) {
	return func(t *testing.T, fixture *legacyReplyFixture) {
		value := cloneReplyJSON(fixture.shadow.Capabilities)
		delete(value, key)
		updateLegacyReplyShadow(t, fixture, "capabilities", value)
	}
}

func updateLegacyReplyShadow(t *testing.T, fixture *legacyReplyFixture, column string, value models.JSONB) {
	t.Helper()
	require.NoError(t, fixture.app.DB.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?", fixture.shadow.ID, fixture.organization.ID,
	).Update(column, value).Error)
}

func enableLegacyReplyOmnichannel(t *testing.T, db *gorm.DB, orgID, userID uuid.UUID) {
	t.Helper()
	plan := models.Plan{
		BaseModel: models.BaseModel{ID: uuid.New()}, ScopeKey: "legacy-reply-" + uuid.NewString(),
		Code: "legacy-reply-" + uuid.NewString(), Name: "Legacy reply test",
		Status: models.CommercialPlanStatusActive, Vertical: "general", Metadata: models.JSONB{}, CreatedByID: &userID,
	}
	require.NoError(t, db.Create(&plan).Error)
	billing := models.BillingAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		Provider: models.BillingProviderManual, Status: models.BillingAccountStatusActive,
		DefaultCurrency: "MYR", BillingProfile: models.JSONB{}, ProviderData: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&billing).Error)
	periodStart := time.Now().UTC().Add(-time.Hour)
	periodEnd := periodStart.AddDate(0, 1, 0)
	subscription := models.Subscription{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		BillingAccountID: billing.ID, PlanID: plan.ID, Provider: models.BillingProviderManual,
		Status: models.SubscriptionStatusActive, Quantity: 1, CollectionMethod: "send_invoice",
		EntitlementsSnapshot: models.JSONB{channelapi.OmnichannelEntitlementKey: true}, ProviderData: models.JSONB{},
		CurrentPeriodStart: &periodStart, CurrentPeriodEnd: &periodEnd, CreatedByID: &userID,
	}
	require.NoError(t, db.Create(&subscription).Error)
}

func cloneReplyJSON(source models.JSONB) models.JSONB {
	cloned := make(models.JSONB, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// legacyReplyAdmissionFence holds the lock prefix of a Coexistence admission
// on the fixture's tenant (WAMID fence, then the organization policy fence) in
// its own transaction, as persistAuthenticatedIncomingMessageBeforeAck does,
// so a test can interleave it with a reply exactly.
type legacyReplyAdmissionFence struct {
	tx  *gorm.DB
	pid int
}

func holdLegacyReplyAdmissionFence(
	ctx context.Context,
	t *testing.T,
	fixture *legacyReplyFixture,
) *legacyReplyAdmissionFence {
	t.Helper()
	tx := fixture.app.DB.WithContext(ctx).Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { _ = tx.Rollback().Error })
	require.NoError(t, database.LockWhatsAppWAMIDScopes(
		tx,
		fixture.organization.ID,
		"wamid.fence-"+uuid.NewString(),
	))
	require.NoError(t, database.LockOrganizationPolicyScope(tx, fixture.organization.ID))
	fence := &legacyReplyAdmissionFence{tx: tx}
	require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).
		Raw("SELECT pg_backend_pid()").Scan(&fence.pid).Error)
	return fence
}

// ensureShadow continues like the admission and locks the reply's shadow.
func (fence *legacyReplyAdmissionFence) ensureShadow(fixture *legacyReplyFixture) error {
	_, err := channelapi.EnsureLegacyMetaWhatsAppAccount(fence.tx, channelapi.LegacyMetaAccountRef{
		ID:             fixture.selected.ID,
		OrganizationID: fixture.organization.ID,
		Name:           fixture.selected.Name,
		Status:         fixture.selected.Status,
	})
	return err
}

// waiter returns the query of the one backend waiting on the fence, if any.
func (fence *legacyReplyAdmissionFence) waiter(db *gorm.DB) (string, bool) {
	var waiters []struct{ Query string }
	if db.Raw(
		`SELECT query FROM pg_catalog.pg_stat_activity
		  WHERE ? = ANY(pg_catalog.pg_blocking_pids(pid))`,
		fence.pid,
	).Scan(&waiters).Error != nil || len(waiters) != 1 {
		return "", false
	}
	return waiters[0].Query, true
}

func (fence *legacyReplyAdmissionFence) waiting(db *gorm.DB) bool {
	var blockers []int64
	return db.Raw("SELECT unnest(pg_catalog.pg_blocking_pids(?))", fence.pid).
		Scan(&blockers).Error == nil && len(blockers) > 0
}

// A strict reply's pre-provider transaction inserts the pending Message, which
// locks the organization row, while it owns the legacy shadow. It must take the
// organization first and queue behind a Coexistence admission's policy fence
// instead of holding the shadow that admission needs next.
func TestSendLegacyWhatsAppConversationReplyQueuesBehindCoexistenceAdmissionFence(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fence := holdLegacyReplyAdmissionFence(ctx, t, fixture)

	request := legacyReplyRequest(t, fixture, "Reply racing an inbound admission")
	var replyErr error
	replied := make(chan struct{})
	go func() {
		defer close(replied)
		replyErr = fixture.app.SendLegacyWhatsAppConversationReply(request)
	}()
	defer func() {
		_ = fence.tx.Rollback().Error
		select {
		case <-replied:
		case <-time.After(10 * time.Second):
			t.Error("legacy reply did not terminate after the admission ended")
		}
	}()

	var waitingQuery string
	require.Eventually(t, func() bool {
		var waiting bool
		waitingQuery, waiting = fence.waiter(fixture.app.DB)
		return waiting
	}, 10*time.Second, 10*time.Millisecond, "the reply must wait on the admission's policy fence")
	var probed models.ChannelAccount
	require.NoError(t, fence.tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "NOWAIT"}).
		Select("id").
		Where("id = ? AND organization_id = ?", fixture.shadow.ID, fixture.organization.ID).
		First(&probed).Error,
		"the reply owned the shadow while waiting in %q", waitingQuery)
	require.NoError(t, fence.ensureShadow(fixture))
	require.NoError(t, fence.tx.Commit().Error)

	select {
	case <-replied:
	case <-time.After(10 * time.Second):
		require.Fail(t, "legacy reply did not finish after the admission committed")
	}
	require.NoError(t, replyErr)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	assert.Equal(t, 1, fixture.mock.sentMessageCount())
	var persisted models.Message
	require.NoError(t, fixture.app.DB.Where(
		"id = ? AND organization_id = ?",
		legacyReplyResponseID(t, request),
		fixture.organization.ID,
	).First(&persisted).Error)
	assert.Equal(t, models.MessageStatusSent, persisted.Status)
	assert.NotEmpty(t, persisted.WhatsAppMessageID)
}

// The provider transaction holds the shadow across the Meta call but never
// waits for the organization row, so a Coexistence admission can fence the
// tenant mid-send and then wait for that shadow. The WAMID owner trigger's
// FOR SHARE NOWAIT then fails the provider transaction; recovery must wait for
// the fence and still record the provider's WAMID without calling Meta again.
func TestSendLegacyWhatsAppConversationReplyRecordsWAMIDWhenAdmissionFencesDuringSend(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	inSend := make(chan struct{})
	release := make(chan struct{})
	var inSendOnce, releaseOnce sync.Once
	releaseSend := func() { releaseOnce.Do(func() { close(release) }) }
	fixture.mock.messageHook = func() {
		inSendOnce.Do(func() { close(inSend) })
		<-release
	}
	defer releaseSend()

	request := legacyReplyRequest(t, fixture, "Reply sent while an inbound admission fences")
	var replyErr error
	replied := make(chan struct{})
	go func() {
		defer close(replied)
		replyErr = fixture.app.SendLegacyWhatsAppConversationReply(request)
	}()
	select {
	case <-inSend:
	case <-replied:
		require.Failf(t, "legacy reply finished before calling Meta", "error: %v", replyErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "legacy reply never reached Meta")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fence := holdLegacyReplyAdmissionFence(ctx, t, fixture)
	var shadowErr error
	shadowed := make(chan struct{})
	go func() {
		defer close(shadowed)
		shadowErr = fence.ensureShadow(fixture)
	}()
	defer func() {
		releaseSend()
		select {
		case <-shadowed:
		case <-time.After(10 * time.Second):
			t.Error("admission shadow lock did not terminate")
		}
		_ = fence.tx.Rollback().Error
		select {
		case <-replied:
		case <-time.After(10 * time.Second):
			t.Error("legacy reply did not terminate after the admission ended")
		}
	}()
	require.Eventually(t, func() bool { return fence.waiting(fixture.app.DB) },
		10*time.Second, 10*time.Millisecond, "the admission must wait for the shadow held across Meta")

	releaseSend()
	select {
	case <-shadowed:
	case <-time.After(10 * time.Second):
		require.Fail(t, "the provider transaction never released the shadow")
	}
	require.NoError(t, shadowErr)
	// The provider transaction gave up the shadow; its recovery now either
	// queues behind the admission or has already given up.
	require.Eventually(t, func() bool {
		select {
		case <-replied:
			return true
		default:
		}
		_, waiting := fence.waiter(fixture.app.DB)
		return waiting
	}, 10*time.Second, 10*time.Millisecond, "delivery recovery never reached the admission fence")
	require.NoError(t, fence.tx.Commit().Error)

	select {
	case <-replied:
	case <-time.After(15 * time.Second):
		require.Fail(t, "legacy reply did not finish after the admission committed")
	}
	require.NoError(t, replyErr)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	assert.Equal(t, 1, fixture.mock.sentMessageCount(), "recovery must not call Meta again")
	var persisted models.Message
	require.NoError(t, fixture.app.DB.Where(
		"organization_id = ? AND inbox_conversation_id = ? AND direction = ?",
		fixture.organization.ID,
		fixture.conversation.ID,
		models.DirectionOutgoing,
	).First(&persisted).Error)
	assert.Equal(t, models.MessageStatusSent, persisted.Status)
	assert.Equal(t, fixture.mock.nextMessageID, persisted.WhatsAppMessageID)
}

// A reply whose pre-provider transaction queues for a shadow that another
// reply's provider transaction holds across its Meta call must not hold the
// organization row meanwhile. PostgreSQL lets a new FOR SHARE overtake a
// waiting FOR UPDATE, so queued SHARE holders would keep every policy fence in
// the tenant (each inbound admission) out until those Meta calls return.
func TestSendLegacyWhatsAppConversationReplyQueuedBehindSendLeavesPolicyFenceAvailable(t *testing.T) {
	fixture := newLegacyReplyFixture(t)
	inSend := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var releaseOnce sync.Once
	releaseSend := func() { releaseOnce.Do(func() { close(release) }) }
	fixture.mock.messageHook = func() {
		if calls.Add(1) == 1 {
			close(inSend)
			<-release
			return
		}
		fixture.mock.mu.Lock()
		fixture.mock.nextMessageID = "wamid.test-second-" + uuid.NewString()[:8]
		fixture.mock.mu.Unlock()
	}
	defer releaseSend()

	first := legacyReplyRequest(t, fixture, "First reply holds the shadow across Meta")
	var firstErr, secondErr error
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		firstErr = fixture.app.SendLegacyWhatsAppConversationReply(first)
	}()
	select {
	case <-inSend:
	case <-firstDone:
		require.Failf(t, "first reply finished before calling Meta", "error: %v", firstErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "first reply never reached Meta")
	}
	second := legacyReplyRequest(t, fixture, "Second reply queues behind the first")
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		secondErr = fixture.app.SendLegacyWhatsAppConversationReply(second)
	}()
	defer func() {
		releaseSend()
		for _, done := range []chan struct{}{firstDone, secondDone} {
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("legacy reply did not terminate")
			}
		}
	}()
	require.Eventually(t, func() bool {
		var queued int64
		return fixture.app.DB.Raw(
			`SELECT count(*) FROM pg_catalog.pg_stat_activity
			  WHERE datname = pg_catalog.current_database()
			    AND wait_event_type = 'Lock'
			    AND query ILIKE '%channel_accounts%'`,
		).Scan(&queued).Error == nil && queued == 1
	}, 10*time.Second, 10*time.Millisecond, "the second reply must queue on the shadow held across Meta")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fence := fixture.app.DB.WithContext(ctx).Begin()
	require.NoError(t, fence.Error)
	defer func() { _ = fence.Rollback().Error }()
	require.NoError(t, fence.Exec("SET LOCAL lock_timeout = '1s'").Error)
	require.NoError(t, database.LockOrganizationPolicyScope(fence, fixture.organization.ID),
		"the queued reply held the organization row")
	require.NoError(t, fence.Commit().Error)

	releaseSend()
	for _, done := range []chan struct{}{firstDone, secondDone} {
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			require.Fail(t, "legacy reply did not finish after Meta returned")
		}
	}
	require.NoError(t, firstErr)
	require.NoError(t, secondErr)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(second))
	assert.Equal(t, 2, fixture.mock.sentMessageCount())
	var sent int64
	require.NoError(t, fixture.app.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND inbox_conversation_id = ? AND direction = ? AND status = ? AND BTRIM(whats_app_message_id) <> ''",
		fixture.organization.ID,
		fixture.conversation.ID,
		models.DirectionOutgoing,
		models.MessageStatusSent,
	).Count(&sent).Error)
	assert.EqualValues(t, 2, sent)
}
