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
