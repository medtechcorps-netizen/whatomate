package handlers

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	reviewReplySessionA   = "reviewer-browser-session-a"
	reviewReplySessionB   = "reviewer-browser-session-b"
	reviewReplyCSRF       = "reviewer-double-submit-csrf"
	reviewReplyRecipient  = "61587345186563"
	reviewReplyInboundMID = "m_review_inbound_exact_01"
	reviewReplyText       = "Thanks for your message. How can Klinik Insan help?"
)

type reviewReplyFixture struct {
	reviewPagePostsDBFixture
	contact      models.Contact
	identity     models.ContactIdentity
	conversation models.InboxConversation
	inbound      models.Message
}

func newReviewReplyFixture(t *testing.T) reviewReplyFixture {
	t.Helper()
	pageFixture := newReviewPagePostsDBFixture(
		t,
		"",
	)
	reviewerRole := testutil.CreateTestRoleWithKeys(
		t,
		pageFixture.app.DB,
		pageFixture.orgID,
		"meta-reviewer-read-only",
		[]string{
			models.ResourceChannelAccounts + ":" + models.ActionRead,
			models.ResourceContacts + ":" + models.ActionRead,
			models.ResourceConversations + ":" + models.ActionRead,
		},
	)
	pageFixture.user = testutil.CreateTestUser(
		t,
		pageFixture.app.DB,
		pageFixture.orgID,
		testutil.WithRoleID(&reviewerRole.ID),
	)
	pageFixture.app.Config.MetaMessengerReviewRelay.ReviewerUserID = pageFixture.user.ID.String()
	pageFixture.app.Config.MetaMessengerReviewRelay.ReviewerRoleID = reviewerRole.ID.String()

	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	windowEnd := now.Add(channelapi.MetaCustomerServiceWindow)
	contact := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: pageFixture.orgID,
		PhoneNumber:    "+60123456789",
		ProfileName:    "Messenger review sender",
		Metadata:       models.JSONB{},
		LastInboundAt:  &now,
	}
	require.NoError(t, pageFixture.app.DB.Create(&contact).Error)
	identity := models.ContactIdentity{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   pageFixture.orgID,
		ContactID:        contact.ID,
		ChannelAccountID: pageFixture.accountID,
		Channel:          models.ChannelMessenger,
		ExternalID:       reviewReplyRecipient,
		DisplayName:      "Messenger reviewer",
		IsPrimary:        true,
		IsVerified:       true,
		FirstSeenAt:      &now,
		LastSeenAt:       &now,
		Metadata:         models.JSONB{},
	}
	require.NoError(t, pageFixture.app.DB.Create(&identity).Error)
	conversation := models.InboxConversation{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		OrganizationID:         pageFixture.orgID,
		ChannelAccountID:       pageFixture.accountID,
		ContactID:              contact.ID,
		ContactIdentityID:      &identity.ID,
		Channel:                models.ChannelMessenger,
		ExternalConversationID: reviewReplyRecipient,
		Status:                 models.InboxConversationStatusOpen,
		OpenedAt:               now,
		LastMessageAt:          &now,
		LastInboundAt:          &now,
		ServiceWindowEndsAt:    &windowEnd,
		Config:                 models.JSONB{},
		Metadata:               models.JSONB{},
	}
	require.NoError(t, pageFixture.app.DB.Create(&conversation).Error)
	participant := models.ConversationParticipant{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    pageFixture.orgID,
		ConversationID:    conversation.ID,
		ParticipantKey:    "external:" + reviewReplyRecipient,
		Role:              models.ConversationParticipantRoleCustomer,
		ContactIdentityID: &identity.ID,
		ExternalID:        reviewReplyRecipient,
		DisplayName:       "Messenger reviewer",
		JoinedAt:          now,
		Metadata:          models.JSONB{},
	}
	require.NoError(t, pageFixture.app.DB.Create(&participant).Error)
	inbound := models.Message{
		BaseModel: models.BaseModel{
			ID:        uuid.New(),
			CreatedAt: now,
			UpdatedAt: now,
		},
		OrganizationID:      pageFixture.orgID,
		WhatsAppAccount:     "Staging Messenger review",
		ContactID:           contact.ID,
		WhatsAppMessageID:   reviewReplyInboundMID,
		ConversationID:      reviewReplyRecipient,
		InboxConversationID: &conversation.ID,
		Direction:           models.DirectionIncoming,
		MessageType:         models.MessageTypeText,
		Content:             "Messenger App Review inbound",
		Status:              models.MessageStatusReceived,
		Metadata: models.JSONB{
			"channel":  models.ChannelMessenger,
			"provider": channelapi.RelayProvider,
		},
	}
	require.NoError(t, pageFixture.app.DB.Create(&inbound).Error)
	return reviewReplyFixture{
		reviewPagePostsDBFixture: pageFixture,
		contact:                  contact,
		identity:                 identity,
		conversation:             conversation,
		inbound:                  inbound,
	}
}

func reviewReplyEligibilityRequest(
	t *testing.T,
	fixture reviewReplyFixture,
	userID uuid.UUID,
	session string,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, fixture.orgID, userID)
	testutil.SetPathParam(request, "id", fixture.conversation.ID.String())
	request.RequestCtx.Request.Header.SetCookie("whm_access", session)
	return request
}

func reviewReplyEligibility(
	t *testing.T,
	fixture reviewReplyFixture,
	session string,
) metaMessengerReviewReplyEligibilityResponse {
	t.Helper()
	request := reviewReplyEligibilityRequest(t, fixture, fixture.user.ID, session)
	require.NoError(t, fixture.app.GetMetaMessengerReviewReply(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request), string(testutil.GetResponseBody(request)))
	var response metaMessengerReviewReplyEligibilityResponse
	testutil.ParseEnvelopeResponse(t, request, &response)
	return response
}

func reviewReplySendRequest(
	t *testing.T,
	fixture reviewReplyFixture,
	userID uuid.UUID,
	session, attestation, clientNonce, text string,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewJSONRequest(t, metaMessengerReviewReplyRequest{
		AttestationID:      attestation,
		IdempotencyKey:     clientNonce,
		Text:               text,
		ManualConfirmation: true,
	})
	testutil.SetAuthContext(request, fixture.orgID, userID)
	testutil.SetPathParam(request, "id", fixture.conversation.ID.String())
	request.RequestCtx.Request.Header.SetCookie("whm_access", session)
	request.RequestCtx.Request.Header.SetCookie("whm_csrf", reviewReplyCSRF)
	request.RequestCtx.Request.Header.Set("X-CSRF-Token", reviewReplyCSRF)
	return request
}

func TestMetaMessengerReviewReplySendsOneExactTextResponseWithoutLeakingSecrets(t *testing.T) {
	fixture := newReviewReplyFixture(t)
	var graphCalls atomic.Int32
	fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(writer http.ResponseWriter, request *http.Request) {
		graphCalls.Add(1)
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/v25.0/"+fixture.tuple.PageID+"/messages", request.URL.Path)
		assert.Equal(t, "Bearer "+reviewPagePostsPlaintextToken, request.Header.Get("Authorization"))
		assert.Equal(t, "application/json", request.Header.Get("Content-Type"))
		assert.Empty(t, request.URL.Query().Get("access_token"))
		var payload struct {
			Recipient     map[string]string `json:"recipient"`
			MessagingType string            `json:"messaging_type"`
			Message       map[string]string `json:"message"`
		}
		assert.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
		assert.Equal(t, "RESPONSE", payload.MessagingType)
		assert.Equal(t, reviewReplyRecipient, payload.Recipient["id"])
		assert.Equal(t, reviewReplyText, payload.Message["text"])
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"recipient_id":"` + reviewReplyRecipient + `","message_id":"mid.review.sent.1"}`))
	})

	eligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
	require.True(t, eligibility.Eligible)
	assert.Equal(t, fixture.tuple.PageID, eligibility.PageID)
	assert.Equal(t, "••••6563", eligibility.RecipientLabel)
	assert.NotContains(t, reviewReplyJSON(t, eligibility), reviewReplyRecipient)
	assert.True(t, eligibility.Constraints.TextOnly)
	assert.True(t, eligibility.Constraints.ManualConfirmationRequired)
	assert.True(t, eligibility.Constraints.AIDisabled)
	assert.True(t, eligibility.Constraints.MarkReadDisabled)
	attestationExpiry, err := time.Parse(time.RFC3339Nano, eligibility.ExpiresAt)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().UTC().Add(metaMessengerReviewReplyAttestationTTL), attestationExpiry, 3*time.Second)

	request := reviewReplySendRequest(
		t,
		fixture,
		fixture.user.ID,
		reviewReplySessionA,
		eligibility.AttestationID,
		uuid.NewString(),
		reviewReplyText,
	)
	require.NoError(t, fixture.app.SendMetaMessengerReviewReply(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request), string(testutil.GetResponseBody(request)))
	assert.EqualValues(t, 1, graphCalls.Load())
	responseBody := string(testutil.GetResponseBody(request))
	assert.NotContains(t, responseBody, reviewReplyRecipient)
	assert.NotContains(t, responseBody, reviewPagePostsPlaintextToken)
	assert.NotContains(t, responseBody, fixture.encryptedToken)
	var response metaMessengerReviewReplyResponse
	testutil.ParseEnvelopeResponse(t, request, &response)
	assert.Equal(t, "", response.Message.ConversationID)
	assert.Equal(t, models.MessageStatusSent, response.Message.Status)
	assert.Equal(t, "••••6563", response.Audit.RecipientLabel)
	assert.False(t, response.Idempotent)

	var outbox models.OutboxJob
	require.NoError(t, fixture.app.DB.Where(
		"organization_id = ? AND channel_account_id = ?",
		fixture.orgID,
		fixture.accountID,
	).First(&outbox).Error)
	assert.Equal(t, models.OutboxJobStatusSent, outbox.Status)
	assert.Equal(t, 1, outbox.MaxAttempts)
	assert.Equal(t, 1, outbox.AttemptCount)
	assert.Equal(t, models.ChannelPreferencePurposeService, outbox.Purpose)
	assert.NotContains(t, reviewReplyJSON(t, outbox.Payload), reviewReplyRecipient)
	assert.NotContains(t, reviewReplyJSON(t, outbox.Payload), reviewReplyText)
	assert.NotContains(t, reviewReplyJSON(t, outbox.ProviderState), reviewReplyRecipient)

	var audits []models.AuditLog
	require.NoError(t, fixture.app.DB.Where(
		"organization_id = ? AND resource_type = ?",
		fixture.orgID,
		"meta_messenger_review_reply",
	).Find(&audits).Error)
	require.NotEmpty(t, audits)
	for _, auditEntry := range audits {
		assert.NotContains(t, reviewReplyJSON(t, auditEntry.Changes), reviewReplyRecipient)
		assert.NotContains(t, reviewReplyJSON(t, auditEntry.Changes), reviewPagePostsPlaintextToken)
	}
}

func TestMetaMessengerReviewReplySettlementReloadsStayInsideTenantTransaction(t *testing.T) {
	fixture := newReviewReplyFixture(t)
	fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"recipient_id":"` + reviewReplyRecipient + `","message_id":"mid.review.tenant.reload"}`))
	})
	eligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
	require.True(t, eligibility.Eligible)

	callbackName := "test:meta-review-reply-tenant-reload-" + uuid.NewString()
	var unscopedSettlementReads atomic.Int32
	require.NoError(t, fixture.app.DB.Callback().Query().Before("gorm:query").Register(
		callbackName,
		func(db *gorm.DB) {
			if db == nil || db.Statement == nil ||
				(db.Statement.Table != "outbox_jobs" && db.Statement.Table != "messages") {
				return
			}
			if _, insideTransaction := db.Statement.ConnPool.(*sql.Tx); insideTransaction {
				return
			}
			unscopedSettlementReads.Add(1)
			_ = db.AddError(errors.New("test blocked settlement reload outside tenant transaction"))
		},
	))
	t.Cleanup(func() {
		require.NoError(t, fixture.app.DB.Callback().Query().Remove(callbackName))
	})

	request := reviewReplySendRequest(
		t,
		fixture,
		fixture.user.ID,
		reviewReplySessionA,
		eligibility.AttestationID,
		uuid.NewString(),
		reviewReplyText,
	)
	require.NoError(t, fixture.app.SendMetaMessengerReviewReply(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request), string(testutil.GetResponseBody(request)))
	assert.Zero(t, unscopedSettlementReads.Load())

	var response metaMessengerReviewReplyResponse
	testutil.ParseEnvelopeResponse(t, request, &response)
	assert.Equal(t, models.MessageStatusSent, response.Message.Status)
	assert.Equal(t, "mid.review.tenant.reload", response.Message.WhatsAppMessageID)
}

func TestMetaMessengerReviewReplyReturnsSentUnderRestrictedTenantRLS(t *testing.T) {
	fixture := newReviewReplyFixture(t)
	adminDB := fixture.app.DB
	runtimeRole := "review_reply_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, adminDB.Exec(
		"CREATE ROLE "+runtimeRole+" NOSUPERUSER NOBYPASSRLS NOLOGIN",
	).Error)
	t.Cleanup(func() {
		require.NoError(t, database.RemoveTenantRLS(adminDB))
		require.NoError(t, adminDB.Exec("DROP OWNED BY "+runtimeRole).Error)
		require.NoError(t, adminDB.Exec("DROP ROLE "+runtimeRole).Error)
	})
	require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))

	runtimeDB, err := gorm.Open(postgres.Open(os.Getenv("TEST_DATABASE_URL")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	runtimeSQLDB, err := runtimeDB.DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, runtimeSQLDB.Close())
	})
	fixture.app.Config.Database.RLSEnabled = true
	fixture.app.Config.Database.RuntimeRole = runtimeRole

	var graphCalls atomic.Int32
	fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		graphCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"recipient_id":"` + reviewReplyRecipient + `","message_id":"mid.review.rls.sent"}`))
	})
	require.NoError(t, runtimeDB.Connection(func(runtimeConn *gorm.DB) error {
		if err := runtimeConn.Exec("SET ROLE " + runtimeRole).Error; err != nil {
			return err
		}
		defer func() {
			_ = runtimeConn.Exec("RESET ROLE").Error
		}()
		fixture.app.DB = runtimeConn

		eligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
		require.True(t, eligibility.Eligible)
		request := reviewReplySendRequest(
			t,
			fixture,
			fixture.user.ID,
			reviewReplySessionA,
			eligibility.AttestationID,
			uuid.NewString(),
			reviewReplyText,
		)
		require.NoError(t, fixture.app.SendMetaMessengerReviewReply(request))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request), string(testutil.GetResponseBody(request)))
		assert.EqualValues(t, 1, graphCalls.Load())
		var response metaMessengerReviewReplyResponse
		testutil.ParseEnvelopeResponse(t, request, &response)
		assert.Equal(t, models.MessageStatusSent, response.Message.Status)
		assert.Equal(t, "mid.review.rls.sent", response.Message.WhatsAppMessageID)

		var unscopedVisible int64
		require.NoError(t, runtimeConn.Session(&gorm.Session{NewDB: true}).Model(&models.OutboxJob{}).Where(
			"id = ?",
			response.Audit.ID,
		).Count(&unscopedVisible).Error)
		assert.Zero(t, unscopedVisible)

		var settledOutbox models.OutboxJob
		var settledMessage models.Message
		require.NoError(t, database.WithTenant(runtimeConn, fixture.orgID, func(tx *gorm.DB) error {
			if err := tx.Session(&gorm.Session{NewDB: true}).Where(
				"id = ?",
				response.Audit.ID,
			).First(&settledOutbox).Error; err != nil {
				return err
			}
			return tx.Session(&gorm.Session{NewDB: true}).Where(
				"id = ?",
				response.Message.ID,
			).First(&settledMessage).Error
		}))
		assert.Equal(t, models.OutboxJobStatusSent, settledOutbox.Status)
		assert.Equal(t, models.MessageStatusSent, settledMessage.Status)
		assert.Equal(t, "mid.review.rls.sent", settledMessage.WhatsAppMessageID)

		var foreignVisible int64
		require.NoError(t, database.WithTenant(runtimeConn, uuid.New(), func(tx *gorm.DB) error {
			return tx.Session(&gorm.Session{NewDB: true}).Model(&models.OutboxJob{}).Where(
				"id = ?",
				response.Audit.ID,
			).Count(&foreignVisible).Error
		}))
		assert.Zero(t, foreignVisible)
		return nil
	}))
}

func TestMetaMessengerReviewReplyServerIdempotencyIgnoresNewBrowserNonces(t *testing.T) {
	fixture := newReviewReplyFixture(t)
	firstEligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
	secondEligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
	require.NotEqual(t, firstEligibility.AttestationID, secondEligibility.AttestationID)
	var graphCalls atomic.Int32
	fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		graphCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"recipient_id":"` + reviewReplyRecipient + `","message_id":"mid.review.idempotent"}`))
	})

	first := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionA, firstEligibility.AttestationID, uuid.NewString(), reviewReplyText)
	require.NoError(t, fixture.app.SendMetaMessengerReviewReply(first))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first), string(testutil.GetResponseBody(first)))

	second := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionA, secondEligibility.AttestationID, uuid.NewString(), reviewReplyText)
	require.NoError(t, fixture.app.SendMetaMessengerReviewReply(second))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(second), string(testutil.GetResponseBody(second)))
	var replay metaMessengerReviewReplyResponse
	testutil.ParseEnvelopeResponse(t, second, &replay)
	assert.True(t, replay.Idempotent)
	assert.EqualValues(t, 1, graphCalls.Load())

	collision := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionA, secondEligibility.AttestationID, uuid.NewString(), reviewReplyText+" Different")
	require.NoError(t, fixture.app.SendMetaMessengerReviewReply(collision))
	testutil.AssertErrorResponse(t, collision, fasthttp.StatusConflict, "Idempotency key was already used for a different review reply")
	assert.EqualValues(t, 1, graphCalls.Load())

	getAfterAttempt := reviewReplyEligibilityRequest(t, fixture, fixture.user.ID, reviewReplySessionA)
	require.NoError(t, fixture.app.GetMetaMessengerReviewReply(getAfterAttempt))
	var eligibility metaMessengerReviewReplyEligibilityResponse
	testutil.ParseEnvelopeResponse(t, getAfterAttempt, &eligibility)
	assert.False(t, eligibility.Eligible)
	assert.Equal(t, "already_sent", eligibility.ReasonCode)
}

func TestMetaMessengerReviewReplyAmbiguousProviderResultIsTerminalAcrossNewNonce(t *testing.T) {
	testCases := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "server error", statusCode: http.StatusInternalServerError, body: `{"error":{"message":"unknown"}}`},
		{name: "redirect", statusCode: http.StatusTemporaryRedirect, body: ""},
		{name: "timeout status", statusCode: http.StatusRequestTimeout, body: `{"error":{"message":"timeout"}}`},
		{name: "malformed success", statusCode: http.StatusOK, body: `{"recipient_id":"` + reviewReplyRecipient + `"}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newReviewReplyFixture(t)
			firstEligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
			secondEligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
			var graphCalls atomic.Int32
			fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(writer http.ResponseWriter, _ *http.Request) {
				graphCalls.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(testCase.statusCode)
				_, _ = writer.Write([]byte(testCase.body))
			})

			first := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionA, firstEligibility.AttestationID, uuid.NewString(), reviewReplyText)
			require.NoError(t, fixture.app.SendMetaMessengerReviewReply(first))
			testutil.AssertErrorResponse(t, first, fasthttp.StatusConflict, "The delivery outcome is ambiguous; do not retry with a new key")
			second := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionA, secondEligibility.AttestationID, uuid.NewString(), reviewReplyText)
			require.NoError(t, fixture.app.SendMetaMessengerReviewReply(second))
			testutil.AssertErrorResponse(t, second, fasthttp.StatusConflict, "The earlier delivery outcome is ambiguous; do not retry with a new key")
			assert.EqualValues(t, 1, graphCalls.Load())
			var outbox models.OutboxJob
			require.NoError(t, fixture.app.DB.Where("organization_id = ?", fixture.orgID).First(&outbox).Error)
			assert.Equal(t, models.OutboxJobStatusFailed, outbox.Status)
			assert.Equal(t, "delivery_outcome_ambiguous", outbox.LastErrorCode)
			assert.Equal(t, 1, outbox.AttemptCount)
		})
	}
}

func TestMetaMessengerReviewReplyTransportFailureIsAmbiguousAndNeverReissued(t *testing.T) {
	fixture := newReviewReplyFixture(t)
	firstEligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
	secondEligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
	transport := &countingErrorRoundTripper{err: errors.New("simulated connection reset after request write")}
	fixture.app.HTTPClient = &http.Client{Transport: transport}
	first := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionA, firstEligibility.AttestationID, uuid.NewString(), reviewReplyText)
	require.NoError(t, fixture.app.SendMetaMessengerReviewReply(first))
	testutil.AssertErrorResponse(t, first, fasthttp.StatusConflict, "The delivery outcome is ambiguous; do not retry with a new key")
	second := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionA, secondEligibility.AttestationID, uuid.NewString(), reviewReplyText)
	require.NoError(t, fixture.app.SendMetaMessengerReviewReply(second))
	testutil.AssertErrorResponse(t, second, fasthttp.StatusConflict, "The earlier delivery outcome is ambiguous; do not retry with a new key")
	assert.EqualValues(t, 1, transport.calls.Load())
}

func TestValidMetaMessengerReviewProviderMessageIDMatchesStorageBoundary(t *testing.T) {
	assert.True(t, validMetaMessengerReviewProviderMessageID(
		strings.Repeat("m", metaMessengerReviewProviderMIDMaxBytes),
	))
	assert.False(t, validMetaMessengerReviewProviderMessageID(
		strings.Repeat("m", metaMessengerReviewProviderMIDMaxBytes+1),
	))
}

func TestMetaMessengerReviewReplyAbsoluteDeadlineExpiresAfterRevalidationWithoutGraph(t *testing.T) {
	fixture := newReviewReplyFixture(t)
	eligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
	transport := &countingErrorRoundTripper{err: errors.New("graph transport must not be called")}
	fixture.app.HTTPClient = &http.Client{
		Transport: transport,
		Timeout:   2 * time.Second,
	}
	request := reviewReplySendRequest(
		t,
		fixture,
		fixture.user.ID,
		reviewReplySessionA,
		eligibility.AttestationID,
		uuid.NewString(),
		reviewReplyText,
	)
	deliveryStartedAt := time.Now().UTC()
	deliveryBaseDeadline := deliveryStartedAt.Add(
		metaMessengerReviewReplyDeliveryBudget(fixture.app.HTTPClient),
	)
	deliveryBaseCtx, deliveryBaseCancel := context.WithDeadline(
		context.Background(),
		deliveryBaseDeadline,
	)
	defer deliveryBaseCancel()
	attempt, authErr, err := fixture.app.beginMetaMessengerReviewReply(
		deliveryBaseCtx,
		request,
		fixture.orgID,
		fixture.user.ID,
		fixture.conversation.ID,
		metaMessengerReviewReplyRequest{
			AttestationID:      eligibility.AttestationID,
			IdempotencyKey:     uuid.NewString(),
			Text:               reviewReplyText,
			ManualConfirmation: true,
		},
	)
	require.NoError(t, authErr)
	require.NoError(t, err)
	deliveryDeadline, err := metaMessengerReviewReplyDeliveryDeadline(
		attempt,
		deliveryBaseDeadline,
	)
	require.NoError(t, err)
	assert.Equal(t, deliveryBaseDeadline, deliveryDeadline)
	deliveryCtx, deliveryCancel := context.WithDeadline(deliveryBaseCtx, deliveryDeadline)
	defer deliveryCancel()

	refreshed, err := fixture.app.revalidateMetaMessengerReviewReplyBeforeGraph(
		deliveryCtx,
		fixture.orgID,
		fixture.user.ID,
		attempt,
	)
	require.NoError(t, err)
	attempt.Binding = refreshed
	select {
	case <-deliveryCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("absolute review delivery deadline did not expire")
	}
	require.ErrorIs(t, deliveryCtx.Err(), context.DeadlineExceeded)

	result := fixture.app.sendMetaMessengerReviewGraphReply(
		deliveryCtx,
		attempt.Binding.Tuple.PageID,
		attempt.Binding.RecipientID,
		reviewReplyText,
		reviewPagePostsPlaintextToken,
	)
	assert.False(t, result.Sent)
	assert.False(t, result.Ambiguous)
	assert.Equal(t, "preflight_deadline_expired", result.ErrorCode)
	assert.Zero(t, transport.calls.Load())

	settled, err := fixture.app.settleMetaMessengerReviewReply(
		fixture.orgID,
		fixture.user.ID,
		attempt,
		result,
	)
	require.NoError(t, err)
	assert.Equal(t, models.OutboxJobStatusFailed, settled.Outbox.Status)
	assert.Equal(t, "preflight_deadline_expired", settled.Outbox.LastErrorCode)
	assert.NotNil(t, settled.Outbox.FailedAt)
}

func TestMetaMessengerReviewReplyRequiresExactReadOnlyReviewerCookieAndCSRF(t *testing.T) {
	fixture := newReviewReplyFixture(t)
	var graphCalls atomic.Int32
	fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		graphCalls.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	})

	valid := reviewReplyEligibility(t, fixture, reviewReplySessionA)
	require.True(t, valid.Eligible)
	testCases := []struct {
		name   string
		mutate func(*fastglue.Request)
	}{
		{
			name: "missing session cookie",
			mutate: func(request *fastglue.Request) {
				request.RequestCtx.Request.Header.DelCookie("whm_access")
			},
		},
		{
			name: "API key even with cookie",
			mutate: func(request *fastglue.Request) {
				request.RequestCtx.Request.Header.Set("X-API-Key", "must-not-authorize-review-send")
			},
		},
		{
			name: "Bearer even with cookie",
			mutate: func(request *fastglue.Request) {
				request.RequestCtx.Request.Header.Set("Authorization", "Bearer must-not-authorize-review-send")
			},
		},
		{
			name: "missing CSRF header",
			mutate: func(request *fastglue.Request) {
				request.RequestCtx.Request.Header.Del("X-CSRF-Token")
			},
		},
		{
			name: "mismatched CSRF",
			mutate: func(request *fastglue.Request) {
				request.RequestCtx.Request.Header.Set("X-CSRF-Token", "different")
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			request := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionA, valid.AttestationID, uuid.NewString(), reviewReplyText)
			testCase.mutate(request)
			require.NoError(t, fixture.app.SendMetaMessengerReviewReply(request))
			assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(request))
			assert.Zero(t, graphCalls.Load())
		})
	}

	wrongUser := testutil.CreateTestUser(
		t,
		fixture.app.DB,
		fixture.orgID,
		testutil.WithRoleID(fixture.user.RoleID),
	)
	wrongUserRequest := reviewReplyEligibilityRequest(t, fixture, wrongUser.ID, reviewReplySessionA)
	require.NoError(t, fixture.app.GetMetaMessengerReviewReply(wrongUserRequest))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(wrongUserRequest))

	wrongRole := testutil.CreateTestRoleWithKeys(
		t,
		fixture.app.DB,
		fixture.orgID,
		"wrong-review-role",
		[]string{models.ResourceConversations + ":" + models.ActionRead},
	)
	fixture.app.Config.MetaMessengerReviewRelay.ReviewerRoleID = wrongRole.ID.String()
	wrongRoleRequest := reviewReplyEligibilityRequest(t, fixture, fixture.user.ID, reviewReplySessionA)
	require.NoError(t, fixture.app.GetMetaMessengerReviewReply(wrongRoleRequest))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(wrongRoleRequest))
	fixture.app.Config.MetaMessengerReviewRelay.ReviewerRoleID = fixture.user.RoleID.String()

	var extraReadPermission models.Permission
	require.NoError(t, fixture.app.DB.Where(
		"resource = ? AND action = ?",
		models.ResourceAuditLogs,
		models.ActionRead,
	).First(&extraReadPermission).Error)
	var pinnedRole models.CustomRole
	require.NoError(t, fixture.app.DB.First(&pinnedRole, "id = ?", *fixture.user.RoleID).Error)
	require.NoError(t, fixture.app.DB.Model(&pinnedRole).Association("Permissions").Append(&extraReadPermission))
	extraReadRequest := reviewReplyEligibilityRequest(t, fixture, fixture.user.ID, reviewReplySessionA)
	require.NoError(t, fixture.app.GetMetaMessengerReviewReply(extraReadRequest))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(extraReadRequest))
	require.NoError(t, fixture.app.DB.Model(&pinnedRole).Association("Permissions").Delete(&extraReadPermission))

	var writePermission models.Permission
	require.NoError(t, fixture.app.DB.Where(
		"resource = ? AND action = ?",
		models.ResourceConversations,
		models.ActionWrite,
	).First(&writePermission).Error)
	require.NoError(t, fixture.app.DB.Model(&pinnedRole).Association("Permissions").Append(&writePermission))
	driftedRoleRequest := reviewReplyEligibilityRequest(t, fixture, fixture.user.ID, reviewReplySessionA)
	require.NoError(t, fixture.app.GetMetaMessengerReviewReply(driftedRoleRequest))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(driftedRoleRequest))
	assert.Zero(t, graphCalls.Load())
}

func TestMetaMessengerReviewReplyRevalidatesEveryPinnedFactAndPolicyBeforeGraph(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*testing.T, reviewReplyFixture)
	}{
		{
			name: "account status",
			mutate: func(t *testing.T, fixture reviewReplyFixture) {
				require.NoError(t, fixture.app.DB.Model(&models.ChannelAccount{}).Where("id = ?", fixture.accountID).Update("status", models.ChannelAccountStatusDisconnected).Error)
			},
		},
		{
			name: "outbound flag enabled",
			mutate: func(t *testing.T, fixture reviewReplyFixture) {
				var account models.ChannelAccount
				require.NoError(t, fixture.app.DB.First(&account, "id = ?", fixture.accountID).Error)
				account.Config = cloneJSONB(account.Config)
				account.Config["outbound_enabled"] = true
				require.NoError(t, fixture.app.DB.Model(&account).Update("config", account.Config).Error)
			},
		},
		{
			name: "AI flag enabled",
			mutate: func(t *testing.T, fixture reviewReplyFixture) {
				var account models.ChannelAccount
				require.NoError(t, fixture.app.DB.First(&account, "id = ?", fixture.accountID).Error)
				account.Config = cloneJSONB(account.Config)
				account.Config["ai_reply_enabled"] = true
				require.NoError(t, fixture.app.DB.Model(&account).Update("config", account.Config).Error)
			},
		},
		{
			name: "default outgoing enabled",
			mutate: func(t *testing.T, fixture reviewReplyFixture) {
				require.NoError(t, fixture.app.DB.Model(&models.ChannelAccount{}).Where("id = ?", fixture.accountID).Update("is_default_outgoing", true).Error)
			},
		},
		{
			name: "generation changed",
			mutate: func(t *testing.T, fixture reviewReplyFixture) {
				var account models.ChannelAccount
				require.NoError(t, fixture.app.DB.First(&account, "id = ?", fixture.accountID).Error)
				account.Metadata = cloneJSONB(account.Metadata)
				account.Metadata["review_generation"] = uuid.NewString()
				require.NoError(t, fixture.app.DB.Model(&account).Update("metadata", account.Metadata).Error)
			},
		},
		{
			name: "OAuth revoked",
			mutate: func(t *testing.T, fixture reviewReplyFixture) {
				require.NoError(t, fixture.app.DB.Model(&models.ChannelCredential{}).Where("id = ?", fixture.oauthCredential.ID).Update("status", models.ChannelCredentialStatusRevoked).Error)
			},
		},
		{
			name: "production runtime",
			mutate: func(_ *testing.T, fixture reviewReplyFixture) {
				fixture.app.Config.App.Environment = "production"
			},
		},
		{
			name: "expired grant",
			mutate: func(_ *testing.T, fixture reviewReplyFixture) {
				fixture.app.Config.MetaMessengerReviewRelay.ExpiresAt = time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
			},
		},
		{
			name: "stale service window",
			mutate: func(t *testing.T, fixture reviewReplyFixture) {
				stale := time.Now().UTC().Add(-time.Hour)
				require.NoError(t, fixture.app.DB.Model(&models.InboxConversation{}).Where("id = ?", fixture.conversation.ID).Updates(map[string]any{
					"service_window_ends_at": stale,
				}).Error)
			},
		},
		{
			name: "recipient changed",
			mutate: func(t *testing.T, fixture reviewReplyFixture) {
				foreign := "61587345189999"
				require.NoError(t, fixture.app.DB.Model(&models.ContactIdentity{}).Where("id = ?", fixture.identity.ID).Update("external_id", foreign).Error)
				require.NoError(t, fixture.app.DB.Model(&models.InboxConversation{}).Where("id = ?", fixture.conversation.ID).Update("external_conversation_id", foreign).Error)
				require.NoError(t, fixture.app.DB.Model(&models.ConversationParticipant{}).Where("conversation_id = ?", fixture.conversation.ID).Update("external_id", foreign).Error)
			},
		},
		{
			name: "second live customer participant",
			mutate: func(t *testing.T, fixture reviewReplyFixture) {
				foreign := "61587345187777"
				extra := models.ConversationParticipant{
					BaseModel:      models.BaseModel{ID: uuid.New()},
					OrganizationID: fixture.orgID,
					ConversationID: fixture.conversation.ID,
					ParticipantKey: "external:" + foreign,
					Role:           models.ConversationParticipantRoleCustomer,
					ExternalID:     foreign,
					JoinedAt:       time.Now().UTC(),
					Metadata:       models.JSONB{},
				}
				require.NoError(t, fixture.app.DB.Create(&extra).Error)
			},
		},
		{
			name: "entitlement removed",
			mutate: func(t *testing.T, fixture reviewReplyFixture) {
				require.NoError(t, fixture.app.DB.Unscoped().Where("organization_id = ?", fixture.orgID).Delete(&models.Subscription{}).Error)
			},
		},
		{
			name: "service consent opted out",
			mutate: func(t *testing.T, fixture reviewReplyFixture) {
				now := time.Now().UTC()
				preference := models.ContactChannelPreference{
					BaseModel:         models.BaseModel{ID: uuid.New()},
					OrganizationID:    fixture.orgID,
					ContactID:         fixture.contact.ID,
					ChannelAccountID:  fixture.accountID,
					ContactIdentityID: &fixture.identity.ID,
					Channel:           models.ChannelMessenger,
					Purpose:           models.ChannelPreferencePurposeService,
					Status:            models.ChannelPreferenceStatusOptedOut,
					Source:            "review-test",
					OptedOutAt:        &now,
					QuietHours:        models.JSONB{},
					Config:            models.JSONB{},
					Metadata:          models.JSONB{},
				}
				require.NoError(t, fixture.app.DB.Create(&preference).Error)
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newReviewReplyFixture(t)
			eligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
			require.True(t, eligibility.Eligible)
			var graphCalls atomic.Int32
			fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(writer http.ResponseWriter, _ *http.Request) {
				graphCalls.Add(1)
				writer.WriteHeader(http.StatusNoContent)
			})
			testCase.mutate(t, fixture)
			request := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionA, eligibility.AttestationID, uuid.NewString(), reviewReplyText)
			require.NoError(t, fixture.app.SendMetaMessengerReviewReply(request))
			assert.NotEqual(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request), string(testutil.GetResponseBody(request)))
			assert.Zero(t, graphCalls.Load())
		})
	}
}

func TestMetaMessengerReviewReplyAttestationIsShortLivedAndSessionBound(t *testing.T) {
	fixture := newReviewReplyFixture(t)
	now := time.Now().UTC()
	var binding metaMessengerReviewReplyBinding
	require.NoError(t, database.WithTenantReadCommitted(fixture.app.DB, fixture.orgID, func(tx *gorm.DB) error {
		loaded, reason, _, err := fixture.app.loadMetaMessengerReviewReplyBindingTx(
			tx,
			fixture.orgID,
			fixture.user.ID,
			fixture.conversation.ID,
			now,
			false,
		)
		if err != nil {
			return err
		}
		if reason != "eligible" {
			return errors.New("review fixture was unexpectedly ineligible")
		}
		binding = loaded
		return nil
	}))
	binding.SessionDigest = sha256.Sum256([]byte(reviewReplySessionA))
	attestation, expiresAt, err := fixture.app.newMetaMessengerReviewReplyAttestation(binding, now)
	require.NoError(t, err)
	assert.LessOrEqual(t, expiresAt.Sub(now), metaMessengerReviewReplyAttestationTTL)
	verifiedExpiry, err := fixture.app.verifyMetaMessengerReviewReplyAttestation(binding, attestation, now.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, expiresAt, verifiedExpiry)
	_, err = fixture.app.verifyMetaMessengerReviewReplyAttestation(binding, attestation, expiresAt.Add(time.Nanosecond))
	assert.ErrorIs(t, err, errMetaMessengerReviewReplyIneligible)
	binding.SessionDigest = sha256.Sum256([]byte(reviewReplySessionB))
	_, err = fixture.app.verifyMetaMessengerReviewReplyAttestation(binding, attestation, now.Add(time.Second))
	assert.ErrorIs(t, err, errMetaMessengerReviewReplyIneligible)

	endpointAttestation := reviewReplyEligibility(t, fixture, reviewReplySessionA)
	var graphCalls atomic.Int32
	fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		graphCalls.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	})
	wrongSession := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionB, endpointAttestation.AttestationID, uuid.NewString(), reviewReplyText)
	require.NoError(t, fixture.app.SendMetaMessengerReviewReply(wrongSession))
	assert.NotEqual(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(wrongSession))
	assert.Zero(t, graphCalls.Load())
}

func TestMetaMessengerReviewReplySendFenceWinsAndOldAgeStillDrainsDeprovision(t *testing.T) {
	fixture := newReviewReplyFixture(t)
	admin := integrationTestUser(
		t,
		fixture.app,
		fixture.orgID,
		models.ResourceChannelAccounts+":"+models.ActionDelete,
	)
	eligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
	graphStarted := make(chan struct{})
	releaseGraph := make(chan struct{})
	var startOnce sync.Once
	var messageCalls atomic.Int32
	var cleanupCalls atomic.Int32
	fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/messages"):
			messageCalls.Add(1)
			startOnce.Do(func() { close(graphStarted) })
			<-releaseGraph
			_, _ = writer.Write([]byte(`{"recipient_id":"` + reviewReplyRecipient + `","message_id":"mid.review.race.sent"}`))
		case request.Method == http.MethodDelete && strings.HasSuffix(request.URL.Path, "/subscribed_apps"):
			cleanupCalls.Add(1)
			_, _ = writer.Write([]byte(`{"success":true}`))
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/subscribed_apps"):
			cleanupCalls.Add(1)
			_, _ = writer.Write([]byte(`{"data":[]}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
			_, _ = writer.Write([]byte(`{"error":{"message":"unexpected test request"}}`))
		}
	})

	send := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionA, eligibility.AttestationID, uuid.NewString(), reviewReplyText)
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- fixture.app.SendMetaMessengerReviewReply(send)
	}()
	select {
	case <-graphStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("review Graph request did not start")
	}

	var fenced models.OutboxJob
	require.NoError(t, fixture.app.DB.Where(
		"organization_id = ? AND channel_account_id = ?",
		fixture.orgID,
		fixture.accountID,
	).First(&fenced).Error)
	require.Equal(t, models.OutboxJobStatusReviewDispatching, fenced.Status)
	oldFence := time.Now().UTC().Add(-10 * time.Minute)
	fenced.ProviderState = cloneJSONB(fenced.ProviderState)
	fenced.ProviderState["provider_deadline_at"] = oldFence.Format(time.RFC3339Nano)
	require.NoError(t, fixture.app.DB.Model(&models.OutboxJob{}).
		Where("id = ? AND organization_id = ?", fenced.ID, fixture.orgID).
		Updates(map[string]any{
			"locked_at":      oldFence,
			"provider_state": fenced.ProviderState,
		}).Error)

	firstDeprovision := newReviewLifecycleDeleteRequest(t, fixture.reviewHandlerFixture, admin.ID)
	require.NoError(t, fixture.app.DeprovisionMetaMessengerReviewAccount(firstDeprovision))
	testutil.AssertErrorResponse(
		t,
		firstDeprovision,
		fasthttp.StatusServiceUnavailable,
		"An earlier Messenger review reply remains permanently fenced; audited operator reconciliation is required before deprovisioning",
	)
	var unchangedAccount models.ChannelAccount
	require.NoError(t, fixture.app.DB.First(&unchangedAccount, "id = ?", fixture.accountID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, unchangedAccount.Status)
	assert.True(t, boolConfigValue(unchangedAccount.Metadata, "review_ready"))
	var unchangedCredential models.ChannelCredential
	require.NoError(t, fixture.app.DB.First(&unchangedCredential, "id = ?", fixture.oauthCredential.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, unchangedCredential.Status)
	assert.NotEmpty(t, unchangedCredential.CredentialBlob)
	assert.Zero(t, cleanupCalls.Load())

	close(releaseGraph)
	require.NoError(t, <-sendDone)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(send), string(testutil.GetResponseBody(send)))
	assert.EqualValues(t, 1, messageCalls.Load())
	require.NoError(t, fixture.app.DB.First(&fenced, "id = ?", fenced.ID).Error)
	assert.Equal(t, models.OutboxJobStatusSent, fenced.Status)

	secondDeprovision := newReviewLifecycleDeleteRequest(t, fixture.reviewHandlerFixture, admin.ID)
	require.NoError(t, fixture.app.DeprovisionMetaMessengerReviewAccount(secondDeprovision))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(secondDeprovision), string(testutil.GetResponseBody(secondDeprovision)))
	assert.GreaterOrEqual(t, cleanupCalls.Load(), int32(2))
	var deletedAccount models.ChannelAccount
	require.NoError(t, fixture.app.DB.Unscoped().First(&deletedAccount, "id = ?", fixture.accountID).Error)
	assert.True(t, deletedAccount.DeletedAt.Valid)
	require.NoError(t, fixture.app.DB.First(&unchangedCredential, "id = ?", fixture.oauthCredential.ID).Error)
	assert.Empty(t, unchangedCredential.CredentialBlob)
}

func TestMetaMessengerReviewReplyDeprovisionFenceWinsBeforeSendAndGraphIsNeverCalled(t *testing.T) {
	fixture := newReviewReplyFixture(t)
	admin := integrationTestUser(
		t,
		fixture.app,
		fixture.orgID,
		models.ResourceChannelAccounts+":"+models.ActionDelete,
	)
	eligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
	var messageCalls atomic.Int32
	fixture.app.HTTPClient = reviewPagePostsGraphClient(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/messages") {
			messageCalls.Add(1)
			_, _ = writer.Write([]byte(`{"recipient_id":"` + reviewReplyRecipient + `","message_id":"must-not-send"}`))
			return
		}
		if request.Method == http.MethodDelete {
			_, _ = writer.Write([]byte(`{"success":true}`))
			return
		}
		if request.Method == http.MethodGet {
			_, _ = writer.Write([]byte(`{"data":[]}`))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	})

	deprovision := newReviewLifecycleDeleteRequest(t, fixture.reviewHandlerFixture, admin.ID)
	require.NoError(t, fixture.app.DeprovisionMetaMessengerReviewAccount(deprovision))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(deprovision), string(testutil.GetResponseBody(deprovision)))

	send := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionA, eligibility.AttestationID, uuid.NewString(), reviewReplyText)
	require.NoError(t, fixture.app.SendMetaMessengerReviewReply(send))
	assert.NotEqual(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(send), string(testutil.GetResponseBody(send)))
	assert.Zero(t, messageCalls.Load())
}

func TestMetaMessengerReviewReplyPersistedOldFenceNeverAutoClears(t *testing.T) {
	fixture := newReviewReplyFixture(t)
	admin := integrationTestUser(
		t,
		fixture.app,
		fixture.orgID,
		models.ResourceChannelAccounts+":"+models.ActionDelete,
	)
	eligibility := reviewReplyEligibility(t, fixture, reviewReplySessionA)
	request := reviewReplySendRequest(t, fixture, fixture.user.ID, reviewReplySessionA, eligibility.AttestationID, uuid.NewString(), reviewReplyText)
	attempt, authErr, err := fixture.app.beginMetaMessengerReviewReply(
		context.Background(),
		request,
		fixture.orgID,
		fixture.user.ID,
		fixture.conversation.ID,
		metaMessengerReviewReplyRequest{
			AttestationID:      eligibility.AttestationID,
			IdempotencyKey:     uuid.NewString(),
			Text:               reviewReplyText,
			ManualConfirmation: true,
		},
	)
	require.NoError(t, authErr)
	require.NoError(t, err)
	require.Equal(t, models.OutboxJobStatusReviewDispatching, attempt.Outbox.Status)

	oldFence := time.Now().UTC().Add(-24 * time.Hour)
	providerState := cloneJSONB(attempt.Outbox.ProviderState)
	providerState["provider_deadline_at"] = oldFence.Format(time.RFC3339Nano)
	require.NoError(t, fixture.app.DB.Model(&models.OutboxJob{}).
		Where("id = ? AND organization_id = ?", attempt.Outbox.ID, fixture.orgID).
		Updates(map[string]any{
			"created_at":     oldFence,
			"locked_at":      oldFence,
			"provider_state": providerState,
		}).Error)
	var protected bool
	require.NoError(t, database.WithTenantReadCommitted(fixture.app.DB, fixture.orgID, func(tx *gorm.DB) error {
		var checkErr error
		protected, checkErr = hasProtectedMetaMessengerReviewDispatchTx(
			tx,
			fixture.orgID,
			fixture.accountID,
			time.Now().UTC(),
		)
		return checkErr
	}))
	assert.True(t, protected)
	require.NoError(t, fixture.app.DB.First(&attempt.Outbox, "id = ?", attempt.Outbox.ID).Error)
	assert.Equal(t, models.OutboxJobStatusReviewDispatching, attempt.Outbox.Status)
	assert.Nil(t, attempt.Outbox.FailedAt)

	deprovision := newReviewLifecycleDeleteRequest(t, fixture.reviewHandlerFixture, admin.ID)
	require.NoError(t, fixture.app.DeprovisionMetaMessengerReviewAccount(deprovision))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(deprovision))
	require.NoError(t, fixture.app.DB.First(&attempt.Outbox, "id = ?", attempt.Outbox.ID).Error)
	assert.Equal(t, models.OutboxJobStatusReviewDispatching, attempt.Outbox.Status)
	var credential models.ChannelCredential
	require.NoError(t, fixture.app.DB.First(&credential, "id = ?", fixture.oauthCredential.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, credential.Status)
	assert.NotEmpty(t, credential.CredentialBlob)
}

type countingErrorRoundTripper struct {
	calls atomic.Int32
	err   error
}

func (transport *countingErrorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return nil, transport.err
}

func reviewReplyJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}
