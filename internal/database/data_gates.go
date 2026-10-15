package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These are observations, not repair predicates or permission to apply data.
// In particular the second count is contact groups and the third is messages.
const dataGateCandidateSQL = `SELECT pg_catalog.count(*)::bigint
FROM public.contacts AS c
WHERE pg_catalog.btrim(c.phone_number, U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000') IN ('', '+')
OR pg_catalog.jsonb_exists(c.metadata, 'empty_phone_upgraded_from')`

const dataGateMixedSQL = `SELECT pg_catalog.count(*)::bigint FROM (
 SELECT m.organization_id, m.contact_id
 FROM public.messages AS m
 JOIN public.contacts AS c ON c.organization_id = m.organization_id AND c.id = m.contact_id
 JOIN public.scheduled_jobs AS j ON j.organization_id = m.organization_id
  AND j.kind = 'inbound_message.continuation' AND j.aggregate_id = m.id
 WHERE m.direction = 'incoming'
 GROUP BY m.organization_id, m.contact_id
 HAVING pg_catalog.count(DISTINCT COALESCE(NULLIF(j.payload -> 'message' ->> 'from_user_id', ''),
  NULLIF(j.payload -> 'message' ->> 'from_parent_user_id', ''))) > 1
) AS mixed_contacts`

const dataGateMissingSQL = `SELECT pg_catalog.count(*)::bigint
FROM public.messages AS m
JOIN public.contacts AS c ON c.organization_id = m.organization_id AND c.id = m.contact_id
WHERE m.direction = 'incoming'
AND (pg_catalog.btrim(c.phone_number, U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000') IN ('', '+')
 OR pg_catalog.jsonb_exists(c.metadata, 'empty_phone_upgraded_from'))
AND NOT EXISTS (SELECT 1 FROM public.scheduled_jobs AS j
 WHERE j.organization_id = m.organization_id AND j.kind = 'inbound_message.continuation' AND j.aggregate_id = m.id)`

type dataGateObservation struct {
	id    string
	count int64
	known bool
}

func dataGateObservations() [3]dataGateObservation {
	return [3]dataGateObservation{
		{id: "empty-phone-legacy-contacts"},
		{id: "empty-phone-mixed-sender-contacts"},
		{id: "empty-phone-missing-continuation-messages"},
	}
}

// observeMigrationDataGates is called only inside the coordinator's held
// connection, before profile selection. Raw transaction control is deliberate:
// database/sql Tx.Rollback has no cleanup context, and cancellation of BeginTx's
// context would schedule an independent rollback on this shared connection.
// The enclosing Connection owns the lease; this function never acquires one.
func observeMigrationDataGates(session *gorm.DB, runtimeRole string) (returnErr error) {
	observations := dataGateObservations()
	defer func() {
		for _, observation := range observations {
			if observation.known && returnErr == nil {
				fmt.Printf("data-gate %s count=%d mode=observe\n", observation.id, observation.count)
			} else {
				fmt.Printf("data-gate %s error\n", observation.id)
			}
		}
	}()
	if session == nil || session.Statement == nil {
		return errors.New("data-gate session unavailable")
	}
	if _, ok := session.Statement.ConnPool.(*sql.Conn); !ok {
		return errors.New("data-gate requires held migration connection")
	}
	parent := session.Statement.Context
	if parent == nil {
		parent = context.Background()
	}
	start := time.Now()
	finalAt := start.Add(17 * time.Second)
	if at, ok := parent.Deadline(); ok && at.Before(finalAt) {
		finalAt = at
	}
	workAt := start.Add(15 * time.Second)
	if at := finalAt.Add(-2 * time.Second); at.Before(workAt) {
		workAt = at
	}
	ctx, cancel := context.WithDeadline(parent, workAt)
	defer cancel()
	// Silence SQL and driver detail, including errors containing row values.
	db := session.Session(&gorm.Session{NewDB: true, Logger: logger.Discard}).WithContext(ctx)
	var before dataGateSessionSettings
	if err := readDataGateSettings(db, &before); err != nil || !before.MigrationLockHeld {
		// Without a healthy baseline we cannot prove restoration of this lease.
		return errors.New("data-gate session baseline unavailable")
	}
	defer func() {
		end := time.Now().Add(2 * time.Second)
		if finalAt.Before(end) {
			end = finalAt
		}
		cleanup, done := context.WithDeadline(context.Background(), end)
		defer done()
		cleanDB := session.Session(&gorm.Session{NewDB: true, Logger: logger.Discard}).WithContext(cleanup)
		// Also run after an ambiguous BEGIN failure: never pass an unknown
		// transaction state to a mutation branch.
		if cleanDB.Exec("ROLLBACK").Error != nil {
			returnErr = errors.New("data-gate rollback failed")
			return
		}
		var after dataGateSessionSettings
		if readDataGateSettings(cleanDB, &after) != nil || before != after || cleanup.Err() != nil {
			returnErr = errors.New("data-gate session restoration failed")
		}
	}()
	if ctx.Err() != nil {
		return nil
	}
	if db.Exec("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY").Error != nil {
		return nil
	}
	for _, setting := range []struct {
		name    string
		maximum time.Duration
	}{
		{"statement_timeout", 3 * time.Second}, {"lock_timeout", time.Second}, {"idle_in_transaction_session_timeout", 5 * time.Second},
	} {
		limit := time.Until(workAt)
		if limit > setting.maximum {
			limit = setting.maximum
		}
		// PostgreSQL zero disables a timeout: never round a positive remainder
		// down to zero or reset the original observer deadline.
		if limit < time.Millisecond {
			return nil
		}
		if db.Exec("SELECT pg_catalog.set_config(?, ?, true)", setting.name, fmt.Sprintf("%dms", limit.Milliseconds())).Error != nil {
			return nil
		}
	}
	var eligible bool
	// A managed migration owner may have CREATEROLE/BYPASSRLS, but only the
	// native authenticated database owner on this exact held coordinator lease
	// may use that path. Restricted schema/table owners retain their old path.
	if db.Raw(`SELECT current_user = session_user AND current_user <> CAST(? AS text)
 AND NOT r.rolsuper AND NOT r.rolreplication
 AND ((NOT r.rolbypassrls AND NOT r.rolcreaterole) OR
  EXISTS(SELECT 1 FROM pg_catalog.pg_database d WHERE d.datname=pg_catalog.current_database() AND d.datdba=r.oid))
 AND r.oid::bigint=CAST(? AS bigint)
 AND (SELECT d.oid::bigint FROM pg_catalog.pg_database d WHERE d.datname=pg_catalog.current_database())=CAST(? AS bigint)
 AND pg_catalog.pg_backend_pid()=CAST(? AS integer)
 AND EXISTS(SELECT 1 FROM pg_catalog.pg_locks l WHERE l.pid=pg_catalog.pg_backend_pid()
  AND l.locktype='advisory' AND l.mode='ExclusiveLock' AND l.granted AND l.objsubid=2
  AND l.database=(SELECT oid FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database())
  AND l.classid::bigint=(pg_catalog.hashtext(pg_catalog.current_database())::bigint & 4294967295)
  AND l.objid=CAST(? AS oid))
 AND pg_catalog.current_setting('transaction_read_only') = 'on'
 AND pg_catalog.current_setting('transaction_isolation') = 'repeatable read'
 AND pg_catalog.current_setting('server_encoding') = 'UTF8'
 AND pg_catalog.current_setting('session_replication_role') = 'origin'
 FROM pg_catalog.pg_roles AS r WHERE r.rolname = current_user`, runtimeRole, before.RoleOID, before.DatabaseOID, before.PID, migrationAdvisoryLockNamespace).Row().Scan(&eligible) != nil || !eligible {
		return nil
	}
	for i := range observations {
		if ctx.Err() != nil {
			break
		}
		if db.Exec("SAVEPOINT rereply_data_gate").Error != nil {
			return errors.New("data-gate savepoint failed")
		}
		tables := []string{"contacts", "messages", "scheduled_jobs"}
		query := dataGateMixedSQL
		if i == 0 {
			tables = []string{"contacts"}
			query = dataGateCandidateSQL
		}
		if i == 2 {
			query = dataGateMissingSQL
		}
		err := verifyCanonicalTenantPolicyInventory(db, tables, runtimeRole)
		var count int64
		if err == nil {
			err = db.Raw(query).Row().Scan(&count)
		}
		if err != nil || count < 0 {
			if db.Exec("ROLLBACK TO SAVEPOINT rereply_data_gate").Error != nil {
				return errors.New("data-gate savepoint recovery failed")
			}
		} else {
			observations[i].count = count
			observations[i].known = true
		}
		if db.Exec("RELEASE SAVEPOINT rereply_data_gate").Error != nil {
			return errors.New("data-gate savepoint release failed")
		}
	}
	return nil
}

type dataGateSessionSettings struct {
	PID               int
	RoleOID           int64
	AuthenticatedRole string
	DatabaseOID       int64
	SearchPath        string
	Replication       string
	StatementTimeout  string
	LockTimeout       string
	IdleTimeout       string
	ReadOnly          string
	Isolation         string
	MigrationLockHeld bool
}

func readDataGateSettings(db *gorm.DB, state *dataGateSessionSettings) error {
	return db.Raw(`SELECT pg_catalog.pg_backend_pid(),
 (SELECT oid::bigint FROM pg_catalog.pg_roles WHERE rolname=current_user),session_user::text,
 (SELECT oid::bigint FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database()),
 pg_catalog.current_setting('search_path'),
 pg_catalog.current_setting('session_replication_role'), pg_catalog.current_setting('statement_timeout'),
 pg_catalog.current_setting('lock_timeout'), pg_catalog.current_setting('idle_in_transaction_session_timeout'),
 pg_catalog.current_setting('transaction_read_only'), pg_catalog.current_setting('transaction_isolation'),
 EXISTS(SELECT 1 FROM pg_catalog.pg_locks l WHERE l.pid=pg_catalog.pg_backend_pid()
 AND l.locktype='advisory' AND l.mode='ExclusiveLock' AND l.granted AND l.objsubid=2
 AND l.database=(SELECT oid FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database())
 AND l.classid::bigint=(pg_catalog.hashtext(pg_catalog.current_database())::bigint & 4294967295)
 AND l.objid=CAST(? AS oid))`, migrationAdvisoryLockNamespace).Row().Scan(
		&state.PID, &state.RoleOID, &state.AuthenticatedRole, &state.DatabaseOID, &state.SearchPath, &state.Replication, &state.StatementTimeout,
		&state.LockTimeout, &state.IdleTimeout, &state.ReadOnly, &state.Isolation, &state.MigrationLockHeld)
}
