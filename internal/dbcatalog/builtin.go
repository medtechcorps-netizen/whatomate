package dbcatalog

import _ "embed"

// These files are generated from the disposable synthetic empty bootstrap,
// then reviewed and committed. Production never learns a replacement golden.
//
//go:embed golden/catalog.json
var builtinCatalog []byte

//go:embed golden/global_tables.json
var builtinGlobals []byte

//go:embed golden/history.json
var builtinHistory []byte

func BuiltinCatalog() (Catalog, error) {
	var history []struct {
		Version          int    `json:"version"`
		Class            string `json:"class"`
		MinReaderVersion int    `json:"min_reader_version"`
	}
	if err := strictDecode(builtinHistory, &history); err != nil {
		return Catalog{}, err
	}
	if len(history) != 1 || history[0].Version != 0 || history[0].Class != "baseline" || history[0].MinReaderVersion != 0 {
		return Catalog{}, refuse("history-format")
	}
	c, err := LoadCatalog(builtinCatalog)
	if err != nil {
		return Catalog{}, err
	}
	if len(c.Objects) == 0 {
		return Catalog{}, refuse("empty-catalog")
	}
	return c, nil
}

func BuiltinGlobalTables() ([]GlobalTable, error) { return LoadGlobalTables(builtinGlobals) }

// Fixed reviewed inputs for the dormant staging/test empty bootstrap.
//
//go:embed golden/production-v0.json
var bootstrapEmptyProductionV0 []byte

//go:embed shape/production-v0.sql
var bootstrapEmptyProductionOverlayV0 []byte

// BootstrapEmptyProfileV0 returns separate copies; callers cannot mutate the
// package's embedded profile or select a filesystem/environment replacement.
func BootstrapEmptyProfileV0() ([]byte, []byte) {
	return append([]byte(nil), bootstrapEmptyProductionV0...), append([]byte(nil), bootstrapEmptyProductionOverlayV0...)
}

// Planner declarations are compiled consistency data, never dispatch targets.
// Actual closure/revision verification remains the registry generator's job.
//
//go:embed registry/data_steps.json
var builtinPlanSteps []byte

//go:embed registry/coordinator_bindings.json
var builtinPlanBindings []byte
