package handlers

import (
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/calling"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestProcessCallStatusWebhookRejectsCrossTenantSession(t *testing.T) {
	for _, rlsEnabled := range []bool{false, true} {
		t.Run(rlsModeName(rlsEnabled), func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			requestOrg := testutil.CreateTestOrganization(t, db)
			sessionOrg := testutil.CreateTestOrganization(t, db)
			account := testutil.CreateTestWhatsAppAccount(t, db, requestOrg.ID)
			contact := testutil.CreateTestContact(t, db, sessionOrg.ID)
			callLog := createOutgoingCallWebhookTestLog(t, db, sessionOrg.ID, contact, "status-cross-tenant")

			manager := calling.NewManager(
				&config.CallingConfig{}, nil, db, nil, nil, nil, nil, testutil.NopLogger(),
			)
			session := &calling.CallSession{
				ID:             callLog.WhatsAppCallID,
				OrganizationID: sessionOrg.ID,
				ContactID:      contact.ID,
				CallLogID:      callLog.ID,
				Status:         models.CallStatusInitiating,
				Direction:      models.CallDirectionOutgoing,
				TargetPhone:    contact.PhoneNumber,
			}
			installCallWebhookTestSession(t, manager, session)

			runCallWebhookTenantMode(t, db, requestOrg.ID, rlsEnabled, manager, func(app *App) {
				app.processCallStatusWebhook(account.PhoneID, WebhookStatus{
					ID:     callLog.WhatsAppCallID,
					Status: "RINGING",
				})
			})

			require.Equal(t, models.CallStatusInitiating, session.Status,
				"a status routed through another tenant's phone number must not mutate the live session")
			var stored models.CallLog
			require.NoError(t, db.First(&stored, callLog.ID).Error)
			require.Equal(t, models.CallStatusInitiating, stored.Status)
		})
	}
}

func TestProcessCallWebhookOrphanedOutgoingEventIsTenantQualified(t *testing.T) {
	for _, rlsEnabled := range []bool{false, true} {
		t.Run(rlsModeName(rlsEnabled), func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			requestOrg := testutil.CreateTestOrganization(t, db)
			foreignOrg := testutil.CreateTestOrganization(t, db)
			account := testutil.CreateTestWhatsAppAccount(t, db, requestOrg.ID)
			contact := testutil.CreateTestContact(t, db, foreignOrg.ID)
			callLog := createOutgoingCallWebhookTestLog(t, db, foreignOrg.ID, contact, "orphan-cross-tenant")

			runCallWebhookTenantMode(t, db, requestOrg.ID, rlsEnabled, nil, func(app *App) {
				app.processCallWebhook(account.PhoneID, map[string]any{
					"id":        callLog.WhatsAppCallID,
					"event":     "terminate",
					"direction": "BUSINESS_INITIATED",
					"duration":  42,
				})
			})

			var stored models.CallLog
			require.NoError(t, db.First(&stored, callLog.ID).Error)
			require.Equal(t, models.CallStatusInitiating, stored.Status,
				"an orphan event must not find another tenant's call by global WhatsApp call ID")
			require.Nil(t, stored.EndedAt)
			require.Zero(t, stored.Duration)
		})
	}
}

func runCallWebhookTenantMode(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	rlsEnabled bool,
	manager *calling.Manager,
	fn func(*App),
) {
	t.Helper()
	newApp := func(scopedDB *gorm.DB, tenantOrgID uuid.UUID) *App {
		return &App{
			Config: &config.Config{
				App:      config.AppConfig{EncryptionKey: "test-encryption-key-for-call-webhooks"},
				Database: config.DatabaseConfig{RLSEnabled: rlsEnabled},
			},
			DB:          scopedDB,
			Log:         testutil.NopLogger(),
			CallManager: manager,
			tenantOrgID: tenantOrgID,
		}
	}

	if !rlsEnabled {
		fn(newApp(db, uuid.Nil))
		return
	}

	require.NoError(t, database.WithTenant(db, organizationID, func(tx *gorm.DB) error {
		fn(newApp(tx, organizationID))
		return nil
	}))
}

func createOutgoingCallWebhookTestLog(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	contact *models.Contact,
	prefix string,
) *models.CallLog {
	t.Helper()
	startedAt := time.Now().Add(-time.Minute)
	callLog := &models.CallLog{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organizationID,
		WhatsAppAccount: "test-account",
		ContactID:       contact.ID,
		WhatsAppCallID:  prefix + "-" + uuid.NewString(),
		CallerPhone:     contact.PhoneNumber,
		Direction:       models.CallDirectionOutgoing,
		Status:          models.CallStatusInitiating,
		StartedAt:       &startedAt,
	}
	require.NoError(t, db.Create(callLog).Error)
	return callLog
}

// Manager intentionally has no public session-registration API. This test-only
// helper installs a synthetic session so the handler boundary can be exercised
// without initiating a real WebRTC call.
func installCallWebhookTestSession(t *testing.T, manager *calling.Manager, session *calling.CallSession) {
	t.Helper()
	sessions := reflect.ValueOf(manager).Elem().FieldByName("sessions")
	require.True(t, sessions.IsValid())
	writable := reflect.NewAt(sessions.Type(), unsafe.Pointer(sessions.UnsafeAddr())).Elem()
	writable.SetMapIndex(reflect.ValueOf(session.ID), reflect.ValueOf(session))
	require.Same(t, session, manager.GetSession(session.ID))
}

func rlsModeName(enabled bool) string {
	if enabled {
		return "rls_enabled"
	}
	return "rls_disabled"
}
