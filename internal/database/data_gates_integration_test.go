package database_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func dataGateOutput(t *testing.T, run func() error) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	previous := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = previous; _ = writer.Close(); _ = reader.Close() }()
	read := make(chan []byte, 1)
	go func() { raw, _ := io.ReadAll(io.LimitReader(reader, 1<<20)); read <- raw }()
	runErr := run()
	require.NoError(t, writer.Close())
	os.Stdout = previous
	select {
	case raw := <-read:
		return string(raw), runErr
	case <-time.After(2 * time.Second):
		t.Fatal("data gate output capture did not finish")
		return "", runErr
	}
}

func dataGateLines(counts [3]int64) string {
	ids := []string{"empty-phone-legacy-contacts", "empty-phone-mixed-sender-contacts", "empty-phone-missing-continuation-messages"}
	var lines strings.Builder
	for i, id := range ids {
		fmt.Fprintf(&lines, "data-gate %s count=%d mode=observe\n", id, counts[i])
	}
	return lines.String()
}

func dataGateRows(t *testing.T, f catalogFixture) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	for _, table := range []string{"contacts", "messages", "scheduled_jobs"} {
		var raw string
		require.NoError(t, f.owner.Raw("SELECT COALESCE(jsonb_agg(to_jsonb(s) ORDER BY to_jsonb(s)::text),'[]'::jsonb)::text FROM public."+table+" s").Scan(&raw).Error)
		out[table] = sha256.Sum256([]byte(raw))
	}
	return out
}

func dataGateOwnerState(t *testing.T, f catalogFixture) string {
	t.Helper()
	var state string
	require.NoError(t, f.owner.Raw(`SELECT jsonb_build_array(pg_backend_pid(),current_setting('search_path'),
 current_setting('session_replication_role'),current_setting('statement_timeout'),current_setting('lock_timeout'),
 current_setting('idle_in_transaction_session_timeout'),current_setting('transaction_read_only'),current_setting('transaction_isolation'),
 (SELECT count(*) FROM pg_catalog.pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory'))::text`).Scan(&state).Error)
	return state
}

func dataGatePoolOne(t *testing.T, f catalogFixture) {
	t.Helper()
	pool, err := f.owner.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
}

func TestTenantRLS_DataGateUnicodeAndDenominators(t *testing.T) {
	f := catalogClone(t)
	dataGatePoolOne(t, f)
	var org models.Organization
	require.NoError(t, f.owner.First(&org).Error)
	contact := func(phone string, metadata models.JSONB) models.Contact {
		row := models.Contact{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, PhoneNumber: phone, WhatsAppAccount: uuid.NewString(), Metadata: metadata}
		require.NoError(t, f.owner.Create(&row).Error)
		return row
	}
	spaces := []rune{0x9, 0xa, 0xb, 0xc, 0xd, 0x20, 0x85, 0xa0, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000}
	require.Len(t, spaces, 25)
	var candidate models.Contact
	for _, space := range spaces {
		require.True(t, contactutil.IsEmptyPhone(string(space)))
		candidate = contact(string(space), models.JSONB{})
	}
	contact("", models.JSONB{})
	contact("+", models.JSONB{})
	contact(" + ", models.JSONB{})
	contact("60111", models.JSONB{"empty_phone_upgraded_from": false})
	contact("60222", models.JSONB{"empty_phone_upgraded_from": nil})
	array := contact("60333", models.JSONB{})
	require.NoError(t, f.owner.Exec("UPDATE public.contacts SET metadata='[\"empty_phone_upgraded_from\"]'::jsonb WHERE id=?", array.ID).Error)
	ordinary := contact("601234", models.JSONB{})
	for _, excluded := range []string{"\u001c", "\u001d", "\u001e", "\u001f", "\u200b", "\u180e", "\ufeff", "++", "+ 1"} {
		require.False(t, contactutil.IsEmptyPhone(excluded))
		contact(excluded, models.JSONB{})
	}
	nullMetadata := contact("60444", models.JSONB{})
	require.NoError(t, f.owner.Exec("UPDATE public.contacts SET metadata=NULL WHERE id=?", nullMetadata.ID).Error)
	message := func(c models.Contact, direction models.Direction) models.Message {
		row := models.Message{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, ContactID: c.ID, WhatsAppAccount: uuid.NewString(), Direction: direction, MessageType: models.MessageType("text")}
		require.NoError(t, f.owner.Create(&row).Error)
		return row
	}
	job := func(m models.Message, payload models.JSONB) {
		row := models.ScheduledJob{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, Kind: "inbound_message.continuation", AggregateType: "any-type", AggregateID: &m.ID, RunAt: time.Now(), IdempotencyKey: uuid.NewString(), Payload: payload}
		require.NoError(t, f.owner.Create(&row).Error)
	}
	// Three messages and duplicate jobs are one mixed contact group. A whitespace
	// sender is nonempty; from_parent_user_id is used only for an empty first key.
	m1 := message(ordinary, models.Direction("incoming"))
	m2 := message(ordinary, models.Direction("incoming"))
	m3 := message(ordinary, models.Direction("incoming"))
	job(m1, models.JSONB{"message": map[string]any{"from_user_id": "sender-a"}})
	job(m1, models.JSONB{"message": map[string]any{"from_user_id": "sender-a"}})
	job(m2, models.JSONB{"message": map[string]any{"from_user_id": "", "from_parent_user_id": "sender-b"}})
	job(m3, models.JSONB{"message": map[string]any{"from_user_id": " "}})
	covered := message(candidate, models.Direction("incoming"))
	job(covered, models.JSONB{})
	missing1 := message(candidate, models.Direction("incoming"))
	message(candidate, models.Direction("incoming"))
	message(candidate, models.Direction("outgoing"))
	otherOrg := models.Organization{BaseModel: models.BaseModel{ID: uuid.New()}, Name: "Synthetic other tenant", Slug: "synthetic-pr18-" + uuid.NewString(), Settings: models.JSONB{}}
	require.NoError(t, f.owner.Create(&otherOrg).Error)
	// A continuation belonging to a different tenant cannot cover this message.
	crossTenantJob := models.ScheduledJob{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: otherOrg.ID, Kind: "inbound_message.continuation", AggregateID: &missing1.ID, RunAt: time.Now(), IdempotencyKey: uuid.NewString(), Payload: models.JSONB{}}
	require.NoError(t, f.owner.Create(&crossTenantJob).Error)
	// The migration observer sees a second tenant without any app.tenant_id.
	originalOrg := org
	org = otherOrg
	other := contact("+", models.JSONB{})
	otherMessage := message(other, models.Direction("incoming"))
	job(otherMessage, models.JSONB{"message": map[string]any{"from_user_id": "tenant-b-first"}})
	job(otherMessage, models.JSONB{"message": map[string]any{"from_user_id": "tenant-b-second"}})
	org = originalOrg
	// Soft-deleted/merged rows remain in all three raw observations.
	require.NoError(t, f.owner.Exec("UPDATE public.contacts SET deleted_at=NOW(),merged_into_id=? WHERE id=?", ordinary.ID, candidate.ID).Error)
	require.NoError(t, f.owner.Exec("UPDATE public.messages SET deleted_at=NOW() WHERE id=?", missing1.ID).Error)
	before, session := dataGateRows(t, f), dataGateOwnerState(t, f)
	output, err := dataGateOutput(t, func() error { return database.ObserveMigrationDataGatesForTest(f.owner, catalogTemplate.runtime) })
	require.NoError(t, err)
	require.Equal(t, dataGateLines([3]int64{32, 2, 2}), output)
	require.Equal(t, before, dataGateRows(t, f))
	require.Equal(t, session, dataGateOwnerState(t, f))
}

func TestTenantRLS_DataGateIncompleteAndRecovery(t *testing.T) {
	t.Run("missing_table_is_informational", func(t *testing.T) {
		f := catalogEmptyTarget(t)
		dataGatePoolOne(t, f)
		before := dataGateOwnerState(t, f)
		output, err := dataGateOutput(t, func() error { return database.ObserveMigrationDataGatesForTest(f.owner, catalogTemplate.runtime) })
		require.NoError(t, err)
		require.Equal(t, 3, strings.Count(output, " error\n"))
		require.NotContains(t, output, "count=")
		require.Equal(t, before, dataGateOwnerState(t, f))
	})
	t.Run("recoverable_count_error_does_not_abort_next_queries", func(t *testing.T) {
		f := catalogClone(t)
		dataGatePoolOne(t, f)
		require.NoError(t, f.owner.Exec("ALTER TABLE public.contacts RENAME COLUMN phone_number TO unavailable_phone_number").Error)
		before, session := dataGateRows(t, f), dataGateOwnerState(t, f)
		output, err := dataGateOutput(t, func() error { return database.ObserveMigrationDataGatesForTest(f.owner, catalogTemplate.runtime) })
		require.NoError(t, err)
		require.Equal(t, "data-gate empty-phone-legacy-contacts error\ndata-gate empty-phone-mixed-sender-contacts count=0 mode=observe\ndata-gate empty-phone-missing-continuation-messages error\n", output)
		require.Equal(t, before, dataGateRows(t, f))
		require.Equal(t, session, dataGateOwnerState(t, f))
	})
	t.Run("lock_timeout_recovers_without_false_zero", func(t *testing.T) {
		f := catalogClone(t)
		dataGatePoolOne(t, f)
		before, session := dataGateRows(t, f), dataGateOwnerState(t, f)
		var observerPID int
		require.NoError(t, f.owner.Raw("SELECT pg_catalog.pg_backend_pid()").Scan(&observerPID).Error)
		blocker, err := catalogConnect(catalogRoleURL(catalogTemplate.base, f.name, catalogTemplate.owner, catalogTemplate.ownerPassword))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, catalogClose(blocker)) })
		blockerPool, err := blocker.DB()
		require.NoError(t, err)
		blockerTx, err := blockerPool.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		released := false
		t.Cleanup(func() {
			if !released {
				require.NoError(t, blockerTx.Rollback())
			}
		})
		var blockerPID int
		require.NoError(t, blockerTx.QueryRowContext(context.Background(), "SELECT pg_catalog.pg_backend_pid()").Scan(&blockerPID))
		require.NotEqual(t, observerPID, blockerPID, "blocker must be a separate real backend")
		_, err = blockerTx.ExecContext(context.Background(), "LOCK TABLE public.contacts IN ACCESS EXCLUSIVE MODE NOWAIT")
		require.NoError(t, err)
		var lockHeld bool
		require.NoError(t, blockerTx.QueryRowContext(context.Background(), "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_locks WHERE pid=pg_catalog.pg_backend_pid() AND locktype='relation' AND relation='public.contacts'::regclass AND mode='AccessExclusiveLock' AND granted)").Scan(&lockHeld))
		require.True(t, lockHeld)

		started := time.Now()
		output, observeErr := dataGateOutput(t, func() error {
			return database.ObserveMigrationDataGatesForTest(f.owner, catalogTemplate.runtime)
		})
		elapsed := time.Since(started)
		// The lock remains held for the complete observation. Each query touches
		// contacts, so an unavailable count must never become a fabricated zero.
		require.NoError(t, observeErr, "real lock timeouts must recover through the savepoints and outer rollback")
		require.Equal(t, "data-gate empty-phone-legacy-contacts error\ndata-gate empty-phone-mixed-sender-contacts error\ndata-gate empty-phone-missing-continuation-messages error\n", output)
		require.GreaterOrEqual(t, elapsed, time.Second, "the observer must encounter the held lock")
		require.Less(t, elapsed, 17*time.Second, "work and cleanup share the original bounded horizon")
		err = blockerTx.Rollback()
		released = true
		require.NoError(t, err)
		require.Equal(t, before, dataGateRows(t, f))
		require.Equal(t, session, dataGateOwnerState(t, f), "held session restored and migration advisory lock released")
		// The same real pooled backend remains usable after rollback; releasing
		// the sole blocker restores genuine known counts without any repair.
		output, err = dataGateOutput(t, func() error {
			return database.ObserveMigrationDataGatesForTest(f.owner, catalogTemplate.runtime)
		})
		require.NoError(t, err)
		require.Equal(t, dataGateLines([3]int64{0, 0, 0}), output)
		require.Equal(t, before, dataGateRows(t, f))
		require.Equal(t, session, dataGateOwnerState(t, f))
	})
	t.Run("wrong_migration_policy_cannot_report_zero", func(t *testing.T) {
		f := catalogClone(t)
		dataGatePoolOne(t, f)
		require.NoError(t, f.owner.Exec("ALTER POLICY rereply_migration_access ON public.contacts USING(false) WITH CHECK(false)").Error)
		output, err := dataGateOutput(t, func() error { return database.ObserveMigrationDataGatesForTest(f.owner, catalogTemplate.runtime) })
		require.NoError(t, err)
		require.Equal(t, 3, strings.Count(output, " error\n"))
		require.NotContains(t, output, "count=")
	})
}

func TestTenantRLS_DataGatePublicCoordinatorRunsBeforeProfileAndPreservesVerifier(t *testing.T) {
	t.Run("partial_profile_refusal_still_observes", func(t *testing.T) {
		f := catalogEmptyTarget(t)
		dataGatePoolOne(t, f)
		before := dataGateOwnerState(t, f)
		backfills, verifiers := 0, 0
		output, err := dataGateOutput(t, func() error {
			return database.RunRLSMigrationCoordinator(f.owner, &config.DefaultAdminConfig{}, catalogTemplate.runtime,
				func(*gorm.DB) error { backfills++; return errors.New("unexpected mutation") }, func() error { verifiers++; return database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime) })
		})
		require.Error(t, err)
		require.Equal(t, 3, strings.Count(output, " error\n"))
		require.Zero(t, backfills)
		require.Zero(t, verifiers)
		require.Equal(t, before, dataGateOwnerState(t, f))
	})
	t.Run("completed_profile_verifier_failure_is_preserved", func(t *testing.T) {
		f := catalogClone(t)
		dataGatePoolOne(t, f)
		var org models.Organization
		require.NoError(t, f.owner.First(&org).Error)
		positive := models.Contact{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, PhoneNumber: "+", WhatsAppAccount: uuid.NewString(), Metadata: models.JSONB{}}
		require.NoError(t, f.owner.Create(&positive).Error)
		before := catalogPlanRows(t, f)
		countRows := dataGateRows(t, f)
		session := dataGateOwnerState(t, f)
		backfills, verifiers := 0, 0
		sentinel := errors.New("synthetic verifier refusal")
		output, err := dataGateOutput(t, func() error {
			return database.RunRLSMigrationCoordinator(f.owner, &config.DefaultAdminConfig{}, catalogTemplate.runtime,
				func(*gorm.DB) error { backfills++; return errors.New("unexpected mutation") }, func() error {
					verifiers++
					if err := database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime); err != nil {
						return err
					}
					return sentinel
				})
		})
		require.ErrorIs(t, err, sentinel)
		require.Equal(t, dataGateLines([3]int64{1, 0, 0}), output)
		require.Zero(t, backfills)
		require.Equal(t, 1, verifiers)
		require.Equal(t, before, catalogPlanRows(t, f))
		require.Equal(t, countRows, dataGateRows(t, f))
		require.Equal(t, session, dataGateOwnerState(t, f))
	})
}

func TestTenantRLS_PlanSeedCancelledContextCannotClaimClear(t *testing.T) {
	f := catalogClone(t)
	start := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(15*time.Second))
	ownerURL := catalogRoleURL(catalogTemplate.base, f.name, catalogTemplate.owner, catalogTemplate.ownerPassword)
	tx, done := catalogPlanTransaction(t, ctx, start.Add(17*time.Second), ownerURL.String())
	cancel()
	got, err := database.ObservePlanSeeds(ctx, tx)
	require.Error(t, err)
	for _, i := range []int{0, 2, 3, 4, 5, 7, 8, 9} {
		require.Equal(t, database.PlanSeedIncomplete, got.Steps[i].State)
	}
	done()
}

func TestTenantRLS_DataGateLostBackendRefusesBeforeMutation(t *testing.T) {
	f := catalogClone(t)
	dataGatePoolOne(t, f)
	adminURL := *catalogTemplate.base
	adminURL.Path, adminURL.RawPath = "/"+f.name, ""
	admin, err := catalogConnect(&adminURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalogClose(admin)) })
	before := dataGateRows(t, f)
	killed, attempted := false, false
	var injectionErr error
	require.NoError(t, f.owner.Callback().Row().Before("gorm:row").Register("pr18:terminate_owned_data_gate", func(tx *gorm.DB) {
		if attempted || !strings.HasPrefix(tx.Statement.SQL.String(), "SELECT pg_catalog.count(*)::bigint") {
			return
		}
		attempted = true
		var pid int
		injectionErr = tx.Statement.ConnPool.QueryRowContext(context.Background(), "SELECT pg_catalog.pg_backend_pid()").Scan(&pid)
		if injectionErr == nil {
			injectionErr = admin.Raw("SELECT pg_catalog.pg_terminate_backend(?)", pid).Scan(&killed).Error
		}
	}))
	t.Cleanup(func() { require.NoError(t, f.owner.Callback().Row().Remove("pr18:terminate_owned_data_gate")) })
	backfills, verifiers := 0, 0
	output, runErr := dataGateOutput(t, func() error {
		return database.RunRLSMigrationCoordinator(f.owner, &config.DefaultAdminConfig{}, catalogTemplate.runtime,
			func(*gorm.DB) error { backfills++; return errors.New("unexpected mutation") }, func() error { verifiers++; return database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime) })
	})
	require.True(t, attempted)
	require.NoError(t, injectionErr)
	require.True(t, killed)
	require.ErrorContains(t, runErr, "data-gate rollback failed")
	require.Equal(t, 3, strings.Count(output, " error\n"))
	require.Zero(t, backfills)
	require.Zero(t, verifiers)
	require.Equal(t, before, dataGateRows(t, f))
}
