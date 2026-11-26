package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

type templateSyncRouteTransport func(*http.Request) (*http.Response, error)

func (f templateSyncRouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// Run serially because installing RLS changes the disposable fixture schema.
func TestTemplateSyncRegisteredRouteUsesExactSelectedAccount(t *testing.T) {
	db := testutil.SetupTestDB(t)
	home := testutil.CreateTestOrganization(t, db)
	selected := testutil.CreateTestOrganization(t, db)
	foreign := testutil.CreateTestOrganization(t, db)
	homeRole := testutil.CreateTestRoleWithKeys(t, db, home.ID, "home-template-sync", []string{"templates:sync"})
	selectedReader := testutil.CreateTestRoleWithKeys(t, db, selected.ID, "selected-template-reader", []string{"templates:read"})
	selectedRole := testutil.CreateTestRoleWithKeys(t, db, selected.ID, "selected-template-sync", []string{"templates:sync"})
	user := testutil.CreateTestUser(t, db, home.ID, testutil.WithRoleID(&homeRole.ID))
	require.False(t, user.IsSuperAdmin)
	membership := models.UserOrganization{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		UserID:         user.ID,
		OrganizationID: selected.ID,
		RoleID:         &selectedReader.ID,
		Source:         models.MembershipSourceDirect,
	}
	require.NoError(t, db.Create(&membership).Error)

	// Another account is available in both the home and selected workspaces.
	// Only the exact literal selection may reach the provider.
	testutil.CreateTestWhatsAppAccount(t, db, home.ID)
	other := testutil.CreateTestWhatsAppAccount(t, db, selected.ID)
	all := testutil.CreateTestWhatsAppAccountWith(t, db, selected.ID, testutil.WithAccountName("all"), func(account *models.WhatsAppAccount) {
		account.AccessToken = "synthetic-template-sync-selected-account"
	})
	inactive := testutil.CreateTestWhatsAppAccountWith(t, db, selected.ID, func(account *models.WhatsAppAccount) {
		account.Status = "pending_subscription"
	})
	expired := testutil.CreateTestWhatsAppAccountWith(t, db, selected.ID, func(account *models.WhatsAppAccount) {
		expires := time.Now().Add(-time.Hour)
		account.AccessTokenExpiresAt = &expires
	})
	templateName := "synthetic_route_template_" + uuid.NewString()[:8]
	t.Cleanup(func() {
		if err := db.Unscoped().Where("name = ? AND organization_id IN ?", templateName, []uuid.UUID{home.ID, selected.ID}).Delete(&models.Template{}).Error; err != nil {
			t.Errorf("remove synthetic synced template: %v", err)
		}
		if err := db.Unscoped().Where("id IN ?", []uuid.UUID{all.ID, inactive.ID, expired.ID}).Delete(&models.WhatsAppAccount{}).Error; err != nil {
			t.Errorf("remove synthetic template sync accounts: %v", err)
		}
	})
	const templateID = "123450000000001"
	providerBody, err := json.Marshal(map[string]any{"data": []whatsapp.MetaTemplate{{
		ID: templateID, Name: templateName, Language: "en", Category: "UTILITY", Status: "APPROVED",
		Components: []whatsapp.TemplateComponent{{Type: "BODY", Text: "Synthetic route template"}},
	}}})
	require.NoError(t, err)

	const providerHost = "template-sync-fixture.invalid"
	wantPath := "/" + all.APIVersion + "/" + all.BusinessID + "/message_templates"
	providerCalls := 0
	log := testutil.NopLogger()
	client := whatsapp.NewWithBaseURL(log, "https://"+providerHost)
	client.HTTPClient.Transport = templateSyncRouteTransport(func(req *http.Request) (*http.Response, error) {
		providerCalls++
		if req.Method != http.MethodGet || req.URL.Scheme != "https" || req.URL.Host != providerHost || req.URL.Path != wantPath ||
			req.Header.Get("Authorization") != "Bearer "+all.AccessToken {
			return nil, errors.New("unexpected synthetic template provider request")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(providerBody))}, nil
	})

	runtimeRole := "rereply_template_route_" + uuid.NewString()[:8]
	runtimePassword := uuid.NewString() + uuid.NewString()
	require.NoError(t, db.Exec(fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION",
		runtimeRole, runtimePassword,
	)).Error)
	rlsApplied := false
	t.Cleanup(func() {
		if rlsApplied {
			if err := database.RemoveTenantRLS(db); err != nil {
				t.Errorf("remove disposable Template Sync RLS: %v", err)
			}
		}
		if err := db.Exec("DROP OWNED BY " + runtimeRole).Error; err != nil {
			t.Errorf("remove disposable Template Sync grants: %v", err)
		}
		if err := db.Exec("DROP ROLE " + runtimeRole).Error; err != nil {
			t.Errorf("remove disposable Template Sync role: %v", err)
		}
	})
	require.NoError(t, database.ApplyTenantRLS(db, runtimeRole))
	rlsApplied = true
	runtime := testutil.OpenTestDBAsRole(t, runtimeRole, runtimePassword)
	require.NoError(t, database.VerifyTenantRLS(runtime, runtimeRole))
	pool, err := runtime.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	runtime = runtime.WithContext(ctx)

	// The mounted route runs with the restricted login, while the fixture
	// owner is used only for setup, independent observation, and cleanup.
	cfg := &config.Config{
		Database: config.DatabaseConfig{RLSEnabled: true, RuntimeRole: runtimeRole},
		JWT:      config.JWTConfig{Secret: testutil.TestJWTSecret},
	}
	app := &handlers.App{DB: runtime, Config: cfg, Log: log, WhatsApp: client}
	glue := fastglue.NewGlue()
	glue.Before(middleware.SecurityHeaders())
	glue.Before(middleware.Recovery(log))
	glue.Before(middleware.CSRFProtection())
	setupRoutes(glue, app, log, "", nil, cfg)

	now := time.Now()
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.JWTClaims{
		UserID:         user.ID,
		OrganizationID: home.ID,
		Email:          user.Email,
		RoleID:         user.RoleID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   middleware.JWTSubjectAccess,
			Issuer:    "whatomate",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}).SignedString([]byte(cfg.JWT.Secret))
	require.NoError(t, err)
	csrf := uuid.NewString()
	cookies := map[string]string{"whm_access": access, "whm_csrf": csrf}
	invoke := func(cookies, headers map[string]string) *fasthttp.RequestCtx {
		return laneAInvoke(glue, fasthttp.MethodPost, "/api/templates/sync", cookies, headers,
			map[string]string{"whatsapp_account": "all"})
	}
	choices := func(cookies, headers map[string]string) *fasthttp.RequestCtx {
		return laneAInvoke(glue, fasthttp.MethodGet, "/api/templates/sync/accounts", cookies, headers, nil)
	}
	headers := map[string]string{"X-Organization-ID": selected.ID.String(), "X-CSRF-Token": csrf}
	noAuth := invoke(nil, headers)
	require.Equal(t, fasthttp.StatusUnauthorized, noAuth.Response.StatusCode())
	require.Equal(t, fasthttp.StatusUnauthorized, choices(nil, headers).Response.StatusCode())
	noCSRF := invoke(cookies, map[string]string{"X-Organization-ID": selected.ID.String()})
	require.Equal(t, fasthttp.StatusForbidden, noCSRF.Response.StatusCode())
	require.JSONEq(t, `{"status":"error","message":"CSRF token mismatch"}`, string(noCSRF.Response.Body()))
	readOnly := invoke(cookies, headers)
	require.Equal(t, fasthttp.StatusForbidden, readOnly.Response.StatusCode())
	require.JSONEq(t, `{"status":"error","message":"Insufficient permissions","data":null}`, string(readOnly.Response.Body()))
	require.Equal(t, fasthttp.StatusForbidden, choices(cookies, headers).Response.StatusCode())
	require.Zero(t, providerCalls)

	granted := db.Model(&membership).Update("role_id", selectedRole.ID)
	require.NoError(t, granted.Error)
	require.EqualValues(t, 1, granted.RowsAffected)

	generalAccounts := laneAInvoke(glue, fasthttp.MethodGet, "/api/accounts", cookies, headers, nil)
	require.Equal(t, fasthttp.StatusForbidden, generalAccounts.Response.StatusCode())
	require.Equal(t, fasthttp.StatusBadRequest, choices(cookies, nil).Response.StatusCode())
	require.Equal(t, fasthttp.StatusForbidden, choices(cookies, map[string]string{"X-Organization-ID": foreign.ID.String()}).Response.StatusCode())
	chosen := choices(cookies, headers)
	require.Equal(t, fasthttp.StatusOK, chosen.Response.StatusCode())
	require.Equal(t, "private, no-store", string(chosen.Response.Header.Peek("Cache-Control")))
	var choiceResult struct {
		Data struct {
			Accounts []map[string]any `json:"accounts"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(chosen.Response.Body(), &choiceResult))
	require.ElementsMatch(t, []map[string]any{
		{"id": all.ID.String(), "name": "all"},
		{"id": other.ID.String(), "name": other.Name},
	}, choiceResult.Data.Accounts)
	require.Zero(t, providerCalls)

	for _, candidate := range []struct {
		path   string
		id     any
		status int
	}{
		{"/api/templates/sync", other.ID.String(), fasthttp.StatusNotFound},
		{"/api/templates/sync", "invalid-uuid", fasthttp.StatusBadRequest},
		{"/api/templates/sync?account=all", nil, fasthttp.StatusBadRequest},
		{"/api/templates/sync", uuid.Nil.String(), fasthttp.StatusBadRequest},
	} {
		refused := laneAInvoke(glue, fasthttp.MethodPost, candidate.path, cookies, headers,
			map[string]any{"whatsapp_account": "all", "whatsapp_account_id": candidate.id})
		require.Equal(t, candidate.status, refused.Response.StatusCode())
		require.Zero(t, providerCalls)
	}

	synced := laneAInvoke(glue, fasthttp.MethodPost, "/api/templates/sync", cookies, headers,
		map[string]string{"whatsapp_account": "all", "whatsapp_account_id": all.ID.String()})
	require.Equal(t, fasthttp.StatusOK, synced.Response.StatusCode())
	var result struct {
		Data struct {
			Count int `json:"count"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(synced.Response.Body(), &result))
	require.Equal(t, 1, result.Data.Count)
	require.Equal(t, 1, providerCalls)
	var rows []models.Template
	require.NoError(t, db.Where("organization_id IN ?", []uuid.UUID{home.ID, selected.ID}).Find(&rows).Error)
	require.Len(t, rows, 1)
	require.Equal(t, selected.ID, rows[0].OrganizationID)
	require.Equal(t, all.Name, rows[0].WhatsAppAccount)
	require.Equal(t, templateName, rows[0].Name)
	require.Equal(t, templateID, rows[0].MetaTemplateID)

	for _, scope := range []struct {
		workspace uuid.UUID
		count     int64
	}{{selected.ID, 1}, {home.ID, 0}} {
		var count int64
		require.NoError(t, database.WithTenant(runtime, scope.workspace, func(tx *gorm.DB) error {
			return tx.Model(&models.Template{}).Where("id = ?", rows[0].ID).Count(&count).Error
		}))
		require.Equal(t, scope.count, count)
	}

	removed := db.Delete(&membership)
	require.NoError(t, removed.Error)
	require.EqualValues(t, 1, removed.RowsAffected)
	refused := invoke(cookies, headers)
	require.Equal(t, fasthttp.StatusForbidden, refused.Response.StatusCode())
	require.JSONEq(t, `{"status":"error","message":"Selected organization is not available","data":null}`, string(refused.Response.Body()))
	require.Equal(t, fasthttp.StatusForbidden, choices(cookies, headers).Response.StatusCode())
	require.Equal(t, 1, providerCalls)
}
