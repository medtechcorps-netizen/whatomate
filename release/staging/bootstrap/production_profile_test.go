package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"go/format"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func repositoryProductionProfile() (productionProfile, []byte, error) {
	return readProductionProfile(
		filepath.Join(repositoryRoot, "internal", "dbcatalog", "golden", "production-v0.json"),
		filepath.Join(repositoryRoot, "internal", "dbcatalog", "shape", "production-v0.sql"))
}

func TestProductionProfilePrecreationMatchesPR4(t *testing.T) {
	// The two packages cannot share a test-only implementation. Compare the
	// complete function AST, including source types/defaults/PK/escaping, so
	// the staging implementation cannot drift from the reviewed exporter.
	exporter := parseGoFile(t, filepath.Join(repositoryRoot, "internal", "database", "catalog_production_shape_test.go"))
	staging := parseGoFile(t, "production_profile.go")
	var want, got bytes.Buffer
	require.NoError(t, format.Node(&want, token.NewFileSet(), exporter.function(t, "productionPrecreationPlans")))
	require.NoError(t, format.Node(&got, token.NewFileSet(), staging.function(t, "productionPrecreationPlans")))
	require.Equal(t, want.String(), got.String())
	profile, overlay, err := repositoryProductionProfile()
	require.NoError(t, err)
	require.Equal(t, "1c85a9ad0b9bb652ea21b51faa2ee887592dacb00c7f4b388de5ef5d6085a4e7", profile.LiveSHA256)
	require.Len(t, profile.BootstrapColumnOrder, 8)
	require.NotEmpty(t, overlay)
}

func TestProductionProfileImageAndTriggerWiring(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join(repositoryRoot, "docker", "staging", "bootstrap.Dockerfile"))
	require.NoError(t, err)
	workflow, err := os.ReadFile(filepath.Join(repositoryRoot, ".github", "workflows", "staging-images.yml"))
	require.NoError(t, err)
	for _, pair := range [][2]string{
		{"internal/dbcatalog/golden/production-v0.json", "/production-shape/production-v0.json"},
		{"internal/dbcatalog/shape/production-v0.sql", "/production-shape/production-v0.sql"},
	} {
		require.Equal(t, 1, strings.Count(string(dockerfile), "COPY "+pair[0]+" "+pair[1]+"\n"))
		require.Equal(t, 1, strings.Count(string(workflow), "      - "+pair[0]+"\n"))
	}
}

func TestProductionProfileFilesFailClosed(t *testing.T) {
	profile, err := os.ReadFile(filepath.Join(repositoryRoot, "internal", "dbcatalog", "golden", "production-v0.json"))
	require.NoError(t, err)
	overlay, err := os.ReadFile(filepath.Join(repositoryRoot, "internal", "dbcatalog", "shape", "production-v0.sql"))
	require.NoError(t, err)
	for _, mode := range []string{"missing-profile", "missing-overlay", "profile-directory", "profile-mutated", "profile-trailing", "profile-oversized", "overlay-SQL", "overlay-oversized"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			p, o := filepath.Join(dir, "production-v0.json"), filepath.Join(dir, "production-v0.sql")
			require.NoError(t, os.WriteFile(p, profile, 0600))
			require.NoError(t, os.WriteFile(o, overlay, 0600))
			switch mode {
			case "missing-profile":
				require.NoError(t, os.Remove(p))
			case "missing-overlay":
				require.NoError(t, os.Remove(o))
			case "profile-directory":
				require.NoError(t, os.Remove(p))
				require.NoError(t, os.Mkdir(p, 0700))
			case "profile-mutated":
				require.NoError(t, os.WriteFile(p, bytes.Replace(profile, []byte(`"version": 0`), []byte(`"version": 1`), 1), 0600))
			case "profile-trailing":
				require.NoError(t, os.WriteFile(p, append(append([]byte{}, profile...), []byte("{}")...), 0600))
			case "profile-oversized":
				require.NoError(t, os.WriteFile(p, bytes.Repeat([]byte("x"), productionProfileLimit+1), 0600))
			case "overlay-SQL":
				require.NoError(t, os.WriteFile(o, append(append([]byte{}, overlay...), []byte("CREATE TABLE unwanted(id int);")...), 0600))
			case "overlay-oversized":
				require.NoError(t, os.WriteFile(o, bytes.Repeat([]byte("x"), productionOverlayLimit+1), 0600))
			}
			_, _, err := readProductionProfile(p, o)
			require.Error(t, err)
			require.NotContains(t, err.Error(), dir)
			require.NotContains(t, err.Error(), "unwanted")
		})
	}
}

func TestProductionProfileRefusesBeforeAnyWrite(t *testing.T) {
	shape := newDOShape(t)
	path := writeConfig(t, "test", shape.runtimeURL(), shape.ownerURL(), shape.runtime, testAdminPassword)
	before := shape.publicObjects(shape.database)
	for _, mode := range []string{"unavailable", "missing-final-column", "duplicate-final-column", "foreign-final-column", "duplicate-final-table", "missing-table"} {
		t.Run(mode, func(t *testing.T) {
			load := func() (productionProfile, []byte, error) {
				p, overlay, err := repositoryProductionProfile()
				if err != nil {
					return p, overlay, err
				}
				i := len(p.BootstrapColumnOrder) - 1
				switch mode {
				case "unavailable":
					return p, nil, errors.New("synthetic-private-path-secret")
				case "missing-final-column":
					p.BootstrapColumnOrder[i].Columns = p.BootstrapColumnOrder[i].Columns[1:]
				case "duplicate-final-column":
					p.BootstrapColumnOrder[i].Columns[1] = p.BootstrapColumnOrder[i].Columns[0]
				case "foreign-final-column":
					p.BootstrapColumnOrder[i].Columns[0] = "unmapped"
				case "duplicate-final-table":
					p.BootstrapColumnOrder[i].Table = p.BootstrapColumnOrder[0].Table
				case "missing-table":
					p.BootstrapColumnOrder = p.BootstrapColumnOrder[:i]
				}
				return p, overlay, nil
			}
			var stdout, stderr bytes.Buffer
			code := runWithProfile([]string{"-config", path}, &stdout, &stderr, load)
			require.Equal(t, exitRefused, code)
			require.Empty(t, stdout.String())
			require.Contains(t, stderr.String(), "production profile")
			require.NotContains(t, stderr.String(), "synthetic-private-path-secret")
			require.Equal(t, before, shape.publicObjects(shape.database), "all plans must be validated before the first CREATE")
		})
	}
}

func TestProductionProfileRefusesNamespaceObject(t *testing.T) {
	shape := newDOShape(t)
	require.NoError(t, shape.open(shape.ownerURL()).Exec(`CREATE COLLATION public.synthetic_profile_existing FROM pg_catalog."C"`).Error)
	called := false
	var stdout, stderr bytes.Buffer
	code := runWithProfile([]string{"-config", writeConfig(t, "test", shape.runtimeURL(), shape.ownerURL(), shape.runtime, testAdminPassword)}, &stdout, &stderr,
		func() (productionProfile, []byte, error) { called = true; return repositoryProductionProfile() })
	require.Equal(t, exitRefused, code)
	require.Contains(t, stderr.String(), "public schema is not empty namespace dependencies=1")
	require.False(t, called, "empty and authority guards precede profile loading/precreation")
	require.EqualValues(t, 0, shape.publicObjects(shape.database).Relations)
}

func TestProductionProfileBootstrapMatchesObservedCatalog(t *testing.T) {
	shape := newDOShape(t)
	path := writeConfig(t, "test", shape.runtimeURL(), shape.ownerURL(), shape.runtime, testAdminPassword)
	dir := t.TempDir()
	binary := filepath.Join(dir, "bootstrap")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	// Exercise the actual fixed executable-relative loader, not only its unit
	// test seam. All dependencies are already cached; this test may not fetch.
	childEnv := []string{}
	for _, entry := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(entry, "=", 2)[0])
		if !strings.HasPrefix(key, "WHATOMATE_") && !strings.HasPrefix(key, "PG") && key != "GOPROXY" && key != "GOSUMDB" {
			childEnv = append(childEnv, entry)
		}
	}
	childEnv = append(childEnv, "GOPROXY=off", "GOSUMDB=off")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", binary, "./release/staging/bootstrap")
	build.Dir, build.Env, build.WaitDelay = repositoryRoot, childEnv, 2*time.Second
	output, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic bootstrap build failed: bytes=%d sha256=%x", len(output), sha256.Sum256(output))
	}
	runBinary := func() (int, *bytes.Buffer, *bytes.Buffer) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		var stdout, stderr bytes.Buffer
		command := exec.CommandContext(ctx, binary, "-config", path)
		command.Dir, command.Env, command.WaitDelay = t.TempDir(), childEnv, 2*time.Second
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || ctx.Err() != nil {
				t.Fatal("synthetic bootstrap child unavailable or timed out")
			}
			code = exit.ExitCode()
		}
		return code, &stdout, &stderr
	}
	code, stdout, stderr := runBinary()
	require.Equal(t, exitRefused, code)
	require.Empty(t, stdout.String())
	require.Equal(t, logPrefix+"refusing: production profile or overlay is unavailable or invalid\n", stderr.String())
	require.Equal(t, publicSchema{Schemas: 1}, shape.publicObjects(shape.database))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "production-shape"), 0700))
	for _, pair := range [][2]string{{"golden", "production-v0.json"}, {"shape", "production-v0.sql"}} {
		raw, err := os.ReadFile(filepath.Join(repositoryRoot, "internal", "dbcatalog", pair[0], pair[1]))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "production-shape", pair[1]), raw, 0600))
	}
	code, stdout, stderr = runBinary()
	if code != exitComplete {
		t.Fatalf("staging profile bootstrap failed: exit=%d private-output-bytes=%d sha256=%x", code, stderr.Len(), sha256.Sum256(stderr.Bytes()))
	}
	require.Contains(t, stdout.String(), "bootstrap complete")
	p, overlay, err := repositoryProductionProfile()
	require.NoError(t, err)
	known, err := dbcatalog.BuiltinCatalog()
	require.NoError(t, err)
	globals, err := dbcatalog.BuiltinGlobalTables()
	require.NoError(t, err)
	capture := func() dbcatalog.Snapshot {
		snapshot, err := dbcatalog.Capture(context.Background(), shape.runtimeURL(), dbcatalog.Options{Catalog: known, GlobalTables: globals, IncludeSeeds: true, Timeout: 45 * time.Second})
		require.NoError(t, err)
		return snapshot
	}
	before := capture()
	require.Equal(t, p.LiveSHA256, before.SHA256)
	require.Empty(t, before.Unknown)
	require.Len(t, before.Seeds, 1)
	require.EqualValues(t, len(models.DefaultPermissions()), before.Seeds[0].Count, "retain the current source seed set, not production's data drift")
	// Applying the accepted no-op twice cannot change either structural or seed evidence.
	owner := shape.open(shape.ownerURL())
	for range 2 {
		require.NoError(t, owner.Exec(string(overlay)).Error)
		after := capture()
		require.Equal(t, before.SHA256, after.SHA256)
		require.Equal(t, before.Objects, after.Objects)
		require.Equal(t, before.Seeds, after.Seeds)
		require.Empty(t, after.Unknown)
	}
	// Force the real coordinator's owner connection read-only. A writable
	// branch would fail, and the callback must never be reached on this shape.
	readOnlyOwner := shape.open(shape.ownerURL("default_transaction_read_only", "on"))
	runtimeDB := shape.open(shape.runtimeURL("default_transaction_read_only", "on"))
	verified := 0
	require.NoError(t, database.RunRLSMigrationCoordinator(readOnlyOwner, &config.DefaultAdminConfig{}, shape.runtime,
		func(*gorm.DB) error { return errors.New("unexpected writable callback") },
		func() error { verified++; return database.VerifyTenantRLS(runtimeDB, shape.runtime) }))
	require.Equal(t, 1, verified)
	code, _, stderr = runBinary()
	require.Equal(t, exitRefused, code)
	require.Contains(t, stderr.String(), "public schema is not empty")
	after := capture()
	require.Equal(t, before.SHA256, after.SHA256)
	require.Equal(t, before.Objects, after.Objects)
	require.Equal(t, before.Seeds, after.Seeds)
	require.Empty(t, after.Unknown)
}
