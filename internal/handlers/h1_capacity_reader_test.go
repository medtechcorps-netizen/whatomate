package handlers

// SOURCE PROPOSAL ONLY: reviewed separately before copying into an isolated
// candidate. These tests use synthetic rows and the actual physical-attempt
// fence; the blocked callback performs no provider I/O. No test runs in parallel.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func h1CapacityReaderPrivatePool(t *testing.T, app *App) *gorm.DB {
	t.Helper()
	shared := app.DB
	pool, _ := h1OpenPool(t, 10)
	app.DB = pool
	app.Config = &config.Config{Database: config.DatabaseConfig{RLSEnabled: true}}
	return shared
}

// The callback is inside withInboundContinuationPhysicalAIAttempt, after its
// real KEY SHARE lock and fresh physical-contact policy checks. A2 must progress
// before A1's callback is released, not merely after an arbitrary short sleep.
func h1CapacityReaderHoldAttempt(t *testing.T, app *App, work *persistedIncomingMessage) func() {
	t.Helper()
	execution := app.scopedApp(app.DB, work.OrganizationID)
	execution.tenantOrgID = uuid.Nil
	execution.inboundContinuation = &inboundContinuationExecution{
		OrganizationID: work.OrganizationID, ContactID: work.Contact.ID,
		MessageID: work.Persisted.ID, WAMID: work.Message.ID,
	}
	acquired, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- execution.withInboundContinuationPhysicalAIAttempt(context.Background(), func() error {
			close(acquired)
			<-release
			return nil
		})
	}()
	var once sync.Once
	unblock := func() {
		once.Do(func() {
			close(release)
			select {
			case err := <-done:
				require.NoError(t, err, "physical attempt must finish after explicit release")
			case <-time.After(8 * time.Second):
				t.Fatal("owned physical attempt failed to join")
			}
		})
	}
	t.Cleanup(unblock)
	select {
	case <-acquired:
	case err := <-done:
		// Put it back so cleanup can always join through one path.
		done <- err
		t.Fatalf("physical attempt ended before acquiring its guard: %v", err)
	case <-time.After(8 * time.Second):
		unblock()
		t.Fatal("physical attempt never acquired its guard")
	}
	return unblock
}

// Full row comparisons catch unexpected selector, hold, suppression, claim or
// ledger mutations. Only the named message's display projection/updated_at and
// one explicitly inspected new session may differ. Account credential rows are
// deliberately not copied into assertion output.
func h1CapacityReaderRows(t *testing.T, db *gorm.DB, org, repairMessage, newSession uuid.UUID) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, table := range []string{"contacts", "messages", "scheduled_jobs", "outbox_events", "inbound_events",
		"whatsapp_identity_review_holds", "customer_activity_events", "chatbot_sessions"} {
		expression := "to_jsonb(t)"
		args := []any{}
		if table == "messages" {
			expression = "CASE WHEN id = ? THEN to_jsonb(t) - 'whats_app_account' - 'updated_at' ELSE to_jsonb(t) END"
			args = append(args, repairMessage)
		}
		query := fmt.Sprintf("SELECT (%s)::text AS body FROM %s t WHERE organization_id = ?", expression, table)
		args = append(args, org)
		if table == "chatbot_sessions" {
			query += " AND id <> ?"
			args = append(args, newSession)
		}
		query += " ORDER BY id"
		var rows []struct{ Body string }
		require.NoError(t, db.Raw(query, args...).Scan(&rows).Error)
		out[table] = []string{}
		for _, row := range rows {
			out[table] = append(out[table], row.Body)
		}
	}
	return out
}

func h1CapacityReaderPersist(t *testing.T, app *App, account *models.WhatsAppAccount, suffix string) *persistedIncomingMessage {
	t.Helper()
	inbound := inboundContinuationTextMessage(t, "wamid.capacity-reader-"+suffix+"-"+uuid.NewString(),
		"60"+testutil.NewTestGraphObjectID(), "Synthetic capacity reader")
	work, duplicate, err := app.persistIncomingMessageBeforeAck(account.PhoneID, inbound, "Synthetic reader patient")
	require.NoError(t, err)
	require.False(t, duplicate)
	require.NotNil(t, work)
	return work
}

func TestH1CapacityReader_DifferentContactValidatesAndCreatesWhileAttemptHeld(t *testing.T) {
	app := newProcessorTestApp(t)
	orgA, accountA := createProcessorTestOrg(t, app)
	orgB, accountB := createProcessorTestOrg(t, app)
	first := h1CapacityReaderPersist(t, app, accountA, "holder")
	second := h1CapacityReaderPersist(t, app, accountA, "reader")
	foreign := h1CapacityReaderPersist(t, app, accountB, "foreign")
	shared := h1CapacityReaderPrivatePool(t, app)
	processor := NewInboundContinuationProcessor(app, time.Hour)
	job, err := processor.claim(context.Background(), orgA.ID, &second.Persisted.ID)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.NoError(t, shared.Model(&models.Message{}).Where("id = ?", second.Persisted.ID).
		Update("whats_app_account", "stale-display-projection").Error)
	beforeA := h1CapacityReaderRows(t, shared, orgA.ID, second.Persisted.ID, uuid.Nil)
	beforeB := h1CapacityReaderRows(t, shared, orgB.ID, uuid.Nil, uuid.Nil)
	release := h1CapacityReaderHoldAttempt(t, app, first)

	type result struct {
		work    *persistedIncomingMessage
		session *models.ChatbotSession
		isNew   bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		r.err = app.withContinuationReaderApp(orgA.ID, func(scoped *App) error {
			var loadErr error
			r.work, loadErr = loadInboundContinuationWorkMode(scoped, orgA.ID, job, true)
			return loadErr
		})
		if r.err == nil {
			execution := app.scopedApp(app.DB, orgA.ID)
			execution.tenantOrgID = uuid.Nil
			execution.inboundContinuation = &inboundContinuationExecution{OrganizationID: orgA.ID,
				ContactID: r.work.Contact.ID, MessageID: r.work.Persisted.ID, WAMID: r.work.Message.ID}
			r.session, r.isNew, r.err = execution.getOrCreatePreparedIncomingSession(orgA.ID,
				r.work.Contact.ID, accountA.Name, r.work.Message.From, 30)
		}
		done <- r
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(8 * time.Second):
		release()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("reader did not join after releasing the physical guard")
		}
		t.Fatal("different-contact reader/session creation waited behind a physical AI guard")
	}
	require.NoError(t, got.err)
	require.NotNil(t, got.work)
	require.NotNil(t, got.session)
	require.True(t, got.isNew)
	assert.Equal(t, second.Contact.ID, got.work.Contact.ID)
	assert.Equal(t, accountA.Name, got.work.Persisted.WhatsAppAccount)
	assert.Equal(t, orgA.ID, got.session.OrganizationID)
	assert.Equal(t, second.Contact.ID, got.session.ContactID)
	assert.Equal(t, accountA.Name, got.session.WhatsAppAccount)
	assert.Equal(t, models.SessionStatusActive, got.session.Status)
	assert.Equal(t, second.Persisted.ID.String(), got.session.SessionData["__inbound_session_origin"])
	assert.Equal(t, beforeA, h1CapacityReaderRows(t, shared, orgA.ID, second.Persisted.ID, got.session.ID))
	assert.Equal(t, beforeB, h1CapacityReaderRows(t, shared, orgB.ID, uuid.Nil, uuid.Nil))
	var repaired models.Message
	require.NoError(t, shared.First(&repaired, second.Persisted.ID).Error)
	assert.Equal(t, accountA.Name, repaired.WhatsAppAccount, "the display-only repair committed")
	var stored models.ChatbotSession
	require.NoError(t, shared.First(&stored, got.session.ID).Error)
	assert.Equal(t, got.session.ID, stored.ID)
	assert.True(t, got.session.LastActivityAt.Equal(stored.LastActivityAt))

	// Tenant B cannot consume A's claimed proof or mutate either tenant through
	// the reader. This is an explicit cross-tenant authority test, not a claim
	// that the fixture's administrative DB role exercises FORCE RLS itself.
	err = app.withContinuationReaderApp(orgB.ID, func(scoped *App) error {
		_, loadErr := loadInboundContinuationWorkMode(scoped, orgB.ID, job, true)
		return loadErr
	})
	require.ErrorContains(t, err, "tenant or kind mismatch")
	assert.NotEqual(t, foreign.Contact.ID, got.session.ContactID)
	assert.Equal(t, beforeB, h1CapacityReaderRows(t, shared, orgB.ID, uuid.Nil, uuid.Nil))
	release()
}

func TestH1CapacityReader_EligibleBSUIDNeverAttemptsSelectorMutation(t *testing.T) {
	app, account, contact := whatsappIdentityFixture(t)
	inbound := inboundContinuationTextMessage(t, "wamid.capacity-bsuid-"+uuid.NewString(), contact.PhoneNumber, "Synthetic history")
	original := persistWhatsAppIdentityHistory(t, app, account, contact, inbound)
	holder := h1CapacityReaderPersist(t, app, account, "bsuid-holder")
	foreign := testutil.CreateTestContact(t, app.DB, account.OrganizationID)
	foreignBSUID := "US.foreign-" + uuid.NewString()
	require.NoError(t, app.DB.Model(foreign).Update("bs_uid", foreignBSUID).Error)
	shared := h1CapacityReaderPrivatePool(t, app)
	identity := coexistenceContactIdentity{Phone: contact.PhoneNumber, UserID: "US.new-" + uuid.NewString()}
	require.Empty(t, contact.BSUID)
	require.True(t, canEnrichProvenWhatsAppMessageBSUID(contact, identity), "this fixture must be eligible for the writer's optional enrichment")
	var beforeContact models.Contact
	require.NoError(t, shared.First(&beforeContact, contact.ID).Error)
	before := h1CapacityReaderRows(t, shared, account.OrganizationID, uuid.Nil, uuid.Nil)
	var attemptedContactUpdates atomic.Int32
	name := "h1:capacity-reader-contact-updates:" + uuid.NewString()
	require.NoError(t, app.DB.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "contacts" {
			attemptedContactUpdates.Add(1)
		}
	}))
	t.Cleanup(func() { _ = app.DB.Callback().Update().Remove(name) })
	release := h1CapacityReaderHoldAttempt(t, app, holder)
	resolve := func(validation bool, who coexistenceContactIdentity) error {
		fn := func(scoped *App) error {
			current := *account
			if err := scoped.prepareWhatsAppMessageAuthority(&current); err != nil {
				return err
			}
			_, err := scoped.resolveWhatsAppMessage(&current, whatsAppMessageLookup{WAMID: inbound.ID,
				MessageID: original.ID, ContactID: contact.ID, Identity: who, Direction: models.DirectionIncoming,
				Lock: true, RepairProjection: true, continuationValidation: validation})
			return err
		}
		if validation {
			return app.withContinuationReaderApp(account.OrganizationID, fn)
		}
		return app.WithCommittedTenantApp(account.OrganizationID, func(scoped *App) error {
			if err := database.LockOrganizationPolicyScope(scoped.DB, account.OrganizationID); err != nil {
				return err
			}
			return fn(scoped)
		})
	}
	done := make(chan error, 1)
	go func() { done <- resolve(true, identity) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(8 * time.Second):
		release()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("eligible-BSUID reader failed to join")
		}
		t.Fatal("validation tried to take the selector writer fence")
	}
	assert.Zero(t, attemptedContactUpdates.Load(), "even a swallowed optional UPDATE is forbidden in reader validation")
	assert.Equal(t, before, h1CapacityReaderRows(t, shared, account.OrganizationID, uuid.Nil, uuid.Nil))
	conflicting := identity
	conflicting.UserID = foreignBSUID
	require.Error(t, resolve(true, conflicting), "a conflicting BSUID cannot borrow the phone-only winner")
	assert.Zero(t, attemptedContactUpdates.Load())
	assert.Equal(t, before, h1CapacityReaderRows(t, shared, account.OrganizationID, uuid.Nil, uuid.Nil))
	require.NoError(t, app.DB.Callback().Update().Remove(name))

	// The existing database trigger must still prevent selector mutation while
	// another physical guard owns KEY SHARE. Roll back only this deliberate failed
	// statement, then prove its transaction remains usable (the optional-write contract).
	require.NoError(t, database.WithTenantReadCommitted(app.DB, account.OrganizationID, func(tx *gorm.DB) error {
		if err := tx.SavePoint("h1_selector_guard_control").Error; err != nil {
			return err
		}
		writeErr := tx.Exec("UPDATE contacts SET bs_uid = ? WHERE id = ? AND organization_id = ?",
			identity.UserID, contact.ID, account.OrganizationID).Error
		assert.Equal(t, "55P03", postgresErrorCode(writeErr), "the actual selector trigger keeps its UPDATE NOWAIT fence")
		if err := tx.RollbackTo("h1_selector_guard_control").Error; err != nil {
			return err
		}
		var one int
		return tx.Raw("SELECT 1").Scan(&one).Error
	}))
	assert.Equal(t, before, h1CapacityReaderRows(t, shared, account.OrganizationID, uuid.Nil, uuid.Nil))
	release()
	require.NoError(t, resolve(false, identity), "normal UPDATE-fenced resolver retains optional enrichment after the guard ends")
	var enriched models.Contact
	require.NoError(t, shared.First(&enriched, contact.ID).Error)
	assert.Equal(t, identity.UserID, enriched.BSUID)
	beforeContact.BSUID, beforeContact.UpdatedAt = enriched.BSUID, enriched.UpdatedAt
	assert.Equal(t, beforeContact, enriched, "writer enrichment changes only its selector and update timestamp")
}
