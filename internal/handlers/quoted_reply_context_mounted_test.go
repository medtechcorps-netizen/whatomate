package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

func TestStoredReplyContextScopedMedia(t *testing.T) {
	f := newQuoteMediaFixture(t)
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jD1sAAAAASUVORK5CYII=")
	require.NoError(t, err)
	key := tenantObjectKey(f.org.ID, "messages", "images", uuid.NewString()+".png")
	f.store.objects[key] = quoteMediaFixtureBlob{body: png, mime: "image/png"}
	source := f.message(t, f.account, f.contact, models.MessageTypeImage, key, f.cutoff.Add(time.Minute), true)
	child := f.message(t, f.account, f.contact, models.MessageTypeText, "", f.cutoff.Add(2*time.Minute), true)
	caption := "Synthetic stored image caption"
	require.NoError(t, f.app.DB.Model(&source).UpdateColumns(map[string]any{
		"content": caption, "metadata": models.JSONB{},
	}).Error)
	require.NoError(t, f.app.DB.Model(&child).UpdateColumns(map[string]any{
		"is_reply": true, "reply_to_message_id": source.ID, "metadata": models.JSONB{},
	}).Error)
	require.Equal(t, source.InboxConversationID, child.InboxConversationID)
	unsupported := f.message(t, f.account, f.contact, models.MessageTypeVideo, "", f.cutoff.Add(3*time.Minute), true)
	unsupportedReply := f.message(t, f.account, f.contact, models.MessageTypeText, "", f.cutoff.Add(4*time.Minute), true)
	require.NoError(t, f.app.DB.Model(&unsupportedReply).UpdateColumns(map[string]any{
		"is_reply": true, "reply_to_message_id": unsupported.ID, "metadata": models.JSONB{},
	}).Error)
	require.Equal(t, unsupported.InboxConversationID, unsupportedReply.InboxConversationID)

	selected := f.org.ID.String()
	contextRead := func(t *testing.T, inbox bool, child models.Message) replyContextProjectionWire {
		t.Helper()
		thread := child.ContactID.String()
		if inbox {
			thread = child.InboxConversationID.String()
		}
		req := f.request(t, selected, thread, child.ID.String())
		if inbox {
			require.NoError(t, f.app.GetInboxConversationMessages(req))
			var wire struct {
				Data struct {
					Messages []struct {
						Message struct {
							ID uuid.UUID `json:"id"`
						} `json:"message"`
						replyContextProjectionWire
						ForwardedAttachment json.RawMessage `json:"forwarded_attachment"`
					} `json:"messages"`
				} `json:"data"`
			}
			require.Equal(t, fasthttp.StatusOK, req.RequestCtx.Response.StatusCode())
			require.Equal(t, "private, no-store", string(req.RequestCtx.Response.Header.Peek("Cache-Control")))
			require.NoError(t, json.Unmarshal(req.RequestCtx.Response.Body(), &wire))
			for _, row := range wire.Data.Messages {
				if row.Message.ID == child.ID {
					require.Empty(t, row.ForwardedAttachment)
					return row.replyContextProjectionWire
				}
			}
		} else {
			testutil.SetQueryParam(req, "account", f.account.Name)
			testutil.SetQueryParam(req, "acknowledge", false)
			require.NoError(t, f.app.GetMessages(req))
			var wire struct {
				Data struct {
					Messages []struct {
						ID uuid.UUID `json:"id"`
						replyContextProjectionWire
					} `json:"messages"`
				} `json:"data"`
			}
			require.Equal(t, fasthttp.StatusOK, req.RequestCtx.Response.StatusCode())
			require.Equal(t, "private, no-store", string(req.RequestCtx.Response.Header.Peek("Cache-Control")))
			require.NoError(t, json.Unmarshal(req.RequestCtx.Response.Body(), &wire))
			for _, row := range wire.Data.Messages {
				if row.ID == child.ID {
					return row.replyContextProjectionWire
				}
			}
		}
		t.Fatal("exact synthetic reply child is absent")
		return replyContextProjectionWire{}
	}
	native := contextRead(t, false, child)
	inbox := contextRead(t, true, child)
	require.NotNil(t, native.Original)
	require.Equal(t, native, inbox)
	require.True(t, native.Original.ScopedContext)
	require.True(t, inbox.Original.ScopedContext)
	require.Equal(t, source.ID.String(), native.Original.ID)
	require.Equal(t, caption, native.Original.Content["body"])
	require.Regexp(t, "^[0-9a-f]{64}$", native.Original.ImageRevision)

	unsupportedNative := contextRead(t, false, unsupportedReply)
	require.NotNil(t, unsupportedNative.Original)
	require.Equal(t, unsupported.ID.String(), unsupportedNative.Original.ID)
	require.False(t, unsupportedNative.Original.ScopedContext)
	require.Empty(t, unsupportedNative.Original.ImageRevision)
	require.False(t, unsupportedNative.Unavailable)
	require.Equal(t, replyContextProjectionWire{}, contextRead(t, true, unsupportedReply))
	require.Empty(t, f.store.reads)

	requestImage := func(t *testing.T, inbox bool, workspace, thread, childID, account, revision string) *fasthttp.Response {
		t.Helper()
		req := f.request(t, workspace, thread, childID)
		testutil.SetQueryParam(req, "revision", revision)
		var handle func(*fastglue.Request) error
		if inbox {
			handle = f.app.ServeInboxReplyContextMedia
		} else {
			handle = f.app.ServeNativeReplyContextMedia
			testutil.SetQueryParam(req, "account", account)
		}
		require.NoError(t, handle(req))
		require.Equal(t, "private, no-store", string(req.RequestCtx.Response.Header.Peek("Cache-Control")))
		return &req.RequestCtx.Response
	}
	revision := native.Original.ImageRevision
	for _, scope := range []struct {
		name   string
		inbox  bool
		thread string
	}{
		{"native", false, child.ContactID.String()},
		{"inbox", true, child.InboxConversationID.String()},
	} {
		t.Run(scope.name, func(t *testing.T) {
			before := len(f.store.reads)
			response := requestImage(t, scope.inbox, selected, scope.thread, child.ID.String(), f.account.Name, revision)
			require.Equal(t, fasthttp.StatusOK, response.StatusCode())
			require.Equal(t, "image/png", string(response.Header.ContentType()))
			require.Equal(t, "nosniff", string(response.Header.Peek("X-Content-Type-Options")))
			require.Equal(t, png, response.Body())
			require.Equal(t, quoteMediaFixtureRead{key: key, limit: forwardImageMaxBytes}, f.store.reads[before])
			for _, refused := range []struct {
				thread   string
				child    string
				revision string
			}{
				{uuid.NewString(), child.ID.String(), revision},
				{scope.thread, source.ID.String(), revision},
				{scope.thread, child.ID.String(), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
			} {
				response := requestImage(t, scope.inbox, selected, refused.thread, refused.child, f.account.Name, refused.revision)
				require.NotEqual(t, fasthttp.StatusOK, response.StatusCode())
				require.False(t, response.IsBodyStream())
			}
			require.Len(t, f.store.reads, before+1)
		})
	}

	f.setRole(t, f.chatOnly)
	before := len(f.store.reads)
	response := requestImage(t, true, selected, child.InboxConversationID.String(), child.ID.String(), f.account.Name, revision)
	require.Equal(t, fasthttp.StatusNotFound, response.StatusCode())
	require.Len(t, f.store.reads, before)
	f.setRole(t, f.both)

	f.store.beforeReturn = func(ctx context.Context) error {
		// Storage runs outside the initial authorization snapshot. A revocation
		// that commits here must cause the final authorization to discard bytes.
		return f.app.DB.WithContext(ctx).Model(&source).
			Update("metadata", models.JSONB{coexistenceMediaRevokedMetadataKey: true}).Error
	}
	response = requestImage(t, false, selected, child.ContactID.String(), child.ID.String(), f.account.Name, revision)
	require.Equal(t, fasthttp.StatusNotFound, response.StatusCode())
	require.False(t, response.IsBodyStream())
	require.NotContains(t, string(response.Body()), caption)
	require.Len(t, f.store.reads, before+1)
	f.store.beforeReturn = nil
	revoked := contextRead(t, true, child)
	require.Nil(t, revoked.Original)
	require.True(t, revoked.Unavailable)
	f.assertNoReadState(t, child)
}
