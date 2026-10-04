package handlers_test

import (
	"fmt"
	"mime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

const servedMediaTestBody = "<svg xmlns=\"http://www.w3.org/2000/svg\" onload=\"alert(document.domain)\"/>"

type storedMediaFixture struct {
	storedType  string
	mimeType    string
	filename    string
	messageType models.MessageType
}

// serveStoredMedia serves one stored media object through ServeMedia behind the
// app-wide security headers, as the /api/media route does.
func serveStoredMedia(t *testing.T, fixture storedMediaFixture) *fastglue.Request {
	t.Helper()
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "media-type-reader", []string{"contacts:read"})
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
	contact := testutil.CreateTestContact(t, app.DB, org.ID)

	key := fmt.Sprintf("organizations/%s/messages/media/%s", org.ID, uuid.NewString())
	app.Config.Storage.Type = "s3"
	app.ObjectStore = &memoryObjectStore{objects: map[string]memoryObject{
		key: {data: []byte(servedMediaTestBody), contentType: fixture.storedType},
	}}
	messageType := fixture.messageType
	if messageType == "" {
		messageType = models.MessageTypeDocument
	}
	msg := &models.Message{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		ContactID:      contact.ID,
		Direction:      models.DirectionIncoming,
		MessageType:    messageType,
		MediaURL:       key,
		MediaMimeType:  fixture.mimeType,
		MediaFilename:  fixture.filename,
		Status:         models.MessageStatusDelivered,
	}
	require.NoError(t, app.DB.Create(msg).Error)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, org.ID, user.ID)
	testutil.SetPathParam(req, "message_id", msg.ID.String())
	middleware.SecurityHeaders()(req)

	require.NoError(t, app.ServeMedia(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Equal(t, servedMediaTestBody, string(testutil.GetResponseBody(req)))
	assert.Equal(t, "nosniff", string(req.RequestCtx.Response.Header.Peek("X-Content-Type-Options")))
	return req
}

// assertMediaDownload checks that the response downloads under wantFilename
// and stays inert if a browser renders it anyway.
func assertMediaDownload(t *testing.T, req *fastglue.Request, wantFilename string) {
	t.Helper()
	header := &req.RequestCtx.Response.Header
	disposition, params, err := mime.ParseMediaType(string(header.Peek("Content-Disposition")))
	require.NoError(t, err)
	assert.Equal(t, "attachment", disposition)
	assert.Equal(t, wantFilename, params["filename"])
	assert.Equal(t, "sandbox; default-src 'none'", string(header.Peek("Content-Security-Policy")),
		"the app-wide policy allows same-origin scripts and must be replaced")
}

func TestApp_ServeMedia_DownloadsScriptableTypesAsOpaqueBytes(t *testing.T) {
	cases := []struct {
		name       string
		storedType string
	}{
		{"html", "text/html"},
		{"html with parameters and capitals", "Text/HTML; charset=utf-8"},
		{"xhtml", "application/xhtml+xml"},
		{"svg", "image/svg+xml"},
		{"svg with parameters", "image/svg+xml; charset=utf-8"},
		{"other +xml type", "application/rss+xml"},
		{"text xml", "text/xml"},
		{"application xml", "application/xml"},
		{"xsl", "text/xsl"},
		{"javascript", "text/javascript"},
		{"application javascript", "application/javascript"},
		{"ecmascript", "application/ecmascript"},
		{"legacy javascript", "application/x-javascript"},
		{"multipart with html parts", "multipart/x-mixed-replace; boundary=part"},
		{"not a media type", "not a media type"},
		{"type without subtype", "html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := serveStoredMedia(t, storedMediaFixture{storedType: tc.storedType})
			assert.Equal(t, "application/octet-stream", string(req.RequestCtx.Response.Header.Peek("Content-Type")))
			assertMediaDownload(t, req, "media")
		})
	}
}

func TestApp_ServeMedia_ScriptableTypeFromMessageIsOpaqueToo(t *testing.T) {
	// Local files have no stored type; the message's type is used instead.
	req := serveStoredMedia(t, storedMediaFixture{mimeType: "image/svg+xml", messageType: models.MessageTypeImage})
	assert.Equal(t, "application/octet-stream", string(req.RequestCtx.Response.Header.Peek("Content-Type")))
	assertMediaDownload(t, req, "media")
}

func TestApp_ServeMedia_DownloadsOtherDocumentsWithTheirType(t *testing.T) {
	cases := []struct {
		name         string
		storedType   string
		filename     string
		wantType     string
		wantFilename string
	}{
		{"pdf without a name", "application/pdf", "", "application/pdf", "media.pdf"},
		{"pdf with a name", "application/pdf", "Invoice 42.pdf", "application/pdf", "Invoice 42.pdf"},
		{"plain text", "text/plain; charset=utf-8", "", "text/plain", "media.txt"},
		{"spreadsheet", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "media.xlsx"},
		{"image not shown inline", "image/tiff", "scan.tiff", "image/tiff", "scan.tiff"},
		{"generic bytes", "application/octet-stream", "", "application/octet-stream", "media"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := serveStoredMedia(t, storedMediaFixture{storedType: tc.storedType, filename: tc.filename})
			assert.Equal(t, tc.wantType, string(req.RequestCtx.Response.Header.Peek("Content-Type")))
			assertMediaDownload(t, req, tc.wantFilename)
		})
	}
}

func TestApp_ServeMedia_DownloadFilenameIsSafe(t *testing.T) {
	cases := []struct {
		name         string
		filename     string
		wantFilename string
	}{
		{"keeps the sender's html name", "receipt.html", "receipt.html"},
		{"drops a windows path", `C:\fakepath\report.html`, "report.html"},
		{"drops a unix path", "../../etc/report.html", "report.html"},
		{"drops a right-to-left override", "invoice\u202Efdp.html", "invoicefdp.html"},
		{"drops header line breaks", "report\r\nX-Injected: 1.html", "reportX-Injected: 1.html"},
		{"keeps quotes quoted", `say "hi".html`, `say "hi".html`},
		{"keeps non-ascii names", "résumé.html", "résumé.html"},
		{"falls back without a usable name", " ../.. ", "media"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := serveStoredMedia(t, storedMediaFixture{storedType: "text/html", filename: tc.filename})
			assert.Equal(t, "application/octet-stream", string(req.RequestCtx.Response.Header.Peek("Content-Type")))
			assertMediaDownload(t, req, tc.wantFilename)
			raw := string(req.RequestCtx.Response.Header.Peek("Content-Disposition"))
			assert.NotContains(t, raw, "\r")
			assert.NotContains(t, raw, "\n")
			assert.NotContains(t, raw, "\u202E")
		})
	}
}

func TestApp_ServeMedia_ServesRasterImagesAndPlayableMediaInline(t *testing.T) {
	cases := []struct {
		name        string
		storedType  string
		messageType models.MessageType
		wantType    string
	}{
		{"png", "image/png", models.MessageTypeImage, "image/png"},
		{"jpeg", "image/jpeg", models.MessageTypeImage, "image/jpeg"},
		{"webp sticker", "image/webp", models.MessageType("sticker"), "image/webp"},
		{"mp4", "video/mp4", models.MessageTypeVideo, "video/mp4"},
		{"voice note", "audio/ogg; codecs=opus", models.MessageTypeAudio, "audio/ogg"},
		{"capitalised mpeg audio", "Audio/MPEG", models.MessageTypeAudio, "audio/mpeg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := serveStoredMedia(t, storedMediaFixture{
				storedType:  tc.storedType,
				filename:    "customer.html",
				messageType: tc.messageType,
			})
			header := &req.RequestCtx.Response.Header
			assert.Equal(t, tc.wantType, string(header.Peek("Content-Type")))
			assert.Empty(t, header.Peek("Content-Disposition"))
			assert.Equal(t, "private, max-age=3600", string(header.Peek("Cache-Control")))
			assert.True(t, strings.HasPrefix(string(header.Peek("Content-Security-Policy")), "default-src 'self'"),
				"inline media keeps the app-wide policy")
		})
	}
}
