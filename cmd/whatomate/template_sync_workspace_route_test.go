package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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

// Give the mounted route its own database so the RLS verifier only sees this fixture.
func TestTemplateSyncRegisteredRouteUsesExactSelectedAccount(t *testing.T) {
	db, runtimeRole := testutil.OpenIsolatedTestDatabaseAsAdmin(t)
	reseller := testutil.CreateTestReseller(t, db)
	home := testutil.CreateTestOrganizationForReseller(t, db, reseller.ID)
	selected := testutil.CreateTestOrganizationForReseller(t, db, reseller.ID)
	foreign := testutil.CreateTestOrganizationForReseller(t, db, reseller.ID)
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
	testutil.CreateTestWhatsAppAccountWith(t, db, selected.ID, func(account *models.WhatsAppAccount) {
		account.Status = "pending_subscription"
	})
	testutil.CreateTestWhatsAppAccountWith(t, db, selected.ID, func(account *models.WhatsAppAccount) {
		expires := time.Now().Add(-time.Hour)
		account.AccessTokenExpiresAt = &expires
	})
	templateName := "synthetic_route_template_" + uuid.NewString()[:8]
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

	var fixtureDatabase string
	planPGOK(t, db.Raw("SELECT pg_catalog.current_database()").Scan(&fixtureDatabase).Error, "identify isolated Template Sync database")
	require.NotEmpty(t, fixtureDatabase)
	runtimeURL, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	planPGOK(t, err, "parse isolated Template Sync test URL")
	runtimePassword := uuid.NewString() + uuid.NewString()
	runtimeURL.User = url.UserPassword(runtimeRole, runtimePassword)
	runtimeURL.Path = "/" + fixtureDatabase
	runtimeURL.RawPath = ""
	// pgx gives these query fields precedence over the fixture login and path.
	query := runtimeURL.Query()
	query.Del("database")
	query.Del("dbname")
	query.Del("user")
	query.Del("password")
	runtimeURL.RawQuery = query.Encode()
	// Keep the isolation helper's restricted role attributes and enable direct test login.
	planPGOK(t, db.Exec(fmt.Sprintf("ALTER ROLE %s LOGIN PASSWORD '%s'", runtimeRole, runtimePassword)).Error, "enable isolated Template Sync login")
	require.NoError(t, database.ApplyTenantRLS(db, runtimeRole))
	runtimeConfig := config.DatabaseConfig{URL: runtimeURL.String(), MaxOpenConns: 1, MaxIdleConns: 1}
	connectCtx, cancelConnect := context.WithTimeout(t.Context(), 30*time.Second)
	runtime, err := database.NewPostgresWithContext(connectCtx, &runtimeConfig, false)
	cancelConnect()
	planPGOK(t, err, "open isolated Template Sync runtime")
	pool, err := runtime.DB()
	planPGOK(t, err, "inspect isolated Template Sync runtime pool")
	t.Cleanup(func() { planPGOK(t, pool.Close(), "close isolated Template Sync runtime") })
	var runtimeDatabase string
	planPGOK(t, runtime.Raw("SELECT pg_catalog.current_database()").Scan(&runtimeDatabase).Error, "verify isolated Template Sync runtime database")
	require.Equal(t, fixtureDatabase, runtimeDatabase)
	require.NoError(t, database.VerifyTenantRLS(runtime, runtimeRole))
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
		{"/api/templates/sync", "!" + all.ID.String() + "?", fasthttp.StatusBadRequest},
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
