package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func slaNoticeStateSettings(organizationID uuid.UUID) models.ChatbotSettings {
	return models.ChatbotSettings{
		OrganizationID: organizationID,
		SLA: models.SLAConfig{
			Enabled:           true,
			AutoCloseHours:    2,
			AutoCloseMessage:  "Synthetic state auto-close",
			EscalationMinutes: 30,
			WarningMessage:    "Synthetic state warning",
		},
	}
}

// requireSLATransferUpdateWaiting waits until n statements are queued on an
// agent_transfers row lock.
func requireSLATransferUpdateWaiting(t *testing.T, db *gorm.DB, n int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int64
		return db.Raw(
			`SELECT COUNT(*) FROM pg_catalog.pg_stat_activity
			  WHERE datname = pg_catalog.current_database()
			    AND wait_event_type = 'Lock'
			    AND query LIKE 'UPDATE "agent_transfers"%'`,
		).Scan(&waiting).Error == nil && waiting == n
	}, 10*time.Second, 10*time.Millisecond, "expected %d waiting transfer UPDATEs", n)
}

// A transfer resumed after the tick selected it makes the guarded transition
// lose. No customer notice may go out for a transition that never happened.
func TestSLATransitionLostToConcurrentResumeSendsNoNotice(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	for _, kind := range []string{"auto_close", "escalation"} {
		for _, rls := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/rls=%v", kind, rls), func(t *testing.T) {
				app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
				app.Config.Database.RLSEnabled = rls
				sla := models.SLATracking{ExpiresAt: &past}
				if kind == "escalation" {
					sla = models.SLATracking{EscalationAt: &past}
				}
				transfer := createTransfer(sla)
				settings := slaNoticeStateSettings(organization.ID)

				// The resume holds the row until the tick's guarded UPDATE waits on it.
				resume := app.DB.Begin()
				require.NoError(t, resume.Error)
				t.Cleanup(func() { _ = resume.Rollback().Error })
				require.NoError(t, resume.Exec(
					"UPDATE agent_transfers SET status = ? WHERE id = ?",
					models.TransferStatusResumed,
					transfer.ID,
				).Error)
				done := make(chan error, 1)
				go func() {
					done <- app.WithTenantApp(organization.ID, func(scoped *App) error {
						processor := NewSLAProcessor(scoped, time.Minute)
						if kind == "auto_close" {
							processor.autoCloseExpiredTransfers(organization.ID, settings, time.Now())
						} else {
							processor.escalateTransfers(organization.ID, settings, time.Now())
						}
						return nil
					})
				}()
				requireSLATransferUpdateWaiting(t, app.DB, 1)
				require.NoError(t, resume.Commit().Error)
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(20 * time.Second):
					t.Fatal("SLA tick did not finish")
				}

				assert.Empty(t, deliveries(), "a transition lost to a resume must not notify the customer")
				var stored models.AgentTransfer
				require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
				assert.Equal(t, models.TransferStatusResumed, stored.Status)
				assert.Zero(t, stored.SLA.EscalationLevel)
			})
		}
	}
}

// slaPassWithFirstNoticeHeld runs one SLA pass over transfers a and b while
// the pass's first customer notice is held at the provider. It calls
// meanwhile with the transfer whose notice is still queued, releases the
// provider, and returns every delivery plus that other transfer.
func slaPassWithFirstNoticeHeld(
	t *testing.T,
	app *App,
	organizationID uuid.UUID,
	settings models.ChatbotSettings,
	a, b *models.AgentTransfer,
	meanwhile func(other *models.AgentTransfer),
) ([]slaNoticeDelivery, *models.AgentTransfer) {
	t.Helper()
	first := make(chan string, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	deliveries := slaNoticeProviderWithHook(t, app, app.DB, func(to string) {
		if calls.Add(1) != 1 {
			return
		}
		first <- to
		select {
		case <-release:
		case <-time.After(20 * time.Second):
		}
	})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	done := make(chan error, 1)
	go func() {
		done <- app.WithTenantApp(organizationID, func(scoped *App) error {
			NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, time.Now())
			return nil
		})
	}()
	var firstPhone string
	select {
	case firstPhone = <-first:
	case <-time.After(15 * time.Second):
		t.Fatal("no notice reached the provider")
	}
	other := b
	if firstPhone == b.PhoneNumber {
		other = a
	}
	// The pass has committed, so nothing meanwhile does waits on it.
	meanwhile(other)
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("SLA tick did not finish")
	}
	return deliveries(), other
}

// Notices go out one after another once the tick commits, each bounded only
// by the provider. A transfer that moves on while an earlier notice is still
// in flight must not get its now-stale notice: a resumed transfer, or one an
// agent has already answered, gets no "still waiting" warning, and a contact
// back in a human handover gets no auto-close.
func TestSLADeferredNoticeSkipsTransferThatMovedOn(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	for _, change := range []string{"new_handover", "resumed", "agent_replied"} {
		kind := "escalation"
		if change == "new_handover" {
			kind = "auto_close"
		}
		for _, rls := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%s/rls=%v", kind, change, rls), func(t *testing.T) {
				app, organization, createTransfer, _ := slaAfterCommitFixture(t)
				app.Config.Database.RLSEnabled = rls
				sla := models.SLATracking{ExpiresAt: &past}
				if kind == "escalation" {
					sla = models.SLATracking{EscalationAt: &past}
				}
				a := createTransfer(sla)
				b := createTransfer(sla)
				settings := slaNoticeStateSettings(organization.ID)
				got, other := slaPassWithFirstNoticeHeld(t, app, organization.ID, settings, a, b, func(other *models.AgentTransfer) {
					switch change {
					case "resumed":
						result := app.DB.Model(&models.AgentTransfer{}).
							Where("id = ? AND status = ?", other.ID, models.TransferStatusActive).
							Update("status", models.TransferStatusResumed)
						require.NoError(t, result.Error)
						require.EqualValues(t, 1, result.RowsAffected)
					case "agent_replied":
						agent := testutil.CreateTestUser(t, app.DB, organization.ID)
						require.NoError(t, app.DB.Model(&models.AgentTransfer{}).
							Where("id = ?", other.ID).Update("agent_id", agent.ID).Error)
						createTestAgentMessage(t, app, organization.ID, other.ContactID, agent.ID,
							other.WhatsAppAccount, time.Now())
					default:
						require.NoError(t, app.DB.Create(&models.AgentTransfer{
							BaseModel:       models.BaseModel{ID: uuid.New()},
							OrganizationID:  organization.ID,
							ContactID:       other.ContactID,
							WhatsAppAccount: other.WhatsAppAccount,
							PhoneNumber:     other.PhoneNumber,
							Status:          models.TransferStatusActive,
							Source:          models.TransferSourceManual,
							TransferredAt:   time.Now().UTC(),
						}).Error)
					}
				})

				require.Len(t, got, 1, "the moved-on transfer must not get its stale notice: %+v", got)
				assert.NotEqual(t, other.PhoneNumber, got[0].To)
				var notices int64
				require.NoError(t, app.DB.Model(&models.Message{}).Where(
					"organization_id = ? AND contact_id = ? AND direction = ? AND content IN ?",
					organization.ID,
					other.ContactID,
					models.DirectionOutgoing,
					[]string{settings.SLA.AutoCloseMessage, settings.SLA.WarningMessage},
				).Count(&notices).Error)
				assert.Zero(t, notices)
			})
		}
	}
}

// Only a real reply since this escalation makes the "still waiting" warning
// untrue. A failed agent text never reached the customer; an API-key
// integration sends templates under its key owner's user id; and a reply that
// came before the escalation was already answered by it.
func TestSLAWarningIsNotSuppressedByNonReplies(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	outgoing := func(t *testing.T, app *App, other *models.AgentTransfer, messageType models.MessageType, status models.MessageStatus, at time.Time) {
		t.Helper()
		user := testutil.CreateTestUser(t, app.DB, other.OrganizationID)
		require.NoError(t, app.DB.Create(&models.Message{
			BaseModel:       models.BaseModel{ID: uuid.New(), CreatedAt: at},
			OrganizationID:  other.OrganizationID,
			ContactID:       other.ContactID,
			WhatsAppAccount: other.WhatsAppAccount,
			Direction:       models.DirectionOutgoing,
			MessageType:     messageType,
			Content:         "synthetic non-reply",
			SentByUserID:    &user.ID,
			Status:          status,
		}).Error)
	}
	for _, kind := range []string{"failed_agent_text", "api_template"} {
		t.Run(kind, func(t *testing.T) {
			app, organization, createTransfer, _ := slaAfterCommitFixture(t)
			a := createTransfer(models.SLATracking{EscalationAt: &past})
			b := createTransfer(models.SLATracking{EscalationAt: &past})
			settings := slaNoticeStateSettings(organization.ID)
			got, other := slaPassWithFirstNoticeHeld(t, app, organization.ID, settings, a, b, func(other *models.AgentTransfer) {
				if kind == "failed_agent_text" {
					outgoing(t, app, other, models.MessageTypeText, models.MessageStatusFailed, time.Now())
				} else {
					outgoing(t, app, other, models.MessageTypeTemplate, models.MessageStatusSent, time.Now())
				}
			})
			delivered := map[string]bool{}
			for _, delivery := range got {
				delivered[delivery.To] = true
			}
			assert.True(t, delivered[other.PhoneNumber], "%s must not suppress the warning: %+v", kind, got)
			assert.Len(t, got, 2)
		})
	}
	t.Run("reply_before_this_escalation", func(t *testing.T) {
		app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
		transfer := createTransfer(models.SLATracking{EscalationAt: &past})
		// The tick started a minute ago; earlier organizations' passes took
		// that long. The reply came before this transfer's escalation.
		tickStart := time.Now().Add(-time.Minute)
		outgoing(t, app, transfer, models.MessageTypeText, models.MessageStatusSent, time.Now().Add(-30*time.Second))
		settings := slaNoticeStateSettings(organization.ID)
		require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
			NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, tickStart)
			return nil
		}))
		var stored models.AgentTransfer
		require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
		assert.Equal(t, 1, stored.SLA.EscalationLevel)
		require.NotNil(t, stored.SLA.EscalatedAt)
		assert.True(t, stored.SLA.EscalatedAt.After(time.Now().Add(-20*time.Second)),
			"escalated_at is the escalation's own time, not the tick's start")
		assert.Len(t, deliveries(), 1, "a reply before the escalation does not answer it")
	})
}

// A panic while handling one queued notice (its re-check, the recipient
// lookup) must not drop the other notices of the pass: their transitions have
// committed and no later tick would send them.
func TestSLAPanicInOneNoticeKeepsTheRestOfThePass(t *testing.T) {
	app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
	first := createTransfer(models.SLATracking{})
	second := createTransfer(models.SLATracking{})
	panicking := func(*gorm.DB, models.AgentTransfer) (bool, error) {
		panic("synthetic re-check panic")
	}
	require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
		NewSLAProcessor(scoped, time.Minute).sendSLANoticesAfterCommit([]slaNotice{
			{transfer: *first, label: "Synthetic SLA notice", message: "Synthetic notice one", stillCurrent: panicking},
			{transfer: *second, label: "Synthetic SLA notice", message: "Synthetic notice two"},
		})
		return nil
	}))
	got := deliveries()
	require.Len(t, got, 1, "the second notice must survive the first one's panic")
	assert.Equal(t, second.PhoneNumber, got[0].To)
	assert.Equal(t, "Synthetic notice two", got[0].Body)
}

// Rows that fail on every tick must not use up the per-pass cap. The routine
// cause is a contact deleted during the handover: such a transfer is closed
// without a notice. Any other permanently failing row (here a trigger) is
// skipped, and the transfer behind 30 of them is still handled, and its
// customer told, in the same tick.
func TestSLAFailingRowsDoNotStarveTheTransfersBehindThem(t *testing.T) {
	for _, tc := range []struct{ step, poison string }{
		{"auto_close", "deleted_contact"},
		{"auto_close", "trigger"},
		{"escalation", "deleted_contact"},
		{"escalation", "trigger"},
	} {
		t.Run(tc.step+"/"+tc.poison, func(t *testing.T) {
			app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
			deadline := func(at time.Time) models.SLATracking {
				if tc.step == "escalation" {
					return models.SLATracking{EscalationAt: &at}
				}
				return models.SLATracking{ExpiresAt: &at}
			}
			oldest := time.Now().UTC().Add(-48 * time.Hour)
			poisoned := make([]*models.AgentTransfer, 0, slaTransfersPerPass)
			for i := range slaTransfersPerPass {
				transfer := createTransfer(deadline(oldest.Add(time.Duration(i) * time.Minute)))
				poisoned = append(poisoned, transfer)
				if tc.poison == "deleted_contact" {
					require.NoError(t, app.DB.Delete(&models.Contact{}, "id = ?", transfer.ContactID).Error)
				} else {
					require.NoError(t, app.DB.Model(transfer).Update("notes", "synthetic poisoned row").Error)
				}
			}
			normal := createTransfer(deadline(time.Now().UTC().Add(-time.Hour)))
			if tc.poison == "trigger" {
				suffix := uuid.NewString()[:8]
				function := "sla_poisoned_row_" + suffix
				trigger := "sla_poisoned_row_trigger_" + suffix
				require.NoError(t, app.DB.Exec(fmt.Sprintf(`
					CREATE FUNCTION %s() RETURNS trigger
					LANGUAGE plpgsql
					AS $$
					BEGIN
						IF OLD.notes = 'synthetic poisoned row' THEN
							RAISE EXCEPTION 'synthetic poisoned SLA row';
						END IF;
						RETURN NEW;
					END;
					$$`, function)).Error)
				require.NoError(t, app.DB.Exec(fmt.Sprintf(
					"CREATE TRIGGER %s BEFORE UPDATE ON agent_transfers FOR EACH ROW EXECUTE FUNCTION %s()",
					trigger, function,
				)).Error)
				t.Cleanup(func() {
					_ = app.DB.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON agent_transfers", trigger)).Error
					_ = app.DB.Exec(fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", function)).Error
				})
			}

			settings := slaNoticeStateSettings(organization.ID)
			require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
				NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, time.Now())
				return nil
			}))

			var stored models.AgentTransfer
			require.NoError(t, app.DB.First(&stored, "id = ?", normal.ID).Error)
			wantNotice := settings.SLA.AutoCloseMessage
			if tc.step == "escalation" {
				assert.Equal(t, 1, stored.SLA.EscalationLevel)
				wantNotice = settings.SLA.WarningMessage
			} else {
				assert.Equal(t, models.TransferStatusExpired, stored.Status)
			}
			got := deliveries()
			require.Len(t, got, 1, "only the transfer behind the failing rows is notified: %+v", got)
			assert.Equal(t, normal.PhoneNumber, got[0].To)
			assert.Equal(t, wantNotice, got[0].Body)

			for _, transfer := range poisoned {
				var row models.AgentTransfer
				require.NoError(t, app.DB.First(&row, "id = ?", transfer.ID).Error)
				switch {
				case tc.poison == "deleted_contact" && tc.step == "auto_close":
					assert.Equal(t, models.TransferStatusExpired, row.Status, "closed without a notice")
					assert.Contains(t, row.Notes, "contact deleted")
				default:
					assert.Equal(t, models.TransferStatusActive, row.Status)
					assert.Zero(t, row.SLA.EscalationLevel)
				}
			}
		})
	}
}

type slaTransferEvent struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

// collectSLATransferEvents drains up to want agent websocket events of an SLA
// tick (expired, escalated, escalation alert), waiting at most wait.
func collectSLATransferEvents(client *websocket.Client, want int, wait time.Duration) []slaTransferEvent {
	var events []slaTransferEvent
	deadline := time.After(wait)
	for len(events) < want {
		select {
		case data := <-client.SendChan():
			var event slaTransferEvent
			if json.Unmarshal(data, &event) != nil {
				continue
			}
			switch event.Type {
			case websocket.TypeTransferExpired, websocket.TypeTransferEscalated, websocket.TypeTransferEscalation:
				events = append(events, event)
			}
		case <-deadline:
			return events
		}
	}
	return events
}

// slaTransferEvents is collectSLATransferEvents keyed by event type.
func slaTransferEvents(client *websocket.Client, want int, wait time.Duration) map[string]map[string]any {
	events := map[string]map[string]any{}
	for _, event := range collectSLATransferEvents(client, want, wait) {
		events[event.Type] = event.Payload
	}
	return events
}

// Agents are told about an expiry or escalation only once the tick has
// committed it, and the event carries the new status and level. A rolled-back
// tick tells them nothing.
func TestSLAAgentBroadcastsFollowTheCommittedTransition(t *testing.T) {
	app, organization, createTransfer, _ := slaAfterCommitFixture(t)
	client := websocket.NewClient(app.WSHub, nil, uuid.New(), organization.ID)
	app.WSHub.Register(client)
	testutil.AssertEventually(t, func() bool { return app.WSHub.GetClientCount() == 1 }, 2*time.Second, "client registers")

	past := time.Now().UTC().Add(-time.Hour)
	expired := createTransfer(models.SLATracking{ExpiresAt: &past})
	escalated := createTransfer(models.SLATracking{EscalationAt: &past})
	settings := models.ChatbotSettings{
		OrganizationID: organization.ID,
		SLA: models.SLAConfig{
			Enabled:             true,
			AutoCloseHours:      2,
			EscalationMinutes:   30,
			EscalationNotifyIDs: models.StringArray{uuid.NewString()},
		},
	}
	errRollback := errors.New("synthetic SLA tick rollback")
	tick := func(rollback bool) error {
		return app.WithTenantApp(organization.ID, func(scoped *App) error {
			processor := NewSLAProcessor(scoped, time.Minute)
			processor.autoCloseExpiredTransfers(organization.ID, settings, time.Now())
			processor.escalateTransfers(organization.ID, settings, time.Now())
			if rollback {
				return errRollback
			}
			return nil
		})
	}

	require.ErrorIs(t, tick(true), errRollback)
	assert.Empty(t, slaTransferEvents(client, 1, 300*time.Millisecond),
		"a rolled-back tick must not tell agents a transfer expired or escalated")

	require.NoError(t, tick(false))
	events := slaTransferEvents(client, 3, 5*time.Second)
	require.Len(t, events, 3, "events: %+v", events)
	expiredEvent := events[websocket.TypeTransferExpired]
	assert.Equal(t, expired.ID.String(), expiredEvent["id"])
	assert.Equal(t, string(models.TransferStatusExpired), expiredEvent["status"])
	escalatedEvent := events[websocket.TypeTransferEscalated]
	assert.Equal(t, escalated.ID.String(), escalatedEvent["id"])
	assert.Equal(t, string(models.TransferStatusActive), escalatedEvent["status"])
	assert.EqualValues(t, 1, escalatedEvent["escalation_level"])
	notifyEvent := events[websocket.TypeTransferEscalation]
	assert.Equal(t, escalated.ID.String(), notifyEvent["id"])
	assert.EqualValues(t, 1, notifyEvent["escalation_level"])
}

// Each customer notice can take up to the 30 s send timeout. Every agent event
// of the pass, including the managers' escalation alert, must already have gone
// out while the pass's first customer notice is still at the provider.
func TestSLAAgentEventsAreNotHeldBehindCustomerSends(t *testing.T) {
	for _, rls := range []bool{true, false} {
		t.Run(fmt.Sprintf("rls=%v", rls), func(t *testing.T) {
			app, organization, createTransfer, _ := slaAfterCommitFixture(t)
			app.Config.Database.RLSEnabled = rls
			client := websocket.NewClient(app.WSHub, nil, uuid.New(), organization.ID)
			app.WSHub.Register(client)
			testutil.AssertEventually(t, func() bool { return app.WSHub.GetClientCount() == 1 }, 2*time.Second, "client registers")
			sending := make(chan struct{}, 1)
			release := make(chan struct{})
			var calls atomic.Int32
			slaNoticeProviderWithHook(t, app, app.DB, func(string) {
				if calls.Add(1) != 1 {
					return
				}
				sending <- struct{}{}
				select {
				case <-release:
				case <-time.After(20 * time.Second):
				}
			})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

			past := time.Now().UTC().Add(-time.Hour)
			createTransfer(models.SLATracking{ExpiresAt: &past})
			createTransfer(models.SLATracking{ExpiresAt: &past})
			escalated := createTransfer(models.SLATracking{EscalationAt: &past})
			settings := slaNoticeStateSettings(organization.ID)
			settings.SLA.EscalationNotifyIDs = models.StringArray{uuid.NewString()}
			done := make(chan error, 1)
			go func() {
				done <- app.WithTenantApp(organization.ID, func(scoped *App) error {
					NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, time.Now())
					return nil
				})
			}()
			select {
			case <-sending:
			case <-time.After(15 * time.Second):
				t.Fatal("no customer notice reached the provider")
			}

			counts := map[string]int{}
			for _, event := range collectSLATransferEvents(client, 4, 5*time.Second) {
				counts[event.Type]++
			}
			assert.Equal(t, map[string]int{
				websocket.TypeTransferExpired:    2,
				websocket.TypeTransferEscalated:  1,
				websocket.TypeTransferEscalation: 1,
			}, counts, "agent events must not wait behind a customer send in flight")
			releaseOnce.Do(func() { close(release) })
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(20 * time.Second):
				t.Fatal("SLA tick did not finish")
			}
			var stored models.AgentTransfer
			require.NoError(t, app.DB.First(&stored, "id = ?", escalated.ID).Error)
			assert.Equal(t, 1, stored.SLA.EscalationLevel)
		})
	}
}

// slaPassSubxacts reports the subtransaction cache of the backend running scoped's
// transaction, read from another connection.
func slaPassSubxacts(t *testing.T, app *App, scoped *App) (int64, bool) {
	t.Helper()
	var subxacts struct {
		Count      int64 `gorm:"column:subxact_count"`
		Overflowed bool  `gorm:"column:subxact_overflowed"`
	}
	var pid int
	require.NoError(t, scoped.DB.Raw("SELECT pg_catalog.pg_backend_pid()").Scan(&pid).Error)
	require.NoError(t, app.DB.Raw(`
		SELECT subxact.subxact_count, subxact.subxact_overflowed
		  FROM pg_catalog.pg_stat_get_backend_idset() AS backend(id),
		       LATERAL pg_catalog.pg_stat_get_backend_subxact(backend.id) AS subxact
		 WHERE pg_catalog.pg_stat_get_backend_pid(backend.id) = ?`,
		pid,
	).Scan(&subxacts).Error)
	return subxacts.Count, subxacts.Overflowed
}

// slaRowTrigger installs a BEFORE UPDATE trigger on agent_transfers that runs
// body (PL/pgSQL) for rows whose notes equal marker. It returns a function
// that drops it early; cleanup drops it in any case.
func slaRowTrigger(t *testing.T, app *App, marker, body string) func() {
	t.Helper()
	suffix := uuid.NewString()[:8]
	function := "sla_row_" + suffix
	trigger := "sla_row_trigger_" + suffix
	require.NoError(t, app.DB.Exec(fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			IF OLD.notes = '%s' THEN
				%s
			END IF;
			RETURN NEW;
		END;
		$$`, function, marker, body)).Error)
	require.NoError(t, app.DB.Exec(fmt.Sprintf(
		"CREATE TRIGGER %s BEFORE UPDATE ON agent_transfers FOR EACH ROW EXECUTE FUNCTION %s()",
		trigger, function,
	)).Error)
	drop := func() {
		_ = app.DB.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON agent_transfers", trigger)).Error
		_ = app.DB.Exec(fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", function)).Error
	}
	t.Cleanup(drop)
	return drop
}

// slaAgentReplied assigns an agent who replied half an hour ago, so the tick
// extends the transfer's deadline instead of acting on it.
func slaAgentReplied(t *testing.T, app *App, transfer *models.AgentTransfer) {
	t.Helper()
	agent := testutil.CreateTestUser(t, app.DB, transfer.OrganizationID)
	require.NoError(t, app.DB.Model(transfer).Update("agent_id", agent.ID).Error)
	createTestAgentMessage(t, app, transfer.OrganizationID, transfer.ContactID, agent.ID,
		transfer.WhatsAppAccount, time.Now().Add(-30*time.Minute))
}

// slaProcessorPasses runs passes of one long-lived processor, the way
// processStaleTransfers does: a per-organization copy bound to the tenant scope.
func slaProcessorPasses(t *testing.T, app *App, processor *SLAProcessor, settings ...models.ChatbotSettings) func() {
	t.Helper()
	return func() {
		for _, organizationSettings := range settings {
			require.NoError(t, app.WithTenantApp(organizationSettings.OrganizationID, func(scoped *App) error {
				tick := *processor
				tick.app = scoped
				tick.processOrganizationSLA(organizationSettings, time.Now())
				return nil
			}))
		}
	}
}

// Under RLS a pass is one transaction and each written transfer a savepoint.
// After a backlog (processor downtime, a mass expiry) one pass must still stay
// inside PostgreSQL's 64-entry per-backend subtransaction cache, handling the
// oldest deadlines first and leaving the rest for the next tick.
func TestSLAPassStaysWithinTheSubtransactionCache(t *testing.T) {
	app, organization, createTransfer, _ := slaAfterCommitFixture(t)
	const backlog = slaTransfersPerPass + 5
	oldest := time.Now().UTC().Add(-3 * time.Hour)
	// Index i holds the i-th oldest deadline. Create newest first, so the
	// table's physical order is the reverse of deadline order.
	expiring := make([]*models.AgentTransfer, backlog)
	escalating := make([]*models.AgentTransfer, backlog)
	for i := backlog - 1; i >= 0; i-- {
		expiresAt := oldest.Add(time.Duration(i) * time.Minute)
		escalationAt := oldest.Add(time.Duration(i) * time.Minute)
		expiring[i] = createTransfer(models.SLATracking{ExpiresAt: &expiresAt})
		escalating[i] = createTransfer(models.SLATracking{EscalationAt: &escalationAt})
	}
	responseDeadline := oldest
	unanswered := createTransfer(models.SLATracking{ResponseDeadline: &responseDeadline})
	settings := models.ChatbotSettings{
		OrganizationID: organization.ID,
		SLA: models.SLAConfig{
			Enabled:           true,
			AutoCloseHours:    2,
			EscalationMinutes: 30,
			ResponseMinutes:   15,
		},
	}

	var subxacts struct {
		Count      int64 `gorm:"column:subxact_count"`
		Overflowed bool  `gorm:"column:subxact_overflowed"`
	}
	require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
		NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, time.Now())
		var pid int
		if err := scoped.DB.Raw("SELECT pg_catalog.pg_backend_pid()").Scan(&pid).Error; err != nil {
			return err
		}
		return app.DB.Raw(`
			SELECT subxact.subxact_count, subxact.subxact_overflowed
			  FROM pg_catalog.pg_stat_get_backend_idset() AS backend(id),
			       LATERAL pg_catalog.pg_stat_get_backend_subxact(backend.id) AS subxact
			 WHERE pg_catalog.pg_stat_get_backend_pid(backend.id) = ?`,
			pid,
		).Scan(&subxacts).Error
	}))
	assert.False(t, subxacts.Overflowed, "the pass overflowed the subtransaction cache (%d)", subxacts.Count)
	assert.EqualValues(t, 2*slaTransfersPerPass+1, subxacts.Count)

	stored := func(id uuid.UUID) models.AgentTransfer {
		var transfer models.AgentTransfer
		require.NoError(t, app.DB.First(&transfer, "id = ?", id).Error)
		return transfer
	}
	for i := range backlog {
		wantStatus, wantLevel := models.TransferStatusExpired, 1
		if i >= slaTransfersPerPass {
			wantStatus, wantLevel = models.TransferStatusActive, 0
		}
		assert.Equal(t, wantStatus, stored(expiring[i].ID).Status, "expiry %d", i)
		assert.Equal(t, wantLevel, stored(escalating[i].ID).SLA.EscalationLevel, "escalation %d", i)
	}
	assert.True(t, stored(unanswered.ID).SLA.Breached)
}

// Failed writes must not cost subtransaction IDs: each is rolled back to its
// savepoint and released. Failing rows ahead of full caps, plus the
// deleted-contact close and the breach update, stay at exactly 62.
func TestSLAPassStaysWithinTheSubtransactionCacheWhenWritesFail(t *testing.T) {
	app, organization, createTransfer, _ := slaAfterCommitFixture(t)
	const failing = 5
	slaRowTrigger(t, app, "synthetic failing row", "RAISE EXCEPTION 'synthetic failing SLA row';")
	oldest := time.Now().UTC().Add(-5 * time.Hour)
	orphanAt := oldest
	orphan := createTransfer(models.SLATracking{ExpiresAt: &orphanAt})
	require.NoError(t, app.DB.Delete(&models.Contact{}, "id = ?", orphan.ContactID).Error)
	for i := range failing {
		expiresAt := oldest.Add(time.Duration(i) * time.Second)
		escalationAt := oldest.Add(time.Duration(i) * time.Second)
		extendAt := oldest.Add(time.Duration(i) * time.Second)
		for _, sla := range []models.SLATracking{{ExpiresAt: &expiresAt}, {EscalationAt: &escalationAt}, {ExpiresAt: &extendAt}} {
			row := createTransfer(sla)
			if sla.ExpiresAt == &extendAt {
				// Its agent replied, so the tick tries (and fails) to extend it.
				slaAgentReplied(t, app, row)
			}
			require.NoError(t, app.DB.Model(row).Update("notes", "synthetic failing row").Error)
		}
	}
	for i := range slaTransfersPerPass + 5 {
		expiresAt := oldest.Add(time.Minute + time.Duration(i)*time.Second)
		escalationAt := oldest.Add(time.Minute + time.Duration(i)*time.Second)
		createTransfer(models.SLATracking{ExpiresAt: &expiresAt})
		createTransfer(models.SLATracking{EscalationAt: &escalationAt})
	}
	responseDeadline := oldest
	createTransfer(models.SLATracking{ResponseDeadline: &responseDeadline})
	settings := models.ChatbotSettings{
		OrganizationID: organization.ID,
		SLA:            models.SLAConfig{Enabled: true, AutoCloseHours: 2, EscalationMinutes: 30, ResponseMinutes: 15},
	}

	var count int64
	var overflowed bool
	require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
		NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, time.Now())
		count, overflowed = slaPassSubxacts(t, app, scoped)
		return nil
	}))
	assert.False(t, overflowed, "failing writes overflowed the subtransaction cache (%d)", count)
	assert.EqualValues(t, 2*slaTransfersPerPass+2, count)
	var expired, escalated int64
	require.NoError(t, app.DB.Model(&models.AgentTransfer{}).
		Where("organization_id = ? AND status = ?", organization.ID, models.TransferStatusExpired).Count(&expired).Error)
	require.NoError(t, app.DB.Model(&models.AgentTransfer{}).
		Where("organization_id = ? AND escalation_level = 1", organization.ID).Count(&escalated).Error)
	assert.EqualValues(t, slaTransfersPerPass+1, expired, "30 expiries plus the deleted contact's transfer")
	assert.EqualValues(t, slaTransfersPerPass, escalated)
}

// Every instance runs the processor. Two passes over a backlog of twice the
// cap race on the same rows: the loser's lost transitions must not overflow
// its cache before it moves on to rows of its own.
func TestSLAConcurrentPassesStayWithinTheSubtransactionCache(t *testing.T) {
	app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
	oldest := time.Now().UTC().Add(-5 * time.Hour)
	for i := range 2 * slaTransfersPerPass {
		expiresAt := oldest.Add(time.Duration(i) * time.Second)
		escalationAt := oldest.Add(time.Duration(i) * time.Second)
		createTransfer(models.SLATracking{ExpiresAt: &expiresAt})
		createTransfer(models.SLATracking{EscalationAt: &escalationAt})
	}
	settings := slaNoticeStateSettings(organization.ID)
	type result struct {
		count      int64
		overflowed bool
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			assert.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
				NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, time.Now())
				var r result
				r.count, r.overflowed = slaPassSubxacts(t, app, scoped)
				results <- r
				return nil
			}))
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for r := range results {
		assert.False(t, r.overflowed, "a pass overflowed the subtransaction cache (%d)", r.count)
		assert.LessOrEqual(t, r.count, int64(2*slaTransfersPerPass+2))
	}
	byPhone := map[string]int{}
	for _, delivery := range deliveries() {
		byPhone[delivery.To]++
	}
	for phone, sent := range byPhone {
		assert.Equal(t, 1, sent, "notice to %s sent more than once", phone)
	}
}

// More than slaAttemptsPerPass rows that fail on every tick at the head of the
// order stop one pass (the bound), but the next pass of the same processor
// continues past them, so the transfer behind them is still handled.
func TestSLAAttemptBoundRotatesPastPersistentFailures(t *testing.T) {
	app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
	slaRowTrigger(t, app, "synthetic failing row", "RAISE EXCEPTION 'synthetic failing SLA row';")
	oldest := time.Now().UTC().Add(-48 * time.Hour)
	for i := range slaAttemptsPerPass + 5 {
		expiresAt := oldest.Add(time.Duration(i) * time.Second)
		row := createTransfer(models.SLATracking{ExpiresAt: &expiresAt})
		require.NoError(t, app.DB.Model(row).Update("notes", "synthetic failing row").Error)
	}
	recent := time.Now().UTC().Add(-time.Hour)
	normal := createTransfer(models.SLATracking{ExpiresAt: &recent})
	settings := slaNoticeStateSettings(organization.ID)
	processor := NewSLAProcessor(app, time.Minute)
	pass := slaProcessorPasses(t, app, processor, settings)
	status := func() models.TransferStatus {
		var stored models.AgentTransfer
		require.NoError(t, app.DB.First(&stored, "id = ?", normal.ID).Error)
		return stored.Status
	}

	pass()
	assert.Equal(t, models.TransferStatusActive, status(), "one pass stops after slaAttemptsPerPass attempts")
	assert.Empty(t, deliveries())
	pass()
	assert.Equal(t, models.TransferStatusExpired, status(), "the next pass continues past the failing rows")
	got := deliveries()
	require.Len(t, got, 1)
	assert.Equal(t, normal.PhoneNumber, got[0].To)
}

// The deleted-contact close takes the organization fence before it locks or
// closes anything. A contact restored while it waits for that fence is
// customer-facing again: its transfer must get the normal auto-close and
// notice, not the silent close.
func TestSLADeletedContactCloseSkipsAContactRestoredMeanwhile(t *testing.T) {
	app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
	past := time.Now().UTC().Add(-time.Hour)
	transfer := createTransfer(models.SLATracking{ExpiresAt: &past})
	require.NoError(t, app.DB.Delete(&models.Contact{}, "id = ?", transfer.ContactID).Error)

	restore := app.DB.Begin()
	require.NoError(t, restore.Error)
	t.Cleanup(func() { _ = restore.Rollback().Error })
	require.NoError(t, database.LockOrganizationPolicyScope(restore, organization.ID))
	require.NoError(t, restore.Exec("UPDATE contacts SET deleted_at = NULL WHERE id = ?", transfer.ContactID).Error)

	settings := slaNoticeStateSettings(organization.ID)
	done := make(chan error, 1)
	go func() {
		done <- app.WithTenantApp(organization.ID, func(scoped *App) error {
			NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, time.Now())
			return nil
		})
	}()
	require.Eventually(t, func() bool {
		var waiting int64
		return app.DB.Raw(`SELECT COUNT(*) FROM pg_catalog.pg_stat_activity
			WHERE datname = pg_catalog.current_database() AND wait_event_type = 'Lock'
			  AND query LIKE '%pg_advisory_xact_lock%'`).Scan(&waiting).Error == nil && waiting == 1
	}, 10*time.Second, 10*time.Millisecond, "the close must wait on the fence")
	require.NoError(t, restore.Commit().Error)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("SLA tick did not finish")
	}

	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
	assert.Equal(t, models.TransferStatusExpired, stored.Status)
	assert.NotContains(t, stored.Notes, "contact deleted")
	assert.Contains(t, stored.Notes, "No agent response within SLA")
	got := deliveries()
	require.Len(t, got, 1, "the restored contact gets its auto-close notice")
	assert.Equal(t, transfer.PhoneNumber, got[0].To)
}

// A deleted contact's transfer is closed when its SLA would have closed it,
// not before; agents see that close, with the contact's name.
func TestSLADeletedContactCloseWaitsForExpiryAndTellsAgents(t *testing.T) {
	app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
	client := websocket.NewClient(app.WSHub, nil, uuid.New(), organization.ID)
	app.WSHub.Register(client)
	testutil.AssertEventually(t, func() bool { return app.WSHub.GetClientCount() == 1 }, 2*time.Second, "client registers")
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	expired := createTransfer(models.SLATracking{ExpiresAt: &past})
	notYet := createTransfer(models.SLATracking{ExpiresAt: &future})
	var contact models.Contact
	require.NoError(t, app.DB.First(&contact, "id = ?", expired.ContactID).Error)
	for _, transfer := range []*models.AgentTransfer{expired, notYet} {
		require.NoError(t, app.DB.Delete(&models.Contact{}, "id = ?", transfer.ContactID).Error)
	}

	settings := slaNoticeStateSettings(organization.ID)
	require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
		NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, time.Now())
		return nil
	}))

	stored := func(id uuid.UUID) models.AgentTransfer {
		var transfer models.AgentTransfer
		require.NoError(t, app.DB.First(&transfer, "id = ?", id).Error)
		return transfer
	}
	assert.Equal(t, models.TransferStatusExpired, stored(expired.ID).Status)
	assert.Equal(t, models.TransferStatusActive, stored(notYet.ID).Status, "not past its expiry yet")
	assert.Empty(t, deliveries(), "nobody to notify")
	events := collectSLATransferEvents(client, 1, 5*time.Second)
	require.Len(t, events, 1)
	assert.Equal(t, websocket.TypeTransferExpired, events[0].Type)
	assert.Equal(t, expired.ID.String(), events[0].Payload["id"])
	assert.Equal(t, string(models.TransferStatusExpired), events[0].Payload["status"])
	assert.Equal(t, contact.ProfileName, events[0].Payload["contact_name"])
}

// A lost escalation compare-and-set may still have taken a subtransaction ID,
// and its savepoint is released, so it counts toward the cap like a write.
func TestSLALostEscalationCountsTowardTheCap(t *testing.T) {
	app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
	// A BEFORE trigger returning NULL skips the row without an error: the
	// compare-and-set matches nothing, exactly like a lost race.
	slaRowTrigger(t, app, "synthetic skipped row", "RETURN NULL;")
	oldest := time.Now().UTC().Add(-48 * time.Hour)
	for i := range slaTransfersPerPass {
		escalationAt := oldest.Add(time.Duration(i) * time.Second)
		row := createTransfer(models.SLATracking{EscalationAt: &escalationAt})
		require.NoError(t, app.DB.Model(row).Update("notes", "synthetic skipped row").Error)
	}
	recent := time.Now().UTC().Add(-time.Hour)
	behind := createTransfer(models.SLATracking{EscalationAt: &recent})
	settings := slaNoticeStateSettings(organization.ID)
	require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
		NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, time.Now())
		return nil
	}))
	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", behind.ID).Error)
	assert.Zero(t, stored.SLA.EscalationLevel, "30 lost compare-and-sets used up this pass's cap")
	assert.Empty(t, deliveries())
}

// Under RLS the whole organization tick is one transaction. A write that fails
// for one transfer (here a trigger, in production for example a deadlock with
// another instance's tick) must not abort the tick and undo every other
// transfer's expiry and escalation, or one bad row would block the
// organization's SLA work on every tick.
func TestSLAFailingTransferRowDoesNotAbortTheTick(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	for _, failing := range []string{"escalation", "extension", "breach"} {
		t.Run(failing, func(t *testing.T) {
			app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
			expired := createTransfer(models.SLATracking{ExpiresAt: &past})
			escalated := createTransfer(models.SLATracking{EscalationAt: &past})
			var poisoned *models.AgentTransfer
			switch failing {
			case "escalation":
				poisoned = createTransfer(models.SLATracking{EscalationAt: &past})
			case "breach":
				// No agent picked it up before its response deadline.
				poisoned = createTransfer(models.SLATracking{ResponseDeadline: &past})
			default:
				// An agent replied recently, so the tick extends this expiry.
				poisoned = createTransfer(models.SLATracking{ExpiresAt: &past})
				agent := testutil.CreateTestUser(t, app.DB, organization.ID)
				require.NoError(t, app.DB.Model(poisoned).Update("agent_id", agent.ID).Error)
				createTestAgentMessage(t, app, organization.ID, poisoned.ContactID, agent.ID,
					poisoned.WhatsAppAccount, time.Now().Add(-30*time.Minute))
			}
			suffix := uuid.NewString()[:8]
			function := "sla_row_failure_" + suffix
			trigger := "sla_row_failure_trigger_" + suffix
			require.NoError(t, app.DB.Exec(fmt.Sprintf(`
				CREATE FUNCTION %s() RETURNS trigger
				LANGUAGE plpgsql
				AS $$
				BEGIN
					IF NEW.id = '%s'::uuid AND (
						NEW.escalation_level IS DISTINCT FROM OLD.escalation_level OR
						NEW.expires_at IS DISTINCT FROM OLD.expires_at OR
						NEW.sla_breached IS DISTINCT FROM OLD.sla_breached
					) THEN
						RAISE EXCEPTION 'synthetic SLA row failure';
					END IF;
					RETURN NEW;
				END;
				$$`, function, poisoned.ID)).Error)
			require.NoError(t, app.DB.Exec(fmt.Sprintf(
				"CREATE TRIGGER %s BEFORE UPDATE ON agent_transfers FOR EACH ROW EXECUTE FUNCTION %s()",
				trigger, function,
			)).Error)
			t.Cleanup(func() {
				_ = app.DB.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON agent_transfers", trigger)).Error
				_ = app.DB.Exec(fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", function)).Error
			})

			settings := slaNoticeStateSettings(organization.ID)
			settings.SLA.ResponseMinutes = 15
			require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
				NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, time.Now())
				return nil
			}), "one failing transfer must not abort the organization's tick")

			stored := func(id uuid.UUID) models.AgentTransfer {
				var transfer models.AgentTransfer
				require.NoError(t, app.DB.First(&transfer, "id = ?", id).Error)
				return transfer
			}
			assert.Equal(t, models.TransferStatusExpired, stored(expired.ID).Status)
			assert.Equal(t, 1, stored(escalated.ID).SLA.EscalationLevel)
			unchanged := stored(poisoned.ID)
			assert.Equal(t, models.TransferStatusActive, unchanged.Status)
			assert.Zero(t, unchanged.SLA.EscalationLevel)
			assert.False(t, unchanged.SLA.Breached)
			if failing == "extension" {
				require.NotNil(t, unchanged.SLA.ExpiresAt)
				assert.WithinDuration(t, past, *unchanged.SLA.ExpiresAt, time.Millisecond)
			}

			byPhone := map[string]string{}
			for _, delivery := range deliveries() {
				byPhone[delivery.To] = delivery.Body
			}
			assert.Equal(t, map[string]string{
				expired.PhoneNumber:   settings.SLA.AutoCloseMessage,
				escalated.PhoneNumber: settings.SLA.WarningMessage,
			}, byPhone)
		})
	}
}

// A rotating pass wraps to the oldest deadline once it reaches the end, so a
// row behind the stored cursor is handled in that same pass. When a walk gets
// all the way round, the cursor is dropped and passes start at the oldest
// deadline again.
func TestSLACursorWrapsAndIsDroppedAfterAFullRound(t *testing.T) {
	app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
	dropTrigger := slaRowTrigger(t, app, "synthetic failing row", "RAISE EXCEPTION 'synthetic failing SLA row';")
	oldest := time.Now().UTC().Add(-48 * time.Hour)
	for i := range slaAttemptsPerPass + 5 {
		expiresAt := oldest.Add(time.Duration(i) * time.Second)
		row := createTransfer(models.SLATracking{ExpiresAt: &expiresAt})
		require.NoError(t, app.DB.Model(row).Update("notes", "synthetic failing row").Error)
	}
	settings := slaNoticeStateSettings(organization.ID)
	processor := NewSLAProcessor(app, time.Minute)
	pass := slaProcessorPasses(t, app, processor, settings)
	key := slaResumeKey{organizationID: organization.ID, step: "expires_at"}

	pass()
	require.NotEqual(t, uuid.Nil, processor.resume.byStep[key].id, "the attempt bound left a cursor")
	behindAt := oldest.Add(-time.Hour)
	behind := createTransfer(models.SLATracking{ExpiresAt: &behindAt})
	pass()
	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", behind.ID).Error)
	assert.Equal(t, models.TransferStatusExpired, stored.Status, "the rotating pass wrapped to the row behind its cursor")
	require.Len(t, deliveries(), 1)

	// Once the rows stop failing, rotation drains them and ends.
	dropTrigger()
	for range 6 {
		pass()
	}
	var active int64
	require.NoError(t, app.DB.Model(&models.AgentTransfer{}).
		Where("organization_id = ? AND status = ?", organization.ID, models.TransferStatusActive).Count(&active).Error)
	assert.Zero(t, active)
	assert.NotContains(t, processor.resume.byStep, key, "a walk that got all the way round drops its cursor")
}

// Each organization keeps its own cursor: one organization's rotation must not
// move where another one resumes.
func TestSLACursorsAreKeptPerOrganization(t *testing.T) {
	app, first, createFirst, deliveries := slaAfterCommitFixture(t)
	second := testutil.CreateTestOrganization(t, app.DB)
	secondAccount := testutil.CreateTestWhatsAppAccount(t, app.DB, second.ID)
	createSecond := func(sla models.SLATracking) *models.AgentTransfer {
		contact := testutil.CreateTestContactWith(t, app.DB, second.ID,
			testutil.WithContactAccount(secondAccount.Name),
			testutil.WithPhoneNumber("60"+testutil.NewTestGraphObjectID()[:10]))
		transfer := &models.AgentTransfer{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: second.ID, ContactID: contact.ID,
			WhatsAppAccount: secondAccount.Name, PhoneNumber: contact.PhoneNumber,
			Status: models.TransferStatusActive, Source: models.TransferSourceManual,
			TransferredAt: time.Now().UTC().Add(-2 * time.Hour), SLA: sla,
		}
		require.NoError(t, app.DB.Create(transfer).Error)
		return transfer
	}
	slaRowTrigger(t, app, "synthetic failing row", "RAISE EXCEPTION 'synthetic failing SLA row';")
	// The second organization's deadlines all come after the first's, so a
	// shared cursor would skip the first organization's normal transfer.
	oldest := time.Now().UTC().Add(-48 * time.Hour)
	var normals []*models.AgentTransfer
	for offset, create := range map[time.Duration]func(models.SLATracking) *models.AgentTransfer{
		0:             createFirst,
		2 * time.Hour: createSecond,
	} {
		for i := range slaAttemptsPerPass + 5 {
			expiresAt := oldest.Add(offset + time.Duration(i)*time.Second)
			row := create(models.SLATracking{ExpiresAt: &expiresAt})
			require.NoError(t, app.DB.Model(row).Update("notes", "synthetic failing row").Error)
		}
		normalAt := oldest.Add(offset + time.Hour)
		normals = append(normals, create(models.SLATracking{ExpiresAt: &normalAt}))
	}
	processor := NewSLAProcessor(app, time.Minute)
	pass := slaProcessorPasses(t, app, processor, slaNoticeStateSettings(first.ID), slaNoticeStateSettings(second.ID))

	pass()
	assert.Empty(t, deliveries(), "the attempt bound stops both organizations' first pass")
	pass()
	for _, normal := range normals {
		var stored models.AgentTransfer
		require.NoError(t, app.DB.First(&stored, "id = ?", normal.ID).Error)
		assert.Equal(t, models.TransferStatusExpired, stored.Status, "organization %s resumed from its own cursor", normal.OrganizationID)
	}
	assert.Len(t, deliveries(), 2)
}

// A pass stopped by the write cap leaves no cursor: the rows it wrote left the
// window or moved up a level, and the next pass must start at the oldest
// deadline. Escalated rows reach level 2 (the critical alert) on the very next
// pass, not one idle pass later.
func TestSLAWriteCapLeavesNoCursorForTheNextPass(t *testing.T) {
	for _, backlog := range []int{slaTransfersPerPass, slaTransfersPerPass + 15} {
		t.Run(fmt.Sprintf("rows=%d", backlog), func(t *testing.T) {
			app, organization, createTransfer, _ := slaAfterCommitFixture(t)
			oldest := time.Now().UTC().Add(-5 * time.Hour)
			var rows []*models.AgentTransfer
			for i := range backlog {
				escalationAt := oldest.Add(time.Duration(i) * time.Second)
				rows = append(rows, createTransfer(models.SLATracking{EscalationAt: &escalationAt}))
			}
			settings := models.ChatbotSettings{
				OrganizationID: organization.ID,
				SLA:            models.SLAConfig{Enabled: true, EscalationMinutes: 30},
			}
			processor := NewSLAProcessor(app, time.Minute)
			pass := slaProcessorPasses(t, app, processor, settings)
			level := func(i int) int {
				var stored models.AgentTransfer
				require.NoError(t, app.DB.First(&stored, "id = ?", rows[i].ID).Error)
				return stored.SLA.EscalationLevel
			}

			pass()
			pass()
			// The second pass starts at the oldest deadline again: it takes the
			// oldest rows to level 2 (none idle), and newer rows wait.
			for i := range backlog {
				want := 2
				if i >= slaTransfersPerPass {
					want = 0
				}
				assert.Equal(t, want, level(i), "row %d after two passes", i)
			}
		})
	}
}

// A failed deleted-contact close is rolled back and released like any other
// failed write: it costs no subtransaction ID, so the writes after it in the
// pass are the only ones held.
func TestSLAFailedDeletedContactCloseCostsNoSubtransaction(t *testing.T) {
	app, organization, createTransfer, _ := slaAfterCommitFixture(t)
	slaRowTrigger(t, app, "synthetic failing orphan", "RAISE EXCEPTION 'synthetic failing orphan close';")
	oldest := time.Now().UTC().Add(-5 * time.Hour)
	orphanAt := oldest
	orphan := createTransfer(models.SLATracking{ExpiresAt: &orphanAt})
	require.NoError(t, app.DB.Model(orphan).Update("notes", "synthetic failing orphan").Error)
	require.NoError(t, app.DB.Delete(&models.Contact{}, "id = ?", orphan.ContactID).Error)
	const expiring = 3
	for i := range expiring {
		expiresAt := oldest.Add(time.Minute + time.Duration(i)*time.Second)
		createTransfer(models.SLATracking{ExpiresAt: &expiresAt})
	}
	settings := models.ChatbotSettings{
		OrganizationID: organization.ID,
		SLA:            models.SLAConfig{Enabled: true, AutoCloseHours: 2},
	}

	var count int64
	var overflowed bool
	require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
		NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, time.Now())
		count, overflowed = slaPassSubxacts(t, app, scoped)
		return nil
	}))
	assert.False(t, overflowed)
	assert.EqualValues(t, expiring, count, "only the kept expiries hold a subtransaction ID")
	var stored models.AgentTransfer
	require.NoError(t, app.DB.First(&stored, "id = ?", orphan.ID).Error)
	assert.Equal(t, models.TransferStatusActive, stored.Status, "the failed close was rolled back")
}

// The extension's deadline and first-response writes are one unit, in both
// modes: if recording the first response fails, the deadline is not moved.
func TestSLAFailedFirstResponseLeavesTheDeadlineUnchanged(t *testing.T) {
	for _, rls := range []bool{true, false} {
		t.Run(fmt.Sprintf("rls=%v", rls), func(t *testing.T) {
			app, organization, createTransfer, deliveries := slaAfterCommitFixture(t)
			app.Config.Database.RLSEnabled = rls
			slaRowTrigger(t, app, "synthetic first response failure", `
				IF NEW.first_response_at IS DISTINCT FROM OLD.first_response_at THEN
					RAISE EXCEPTION 'synthetic first response failure';
				END IF;`)
			past := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			transfer := createTransfer(models.SLATracking{ExpiresAt: &past})
			require.NoError(t, app.DB.Model(transfer).Update("notes", "synthetic first response failure").Error)
			slaAgentReplied(t, app, transfer)

			settings := slaNoticeStateSettings(organization.ID)
			require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
				NewSLAProcessor(scoped, time.Minute).processOrganizationSLA(settings, time.Now())
				return nil
			}))
			var stored models.AgentTransfer
			require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
			assert.Equal(t, models.TransferStatusActive, stored.Status)
			assert.Nil(t, stored.SLA.FirstResponseAt)
			require.NotNil(t, stored.SLA.ExpiresAt)
			assert.True(t, past.Equal(stored.SLA.ExpiresAt.UTC()), "the deadline moved without its first response: %s", stored.SLA.ExpiresAt)
			assert.Empty(t, deliveries())
		})
	}
}

// A write that swallows a failed statement returns nil but leaves its savepoint
// aborted. The helper rolls it back, so the pass transaction stays usable and
// later writes still commit.
func TestSLARowSavepointRecoversFromAFailedRelease(t *testing.T) {
	for _, rls := range []bool{true, false} {
		t.Run(fmt.Sprintf("rls=%v", rls), func(t *testing.T) {
			app, organization, createTransfer, _ := slaAfterCommitFixture(t)
			app.Config.Database.RLSEnabled = rls
			transfer := createTransfer(models.SLATracking{})
			require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
				processor := NewSLAProcessor(scoped, time.Minute)
				swallowed := processor.withSLARowSavepoint(organization.ID, func(tx *gorm.DB) error {
					_ = tx.Exec("SELECT 1 / 0").Error
					return nil
				})
				assert.Error(t, swallowed, "an aborted savepoint cannot be kept")
				return processor.withSLARowSavepoint(organization.ID, func(tx *gorm.DB) error {
					return tx.Model(&models.AgentTransfer{}).Where("id = ?", transfer.ID).Update("notes", "synthetic kept write").Error
				})
			}))
			var stored models.AgentTransfer
			require.NoError(t, app.DB.First(&stored, "id = ?", transfer.ID).Error)
			assert.Equal(t, "synthetic kept write", stored.Notes)
		})
	}
}

// Side effects a write queued for after commit belong to that write: they are
// discarded when it is rolled back and run when it is kept.
func TestSLARowSavepointDiscardsSideEffectsOfARolledBackWrite(t *testing.T) {
	for _, rls := range []bool{true, false} {
		t.Run(fmt.Sprintf("rls=%v", rls), func(t *testing.T) {
			app, organization, _, _ := slaAfterCommitFixture(t)
			app.Config.Database.RLSEnabled = rls
			var ran []string
			errRolledBack := errors.New("synthetic rolled-back write")
			require.NoError(t, app.WithTenantApp(organization.ID, func(scoped *App) error {
				processor := NewSLAProcessor(scoped, time.Minute)
				err := processor.withSLARowSavepoint(organization.ID, func(*gorm.DB) error {
					scoped.afterTenantCommit(func() { ran = append(ran, "rolled back") })
					return errRolledBack
				})
				require.ErrorIs(t, err, errRolledBack)
				return processor.withSLARowSavepoint(organization.ID, func(*gorm.DB) error {
					scoped.afterTenantCommit(func() { ran = append(ran, "kept") })
					return nil
				})
			}))
			assert.Equal(t, []string{"kept"}, ran)
		})
	}
}

// Cursors no pass used during a tick (organization gone, SLA or the step
// turned off) are dropped at the end of that tick.
func TestSLAResumeCursorsPruneUnused(t *testing.T) {
	cursors := &slaResumeCursors{byStep: map[slaResumeKey]slaCursor{}, used: map[slaResumeKey]bool{}}
	active := slaResumeKey{organizationID: uuid.New(), step: "expires_at"}
	stale := slaResumeKey{organizationID: uuid.New(), step: "sla_escalation_at"}
	cursors.store(active, slaCursor{deadline: time.Now(), id: uuid.New()})
	cursors.store(stale, slaCursor{deadline: time.Now(), id: uuid.New()})

	cursors.load(active)
	cursors.pruneUnused()
	assert.Contains(t, cursors.byStep, active)
	assert.NotContains(t, cursors.byStep, stale)
	cursors.pruneUnused()
	assert.NotContains(t, cursors.byStep, active, "not used during the next tick either")
}

// The tick itself prunes: a cursor for an organization the tick did not
// process is gone after it.
func TestSLATickPrunesCursorsOfOrganizationsItDidNotProcess(t *testing.T) {
	testutil.TruncateTables(testutil.SetupTestDB(t))
	app := newSLATestApp(t)
	processor := NewSLAProcessor(app, time.Minute)
	stale := slaResumeKey{organizationID: uuid.New(), step: "expires_at"}
	processor.resume.store(stale, slaCursor{deadline: time.Now(), id: uuid.New()})
	processor.processStaleTransfers()
	assert.NotContains(t, processor.resume.byStep, stale)
}
