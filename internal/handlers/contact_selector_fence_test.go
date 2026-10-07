package handlers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

// Contact-selector triggers use NOWAIT on the organization. Without the
// outer policy fence these writes exhaust their retries under overlapping
// mirrors. Observe their fence queue while four bridge members hold SHARE;
// the regression fails before release if any writer still relies on retries.
func TestContactSelectorWritersQueueBehindMirrors(t *testing.T) {
	for _, rlsEnabled := range []bool{false, true} {
		for _, operation := range []string{"create", "restore", "delete", "import", "inbound"} {
			t.Run(fmt.Sprintf("%s/rls=%v", operation, rlsEnabled), func(t *testing.T) {
				app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
				org, account := createProcessorTestOrg(t, app)
				role := testutil.CreateAdminRole(t, app.DB, org.ID)
				user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithRoleID(&role.ID))
				contact := testutil.CreateTestContact(t, app.DB, org.ID)
				if operation == "restore" {
					require.NoError(t, app.DB.Delete(contact).Error)
				}
				app.Config.Database.RLSEnabled = rlsEnabled
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				var mirrors []*gorm.DB
				release := func() {
					for _, tx := range mirrors {
						_ = tx.Rollback().Error
					}
				}
				defer release()
				for range 4 {
					tx := app.DB.WithContext(ctx).Begin()
					require.NoError(t, tx.Error)
					mirrors = append(mirrors, tx)
					require.NoError(t, channelapi.LockLegacyMetaOrganization(tx, org.ID))
				}
				phone := "601" + fmt.Sprint(time.Now().UnixNano())[8:]
				if operation == "restore" {
					phone = contact.PhoneNumber
				}
				request := testutil.NewJSONRequest(t, map[string]any{"phone_number": phone, "profile_name": "Fence probe"})
				testutil.SetAuthContext(request, org.ID, user.ID)
				testutil.SetPathParam(request, "id", contact.ID.String())
				done := make(chan error, 1)
				go func() {
					var err error
					switch operation {
					case "create", "restore", "delete":

						if operation == "delete" {
							err = app.Tenant((*App).DeleteContact)(request)
						} else {
							err = app.Tenant((*App).CreateContact)(request)
						}
						if err == nil && testutil.GetResponseStatusCode(request) != fasthttp.StatusOK {
							err = fmt.Errorf("%s returned %d: %s", operation, testutil.GetResponseStatusCode(request), testutil.GetResponseBody(request))
						}
					case "import":
						err = app.WithTenantApp(org.ID, func(scoped *App) error {
							return importRecordWrite(scoped.DB, org.ID, &models.Contact{}, func(tx *gorm.DB) error {
								return tx.Create(&models.Contact{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, PhoneNumber: "601" + fmt.Sprint(time.Now().UnixNano())[8:]}).Error
							})
						})
					case "inbound":
						err = app.WithTenantApp(org.ID, func(scoped *App) error {
							_, _, err := scoped.getOrCreateInboundContact(account, "601"+fmt.Sprint(time.Now().UnixNano())[8:], "Fence probe", "synthetic-bsuid")
							return err
						})
					}
					done <- err
				}()
				consumed := false
				defer func() {
					release()
					if !consumed {
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							t.Error("contact writer did not stop")
						}
					}
				}()
				require.Eventually(t, func() bool {
					var queued bool
					return app.DB.Raw(`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_locks
       WHERE locktype = 'advisory' AND mode = 'ExclusiveLock' AND NOT granted
       AND database = (SELECT oid FROM pg_catalog.pg_database WHERE datname = current_database())
       AND (classid::int8 << 32) | objid::int8 = hashtextextended(?, 0))`,
						database.WhatsAppIdentityReviewContactSelectorFenceKey(org.ID)).Scan(&queued).Error == nil && queued
				}, 5*time.Second, 5*time.Millisecond, "selector writer must queue before its NOWAIT trigger")
				release()
				select {
				case err := <-done:
					consumed = true
					require.NoError(t, err)
				case <-time.After(10 * time.Second):
					t.Fatal("selector writer did not complete after the mirrors")
				}
			})
		}
	}
}
