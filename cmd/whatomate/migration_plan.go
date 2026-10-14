package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	planUnused uint32 = iota
	planRunning
	planSpent
	planDisabled
	planRuntimeName = "rereply-plan-runtime"
	planOwnerName   = "rereply-plan-owner"
)

// This state belongs to one migration command. No observer handle survives an
// invocation, and no caller can supply a query, dial function or source loader.
type migrationPlanState struct {
	slot    atomic.Uint32
	runtime config.DatabaseConfig
	owner   *pgx.ConnConfig
	marker  string
}

type planDeadline struct{ workAt, finalAt time.Time }

type planObservation struct {
	result     dbcatalog.PlanResult
	incomplete []dbcatalog.PlanIssue
	parent     context.Context
	deadline   planDeadline
}

func newPlanDeadline(parent context.Context, start time.Time) (planDeadline, bool) {
	d := planDeadline{workAt: start.Add(15 * time.Second), finalAt: start.Add(17 * time.Second)}
	if parent == nil || parent.Err() != nil {
		return d, false
	}
	if until, ok := parent.Deadline(); ok && until.Before(d.finalAt) {
		d.finalAt = until
	}
	if reserved := d.finalAt.Add(-2 * time.Second); reserved.Before(d.workAt) {
		d.workAt = reserved
	}
	return d, start.Before(d.workAt)
}

func (d planDeadline) alive(ctx context.Context) bool {
	return ctx != nil && ctx.Err() == nil && time.Now().Before(d.workAt) && time.Now().Before(d.finalAt)
}

func (d planDeadline) cleanup() (context.Context, context.CancelFunc) {
	until := time.Now().Add(2 * time.Second)
	if d.finalAt.Before(until) {
		until = d.finalAt
	}
	// Cancellation stops work but must not prevent an already-owned connection
	// from being disposed inside the original, unextended final deadline.
	return context.WithDeadline(context.Background(), until)
}

// parseMigrationPlanConfig preserves the pinned GORM postgres driver's extra
// timezone handling. The original DSN is parsed once; copies retain pgx's TLS,
// authentication and ordinary connection-selection behavior.
func parseMigrationPlanConfig(dsn string) (*pgx.ConnConfig, error) {
	if dsn == "" {
		return nil, errors.New("migration database configuration unavailable")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("migration database configuration unavailable")
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = make(map[string]string)
	}
	if match := regexp.MustCompile(`(time_zone|TimeZone)=(.*?)($|&| )`).FindStringSubmatch(dsn); len(match) > 2 {
		cfg.RuntimeParams["timezone"] = match[2]
	}
	return cfg, nil
}

func migrationPlanMarker(id uuid.UUID) string {
	if id == uuid.Nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 {
		return ""
	}
	return "rereply-plan-lock-" + id.String()
}

func openPlannedMigration(ctx context.Context, cfg *config.DatabaseConfig) (*gorm.DB, *migrationPlanState, error) {
	if cfg == nil || ctx == nil || ctx.Err() != nil {
		return nil, nil, errors.New("migration database initialization unavailable")
	}
	parsed, err := parseMigrationPlanConfig(cfg.MigrationURL)
	if err != nil {
		return nil, nil, err
	}
	state := &migrationPlanState{runtime: *cfg, owner: parsed.Copy()}
	pool := parsed.Copy()
	id, entropyErr := uuid.NewRandom()
	if entropyErr == nil {
		state.marker = migrationPlanMarker(id)
	}
	if state.marker == "" {
		state.slot.Store(planDisabled)
	} else {
		pool.RuntimeParams["application_name"] = state.marker
	}
	sqlDB := stdlib.OpenDB(*pool)
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Second)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), DisableAutomaticPing: true,
	})
	if err == nil {
		err = sqlDB.PingContext(ctx)
	}
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, errors.New("migration database initialization unavailable")
	}
	return db, state, nil
}

func incompletePlan(component, code string) planObservation {
	return planObservation{incomplete: []dbcatalog.PlanIssue{{Component: component, Code: code}}}
}

func (s *migrationPlanState) observe(parent context.Context) planObservation {
	if s == nil || s.slot.Load() == planDisabled {
		return incompletePlan("planner", "disabled")
	}
	if !s.slot.CompareAndSwap(planUnused, planRunning) {
		return incompletePlan("planner", "spent")
	}
	defer s.slot.Store(planSpent)
	d, ok := newPlanDeadline(parent, time.Now())
	if !ok {
		return incompletePlan("planner", "deadline")
	}
	ctx, cancel := context.WithDeadline(parent, d.workAt)
	defer cancel()
	if s.owner == nil || s.marker == "" || s.runtime.RuntimeRole == "" {
		return incompletePlan("planner", "configuration")
	}
	source, err := dbcatalog.BuiltinProductionPlanInputs()
	if err != nil || !d.alive(ctx) {
		return incompletePlan("source", "unavailable")
	}
	dsn, err := catalogRuntimeDSN(s.runtime)
	if err != nil {
		return incompletePlan("runtime", "configuration")
	}
	runtime, err := pgx.ParseConfig(dsn)
	if err != nil || !d.alive(ctx) {
		return incompletePlan("runtime", "configuration")
	}
	runtime, err = planObserverConfig(runtime, planRuntimeName, d, ctx)
	if err != nil {
		return incompletePlan("runtime", "configuration")
	}
	snapshot, held, code := s.captureRuntime(ctx, d, runtime, source.CaptureCatalog())
	if code != "" {
		return incompletePlan("runtime", code)
	}
	// The command's held GORM lease and this dedicated owner observer require
	// two owner slots. Zero (and negative database/sql limits) means unlimited.
	if s.runtime.MaxOpenConns > 0 && s.runtime.MaxOpenConns < 2 {
		return incompletePlan("owner", "capacity")
	}
	if !d.alive(ctx) {
		return incompletePlan("planner", "deadline")
	}
	owner, err := planObserverConfig(s.owner, planOwnerName, d, ctx)
	if err != nil {
		return incompletePlan("owner", "configuration")
	}
	seeds, code := s.captureSeeds(ctx, d, owner, held)
	if code != "" {
		return incompletePlan("owner", code)
	}
	if !d.alive(ctx) {
		return incompletePlan("planner", "deadline")
	}
	mapped, ok := planSeedInput(seeds)
	if !ok {
		return incompletePlan("seeds", "unavailable")
	}
	result, err := dbcatalog.Plan(ctx, dbcatalog.PlanInput{Source: source, Snapshot: snapshot, Seeds: mapped})
	if err != nil || !d.alive(ctx) {
		return incompletePlan("plan", "unavailable")
	}
	if len(result.Incomplete) != 0 {
		return incompletePlan("plan", "facts")
	}
	return planObservation{result: result, parent: parent, deadline: d}
}

func planObserverConfig(parsed *pgx.ConnConfig, name string, d planDeadline, ctx context.Context) (*pgx.ConnConfig, error) {
	if parsed == nil || !d.alive(ctx) || (name != planRuntimeName && name != planOwnerName) {
		return nil, errors.New("plan observer configuration unavailable")
	}
	cfg := parsed.Copy()
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = make(map[string]string)
	}
	if cfg.RuntimeParams["options"] != "" {
		return nil, errors.New("plan observer options unavailable")
	}
	remaining := time.Until(d.workAt)
	if remaining < time.Millisecond {
		return nil, errors.New("plan observer deadline")
	}
	if cfg.ConnectTimeout <= 0 || cfg.ConnectTimeout > remaining {
		cfg.ConnectTimeout = remaining
	}
	cfg.RuntimeParams["application_name"] = name
	cfg.RuntimeParams["search_path"] = "pg_catalog"
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	for key, cap := range map[string]time.Duration{"statement_timeout": 3 * time.Second, "lock_timeout": time.Second, "idle_in_transaction_session_timeout": 5 * time.Second} {
		if remaining < cap {
			cap = remaining
		}
		ms := cap.Milliseconds()
		if ms < 1 {
			return nil, errors.New("plan observer deadline")
		}
		cfg.RuntimeParams[key] = strconv.FormatInt(ms, 10) + "ms"
	}
	return cfg, nil
}

// Close returning nil is not proof that pgx's asynchronous cancellation cleanup
// finished. The same bounded cleanup context covers rollback, close and waiting.
type planDisposal struct {
	rollbackErr, closeErr error
	cleanupDone, inTime   bool
}

func (r planDisposal) complete() bool {
	return r.rollbackErr == nil && r.closeErr == nil && r.cleanupDone && r.inTime
}

func disposePlanSession(d planDeadline, conn *pgx.Conn, tx pgx.Tx, committed bool) planDisposal {
	ctx, cancel := d.cleanup()
	defer cancel()
	r := planDisposal{}
	if tx != nil && !committed {
		r.rollbackErr = tx.Rollback(ctx)
	}
	if conn != nil {
		r.closeErr = conn.Close(ctx)
		select {
		case <-conn.PgConn().CleanupDone():
			r.cleanupDone = true
		case <-ctx.Done():
		}
	}
	r.inTime = ctx.Err() == nil && time.Now().Before(d.finalAt)
	return r
}

func openPlanSession(ctx context.Context, cfg *pgx.ConnConfig) (*pgx.Conn, pgx.Tx, error) {
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return conn, nil, errors.New("plan connection unavailable")
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	return conn, tx, err
}

func (s *migrationPlanState) captureRuntime(ctx context.Context, d planDeadline, cfg *pgx.ConnConfig, expected dbcatalog.Catalog) (snapshot dbcatalog.Snapshot, held planHeldIdentity, code string) {
	conn, tx, err := openPlanSession(ctx, cfg)
	committed := false
	defer func() {
		if !disposePlanSession(d, conn, tx, committed).complete() {
			code = "cleanup"
		}
	}()
	if err != nil {
		return snapshot, held, "connect"
	}
	self, err := readPlanSelf(ctx, tx, planRuntimeName, s.runtime.RuntimeRole, true)
	if err != nil {
		return snapshot, held, "identity"
	}
	held, err = readPlanHeld(ctx, tx, self, s.marker, s.owner.User, s.owner.Database, nil)
	if err != nil {
		return snapshot, held, "target"
	}
	snapshot, err = dbcatalog.CapturePlanInTx(ctx, tx, expected)
	if err != nil || !d.alive(ctx) {
		return snapshot, held, "capture"
	}
	after, err := readPlanSelf(ctx, tx, planRuntimeName, s.runtime.RuntimeRole, true)
	if err != nil || !samePlanSelf(self, after) {
		return snapshot, held, "identity"
	}
	if _, err = readPlanHeld(ctx, tx, after, s.marker, s.owner.User, s.owner.Database, &held); err != nil {
		return snapshot, held, "target"
	}
	if !d.alive(ctx) {
		return snapshot, held, "deadline"
	}
	if err = tx.Commit(ctx); err != nil {
		return snapshot, held, "commit"
	}
	committed = true
	return snapshot, held, ""
}

func (s *migrationPlanState) captureSeeds(ctx context.Context, d planDeadline, cfg *pgx.ConnConfig, held planHeldIdentity) (seeds database.PlanSeedObservations, code string) {
	conn, tx, err := openPlanSession(ctx, cfg)
	committed := false
	defer func() {
		if !disposePlanSession(d, conn, tx, committed).complete() {
			code = "cleanup"
		}
	}()
	if err != nil {
		return seeds, "connect"
	}
	self, err := readPlanSelf(ctx, tx, planOwnerName, s.owner.User, false)
	if err != nil || self.roleOID != held.RoleOID {
		return seeds, "identity"
	}
	if _, err = readPlanHeld(ctx, tx, self, s.marker, s.owner.User, s.owner.Database, &held); err != nil {
		return seeds, "target"
	}
	seeds, err = database.ObservePlanSeeds(ctx, tx)
	if err != nil || !d.alive(ctx) {
		return seeds, "seeds"
	}
	after, err := readPlanSelf(ctx, tx, planOwnerName, s.owner.User, false)
	if err != nil || !samePlanSelf(self, after) {
		return seeds, "identity"
	}
	if _, err = readPlanHeld(ctx, tx, after, s.marker, s.owner.User, s.owner.Database, &held); err != nil {
		return seeds, "target"
	}
	if !d.alive(ctx) {
		return seeds, "deadline"
	}
	if err = tx.Commit(ctx); err != nil {
		return seeds, "commit"
	}
	committed = true
	return seeds, ""
}

type planSelfIdentity struct {
	pid, databaseOID, roleOID                                   int64
	database, role, application                                 string
	backendStart                                                time.Time
	superuser, createRole, bypassRLS, replication, readAllStats bool
}

func samePlanSelf(a, b planSelfIdentity) bool {
	start := a.backendStart.Equal(b.backendStart)
	a.backendStart, b.backendStart = time.Time{}, time.Time{}
	return start && a == b
}

const planSelfSQL = `SELECT a.pid::bigint, a.datid::bigint, a.datname::text,
a.usesysid::bigint, a.usename::text, a.application_name::text, a.backend_start, a.backend_type::text,
pg_catalog.current_database()::text, current_user::text, c.oid::bigint,
session_user::text, s.oid::bigint, c.rolsuper, c.rolcreaterole, c.rolbypassrls, c.rolreplication,
pg_catalog.current_setting('server_version_num')::integer,
pg_catalog.current_setting('application_name'),
pg_catalog.octet_length(pg_catalog.current_setting('application_name')),
pg_catalog.pg_is_in_recovery(), pg_catalog.pg_has_role(current_user, 'pg_read_all_stats', 'MEMBER'),
pg_catalog.current_setting('transaction_read_only'), pg_catalog.current_setting('transaction_isolation'),
pg_catalog.current_setting('search_path'),
pg_catalog.current_setting('statement_timeout')::interval <= interval '3 seconds' AND pg_catalog.current_setting('statement_timeout')::interval > interval '0 seconds',
pg_catalog.current_setting('lock_timeout')::interval <= interval '1 second' AND pg_catalog.current_setting('lock_timeout')::interval > interval '0 seconds',
pg_catalog.current_setting('idle_in_transaction_session_timeout')::interval <= interval '5 seconds' AND pg_catalog.current_setting('idle_in_transaction_session_timeout')::interval > interval '0 seconds'
FROM pg_catalog.pg_stat_activity AS a
JOIN pg_catalog.pg_database AS d ON d.oid=a.datid AND d.datname=pg_catalog.current_database()
JOIN pg_catalog.pg_roles AS c ON c.rolname=current_user
JOIN pg_catalog.pg_roles AS s ON s.rolname=session_user
WHERE a.pid=pg_catalog.pg_backend_pid() AND a.backend_start IS NOT NULL`

func readPlanSelf(ctx context.Context, tx pgx.Tx, application, role string, restricted bool) (planSelfIdentity, error) {
	var v planSelfIdentity
	rows, err := tx.Query(ctx, planSelfSQL)
	if err != nil {
		return v, errors.New("plan self unavailable")
	}
	defer rows.Close()
	var backend, db, current, session, configured, readOnly, isolation, searchPath string
	var currentOID, sessionOID int64
	var version, applicationBytes int
	var recovery, statement, lock, idle bool
	if !rows.Next() {
		return v, errors.New("plan self unavailable")
	}
	err = rows.Scan(&v.pid, &v.databaseOID, &v.database, &v.roleOID, &v.role, &v.application, &v.backendStart, &backend,
		&db, &current, &currentOID, &session, &sessionOID, &v.superuser, &v.createRole, &v.bypassRLS, &v.replication,
		&version, &configured, &applicationBytes, &recovery, &v.readAllStats, &readOnly, &isolation, &searchPath, &statement, &lock, &idle)
	if err != nil || rows.Next() || rows.Err() != nil || ctx.Err() != nil || v.pid <= 0 || v.pid >= 1<<32 || v.databaseOID <= 0 || v.databaseOID >= 1<<32 || v.roleOID <= 0 || v.roleOID >= 1<<32 || v.backendStart.IsZero() ||
		v.database == "" || v.database != db || v.role != role || current != role || session != role || currentOID != v.roleOID || sessionOID != v.roleOID ||
		backend != "client backend" || v.application != application || configured != application || applicationBytes != len(application) ||
		version/10000 != 17 || recovery || readOnly != "on" || isolation != "repeatable read" || searchPath != "pg_catalog" || !statement || !lock || !idle ||
		(restricted && (v.superuser || v.createRole || v.bypassRLS || v.replication || v.readAllStats)) {
		return v, errors.New("plan self unavailable")
	}
	return v, nil
}

type planHeldIdentity struct {
	PID             int64  `json:"pid"`
	DatabaseOID     int64  `json:"database_oid"`
	DatabaseName    string `json:"database_name"`
	RoleOID         int64  `json:"role_oid"`
	RoleName        string `json:"role_name"`
	ApplicationName string `json:"application_name"`
}

type planWitness struct {
	ObserverPID          int64             `json:"observer_pid"`
	ObserverDatabaseOID  int64             `json:"observer_database_oid"`
	ObserverDatabaseName string            `json:"observer_database_name"`
	TokenRows            int64             `json:"token_rows"`
	LockRows             int64             `json:"lock_rows"`
	JoinedRows           int64             `json:"joined_rows"`
	Held                 *planHeldIdentity `json:"held"`
}

// The fixed PG17 witness used by the held-lock experiment, with its sole
// challenge value bound as $1. Cross-role backend_start is deliberately absent.
const planHeldSQL = `WITH marked AS (
  SELECT a.pid::bigint AS pid, a.datid::bigint AS database_oid,
         a.datname::text AS database_name, a.usesysid::bigint AS role_oid,
         a.usename::text AS role_name, a.application_name::text AS application_name
  FROM pg_catalog.pg_stat_activity AS a WHERE a.application_name=$1
), target_locks AS (
  SELECT l.pid::bigint AS pid, l.database::bigint AS database_oid
  FROM pg_catalog.pg_locks AS l
  WHERE l.locktype='advisory'
    AND l.database=(SELECT d.oid FROM pg_catalog.pg_database AS d WHERE d.datname=pg_catalog.current_database())
    AND l.classid::bigint=(pg_catalog.hashtext(pg_catalog.current_database())::bigint & 4294967295::bigint)
    AND l.objid::bigint=1380270905 AND l.objsubid=2 AND l.mode='ExclusiveLock' AND l.granted IS TRUE
)
SELECT pg_catalog.row_to_json(witness) FROM (
  SELECT pg_catalog.pg_backend_pid()::bigint AS observer_pid,
         (SELECT d.oid::bigint FROM pg_catalog.pg_database AS d WHERE d.datname=pg_catalog.current_database()) AS observer_database_oid,
         pg_catalog.current_database()::text AS observer_database_name,
         (SELECT COUNT(*)::bigint FROM marked) AS token_rows,
         (SELECT COUNT(*)::bigint FROM target_locks) AS lock_rows,
         (SELECT COUNT(*)::bigint FROM marked AS m JOIN target_locks AS l ON l.pid=m.pid AND l.database_oid=m.database_oid) AS joined_rows,
         (SELECT CASE WHEN COUNT(*)=1 THEN pg_catalog.json_build_object(
           'pid',MIN(pid),'database_oid',MIN(database_oid),'database_name',MIN(database_name),
           'role_oid',MIN(role_oid),'role_name',MIN(role_name),'application_name',MIN(application_name)
         ) ELSE NULL END FROM marked) AS held
) AS witness`

func validPlanHeld(w planWitness, self planSelfIdentity, marker, owner, databaseName string, retained *planHeldIdentity) bool {
	h := w.Held
	return w.ObserverPID == self.pid && w.ObserverDatabaseOID == self.databaseOID && w.ObserverDatabaseName == self.database &&
		w.TokenRows == 1 && w.LockRows == 1 && w.JoinedRows == 1 && h != nil && h.PID > 0 && h.PID < 1<<32 && h.PID != self.pid &&
		h.DatabaseOID == self.databaseOID && h.DatabaseOID > 0 && h.DatabaseOID < 1<<32 && h.DatabaseName == self.database && h.DatabaseName == databaseName &&
		h.RoleOID > 0 && h.RoleOID < 1<<32 && h.RoleName != "" && h.RoleName == owner && h.ApplicationName == marker && len(marker) == 54 &&
		(retained == nil || *h == *retained)
}

func readPlanHeld(ctx context.Context, tx pgx.Tx, self planSelfIdentity, marker, owner, databaseName string, retained *planHeldIdentity) (planHeldIdentity, error) {
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.pg_stat_clear_snapshot()"); err != nil {
		return planHeldIdentity{}, errors.New("plan target unavailable")
	}
	var raw []byte
	if err := tx.QueryRow(ctx, planHeldSQL, marker).Scan(&raw); err != nil || len(raw) > 8192 {
		return planHeldIdentity{}, errors.New("plan target unavailable")
	}
	var w planWitness
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&w); err != nil || ctx.Err() != nil || !validPlanHeld(w, self, marker, owner, databaseName, retained) {
		return planHeldIdentity{}, errors.New("plan target unavailable")
	}
	return *w.Held, nil
}

func planSeedInput(raw database.PlanSeedObservations) ([10]dbcatalog.PlanSeedObservation, bool) {
	var out [10]dbcatalog.PlanSeedObservation
	for i, seed := range raw.Steps {
		var state dbcatalog.SeedState
		switch seed.State {
		case database.PlanSeedClear:
			state = dbcatalog.SeedClear
		case database.PlanSeedDrift:
			state = dbcatalog.SeedDrift
		case database.PlanSeedIncomplete:
			state = dbcatalog.SeedIncomplete
		default:
			return out, false
		}
		out[i] = dbcatalog.PlanSeedObservation{ID: seed.ID, State: state, Code: seed.Code}
	}
	return out, true
}

func (o planObservation) line() string {
	// All incomplete paths use a fixed diagnostic; no DSN, driver error, nonce,
	// database name, role name, catalog identity or row can enter this line.
	if len(o.incomplete) != 0 || len(o.result.Incomplete) != 0 || !o.deadline.alive(o.parent) {
		return "rereply-plan incomplete"
	}
	r := o.result
	if r.CatalogVersion < 0 || len(r.LiveSHA256) != 64 || r.OtherSchemaExcess < 0 {
		return "rereply-plan incomplete"
	}
	for _, ch := range r.LiveSHA256 {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return "rereply-plan incomplete"
		}
	}
	if len(r.SeedDriftIDs) > 10 || !sort.StringsAreSorted(r.SeedDriftIDs) {
		return "rereply-plan incomplete"
	}
	for i, id := range r.SeedDriftIDs {
		if !publicPlanSeedID(id) || (i > 0 && r.SeedDriftIDs[i-1] == id) {
			return "rereply-plan incomplete"
		}
	}
	// Plan compares each bucket independently first. Sum only positive excesses
	// and the source-defined schema excess; decreases can never offset drift.
	unknown := r.OtherSchemaExcess
	const maximumCount int64 = 1<<63 - 1
	for _, bucket := range r.UnknownExcess {
		if bucket.Count <= 0 || bucket.Count > maximumCount-unknown {
			return "rereply-plan incomplete"
		}
		unknown += bucket.Count
	}
	line := fmt.Sprintf("rereply-plan catalog_version=%d live_sha256=%s pending=%d drift=%d unknown=%d seed_drift_steps=%s", r.CatalogVersion, r.LiveSHA256[:12], len(r.PendingIDs), len(r.KnownDrift), unknown, strings.Join(r.SeedDriftIDs, ","))
	if len(line) > 1024 || !o.deadline.alive(o.parent) {
		return "rereply-plan incomplete"
	}
	return line
}

func publicPlanSeedID(id string) bool {
	switch id {
	case "SeedPermissionsAndRoles", "SeedSystemRolesForAllOrgs", "SeedSystemRolesForAllOrgs/seedSystemRolesForOrg",
		"SeedSystemRolesForAllOrgs/FixSystemRolePermissions", "SeedSystemRolesForAllOrgs/ApplyManagerSettingsPolicyMigration",
		"SeedSystemRolesForAllOrgs/MigrateExistingUserRoles", "SeedSystemRolesForAllOrgs/legacy-super-admin-update",
		"EnsurePlatformReseller", "EnsureReReplyProductCatalog", "SeedDefaultWidgets":
		return true
	}
	return false
}
