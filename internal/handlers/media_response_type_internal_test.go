package handlers

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatMediaResponseType(t *testing.T) {
	cases := []struct {
		contentType string
		wantType    string
		wantInline  bool
	}{
		{"image/png", "image/png", true},
		{"IMAGE/JPEG", "image/jpeg", true},
		{" image/webp ; q=1", "image/webp", true},
		{"video/mp4", "video/mp4", true},
		{"audio/ogg; codecs=opus", "audio/ogg", true},
		{"image/svg+xml", "application/octet-stream", false},
		{"application/xhtml+xml", "application/octet-stream", false},
		{"application/vnd.custom+xml; charset=utf-8", "application/octet-stream", false},
		{"text/html", "application/octet-stream", false},
		{"text/javascript1.5", "application/octet-stream", false},
		{"text/x-ecmascript", "application/octet-stream", false},
		{"", "application/octet-stream", false},
		{"html", "application/octet-stream", false},
		{"text/html/x", "application/octet-stream", false},
		{"application/pdf", "application/pdf", false},
		{"image/x-icon", "image/x-icon", false},
		{"text/plain; charset=utf-8", "text/plain", false},
	}
	for _, tc := range cases {
		servedType, inline := chatMediaResponseType(tc.contentType)
		assert.Equal(t, tc.wantType, servedType, tc.contentType)
		assert.Equal(t, tc.wantInline, inline, tc.contentType)
	}
}

func TestMediaDownloadFilename(t *testing.T) {
	cases := []struct {
		name       string
		servedType string
		want       string
	}{
		{"report.pdf", "application/pdf", "report.pdf"},
		{`..\..\secret\report.pdf`, "application/pdf", "report.pdf"},
		{"a/b/c.html", "application/octet-stream", "c.html"},
		{"x\u202Egnp.html", "application/octet-stream", "xgnp.html"},
		{"zero\u200Bwidth\uFEFF.txt", "text/plain", "zerowidth.txt"},
		{"tab\tand\x00nul.txt", "text/plain", "tabandnul.txt"},
		{"bad\xffutf8.txt", "text/plain", "badutf8.txt"},
		{" trailing dots... ", "text/plain", "trailing dots"},
		{"", "application/pdf", "media.pdf"},
		{"", "application/octet-stream", "media"},
		{"..", "image/tiff", "media"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, mediaDownloadFilename(tc.name, tc.servedType), "%q", tc.name)
	}
}

// The frontend re-types chat media blobs with the same inline lists, so the
// server must not serve inline anything the frontend would not show inline,
// nor the other way round.
func TestInlineChatMediaTypesMatchFrontend(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "frontend", "src", "lib", "chatMedia.ts"))
	require.NoError(t, err)
	quoted := regexp.MustCompile(`'([^']+)'`)
	var frontend []string
	for _, name := range []string{"INLINE_IMAGE_TYPES", "INLINE_VIDEO_TYPES", "INLINE_AUDIO_TYPES"} {
		set := regexp.MustCompile(`(?s)\b` + name + `\b[^=]*=\s*new Set\(\[(.*?)\]\)`).FindSubmatch(source)
		require.NotNil(t, set, "%s not found in chatMedia.ts", name)
		matches := quoted.FindAllSubmatch(set[1], -1)
		require.NotEmpty(t, matches, "%s is empty", name)
		for _, match := range matches {
			frontend = append(frontend, string(match[1]))
		}
	}
	backend := make([]string, 0, len(inlineChatMediaTypes))
	for mediaType := range inlineChatMediaTypes {
		backend = append(backend, mediaType)
	}
	slices.Sort(frontend)
	slices.Sort(backend)
	assert.Equal(t, frontend, backend)
}
