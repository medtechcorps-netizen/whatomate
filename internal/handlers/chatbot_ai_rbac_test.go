package handlers_test

import (
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestAIContextGranularRBAC(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	aiContext := createTestAIContext(t, app, org.ID, "RBAC context")
	reader := createAIContextUser(t, app, org.ID, models.ActionRead)

	listReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(listReq, org.ID, reader.ID)
	require.NoError(t, app.ListAIContexts(listReq))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(listReq))

	getReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(getReq, org.ID, reader.ID)
	testutil.SetPathParam(getReq, "id", aiContext.ID.String())
	require.NoError(t, app.GetAIContext(getReq))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(getReq))

	createReq := testutil.NewJSONRequest(t, map[string]any{
		"name":         "Denied create",
		"context_type": "static",
	})
	testutil.SetAuthContext(createReq, org.ID, reader.ID)
	require.NoError(t, app.CreateAIContext(createReq))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(createReq))

	updateReq := testutil.NewJSONRequest(t, map[string]any{"name": "Denied update"})
	testutil.SetAuthContext(updateReq, org.ID, reader.ID)
	testutil.SetPathParam(updateReq, "id", aiContext.ID.String())
	require.NoError(t, app.UpdateAIContext(updateReq))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(updateReq))

	deleteReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(deleteReq, org.ID, reader.ID)
	testutil.SetPathParam(deleteReq, "id", aiContext.ID.String())
	require.NoError(t, app.DeleteAIContext(deleteReq))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(deleteReq))
}
