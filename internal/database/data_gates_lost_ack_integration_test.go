package database_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This entire adapter is compiled only into database_test. Every query,
// transaction and Commit reaches the real pgx driver. Only one successful
// activation Commit's acknowledgement is replaced with a deterministic error.
var errRoute4LostAck = errors.New("synthetic lost activation commit acknowledgement")

type route4AckState struct {
	mu                                                   sync.Mutex
	activationExecs, activationCommits, acknowledgements int
	revokeOwnerSelect                                    bool
	faultErr                                             error
	committedRows                                        map[string][32]byte
	captureErr                                           error
}

type route4Connector struct {
	driver.Connector
	state *route4AckState
}

func (c *route4Connector) Connect(ctx context.Context) (driver.Conn, error) {
	dc, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	pc, ok := dc.(*stdlib.Conn)
	if !ok {
		_ = dc.Close()
		return nil, errors.New("route4 test requires the real pinned pgx stdlib connection")
	}
	return &route4Conn{Conn: pc, state: c.state}, nil
}

// Embed the pinned concrete driver so PrepareContext, QueryContext, Ping,
// NamedValueChecker and SessionResetter retain their real implementations.
type route4Conn struct {
	*stdlib.Conn
	state  *route4AckState
	active *route4Tx
}

func (c *route4Conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *route4Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	wrapped := &route4Tx{Tx: tx, conn: c, ctx: ctx}
	c.active = wrapped
	return wrapped, nil
}

func (c *route4Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	// These are the same two distinguishing activation markers asserted by
	// splitMigrationIndexes. Assert exactly one real execution below.
	activation := strings.Contains(query, "owner_trigger_installed boolean") &&
		strings.Contains(query, "CREATE TRIGGER rereply_identity_review_contact_selector_fence")
	if activation && (c.active == nil || len(args) != 0) {
		return nil, errors.New("route4 activation was not the expected parameterless transaction statement")
	}
	result, err := c.Conn.ExecContext(ctx, query, args)
	if activation && err == nil {
		c.active.activation = true
		c.state.mu.Lock()
		c.state.activationExecs++
		c.state.mu.Unlock()
	}
	return result, err
}

type route4Tx struct {
	driver.Tx
	conn       *route4Conn
	ctx        context.Context
	activation bool
}

func (tx *route4Tx) Rollback() error {
	defer func() { tx.conn.active = nil }()
	return tx.Tx.Rollback()
}

// Read the same closed twelve tables and exact complete-row JSONB hashes as
// catalogPlanRows, directly on the real connection just after activation commits.
// Raw synthetic rows remain local memory; capture errors never print them.
func route4CommittedRows(ctx context.Context, conn *pgx.Conn) (map[string][32]byte, error) {
	result := map[string][32]byte{}
	for _, table := range []string{
		"permissions", "custom_roles", "role_permissions", "organizations", "users",
		"user_organizations", "resellers", "reseller_members", "plans", "plan_prices",
		"plan_entitlements", "widgets",
	} {
		var rows string
		query := "SELECT COALESCE(jsonb_agg(to_jsonb(s) ORDER BY to_jsonb(s)::text), '[]'::jsonb)::text FROM public." + pgx.Identifier{table}.Sanitize() + " AS s"
		if err := conn.QueryRow(ctx, query).Scan(&rows); err != nil {
			return nil, errors.New("route4 committed row fingerprint failed")
		}
		result[table] = sha256.Sum256([]byte(rows))
	}
	return result, nil
}

func (tx *route4Tx) Commit() error {
	defer func() { tx.conn.active = nil }()
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if !tx.activation {
		return nil
	}
	state := tx.conn.state
	state.mu.Lock()
	state.activationCommits++
	if state.acknowledgements != 0 {
		state.mu.Unlock()
		return errors.New("route4 test observed an unexpected repeated activation commit")
	}
	state.acknowledgements++
	revoke := state.revokeOwnerSelect
	state.mu.Unlock()
	// Normal bridge preparation has already run. Capture its exact persisted
	// result before the fault/ack, so reconciliation may not change any column.
	captureCtx, captureCancel := context.WithTimeout(tx.ctx, 5*time.Second)
	committedRows, captureErr := route4CommittedRows(captureCtx, tx.conn.Conn.Conn())
	captureCancel()
	state.mu.Lock()
	state.committedRows, state.captureErr = committedRows, captureErr
	state.mu.Unlock()
	if captureErr != nil {
		return errors.Join(errRoute4LostAck, captureErr)
	}
	if revoke {
		// This real ACL change is confined to the clone and occurs only AFTER
		// activation actually commits. Runtime ACLs, policies and ownership
		// stay untouched. The production owner predicate sees the real denial.
		ctx, cancel := context.WithTimeout(tx.ctx, 5*time.Second)
		_, err := tx.conn.Conn.Conn().Exec(ctx, "REVOKE SELECT ON public.contacts FROM CURRENT_USER")
		cancel()
		state.mu.Lock()
		state.faultErr = err
		state.mu.Unlock()
		if err != nil {
			return errors.Join(errRoute4LostAck, err)
		}
	}
	return errRoute4LostAck
}

var (
	_ driver.Connector          = (*route4Connector)(nil)
	_ driver.ConnBeginTx        = (*route4Conn)(nil)
	_ driver.ExecerContext      = (*route4Conn)(nil)
	_ driver.QueryerContext     = (*route4Conn)(nil)
	_ driver.ConnPrepareContext = (*route4Conn)(nil)
	_ driver.Pinger             = (*route4Conn)(nil)
	_ driver.NamedValueChecker  = (*route4Conn)(nil)
	_ driver.SessionResetter    = (*route4Conn)(nil)
)

func TestTenantRLS_DataGateActivationLostAcknowledgementRetainsOwnerVerification(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ownerFailure bool
	}{
		{"committed_activation_both_verifiers_healthy", false},
		{"committed_activation_real_owner_failure_runtime_healthy", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := catalogClone(t)
			runtimeRole := catalogTemplate.runtime
			require.NoError(t, database.VerifyTenantRLS(f.runtime, runtimeRole))
			dropIdentityReviewOldCoreTriggers(t, f.owner)
			require.Empty(t, identityReviewOldCoreTriggerBindings(t, f.owner))

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			ownerURL := catalogRoleURL(catalogTemplate.base, f.name, catalogTemplate.owner, catalogTemplate.ownerPassword)
			pc, err := pgx.ParseConfig(ownerURL.String())
			require.NoError(t, err)
			state := &route4AckState{revokeOwnerSelect: tc.ownerFailure}
			pool := sql.OpenDB(&route4Connector{Connector: stdlib.GetConnector(*pc), state: state})
			pool.SetMaxOpenConns(1)
			pool.SetMaxIdleConns(1)
			// Registered after catalogClone: the wrapped pool closes before its
			// owned database is dropped, including on any assertion failure.
			t.Cleanup(func() { require.NoError(t, pool.Close()) })
			owner, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}),
				&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			owner = owner.WithContext(ctx)
			wrappedFixture := f
			wrappedFixture.owner = owner
			beforeSession := dataGateOwnerState(t, wrappedFixture)
			beforeRows := catalogPlanRows(t, f)
			// The owner retains implicit grant authority. Restore the clone's
			// original SELECT privilege even if an assertion stops this subtest.
			if tc.ownerFailure {
				t.Cleanup(func() {
					require.NoError(t, f.owner.Exec("GRANT SELECT ON public.contacts TO "+pgx.Identifier{catalogTemplate.owner}.Sanitize()).Error)
				})
			}
			var originalOwnerSelect, originalRuntimeSelect bool
			require.NoError(t, f.owner.Raw("SELECT pg_catalog.has_table_privilege(current_user, 'public.contacts', 'SELECT')").Scan(&originalOwnerSelect).Error)
			require.NoError(t, f.runtime.Raw("SELECT pg_catalog.has_table_privilege(current_user, 'public.contacts', 'SELECT')").Scan(&originalRuntimeSelect).Error)
			require.True(t, originalOwnerSelect)
			require.True(t, originalRuntimeSelect)

			backfills, verifiers := 0, 0
			var runtimeErr, actualOwnerErr error
			var ownerSelectAtVerification, runtimeSelectAtVerification bool
			output, coordinatorErr := dataGateOutput(t, func() error {
				return database.RunRLSMigrationCoordinatorForTest(owner, &config.DefaultAdminConfig{}, runtimeRole, "bridge",
					func(*gorm.DB) error { backfills++; return nil },
					func() error {
						verifiers++
						// Observe the fault through an independent real owner pool;
						// do not synthesize or substitute the coordinator ownerErr.
						require.NoError(t, f.owner.Raw("SELECT pg_catalog.has_table_privilege(current_user, 'public.contacts', 'SELECT')").Scan(&ownerSelectAtVerification).Error)
						require.NoError(t, f.runtime.Raw("SELECT pg_catalog.has_table_privilege(current_user, 'public.contacts', 'SELECT')").Scan(&runtimeSelectAtVerification).Error)
						actualOwnerErr = database.VerifyPlatformComplianceScanAuthority(f.owner.WithContext(ctx))
						runtimeErr = database.VerifyTenantRLS(f.runtime.WithContext(ctx), runtimeRole)
						return runtimeErr
					})
			})
			require.Contains(t, output, dataGateLines([3]int64{0, 0, 0}), "COUNT observations precede the real bridge mutation")
			require.Equal(t, 1, backfills)
			require.Equal(t, 1, verifiers, "route4 invokes runtime verification even when ownerErr is non-nil")
			require.NoError(t, runtimeErr)
			require.True(t, runtimeSelectAtVerification)
			state.mu.Lock()
			execs, commits, acknowledgements, faultErr := state.activationExecs, state.activationCommits, state.acknowledgements, state.faultErr
			committedRows, captureErr := state.committedRows, state.captureErr
			state.mu.Unlock()
			require.NoError(t, captureErr, "the actual post-Commit pre-ack fingerprint is mandatory")
			require.Len(t, committedRows, 12)
			require.Len(t, beforeRows, 12)
			require.NoError(t, faultErr)
			require.Equal(t, 1, execs)
			require.Equal(t, 1, commits)
			require.Equal(t, 1, acknowledgements)
			if tc.ownerFailure {
				require.False(t, ownerSelectAtVerification)
				require.ErrorIs(t, actualOwnerErr, database.ErrPlatformComplianceScanAuthority)
				require.ErrorIs(t, coordinatorErr, database.ErrRLSMigrationCommitOutcomeIndeterminate)
				require.ErrorIs(t, coordinatorErr, errRoute4LostAck)
				require.ErrorIs(t, coordinatorErr, database.ErrPlatformComplianceScanAuthority,
					"the real coordinator ownerErr must be retained even when runtime verification succeeds")
			} else {
				require.True(t, ownerSelectAtVerification)
				require.NoError(t, actualOwnerErr)
				require.NoError(t, coordinatorErr, "both proofs reconcile the genuine committed activation")
			}
			require.Equal(t, expectedIdentityReviewOldCoreTriggerBindings(), identityReviewOldCoreTriggerBindings(t, f.owner))
			if tc.ownerFailure {
				require.NoError(t, f.owner.Exec("GRANT SELECT ON public.contacts TO "+pgx.Identifier{catalogTemplate.owner}.Sanitize()).Error)
			}
			require.NoError(t, database.VerifyPlatformComplianceScanAuthority(f.owner))
			require.NoError(t, database.VerifyTenantRLS(f.runtime, runtimeRole))
			afterRows := catalogPlanRows(t, f)
			require.Equal(t, committedRows, afterRows, "reconciliation must preserve every full row, including reseller_members.updated_at")
			for table, before := range beforeRows {
				require.Contains(t, committedRows, table)
				if table != "reseller_members" {
					require.Equal(t, before, committedRows[table], "normal bridge unexpectedly changed rows in %s", table)
				}
			}
			// EnsurePlatformReseller intentionally updates membership updated_at
			// during normal bridge preparation. Its complete rows are therefore
			// compared at the post-Commit boundary above, with no field excluded.
			require.Equal(t, beforeSession, dataGateOwnerState(t, wrappedFixture), "same real session restored and migration interlock released")
		})
	}
}
