package handlers

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/metareview"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestMetaMessengerReviewWebhookPreauthorizationValidatesReservedRouteWithoutStorage(t *testing.T) {
	fixture := newReviewHandlerFixture(t)
	body := []byte(`{"object":"page","entry":[]}`)
	credentialID := uuid.NewString()
	headers := validMetaMessengerReviewPreauthHeaders(
		t,
		fixture,
		credentialID,
		1,
		body,
	)

	tests := []struct {
		name      string
		accountID uuid.UUID
		body      []byte
		headers   func() http.Header
		wantBound bool
		wantErr   error
	}{
		{
			name:      "unreserved account bypasses review preauthorization",
			accountID: uuid.New(),
			body:      nil,
			headers:   func() http.Header { return nil },
			wantBound: false,
		},
		{
			name:      "empty body",
			accountID: fixture.accountID,
			body:      nil,
			headers:   func() http.Header { return headers.Clone() },
			wantBound: true,
			wantErr:   errMetaMessengerReviewPreauthBody,
		},
		{
			name:      "oversized body",
			accountID: fixture.accountID,
			body:      make([]byte, metareview.MaximumInboundBodyBytes+1),
			headers:   func() http.Header { return headers.Clone() },
			wantBound: true,
			wantErr:   errMetaMessengerReviewPreauthBody,
		},
		{
			name:      "missing generation",
			accountID: fixture.accountID,
			body:      body,
			headers: func() http.Header {
				value := headers.Clone()
				value.Del(metareview.GenerationHeader)
				return value
			},
			wantBound: true,
			wantErr:   errMetaMessengerReviewPreauthHeader,
		},
		{
			name:      "non-exact generation",
			accountID: fixture.accountID,
			body:      body,
			headers: func() http.Header {
				value := headers.Clone()
				value.Set(metareview.GenerationHeader, " "+fixture.tuple.Generation)
				return value
			},
			wantBound: true,
			wantErr:   errMetaMessengerReviewPreauthHeader,
		},
		{
			name:      "duplicate credential ID",
			accountID: fixture.accountID,
			body:      body,
			headers: func() http.Header {
				value := headers.Clone()
				value.Add(metareview.CredentialIDHeader, uuid.NewString())
				return value
			},
			wantBound: true,
			wantErr:   errMetaMessengerReviewPreauthHeader,
		},
		{
			name:      "noncanonical credential ID",
			accountID: fixture.accountID,
			body:      body,
			headers: func() http.Header {
				value := headers.Clone()
				value.Set(metareview.CredentialIDHeader, strings.ToUpper(credentialID))
				return value
			},
			wantBound: true,
			wantErr:   errMetaMessengerReviewPreauthHeader,
		},
		{
			name:      "noncanonical credential version",
			accountID: fixture.accountID,
			body:      body,
			headers: func() http.Header {
				value := headers.Clone()
				value.Set(metareview.CredentialVersionHeader, "01")
				return value
			},
			wantBound: true,
			wantErr:   errMetaMessengerReviewPreauthHeader,
		},
		{
			name:      "missing proof",
			accountID: fixture.accountID,
			body:      body,
			headers: func() http.Header {
				value := headers.Clone()
				value.Del(metareview.ReviewProofHeader)
				return value
			},
			wantBound: true,
			wantErr:   errMetaMessengerReviewPreauthProof,
		},
		{
			name:      "invalid proof",
			accountID: fixture.accountID,
			body:      body,
			headers: func() http.Header {
				value := headers.Clone()
				value.Set(metareview.ReviewProofHeader, "sha256="+strings.Repeat("0", 64))
				return value
			},
			wantBound: true,
			wantErr:   errMetaMessengerReviewPreauthProof,
		},
		{
			name:      "valid proof",
			accountID: fixture.accountID,
			body:      body,
			headers:   func() http.Header { return headers.Clone() },
			wantBound: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			bound, err := fixture.app.preauthorizeMetaMessengerReviewWebhook(
				testCase.accountID,
				testCase.headers(),
				testCase.body,
				time.Now().UTC(),
			)
			assert.Equal(t, testCase.wantBound, bound)
			if testCase.wantErr == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, testCase.wantErr)
			}
		})
	}
}

func TestMetaMessengerReviewWebhookPreauthorizationKeepsExpiredRouteReserved(t *testing.T) {
	fixture := newReviewHandlerFixture(t)
	fixture.app.Config.MetaMessengerReviewRelay.ExpiresAt = time.Now().UTC().
		Add(-time.Second).
		Format(time.RFC3339Nano)

	bound, err := fixture.app.preauthorizeMetaMessengerReviewWebhook(
		fixture.accountID,
		http.Header{},
		[]byte(`{"object":"page"}`),
		time.Now().UTC(),
	)
	require.True(t, bound)
	require.ErrorIs(t, err, errMetaMessengerReviewPreauthAuthority)
}

func TestMetaMessengerReviewWebhookPreauthorizationKeepsMisconfiguredRouteReserved(t *testing.T) {
	fixture := newReviewHandlerFixture(t)
	fixture.app.Config.App.Environment = "production"
	fixture.app.Config.MetaMessengerReviewRelay.Enabled = false
	fixture.app.Config.MetaMessengerReviewRelay.Mode = ""

	bound, err := fixture.app.preauthorizeMetaMessengerReviewWebhook(
		fixture.accountID,
		http.Header{},
		[]byte(`{"object":"page"}`),
		time.Now().UTC(),
	)
	require.True(t, bound)
	require.ErrorIs(t, err, errMetaMessengerReviewPreauthAuthority)
}

func TestRelayChannelWebhookRejectsReviewPreauthBeforeResolverAndDB(t *testing.T) {
	db := testutil.SetupTestDB(t)
	queryCounter := &reviewPreauthQueryCounter{}
	db = db.Session(&gorm.Session{Logger: queryCounter})
	fixture := newReviewHandlerFixture(t)
	fixture.app.DB = db
	body := []byte(`{"object":"page","entry":[]}`)
	credentialID := uuid.NewString()
	validHeaders := validMetaMessengerReviewPreauthHeaders(
		t,
		fixture,
		credentialID,
		1,
		body,
	)

	var uniformBody []byte
	for _, testCase := range []struct {
		name   string
		body   []byte
		mutate func(http.Header)
	}{
		{
			name: "empty body",
			body: nil,
			mutate: func(http.Header) {
			},
		},
		{
			name: "missing generation",
			body: body,
			mutate: func(headers http.Header) {
				headers.Del(metareview.GenerationHeader)
			},
		},
		{
			name: "missing proof",
			body: body,
			mutate: func(headers http.Header) {
				headers.Del(metareview.ReviewProofHeader)
			},
		},
		{
			name: "invalid proof",
			body: body,
			mutate: func(headers http.Header) {
				headers.Set(metareview.ReviewProofHeader, "sha256="+strings.Repeat("f", 64))
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			headers := validHeaders.Clone()
			testCase.mutate(headers)
			request := metaMessengerReviewPreauthRequest(t, fixture.accountID, testCase.body, headers)

			require.NoError(t, fixture.app.RelayChannelWebhook(request))
			require.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(request))
			require.Zero(t, queryCounter.traces.Load(), "preauthorization failure must not resolve a tenant or touch the database")
			if uniformBody == nil {
				uniformBody = append([]byte(nil), testutil.GetResponseBody(request)...)
			} else {
				require.Equal(t, uniformBody, testutil.GetResponseBody(request))
			}
		})
	}

	validRequest := metaMessengerReviewPreauthRequest(
		t,
		fixture.accountID,
		body,
		validHeaders,
	)
	require.NoError(t, fixture.app.RelayChannelWebhook(validRequest))
	require.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(validRequest))
	require.Positive(t, queryCounter.traces.Load(), "valid preauthorization must continue to tenant resolution")
}

func validMetaMessengerReviewPreauthHeaders(
	t *testing.T,
	fixture reviewHandlerFixture,
	credentialID string,
	credentialVersion int,
	body []byte,
) http.Header {
	t.Helper()
	proof, err := metareview.SignInboundProof(
		fixture.app.Config.MetaMessengerReviewRelay.ProviderProofSecret,
		fixture.tuple,
		credentialID,
		credentialVersion,
		body,
	)
	require.NoError(t, err)
	headers := make(http.Header)
	headers.Set(metareview.GenerationHeader, fixture.tuple.Generation)
	headers.Set(metareview.CredentialIDHeader, credentialID)
	headers.Set(metareview.CredentialVersionHeader, strconv.Itoa(credentialVersion))
	headers.Set(metareview.ReviewProofHeader, proof)
	return headers
}

func metaMessengerReviewPreauthRequest(
	t *testing.T,
	accountID uuid.UUID,
	body []byte,
	headers http.Header,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewRequest(t)
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	request.RequestCtx.Request.SetBody(body)
	for key, values := range headers {
		for _, value := range values {
			request.RequestCtx.Request.Header.Add(key, value)
		}
	}
	testutil.SetPathParam(request, "channel_account_id", accountID.String())
	return request
}

type reviewPreauthQueryCounter struct {
	traces atomic.Int32
}

func (counter *reviewPreauthQueryCounter) LogMode(logger.LogLevel) logger.Interface {
	return counter
}

func (*reviewPreauthQueryCounter) Info(context.Context, string, ...interface{})  {}
func (*reviewPreauthQueryCounter) Warn(context.Context, string, ...interface{})  {}
func (*reviewPreauthQueryCounter) Error(context.Context, string, ...interface{}) {}

func (counter *reviewPreauthQueryCounter) Trace(
	_ context.Context,
	_ time.Time,
	_ func() (string, int64),
	_ error,
) {
	counter.traces.Add(1)
}

var _ logger.Interface = (*reviewPreauthQueryCounter)(nil)
