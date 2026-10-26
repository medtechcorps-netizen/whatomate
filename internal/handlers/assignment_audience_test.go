package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	wire "github.com/fasthttp/websocket"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	ws "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Exercise real wire pumps and the production typed WebSocket token validator.
// Read in one owner goroutine so a negative delivery assertion does not poison
// the socket with a read deadline before a subsequent positive assertion.
func assignmentSocketServer(t *testing.T, app *App) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	up := newUpgrader(nil)
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		_ = up.Upgrade(ctx, func(conn *wire.Conn) {
			client := ws.NewUnauthenticatedClient(app.WSHub, conn, app.validateWSTokenFn())
			go client.WritePump()
			client.ReadPump()
		})
	}}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("socket listener did not stop")
		}
	})
	return "ws://" + listener.Addr().String()
}
func assignmentSocket(t *testing.T, app *App, address string, userID, orgID uuid.UUID) <-chan []byte {
	t.Helper()
	before := app.WSHub.GetClientCount()
	conn, _, err := wire.DefaultDialer.Dial(address, nil)
	require.NoError(t, err)
	token := signAuthFenceToken(t, userID, orgID, middleware.JWTSubjectWebSocket, "", jwt.SigningMethodHS256)
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "auth", "payload": map[string]string{"token": token}}))
	testutil.AssertEventually(t, func() bool { return app.WSHub.GetClientCount() == before+1 }, 2*time.Second, "socket authenticates")
	messages := make(chan []byte, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(messages)
		for {
			_, b, e := conn.ReadMessage()
			if e != nil {
				return
			}
			select {
			case messages <- b:
			default:
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("socket reader did not stop")
		}
	})
	return messages
}
func assignmentWireHint(t *testing.T, ch <-chan []byte, contactID uuid.UUID) {
	t.Helper()
	select {
	case b, ok := <-ch:
		require.True(t, ok)
		var envelope struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		require.NoError(t, json.Unmarshal(b, &envelope))
		assert.Equal(t, ws.TypeContactUpdate, envelope.Type)
		assert.Equal(t, map[string]any{"contact_id": contactID.String(), "kind": "assignment"}, envelope.Payload)
	case <-time.After(3 * time.Second):
		t.Fatal("missing assignment hint")
	}
}
func assignmentNoHint(t *testing.T, ch <-chan []byte) {
	t.Helper()
	select {
	case b, ok := <-ch:
		if ok {
			t.Fatalf("unexpected assignment event: %s", b)
		}
	case <-time.After(100 * time.Millisecond):
	}
}
func assignmentGet(t *testing.T, app *App, orgID, userID, contactID uuid.UUID, explicit string) int {
	t.Helper()
	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, orgID, userID)
	testutil.SetPathParam(request, "id", contactID.String())
	if explicit != "" {
		request.RequestCtx.Request.Header.Set("X-Organization-ID", explicit)
	}
	require.NoError(t, app.Tenant((*App).GetContact)(request))
	assert.NotContains(t, string(testutil.GetResponseBody(request)), "assignment-private-fault")
	return testutil.GetResponseStatusCode(request)
}
func assignmentWrite(t *testing.T, app *App, orgID, userID, contactID uuid.UUID, body map[string]any, update bool) int {
	t.Helper()
	request := testutil.NewJSONRequest(t, body)
	testutil.SetAuthContext(request, orgID, userID)
	testutil.SetPathParam(request, "id", contactID.String())
	handler := (*App).AssignContact
	if update {
		handler = (*App).UpdateContact
	}
	require.NoError(t, app.Tenant(handler)(request))
	return testutil.GetResponseStatusCode(request)
}

func TestAssignmentAudienceRealSocketsAndFreshAuthority(t *testing.T) {
	app := authFenceTestApp(t)
	app.Config.Database.RLSEnabled = true
	org := testutil.CreateTestOrganization(t, app.DB)
	other := testutil.CreateTestOrganization(t, app.DB)
	adminRole := testutil.CreateAdminRole(t, app.DB, org.ID)
	readerRole := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "assignment-reader-"+uuid.NewString(), []string{"contacts:read"})
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&adminRole.ID))
	reader := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&readerRole.ID))
	old := testutil.CreateTestUser(t, app.DB, org.ID)
	next := testutil.CreateTestUser(t, app.DB, org.ID)
	transfer := testutil.CreateTestUser(t, app.DB, org.ID)
	observer := testutil.CreateTestUser(t, app.DB, org.ID)
	removed := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&readerRole.ID))
	inactive := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&readerRole.ID))
	foreign := testutil.CreateTestUser(t, app.DB, other.ID)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
	contact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(account.Name))
	require.NoError(t, app.DB.Model(contact).Update("assigned_user_id", old.ID).Error)
	laneDCreateTransfer(t, app.DB, contact, account.Name, &transfer.ID, models.TransferStatusActive)
	app.WSHub = ws.NewHub(app.Log)
	go app.WSHub.Run()
	address := assignmentSocketServer(t, app)
	sockets := map[string]<-chan []byte{}
	for name, u := range map[string]*models.User{"admin": admin, "reader": reader, "old": old, "next": next, "transfer": transfer, "observer": observer, "removed": removed, "inactive": inactive, "foreign": foreign} {
		sockets[name] = assignmentSocket(t, app, address, u.ID, u.OrganizationID)
	}
	// Connections were authenticated before their authority changed.
	require.NoError(t, app.DB.Where("user_id = ? AND organization_id = ?", removed.ID, org.ID).Delete(&models.UserOrganization{}).Error)
	require.NoError(t, app.DB.Model(inactive).Update("is_active", false).Error)
	require.Equal(t, 200, assignmentWrite(t, app, org.ID, admin.ID, contact.ID, map[string]any{"user_id": next.ID.String()}, false))
	for _, name := range []string{"admin", "reader", "old", "next", "transfer"} {
		assignmentWireHint(t, sockets[name], contact.ID)
	}
	for _, name := range []string{"observer", "removed", "inactive", "foreign"} {
		assignmentNoHint(t, sockets[name])
	}
	require.Equal(t, 404, assignmentGet(t, app, org.ID, old.ID, contact.ID, org.ID.String()), "old recipient gains no read right")
	require.Equal(t, 200, assignmentGet(t, app, org.ID, next.ID, contact.ID, org.ID.String()))
	require.Equal(t, 200, assignmentGet(t, app, org.ID, transfer.ID, contact.ID, org.ID.String()))
	// Revoke a read grant after the first delivered event. The existing socket
	// remains connected, but both subsequent delivery and HTTP read use fresh
	// role rows rather than its earlier authority observation.
	require.NoError(t, app.DB.Exec("DELETE FROM role_permissions WHERE custom_role_id = ?", readerRole.ID).Error)
	require.Equal(t, 404, assignmentGet(t, app, org.ID, reader.ID, contact.ID, org.ID.String()))
	// The second mutation path captures the former assignee on the same lock.
	require.Equal(t, 200, assignmentWrite(t, app, org.ID, admin.ID, contact.ID, map[string]any{"clear_assigned_agent": true}, true))
	for _, name := range []string{"admin", "next", "transfer"} {
		assignmentWireHint(t, sockets[name], contact.ID)
	}
	for _, name := range []string{"reader", "old", "observer", "removed", "inactive", "foreign"} {
		assignmentNoHint(t, sockets[name])
	}
	require.Equal(t, 404, assignmentGet(t, app, org.ID, next.ID, contact.ID, org.ID.String()))
	// A separate account remains subject to the existing generic contact API,
	// not an invented accounts:read requirement for this invalidation.
	otherAccount := testutil.CreateTestWhatsAppAccountWith(t, app.DB, org.ID, testutil.WithAccountName("assignment-"+uuid.NewString()))
	otherContact := testutil.CreateTestContactWith(t, app.DB, org.ID, testutil.WithContactAccount(otherAccount.Name))
	require.Equal(t, 200, assignmentGet(t, app, org.ID, admin.ID, otherContact.ID, org.ID.String()))
}

func assignmentFaultDB(t *testing.T, healthy *gorm.DB, table string) (*gorm.DB, *int) {
	t.Helper()
	pool, err := healthy.DB()
	require.NoError(t, err)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: healthy.Logger})
	require.NoError(t, err)
	hits := new(int)
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:assignment-private-fault", func(tx *gorm.DB) {
		if tx.Statement.Table == table {
			*hits++
			tx.AddError(errors.New("assignment-private-fault"))
		}
	}))
	return db, hits
}
func TestGetContactAuthorityFailuresAreUnavailableNotAbsent(t *testing.T) {
	for _, table := range []string{"users", "user_organizations", "reseller_members", "custom_roles", "permissions", "contacts"} {
		t.Run(table, func(t *testing.T) {
			app := authFenceTestApp(t)
			app.Config.Database.RLSEnabled = true
			org := testutil.CreateTestOrganization(t, app.DB)
			role := testutil.CreateTestRoleWithKeys(t, app.DB, org.ID, "checked-reader-"+uuid.NewString(), []string{"contacts:read"})
			user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
			contact := testutil.CreateTestContact(t, app.DB, org.ID)
			healthy := app.DB
			fault, hits := assignmentFaultDB(t, healthy, table)
			app.DB = fault
			require.Equal(t, 503, assignmentGet(t, app, org.ID, user.ID, contact.ID, ""))
			require.Positive(t, *hits)
			require.NoError(t, healthy.Exec("SELECT 1").Error)
			app.DB = healthy
			require.Equal(t, 200, assignmentGet(t, app, org.ID, user.ID, contact.ID, ""))
		})
	}
}
func TestGetContactExactScopeAndConfirmedAbsence(t *testing.T) {
	app := authFenceTestApp(t)
	app.Config.Database.RLSEnabled = true
	org := testutil.CreateTestOrganization(t, app.DB)
	other := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	require.NoError(t, app.DB.Model(contact).Update("assigned_user_id", user.ID).Error)
	require.Equal(t, 200, assignmentGet(t, app, org.ID, user.ID, contact.ID, org.ID.String()))
	require.Equal(t, 503, assignmentGet(t, app, org.ID, user.ID, contact.ID, other.ID.String()), "selected-org fallback is not confirmed absence")
	require.Equal(t, 400, assignmentGet(t, app, org.ID, user.ID, contact.ID, "malformed"))
	require.Equal(t, 400, assignmentGet(t, app, org.ID, user.ID, contact.ID, uuid.Nil.String()))
	require.Equal(t, 404, assignmentGet(t, app, org.ID, user.ID, uuid.New(), org.ID.String()))
	require.NoError(t, app.DB.Model(contact).Update("assigned_user_id", nil).Error)
	require.Equal(t, 404, assignmentGet(t, app, org.ID, user.ID, contact.ID, org.ID.String()))
	selectedContact := testutil.CreateTestContact(t, app.DB, other.ID)
	require.NoError(t, app.DB.Model(selectedContact).Update("assigned_user_id", user.ID).Error)
	require.NoError(t, app.DB.Create(&models.UserOrganization{
		BaseModel: models.BaseModel{ID: uuid.New()}, UserID: user.ID, OrganizationID: other.ID,
	}).Error)
	require.Equal(t, 200, assignmentGet(t, app, org.ID, user.ID, selectedContact.ID, other.ID.String()))
	require.NoError(t, app.DB.Where("user_id = ? AND organization_id = ?", user.ID, other.ID).Delete(&models.UserOrganization{}).Error)
	require.Equal(t, 503, assignmentGet(t, app, org.ID, user.ID, selectedContact.ID, other.ID.String()), "revoked selected workspace must not fall back and invalidate the wrong scope")
}
func TestAssignmentAudienceAfterCommitRollbackAndNoNestedCheckout(t *testing.T) {
	app := authFenceTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	role := testutil.CreateAdminRole(t, app.DB, org.ID)
	admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
	next := testutil.CreateTestUser(t, app.DB, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	app.DB = laneDOneConnectionDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	app.DB = app.DB.WithContext(ctx)
	hub, client := laneDHub(t, org.ID, admin.ID)
	app.WSHub = hub
	var callbacks []func()
	rollback := errors.New("intentional rollback")
	err := app.DB.Transaction(func(tx *gorm.DB) error {
		scoped := app.scopedApp(tx, org.ID)
		request := testutil.NewJSONRequest(t, map[string]any{"user_id": next.ID.String()})
		testutil.SetAuthContext(request, org.ID, admin.ID)
		testutil.SetPathParam(request, "id", contact.ID.String())
		require.NoError(t, scoped.AssignContact(request))
		require.Equal(t, 200, testutil.GetResponseStatusCode(request))
		select {
		case <-client.SendChan():
			t.Fatal("event before commit")
		default:
		}
		callbacks = scoped.takeAfterCommit()
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	require.Len(t, callbacks, 1)
	// A rollback owner discards callbacks; the next successful transaction owns
	// a distinct callback and must not wait for a second pooled connection.
	var stored models.Contact
	require.NoError(t, app.DB.First(&stored, "id = ?", contact.ID).Error)
	assert.Nil(t, stored.AssignedUserID)
	callbacks = nil
	require.NoError(t, app.DB.Transaction(func(tx *gorm.DB) error {
		scoped := app.scopedApp(tx, org.ID)
		request := testutil.NewJSONRequest(t, map[string]any{"user_id": next.ID.String()})
		testutil.SetAuthContext(request, org.ID, admin.ID)
		testutil.SetPathParam(request, "id", contact.ID.String())
		require.NoError(t, scoped.AssignContact(request))
		require.Equal(t, 200, testutil.GetResponseStatusCode(request))
		callbacks = scoped.takeAfterCommit()
		return nil
	}))
	app.runAfterCommit(callbacks)
	laneDExpectContactUpdate(t, client, contact.ID, "assignment")
}
func TestAssignmentAudienceFailureDoesNotBroadenOrUndoCommittedWrite(t *testing.T) {
	for _, table := range []string{"agent_transfers", "permissions"} {
		t.Run(table, func(t *testing.T) {
			app := authFenceTestApp(t)
			org := testutil.CreateTestOrganization(t, app.DB)
			role := testutil.CreateAdminRole(t, app.DB, org.ID)
			admin := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
			next := testutil.CreateTestUser(t, app.DB, org.ID)
			contact := testutil.CreateTestContact(t, app.DB, org.ID)
			hub, client := laneDHub(t, org.ID, admin.ID)
			app.WSHub = hub
			var callbacks []func()
			require.NoError(t, app.DB.Transaction(func(tx *gorm.DB) error {
				scoped := app.scopedApp(tx, org.ID)
				request := testutil.NewJSONRequest(t, map[string]any{"user_id": next.ID.String()})
				testutil.SetAuthContext(request, org.ID, admin.ID)
				testutil.SetPathParam(request, "id", contact.ID.String())
				require.NoError(t, scoped.AssignContact(request))
				require.Equal(t, 200, testutil.GetResponseStatusCode(request))
				callbacks = scoped.takeAfterCommit()
				return nil
			}))
			healthy := app.DB
			fault, hits := assignmentFaultDB(t, healthy, table)
			app.DB = fault
			app.runAfterCommit(callbacks)
			require.Positive(t, *hits)
			select {
			case b := <-client.SendChan():
				t.Fatalf("query failure must emit no partial audience: %s", b)
			case <-time.After(100 * time.Millisecond):
			}
			var stored models.Contact
			require.NoError(t, healthy.First(&stored, "id = ?", contact.ID).Error)
			require.NotNil(t, stored.AssignedUserID)
			assert.Equal(t, next.ID, *stored.AssignedUserID)

		})
	}
}

func TestAssignmentAudienceWaitHasOneBoundAndPoolRecovers(t *testing.T) {
	app := authFenceTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	contact := testutil.CreateTestContact(t, app.DB, org.ID)
	app.DB = laneDOneConnectionDB(t)
	hub, client := laneDHub(t, org.ID, user.ID)
	app.WSHub = hub
	pool, err := app.DB.DB()
	require.NoError(t, err)
	held, err := pool.Conn(context.Background())
	require.NoError(t, err)
	defer held.Close()
	start := time.Now()
	app.broadcastAssignmentUpdate(org.ID, contact.ID, user.ID)
	require.GreaterOrEqual(t, time.Since(start), 4*time.Second)
	require.Less(t, time.Since(start), 8*time.Second)
	require.NoError(t, held.Close())
	require.NoError(t, app.DB.Exec("SELECT 1").Error)
	select {
	case b := <-client.SendChan():
		t.Fatalf("pool timeout published: %s", b)
	default:
	}
}
func TestAssignmentAudienceRejectsOverLimitWithoutPartialResult(t *testing.T) {
	app := authFenceTestApp(t)
	ids := make([]uuid.UUID, assignmentAudienceLimit+1)
	_, err := app.assignmentHintAudience(uuid.New(), uuid.New(), uuid.Nil, ids)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "bound"))
}
