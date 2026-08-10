package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

const reviewPagePostsPlaintextToken = "review-page-posts-plaintext-token-do-not-return"

type reviewPagePostsDBFixture struct {
	reviewHandlerFixture
	user            *models.User
	oauthCredential models.ChannelCredential
	encryptedToken  string
}

func TestMetaMessengerReviewPagePostsRequiresAuthenticationBeforeStorageOrGraph(t *testing.T) {
	fixture := newReviewHandlerFixture(t)
	var graphCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		graphCalls.Add(1)
	}))
	t.Cleanup(server.Close)
	fixture.app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		"https://graph.facebook.com": server,
	})

	request := testutil.NewGETRequest(t)
	testutil.SetPathParam(request, "id", fixture.accountID.String())
	require.NoError(t, fixture.app.GetMetaMessengerReviewPagePosts(request))
	testutil.AssertErrorResponse(t, request, fasthttp.StatusUnauthorized, "Unauthorized")
	assert.Zero(t, graphCalls.Load())
}

func TestMetaMessengerReviewPagePostsRequiresChannelAccountRead(t *testing.T) {
	fixture := newReviewPagePostsDBFixture(t, "")
	var graphCalls atomic.Int32
	fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		graphCalls.Add(1)
		response.WriteHeader(http.StatusNoContent)
	})

	request := reviewPagePostsRequest(t, fixture, fixture.accountID)
	require.NoError(t, fixture.app.GetMetaMessengerReviewPagePosts(request))
	testutil.AssertErrorResponse(
		t,
		request,
		fasthttp.StatusForbidden,
		"Insufficient permissions",
	)
	assert.Zero(t, graphCalls.Load())
}

func TestMetaMessengerReviewPagePostsUsesExactGraphRequestAndDoesNotLeakToken(t *testing.T) {
	fixture := newReviewPagePostsDBFixture(
		t,
		models.ResourceChannelAccounts+":"+models.ActionRead,
	)
	var graphCalls atomic.Int32
	fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		graphCalls.Add(1)
		assert.Equal(
			t,
			"/v25.0/"+fixture.tuple.PageID+"/posts",
			request.URL.Path,
		)
		assert.Equal(
			t,
			"id,message,created_time,permalink_url",
			request.URL.Query().Get("fields"),
		)
		assert.Equal(t, "3", request.URL.Query().Get("limit"))
		assert.Empty(t, request.URL.Query().Get("access_token"))
		assert.Equal(
			t,
			"Bearer "+reviewPagePostsPlaintextToken,
			request.Header.Get("Authorization"),
		)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"data":[{"id":"page_post_1","message":"Welcome to Klinik Insan","created_time":"2026-08-11T09:30:00+0000","permalink_url":"https://www.facebook.com/klinikinsan/posts/1"}]}`))
	})

	request := reviewPagePostsRequest(t, fixture, fixture.accountID)
	require.NoError(t, fixture.app.GetMetaMessengerReviewPagePosts(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	assert.EqualValues(t, 1, graphCalls.Load())

	body := string(request.RequestCtx.Response.Body())
	assert.NotContains(t, body, reviewPagePostsPlaintextToken)
	assert.NotContains(t, body, fixture.encryptedToken)
	var envelope struct {
		Data metaMessengerReviewPagePostsResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(request.RequestCtx.Response.Body(), &envelope))
	assert.Equal(t, fixture.tuple.PageID, envelope.Data.PageID)
	assert.Equal(t, "Staging Messenger review", envelope.Data.PageName)
	require.Len(t, envelope.Data.Posts, 1)
	assert.Equal(t, "Welcome to Klinik Insan", envelope.Data.Posts[0].Message)
	assert.False(t, envelope.Data.FetchedAt.IsZero())
}

func TestMetaMessengerReviewPagePostsRejectsNonExactBindingsWithoutGraph(t *testing.T) {
	testCases := []struct {
		name       string
		accountID  func(reviewPagePostsDBFixture) uuid.UUID
		mutate     func(*testing.T, reviewPagePostsDBFixture)
		production bool
	}{
		{
			name:      "foreign account",
			accountID: func(reviewPagePostsDBFixture) uuid.UUID { return uuid.New() },
		},
		{
			name: "review not ready",
			mutate: func(t *testing.T, fixture reviewPagePostsDBFixture) {
				require.NoError(t, fixture.app.DB.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ?", fixture.accountID, fixture.orgID).
					Update("metadata", models.JSONB{
						"management_mode":            metaMessengerManagementMode,
						"review_relay_mode":          "staging_messenger_review_v1",
						"review_generation":          fixture.tuple.Generation,
						"review_expires_at":          fixture.tuple.ExpiresAt,
						"meta_business_id":           fixture.tuple.MetaBusinessID,
						"meta_app_id":                fixture.tuple.MetaAppID,
						"meta_app_owner_business_id": reviewHandlerTestOwnerBusinessID,
						"review_ready":               false,
						"subscription_verified":      true,
					}).Error)
			},
		},
		{
			name: "generation mismatch",
			mutate: func(t *testing.T, fixture reviewPagePostsDBFixture) {
				require.NoError(t, fixture.app.DB.Exec(
					"UPDATE channel_accounts SET metadata = jsonb_set(metadata, '{review_generation}', to_jsonb(?::text)) WHERE id = ? AND organization_id = ?",
					uuid.NewString(),
					fixture.accountID,
					fixture.orgID,
				).Error)
			},
		},
		{
			name: "credential app mismatch",
			mutate: func(t *testing.T, fixture reviewPagePostsDBFixture) {
				require.NoError(t, fixture.app.DB.Exec(
					"UPDATE channel_credentials SET metadata = jsonb_set(metadata, '{app_id}', to_jsonb(?::text)) WHERE id = ? AND organization_id = ?",
					"999999999999999",
					fixture.oauthCredential.ID,
					fixture.orgID,
				).Error)
			},
		},
		{
			name: "credential business mismatch",
			mutate: func(t *testing.T, fixture reviewPagePostsDBFixture) {
				require.NoError(t, fixture.app.DB.Exec(
					"UPDATE channel_credentials SET metadata = jsonb_set(metadata, '{meta_business_id}', to_jsonb(?::text)) WHERE id = ? AND organization_id = ?",
					"999999999999998",
					fixture.oauthCredential.ID,
					fixture.orgID,
				).Error)
			},
		},
		{
			name: "credential Page mismatch",
			mutate: func(t *testing.T, fixture reviewPagePostsDBFixture) {
				require.NoError(t, fixture.app.DB.Exec(
					"UPDATE channel_credentials SET metadata = jsonb_set(metadata, '{page_id}', to_jsonb(?::text)) WHERE id = ? AND organization_id = ?",
					"999999999999997",
					fixture.oauthCredential.ID,
					fixture.orgID,
				).Error)
			},
		},
		{name: "production", production: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newReviewPagePostsDBFixture(
				t,
				models.ResourceChannelAccounts+":"+models.ActionRead,
			)
			if testCase.production {
				fixture.app.Config.App.Environment = "production"
			}
			if testCase.mutate != nil {
				testCase.mutate(t, fixture)
			}
			var graphCalls atomic.Int32
			fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(
				response http.ResponseWriter,
				request *http.Request,
			) {
				graphCalls.Add(1)
				response.WriteHeader(http.StatusNoContent)
			})
			accountID := fixture.accountID
			if testCase.accountID != nil {
				accountID = testCase.accountID(fixture)
			}
			request := reviewPagePostsRequest(t, fixture, accountID)
			require.NoError(t, fixture.app.GetMetaMessengerReviewPagePosts(request))
			testutil.AssertErrorResponse(
				t,
				request,
				fasthttp.StatusNotFound,
				"Messenger review Page preview not found",
			)
			assert.Zero(t, graphCalls.Load())
		})
	}
}

func TestMetaMessengerReviewPagePostsSanitizesProviderAndMalformedResponses(t *testing.T) {
	testCases := []struct {
		name       string
		statusCode int
		body       string
		message    string
	}{
		{
			name:       "provider error",
			statusCode: http.StatusForbidden,
			body:       `{"error":{"message":"` + reviewPagePostsPlaintextToken + `","type":"OAuthException","code":190}}`,
			message:    "Meta could not load the review Page posts",
		},
		{
			name:       "invalid JSON",
			statusCode: http.StatusOK,
			body:       `{"data":`,
			message:    "Meta could not load the review Page posts",
		},
		{
			name:       "unsafe permalink",
			statusCode: http.StatusOK,
			body:       `{"data":[{"id":"page_post_1","message":"Hello","created_time":"2026-08-11T09:30:00Z","permalink_url":"javascript:alert(1)"}]}`,
			message:    "Meta returned an invalid review Page post response",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newReviewPagePostsDBFixture(
				t,
				models.ResourceChannelAccounts+":"+models.ActionRead,
			)
			fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(
				response http.ResponseWriter,
				request *http.Request,
			) {
				assert.Equal(
					t,
					"Bearer "+reviewPagePostsPlaintextToken,
					request.Header.Get("Authorization"),
				)
				response.Header().Set("Content-Type", "application/json")
				response.WriteHeader(testCase.statusCode)
				_, _ = response.Write([]byte(testCase.body))
			})

			request := reviewPagePostsRequest(t, fixture, fixture.accountID)
			require.NoError(t, fixture.app.GetMetaMessengerReviewPagePosts(request))
			testutil.AssertErrorResponse(
				t,
				request,
				fasthttp.StatusBadGateway,
				testCase.message,
			)
			body := string(request.RequestCtx.Response.Body())
			assert.NotContains(t, body, reviewPagePostsPlaintextToken)
			assert.NotContains(t, body, fixture.encryptedToken)
		})
	}
}

func TestMetaMessengerReviewPagePostsDiscardsProviderDataAfterConcurrentRevocation(t *testing.T) {
	fixture := newReviewPagePostsDBFixture(
		t,
		models.ResourceChannelAccounts+":"+models.ActionRead,
	)
	requestReceived := make(chan struct{})
	allowResponse := make(chan struct{})
	fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		close(requestReceived)
		<-allowResponse
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"data":[{"id":"page_post_revoked","message":"MUST NOT BE RETURNED","created_time":"2026-08-11T09:30:00Z","permalink_url":"https://www.facebook.com/klinikinsan/posts/revoked"}]}`))
	})

	request := reviewPagePostsRequest(t, fixture, fixture.accountID)
	handlerDone := make(chan error, 1)
	go func() {
		handlerDone <- fixture.app.GetMetaMessengerReviewPagePosts(request)
	}()
	<-requestReceived
	require.NoError(t, fixture.app.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.ChannelAccount{}).
			Where("id = ? AND organization_id = ?", fixture.accountID, fixture.orgID).
			Updates(map[string]any{
				"status": models.ChannelAccountStatusDisconnected,
				"metadata": gorm.Expr(
					"jsonb_set(jsonb_set(metadata, '{review_ready}', 'false'::jsonb), '{subscription_verified}', 'false'::jsonb)",
				),
			}).Error; err != nil {
			return err
		}
		return tx.Model(&models.ChannelCredential{}).
			Where("id = ? AND organization_id = ?", fixture.oauthCredential.ID, fixture.orgID).
			Update("status", models.ChannelCredentialStatusRevoked).Error
	}))
	close(allowResponse)
	require.NoError(t, <-handlerDone)
	testutil.AssertErrorResponse(
		t,
		request,
		fasthttp.StatusNotFound,
		"Messenger review Page preview not found",
	)
	assert.NotContains(t, string(request.RequestCtx.Response.Body()), "MUST NOT BE RETURNED")
}

func newReviewPagePostsDBFixture(
	t *testing.T,
	permission string,
) reviewPagePostsDBFixture {
	t.Helper()
	fixture := newReviewHandlerFixture(t)
	fixture.app.DB = testutil.SetupTestDB(t)
	organization := &models.Organization{
		BaseModel: models.BaseModel{ID: fixture.orgID},
		Name:      "Review Page preview organization",
		Slug:      "review-page-preview-" + uuid.NewString(),
	}
	require.NoError(t, fixture.app.DB.Create(organization).Error)
	permissions := []string{}
	if strings.TrimSpace(permission) != "" {
		permissions = append(permissions, permission)
	}
	user := integrationTestUser(t, fixture.app, fixture.orgID, permissions...)
	enableBookingCommerceTestEntitlement(
		t,
		fixture.app.DB,
		fixture.orgID,
		user.ID,
		"omnichannel.enabled",
	)
	account := fixture.readyAccount(t)
	account.CreatedByID = &user.ID
	account.UpdatedByID = &user.ID
	require.NoError(t, fixture.app.DB.Create(&account).Error)

	encryptedToken, err := appcrypto.Encrypt(
		reviewPagePostsPlaintextToken,
		reviewHandlerTestEncryptionKey,
	)
	require.NoError(t, err)
	oauth := models.ChannelCredential{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   fixture.orgID,
		ChannelAccountID: fixture.accountID,
		Kind:             models.ChannelCredentialKindOAuth,
		Version:          1,
		CredentialBlob:   models.JSONB{"access_token": encryptedToken},
		Status:           models.ChannelCredentialStatusActive,
		KeyVersion:       "app:v1",
		Metadata: models.JSONB{
			"app_id":           fixture.tuple.MetaAppID,
			"page_id":          fixture.tuple.PageID,
			"meta_business_id": fixture.tuple.MetaBusinessID,
			"token_type":       "page",
		},
	}
	require.NoError(t, fixture.app.DB.Create(&oauth).Error)
	return reviewPagePostsDBFixture{
		reviewHandlerFixture: fixture,
		user:                 user,
		oauthCredential:      oauth,
		encryptedToken:       encryptedToken,
	}
}

func reviewPagePostsRequest(
	t *testing.T,
	fixture reviewPagePostsDBFixture,
	accountID uuid.UUID,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, fixture.orgID, fixture.user.ID)
	testutil.SetPathParam(request, "id", accountID.String())
	return request
}

func reviewPagePostsGraphClient(
	t *testing.T,
	handler http.HandlerFunc,
) *http.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		"https://graph.facebook.com": server,
	})
}

func TestNormalizeMetaMessengerReviewPagePostsAcceptsMetaTimestampFormats(t *testing.T) {
	for _, timestamp := range []string{
		"2026-08-11T09:30:00Z",
		"2026-08-11T09:30:00+0000",
	} {
		posts, err := normalizeMetaMessengerReviewPagePosts([]metaMessengerReviewPagePost{
			{
				ID:           "page_post_1",
				CreatedTime:  timestamp,
				PermalinkURL: "https://www.facebook.com/klinikinsan/posts/1",
			},
		})
		require.NoError(t, err)
		require.Len(t, posts, 1)
		assert.Equal(t, timestamp, posts[0].CreatedTime)
	}
}
