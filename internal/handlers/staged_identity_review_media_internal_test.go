package handlers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestSafeStagedIdentityReviewContentTypeRefusesScriptableTypes(t *testing.T) {
	cases := []struct {
		values []string
		want   string
	}{
		{[]string{"image/svg+xml"}, "application/octet-stream"},
		{[]string{"image/svg+xml", "image/svg+xml"}, "application/octet-stream"},
		{[]string{"", "image/svg+xml; charset=utf-8"}, "application/octet-stream"},
		{[]string{"video/vnd.custom+xml"}, "application/octet-stream"},
		{[]string{"audio/vnd.custom+xml"}, "application/octet-stream"},
		{[]string{"application/xhtml+xml"}, "application/octet-stream"},
		{[]string{"text/html"}, "application/octet-stream"},
		{[]string{"text/javascript"}, "application/octet-stream"},
		{[]string{"image/png"}, "image/png"},
		{[]string{"image/jpeg"}, "image/jpeg"},
		{[]string{"video/mp4"}, "video/mp4"},
		{[]string{"audio/ogg; codecs=opus"}, "audio/ogg"},
		{[]string{"application/pdf"}, "application/pdf"},
		{[]string{"", "image/webp"}, "image/webp"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, safeStagedIdentityReviewContentType(tc.values...), "%q", tc.values)
	}
}

// readyStagedIdentityReviewMedia stages and hydrates media whose webhook
// declares mimeType, then stores body as its bytes, the way a customer's file
// arrives labelled with whatever type the customer's client chose.
func readyStagedIdentityReviewMedia(
	t *testing.T,
	mimeType string,
	body []byte,
) (*App, models.InboundEvent, string) {
	t.Helper()
	app, account, event, _ := createStagedIdentityReviewMediaFixtureWithMimeType(t, mimeType)
	require.Equal(t, mimeType, coexistenceMediaPayloadString(event.Payload, "media_mime_type"))
	processor := NewCoexistenceMediaProcessor(app, time.Hour)
	require.NoError(t, processor.ProcessStagedEvent(context.Background(), account.OrganizationID, event.ID))
	require.NoError(t, app.DB.First(&event, event.ID).Error)
	require.Equal(t, "ready", coexistenceMediaPayloadString(event.Payload, "media_status"))

	mediaURL := coexistenceMediaPayloadString(event.Payload, "media_url")
	require.NoError(t, os.WriteFile(filepath.Join(app.Config.Storage.LocalPath, filepath.FromSlash(mediaURL)), body, 0o600))
	_, revision, _, _, supported := coexistenceStagedMediaRevision(&event)
	require.True(t, supported)
	return app, event, revision
}

func TestGetStagedContactIdentityReviewMediaDownloadsWithSafeType(t *testing.T) {
	cases := []struct {
		name     string
		mimeType string
		wantType string
	}{
		{"svg", "image/svg+xml", "application/octet-stream"},
		{"svg with parameters", "image/svg+xml; charset=utf-8", "application/octet-stream"},
		{"xhtml", "application/xhtml+xml", "application/octet-stream"},
		{"html", "text/html", "application/octet-stream"},
		{"javascript", "application/javascript", "application/octet-stream"},
		{"png", "image/png", "image/png"},
		{"jpeg", "image/jpeg", "image/jpeg"},
		{"mp4", "video/mp4", "video/mp4"},
	}
	body := []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(document.domain)"/>`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, event, revision := readyStagedIdentityReviewMedia(t, tc.mimeType, body)
			reviewer := createWhatsAppIdentityReviewResolver(t, app.DB, event.OrganizationID)

			req := testutil.NewGETRequest(t)
			testutil.SetAuthContext(req, event.OrganizationID, reviewer.ID)
			testutil.SetPathParam(req, "id", event.ID.String())
			testutil.SetPathParam(req, "revision", revision)
			require.NoError(t, app.GetStagedContactIdentityReviewMedia(req))

			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
			header := &req.RequestCtx.Response.Header
			assert.Equal(t, tc.wantType, string(header.Peek("Content-Type")))
			assert.Equal(t, "attachment", string(header.Peek("Content-Disposition")))
			assert.Equal(t, "nosniff", string(header.Peek("X-Content-Type-Options")))
			assert.Equal(t, "sandbox; default-src 'none'", string(header.Peek("Content-Security-Policy")))
			assert.Equal(t, "private, no-store", string(header.Peek("Cache-Control")))
			assert.Equal(t, body, testutil.GetResponseBody(req))
		})
	}
}
