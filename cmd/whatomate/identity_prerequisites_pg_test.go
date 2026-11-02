package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/stretchr/testify/require"
)

func readIdentityPrerequisites(t *testing.T, cfg config.DatabaseConfig) (identityPrerequisiteReport, []byte) {
	t.Helper()
	args, err := parseCatalogSnapshotArgs(identityPrerequisiteArguments())
	require.NoError(t, err)
	raw, err := collectIdentityPrerequisites(context.Background(), cfg, args)
	planPGOK(t, err, "collect catalog-only identity prerequisites")
	var report identityPrerequisiteReport
	require.NoError(t, json.Unmarshal(raw, &report))
	require.Equal(t, 1, report.Version)
	require.Equal(t, "identity_inventory_prerequisites", report.Purpose)
	require.Equal(t, "not_granted", report.InventoryAdmission)
	require.Equal(t, "not_verified", report.FalseGuardMeaning)
	require.True(t, report.SessionBeforeAfterMatch)
	require.True(t, report.SessionDisposed)
	require.False(t, report.CustomerRowsRead)
	require.False(t, report.AccountScopeObserved)
	require.LessOrEqual(t, len(raw), identityPrerequisiteByteLimit)
	require.Len(t, report.Visibility, 7)
	for i, table := range identityInventoryFixtureTables() {
		require.Equal(t, table, report.Visibility[i].Table)
	}
	return report, raw
}

func TestIdentityPrerequisitesPGOwnerRuntimePolicyAndMissingRelation(t *testing.T) {
	f, ownerCfg, scope := newIdentityInventoryFixture(t)
	owner, raw := readIdentityPrerequisites(t, ownerCfg)
	require.True(t, owner.ProfilePassed)
	require.True(t, owner.VisibilityPassed)
	for _, item := range owner.Visibility {
		require.True(t, item.GuardPassed)
	}
	for _, private := range []string{scope.organizationID, scope.accountID, f.owner, f.runtime, "never-observe-credential", "never-observe-secret"} {
		require.NotContains(t, string(raw), private)
	}
	// The configured runtime role is reported as a failed prerequisite; the
	// valid MigrationURL must not silently turn this into an owner observation.
	runtime, _ := readIdentityPrerequisites(t, f.cfg)
	require.True(t, runtime.Profile.NonSuperuser)
	require.False(t, runtime.Profile.DatabaseOwner)
	require.False(t, runtime.ProfilePassed)
	require.False(t, runtime.VisibilityPassed)
	for _, item := range runtime.Visibility {
		require.False(t, item.GuardPassed)
	}
	identityInventoryExec(t, f, "DROP POLICY rereply_migration_access ON public.whatsapp_accounts")
	policy, _ := readIdentityPrerequisites(t, ownerCfg)
	require.True(t, policy.ProfilePassed)
	require.False(t, policy.VisibilityPassed)
	for _, item := range policy.Visibility {
		require.Equal(t, item.Table != "whatsapp_accounts", item.GuardPassed)
	}
	identityInventoryExec(t, f, "DROP TABLE public.channel_accounts")
	missing, _ := readIdentityPrerequisites(t, ownerCfg)
	for _, item := range missing.Visibility {
		require.Equal(t, item.Table != "whatsapp_accounts" && item.Table != "channel_accounts", item.GuardPassed)
	}
}

type identityPrerequisiteCancelTrace struct {
	cancel context.CancelFunc
	fired  bool
}

type identityPrerequisiteTraceKey struct{}

func (trace *identityPrerequisiteCancelTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, identityPrerequisiteTraceKey{}, strings.Contains(data.SQL, "SELECT a.pid::bigint,a.datid::bigint,a.usesysid::bigint,a.backend_start,"))
}

func (trace *identityPrerequisiteCancelTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if selected, _ := ctx.Value(identityPrerequisiteTraceKey{}).(bool); selected && !trace.fired {
		trace.fired = true
		trace.cancel()
	}
}

func TestIdentityPrerequisitesPGCancellationPublishesNothingAndCloses(t *testing.T) {
	f, cfg, _ := newIdentityInventoryFixture(t)
	parent, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	d, ok := newPlanDeadline(parent, time.Now())
	require.True(t, ok)
	ctx, cancel := context.WithDeadline(parent, d.workAt)
	defer cancel()
	parsed, err := pgx.ParseConfig(cfg.URL)
	planPGOK(t, err, "parse native prerequisite fixture")
	observer, err := seedDiagnosticConfig(parsed, d, ctx)
	planPGOK(t, err, "configure native prerequisite fixture")
	observer.RuntimeParams["application_name"] = identityPrerequisiteApplication
	trace := &identityPrerequisiteCancelTrace{cancel: cancel}
	observer.Tracer = trace
	raw, err := captureIdentityPrerequisites(ctx, d, observer)
	require.True(t, trace.fired)
	require.Error(t, err)
	require.Nil(t, raw, "cancellation must discard all partial profile and policy output")
	verify, end := context.WithTimeout(context.Background(), 3*time.Second)
	defer end()
	_, err = f.admin.Exec(verify, "SELECT pg_catalog.pg_stat_clear_snapshot()")
	planPGOK(t, err, "clear prerequisite cleanup statistics")
	var remaining int
	err = f.admin.QueryRow(verify, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname=pg_catalog.current_database() AND application_name=$1", identityPrerequisiteApplication).Scan(&remaining)
	planPGOK(t, err, "verify native prerequisite backend disposal")
	require.Zero(t, remaining)
}

func TestIdentityPrerequisitesPGRejectsWritableTransaction(t *testing.T) {
	_, cfg, _ := newIdentityInventoryFixture(t)
	parent, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	d, ok := newPlanDeadline(parent, time.Now())
	require.True(t, ok)
	ctx, cancel := context.WithDeadline(parent, d.workAt)
	defer cancel()
	parsed, err := pgx.ParseConfig(cfg.URL)
	planPGOK(t, err, "parse prerequisite transaction fixture")
	observer, err := seedDiagnosticConfig(parsed, d, ctx)
	planPGOK(t, err, "configure prerequisite transaction fixture")
	conn, err := pgx.ConnectConfig(ctx, observer)
	planPGOK(t, err, "connect prerequisite transaction fixture")
	var tx pgx.Tx
	defer func() { require.True(t, disposePlanSession(d, conn, tx, false).complete()) }()
	tx, err = conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite})
	planPGOK(t, err, "begin explicitly writable fixture transaction")
	report, err := database.ObserveIdentityPrerequisiteVisibility(ctx, tx)
	require.Error(t, err)
	require.Nil(t, report)
	require.Equal(t, byte('T'), conn.PgConn().TxStatus(), "refusal must not abort or alter the borrowed transaction")
	var readOnly string
	planPGOK(t, tx.QueryRow(ctx, "SELECT pg_catalog.current_setting('transaction_read_only')").Scan(&readOnly), "retain caller transaction")
	require.Equal(t, "off", readOnly)
}
