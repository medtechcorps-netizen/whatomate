package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// This uses the real PostgreSQL driver and real Tenant wrapper. The fixture's
// database owner is not a restricted runtime role; this tests transaction
// cancellation/recovery, not RLS policy enforcement under that role.
func TestContactListStateDriverCancellationIsRetryable(t *testing.T) {
	for _, rls := range []bool{true, false} {
		for _, cause := range []string{"deadline", "cancel"} {
			t.Run(fmt.Sprintf("rls_%t/%s", rls, cause), func(t *testing.T) {
				db := testutil.SetupTestDB(t)
				org := testutil.CreateTestOrganization(t, db)
				role := testutil.CreateAdminRole(t, db, org.ID)
				reader := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
				account := testutil.CreateTestWhatsAppAccount(t, db, org.ID)
				contact := testutil.CreateTestContactWith(t, db, org.ID, testutil.WithContactAccount(account.Name))
				message := laneDCreateMessage(t, db, org.ID, account.Name, contact.ID, models.DirectionIncoming, models.MessageStatusDelivered)
				laneDCreateTransfer(t, db, contact, account.Name, &reader.ID, models.TransferStatusActive)
				laneDSetManualUnread(t, db, contact, nil)

				locker := db.Begin()
				require.NoError(t, locker.Error)
				defer locker.Rollback()
				require.NoError(t, locker.Exec("LOCK TABLE agent_transfers IN ACCESS EXCLUSIVE MODE").Error)

				request := testutil.NewGETRequest(t)
				testutil.SetAuthContext(request, org.ID, reader.ID)
				requestCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				pid := make(chan int, 1)
				done := make(chan error, 1)
				var committed atomic.Bool
				joined := false
				defer func() {
					cancel()
					_ = locker.Rollback().Error
					if !joined {
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Error("contact list handler did not stop after cancellation and lock release")
						}
					}
				}()
				go func() {
					// Pin a connection so pg_locks identifies this exact request in
					// both the outer-transaction and non-RLS paths.
					done <- db.WithContext(requestCtx).Connection(func(conn *gorm.DB) error {
						var backendPID int
						if err := conn.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
							return err
						}
						pid <- backendPID
						app := &App{DB: conn, Config: &config.Config{Database: config.DatabaseConfig{RLSEnabled: rls}}, Log: testutil.NopLogger()}
						return app.Tenant(func(scoped *App, r *fastglue.Request) error {
							scoped.afterTenantCommit(func() { committed.Store(true) })
							return scoped.ListContacts(r)
						})(request)
					})
				}()
				var backendPID int
				select {
				case backendPID = <-pid:
				case err := <-done:
					joined = true
					t.Fatalf("request ended before acquiring a connection: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("request did not acquire a connection")
				}
				// A SQL lock, not a fabricated callback error or a sleeping handler,
				// keeps the actual state query in the driver until cancellation.
				require.Eventually(t, func() bool {
					observeCtx, stop := context.WithTimeout(context.Background(), time.Second)
					defer stop()
					var waiting bool
					err := db.WithContext(observeCtx).Raw(`SELECT EXISTS (
						SELECT 1 FROM pg_locks WHERE pid = ? AND relation = 'agent_transfers'::regclass AND NOT granted
					)`, backendPID).Scan(&waiting).Error
					return err == nil && waiting
				}, 3*time.Second, 20*time.Millisecond, "request never reached the locked state query")
				if cause == "cancel" {
					cancel()
				}
				select {
				case err := <-done:
					joined = true
					require.NoError(t, err)
				case <-time.After(contactStateQueryTimeout + 3*time.Second):
					t.Fatal("bounded state query did not return")
				}
				require.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(request))
				body := string(testutil.GetResponseBody(request))
				assert.Contains(t, body, "contact_list_unavailable")
				assert.Contains(t, body, "Please retry")
				assert.NotContains(t, body, "agent_transfers")
				assert.NotContains(t, body, "SQLSTATE")
				assert.False(t, committed.Load(), "an error response must not run after-commit effects")

				// A fresh request must recover with the full state after the lock
				// is released, without the timed-out read changing message state.
				require.NoError(t, locker.Rollback().Error)
				app := &App{DB: db, Config: &config.Config{Database: config.DatabaseConfig{RLSEnabled: rls}}, Log: testutil.NopLogger()}
				recovered := testutil.NewGETRequest(t)
				testutil.SetAuthContext(recovered, org.ID, reader.ID)
				require.NoError(t, app.Tenant((*App).ListContacts)(recovered))
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(recovered))
				var envelope struct {
					Data laneDContactList `json:"data"`
				}
				require.NoError(t, json.Unmarshal(testutil.GetResponseBody(recovered), &envelope))
				row := laneDContactByID(t, envelope.Data, contact.ID)
				assert.Equal(t, 1, row.UnreadCount)
				require.NotNil(t, row.TransferAgentID)
				assert.Equal(t, reader.ID, *row.TransferAgentID)
				assert.True(t, row.ManualUnread)
				require.NoError(t, db.First(&message, "id = ?", message.ID).Error)
				assert.Equal(t, models.MessageStatusDelivered, message.Status)
			})
		}
	}
}

func TestContactListStateSQLLockTimeoutStillCommitsPartialState(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateAdminRole(t, db, org.ID)
	reader := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, org.ID)
	contact := testutil.CreateTestContactWith(t, db, org.ID, testutil.WithContactAccount(account.Name))
	laneDCreateMessage(t, db, org.ID, account.Name, contact.ID, models.DirectionIncoming, models.MessageStatusDelivered)
	laneDCreateTransfer(t, db, contact, account.Name, &reader.ID, models.TransferStatusActive)
	locker := db.Begin()
	require.NoError(t, locker.Error)
	defer locker.Rollback()
	require.NoError(t, locker.Exec("LOCK TABLE agent_transfers IN ACCESS EXCLUSIVE MODE").Error)

	app := &App{DB: db, Config: &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}, Log: testutil.NopLogger()}
	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, org.ID, reader.ID)
	committed := false
	require.NoError(t, app.Tenant(func(scoped *App, r *fastglue.Request) error {
		if err := scoped.DB.Exec("SET LOCAL lock_timeout = '200ms'").Error; err != nil {
			return err
		}
		scoped.afterTenantCommit(func() { committed = true })
		if err := scoped.ListContacts(r); err != nil {
			return err
		}
		return scoped.DB.Exec("SELECT 1").Error
	})(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	assert.True(t, committed, "a recovered optional SQL error must still commit")
	var envelope struct {
		Data laneDContactList `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &envelope))
	row := laneDContactByID(t, envelope.Data, contact.ID)
	assert.Equal(t, 1, row.UnreadCount)
	assert.Nil(t, row.TransferAgentID)
}

func TestContactWriteResponseKeepsBestEffortStateEnrichment(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateAdminRole(t, db, org.ID)
	reader := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
	account := testutil.CreateTestWhatsAppAccount(t, db, org.ID)
	contact := testutil.CreateTestContactWith(t, db, org.ID, testutil.WithContactAccount(account.Name))
	laneDCreateMessage(t, db, org.ID, account.Name, contact.ID, models.DirectionIncoming, models.MessageStatusDelivered)
	laneDCreateTransfer(t, db, contact, account.Name, &reader.ID, models.TransferStatusActive)
	locker := db.Begin()
	require.NoError(t, locker.Error)
	defer locker.Rollback()
	require.NoError(t, locker.Exec("LOCK TABLE agent_transfers IN ACCESS EXCLUSIVE MODE").Error)

	app := &App{DB: db, Config: &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}, Log: testutil.NopLogger()}
	request := testutil.NewJSONRequest(t, map[string]any{"metadata": map[string]any{"source": "edited"}})
	testutil.SetAuthContext(request, org.ID, reader.ID)
	testutil.SetPathParam(request, "id", contact.ID.String())
	require.NoError(t, app.Tenant(func(scoped *App, r *fastglue.Request) error {
		if err := scoped.DB.Exec("SET LOCAL lock_timeout = '200ms'").Error; err != nil {
			return err
		}
		return scoped.UpdateContact(r)
	})(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	var envelope struct {
		Data ContactResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(request), &envelope))
	assert.Equal(t, 1, envelope.Data.UnreadCount)
	assert.Nil(t, envelope.Data.TransferAgentID)
	require.NoError(t, db.First(contact, "id = ?", contact.ID).Error)
	assert.Equal(t, "edited", contact.Metadata["source"], "the write still commits despite optional enrichment failure")
}
