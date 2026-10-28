package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const originTestKey = "sender_origin_v1"

type originRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f originRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type originTestCredentials struct {
	key, token string
	row        models.APIKey
}

func originCredentials(t *testing.T, db *gorm.DB, user *models.User) originTestCredentials {
	t.Helper()
	key := "whm_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	hash, err := bcrypt.GenerateFromPassword([]byte(key), bcrypt.MinCost)
	require.NoError(t, err)
	row := models.APIKey{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: user.OrganizationID,
		UserID: user.ID, Name: "Synthetic origin", KeyPrefix: key[4:20], KeyHash: string(hash), IsActive: true}
	require.NoError(t, db.Create(&row).Error)
	claims := middleware.JWTClaims{UserID: user.ID, OrganizationID: user.OrganizationID, Email: user.Email,
		RegisteredClaims: jwt.RegisteredClaims{Subject: middleware.JWTSubjectAccess,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testutil.TestJWTSecret))
	require.NoError(t, err)
	return originTestCredentials{key, token, row}
}

func originAuthenticate(t *testing.T, db *gorm.DB, r *fastglue.Request, c originTestCredentials, method string) {
	t.Helper()
	r.RequestCtx.Request.Header.Del("X-API-Key")
	r.RequestCtx.Request.Header.Del("Authorization")
	r.RequestCtx.Request.Header.DelCookie("whm_access")
	switch method {
	case "api_key":
		r.RequestCtx.Request.Header.Set("X-API-Key", c.key)
	case "cookie":
		r.RequestCtx.Request.Header.SetCookie("whm_access", c.token)
	default:
		r.RequestCtx.Request.Header.Set("Authorization", "Bearer "+c.token)
	}
	require.NotNil(t, middleware.AuthWithDB(testutil.TestJWTSecret, db)(r))
	want := method
	if want != "api_key" {
		want = "user_jwt"
	}
	require.Equal(t, want, middleware.AuthenticatedRequestMethod(r))
}

func originJoin(t *testing.T, app *handlers.App) {
	t.Helper()
	done := make(chan struct{})
	go func() { app.WaitForBackgroundTasks(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("origin send background work did not join")
	}
}

func originOnlyMessage(t *testing.T, db *gorm.DB, orgID, contactID uuid.UUID) models.Message {
	t.Helper()
	var rows []models.Message
	require.NoError(t, db.Where("organization_id = ? AND contact_id = ? AND direction = ?", orgID, contactID, models.DirectionOutgoing).Find(&rows).Error)
	require.Len(t, rows, 1)
	return rows[0]
}

func TestSenderOriginCheckedAuthentication(t *testing.T) {
	f := laneENewForwardFixture(t)
	c := originCredentials(t, f.app.DB, f.user)
	r := testutil.NewJSONRequest(t, map[string]any{originTestKey: "user_jwt"})
	r.RequestCtx.Request.Header.Set("X-Sender-Origin", "user_jwt")
	for _, method := range []string{"api_key", "user_jwt", "cookie"} {
		originAuthenticate(t, f.app.DB, r, c, method)
	}
	// API-key admission takes precedence over a valid bearer, and an invalid
	// supplied key must not fall through or retain a prior successful marker.
	r.RequestCtx.Request.Header.Set("Authorization", "Bearer "+c.token)
	r.RequestCtx.Request.Header.Set("X-API-Key", c.key)
	require.NotNil(t, middleware.AuthWithDB(testutil.TestJWTSecret, f.app.DB)(r))
	assert.Equal(t, "api_key", middleware.AuthenticatedRequestMethod(r))
	r.RequestCtx.Request.Header.Set("X-API-Key", "whm_invalid_synthetic_key")
	require.Nil(t, middleware.AuthWithDB(testutil.TestJWTSecret, f.app.DB)(r))
	assert.Empty(t, middleware.AuthenticatedRequestMethod(r))
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(r))

	originAuthenticate(t, f.app.DB, r, c, "api_key")
	past := time.Now().Add(-time.Hour)
	require.NoError(t, f.app.DB.Model(&c.row).Update("expires_at", past).Error)
	require.Nil(t, middleware.AuthWithDB(testutil.TestJWTSecret, f.app.DB)(r))
	assert.Empty(t, middleware.AuthenticatedRequestMethod(r))
	originAuthenticate(t, f.app.DB, r, c, "user_jwt")
	require.NoError(t, f.app.DB.Model(f.user).Update("is_active", false).Error)
	require.Nil(t, middleware.AuthWithDB(testutil.TestJWTSecret, f.app.DB)(r))
	assert.Empty(t, middleware.AuthenticatedRequestMethod(r))
	require.NoError(t, f.app.DB.Model(f.user).Update("is_active", true).Error)
	originAuthenticate(t, f.app.DB, r, c, "user_jwt")
	faultDB, hits := authorityFaultDB(t, f.app.DB, func(tx *gorm.DB) bool { return tx.Statement.Table == "users" }, context.DeadlineExceeded)
	require.Nil(t, middleware.AuthWithDB(testutil.TestJWTSecret, faultDB)(r))
	assert.Positive(t, hits.Load())
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(r))
	assert.Empty(t, middleware.AuthenticatedRequestMethod(r))
}

func originRoutedRequest(t *testing.T, f *laneEForwardFixture, kind, opposite string) (*fastglue.Request, handlers.TenantRequestHandler, *models.Message) {
	t.Helper()
	var source *models.Message
	var r *fastglue.Request
	handler := handlers.TenantRequestHandler((*handlers.App).SendMessage)
	switch kind {
	case "media":
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		require.NoError(t, writer.WriteField("contact_id", f.target.ID.String()))
		require.NoError(t, writer.WriteField("type", "document"))
		require.NoError(t, writer.WriteField(originTestKey, opposite))
		part, err := writer.CreateFormFile("file", "synthetic.pdf")
		require.NoError(t, err)
		_, err = part.Write([]byte("%PDF-1.4 synthetic origin fixture"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		r = testutil.NewRequest(t)
		r.RequestCtx.Request.Header.SetMethod("POST")
		r.RequestCtx.Request.Header.SetContentType(writer.FormDataContentType())
		r.RequestCtx.Request.SetBody(body.Bytes())
		handler = (*handlers.App).SendMediaMessage
	case "template":
		tpl := createTestTemplate(t, f.app, f.org.ID, f.account.Name)
		r = testutil.NewJSONRequest(t, map[string]any{"contact_id": f.target.ID.String(), "template_name": tpl.Name,
			"template_params": map[string]string{"name": "Synthetic", "order_id": "fixture"}, originTestKey: opposite})
		handler = (*handlers.App).SendTemplateMessage
	case "forward":
		source = f.storedMessage(t, f.source, models.MessageTypeText, "Synthetic original", "")
		require.NoError(t, f.app.DB.Model(source).Update("metadata", models.JSONB{originTestKey: opposite}).Error)
		r = testutil.NewJSONRequest(t, map[string]any{"type": "text", "content": map[string]string{"body": ""},
			"forward_of_message_id": source.ID.String(), originTestKey: opposite})
	default:
		r = testutil.NewJSONRequest(t, map[string]any{"type": "text", "content": map[string]string{"body": "Synthetic reply"}, originTestKey: opposite})
	}
	testutil.SetPathParam(r, "id", f.target.ID.String())
	r.RequestCtx.Request.Header.Set("X-Sender-Origin", opposite)
	return r, handler, source
}

func TestSenderOriginFourRoutedWritersBeforeProviderSettlement(t *testing.T) {
	for _, kind := range []string{"text", "media", "template", "forward"} {
		for _, method := range []string{"api_key", "user_jwt"} {
			t.Run(kind+"/"+method, func(t *testing.T) {
				f := laneENewForwardFixture(t)
				c := originCredentials(t, f.app.DB, f.user)
				opposite := "api_key"
				if method == opposite {
					opposite = "user_jwt"
				}
				r, handler, source := originRoutedRequest(t, f, kind, opposite)
				originAuthenticate(t, f.app.DB, r, c, method)
				entered, release := make(chan struct{}), make(chan struct{})
				var arrived, released sync.Once
				unblock := func() { released.Do(func() { close(release) }) }
				f.mock.messageHook = func() {
					arrived.Do(func() { close(entered) })
					select {
					case <-release:
					case <-time.After(15 * time.Second):
					}
				}
				done := make(chan error, 1)
				go func() { done <- f.app.Tenant(handler)(r) }()
				joined := false
				defer func() {
					unblock()
					if !joined {
						select {
						case <-done:
						case <-time.After(20 * time.Second):
							t.Error("routed send did not join")
						}
					}
					originJoin(t, f.app)
				}()
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("routed send did not reach synthetic provider")
				}
				pending := originOnlyMessage(t, f.app.DB, f.org.ID, f.target.ID)
				assert.Equal(t, models.MessageStatusPending, pending.Status)
				assert.Equal(t, method, pending.Metadata[originTestKey])
				require.NotNil(t, pending.SentByUserID)
				assert.Equal(t, f.user.ID, *pending.SentByUserID)
				if kind == "template" {
					assert.NotEmpty(t, pending.Metadata["template_name"])
					assert.NotEmpty(t, pending.Metadata["template_id"])
				}
				if kind == "forward" {
					assert.NotNil(t, pending.Metadata["forwarded_from"])
				}
				unblock()
				select {
				case err := <-done:
					joined = true
					require.NoError(t, err)
				case <-time.After(20 * time.Second):
					t.Fatal("routed send did not finish")
				}
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r))
				originJoin(t, f.app)
				stored := originOnlyMessage(t, f.app.DB, f.org.ID, f.target.ID)
				assert.Equal(t, pending.ID, stored.ID)
				assert.Equal(t, models.MessageStatusSent, stored.Status)
				assert.Equal(t, method, stored.Metadata[originTestKey])
				assert.Equal(t, 1, f.mock.sentMessageCount())
				encoded, err := json.Marshal(stored.Metadata)
				require.NoError(t, err)
				assert.NotContains(t, string(encoded), c.key)
				assert.NotContains(t, string(encoded), c.token)
				assert.NotContains(t, string(encoded), c.row.KeyHash)
				if source != nil {
					var original models.Message
					require.NoError(t, f.app.DB.First(&original, "id = ?", source.ID).Error)
					assert.Equal(t, opposite, original.Metadata[originTestKey])
				}
			})
		}
	}
}

func TestSenderOriginLegacyReplyFirstClaimSurvivesMixedAuthReplay(t *testing.T) {
	for _, firstMethod := range []string{"api_key", "user_jwt"} {
		for _, finalStatus := range []models.MessageStatus{models.MessageStatusSent, models.MessageStatusPending, models.MessageStatusFailed} {
			t.Run(firstMethod+"/"+string(finalStatus), func(t *testing.T) {
				f := newLegacyReplyFixture(t)
				c := originCredentials(t, f.app.DB, &f.user)
				key := "origin-" + uuid.NewString()
				first := legacyReplyRequestWithKey(t, f, "One synthetic logical reply", key)
				originAuthenticate(t, f.app.DB, first, c, firstMethod)
				require.NoError(t, f.app.SendLegacyWhatsAppConversationReply(first))
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))
				id := legacyReplyResponseID(t, first)
				var stored models.Message
				require.NoError(t, f.app.DB.First(&stored, "id = ?", id).Error)
				require.Equal(t, firstMethod, stored.Metadata[originTestKey])
				require.NoError(t, f.app.DB.Model(&stored).Update("status", finalStatus).Error)
				other := "api_key"
				if firstMethod == other {
					other = "user_jwt"
				}
				otherUser := testutil.CreateTestUser(t, f.app.DB, f.organization.ID, testutil.WithRoleID(f.user.RoleID))
				otherCredentials := originCredentials(t, f.app.DB, otherUser)
				second := legacyReplyRequestWithKey(t, f, "One synthetic logical reply", key)
				originAuthenticate(t, f.app.DB, second, otherCredentials, other)
				require.NoError(t, f.app.SendLegacyWhatsAppConversationReply(second))
				require.Equal(t, id, legacyReplyResponseID(t, second))
				require.NoError(t, f.app.DB.First(&stored, "id = ?", id).Error)
				assert.Equal(t, firstMethod, stored.Metadata[originTestKey])
				require.NotNil(t, stored.SentByUserID)
				assert.Equal(t, f.user.ID, *stored.SentByUserID)
				assert.Equal(t, finalStatus, stored.Status)
				assert.Equal(t, 1, f.mock.sentMessageCount(), "a replay must not call the provider again")
			})
		}
	}
}

func TestSenderOriginLegacyFailureAndMissingReceiptPreserveSnapshot(t *testing.T) {
	for _, outcome := range []string{"rejected", "missing_receipt"} {
		for _, method := range []string{"api_key", "user_jwt"} {
			t.Run(outcome+"/"+method, func(t *testing.T) {
				f := newLegacyReplyFixture(t)
				c := originCredentials(t, f.app.DB, &f.user)
				if outcome == "rejected" {
					f.mock.returnError, f.mock.errorMessage = true, "Synthetic rejection"
				} else {
					f.mock.nextMessageID = ""
				}
				r := legacyReplyRequest(t, f, "Synthetic provider outcome")
				originAuthenticate(t, f.app.DB, r, c, method)
				require.NoError(t, f.app.SendLegacyWhatsAppConversationReply(r))
				var message models.Message
				require.NoError(t, f.app.DB.Where("organization_id = ? AND inbox_conversation_id = ? AND direction = ?", f.organization.ID, f.conversation.ID, models.DirectionOutgoing).First(&message).Error)
				assert.Equal(t, method, message.Metadata[originTestKey])
				assert.NotContains(t, []models.MessageStatus{models.MessageStatusSent, models.MessageStatusDelivered, models.MessageStatusRead}, message.Status)
				paths, _ := f.mock.messageRequestSnapshot()
				assert.Len(t, paths, 1)
			})
		}
	}
}

type originNativeFixture struct {
	*laneEForwardFixture
	conversation models.InboxConversation
	channel      models.ChannelAccount
	credentials  originTestCredentials
}

func originNewNative(t *testing.T) *originNativeFixture {
	t.Helper()
	f := laneENewForwardFixture(t)
	enableLegacyReplyOmnichannel(t, f.app.DB, f.org.ID, f.user.ID)
	account := models.ChannelAccount{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: f.org.ID,
		Channel: models.ChannelWebChat, Provider: channelapi.RelayProvider, Name: "Synthetic origin",
		ExternalAccountID: "origin-" + uuid.NewString(), Status: models.ChannelAccountStatusActive,
		Config: models.JSONB{"outbound_enabled": true}, Capabilities: models.JSONB{"text": true}, Metadata: models.JSONB{}}
	require.NoError(t, f.app.DB.Create(&account).Error)
	credential := models.ChannelCredential{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: f.org.ID,
		ChannelAccountID: account.ID, Kind: models.ChannelCredentialKindPrimary, Version: 1,
		Status: models.ChannelCredentialStatusActive, KeyVersion: "synthetic", CredentialBlob: models.JSONB{}, Metadata: models.JSONB{}}
	require.NoError(t, f.app.DB.Create(&credential).Error)
	conversation := models.InboxConversation{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: f.org.ID,
		ChannelAccountID: account.ID, ContactID: f.target.ID, Channel: models.ChannelWebChat,
		ExternalConversationID: "origin-" + uuid.NewString(), Status: models.InboxConversationStatusOpen, Config: models.JSONB{}}
	require.NoError(t, f.app.DB.Create(&conversation).Error)
	return &originNativeFixture{f, conversation, account, originCredentials(t, f.app.DB, f.user)}
}

func (f *originNativeFixture) request(t *testing.T, key, method string) *fastglue.Request {
	t.Helper()
	r := testutil.NewJSONRequest(t, map[string]any{"idempotency_key": key, "purpose": "service",
		"parts": []map[string]string{{"type": "text", "text": "Synthetic native reply"}}, originTestKey: "forged"})
	testutil.SetPathParam(r, "id", f.conversation.ID.String())
	originAuthenticate(t, f.app.DB, r, f.credentials, method)
	return r
}

func (f *originNativeFixture) stored(t *testing.T, key string) (models.Message, models.OutboxJob) {
	t.Helper()
	var jobs []models.OutboxJob
	require.NoError(t, f.app.DB.Where("organization_id = ? AND channel_account_id = ? AND idempotency_key = ?", f.org.ID, f.channel.ID, key).Find(&jobs).Error)
	require.Len(t, jobs, 1)
	require.NotNil(t, jobs[0].MessageID)
	var message models.Message
	require.NoError(t, f.app.DB.First(&message, "id = ?", *jobs[0].MessageID).Error)
	return message, jobs[0]
}

func TestSenderOriginNativeWriterAndMixedAuthReplay(t *testing.T) {
	for _, firstMethod := range []string{"api_key", "user_jwt"} {
		t.Run(firstMethod, func(t *testing.T) {
			f := originNewNative(t)
			key := "origin-" + uuid.NewString()
			first := f.request(t, key, firstMethod)
			require.NoError(t, f.app.Tenant((*handlers.App).SendInboxConversationMessage)(first))
			// Pinned fastglue SendEnvelope writes 200 even after the handler's
			// preceding SetStatusCode(202); preserve the actual current contract.
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first), string(testutil.GetResponseBody(first)))
			message, job := f.stored(t, key)
			assert.Equal(t, models.MessageStatusPending, message.Status)
			assert.Equal(t, firstMethod, message.Metadata[originTestKey])
			var outbound channelapi.OutboundMessage
			encoded, err := json.Marshal(job.Payload)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(encoded, &outbound))
			assert.Equal(t, firstMethod, outbound.Metadata[originTestKey])
			assert.Equal(t, key, message.Metadata["idempotency_key"])
			assert.NotEmpty(t, message.Metadata["payload_digest"])
			other := "api_key"
			if firstMethod == other {
				other = "user_jwt"
			}
			otherUser := testutil.CreateTestUser(t, f.app.DB, f.org.ID, testutil.WithRoleID(f.user.RoleID))
			otherCredentials := originCredentials(t, f.app.DB, otherUser)
			for _, status := range []models.MessageStatus{models.MessageStatusPending, models.MessageStatusFailed, models.MessageStatusSent} {
				require.NoError(t, f.app.DB.Model(&models.Message{}).Where("id = ?", message.ID).Update("status", status).Error)
				replay := f.request(t, key, other)
				originAuthenticate(t, f.app.DB, replay, otherCredentials, other)
				require.NoError(t, f.app.Tenant((*handlers.App).SendInboxConversationMessage)(replay))
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(replay))
				stored, savedJob := f.stored(t, key)
				assert.Equal(t, message.ID, stored.ID)
				assert.Equal(t, firstMethod, stored.Metadata[originTestKey])
				require.NotNil(t, stored.SentByUserID)
				assert.Equal(t, f.user.ID, *stored.SentByUserID)
				assert.Equal(t, status, stored.Status)
				assert.Equal(t, job.ID, savedJob.ID)
				assert.Equal(t, job.Payload, savedJob.Payload)
			}
			assert.Zero(t, f.mock.sentMessageCount(), "enqueue/replay does not itself dispatch an outbox job")
		})
	}
}

func TestSenderOriginRequestReuseAndRollbackPreserveCommitBoundary(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[rollback], func(t *testing.T) {
			f := laneENewForwardFixture(t)
			f.app.Config.Database.RLSEnabled = true
			c := originCredentials(t, f.app.DB, f.user)
			r, handler, _ := originRoutedRequest(t, f, "text", "user_jwt")
			originAuthenticate(t, f.app.DB, r, c, "api_key")
			errRollback := errors.New("synthetic outer transaction rollback")
			err := f.app.WithTenantApp(f.org.ID, func(scoped *handlers.App) error {
				if err := handler(scoped, r); err != nil {
					return err
				}
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(r))
				assert.Zero(t, f.mock.sentMessageCount(), "provider handoff must wait for outer commit")
				// Reuse the admitted request only after the handler has returned.
				// No callback may consult its new authentication marker.
				originAuthenticate(t, scoped.DB, r, c, "user_jwt")
				if rollback {
					return errRollback
				}
				return nil
			})
			if rollback {
				require.ErrorIs(t, err, errRollback)
			} else {
				require.NoError(t, err)
			}
			originJoin(t, f.app)
			var count int64
			require.NoError(t, f.app.DB.Model(&models.Message{}).Where("organization_id = ? AND contact_id = ? AND direction = ?", f.org.ID, f.target.ID, models.DirectionOutgoing).Count(&count).Error)
			if rollback {
				assert.Zero(t, count)
				assert.Zero(t, f.mock.sentMessageCount())
			} else {
				require.EqualValues(t, 1, count)
				stored := originOnlyMessage(t, f.app.DB, f.org.ID, f.target.ID)
				assert.Equal(t, "api_key", stored.Metadata[originTestKey])
				assert.Equal(t, models.MessageStatusSent, stored.Status)
				assert.Equal(t, 1, f.mock.sentMessageCount())
			}
		})
	}
}

func TestSenderOriginProviderFailurePreservesAttribution(t *testing.T) {
	for _, kind := range []string{"text", "media", "template", "forward"} {
		for _, method := range []string{"api_key", "user_jwt"} {
			t.Run(kind+"/"+method, func(t *testing.T) {
				f := laneENewForwardFixture(t)
				c := originCredentials(t, f.app.DB, f.user)
				f.mock.returnError, f.mock.errorMessage = true, "Synthetic permanent rejection"
				// Install on this fixture's client before any send. Delegate unchanged
				// to its existing local-server transport and keep only bounded counters;
				// the shared mock's upload list records successes, not rejected uploads.
				originalTransport := f.app.WhatsApp.HTTPClient.Transport
				require.NotNil(t, originalTransport)
				var observed struct {
					sync.Mutex
					uploads, rejectedUploads, messages, rejectedMessages, other, transportErrors int
				}
				f.app.WhatsApp.HTTPClient.Transport = originRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
					response, err := originalTransport.RoundTrip(request)
					observed.Lock()
					defer observed.Unlock()
					if err != nil {
						observed.transportErrors++
					}
					rejected := response != nil && response.StatusCode == http.StatusBadRequest
					switch {
					case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/v18.0/") && strings.HasSuffix(request.URL.Path, "/media"):
						observed.uploads++
						if rejected {
							observed.rejectedUploads++
						}
					case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/v18.0/") && strings.HasSuffix(request.URL.Path, "/messages"):
						observed.messages++
						if rejected {
							observed.rejectedMessages++
						}
					default:
						observed.other++
					}
					return response, err
				})
				r, handler, _ := originRoutedRequest(t, f, kind, "forged")
				originAuthenticate(t, f.app.DB, r, c, method)
				require.NoError(t, f.app.Tenant(handler)(r))
				originJoin(t, f.app)
				message := originOnlyMessage(t, f.app.DB, f.org.ID, f.target.ID)
				assert.Equal(t, models.MessageStatusFailed, message.Status)
				assert.Equal(t, method, message.Metadata[originTestKey])
				paths, _ := f.mock.messageRequestSnapshot()
				observed.Lock()
				uploads, rejectedUploads := observed.uploads, observed.rejectedUploads
				messages, rejectedMessages := observed.messages, observed.rejectedMessages
				other, transportErrors := observed.other, observed.transportErrors
				observed.Unlock()
				assert.Zero(t, other)
				assert.Zero(t, transportErrors, "failure must come from the injected provider response")
				if kind == "media" {
					assert.Equal(t, 1, uploads, "the upload must be attempted exactly once")
					assert.Equal(t, 1, rejectedUploads, "the injected upload400 must be observed")
					assert.Zero(t, messages)
					assert.Zero(t, rejectedMessages)
					assert.Empty(t, paths, "rejected upload must not reach message sending")
				} else {
					assert.Zero(t, uploads)
					assert.Zero(t, rejectedUploads)
					assert.Equal(t, 1, messages)
					assert.Equal(t, 1, rejectedMessages, "the injected message400 must be observed")
					assert.Len(t, paths, 1, "the injected message failure must be reached exactly once without retry")
				}
			})
		}
	}
}

func TestSenderOriginProviderMissingReceiptDoesNotGainReplyCredit(t *testing.T) {
	for _, method := range []string{"api_key", "user_jwt"} {
		t.Run(method, func(t *testing.T) {
			f := laneENewForwardFixture(t)
			c := originCredentials(t, f.app.DB, f.user)
			f.mock.nextMessageID = ""
			r, handler, _ := originRoutedRequest(t, f, "text", "forged")
			originAuthenticate(t, f.app.DB, r, c, method)
			require.NoError(t, f.app.Tenant(handler)(r))
			originJoin(t, f.app)
			message := originOnlyMessage(t, f.app.DB, f.org.ID, f.target.ID)
			assert.Equal(t, method, message.Metadata[originTestKey])
			assert.Empty(t, message.WhatsAppMessageID)
			assert.NotContains(t, []models.MessageStatus{models.MessageStatusSent, models.MessageStatusDelivered, models.MessageStatusRead}, message.Status)
			assert.Equal(t, 1, f.mock.sentMessageCount(), "missing receipt cannot cause a provenance-driven resend")
		})
	}
}

func TestSenderOriginNativeRollbackLeavesNoAttributedMessageOrJob(t *testing.T) {
	f := originNewNative(t)
	key := "origin-rollback-" + uuid.NewString()
	r := f.request(t, key, "api_key")
	privateDB, _ := authorityFaultDB(t, f.app.DB, func(*gorm.DB) bool { return false }, nil)
	f.app.DB = privateDB
	callback := "test:origin-outbox-rollback:" + uuid.NewString()
	hits := 0
	require.NoError(t, privateDB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "outbox_jobs" {
			hits++
			tx.AddError(errors.New("synthetic outbox insert refusal"))
		}
	}))
	defer func() { _ = privateDB.Callback().Create().Remove(callback) }()
	require.NoError(t, f.app.Tenant((*handlers.App).SendInboxConversationMessage)(r))
	require.Equal(t, 1, hits, "the actual native transaction must reach the failing outbox insertion")
	assert.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(r))
	for _, model := range []any{&models.Message{}, &models.OutboxJob{}} {
		var count int64
		query := privateDB.Model(model).Where("organization_id = ?", f.org.ID)
		if _, ok := model.(*models.Message); ok {
			query = query.Where("inbox_conversation_id = ?", f.conversation.ID)
		} else {
			query = query.Where("conversation_id = ?", f.conversation.ID)
		}
		require.NoError(t, query.Count(&count).Error)
		assert.Zero(t, count)
	}
	var conversation models.InboxConversation
	require.NoError(t, privateDB.First(&conversation, "id = ?", f.conversation.ID).Error)
	assert.Equal(t, f.conversation.Config, conversation.Config, "pause state must roll back with the message/outbox")
	assert.Zero(t, f.mock.sentMessageCount())
}

func TestSenderOriginLegacyConcurrentReplayKeepsFirstClaim(t *testing.T) {
	for _, firstMethod := range []string{"api_key", "user_jwt"} {
		t.Run(firstMethod, func(t *testing.T) {
			f := newLegacyReplyFixture(t)
			c := originCredentials(t, f.app.DB, &f.user)
			key := "origin-race-" + uuid.NewString()
			other := "api_key"
			if firstMethod == other {
				other = "user_jwt"
			}
			first := legacyReplyRequestWithKey(t, f, "Concurrent synthetic reply", key)
			second := legacyReplyRequestWithKey(t, f, "Concurrent synthetic reply", key)
			originAuthenticate(t, f.app.DB, first, c, firstMethod)
			originAuthenticate(t, f.app.DB, second, c, other)
			// Both senders use private callback registries over the real shared
			// pool. The provider phase holds the shadow before any message read;
			// observe that actual lock boundary, not an unreachable later query.
			firstDB, _ := authorityFaultDB(t, f.app.DB, func(*gorm.DB) bool { return false }, nil)
			secondDB, _ := authorityFaultDB(t, f.app.DB, func(*gorm.DB) bool { return false }, nil)
			f.app.DB = firstDB
			secondApp := newMsgTestApp(t, f.mock)
			secondApp.Config.LegacyWhatsAppReply.Enabled = true
			secondApp.Config.LegacyWhatsAppReply.AllowedOrganizationIDs = f.organization.ID.String()
			secondApp.DB = secondDB
			var firstPID, secondPID atomic.Int32
			callback := "test:origin-legacy-shadow:" + uuid.NewString()
			observeShadow := func(db *gorm.DB, pid *atomic.Int32) {
				require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
					locking, locked := tx.Statement.Clauses["FOR"].Expression.(clause.Locking)
					where, scoped := tx.Statement.Clauses["WHERE"].Expression.(clause.Where)
					if tx.Statement.Table != "channel_accounts" || !locked || locking.Strength != "UPDATE" || !scoped {
						return
					}
					exactScope := false
					for _, expression := range where.Exprs {
						condition, ok := expression.(clause.Expr)
						if ok && condition.SQL == "id = ? AND organization_id = ?" && len(condition.Vars) == 2 &&
							condition.Vars[0] == f.shadow.ID && condition.Vars[1] == f.organization.ID {
							exactScope = true
						}
					}
					if !exactScope {
						return
					}
					var current int32
					// NewDB clears the pending model/clauses while retaining this
					// transaction connection and its context. Raw uses the row
					// callback, so it cannot recursively enter this query observer.
					probeCtx, cancelProbe := context.WithTimeout(tx.Statement.Context, time.Second)
					err := tx.Session(&gorm.Session{NewDB: true}).WithContext(probeCtx).
						Raw("SELECT pg_backend_pid()").Scan(&current).Error
					cancelProbe()
					if err != nil {
						tx.AddError(err)
						return
					}
					pid.Store(current)
				}))
			}
			observeShadow(firstDB, &firstPID)
			observeShadow(secondDB, &secondPID)
			entered, release := make(chan struct{}), make(chan struct{})
			providerExited := make(chan struct{})
			var enteredOnce, releaseOnce, exitedOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			f.mock.messageHook = func() {
				defer exitedOnce.Do(func() { close(providerExited) })
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-time.After(15 * time.Second):
				}
			}
			firstDone, secondDone := make(chan error, 1), make(chan error, 1)
			firstJoined, secondJoined, secondStarted := false, false, false
			defer func() {
				unblock()
				if !firstJoined {
					select {
					case <-firstDone:
						firstJoined = true
					case <-time.After(20 * time.Second):
						t.Error("first legacy reply did not join")
					}
				}
				if secondStarted && !secondJoined {
					select {
					case <-secondDone:
						secondJoined = true
					case <-time.After(20 * time.Second):
						t.Error("replay did not join")
					}
				}
				if firstJoined && (!secondStarted || secondJoined) {
					_ = firstDB.Callback().Query().Remove(callback)
					_ = secondDB.Callback().Query().Remove(callback)
				}
			}()
			go func() { firstDone <- f.app.SendLegacyWhatsAppConversationReply(first) }()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("first reply never reached provider")
			}
			var pending models.Message
			require.NoError(t, f.app.DB.Where("organization_id = ? AND inbox_conversation_id = ? AND direction = ?", f.organization.ID, f.conversation.ID, models.DirectionOutgoing).First(&pending).Error)
			require.Equal(t, firstMethod, pending.Metadata[originTestKey])
			secondStarted = true
			go func() { secondDone <- secondApp.SendLegacyWhatsAppConversationReply(second) }()
			waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelWait()
			require.Eventually(t, func() bool {
				owner, waiter := firstPID.Load(), secondPID.Load()
				if owner <= 0 || waiter <= 0 || owner == waiter {
					return false
				}
				var queued int64
				return f.app.DB.WithContext(waitCtx).Raw(`SELECT count(*) FROM pg_catalog.pg_stat_activity
					WHERE datname = pg_catalog.current_database() AND pid = ?
					  AND wait_event_type = 'Lock' AND query ILIKE '%channel_accounts%'
					  AND ? = ANY(pg_catalog.pg_blocking_pids(pid))`, waiter, owner).Scan(&queued).Error == nil && queued == 1
			}, 10*time.Second, 10*time.Millisecond,
				"the exact replay backend must queue on the first sender's shadow lock")
			select {
			case <-providerExited:
				t.Fatal("provider hold ended before the overlapping lock witness")
			default:
			}
			unblock()
			select {
			case err := <-firstDone:
				firstJoined = true
				require.NoError(t, err)
			case <-time.After(20 * time.Second):
				t.Fatal("first reply did not finish")
			}
			select {
			case err := <-secondDone:
				secondJoined = true
				require.NoError(t, err)
			case <-time.After(20 * time.Second):
				t.Fatal("replay did not finish")
			}
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(second))
			assert.Equal(t, legacyReplyResponseID(t, first), legacyReplyResponseID(t, second))
			var stored models.Message
			require.NoError(t, f.app.DB.First(&stored, "id = ?", pending.ID).Error)
			assert.Equal(t, firstMethod, stored.Metadata[originTestKey])
			assert.Equal(t, 1, f.mock.sentMessageCount())
		})
	}
}

func TestSenderOriginNativeConcurrentReplayKeepsFirstPayload(t *testing.T) {
	for _, firstMethod := range []string{"api_key", "user_jwt"} {
		t.Run(firstMethod, func(t *testing.T) {
			f := originNewNative(t)
			other := "api_key"
			if firstMethod == other {
				other = "user_jwt"
			}
			key := "origin-race-" + uuid.NewString()
			first, second := f.request(t, key, firstMethod), f.request(t, key, other)
			firstDB, _ := authorityFaultDB(t, f.app.DB, func(*gorm.DB) bool { return false }, nil)
			secondDB, lookups := authorityFaultDB(t, f.app.DB, func(tx *gorm.DB) bool { return tx.Statement.Table == "inbox_conversations" }, nil)
			f.app.DB = firstDB
			secondApp := newMsgTestApp(t, f.mock)
			secondApp.DB = secondDB
			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			callback := "test:origin-first-outbox:" + uuid.NewString()
			require.NoError(t, firstDB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table != "outbox_jobs" {
					return
				}
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-time.After(15 * time.Second):
					tx.AddError(context.DeadlineExceeded)
				}
			}))
			firstDone, secondDone := make(chan error, 1), make(chan error, 1)
			firstJoined, secondJoined, secondStarted := false, false, false
			defer func() {
				unblock()
				if !firstJoined {
					select {
					case <-firstDone:
						firstJoined = true
					case <-time.After(20 * time.Second):
						t.Error("first enqueue did not join")
					}
				}
				if secondStarted && !secondJoined {
					select {
					case <-secondDone:
						secondJoined = true
					case <-time.After(20 * time.Second):
						t.Error("replay enqueue did not join")
					}
				}
				// A timeout is a fixture failure, not proof that a callback is idle.
				// Leave this private DB callback intact unless every started owner joined.
				if firstJoined && (!secondStarted || secondJoined) {
					assert.NoError(t, firstDB.Callback().Create().Remove(callback))
				}
			}()
			go func() { firstDone <- f.app.Tenant((*handlers.App).SendInboxConversationMessage)(first) }()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("first enqueue never reached outbox insertion")
			}
			secondStarted = true
			go func() { secondDone <- secondApp.Tenant((*handlers.App).SendInboxConversationMessage)(second) }()
			require.Eventually(t, func() bool { return lookups.Load() > 0 }, 10*time.Second, 10*time.Millisecond,
				"second enqueue must enter its real conversation read before first outbox insert commits")
			unblock()
			select {
			case err := <-firstDone:
				firstJoined = true
				require.NoError(t, err)
			case <-time.After(20 * time.Second):
				t.Fatal("first enqueue did not finish")
			}
			select {
			case err := <-secondDone:
				secondJoined = true
				require.NoError(t, err)
			case <-time.After(20 * time.Second):
				t.Fatal("replay enqueue did not finish")
			}
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first), string(testutil.GetResponseBody(first)))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(second), string(testutil.GetResponseBody(second)))
			message, job := f.stored(t, key)
			assert.Equal(t, firstMethod, message.Metadata[originTestKey])
			var outbound channelapi.OutboundMessage
			encoded, err := json.Marshal(job.Payload)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(encoded, &outbound))
			assert.Equal(t, firstMethod, outbound.Metadata[originTestKey])
			var rows int64
			require.NoError(t, f.app.DB.Model(&models.Message{}).Where("organization_id = ? AND inbox_conversation_id = ?", f.org.ID, f.conversation.ID).Count(&rows).Error)
			assert.EqualValues(t, 1, rows)
			assert.Zero(t, f.mock.sentMessageCount())
		})
	}
}
