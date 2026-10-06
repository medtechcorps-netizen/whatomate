package handlers

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slaAfterCommitFixture opens tenant transactions the way the RLS processor
// does, on the shared schema, with one active transfer per call to transfer.
func slaAfterCommitFixture(t *testing.T) (
	*App,
	*models.Organization,
	func(models.SLATracking) *models.AgentTransfer,
	func() []slaNoticeDelivery,
) {
	t.Helper()
	app := newSLATestApp(t)
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	organization := testutil.CreateTestOrganization(t, app.DB)
	account := testutil.CreateTestWhatsAppAccount(t, app.DB, organization.ID)
	deliveries := slaNoticeProvider(t, app, app.DB)
	transfer := func(sla models.SLATracking) *models.AgentTransfer {
		contact := testutil.CreateTestContactWith(
			t,
			app.DB,
			organization.ID,
			testutil.WithContactAccount(account.Name),
			testutil.WithPhoneNumber("60"+testutil.NewTestGraphObjectID()[:10]),
		)
		created := &models.AgentTransfer{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			OrganizationID:  organization.ID,
			ContactID:       contact.ID,
			WhatsAppAccount: account.Name,
			PhoneNumber:     contact.PhoneNumber,
			Status:          models.TransferStatusActive,
			Source:          models.TransferSourceManual,
			TransferredAt:   time.Now().UTC().Add(-2 * time.Hour),
			SLA:             sla,
		}
		require.NoError(t, app.DB.Create(created).Error)
		return created
	}
	return app, organization, transfer, deliveries
}

func countSLATestOutgoing(t *testing.T, app *App, transfer *models.AgentTransfer) int64 {
	t.Helper()
	var outgoing int64
	require.NoError(t, app.DB.Model(&models.Message{}).Where(
		"organization_id = ? AND contact_id = ? AND direction = ?",
		transfer.OrganizationID,
		transfer.ContactID,
		models.DirectionOutgoing,
	).Count(&outgoing).Error)
	return outgoing
}

// A customer notice belongs to the transfer transition that produced it. If
// the tick's tenant transaction rolls back, the transition never happened and
// the customer must not have been told that it did.
func TestSLANoticesAreNotSentWhenTheTickRollsBack(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	for _, kind := range []string{"auto_close", "escalation"} {
		t.Run(kind, func(t *testing.T) {
			app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
			settings := models.ChatbotSettings{
				OrganizationID: organization.ID,
				SLA: models.SLAConfig{
					Enabled:           true,
					AutoCloseHours:    2,
					AutoCloseMessage:  "Synthetic rolled-back auto-close",
					EscalationMinutes: 30,
					WarningMessage:    "Synthetic rolled-back warning",
				},
			}
			sla := models.SLATracking{ExpiresAt: &past}
			if kind == "escalation" {
				sla = models.SLATracking{EscalationAt: &past}
			}
			transfer := createTransfer(sla)

			errRollback := errors.New("synthetic SLA tick rollback")
			started := time.Now()
			err := app.WithTenantApp(organization.ID, func(scoped *App) error {
				processor := NewSLAProcessor(scoped, time.Minute)
				if kind == "auto_close" {
					processor.autoCloseExpiredTransfers(organization.ID, settings, time.Now())
				} else {
					processor.escalateTransfers(organization.ID, settings, time.Now())
				}
				return errRollback
			})
			require.ErrorIs(t, err, errRollback)
			assert.Less(t, time.Since(started), 10*time.Second)
			assert.Empty(t, deliveries(), "a rolled-back SLA transition must not reach the customer")
			assert.Zero(t, countSLATestOutgoing(t, app, transfer))

			var stored models.AgentTransfer
			require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
			assert.Equal(t, models.TransferStatusActive, stored.Status)
			assert.Zero(t, stored.SLA.EscalationLevel)
		})
	}
}

// Every server instance runs the SLA processor. Two ticks that select the same
// level-0 transfer must escalate it, and warn the customer, exactly once.
func TestSLAEscalationWarnsOnceAcrossConcurrentTicks(t *testing.T) {
	app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
	past := time.Now().UTC().Add(-time.Hour)
	transfer := createTransfer(models.SLATracking{EscalationAt: &past})
	settings := models.ChatbotSettings{
		OrganizationID: organization.ID,
		SLA: models.SLAConfig{
			Enabled:           true,
			EscalationMinutes: 30,
			WarningMessage:    "Synthetic concurrent warning",
		},
	}

	// Hold the transfer row so both ticks select it at level 0 and then queue
	// on their escalation UPDATE.
	blocker := app.DB.Begin()
	require.NoError(t, blocker.Error)
	t.Cleanup(func() { _ = blocker.Rollback().Error })
	require.NoError(t, blocker.Exec(
		"SELECT id FROM agent_transfers WHERE id = ? FOR UPDATE",
		transfer.ID,
	).Error)

	const ticks = 2
	done := make(chan error, ticks)
	for range ticks {
		go func() {
			done <- app.WithTenantApp(organization.ID, func(scoped *App) error {
				NewSLAProcessor(scoped, time.Minute).escalateTransfers(organization.ID, settings, time.Now())
				return nil
			})
		}()
	}
	// The second UPDATE queues behind the first one's tuple lock rather than
	// directly behind the blocker, so count every waiting transfer UPDATE.
	require.Eventually(t, func() bool {
		var waiting int64
		return app.DB.Raw(
			`SELECT COUNT(*) FROM pg_catalog.pg_stat_activity
			  WHERE datname = pg_catalog.current_database()
			    AND wait_event_type = 'Lock'
			    AND query LIKE 'UPDATE "agent_transfers"%'`,
		).Scan(&waiting).Error == nil && waiting == ticks
	}, 10*time.Second, 10*time.Millisecond, "both ticks must reach the escalation UPDATE")
	require.NoError(t, blocker.Rollback().Error)
	for range ticks {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(20 * time.Second):
			t.Fatal("SLA tick did not finish")
		}
	}

	require.Len(t, deliveries(), 1, "the losing tick must not warn the customer again")
	assert.Equal(t, 1, deliveries()[0].EscalationLevel)
	assert.EqualValues(t, 1, countSLATestOutgoing(t, app, transfer))
	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
	assert.Equal(t, 1, stored.SLA.EscalationLevel)
}

// A direct send from inside a tenant transaction would wait on whatever that
// transaction holds (here the organization policy fence) until the 30 s send
// timeout. It is refused instead; callers queue it with sendSLATextAfterCommit.
func TestSLATextRefusesToSendInsideCallerTransaction(t *testing.T) {
	app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
	transfer := createTransfer(models.SLATracking{})

	started := time.Now()
	require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
		if err := database.LockOrganizationPolicyScope(scoped.DB, organization.ID); err != nil {
			return err
		}
		NewSLAProcessor(scoped, time.Minute).sendSLATextToCustomer(*transfer, "Synthetic SLA notice", "Synthetic refused notice")
		return nil
	}))
	assert.Less(t, time.Since(started), 10*time.Second)
	assert.Empty(t, deliveries())
	assert.Zero(t, countSLATestOutgoing(t, app, transfer))
}

// The same notice queued for after commit is delivered once the transaction
// holding the fence has committed.
func TestSLATextAfterCommitDeliversOnceAfterTheFenceIsReleased(t *testing.T) {
	app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
	transfer := createTransfer(models.SLATracking{})

	require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
		if err := database.LockOrganizationPolicyScope(scoped.DB, organization.ID); err != nil {
			return err
		}
		NewSLAProcessor(scoped, time.Minute).sendSLATextAfterCommit(*transfer, "Synthetic SLA notice", "Synthetic queued notice", nil)
		return nil
	}))
	require.Len(t, deliveries(), 1)
	assert.Equal(t, "Synthetic queued notice", deliveries()[0].Body)
	assert.EqualValues(t, 1, countSLATestOutgoing(t, app, transfer))
}
