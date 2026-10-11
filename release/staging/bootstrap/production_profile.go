// The staging-only image consumes the authoritative reviewed public profile.
// No production binary imports this main package. Precreation is kept exactly
// equal to the PR4 test exporter by a source-AST contract test.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

const (
	productionProfileSHA256 = "808e11a5fdab638adc6f0004e9595371d1eee9a51763e12e779b355f7cdd391d"
	productionOverlaySHA256 = "48b2558aaa0c54f0de886093c875ff70fbc65231ee80d8b560df91afc97c131d"
	productionProfileLimit  = 128 << 10
	productionOverlayLimit  = 16 << 10
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

// Image paths are /production-shape/{production-v0.json,production-v0.sql}.
// The CI-built standalone binary carries exactly the same sibling directory.
// Neither working directory, environment nor a CLI flag selects these inputs.
func loadProductionProfile() (productionProfile, []byte, error) {
	executable, err := os.Executable()
	if err != nil {
		return productionProfile{}, nil, errors.New("profile executable path")
	}
	dir := filepath.Join(filepath.Dir(executable), "production-shape")
	return readProductionProfile(filepath.Join(dir, "production-v0.json"), filepath.Join(dir, "production-v0.sql"))
}

func readPinnedProfileFile(path, expected string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("profile file unavailable")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("profile file shape")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("profile file bound")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != expected {
		return nil, errors.New("profile file hash")
	}
	return raw, nil
}

func readProductionProfile(profilePath, overlayPath string) (productionProfile, []byte, error) {
	var p productionProfile
	raw, err := readPinnedProfileFile(profilePath, productionProfileSHA256, productionProfileLimit)
	if err != nil {
		return p, nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, nil, errors.New("profile JSON")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return p, nil, errors.New("profile trailing content")
	}
	canonical, err := json.MarshalIndent(p, "", "  ")
	if err != nil || !bytes.Equal(append(canonical, '\n'), raw) {
		return p, nil, errors.New("profile canonical form")
	}
	overlay, err := readPinnedProfileFile(overlayPath, productionOverlaySHA256, productionOverlayLimit)
	if err != nil {
		return p, nil, err
	}
	// The accepted overlay contains only comments. New executable DDL needs a
	// separately reviewed implementation; this tool cannot run arbitrary SQL.
	for _, line := range strings.Split(string(overlay), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			return p, nil, errors.New("profile overlay must be a no-op")
		}
	}
	return p, overlay, nil
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

func bootstrapProductionShape(owner *gorm.DB, cfg *config.Config, logger logf.Logger, plans []productionTablePlan, overlay []byte) error {
	if err := inMigrationSession(owner, func(session *gorm.DB) error {
		for _, plan := range plans {
			if err := session.Exec(plan.sql, plan.args...).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("precreate production profile: %w", err)
	}
	if err := bootstrap(owner, cfg, logger); err != nil {
		return err
	}
	if err := inMigrationSession(owner, func(session *gorm.DB) error { return session.Exec(string(overlay)).Error }); err != nil {
		return fmt.Errorf("apply production overlay: %w", err)
	}
	return nil
}
