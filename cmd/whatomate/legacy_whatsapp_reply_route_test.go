package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestLegacyWhatsAppReplyRouteUsesExplicitHeaderWithoutHoldingTenantConnection
// exercises the registered HTTP route through the restricted production role.
// One pool connection is deliberately shared by two concurrent sends. If the
// route is ever wrapped in App.Tenant again, its outer transaction retains that
// connection and the handler's first committed phase cannot start.
func TestLegacyWhatsAppReplyRouteUsesExplicitHeaderWithoutHoldingTenantConnection(t *testing.T) {
	adminDB := testutil.SetupTestDB(t)
	testutil.TruncateTables(adminDB)
	runtimeRole := "rereply_legacy_route_" + uuid.NewString()[:8]
	runtimePassword := "synthetic" + uuid.NewString()[:8]
	require.NoError(t, adminDB.Exec(fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS",
		runtimeRole,
		runtimePassword,
	)).Error)

	rlsApplied := false
	var runtimeSQL interface{ Close() error }
	t.Cleanup(func() {
		if runtimeSQL != nil {
			_ = runtimeSQL.Close()
		}
		if rlsApplied {
			_ = database.RemoveTenantRLS(adminDB)
		}
		_ = adminDB.Exec("DROP OWNED BY " + runtimeRole).Error
		_ = adminDB.Exec("DROP ROLE IF EXISTS " + runtimeRole).Error
		testutil.TruncateTables(adminDB)
	})

	reseller := testutil.CreateTestReseller(t, adminDB)
	homeOrganization := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	targetOrganization := testutil.CreateTestOrganizationForReseller(t, adminDB, reseller.ID)
	homeRole := testutil.CreateAdminRole(t, adminDB, homeOrganization.ID)
	targetRole := testutil.CreateAdminRole(t, adminDB, targetOrganization.ID)
	user := testutil.CreateTestUser(
		t,
		adminDB,
		homeOrganization.ID,
		testutil.WithRoleID(&homeRole.ID),
	)
	require.NoError(t, adminDB.Create(&models.UserOrganization{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		UserID:         user.ID,
		OrganizationID: targetOrganization.ID,
		RoleID:         &targetRole.ID,
	}).Error)
	enableLegacyWhatsAppReplyRouteEntitlement(t, adminDB, targetOrganization.ID, user.ID)

	account := testutil.CreateTestWhatsAppAccount(t, adminDB, targetOrganization.ID)
	conversationIDs := []uuid.UUID{
		createLegacyWhatsAppReplyRouteConversation(t, adminDB, targetOrganization.ID, account),
		createLegacyWhatsAppReplyRouteConversation(t, adminDB, targetOrganization.ID, account),
	}

	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+account.AccessToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		call := providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": fmt.Sprintf("wamid.route.%d", call)}},
		})
	}))
	t.Cleanup(provider.Close)

	require.NoError(t, database.ApplyTenantRLS(adminDB, runtimeRole))
	rlsApplied = true
	runtimeDB := openLegacyWhatsAppReplyRouteRuntimeDB(t, runtimeRole, runtimePassword)
	sqlDB, err := runtimeDB.DB()
	require.NoError(t, err)
	runtimeSQL = sqlDB
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	require.NoError(t, database.VerifyTenantRLS(runtimeDB, runtimeRole))

	cfg := &config.Config{
		Database: config.DatabaseConfig{RLSEnabled: true, RuntimeRole: runtimeRole},
		JWT:      config.JWTConfig{Secret: testutil.TestJWTSecret},
		LegacyWhatsAppReply: config.LegacyWhatsAppReplyConfig{
			Enabled:                true,
			AllowedOrganizationIDs: targetOrganization.ID.String(),
		},
	}
	log := testutil.NopLogger()
	app := &handlers.App{
		Config:   cfg,
		DB:       runtimeDB,
		Log:      log,
		WhatsApp: whatsapp.NewWithBaseURL(log, provider.URL),
	}
	glue := fastglue.NewGlue()
	setupRoutes(glue, app, log, "", nil, cfg)
	token := testutil.GenerateTestRefreshToken(t, user, testutil.TestJWTSecret, time.Hour)
	missingHeader := &fasthttp.RequestCtx{}
	missingHeader.Request.Header.SetMethod(fasthttp.MethodPost)
	missingHeader.Request.SetRequestURI(
		"/api/conversations/" + conversationIDs[0].String() + "/legacy-whatsapp-replies",
	)
	missingHeader.Request.Header.Set("Authorization", "Bearer "+token)
	missingHeader.Request.Header.SetContentType("application/json")
	missingHeader.Request.SetBodyString(`{
		"idempotency_key":"missing-explicit-organization",
		"type":"text",
		"content":{"body":"must not reach Meta"}
	}`)
	glue.Handler()(missingHeader)
	require.Equal(
		t,
		fasthttp.StatusBadRequest,
		missingHeader.Response.StatusCode(),
		string(missingHeader.Response.Body()),
	)
	assert.Zero(t, providerCalls.Load())

	type routeResult struct {
		status int
		body   string
	}
	results := make(chan routeResult, len(conversationIDs))
	for index, conversationID := range conversationIDs {
		index := index
		conversationID := conversationID
		go func() {
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod(fasthttp.MethodPost)
			ctx.Request.SetRequestURI(
				"/api/conversations/" + conversationID.String() + "/legacy-whatsapp-replies",
			)
			ctx.Request.Header.Set("Authorization", "Bearer "+token)
			ctx.Request.Header.Set("X-Organization-ID", targetOrganization.ID.String())
			ctx.Request.Header.SetContentType("application/json")
			body, marshalErr := json.Marshal(map[string]any{
				"idempotency_key": fmt.Sprintf("route-pool-%d-%s", index, uuid.NewString()),
				"type":            models.MessageTypeText,
				"content":         map[string]string{"body": fmt.Sprintf("pool-safe reply %d", index)},
			})
			if marshalErr != nil {
				results <- routeResult{status: 0, body: marshalErr.Error()}
				return
			}
			ctx.Request.SetBody(body)
			glue.Handler()(ctx)
			results <- routeResult{status: ctx.Response.StatusCode(), body: string(ctx.Response.Body())}
		}()
	}

	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for range conversationIDs {
		select {
		case result := <-results:
			require.Equal(t, fasthttp.StatusOK, result.status, result.body)
		case <-deadline.C:
			_ = sqlDB.Close()
			t.Fatal("registered legacy reply route exhausted its one-connection RLS pool")
		}
	}
	assert.Equal(t, int32(len(conversationIDs)), providerCalls.Load())

	// Force the second canonical-contact lock (the provider phase) to fail
	// after the first lock has committed the durable pending claim. The runtime
	// role cannot update that row without a fresh tenant transaction.
	failureConversationID := createLegacyWhatsAppReplyRouteConversation(
		t,
		adminDB,
		targetOrganization.ID,
		account,
	)
	forcedProviderQueryErr := errors.New("forced provider-phase contact query failure")
	var lockedContactQueries atomic.Int32
	callbackName := "test:legacy_reply_provider_query_failure_" + uuid.NewString()
	require.NoError(t, runtimeDB.Callback().Query().Before("gorm:query").Register(
		callbackName,
		func(tx *gorm.DB) {
			if tx.Statement == nil || tx.Statement.Schema == nil ||
				tx.Statement.Schema.Table != "contacts" {
				return
			}
			if _, locked := tx.Statement.Clauses["FOR"]; !locked {
				return
			}
			// The pending phase locks the canonical Contact, then the strict
			// omnichannel mirror locks its legacy Contact. The provider phase's
			// canonical Contact lock is therefore the third matching query.
			if lockedContactQueries.Add(1) == 3 {
				tx.AddError(forcedProviderQueryErr)
			}
		},
	))
	t.Cleanup(func() { _ = runtimeDB.Callback().Query().Remove(callbackName) })

	hub := appwebsocket.NewHub(log)
	go hub.Run()
	app.WSHub = hub
	client := appwebsocket.NewClient(hub, nil, uuid.New(), targetOrganization.ID)
	hub.Register(client)
	testutil.AssertEventually(
		t,
		func() bool { return hub.GetClientCount() == 1 },
		2*time.Second,
		"strict failure settlement client registered",
	)

	failureKey := "route-provider-phase-failure-" + uuid.NewString()
	failureBody := "Settle the known no-provider failure"
	invokeFailureRoute := func() *fasthttp.RequestCtx {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(fasthttp.MethodPost)
		ctx.Request.SetRequestURI(
			"/api/conversations/" + failureConversationID.String() + "/legacy-whatsapp-replies",
		)
		ctx.Request.Header.Set("Authorization", "Bearer "+token)
		ctx.Request.Header.Set("X-Organization-ID", targetOrganization.ID.String())
		ctx.Request.Header.SetContentType("application/json")
		body, marshalErr := json.Marshal(map[string]any{
			"idempotency_key": failureKey,
			"type":            models.MessageTypeText,
			"content":         map[string]string{"body": failureBody},
		})
		require.NoError(t, marshalErr)
		ctx.Request.SetBody(body)
		glue.Handler()(ctx)
		return ctx
	}

	providerCallsBeforeFailure := providerCalls.Load()
	failedRequest := invokeFailureRoute()
	require.Equal(
		t,
		fasthttp.StatusBadGateway,
		failedRequest.Response.StatusCode(),
		string(failedRequest.Response.Body()),
	)
	assert.Equal(t, int32(3), lockedContactQueries.Load())
	assert.Equal(t, providerCallsBeforeFailure, providerCalls.Load(), "Meta must not be called")

	select {
	case data := <-client.SendChan():
		var envelope struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		require.NoError(t, json.Unmarshal(data, &envelope))
		assert.Equal(t, appwebsocket.TypeStatusUpdate, envelope.Type)
		assert.Equal(t, string(models.MessageStatusFailed), envelope.Payload["status"])
	case <-time.After(2 * time.Second):
		t.Fatal("committed pre-provider failure emitted no realtime status")
	}

	var failedCanonical models.Message
	require.NoError(t, adminDB.Where(
		"organization_id = ? AND inbox_conversation_id = ? AND metadata ->> 'idempotency_key' = ?",
		targetOrganization.ID,
		failureConversationID,
		failureKey,
	).First(&failedCanonical).Error)
	assert.Equal(t, models.MessageStatusFailed, failedCanonical.Status)
	assert.ErrorContains(t, errors.New(failedCanonical.ErrorMessage), forcedProviderQueryErr.Error())

	retry := invokeFailureRoute()
	require.Equal(t, fasthttp.StatusOK, retry.Response.StatusCode(), string(retry.Response.Body()))
	var retryEnvelope struct {
		Data struct {
			ID     uuid.UUID            `json:"id"`
			Status models.MessageStatus `json:"status"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(retry.Response.Body(), &retryEnvelope))
	assert.Equal(t, failedCanonical.ID, retryEnvelope.Data.ID)
	assert.Equal(t, models.MessageStatusFailed, retryEnvelope.Data.Status)
	assert.Equal(t, providerCallsBeforeFailure, providerCalls.Load(), "retry must not resend to Meta")
	assert.Equal(t, int32(4), lockedContactQueries.Load())
	select {
	case data := <-client.SendChan():
		var envelope struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		require.NoError(t, json.Unmarshal(data, &envelope))
		assert.Equal(t, appwebsocket.TypeNewMessage, envelope.Type)
		assert.Equal(t, string(models.MessageStatusFailed), envelope.Payload["status"])
		assert.Equal(t, failedCanonical.ID.String(), envelope.Payload["id"])
	case <-time.After(2 * time.Second):
		t.Fatal("failed idempotent replay emitted no canonical new-message envelope")
	}

	var targetMessages int64
	require.NoError(t, adminDB.Model(&models.Message{}).Where(
		"organization_id = ? AND direction = ? AND metadata ->> 'send_surface' = ?",
		targetOrganization.ID,
		models.DirectionOutgoing,
		"omnichannel_legacy_whatsapp",
	).Count(&targetMessages).Error)
	assert.EqualValues(t, len(conversationIDs)+1, targetMessages)
	var homeMessages int64
	require.NoError(t, adminDB.Model(&models.Message{}).Where(
		"organization_id = ? AND metadata ->> 'send_surface' = ?",
		homeOrganization.ID,
		"omnichannel_legacy_whatsapp",
	).Count(&homeMessages).Error)
	assert.Zero(t, homeMessages, "X-Organization-ID must never fall back to the token workspace")
}

func openLegacyWhatsAppReplyRouteRuntimeDB(
	t *testing.T,
	role, password string,
) *gorm.DB {
	t.Helper()
	parsed, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	require.Contains(
		t,
		[]string{"postgres", "postgresql"},
		parsed.Scheme,
		"TEST_DATABASE_URL must be a PostgreSQL URL",
	)
	parsed.User = url.UserPassword(role, password)
	query := parsed.Query()
	query.Set("statement_cache_capacity", "0")
	query.Set("default_query_exec_mode", "describe_exec")
	parsed.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	return db
}

func createLegacyWhatsAppReplyRouteConversation(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	account *models.WhatsAppAccount,
) uuid.UUID {
	t.Helper()
	contact := testutil.CreateTestContactWith(
		t,
		db,
		organizationID,
		testutil.WithContactAccount(account.Name),
	)
	inbound := models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    organizationID,
		WhatsAppAccount:   account.Name,
		ContactID:         contact.ID,
		Direction:         models.DirectionIncoming,
		MessageType:       models.MessageTypeText,
		Content:           "Customer opened the service window",
		Status:            models.MessageStatusReceived,
		WhatsAppMessageID: "wamid.route.inbound." + uuid.NewString(),
		Metadata:          models.JSONB{},
	}
	require.NoError(t, db.Create(&inbound).Error)
	bridge, err := channelapi.MirrorLegacyWhatsAppMessage(db, channelapi.LegacyMetaAccountRef{
		ID:             account.ID,
		OrganizationID: organizationID,
		Name:           account.Name,
		Status:         account.Status,
	}, inbound.ID)
	require.NoError(t, err)
	return bridge.ConversationID
}

func enableLegacyWhatsAppReplyRouteEntitlement(
	t *testing.T,
	db *gorm.DB,
	organizationID, userID uuid.UUID,
) {
	t.Helper()
	plan := models.Plan{
		BaseModel:   models.BaseModel{ID: uuid.New()},
		ScopeKey:    "legacy-route-" + uuid.NewString(),
		Code:        "legacy-route-" + uuid.NewString(),
		Name:        "Legacy route test",
		Status:      models.CommercialPlanStatusActive,
		Vertical:    "general",
		Metadata:    models.JSONB{},
		CreatedByID: &userID,
	}
	require.NoError(t, db.Create(&plan).Error)
	billing := models.BillingAccount{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organizationID,
		Provider:        models.BillingProviderManual,
		Status:          models.BillingAccountStatusActive,
		DefaultCurrency: "MYR",
		BillingProfile:  models.JSONB{},
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, db.Create(&billing).Error)
	periodStart := time.Now().UTC().Add(-time.Hour)
	periodEnd := periodStart.AddDate(0, 1, 0)
	subscription := models.Subscription{
		BaseModel:            models.BaseModel{ID: uuid.New()},
		OrganizationID:       organizationID,
		BillingAccountID:     billing.ID,
		PlanID:               plan.ID,
		Provider:             models.BillingProviderManual,
		Status:               models.SubscriptionStatusActive,
		Quantity:             1,
		CollectionMethod:     "send_invoice",
		EntitlementsSnapshot: models.JSONB{channelapi.OmnichannelEntitlementKey: true},
		ProviderData:         models.JSONB{},
		CurrentPeriodStart:   &periodStart,
		CurrentPeriodEnd:     &periodEnd,
		CreatedByID:          &userID,
	}
	require.NoError(t, db.Create(&subscription).Error)
}
