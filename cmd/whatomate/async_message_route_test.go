package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
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
	"gorm.io/gorm"
)

// TestAsyncMessageRoutesUseCurrentRLSRequestBeforeProvider exercises the real
// Tenant-wrapped agent and phone-only template routes with one runtime pool
// connection. Pending state must commit before Meta is reached, and the
// pending new_message must be queued before the terminal status event.
func TestAsyncMessageRoutesUseCurrentRLSRequestBeforeProvider(t *testing.T) {
	adminDB := testutil.SetupTestDB(t)
	testutil.TruncateTables(adminDB)
	runtimeRole := "rereply_async_route_" + uuid.NewString()[:8]
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
	account := testutil.CreateTestWhatsAppAccount(t, adminDB, targetOrganization.ID)
	contact := testutil.CreateTestContactWith(
		t,
		adminDB,
		targetOrganization.ID,
		testutil.WithContactAccount(account.Name),
	)
	template := testutil.CreateTestTemplate(t, adminDB, targetOrganization.ID, account.Name)

	var providerCalls atomic.Int32
	var providerSawCommittedPending atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := providerCalls.Add(1)
		var pendingCount int64
		if err := adminDB.Model(&models.Message{}).Where(
			"organization_id = ? AND direction = ? AND status = ? AND BTRIM(whats_app_message_id) = ''",
			targetOrganization.ID,
			models.DirectionOutgoing,
			models.MessageStatusPending,
		).Count(&pendingCount).Error; err == nil && pendingCount > 0 {
			providerSawCommittedPending.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": fmt.Sprintf("wamid.async.route.%d", call)}},
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
	}
	log := testutil.NopLogger()
	hub := appwebsocket.NewHub(log)
	go hub.Run()
	app := &handlers.App{
		Config:   cfg,
		DB:       runtimeDB,
		Log:      log,
		WhatsApp: whatsapp.NewWithBaseURL(log, provider.URL),
		WSHub:    hub,
	}
	client := appwebsocket.NewClient(hub, nil, user.ID, targetOrganization.ID)
	hub.Register(client)
	testutil.AssertEventually(
		t,
		func() bool { return hub.GetClientCount() == 1 },
		2*time.Second,
		"async route realtime client registered",
	)
	glue := fastglue.NewGlue()
	setupRoutes(glue, app, log, "", nil, cfg)
	token := testutil.GenerateTestRefreshToken(t, user, testutil.TestJWTSecret, time.Hour)

	type routeResult struct {
		status int
		body   []byte
	}
	invoke := func(path string, body map[string]any) routeResult {
		t.Helper()
		result := make(chan routeResult, 1)
		go func() {
			request := &fasthttp.RequestCtx{}
			request.Request.Header.SetMethod(fasthttp.MethodPost)
			request.Request.SetRequestURI(path)
			request.Request.Header.Set("Authorization", "Bearer "+token)
			request.Request.Header.Set("X-Organization-ID", targetOrganization.ID.String())
			request.Request.Header.SetContentType("application/json")
			payload, marshalErr := json.Marshal(body)
			if marshalErr != nil {
				result <- routeResult{body: []byte(marshalErr.Error())}
				return
			}
			request.Request.SetBody(payload)
			glue.Handler()(request)
			result <- routeResult{
				status: request.Response.StatusCode(),
				body:   append([]byte(nil), request.Response.Body()...),
			}
		}()
		select {
		case response := <-result:
			return response
		case <-time.After(10 * time.Second):
			t.Fatal("Tenant-wrapped async route exhausted its one-connection RLS pool")
			return routeResult{}
		}
	}

	type realtimeEnvelope struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	readRealtime := func(reason string) realtimeEnvelope {
		t.Helper()
		select {
		case data := <-client.SendChan():
			var envelope realtimeEnvelope
			require.NoError(t, json.Unmarshal(data, &envelope))
			return envelope
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %s realtime event", reason)
			return realtimeEnvelope{}
		}
	}
	waitForDelivery := func() {
		t.Helper()
		done := make(chan struct{})
		go func() {
			app.WaitForBackgroundTasks()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("asynchronous provider delivery did not finish with one RLS connection")
		}
	}

	agentBody := "single-connection agent send " + uuid.NewString()
	agentResult := invoke(
		"/api/contacts/"+contact.ID.String()+"/messages",
		map[string]any{
			"type":    models.MessageTypeText,
			"content": map[string]string{"body": agentBody},
		},
	)
	require.Equal(t, fasthttp.StatusOK, agentResult.status, string(agentResult.body))
	var agentResponse struct {
		Data handlers.MessageResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(agentResult.body, &agentResponse))
	require.Equal(t, models.MessageStatusPending, agentResponse.Data.Status)
	firstAgentEvent := readRealtime("agent pending new_message")
	require.Equal(t, appwebsocket.TypeNewMessage, firstAgentEvent.Type)
	assert.Equal(t, agentResponse.Data.ID.String(), firstAgentEvent.Payload["id"])
	assert.Equal(t, string(models.MessageStatusPending), firstAgentEvent.Payload["status"])
	waitForDelivery()
	secondAgentEvent := readRealtime("agent terminal status")
	require.Equal(t, appwebsocket.TypeStatusUpdate, secondAgentEvent.Type)
	assert.Equal(t, agentResponse.Data.ID.String(), secondAgentEvent.Payload["message_id"])
	assert.Equal(t, string(models.MessageStatusSent), secondAgentEvent.Payload["status"])

	phone := "+6012" + time.Now().Format("150405")
	templateResult := invoke("/api/messages/template", map[string]any{
		"phone_number":    phone,
		"template_id":     template.ID.String(),
		"template_params": map[string]string{"1": "single-connection"},
	})
	require.Equal(t, fasthttp.StatusOK, templateResult.status, string(templateResult.body))
	var templateResponse struct {
		Data handlers.MessageResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(templateResult.body, &templateResponse))
	require.Equal(t, models.MessageStatusPending, templateResponse.Data.Status)
	firstTemplateEvent := readRealtime("template pending new_message")
	require.Equal(t, appwebsocket.TypeNewMessage, firstTemplateEvent.Type)
	assert.Equal(t, templateResponse.Data.ID.String(), firstTemplateEvent.Payload["id"])
	assert.Equal(t, string(models.MessageStatusPending), firstTemplateEvent.Payload["status"])
	waitForDelivery()
	secondTemplateEvent := readRealtime("template terminal status")
	require.Equal(t, appwebsocket.TypeStatusUpdate, secondTemplateEvent.Type)
	assert.Equal(t, templateResponse.Data.ID.String(), secondTemplateEvent.Payload["message_id"])
	assert.Equal(t, string(models.MessageStatusSent), secondTemplateEvent.Payload["status"])

	assert.Equal(t, int32(2), providerCalls.Load())
	assert.Equal(t, int32(2), providerSawCommittedPending.Load(), "Meta must see committed pending state")
	messageIDs := []uuid.UUID{agentResponse.Data.ID, templateResponse.Data.ID}
	var canonical []models.Message
	require.NoError(t, adminDB.Where("id IN ?", messageIDs).Order("id").Find(&canonical).Error)
	require.Len(t, canonical, 2)
	for _, message := range canonical {
		assert.Equal(t, targetOrganization.ID, message.OrganizationID)
		assert.Equal(t, models.MessageStatusSent, message.Status)
		assert.NotEmpty(t, message.WhatsAppMessageID)
	}
	var createdContact models.Contact
	require.NoError(t, adminDB.Where(
		"id = ? AND organization_id = ?",
		templateResponse.Data.ContactID,
		targetOrganization.ID,
	).First(&createdContact).Error)
	assert.Equal(t, templateResponse.Data.ContactID, createdContact.ID)

	require.NoError(t, database.WithTenant(runtimeDB, homeOrganization.ID, func(tx *gorm.DB) error {
		var foreignMessages int64
		if err := tx.Model(&models.Message{}).Where("id IN ?", messageIDs).Count(&foreignMessages).Error; err != nil {
			return err
		}
		assert.Zero(t, foreignMessages, "token-home tenant must not see target messages")
		var foreignContacts int64
		if err := tx.Model(&models.Contact{}).Where("id = ?", createdContact.ID).Count(&foreignContacts).Error; err != nil {
			return err
		}
		assert.Zero(t, foreignContacts, "token-home tenant must not see target contact")
		return nil
	}))
}
