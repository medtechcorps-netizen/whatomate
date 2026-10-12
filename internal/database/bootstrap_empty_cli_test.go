package database_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/stretchr/testify/require"
)

func TestTenantRLS_CatalogBootstrapCommandActualCLI(t *testing.T) {
	t.Setenv("UPDATE_CATALOG_GOLDEN", "0")
	known, globals, expectedRaw, expectedHash := bootstrapCommandOracle(t, "v0")
	f := catalogEmptyTarget(t)
	cfg := bootstrapCommandFixtureConfig(t, f)
	dir := t.TempDir()
	binary := filepath.Join(dir, "rereply-bootstrap-test")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	configPath := filepath.Join(dir, "owned-bootstrap.toml")
	rawConfig := fmt.Sprintf("[app]\nenvironment = \"test\"\n[database]\nurl = %q\nmigration_url = %q\nruntime_role = %q\nrls_enabled = true\n[default_admin]\nemail = %q\npassword = %q\nfull_name = %q\n",
		cfg.Database.URL, cfg.Database.MigrationURL, cfg.Database.RuntimeRole,
		cfg.DefaultAdmin.Email, cfg.DefaultAdmin.Password, cfg.DefaultAdmin.FullName)
	require.NoError(t, os.WriteFile(configPath, []byte(rawConfig), 0600))
	env := []string{}
	for _, entry := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(entry, "=", 2)[0])
		if strings.HasPrefix(key, "WHATOMATE_") || strings.HasPrefix(key, "PG") ||
			key == "GOPROXY" || key == "GOSUMDB" || key == "GOFLAGS" || key == "GOTOOLCHAIN" {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	run := func(deadline time.Duration, expectedExit int, command string, args ...string) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		child := exec.CommandContext(ctx, command, args...)
		child.Dir, child.Env = "../..", env
		child.WaitDelay = 2 * time.Second
		output := &productionCLIOutput{}
		child.Stdout, child.Stderr = output, output
		err := child.Run()
		raw := append([]byte(nil), output.Bytes()...)
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("bootstrap child did not complete (bytes=%d sha256=%x)", len(raw), sha256.Sum256(raw))
			}
			code = exit.ExitCode()
		}
		if code != expectedExit || ctx.Err() != nil {
			t.Fatalf("bootstrap child exit mismatch (exit=%d bytes=%d sha256=%x)", code, len(raw), sha256.Sum256(raw))
		}
		for _, secret := range []string{cfg.Database.URL, cfg.Database.MigrationURL, cfg.DefaultAdmin.Password,
			catalogTemplate.ownerPassword, catalogTemplate.password} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatal("bootstrap child disclosed owned synthetic connection/password material")
			}
		}
		return raw
	}
	run(3*time.Minute, 0, "go", "build", "-mod=readonly", "-buildvcs=false", "-o", binary, "./cmd/whatomate")

	// Track only these fixture roles in this owned database, not unrelated
	// cluster sessions. A bounded post-exit observation permits normal backend
	// teardown latency; it is not a physical Close deadline contract.
	backendPIDs := func(ctx context.Context) []int64 {
		var pids []int64
		require.NoError(t, f.owner.WithContext(ctx).Raw(`SELECT pid::bigint FROM pg_catalog.pg_stat_activity
			WHERE datname=? AND usename IN (?,?) ORDER BY pid`, f.name, catalogTemplate.owner, catalogTemplate.runtime).Scan(&pids).Error)
		return pids
	}
	baseline := backendPIDs(context.Background())
	require.Len(t, baseline, 2)
	waitForOwnedChildExit := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for {
			if reflect.DeepEqual(baseline, backendPIDs(ctx)) {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("bootstrap child left owned role backends after its observation window")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	output := run(2*time.Minute, 0, binary, "rls-bootstrap-empty", "-config", configPath, "-production-shape", "v0")
	require.Equal(t, 1, bytes.Count(output, []byte("rls-bootstrap-empty complete\n")), "fixed CLI success marker missing or duplicated")
	waitForOwnedChildExit()
	actual := captureSyntheticCatalog(t, f, known, globals)
	actualRaw, err := dbcatalog.CanonicalCatalog(dbcatalog.Catalog{Version: 0, Objects: actual.Objects})
	require.NoError(t, err)
	require.Equal(t, expectedRaw, actualRaw)
	require.Equal(t, expectedHash, actual.SHA256)
	require.Empty(t, actual.Unknown)
	require.Equal(t, []dbcatalog.Seed{sourcePermissionSeed(t, f.runtime)}, actual.Seeds)
	bootstrapCommandAdminAndBusinessState(t, f.owner, cfg.DefaultAdmin)
	require.NoError(t, database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime))
	beforeRows := bootstrapCommandRows(t, f.owner, known)
	output = run(time.Minute, 1, binary, "rls-bootstrap-empty", "-config", configPath, "-production-shape=v0")
	require.True(t, bytes.Equal(output, []byte("rls-bootstrap-empty failed; the database may be partially initialized; inspect before retrying\n")), "repeat refusal must emit only its fixed sanitized diagnostic")
	waitForOwnedChildExit()
	require.Equal(t, actual, captureSyntheticCatalog(t, f, known, globals))
	require.Equal(t, beforeRows, bootstrapCommandRows(t, f.owner, known))
}
