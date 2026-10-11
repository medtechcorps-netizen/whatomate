package database_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

type productionAttributeDelta struct {
	Golden     string `json:"golden"`
	Production string `json:"production"`
}

type productionDelta struct {
	Kind        string                              `json:"kind"`
	Identity    string                              `json:"identity"`
	Disposition string                              `json:"disposition"`
	Attributes  map[string]productionAttributeDelta `json:"attributes"`
}

type productionColumnOrder struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
}

type productionProfile struct {
	Version              int                      `json:"version"`
	LiveSHA256           string                   `json:"live_sha256"`
	GoldenSHA256         string                   `json:"golden_sha256"`
	ReleasedSource       string                   `json:"released_source"`
	Delta                []productionDelta        `json:"delta"`
	AcceptedUnpublished  []dbcatalog.UnknownCount `json:"accepted_unpublished"`
	OtherSchemas         int                      `json:"n_other_schemas"`
	BootstrapColumnOrder []productionColumnOrder  `json:"bootstrap_column_order"`
}

func loadProductionProfile() (productionProfile, error) {
	var p productionProfile
	raw, err := os.ReadFile(filepath.Join(catalogGoldenDirectory, "production-v0.json"))
	if err != nil {
		return p, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&p); err != nil {
		return p, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return p, errors.New("production profile trailing content")
	}
	canonical, err := json.MarshalIndent(p, "", "  ")
	if err != nil || !bytes.Equal(append(canonical, '\n'), raw) {
		return p, errors.New("production profile must be canonical and duplicate-free")
	}
	if p.Version != 0 || p.LiveSHA256 != "1c85a9ad0b9bb652ea21b51faa2ee887592dacb00c7f4b388de5ef5d6085a4e7" ||
		p.GoldenSHA256 != "17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8" ||
		p.ReleasedSource != "e0016b3595114728e7864da2adb44c29065f25ef" || p.OtherSchemas != 7 ||
		len(p.AcceptedUnpublished) != 0 || len(p.Delta) != 72 || len(p.BootstrapColumnOrder) != 8 {
		return p, errors.New("production profile observation binding")
	}
	return p, nil
}

type productionTablePlan struct {
	sql  string
	args []any
}

// Build every statement from the current source models before any write. The
// observation contributes only already-known column names and their order;
// types/defaults/primary keys come from GORM's pinned model schema. Constraints,
// indexes and RLS are then installed by the unchanged migration sequence.
func productionPrecreationPlans(owner *gorm.DB, p productionProfile) ([]productionTablePlan, error) {
	modelsByTable := map[string]*schema.Schema{}
	for _, model := range database.GetMigrationModels() {
		statement := &gorm.Statement{DB: owner}
		if err := statement.Parse(model.Model); err != nil {
			return nil, err
		}
		modelsByTable[statement.Schema.Table] = statement.Schema
	}
	if len(p.BootstrapColumnOrder) != 8 {
		return nil, errors.New("production profile table count")
	}
	plans := []productionTablePlan{}
	seenTables := map[string]bool{}
	for _, order := range p.BootstrapColumnOrder {
		model, ok := modelsByTable[order.Table]
		if !ok || seenTables[order.Table] {
			return nil, errors.New("production profile model identity")
		}
		seenTables[order.Table] = true
		fields := map[string]*schema.Field{}
		for _, name := range model.DBNames {
			if f := model.FieldsByDBName[name]; !f.IgnoreMigration {
				fields[name] = f
			}
		}
		if len(order.Columns) != len(fields) || len(model.PrimaryFields) == 0 {
			return nil, errors.New("production profile complete model columns")
		}
		plan := productionTablePlan{sql: "CREATE TABLE ? (", args: []any{clause.Table{Name: "public." + order.Table}}}
		seenColumns := map[string]bool{}
		for _, name := range order.Columns {
			field, exists := fields[name]
			if !exists || seenColumns[name] {
				return nil, errors.New("production profile column identity")
			}
			seenColumns[name] = true
			typ := owner.Migrator().FullDataTypeOf(field)
			if len(typ.Vars) != 0 || strings.Contains(strings.ToUpper(typ.SQL), "PRIMARY KEY") {
				return nil, errors.New("production profile unsupported model type")
			}
			plan.sql += "? ?,"
			plan.args = append(plan.args, clause.Column{Name: name}, typ)
		}
		keys := []any{}
		for _, field := range model.PrimaryFields {
			if !seenColumns[field.DBName] {
				return nil, errors.New("production profile primary key")
			}
			keys = append(keys, clause.Column{Name: field.DBName})
		}
		plan.sql += "PRIMARY KEY ?)"
		plan.args = append(plan.args, keys)
		plans = append(plans, plan)
	}
	return plans, nil
}

func BootstrapProductionShapeEmptyForTest(owner, runtime *gorm.DB, runtimeRole string) error {
	p, err := loadProductionProfile()
	if err != nil {
		return err
	}
	plans, err := productionPrecreationPlans(owner, p)
	if err != nil {
		return err
	}
	return bootstrapCatalogProfileForTest(owner, runtime, runtimeRole, func(db *gorm.DB) error {
		for _, plan := range plans {
			if err := db.Exec(plan.sql, plan.args...).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func applyProductionOverlayForTest(owner *gorm.DB) error {
	raw, err := os.ReadFile("../dbcatalog/shape/production-v0.sql")
	if err != nil {
		return err
	}
	// This observed v0 has no unknown objects and no additive DDL. Reject code
	// added to this no-op without a separately reviewed overlay implementation.
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			return errors.New("production v0 overlay must remain an explicit no-op")
		}
	}
	return owner.Exec(string(raw)).Error
}

func productionObjectMap(c dbcatalog.Catalog) map[string]dbcatalog.Object {
	result := map[string]dbcatalog.Object{}
	for _, o := range c.Objects {
		result[o.Kind+":"+o.Identity] = o
	}
	return result
}

func sourcePermissionSeed(t *testing.T, db *gorm.DB) dbcatalog.Seed {
	t.Helper()
	pairs := [][2]string{}
	for _, permission := range models.DefaultPermissions() {
		pairs = append(pairs, [2]string{permission.Resource, permission.Action})
	}
	raw, err := json.Marshal(pairs)
	require.NoError(t, err)
	// Use exactly Capture's PostgreSQL JSONB string encoding on source literals.
	// This SELECT has no table input and reads no permission rows.
	keys := []string{}
	require.NoError(t, db.Raw(`SELECT pg_catalog.jsonb_build_array(p.value->>0,p.value->>1)::text
FROM pg_catalog.jsonb_array_elements(CAST(? AS jsonb)) AS p(value)`, string(raw)).Scan(&keys).Error)
	require.Len(t, keys, len(pairs))
	sort.Strings(keys)
	encoded, err := json.Marshal(keys)
	require.NoError(t, err)
	hash := sha256.Sum256(encoded)
	return dbcatalog.Seed{Table: "permissions", Count: int64(len(keys)), KeysSHA256: hex.EncodeToString(hash[:])}
}

// Retain bounded child output privately. Neither command failure nor a failed
// success-marker assertion may print a connection URL or synthetic config.
type productionCLIOutput struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *productionCLIOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) > (1<<20)-b.Len() {
		return 0, errors.New("production profile CLI output bound")
	}
	return b.Buffer.Write(p)
}

func runProductionProfileCLI(t *testing.T, f catalogFixture) {
	t.Helper()
	urls := []string{}
	for _, role := range []struct{ name, password string }{
		{catalogTemplate.owner, catalogTemplate.ownerPassword},
		{catalogTemplate.runtime, catalogTemplate.password},
	} {
		u := catalogRoleURL(catalogTemplate.base, f.name, role.name, role.password)
		q := u.Query()
		q.Set("default_transaction_read_only", "on")
		u.RawQuery = q.Encode()
		db, err := catalogConnect(u)
		require.NoError(t, err)
		var readOnly string
		err = db.Raw("SHOW transaction_read_only").Scan(&readOnly).Error
		require.NoError(t, catalogClose(db))
		require.NoError(t, err)
		require.Equal(t, "on", readOnly)
		urls = append(urls, u.String())
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "rereply-profile")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	configPath := filepath.Join(dir, "synthetic.toml")
	configBytes := fmt.Sprintf("[database]\nurl = %q\nmigration_url = %q\nruntime_role = %q\nrls_enabled = true\nmax_open_conns = 2\nmax_idle_conns = 1\n", urls[1], urls[0], catalogTemplate.runtime)
	require.NoError(t, os.WriteFile(configPath, []byte(configBytes), 0600))
	env := []string{}
	for _, entry := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(entry, "=", 2)[0])
		if strings.HasPrefix(key, "WHATOMATE_") || strings.HasPrefix(key, "PG") || key == "GOPROXY" || key == "GOSUMDB" {
			continue
		}
		env = append(env, entry)
	}
	// The test reuses the installed/cached toolchain and dependencies only.
	env = append(env, "GOPROXY=off", "GOSUMDB=off")
	run := func(deadline time.Duration, command string, args ...string) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir, cmd.Env = "../..", env
		cmd.WaitDelay = 2 * time.Second
		output := &productionCLIOutput{}
		cmd.Stdout, cmd.Stderr = output, output
		err := cmd.Run()
		raw := append([]byte(nil), output.Bytes()...)
		if err != nil || ctx.Err() != nil {
			t.Fatalf("production profile child failed (output bytes=%d sha256=%x)", len(raw), sha256.Sum256(raw))
		}
		return raw
	}
	run(3*time.Minute, "go", "build", "-o", binary, "./cmd/whatomate")
	output := run(time.Minute, binary, "rls-migrate", "-config", configPath)
	require.True(t, bytes.Count(output, []byte("Tenant RLS installed")) == 1, "fixed CLI success marker missing or duplicated")
	require.False(t, bytes.Contains(output, []byte("Legacy WhatsApp omnichannel backfill complete")), "read-only CLI entered writable callback")
	t.Log("actual compiled rls-migrate passed with both startup URLs read-only; private child output withheld")
}

func TestTenantRLS_CatalogProductionShapeManifest(t *testing.T) {
	p, err := loadProductionProfile()
	require.NoError(t, err)
	known, _ := loadCatalogGoldens(t)
	canonical, err := dbcatalog.CanonicalCatalog(known)
	require.NoError(t, err)
	hash := sha256.Sum256(canonical)
	require.Equal(t, p.GoldenSHA256, hex.EncodeToString(hash[:]))
	byID := productionObjectMap(known)
	seen := map[string]bool{}
	counts := map[string]int{}
	for _, delta := range p.Delta {
		key := delta.Kind + ":" + delta.Identity
		want, ok := byID[key]
		require.True(t, ok)
		require.False(t, seen[key])
		seen[key] = true
		require.Equal(t, "fix-bootstrap", delta.Disposition)
		require.Len(t, delta.Attributes, 1)
		for attr, change := range delta.Attributes {
			require.Equal(t, want.Attributes[attr], change.Golden)
			require.NotEqual(t, change.Golden, change.Production)
			require.True(t, (delta.Kind == "columns" && attr == "position") || (delta.Kind == "types" && attr == "definition_sha256"))
		}
		counts[delta.Kind]++
	}
	require.Equal(t, map[string]int{"columns": 64, "types": 8}, counts)
	seenTables := map[string]bool{}
	for _, order := range p.BootstrapColumnOrder {
		require.False(t, seenTables[order.Table])
		seenTables[order.Table] = true
		require.True(t, seen["types:"+order.Table])
		actualNames := []string{}
		for _, obj := range known.Objects {
			if obj.Kind == "columns" && obj.Parent == order.Table {
				actualNames = append(actualNames, strings.TrimPrefix(obj.Identity, order.Table+"."))
			}
		}
		require.ElementsMatch(t, actualNames, order.Columns)
		for position, name := range order.Columns {
			key := "columns:" + order.Table + "." + name
			want := byID[key].Attributes["position"]
			if want != fmt.Sprint(position+1) {
				require.True(t, seen[key])
			}
		}
	}
}

func TestTenantRLS_CatalogProductionShapeOverlayMatchesLive(t *testing.T) {
	p, err := loadProductionProfile()
	require.NoError(t, err)
	f := catalogEmptyTarget(t)
	require.NoError(t, BootstrapProductionShapeEmptyForTest(f.owner, f.runtime, catalogTemplate.runtime))
	known, globals := loadCatalogGoldens(t)
	before := captureSyntheticCatalog(t, f, known, globals)
	require.Equal(t, p.LiveSHA256, before.SHA256, "actual source-derived profile must reproduce all eight composite hashes too")
	require.Empty(t, before.Unknown)
	// Seed equality is against current source definitions, not the O1 production
	// data diagnostic. The structural profile must not remove source seeds to
	// force production data parity. Versioned source seed changes remain valid.
	require.Equal(t, []dbcatalog.Seed{sourcePermissionSeed(t, f.runtime)}, before.Seeds)
	gotDiffs, err := dbcatalog.Compare(before, known)
	require.NoError(t, err)
	wantDiffs := []dbcatalog.Difference{}
	byID := productionObjectMap(dbcatalog.Catalog{Version: 0, Objects: before.Objects})
	for _, delta := range p.Delta {
		wantDiffs = append(wantDiffs, dbcatalog.Difference{Kind: delta.Kind, Identity: delta.Identity, Status: "changed"})
		for attr, change := range delta.Attributes {
			require.Equal(t, change.Production, byID[delta.Kind+":"+delta.Identity].Attributes[attr])
		}
	}
	require.ElementsMatch(t, wantDiffs, gotDiffs)
	for i := 0; i < 2; i++ {
		require.NoError(t, applyProductionOverlayForTest(f.owner))
		after := captureSyntheticCatalog(t, f, known, globals)
		require.Equal(t, before.SHA256, after.SHA256)
		require.Equal(t, before.Objects, after.Objects)
		require.Empty(t, after.Unknown)
	}
	// The no-op overlay adds no identities: original golden-only and
	// overlay-aware inventories are identical, never broadened to hide drift.
	discovered := discoverSyntheticCatalog(t, f.owner)
	identities := func(objects []dbcatalog.Object) []string {
		keys := []string{}
		for _, object := range objects {
			keys = append(keys, object.Kind+":"+object.Identity)
		}
		sort.Strings(keys)
		return keys
	}
	require.Equal(t, identities(known.Objects), identities(discovered.Objects))
	// Run the real production coordinator under forced read-only startup
	// options. Its writable callback must never execute on this future profile.
	ownerURL := catalogRoleURL(catalogTemplate.base, f.name, catalogTemplate.owner, catalogTemplate.ownerPassword)
	q := ownerURL.Query()
	q.Set("default_transaction_read_only", "on")
	ownerURL.RawQuery = q.Encode()
	roOwner, err := catalogConnect(ownerURL)
	require.NoError(t, err)
	defer catalogClose(roOwner)
	verified := 0
	require.NoError(t, database.RunRLSMigrationCoordinator(roOwner, &config.DefaultAdminConfig{}, catalogTemplate.runtime,
		func(*gorm.DB) error { return errors.New("production-shape verification attempted mutation") },
		func() error { verified++; return database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime) }))
	require.Equal(t, 1, verified)
	// Exercise the real command/config/runtime verifier, not just its library
	// coordinator. Both owner and runtime startup connections are read-only.
	runProductionProfileCLI(t, f)
	after, err := dbcatalog.Capture(context.Background(), f.runtimeDSN, dbcatalog.Options{Catalog: known, GlobalTables: globals, IncludeSeeds: true, Timeout: 45 * time.Second})
	require.NoError(t, err)
	require.Equal(t, before.SHA256, after.SHA256)
	require.Equal(t, before.Objects, after.Objects)
	require.Equal(t, before.Seeds, after.Seeds)
	require.Empty(t, after.Unknown)
	// The default template/golden profile was neither replaced nor mutated.
	original := captureSyntheticCatalog(t, catalogClone(t), known, globals)
	require.Equal(t, p.GoldenSHA256, original.SHA256)
}

func TestTenantRLS_CatalogProductionShapeRefusesNonemptyBeforeMutation(t *testing.T) {
	f := catalogClone(t)
	known, globals := loadCatalogGoldens(t)
	before := captureSyntheticCatalog(t, f, known, globals)
	require.ErrorContains(t, BootstrapProductionShapeEmptyForTest(f.owner, f.runtime, catalogTemplate.runtime), "empty public schema")
	after := captureSyntheticCatalog(t, f, known, globals)
	require.Equal(t, before, after)
	// Preflight also catches namespaces with no tables or application types.
	empty := catalogEmptyTarget(t)
	require.NoError(t, empty.owner.Exec(`CREATE COLLATION public.synthetic_profile_existing FROM pg_catalog."C"`).Error)
	require.ErrorContains(t, BootstrapProductionShapeEmptyForTest(empty.owner, empty.runtime, catalogTemplate.runtime), "empty public schema")
	var tables int64
	require.NoError(t, empty.owner.Raw(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`).Scan(&tables).Error)
	require.Zero(t, tables)
}

func TestTenantRLS_CatalogProductionShapeRefusesIncompleteSourceMapping(t *testing.T) {
	f := catalogEmptyTarget(t)
	p, err := loadProductionProfile()
	require.NoError(t, err)
	for _, mode := range []string{"missing", "duplicate", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			copyProfile := p
			copyProfile.BootstrapColumnOrder = append([]productionColumnOrder(nil), p.BootstrapColumnOrder...)
			copyProfile.BootstrapColumnOrder[0].Columns = append([]string(nil), p.BootstrapColumnOrder[0].Columns...)
			columns := copyProfile.BootstrapColumnOrder[0].Columns
			switch mode {
			case "missing":
				copyProfile.BootstrapColumnOrder[0].Columns = columns[:len(columns)-1]
			case "duplicate":
				columns[1] = columns[0]
			case "foreign":
				columns[0] = "synthetic_unmapped"
			}
			_, err := productionPrecreationPlans(f.owner, copyProfile)
			require.Error(t, err)
		})
	}
	var tables int64
	require.NoError(t, f.owner.Raw(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`).Scan(&tables).Error)
	require.Zero(t, tables)
	// Reading/mapping the profile must not reorder GORM's shared model cache.
	modelColumnOrders := func() map[string][]string {
		result := map[string][]string{}
		for _, model := range database.GetMigrationModels() {
			statement := &gorm.Statement{DB: f.owner}
			require.NoError(t, statement.Parse(model.Model))
			result[statement.Schema.Table] = append([]string(nil), statement.Schema.DBNames...)
		}
		return result
	}
	original := modelColumnOrders()
	_, err = productionPrecreationPlans(f.owner, p)
	require.NoError(t, err)
	require.Equal(t, original, modelColumnOrders())
}
