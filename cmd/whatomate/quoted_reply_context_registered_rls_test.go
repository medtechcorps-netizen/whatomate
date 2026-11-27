package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

type quotedReplyRouterStore struct {
	protectedMediaRouteStore
	limitedKeys  []string
	limitedSizes []int64
	beforeReturn func(context.Context) error
}

func (s *quotedReplyRouterStore) GetLimited(ctx context.Context, key string, maxBytes int64) ([]byte, string, error) {
	s.limitedKeys = append(s.limitedKeys, key)
	s.limitedSizes = append(s.limitedSizes, maxBytes)
	body, ok := s.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	if s.beforeReturn != nil {
		if err := s.beforeReturn(ctx); err != nil {
			return nil, "", err
		}
	}
	read, err := storage.ReadLimited(ctx, bytes.NewReader(body), maxBytes)
	return read, "image/png", err
}

// Run exactly once against a root-owned disposable PG17 cluster. Supply an
// explicit loopback TEST_DATABASE_URL with synthetic allocator credentials, no
// ambient PG configuration, and REREPLY_TEST_QUOTE_ROUTER_MOUNTED=1.
//
// go test -json -p=1 ./cmd/whatomate -run '^TestQuotedReplyContextRegisteredRoutesUnderRLS$' -count=1 -parallel=1 -timeout=8m
//
// One fresh database/two generated roles, one restricted App connection, in-
// process production router, zero provider/object-store network calls. No
// listener, Redis, browser, customer traffic or production release claim.
func TestQuotedReplyContextRegisteredRoutesUnderRLS(t *testing.T) {
	if os.Getenv("REREPLY_TEST_QUOTE_ROUTER_MOUNTED") != "1" {
		t.Skip("requires the dedicated disposable quote router fixture")
	}
	run, count, parallel := flag.Lookup("test.run"), flag.Lookup("test.count"), flag.Lookup("test.parallel")
	require.True(t, run != nil && run.Value.String() == "^TestQuotedReplyContextRegisteredRoutesUnderRLS$" &&
		count != nil && count.Value.String() == "1" && parallel != nil && parallel.Value.String() == "1",
		"run this mounted quote root alone in a fresh process")
	guardQuoteRouterFixture(t)
	fixture := newMigrationPlanFixture(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	allocatorConfig := fixture.cfg
	allocatorConfig.URL = fixture.admin.Config().ConnString()
	allocator, err := database.NewPostgresWithContext(ctx, &allocatorConfig, false)
	planPGOK(t, err, "open owned quote fixture allocator")
	allocatorPool, err := allocator.DB()
	planPGOK(t, err, "inspect owned quote fixture allocator")
	t.Cleanup(func() { planPGOK(t, allocatorPool.Close(), "close owned quote fixture allocator") })
	allocator = allocator.WithContext(ctx)
	var allocatedDatabase string
	planPGOK(t, allocator.Raw("SELECT current_database()").Scan(&allocatedDatabase).Error, "identify quote fixture database")
	require.Equal(t, fixture.database, allocatedDatabase)

	reseller := testutil.CreateTestReseller(t, allocator)
	home := testutil.CreateTestOrganizationForReseller(t, allocator, reseller.ID)
	selected := testutil.CreateTestOrganizationForReseller(t, allocator, reseller.ID)
	homeRole := testutil.CreateTestRoleWithKeys(t, allocator, home.ID, "quote-home", []string{"chat:read", "conversations:read"})
	selectedRole := testutil.CreateTestRoleWithKeys(t, allocator, selected.ID, "quote-selected", []string{"chat:read", "conversations:read"})
	narrowedRole := testutil.CreateTestRoleWithKeys(t, allocator, selected.ID, "quote-inbox-only", []string{"conversations:read"})
	actor := testutil.CreateTestUser(t, allocator, home.ID, testutil.WithRoleID(&homeRole.ID))
	membership := models.UserOrganization{
		BaseModel: models.BaseModel{ID: uuid.New()}, UserID: actor.ID, OrganizationID: selected.ID,
		RoleID: &selectedRole.ID, Source: models.MembershipSourceDirect,
	}
	require.NoError(t, allocator.Create(&membership).Error)
	enableLegacyWhatsAppReplyRouteEntitlement(t, allocator, selected.ID, actor.ID)

	account := testutil.CreateTestWhatsAppAccount(t, allocator, selected.ID)
	contact := testutil.CreateTestContactWith(t, allocator, selected.ID, testutil.WithContactAccount(account.Name))
	cutoff := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	require.NoError(t, allocator.Create(&models.ChatbotSettings{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: selected.ID,
		AgentAssignment: models.AgentAssignmentConfig{CurrentConversationOnly: true},
	}).Error)
	require.NoError(t, allocator.Create(&models.AgentTransfer{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: selected.ID, ContactID: contact.ID,
		WhatsAppAccount: account.Name, PhoneNumber: contact.PhoneNumber, Status: models.TransferStatusActive,
		Source: models.TransferSourceManual, AgentID: &actor.ID, TransferredAt: cutoff,
	}).Error)

	accountRef := channelapi.LegacyMetaAccountRef{ID: account.ID, OrganizationID: selected.ID, Name: account.Name, Status: account.Status}
	add := func(stamp time.Time, caption, key string, replyTo *uuid.UUID) (models.Message, uuid.UUID) {
		t.Helper()
		message := models.Message{
			BaseModel:      models.BaseModel{ID: uuid.New(), CreatedAt: stamp, UpdatedAt: stamp},
			OrganizationID: selected.ID, ContactID: contact.ID, WhatsAppAccount: account.Name,
			WhatsAppMessageID: "wamid.quote-router." + uuid.NewString(), IngestedAt: &stamp,
			Direction: models.DirectionIncoming, MessageType: models.MessageTypeText,
			Content: caption, Status: models.MessageStatusDelivered,
			IsReply: replyTo != nil, ReplyToMessageID: replyTo, Metadata: models.JSONB{},
		}
		if key != "" {
			message.MessageType, message.MediaURL, message.MediaMimeType = models.MessageTypeImage, key, "image/png"
		}
		require.NoError(t, allocator.Create(&message).Error)
		mirror, err := channelapi.MirrorLegacyWhatsAppMessage(allocator, accountRef, message.ID)
		require.NoError(t, err)
		require.True(t, mirror.Linked)
		require.NotEqual(t, uuid.Nil, mirror.ConversationID)
		return message, mirror.ConversationID
	}
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jD1sAAAAASUVORK5CYII=")
	require.NoError(t, err)
	key := "organizations/" + selected.ID.String() + "/messages/images/" + uuid.NewString() + ".png"
	const caption = "Synthetic authorized quote caption"
	const excludedCaption = "Synthetic pre-cutoff quote must stay hidden"
	old, _ := add(cutoff.Add(-time.Minute), excludedCaption, key, nil)
	source, conversation := add(cutoff.Add(time.Minute), caption, key, nil)
	child, childConversation := add(cutoff.Add(2*time.Minute), "Ordinary synthetic reply", "", &source.ID)
	require.Equal(t, conversation, childConversation)
	replyToOld, oldConversation := add(cutoff.Add(3*time.Minute), "Reply to earlier history", "", &old.ID)
	require.Equal(t, conversation, oldConversation)

	runtimeConfig := fixture.cfg
	runtimeConfig.MaxOpenConns, runtimeConfig.MaxIdleConns = 1, 1
	runtime, err := database.NewPostgresWithContext(ctx, &runtimeConfig, false)
	planPGOK(t, err, "open restricted quote runtime")
	pool, err := runtime.DB()
	planPGOK(t, err, "inspect restricted quote runtime")
	t.Cleanup(func() { planPGOK(t, pool.Close(), "close restricted quote runtime") })
	runtime = runtime.WithContext(ctx)
	planPGOK(t, database.VerifyTenantRLS(runtime, fixture.runtime), "verify real quote runtime RLS")

	var databaseName string
	var restricted bool
	var initialPID int
	planPGOK(t, runtime.Raw(`
		SELECT current_database(), pg_backend_pid(), current_user = session_user AND current_user = ?
			AND rolcanlogin AND NOT rolsuper AND NOT rolbypassrls AND NOT rolcreaterole
			AND NOT rolcreatedb AND NOT rolinherit AND NOT rolreplication
		FROM pg_roles WHERE rolname = current_user
	`, fixture.runtime).Row().Scan(&databaseName, &initialPID, &restricted), "verify physical quote reader login")
	require.Equal(t, fixture.database, databaseName)
	require.True(t, restricted)
	require.Positive(t, initialPID)
	for _, check := range []struct {
		workspace uuid.UUID
		want      int64
	}{{selected.ID, 1}, {home.ID, 0}} {
		var count int64
		err := database.WithTenant(runtime, check.workspace, func(tx *gorm.DB) error {
			// Deliberately omit organization_id so application predicates cannot
			// make a bypass/owner connection pass this RLS check.
			return tx.Model(&models.Message{}).Where("id = ?", source.ID).Count(&count).Error
		})
		planPGOK(t, err, "check physical quote RLS visibility")
		require.Equal(t, check.want, count)
	}
	probeUnscoped := func(parent context.Context) {
		t.Helper()
		probe, stop := context.WithTimeout(parent, 2*time.Second)
		defer stop()
		var pid int
		var tenant string
		var visible int64
		err := runtime.WithContext(probe).Raw(`
			SELECT pg_backend_pid(), COALESCE(current_setting('app.current_organization_id', true), ''),
				(SELECT count(*) FROM public.messages WHERE id IN (?, ?))
		`, child.ID, source.ID).Row().Scan(&pid, &tenant, &visible)
		planPGOK(t, err, "verify the sole quote runtime connection is released")
		require.Equal(t, initialPID, pid)
		require.Empty(t, tenant)
		require.Zero(t, visible)
	}
	probeUnscoped(ctx)

	store := &quotedReplyRouterStore{protectedMediaRouteStore: protectedMediaRouteStore{objects: map[string][]byte{key: png}}}
	network := &protectedMediaNoNetwork{}
	cfg := &config.Config{Database: runtimeConfig, JWT: config.JWTConfig{Secret: testutil.TestJWTSecret}}
	app := &handlers.App{DB: runtime, Config: cfg, Log: testutil.NopLogger(), ObjectStore: store,
		HTTPClient: &http.Client{Transport: network, Timeout: time.Second}}
	router := fastglue.NewGlue()
	router.Before(middleware.SecurityHeaders(), middleware.Recovery(app.Log), middleware.CSRFProtection())
	setupRoutes(router, app, app.Log, "", nil, cfg)
	now := time.Now().UTC()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.JWTClaims{
		UserID: actor.ID, OrganizationID: home.ID, Email: actor.Email, RoleID: actor.RoleID,
		RegisteredClaims: jwt.RegisteredClaims{Subject: middleware.JWTSubjectAccess, Issuer: "whatomate",
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute))},
	}).SignedString([]byte(cfg.JWT.Secret))
	require.NoError(t, err)
	cookies := map[string]string{"whm_access": token}
	get := func(path, workspace string, signed bool) *fasthttp.Response {
		t.Helper()
		headers := map[string]string{}
		if workspace != "" {
			headers["X-Organization-ID"] = workspace
		}
		var auth map[string]string
		if signed {
			auth = cookies
		}
		response := &laneAInvoke(router, fasthttp.MethodGet, path, auth, headers, nil).Response
		t.Cleanup(func() { require.NoError(t, response.CloseBodyStream()) })
		return response
	}

	nativeList := "/api/contacts/" + contact.ID.String() + "/messages"
	inboxList := "/api/conversations/" + conversation.String() + "/messages"
	accountQuery := "account=" + url.QueryEscape(account.Name)
	nativeTranscript := nativeList + "?" + accountQuery + "&acknowledge=false"
	revision := ""
	for _, path := range []string{nativeTranscript, inboxList + "?limit=2"} {
		response := get(path, selected.ID.String(), true)
		require.Equal(t, fasthttp.StatusOK, response.StatusCode())
		require.Equal(t, "private, no-store", string(response.Header.Peek("Cache-Control")))
		raw := response.Body()
		require.NotContains(t, string(raw), excludedCaption)
		var envelope struct {
			Data struct {
				Messages []struct {
					ID      uuid.UUID `json:"id"`
					Message struct {
						ID uuid.UUID `json:"id"`
					} `json:"message"`
					Original *struct {
						ID       uuid.UUID         `json:"id"`
						Content  map[string]string `json:"content"`
						Revision string            `json:"image_revision"`
						Scoped   bool              `json:"scoped_context"`
					} `json:"reply_to_message"`
					Unavailable bool `json:"reply_context_unavailable"`
				} `json:"messages"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(raw, &envelope))
		positive, hidden := false, false
		for _, row := range envelope.Data.Messages {
			id := row.ID
			if id == uuid.Nil {
				id = row.Message.ID
			}
			switch id {
			case child.ID:
				require.NotNil(t, row.Original)
				require.False(t, row.Unavailable)
				require.Equal(t, source.ID, row.Original.ID)
				require.Equal(t, caption, row.Original.Content["body"])
				require.True(t, row.Original.Scoped)
				require.Regexp(t, "^[0-9a-f]{64}$", row.Original.Revision)
				if revision == "" {
					revision = row.Original.Revision
				} else {
					require.Equal(t, revision, row.Original.Revision)
				}
				positive = true
			case replyToOld.ID:
				require.Nil(t, row.Original)
				require.True(t, row.Unavailable)
				hidden = true
			}
		}
		require.True(t, positive)
		require.True(t, hidden)
	}
	require.Empty(t, store.limitedKeys)

	type routeCase struct {
		surface string
		path    func(uuid.UUID) string
	}
	routes := []routeCase{
		{"native", func(id uuid.UUID) string {
			return nativeList + "/" + id.String() + "/reply-context-media?" + accountQuery + "&revision=" + revision
		}},
		{"inbox", func(id uuid.UUID) string {
			return inboxList + "/" + id.String() + "/reply-context-media?revision=" + revision
		}},
	}
	for _, route := range routes {
		t.Run(route.surface, func(t *testing.T) {
			before := len(store.limitedKeys)
			path := route.path(child.ID)
			require.Equal(t, fasthttp.StatusUnauthorized, get(path, selected.ID.String(), false).StatusCode())
			require.Equal(t, fasthttp.StatusBadRequest, get(path, "", true).StatusCode())
			require.Equal(t, fasthttp.StatusNotFound, get(path, home.ID.String(), true).StatusCode())
			require.Equal(t, fasthttp.StatusNotFound, get(route.path(source.ID), selected.ID.String(), true).StatusCode())
			require.Equal(t, fasthttp.StatusNotFound, get(route.path(replyToOld.ID), selected.ID.String(), true).StatusCode())
			require.Len(t, store.limitedKeys, before, "denied requests must not open even fake storage")

			store.beforeReturn = func(readContext context.Context) error { probeUnscoped(readContext); return nil }
			response := get(path, selected.ID.String(), true)
			require.Equal(t, fasthttp.StatusOK, response.StatusCode())
			require.Equal(t, "image/png", string(response.Header.ContentType()))
			require.Equal(t, "nosniff", string(response.Header.Peek("X-Content-Type-Options")))
			require.Equal(t, "private, no-store", string(response.Header.Peek("Cache-Control")))
			require.Equal(t, png, response.Body())
			require.NoError(t, response.CloseBodyStream())
			require.Len(t, store.limitedKeys, before+1)
			require.Equal(t, key, store.limitedKeys[before])

			store.beforeReturn = func(readContext context.Context) error {
				probeUnscoped(readContext)
				revoke := allocator.WithContext(readContext).Model(&models.UserOrganization{}).
					Where("id = ?", membership.ID).Update("role_id", narrowedRole.ID)
				require.NoError(t, revoke.Error)
				require.EqualValues(t, 1, revoke.RowsAffected)
				return nil
			}
			response = get(path, selected.ID.String(), true)
			require.Equal(t, fasthttp.StatusNotFound, response.StatusCode())
			require.False(t, response.IsBodyStream())
			require.NotContains(t, string(response.Body()), caption)
			require.Len(t, store.limitedKeys, before+2, "the in-flight request started before grant loss")
			store.beforeReturn = nil
			restore := allocator.Model(&models.UserOrganization{}).
				Where("id = ?", membership.ID).Update("role_id", selectedRole.ID)
			require.NoError(t, restore.Error)
			require.EqualValues(t, 1, restore.RowsAffected)
			probeUnscoped(ctx)
		})
	}
	// Preserve the old native quote shape while denying the new scoped preview
	// and bytes for a populated, same-name generic provider bridge.
	badShadow := createQuoteRouterAccount(t, allocator, selected.ID, "Generic shadow "+uuid.NewString())
	badConversation := models.InboxConversation{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: selected.ID, ChannelAccountID: badShadow.ID,
		ContactID: contact.ID, Channel: badShadow.Channel, ExternalConversationID: "legacy-contact:" + contact.ID.String(),
		Status: models.InboxConversationStatusOpen, OpenedAt: now, Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, allocator.Create(&badConversation).Error)
	move := allocator.Model(&models.Message{}).Where("id IN ?", []uuid.UUID{source.ID, child.ID}).
		UpdateColumn("inbox_conversation_id", badConversation.ID)
	require.NoError(t, move.Error)
	require.EqualValues(t, 2, move.RowsAffected)
	messageResponse := get(nativeTranscript, selected.ID.String(), true)
	require.Equal(t, fasthttp.StatusOK, messageResponse.StatusCode())
	var aliased struct {
		Data struct {
			Messages []struct {
				ID       uuid.UUID `json:"id"`
				Original *struct {
					ID       uuid.UUID `json:"id"`
					Revision string    `json:"image_revision"`
					Scoped   bool      `json:"scoped_context"`
				} `json:"reply_to_message"`
				Unavailable bool `json:"reply_context_unavailable"`
			} `json:"messages"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(messageResponse.Body(), &aliased))
	found := false
	for _, row := range aliased.Data.Messages {
		if row.ID != child.ID {
			continue
		}
		require.NotNil(t, row.Original)
		require.Equal(t, source.ID, row.Original.ID)
		require.False(t, row.Original.Scoped)
		require.Empty(t, row.Original.Revision)
		require.False(t, row.Unavailable)
		found = true
	}
	require.True(t, found)
	deniedAlias := get(routes[0].path(child.ID), selected.ID.String(), true)
	require.Equal(t, fasthttp.StatusNotFound, deniedAlias.StatusCode())
	require.False(t, deniedAlias.IsBodyStream())
	require.Equal(t, []int64{5 << 20, 5 << 20, 5 << 20, 5 << 20}, store.limitedSizes)
	require.Empty(t, store.gets)
	require.Zero(t, store.writes)
	require.Zero(t, store.lists)
	require.Zero(t, network.calls)
	var current models.Message
	require.NoError(t, allocator.First(&current, "id = ?", child.ID).Error)
	require.Equal(t, models.MessageStatusDelivered, current.Status)
	var cursors int64
	require.NoError(t, allocator.Model(&models.ConversationRead{}).
		Where("organization_id = ? AND conversation_id = ?", selected.ID, conversation).Count(&cursors).Error)
	require.Zero(t, cursors)
}

func guardQuoteRouterFixture(t *testing.T) {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	target, err := url.Parse(raw)
	require.True(t, err == nil && target != nil, "explicit disposable PostgreSQL URL required")
	require.True(t, target.Scheme == "postgres" || target.Scheme == "postgresql", "PostgreSQL URL required")
	require.NotNil(t, target.User, "explicit disposable allocator login required")
	password, present := target.User.Password()
	require.True(t, present && password != "" && target.User.Username() != "", "explicit disposable allocator credentials required")
	host := target.Hostname()
	port := target.Port()
	require.True(t, (host == "127.0.0.1" || host == "::1" || host == "localhost") && port != "" &&
		target.Host == net.JoinHostPort(host, port), "an explicit loopback PostgreSQL endpoint is required")
	query, err := url.ParseQuery(target.RawQuery)
	require.True(t, err == nil, "valid PostgreSQL fixture query required")
	if !query.Has("sslmode") {
		query.Set("sslmode", "disable")
	}
	require.Equal(t, []string{"disable"}, query["sslmode"], "private local PostgreSQL connection mode required")
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		require.False(t, strings.HasPrefix(strings.ToUpper(name), "PG"), "ambient PostgreSQL overrides are unavailable in this fixture")
	}
	// Keep the CI fixture's localhost spelling usable without resolving it.
	if host == "localhost" {
		target.Host = net.JoinHostPort("127.0.0.1", port)
	}
	target.RawQuery = query.Encode()
	t.Setenv("TEST_DATABASE_URL", target.String())
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "unwritten-pgpass"))
}

func createQuoteRouterAccount(t *testing.T, db *gorm.DB, workspace uuid.UUID, name string) models.ChannelAccount {
	t.Helper()
	account := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: workspace,
		Channel: models.ChannelWhatsApp, Provider: "relay", Name: name, ExternalAccountID: uuid.NewString(),
		Status: models.ChannelAccountStatusActive, Capabilities: models.JSONB{}, Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&account).Error)
	return account
}
