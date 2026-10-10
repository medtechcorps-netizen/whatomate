package dbcatalog

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"sort"
	"strings"
)

const maxSeedBytes = 8 << 20

// captureSeeds reads only committed global key columns, with a hard row bound.
// It never selects whole rows or emits keys. JSON arrays distinguish separators,
// nulls and types; sorted encoded keys feed SHA256 without delimiter ambiguity.
func captureSeeds(ctx context.Context, tx pgx.Tx, tables []GlobalTable, known Catalog) ([]Seed, error) {
	parents := map[string]bool{}
	for _, o := range known.Objects {
		if o.Kind == "relations" {
			parents[o.Identity] = true
		}
	}
	seeds := []Seed{}
	totalBytes := 0
	for _, table := range tables {
		if len(table.Keys) == 0 {
			continue
		}
		if !parents[table.Name] {
			return nil, refuse("seed-allowlist")
		}
		var columns int
		var unprotected bool
		err := tx.QueryRow(ctx, `SELECT count(a.attname)::integer, COALESCE(bool_and(NOT c.relrowsecurity AND NOT c.relforcerowsecurity AND c.relkind='r' AND a.atttypid IN ('pg_catalog.text'::regtype,'pg_catalog.varchar'::regtype,'pg_catalog.name'::regtype,'pg_catalog.int2'::regtype,'pg_catalog.int4'::regtype,'pg_catalog.int8'::regtype,'pg_catalog.bool'::regtype)),false) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid WHERE n.nspname='public' AND c.relname=$1 AND a.attname=ANY($2) AND a.attnum>0 AND NOT a.attisdropped`, table.Name, table.Keys).Scan(&columns, &unprotected)
		if err != nil || !unprotected || columns != len(table.Keys) {
			return nil, refuse("seed-allowlist")
		}
		cols := make([]string, len(table.Keys))
		for i, k := range table.Keys {
			cols[i] = pgx.Identifier{k}.Sanitize()
		}
		rows, err := tx.Query(ctx, "SELECT pg_catalog.jsonb_build_array("+strings.Join(cols, ",")+")::text FROM ONLY "+pgx.Identifier{"public", table.Name}.Sanitize()+" LIMIT 100001")
		if err != nil {
			return nil, refuse("seed-read")
		}
		keys := []string{}
		for rows.Next() {
			var key string
			if rows.Scan(&key) != nil || len(key) > 16384 || len(keys) >= 100000 {
				rows.Close()
				return nil, refuse("seed-read")
			}
			if err = consumeSeedBudget(&totalBytes, key); err != nil {
				rows.Close()
				return nil, err
			}
			keys = append(keys, key)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, refuse("seed-read")
		}
		sort.Strings(keys)
		encoded, _ := json.Marshal(keys)
		seeds = append(seeds, Seed{table.Name, int64(len(keys)), digest(encoded)})
	}
	sort.Slice(seeds, func(i, j int) bool { return seeds[i].Table < seeds[j].Table })
	return seeds, nil
}

func consumeSeedBudget(total *int, key string) error {
	// json.Marshal escapes control characters by up to 6x; budget that encoded
	// worst case before retaining a key, across every seed table in the capture.
	n := 6*len(key) + 3
	if n > maxSeedBytes-*total {
		return refuse("seed-limit")
	}
	*total += n
	return nil
}
