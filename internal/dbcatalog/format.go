package dbcatalog

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var integerPattern = regexp.MustCompile(`^-?[0-9]{1,20}$`)
var aclPattern = regexp.MustCompile(`^(?:MIGRATION_OWNER|RUNTIME|PUBLIC|OTHER)>(?:MIGRATION_OWNER|RUNTIME|PUBLIC):[A-Z_]+:(?:true|false)(?:;(?:MIGRATION_OWNER|RUNTIME|PUBLIC|OTHER)>(?:MIGRATION_OWNER|RUNTIME|PUBLIC):[A-Z_]+:(?:true|false))*$`)

var attributes = map[string]map[string]string{
	"relations":   {"relkind": "kind", "rls": "bool", "force_rls": "bool", "owner": "role", "acl": "acl", "security_invoker": "bool", "view_sha256": "hash", "partition_key_sha256": "hash", "partition_bound_sha256": "hash", "parents_sha256": "hash"},
	"columns":     {"position": "int", "type_sha256": "hash", "not_null": "bool", "default_sha256": "hash", "identity": "identity", "generated": "generated", "collation_sha256": "hash", "acl": "acl"},
	"constraints": {"type": "constraint", "definition_sha256": "hash", "validated": "bool", "deferrable": "bool", "deferred": "bool"},
	"indexes":     {"definition_sha256": "hash", "valid": "bool", "ready": "bool", "live": "bool", "unique": "bool", "primary": "bool", "exclusion": "bool"},
	"triggers":    {"definition_sha256": "hash", "enabled": "enabled", "internal": "bool", "function_sha256": "hash"},
	"functions":   {"source_sha256": "hash", "definition_sha256": "hash", "security_definer": "bool", "owner": "role", "acl": "acl", "config_sha256": "hash", "volatility": "volatility", "parallel": "parallel", "strict": "bool", "leakproof": "bool", "language_sha256": "hash"},
	"policies":    {"command": "command", "permissive": "bool", "roles": "roles", "using_sha256": "hash", "check_sha256": "hash"},
	"sequences":   {"type_sha256": "hash", "start": "int", "increment": "int", "min": "int", "max": "int", "cache": "int", "cycle": "bool", "owner": "role", "acl": "acl", "owned_by_sha256": "hash"},
	"types":       {"type": "type", "definition_sha256": "hash", "owner": "role", "acl": "acl", "enum_sha256": "hash"},
	"extensions":  {"version_sha256": "hash", "relocatable": "bool", "owner": "role"},
}

var countKeys = []string{"non_public_schemas", "non_public_objects", "other_roles", "default_acls", "oids", "reloptions", "statistics", "tablespaces", "other_acl_entries", "other_policy_roles"}

func knownKind(k string) bool { _, ok := attributes[k]; return ok }
func safeIdentity(o Object) bool {
	if !knownKind(o.Kind) {
		return false
	}
	if o.Parent != "" && !identifier.MatchString(o.Parent) {
		return false
	}
	if o.Kind == "functions" {
		i := strings.IndexByte(o.Identity, '(')
		return i > 0 && identifier.MatchString(o.Identity[:i]) && strings.HasSuffix(o.Identity, ")") && shaPattern.MatchString(o.Identity[i+1:len(o.Identity)-1]) && o.Parent == ""
	}
	if o.Parent != "" {
		return strings.HasPrefix(o.Identity, o.Parent+".") && identifier.MatchString(strings.TrimPrefix(o.Identity, o.Parent+"."))
	}
	return identifier.MatchString(o.Identity)
}

func safeAttribute(t, v string) bool {
	switch t {
	case "hash":
		return shaPattern.MatchString(v)
	case "bool":
		return v == "true" || v == "false"
	case "int":
		return integerPattern.MatchString(v)
	case "role":
		return v == "MIGRATION_OWNER" || v == "RUNTIME" || v == "PUBLIC" || v == "OTHER"
	case "acl":
		if v == "" {
			return true
		}
		if len(v) > 8192 || !aclPattern.MatchString(v) {
			return false
		}
		previous := ""
		for _, entry := range strings.Split(v, ";") {
			if entry <= previous {
				return false
			}
			previous = entry
			parts := strings.Split(entry, ":")
			switch parts[1] {
			case "SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "EXECUTE", "USAGE", "CREATE", "CONNECT", "TEMPORARY", "SET", "ALTER_SYSTEM", "MAINTAIN":
			default:
				return false
			}
		}
		return true
	case "roles":
		if len(v) > 64 {
			return false
		}
		previous := ""
		for _, s := range strings.Split(v, ",") {
			if !safeAttribute("role", s) || s <= previous {
				return false
			}
			previous = s
		}
		return true
	case "kind":
		return len(v) == 1 && strings.Contains("rpvmfS", v)
	case "identity":
		return v == "" || v == "a" || v == "d"
	case "generated":
		return v == "" || v == "s" || v == "v"
	case "constraint":
		return len(v) == 1 && strings.Contains("cfpuxnt", v)
	case "enabled":
		return len(v) == 1 && strings.Contains("ODRA", v)
	case "volatility":
		return v == "i" || v == "s" || v == "v"
	case "parallel":
		return v == "s" || v == "r" || v == "u"
	case "command":
		return len(v) == 1 && strings.Contains("*rawd", v)
	case "type":
		return len(v) == 1 && strings.Contains("bcdeprm", v)
	}
	return false
}

func validateCatalog(c Catalog) error { return validateCatalogMode(c, false) }

// Identity-only descriptors are accepted solely as Capture's explicit input
// inventory. Golden files, captured output and printers require full attributes.
func validateCatalogMode(c Catalog, identityOnly bool) error {
	if c.Version != Version || len(c.Objects) > 50000 {
		return refuse("catalog-format")
	}
	seen := map[string]bool{}
	totalBytes := 0
	for _, o := range c.Objects {
		if !safeIdentity(o) || seen[o.Kind+":"+o.Identity] || o.Attributes == nil || len(o.Attributes) > 32 {
			return refuse("catalog-format")
		}
		seen[o.Kind+":"+o.Identity] = true
		totalBytes += len(o.Kind) + len(o.Identity) + len(o.Parent)
		for k, v := range o.Attributes {
			totalBytes += len(k) + len(v)
			if totalBytes > 16<<20 {
				return refuse("catalog-format")
			}
			if t, ok := attributes[o.Kind][k]; !ok || !safeAttribute(t, v) {
				return refuse("catalog-format")
			}
		}
		if o.Missing && len(o.Attributes) != 0 {
			return refuse("catalog-format")
		}
		if !o.Missing && len(o.Attributes) != len(attributes[o.Kind]) && (!identityOnly || len(o.Attributes) != 0) {
			return refuse("catalog-format")
		}
	}
	return nil
}

func sortedObjects(objects []Object) []Object {
	out := append([]Object{}, objects...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Identity < out[j].Identity
	})
	return out
}

func strictDecode(data []byte, dst any) error {
	if len(data) == 0 || len(data) > 16<<20 {
		return refuse("catalog-format")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func() error
	value = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, err := d.Token()
					if err != nil {
						return err
					}
					s, ok := k.(string)
					if !ok || seen[s] {
						return refuse("catalog-format")
					}
					seen[s] = true
					if err = value(); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err = value(); err != nil {
						return err
					}
				}
			default:
				return refuse("catalog-format")
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := value(); err != nil {
		return refuse("catalog-format")
	}
	if _, err := d.Token(); err != io.EOF {
		return refuse("catalog-format")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return refuse("catalog-format")
	}
	return nil
}

func LoadCatalog(data []byte) (Catalog, error) {
	var c Catalog
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 2 || fields["version"] == nil || fields["objects"] == nil {
		return Catalog{}, refuse("catalog-format")
	}
	if err := strictDecode(data, &c); err != nil {
		return Catalog{}, err
	}
	if err := validateCatalog(c); err != nil {
		return Catalog{}, err
	}
	return c, nil
}

func LoadGlobalTables(data []byte) ([]GlobalTable, error) {
	var tables []GlobalTable
	if err := strictDecode(data, &tables); err != nil {
		return nil, err
	}
	if err := validateGlobals(tables); err != nil {
		return nil, err
	}
	return tables, nil
}

func validateGlobals(tables []GlobalTable) error {
	seen := map[string]bool{}
	if len(tables) > 1000 {
		return refuse("global-format")
	}
	for _, t := range tables {
		if !identifier.MatchString(t.Name) || seen[t.Name] || len(t.Reason) < 8 || len(t.Reason) > 1024 || len(t.Keys) > 8 {
			return refuse("global-format")
		}
		seen[t.Name] = true
		keys := map[string]bool{}
		for _, k := range t.Keys {
			if !identifier.MatchString(k) || keys[k] {
				return refuse("global-format")
			}
			keys[k] = true
		}
	}
	return nil
}

func Compare(snapshot Snapshot, catalog Catalog) ([]Difference, error) {
	if err := validateCatalog(catalog); err != nil {
		return nil, err
	}
	if err := validateCatalog(Catalog{Version: snapshot.Version, Objects: snapshot.Objects}); err != nil {
		return nil, err
	}
	actual := map[string]Object{}
	for _, o := range snapshot.Objects {
		actual[o.Kind+":"+o.Identity] = o
	}
	diffs := []Difference{}
	for _, want := range sortedObjects(catalog.Objects) {
		got, ok := actual[want.Kind+":"+want.Identity]
		status := "changed"
		if !ok || got.Missing {
			status = "missing"
		}
		if !ok || got.Missing || !reflect.DeepEqual(got.Attributes, want.Attributes) {
			diffs = append(diffs, Difference{want.Kind, want.Identity, status})
		}
	}
	return diffs, nil
}

func writePublic(w io.Writer, s Snapshot, c Catalog, mode string) error {
	if mode != "json" && mode != "sha256" && mode != "compare" {
		return refuse("mode")
	}
	if err := validateCatalog(c); err != nil {
		return err
	}
	canonical, err := CanonicalCatalog(Catalog{Version: s.Version, Objects: s.Objects})
	if err != nil {
		return err
	}
	if digest(canonical) != s.SHA256 {
		return refuse("snapshot-format")
	}
	known := map[string]bool{}
	parents := map[string]bool{}
	for _, o := range c.Objects {
		known[o.Kind+":"+o.Identity] = true
		if o.Kind == "relations" {
			parents[o.Identity] = true
		}
	}
	for _, o := range s.Objects {
		if !known[o.Kind+":"+o.Identity] {
			return refuse("snapshot-format")
		}
	}
	if len(s.Objects) != len(c.Objects) {
		return refuse("snapshot-format")
	}
	for _, v := range s.Unknown {
		if !knownKind(v.Kind) || v.Count < 0 || (v.Parent != "" && !parents[v.Parent]) {
			return refuse("snapshot-format")
		}
	}
	allowed := map[string]bool{}
	for _, k := range countKeys {
		allowed[k] = true
	}
	for k, v := range s.CountOnly {
		if !allowed[k] || v < 0 {
			return refuse("snapshot-format")
		}
	}
	for _, v := range s.Seeds {
		if !parents[v.Table] || v.Count < 0 || !shaPattern.MatchString(v.KeysSHA256) {
			return refuse("snapshot-format")
		}
	}
	var output []byte
	switch mode {
	case "sha256":
		output = []byte(s.SHA256 + "\n")
	case "json":
		output, err = json.MarshalIndent(s, "", "  ")
	default:
		var differences []Difference
		differences, err = Compare(s, c)
		if err == nil {
			output, err = json.MarshalIndent(struct {
				SHA256      string           `json:"sha256"`
				Match       bool             `json:"match"`
				Differences []Difference     `json:"differences"`
				Unknown     []UnknownCount   `json:"unknown"`
				CountOnly   map[string]int64 `json:"count_only"`
				Ownership   Ownership        `json:"ownership"`
				Seeds       []Seed           `json:"seeds,omitempty"`
			}{s.SHA256, len(differences) == 0, differences, s.Unknown, s.CountOnly, s.Ownership, s.Seeds}, "", "  ")
		}
	}
	if err != nil {
		return refuse("encode")
	}
	if mode != "sha256" {
		output = append(output, '\n')
	}
	if n, writeErr := w.Write(output); writeErr != nil || n != len(output) {
		return refuse("output")
	}
	return nil
}
