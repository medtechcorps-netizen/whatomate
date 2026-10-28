package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// This store only contains synthetic bytes. Denied requests must not reach it,
// and a protected GET must never write or remove an object.
type protectedMediaRouteStore struct {
	objects map[string][]byte
	gets    []string
	writes  int
	lists   int
}

var _ storage.ObjectStore = (*protectedMediaRouteStore)(nil)

func (s *protectedMediaRouteStore) Get(_ context.Context, key string) ([]byte, string, error) {
	s.gets = append(s.gets, key)
	data, ok := s.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return data, "image/png", nil
}

func (s *protectedMediaRouteStore) Put(context.Context, string, []byte, string) error {
	s.writes++
	return errors.New("protected media GET attempted object write")
}

func (s *protectedMediaRouteStore) Delete(context.Context, string) error {
	s.writes++
	return errors.New("protected media GET attempted object deletion")
}

func (s *protectedMediaRouteStore) ListPrefix(context.Context, string) ([]storage.ObjectInfo, error) {
	s.lists++
	return nil, errors.New("protected media GET attempted object listing")
}

type protectedMediaNoNetwork struct{ calls int }

func (n *protectedMediaNoNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	n.calls++
	return nil, errors.New("protected media GET attempted external HTTP")
}

func TestProtectedMediaRouteExactWorkspaceAndDenials(t *testing.T) {
	db := testutil.SetupTestDB(t)
	rdb := testutil.SetupTestRedis(t)
	if rdb == nil {
		t.Skip("TEST_REDIS_URL not set, skipping test")
	}
	home := testutil.CreateTestOrganization(t, db)
	selected := testutil.CreateTestOrganization(t, db)
	foreign := testutil.CreateTestOrganization(t, db)
	homeRole := testutil.CreateTestRoleWithKeys(t, db, home.ID, "media-home-reader", []string{"contacts:read"})
	selectedRole := testutil.CreateTestRoleWithKeys(t, db, selected.ID, "media-selected-reader", []string{"contacts:read"})
	reader := testutil.CreateTestUser(t, db, home.ID, testutil.WithRoleID(&homeRole.ID))
	limitedRole := testutil.CreateTestRoleExact(t, db, home.ID, "media-assigned-only", false, false, nil)
	limited := testutil.CreateTestUser(t, db, home.ID, testutil.WithRoleID(&limitedRole.ID))
	membership := models.UserOrganization{
		BaseModel: models.BaseModel{ID: uuid.New()}, UserID: reader.ID,
		OrganizationID: selected.ID, RoleID: &selectedRole.ID,
	}
	require.NoError(t, db.Create(&membership).Error)

	store := &protectedMediaRouteStore{objects: make(map[string][]byte)}
	network := &protectedMediaNoNetwork{}
	cfg := &config.Config{
		JWT:      config.JWTConfig{Secret: testutil.TestJWTSecret, AccessExpiryMins: 15, RefreshExpiryDays: 7},
		Database: config.DatabaseConfig{RLSEnabled: true},
		Storage:  config.StorageConfig{Type: "s3"},
	}
	app := &handlers.App{
		Config: cfg, DB: db, Redis: rdb, Log: testutil.NopLogger(),
		ObjectStore: store, HTTPClient: &http.Client{Transport: network},
	}
	g := fastglue.NewGlue()
	g.Before(middleware.SecurityHeaders())
	g.Before(middleware.Recovery(app.Log))
	g.Before(middleware.CSRFProtection())
	// Use the actual production registration: AuthWithDB -> Tenant -> ServeMedia.
	// The test DB owner exercises the application/transaction boundary; this
	// does not replace the independent restricted-runtime-role RLS suite.
	setupRoutes(g, app, app.Log, "", rdb, cfg)

	tokenFor := func(user *models.User) string {
		t.Helper()
		claims := middleware.JWTClaims{
			UserID: user.ID, OrganizationID: user.OrganizationID, Email: user.Email, RoleID: user.RoleID,
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: middleware.JWTSubjectAccess, Issuer: "whatomate",
				IssuedAt: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
		}
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(cfg.JWT.Secret))
		require.NoError(t, err)
		return token
	}
	readerToken, limitedToken := tokenFor(reader), tokenFor(limited)
	newMedia := func(orgID uuid.UUID, keyOrgID uuid.UUID) (*models.Contact, *models.Message) {
		t.Helper()
		contact := testutil.CreateTestContact(t, db, orgID)
		require.NoError(t, db.Model(contact).Update("is_read", false).Error)
		key := fmt.Sprintf("organizations/%s/messages/images/%s.png", keyOrgID, uuid.New())
		store.objects[key] = []byte("synthetic-protected-media-" + uuid.NewString())
		message := &models.Message{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID, ContactID: contact.ID,
			Direction: models.DirectionIncoming, MessageType: models.MessageTypeImage,
			MediaURL: key, MediaMimeType: "image/png", Status: models.MessageStatusDelivered,
		}
		require.NoError(t, db.Create(message).Error)
		return contact, message
	}
	homeContact, homeMessage := newMedia(home.ID, home.ID)
	_, selectedMessage := newMedia(selected.ID, selected.ID)
	_, foreignMessage := newMedia(foreign.ID, foreign.ID)
	_, wrongKeyMessage := newMedia(home.ID, foreign.ID)
	assignedContact, assignedMessage := newMedia(home.ID, home.ID)
	require.NoError(t, db.Model(assignedContact).Update("assigned_user_id", limited.ID).Error)
	_, transferMessage := newMedia(home.ID, home.ID)
	transfer := models.AgentTransfer{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: home.ID,
		ContactID: transferMessage.ContactID, AgentID: &limited.ID, Status: models.TransferStatusActive,
	}
	require.NoError(t, db.Create(&transfer).Error)

	request := func(t *testing.T, token, selectedHeader string, message *models.Message, want int) {
		t.Helper()
		var beforeContact models.Contact
		var beforeMessage models.Message
		require.NoError(t, db.First(&beforeContact, "id = ?", message.ContactID).Error)
		require.NoError(t, db.First(&beforeMessage, "id = ?", message.ID).Error)
		var beforeMessages int64
		require.NoError(t, db.Model(&models.Message{}).Where("organization_id = ?", message.OrganizationID).Count(&beforeMessages).Error)
		beforeGets := len(store.gets)
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(fasthttp.MethodGet)
		ctx.Request.SetRequestURI("/api/media/" + message.ID.String())
		if token != "" {
			ctx.Request.Header.SetCookie("whm_access", token)
		}
		if selectedHeader != "" {
			ctx.Request.Header.Set("X-Organization-ID", selectedHeader)
		}
		g.Handler()(ctx)
		require.Equal(t, want, ctx.Response.StatusCode(), string(ctx.Response.Body()))
		if want == fasthttp.StatusOK {
			require.Equal(t, store.objects[message.MediaURL], ctx.Response.Body())
			require.Equal(t, []string{message.MediaURL}, store.gets[beforeGets:])
			require.Equal(t, "private, max-age=3600", string(ctx.Response.Header.Peek("Cache-Control")))
			require.Equal(t, "image/png", string(ctx.Response.Header.Peek("Content-Type")))
			require.Equal(t, "nosniff", string(ctx.Response.Header.Peek("X-Content-Type-Options")))
		} else {
			require.Len(t, store.gets, beforeGets, "denial must precede any object read")
			for _, body := range store.objects {
				require.NotContains(t, string(ctx.Response.Body()), string(body))
			}
		}
		require.Zero(t, store.writes)
		require.Zero(t, store.lists)
		require.Zero(t, network.calls, "GET must not send a provider read receipt or other HTTP request")
		var afterContact models.Contact
		var afterMessage models.Message
		require.NoError(t, db.First(&afterContact, "id = ?", message.ContactID).Error)
		require.NoError(t, db.First(&afterMessage, "id = ?", message.ID).Error)
		require.Equal(t, beforeContact.IsRead, afterContact.IsRead)
		require.Equal(t, beforeContact.AssignedUserID, afterContact.AssignedUserID)
		require.True(t, beforeContact.UpdatedAt.Equal(afterContact.UpdatedAt))
		require.Equal(t, beforeMessage.Status, afterMessage.Status)
		require.Equal(t, beforeMessage.MediaURL, afterMessage.MediaURL)
		require.True(t, beforeMessage.UpdatedAt.Equal(afterMessage.UpdatedAt))
		var afterMessages int64
		require.NoError(t, db.Model(&models.Message{}).Where("organization_id = ?", message.OrganizationID).Count(&afterMessages).Error)
		require.Equal(t, beforeMessages, afterMessages)
	}

	for _, tc := range []struct {
		name, token, header string
		message             *models.Message
		want                int
	}{
		{"default_workspace_without_header", readerToken, "", homeMessage, fasthttp.StatusOK},
		{"explicit_home", readerToken, home.ID.String(), homeMessage, fasthttp.StatusOK},
		{"explicit_member_workspace", readerToken, selected.ID.String(), selectedMessage, fasthttp.StatusOK},
		{"home_message_not_visible_in_selected_workspace", readerToken, selected.ID.String(), homeMessage, fasthttp.StatusNotFound},
		{"foreign_message_not_visible_in_home", readerToken, home.ID.String(), foreignMessage, fasthttp.StatusNotFound},
		{"foreign_header_never_falls_back_to_home", readerToken, foreign.ID.String(), homeMessage, fasthttp.StatusForbidden},
		{"malformed_header_never_falls_back", readerToken, "not-an-organization", homeMessage, fasthttp.StatusBadRequest},
		{"nil_header_never_falls_back", readerToken, uuid.Nil.String(), homeMessage, fasthttp.StatusBadRequest},
		{"wrong_organization_object_key", readerToken, home.ID.String(), wrongKeyMessage, fasthttp.StatusBadRequest},
		{"unauthenticated", "", home.ID.String(), homeMessage, fasthttp.StatusUnauthorized},
		{"invalid_access_cookie", "invalid-token", home.ID.String(), homeMessage, fasthttp.StatusUnauthorized},
		{"unassigned_without_contacts_read", limitedToken, home.ID.String(), homeMessage, fasthttp.StatusForbidden},
		{"assigned_without_contacts_read", limitedToken, home.ID.String(), assignedMessage, fasthttp.StatusOK},
		{"active_direct_transfer_without_contacts_read", limitedToken, home.ID.String(), transferMessage, fasthttp.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) { request(t, tc.token, tc.header, tc.message, tc.want) })
	}
	t.Run("revoked_selected_membership_never_falls_back", func(t *testing.T) {
		require.NoError(t, db.Delete(&membership).Error)
		request(t, readerToken, selected.ID.String(), homeMessage, fasthttp.StatusForbidden)
		request(t, readerToken, "", homeMessage, fasthttp.StatusOK)
	})
	t.Run("revoked_assignment_denies_same_cookie", func(t *testing.T) {
		require.NoError(t, db.Model(assignedContact).Update("assigned_user_id", nil).Error)
		request(t, limitedToken, home.ID.String(), assignedMessage, fasthttp.StatusForbidden)
	})
	t.Run("inactive_transfer_denies_same_cookie", func(t *testing.T) {
		require.NoError(t, db.Model(&transfer).Update("status", models.TransferStatusResumed).Error)
		request(t, limitedToken, home.ID.String(), transferMessage, fasthttp.StatusForbidden)
	})
	t.Run("inactive_user_denies_previously_valid_cookie", func(t *testing.T) {
		require.NoError(t, db.Model(reader).Update("is_active", false).Error)
		request(t, readerToken, home.ID.String(), homeMessage, fasthttp.StatusUnauthorized)
	})
	// The ordinary reader's contact never became read or assigned as a result
	// of any successful or refused protected-media request.
	require.NoError(t, db.First(homeContact, "id = ?", homeContact.ID).Error)
	require.False(t, homeContact.IsRead)
	require.Nil(t, homeContact.AssignedUserID)
}
