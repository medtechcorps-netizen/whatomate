package database_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestTenantRLS_CatalogBootstrapCommandRuntimeBackendFailureRemainsFailure(t *testing.T) {
	for _, phase := range []string{"verifier", "restoration"} {
		t.Run(phase, func(t *testing.T) {
			t.Setenv("UPDATE_CATALOG_GOLDEN", "0")
			known, globals, expectedRaw, expectedHash := bootstrapCommandOracle(t, "")
			f := catalogEmptyTarget(t)
			cfg := bootstrapCommandFixtureConfig(t, f)
			original := bootstrapCommandSetOriginalName(t, f.runtime, "runtime-failure-original")
			ownerBefore, err := bootstrapCommandReadSession(context.Background(), f.owner.Statement.ConnPool)
			require.NoError(t, err)
			adminURL := *catalogTemplate.base
			adminURL.Path, adminURL.RawPath = "/"+f.name, ""
			admin, err := catalogConnect(&adminURL)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, catalogClose(admin)) })
			var connectedDatabase string
			var superuser bool
			require.NoError(t, admin.Raw("SELECT pg_catalog.current_database()").Scan(&connectedDatabase).Error)
			require.Equal(t, f.name, connectedDatabase)
			require.NoError(t, admin.Raw("SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname=current_user").Scan(&superuser).Error)
			require.True(t, superuser)
			contender, err := catalogConnect(catalogRoleURL(catalogTemplate.base, f.name, catalogTemplate.owner, catalogTemplate.ownerPassword))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, catalogClose(contender)) })

			var once sync.Once
			var mu sync.Mutex
			verifyCalls, restoreCalls, killCalls := 0, 0, 0
			var faultErr error
			var killed bool
			require.NoError(t, f.runtime.Callback().Row().Before("gorm:row").Register("pr7:owned_backend_failure", func(tx *gorm.DB) {
				statement := tx.Statement.SQL.String()
				isVerifier := strings.Contains(statement, "WITH backend_identity AS (") && strings.Contains(statement, "runtime_identity AS (")
				isRestore := statement == "SELECT pg_catalog.set_config('application_name', $1, false)" &&
					len(tx.Statement.Vars) == 1 && tx.Statement.Vars[0] == original.ApplicationName
				mu.Lock()
				if isVerifier {
					verifyCalls++
				}
				if isRestore {
					restoreCalls++
				}
				mu.Unlock()
				if (phase != "verifier" || !isVerifier) && (phase != "restoration" || !isRestore) {
					return
				}
				once.Do(func() {
					// Read the real statement connection first. The administrator may
					// terminate only that precise backend in this newly owned database.
					state, readErr := bootstrapCommandReadSession(tx.Statement.Context, tx.Statement.ConnPool)
					if readErr != nil || state.PID != original.PID || !state.Started.Equal(original.Started) ||
						state.Role != catalogTemplate.runtime || !strings.HasPrefix(state.ApplicationName, "rereply-bootstrap-") {
						mu.Lock()
						faultErr = errors.Join(readErr, errors.New("owned runtime termination identity refused"))
						mu.Unlock()
						return
					}
					ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
					defer cancel()
					var terminated []bool
					read := admin.WithContext(ctx).Raw(`SELECT pg_catalog.pg_terminate_backend(activity.pid, 5000)
						FROM pg_catalog.pg_stat_activity activity
						WHERE activity.pid=? AND activity.backend_start=? AND activity.datname=?
							AND activity.usename=? AND activity.application_name=?
							AND activity.backend_type='client backend' AND activity.pid<>pg_catalog.pg_backend_pid()`,
						state.PID, state.Started, f.name, catalogTemplate.runtime, state.ApplicationName).Scan(&terminated)
					mu.Lock()
					killCalls++
					faultErr = read.Error
					killed = read.RowsAffected == 1 && len(terminated) == 1 && terminated[0]
					mu.Unlock()
				})
				// No tx.Error assignment or synthetic returned error: the unchanged
				// product query now observes the real terminated connection.
			}))
			calls := 0
			err = database.BootstrapEmptyDatabase(f.owner, f.runtime, cfg, "", nil, nil,
				bootstrapCommandRealBackfills(&calls))
			require.NoError(t, faultErr)
			require.True(t, killed)
			require.Equal(t, 1, killCalls)
			require.Equal(t, 1, verifyCalls)
			require.Equal(t, 1, restoreCalls)
			require.Equal(t, 1, calls, "both real backfills completed before the runtime fault")
			require.Error(t, err)
			require.ErrorContains(t, err, "restore bootstrap application_name")
			require.ErrorContains(t, err, "empty bootstrap failed after a persistent phase began; partial state may remain")
			if phase == "verifier" {
				require.ErrorContains(t, err, "read database roles")
			} else {
				require.NotContains(t, err.Error(), "read database roles")
			}
			// Driver error wrapping may differ between a received PG fatal response
			// and an already closed socket. Retain a safe code if actually present,
			// without manufacturing SQLSTATE or accepting a substituted verdict.
			var serverError *pgconn.PgError
			if errors.As(err, &serverError) {
				t.Logf("observed runtime termination SQLSTATE=%s", serverError.Code)
			} else {
				t.Log("observed runtime termination without a retained PostgreSQL error object")
			}
			var oldBackendRows int64
			require.NoError(t, admin.Raw(`SELECT COUNT(*) FROM pg_catalog.pg_stat_activity
				WHERE pid=? AND backend_start=? AND datname=? AND usename=?`, original.PID, original.Started,
				f.name, catalogTemplate.runtime).Scan(&oldBackendRows).Error)
			require.Zero(t, oldBackendRows)
			acquired, holders, lockErr := bootstrapCommandProbeLock(context.Background(), contender, ownerBefore.PID)
			require.NoError(t, lockErr)
			require.True(t, acquired)
			require.Zero(t, holders)
			for _, db := range []*gorm.DB{f.owner, f.runtime} {
				pool, poolErr := db.DB()
				require.NoError(t, poolErr)
				require.Zero(t, pool.Stats().InUse)
			}
			// The partial-state error is conservative: all persistent phases had
			// actually finished here. A new independent capture proves their state;
			// it does not relabel the failed command as a successful bootstrap.
			actual := captureSyntheticCatalog(t, f, known, globals)
			actualRaw, captureErr := dbcatalog.CanonicalCatalog(dbcatalog.Catalog{Version: 0, Objects: actual.Objects})
			require.NoError(t, captureErr)
			require.Equal(t, expectedRaw, actualRaw)
			require.Equal(t, expectedHash, actual.SHA256)
			require.Empty(t, actual.Unknown)
			require.Equal(t, []dbcatalog.Seed{sourcePermissionSeed(t, f.runtime)}, actual.Seeds)
			beforeRows := bootstrapCommandRows(t, f.owner, known)
			err = database.BootstrapEmptyDatabase(f.owner, f.runtime, cfg, "", nil, nil,
				bootstrapCommandRealBackfills(&calls))
			require.EqualError(t, err, "bootstrap public schema is not empty")
			require.Equal(t, 1, calls)
			require.Equal(t, actual, captureSyntheticCatalog(t, f, known, globals))
			require.Equal(t, beforeRows, bootstrapCommandRows(t, f.owner, known))
		})
	}
}
