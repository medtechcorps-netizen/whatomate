package frontend

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedFrontendServesRouteSpecificRawLegalFallbacks(t *testing.T) {
	t.Parallel()

	handler, err := newHTTPHandler(testFrontendFS(), "")
	require.NoError(t, err)

	testCases := []struct {
		path       string
		key        string
		title      string
		effective  string
		contains   []string
		notContain string
	}{
		{
			path:      "/privacy",
			key:       "privacy",
			title:     "Privacy Policy",
			effective: "Effective 3 August 2026",
			contains: []string{
				"Medtech Healthcare",
				"The Customer Organisation is the data controller",
				"ReReply does not sell personal data",
				"Retention, deletion and your rights",
				"medtechcorps@gmail.com",
			},
			notContain: "These Terms are governed by the laws of Malaysia",
		},
		{
			path:      "/terms",
			key:       "terms",
			title:     "Terms of Service",
			effective: "Effective 28 July 2026",
			contains: []string{
				"Medtech Healthcare",
				"Messaging and channel rules",
				"These Terms are governed by the laws of Malaysia",
				"Customers retain ownership of data",
				"medtechcorps@gmail.com",
			},
			notContain: "acknowledge requests within seven calendar days",
		},
		{
			path:      "/data-deletion",
			key:       "data-deletion",
			title:     "Data Deletion Instructions",
			effective: "Effective 3 August 2026",
			contains: []string{
				"ReReply Data Deletion Request",
				"acknowledge requests within seven calendar days",
				"encrypted or access-restricted backups",
				"medtechcorps@gmail.com",
			},
			notContain: "Search Console access is read-only",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.key, func(t *testing.T) {
			t.Parallel()

			for _, requestPath := range []string{testCase.path, testCase.path + "/"} {
				response := serveFrontendRequest(handler, requestPath, "")
				require.Equal(t, http.StatusOK, response.Code)
				assert.Contains(t, response.Header().Get("Content-Type"), "text/html")

				body := response.Body.String()
				assert.Contains(t, body, `data-rereply-raw-document="`+testCase.key+`"`)
				assert.Contains(t, body, `<h1 id="legal-fallback-title">`+testCase.title+`</h1>`)
				assert.Contains(t, body, `<title>`+testCase.title+` · ReReply</title>`)
				assert.Contains(t, body, testCase.effective)
				for _, marker := range testCase.contains {
					assert.Contains(t, body, marker)
				}
				assert.NotContains(t, body, "Medtech Softwarehouse")
				assert.NotContains(t, body, testCase.notContain)
				assert.Contains(t, body, `<script type="module"`)
			}
		})
	}
}

func TestEmbeddedFrontendPreservesGenericAndStaticServing(t *testing.T) {
	t.Parallel()

	handler, err := newHTTPHandler(testFrontendFS(), "")
	require.NoError(t, err)

	for _, requestPath := range []string{"/", "/about", "/privacy-notice"} {
		response := serveFrontendRequest(handler, requestPath, "")
		require.Equal(t, http.StatusOK, response.Code)
		body := response.Body.String()
		assert.Contains(t, body, `<h1 id="fallback-title">ReReply</h1>`)
		assert.NotContains(t, body, "data-rereply-raw-document")
	}

	assetResponse := serveFrontendRequest(handler, "/assets/app.js", "")
	require.Equal(t, http.StatusOK, assetResponse.Code)
	assert.Equal(t, "application/javascript", assetResponse.Header().Get("Content-Type"))
	assert.Equal(t, "console.info('uncompressed');", assetResponse.Body.String())

	gzipResponse := serveFrontendRequest(handler, "/assets/app.js", "gzip")
	require.Equal(t, http.StatusOK, gzipResponse.Code)
	assert.Equal(t, "gzip", gzipResponse.Header().Get("Content-Encoding"))
	assert.Equal(t, "synthetic-gzip-body", gzipResponse.Body.String())

	apiResponse := serveFrontendRequest(handler, "/api/missing", "")
	assert.Equal(t, http.StatusNotFound, apiResponse.Code)
	assert.NotContains(t, apiResponse.Body.String(), "data-rereply-raw-document")
}

func TestEmbeddedFrontendLegalFallbackLinksRespectBasePath(t *testing.T) {
	t.Parallel()

	rootHandler, err := newHTTPHandler(testFrontendFS(), "")
	require.NoError(t, err)
	consoleHandler, err := newHTTPHandler(testFrontendFS(), "/console/")
	require.NoError(t, err)

	rootBody := serveFrontendRequest(rootHandler, "/privacy", "").Body.String()
	assert.Contains(t, rootBody, `<base href="/">`)
	assert.Contains(t, rootBody, `href="/privacy#scope"`)
	assert.NotContains(t, rootBody, `href="#scope"`)
	assert.NotContains(t, rootBody, `/console/privacy#scope`)

	for _, requestPath := range []string{"/privacy", "/console/privacy", "/console/privacy/"} {
		response := serveFrontendRequest(consoleHandler, requestPath, "")
		require.Equal(t, http.StatusOK, response.Code)
		body := response.Body.String()
		assert.Contains(t, body, `<base href="/console/">`)
		assert.Contains(t, body, `window.__BASE_PATH__ = "/console";`)
		assert.Contains(t, body, `href="/console/privacy#scope"`)
		assert.Contains(t, body, `href="/console/data-deletion"`)
		assert.NotContains(t, body, `href="#scope"`)
	}

	// Creating another handler must not mutate a previously prepared body.
	rootBodyAfter := serveFrontendRequest(rootHandler, "/privacy", "").Body.String()
	assert.Equal(t, rootBody, rootBodyAfter)
}

func TestEmbeddedFrontendRejectsIndexWithoutFallbackMarkers(t *testing.T) {
	t.Parallel()

	_, err := newHTTPHandler(fstest.MapFS{
		"index.html": {Data: []byte(`<!doctype html><html><head></head><body><div id="app"></div></body></html>`)},
	}, "")
	require.ErrorContains(t, err, appFallbackStartMarker)
}

func testFrontendFS() fstest.MapFS {
	index := fmt.Sprintf(`<!doctype html>
<html lang="en">
  <head>
    %s
    <meta name="description" content="generic description">
    %s
    %s
    <title>ReReply</title>
    %s
  </head>
  <body>
    <div id="app">
      %s
      <main class="app-fallback"><h1 id="fallback-title">ReReply</h1><p>generic fallback</p></main>
      %s
    </div>
    <script type="module" src="./assets/app.js"></script>
  </body>
</html>`,
		descriptionStartMarker,
		descriptionEndMarker,
		titleStartMarker,
		titleEndMarker,
		appFallbackStartMarker,
		appFallbackEndMarker,
	)
	return fstest.MapFS{
		"index.html":       {Data: []byte(index)},
		"assets/app.js":    {Data: []byte("console.info('uncompressed');")},
		"assets/app.js.gz": {Data: []byte("synthetic-gzip-body")},
	}
}

func serveFrontendRequest(handler http.Handler, requestPath, acceptEncoding string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "https://app.example.test"+requestPath, nil)
	if acceptEncoding != "" {
		request.Header.Set("Accept-Encoding", acceptEncoding)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
