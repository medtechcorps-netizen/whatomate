package dbcatalog

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxObjects = 50000
const maxAttributeBytes = 2 << 20
const maxCatalogBytes = 64 << 20

type symbols struct{ owner, runtime string }

func (s symbols) role(oid string) string {
	switch oid {
	case "0":
		return "PUBLIC"
	case s.owner:
		return "MIGRATION_OWNER"
	case s.runtime:
		return "RUNTIME"
	default:
		return "OTHER"
	}
}

// The only substituted function literal is the generated fingerprint digest
// in these two named functions. All other function source bytes remain hashed.
var fingerprintSource = regexp.MustCompile(`^\s*SELECT '[0-9a-f]{64}'::text\s*$`)
var fingerprintLiteral = regexp.MustCompile(`'[0-9a-f]{64}'::text`)
var additiveFingerprintSource = regexp.MustCompile(`^\s*SELECT 'v1:[0-9a-f]{64}'::text\s*$`)
var additiveFingerprintLiteral = regexp.MustCompile(`'v1:[0-9a-f]{64}'::text`)

func normalizeFunction(name, source, definition string) (string, string) {
	if name == "rereply_tenant_policy_fingerprint" && fingerprintSource.MatchString(source) {
		source = fingerprintLiteral.ReplaceAllString(source, "'<FINGERPRINT>'::text")
		definition = fingerprintLiteral.ReplaceAllString(definition, "'<FINGERPRINT>'::text")
	} else if name == "rereply_tenant_policy_additive_fingerprint_v1" && additiveFingerprintSource.MatchString(source) {
		source = additiveFingerprintLiteral.ReplaceAllString(source, "'v1:<FINGERPRINT>'::text")
		definition = additiveFingerprintLiteral.ReplaceAllString(definition, "'v1:<FINGERPRINT>'::text")
	}
	return source, definition
}

func normalize(kind, name string, raw map[string]json.RawMessage, roles symbols, counts map[string]int64) (map[string]string, error) {
	out := map[string]string{}
	if kind == "functions" {
		var source, definition string
		if json.Unmarshal(raw["source"], &source) != nil || json.Unmarshal(raw["definition"], &definition) != nil {
			return nil, refuse("catalog-row")
		}
		source, definition = normalizeFunction(name, source, definition)
		raw["source"], _ = json.Marshal(source)
		raw["definition"], _ = json.Marshal(definition)
	}
	for key, value := range raw {
		switch key {
		case "owner":
			var oid string
			if json.Unmarshal(value, &oid) != nil {
				return nil, refuse("catalog-row")
			}
			out[key] = roles.role(oid)
		case "acl":
			var acl []struct {
				Grantee   string `json:"grantee"`
				Grantor   string `json:"grantor"`
				Privilege string `json:"privilege"`
				Grantable bool   `json:"grantable"`
			}
			if json.Unmarshal(value, &acl) != nil {
				return nil, refuse("catalog-row")
			}
			entries := []string{}
			seen := map[string]bool{}
			for _, a := range acl {
				symbol := roles.role(a.Grantee)
				if symbol == "OTHER" {
					counts["other_acl_entries"]++
					continue
				}
				grantor := roles.role(a.Grantor)
				if grantor == "OTHER" {
					counts["other_acl_entries"]++
				}
				entry := grantor + ">" + symbol + ":" + a.Privilege + ":" + strconv.FormatBool(a.Grantable)
				if !seen[entry] {
					entries = append(entries, entry)
					seen[entry] = true
				}
			}
			sort.Strings(entries)
			out[key] = strings.Join(entries, ";")
		case "roles":
			var values []json.Number
			if json.Unmarshal(value, &values) != nil {
				return nil, refuse("catalog-row")
			}
			entries := []string{}
			for _, v := range values {
				symbol := roles.role(v.String())
				if symbol == "OTHER" {
					counts["other_policy_roles"]++
					continue
				}
				entries = append(entries, symbol)
			}
			sort.Strings(entries)
			if len(entries) == 0 {
				entries = append(entries, "OTHER")
			}
			out[key] = strings.Join(entries, ",")
		default:
			var str string
			if json.Unmarshal(value, &str) != nil {
				str = string(value)
			}
			if _, ok := attributes[kind][key+"_sha256"]; ok {
				out[key+"_sha256"] = digest([]byte(str))
			} else {
				out[key] = str
			}
		}
	}
	for key, value := range out {
		typ, ok := attributes[kind][key]
		if !ok || !safeAttribute(typ, value) {
			return nil, refuse("catalog-row")
		}
	}
	if len(out) != len(attributes[kind]) {
		return nil, refuse("catalog-row")
	}
	return out, nil
}

func mergedCatalog(c Catalog, overlay *Catalog) (Catalog, error) {
	if err := validateCatalogMode(c, true); err != nil {
		return Catalog{}, err
	}
	if overlay == nil {
		return c, nil
	}
	if err := validateCatalogMode(*overlay, true); err != nil {
		return Catalog{}, err
	}
	result := Catalog{Version: Version, Objects: append([]Object{}, c.Objects...)}
	seen := map[string]bool{}
	for _, o := range c.Objects {
		seen[o.Kind+":"+o.Identity] = true
	}
	for _, o := range overlay.Objects {
		if seen[o.Kind+":"+o.Identity] {
			return Catalog{}, refuse("overlay-overlap")
		}
		result.Objects = append(result.Objects, o)
	}
	return result, nil
}

func capture(ctx context.Context, dsn string, opt Options) (Snapshot, error) {
	known, err := mergedCatalog(opt.Catalog, opt.Overlay)
	if err != nil {
		return Snapshot{}, err
	}
	if err = validateGlobals(opt.GlobalTables); err != nil {
		return Snapshot{}, err
	}
	if len(known.Objects) == 0 {
		return Snapshot{}, refuse("empty-catalog")
	}
	if opt.Timeout == 0 {
		opt.Timeout = 30 * time.Second
	}
	if opt.Timeout < time.Second || opt.Timeout > 2*time.Minute {
		return Snapshot{}, refuse("timeout")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	cfg, err := connectionConfig(dsn, opt.Timeout)
	if err != nil {
		return Snapshot{}, err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return Snapshot{}, refuse("connect")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
	}()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Snapshot{}, refuse("transaction")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var readOnly, isolation, searchPath string
	var major int
	if err = tx.QueryRow(ctx, "SELECT pg_catalog.current_setting('transaction_read_only'), pg_catalog.current_setting('transaction_isolation'), pg_catalog.current_setting('search_path'), pg_catalog.current_setting('server_version_num')::integer/10000").Scan(&readOnly, &isolation, &searchPath, &major); err != nil || readOnly != "on" || isolation != "repeatable read" || searchPath != "pg_catalog" || (major != 14 && major != 17) {
		return Snapshot{}, refuse("transaction")
	}
	var roles symbols
	var owner Ownership
	if err = tx.QueryRow(ctx, identitySQL).Scan(&roles.owner, &roles.runtime, &owner.MigrationOwnerOwnsDatabase, &owner.MigrationOwnerOwnsPublicSchema); err != nil {
		return Snapshot{}, refuse("identity")
	}
	if roles.owner == roles.runtime {
		return Snapshot{}, refuse("runtime-is-owner")
	}
	counts := map[string]int64{}
	for _, key := range countKeys {
		counts[key] = 0
	}
	values := make([]int64, 8)
	dest := make([]any, 8)
	for i := range values {
		dest[i] = &values[i]
	}
	if err = tx.QueryRow(ctx, countsSQL, roles.owner, roles.runtime).Scan(dest...); err != nil {
		return Snapshot{}, refuse("catalog-read")
	}
	for i := range values {
		counts[countKeys[i]] = values[i]
	}
	all := []Object{}
	totalBytes := 0
	for _, query := range queries {
		rows, queryErr := tx.Query(ctx, query.sql)
		if queryErr != nil {
			return Snapshot{}, refuse("catalog-read")
		}
		for rows.Next() {
			var name, parent, payload string
			if rows.Scan(&name, &parent, &payload) != nil || len(payload) > maxAttributeBytes || len(all) >= maxObjects {
				rows.Close()
				return Snapshot{}, refuse("catalog-read")
			}
			totalBytes += len(name) + len(parent) + len(payload)
			if totalBytes > maxCatalogBytes {
				rows.Close()
				return Snapshot{}, refuse("catalog-limit")
			}
			var raw map[string]json.RawMessage
			if json.Unmarshal([]byte(payload), &raw) != nil {
				rows.Close()
				return Snapshot{}, refuse("catalog-row")
			}
			attrs, normalizeErr := normalize(query.kind, name, raw, roles, counts)
			if normalizeErr != nil {
				rows.Close()
				return Snapshot{}, normalizeErr
			}
			identity := name
			if query.kind == "functions" {
				identity = name + "(" + digest([]byte(parent)) + ")"
				parent = ""
			} else if parent != "" {
				identity = parent + "." + name
			}
			all = append(all, Object{Kind: query.kind, Identity: identity, Parent: parent, Attributes: attrs})
		}
		rowErr := rows.Err()
		rows.Close()
		if rowErr != nil {
			return Snapshot{}, refuse("catalog-read")
		}
	}
	snapshot, err := project(all, known, owner, counts)
	if err != nil {
		return Snapshot{}, err
	}
	if opt.IncludeSeeds {
		snapshot.Seeds, err = captureSeeds(ctx, tx, opt.GlobalTables, known)
		if err != nil {
			return Snapshot{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Snapshot{}, refuse("transaction")
	}
	return snapshot, nil
}

func connectionConfig(dsn string, timeout time.Duration) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || dsn == "" {
		return nil, refuse("connect")
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	if cfg.RuntimeParams["options"] != "" {
		return nil, refuse("connection-options")
	}
	// Startup parameters precede every SQL query; DSN options cannot disable them.
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["search_path"] = "pg_catalog"
	cfg.RuntimeParams["statement_timeout"] = strconv.FormatInt(timeout.Milliseconds(), 10)
	cfg.RuntimeParams["lock_timeout"] = "2000"
	cfg.RuntimeParams["application_name"] = "rereply-catalog-snapshot"
	cfg.ConnectTimeout = timeout
	return cfg, nil
}

func project(all []Object, known Catalog, owner Ownership, counts map[string]int64) (Snapshot, error) {
	wanted := map[string]Object{}
	parents := map[string]bool{}
	for _, o := range known.Objects {
		wanted[o.Kind+":"+o.Identity] = o
		if o.Kind == "relations" {
			parents[o.Identity] = true
		}
	}
	actual := map[string]Object{}
	unknown := map[string]int64{}
	identities := []string{}
	for _, o := range all {
		key := o.Kind + ":" + o.Identity
		if _, ok := wanted[key]; ok {
			if _, dup := actual[key]; dup {
				return Snapshot{}, refuse("duplicate-identity")
			}
			actual[key] = o
		} else {
			parent := ""
			if parents[o.Parent] {
				parent = o.Parent
			}
			unknown[o.Kind+":"+parent]++
			identities = append(identities, key)
		}
	}
	out := Snapshot{Version: Version, Objects: []Object{}, Unknown: []UnknownCount{}, CountOnly: counts, Ownership: owner, unknownIdentities: identities}
	for _, want := range sortedObjects(known.Objects) {
		if got, ok := actual[want.Kind+":"+want.Identity]; ok {
			out.Objects = append(out.Objects, got)
		} else {
			out.Objects = append(out.Objects, Object{Kind: want.Kind, Identity: want.Identity, Parent: want.Parent, Attributes: map[string]string{}, Missing: true})
		}
	}
	keys := make([]string, 0, len(unknown))
	for key := range unknown {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		pair := strings.SplitN(key, ":", 2)
		out.Unknown = append(out.Unknown, UnknownCount{pair[0], pair[1], unknown[key]})
	}
	sort.Strings(out.unknownIdentities)
	canonical, err := CanonicalCatalog(Catalog{Version: Version, Objects: out.Objects})
	if err != nil {
		return Snapshot{}, err
	}
	out.SHA256 = digest(canonical)
	return out, nil
}
