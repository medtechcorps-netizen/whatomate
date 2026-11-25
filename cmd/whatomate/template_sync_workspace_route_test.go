package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

type templateSyncRouteTransport func(*http.Request) (*http.Response, error)

func (f templateSyncRouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestTemplateSyncRegisteredRouteUsesExactSelectedAccount(t *testing.T) {
	db := testutil.SetupTestDB(t)
	home := testutil.CreateTestOrganization(t, db)
	selected := testutil.CreateTestOrganization(t, db)
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
	testutil.CreateTestWhatsAppAccount(t, db, selected.ID)
	all := testutil.CreateTestWhatsAppAccountWith(t, db, selected.ID, testutil.WithAccountName("all"), func(account *models.WhatsAppAccount) {
		account.AccessToken = "synthetic-template-sync-selected-account"
	})
	templateName := "synthetic_route_template_" + uuid.NewString()[:8]
	t.Cleanup(func() {
		if err := db.Unscoped().Where("name = ? AND organization_id IN ?", templateName, []uuid.UUID{home.ID, selected.ID}).Delete(&models.Template{}).Error; err != nil {
			t.Errorf("remove synthetic synced template: %v", err)
		}
		if err := db.Unscoped().Delete(all).Error; err != nil {
			t.Errorf("remove literal all fixture account: %v", err)
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

	// Exercise the real tenant transaction, cookie auth and CSRF middleware on
	// the fixture database connection.
	cfg := &config.Config{
		Database: config.DatabaseConfig{RLSEnabled: true},
		JWT:      config.JWTConfig{Secret: testutil.TestJWTSecret},
	}
	app := &handlers.App{DB: db, Config: cfg, Log: log, WhatsApp: client}
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
	headers := map[string]string{"X-Organization-ID": selected.ID.String(), "X-CSRF-Token": csrf}
	noAuth := invoke(nil, headers)
	require.Equal(t, fasthttp.StatusUnauthorized, noAuth.Response.StatusCode())
	noCSRF := invoke(cookies, map[string]string{"X-Organization-ID": selected.ID.String()})
	require.Equal(t, fasthttp.StatusForbidden, noCSRF.Response.StatusCode())
	require.JSONEq(t, `{"status":"error","message":"CSRF token mismatch"}`, string(noCSRF.Response.Body()))
	readOnly := invoke(cookies, headers)
	require.Equal(t, fasthttp.StatusForbidden, readOnly.Response.StatusCode())
	require.JSONEq(t, `{"status":"error","message":"Insufficient permissions","data":null}`, string(readOnly.Response.Body()))
	require.Zero(t, providerCalls)

	granted := db.Model(&membership).Update("role_id", selectedRole.ID)
	require.NoError(t, granted.Error)
	require.EqualValues(t, 1, granted.RowsAffected)
	synced := invoke(cookies, headers)
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

	removed := db.Delete(&membership)
	require.NoError(t, removed.Error)
	require.EqualValues(t, 1, removed.RowsAffected)
	refused := invoke(cookies, headers)
	require.Equal(t, fasthttp.StatusForbidden, refused.Response.StatusCode())
	require.JSONEq(t, `{"status":"error","message":"Selected organization is not available","data":null}`, string(refused.Response.Body()))
	require.Equal(t, 1, providerCalls)
}
