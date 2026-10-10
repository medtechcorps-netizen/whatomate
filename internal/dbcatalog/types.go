// Package dbcatalog captures catalog metadata without importing migration code.
// All database errors are replaced by fixed codes before they leave this package.
package dbcatalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"time"
)

const Version = 0

var Kinds = []string{"relations", "columns", "constraints", "indexes", "triggers", "functions", "policies", "sequences", "types", "extensions"}

// Identity uses a public object name, or parent.name for dependent objects.
// Functions use name(hex SHA256 of pg_get_function_identity_arguments).
type Object struct {
	Kind       string            `json:"kind"`
	Identity   string            `json:"identity"`
	Parent     string            `json:"parent,omitempty"`
	Attributes map[string]string `json:"attributes"`
	Missing    bool              `json:"missing,omitempty"`
}

type Catalog struct {
	Version int      `json:"version"`
	Objects []Object `json:"objects"`
}

// GlobalTable is a committed allowlist, not an inferred exemption. Keys must
// identify public, non-tenant seed keys; values and sorted keys are never output.
// An empty Keys list marks a global relation with no seed-content comparison.
type GlobalTable struct {
	Name   string   `json:"name"`
	Reason string   `json:"reason"`
	Keys   []string `json:"keys,omitempty"`
}

type Options struct {
	Catalog      Catalog
	Overlay      *Catalog
	GlobalTables []GlobalTable
	IncludeSeeds bool
	Timeout      time.Duration
}

type Ownership struct {
	MigrationOwnerOwnsDatabase     bool `json:"migration_owner_owns_database"`
	MigrationOwnerOwnsPublicSchema bool `json:"migration_owner_owns_public_schema"`
}

type UnknownCount struct {
	Kind   string `json:"kind"`
	Parent string `json:"parent,omitempty"`
	Count  int64  `json:"count"`
}

type Seed struct {
	Table      string `json:"table"`
	Count      int64  `json:"count"`
	KeysSHA256 string `json:"keys_sha256"`
}

type Snapshot struct {
	Version           int              `json:"version"`
	SHA256            string           `json:"sha256"`
	Objects           []Object         `json:"objects"`
	Unknown           []UnknownCount   `json:"unknown"`
	CountOnly         map[string]int64 `json:"count_only"`
	Ownership         Ownership        `json:"ownership"`
	Seeds             []Seed           `json:"seeds,omitempty"`
	unknownIdentities []string
}

type Difference struct {
	Kind     string `json:"kind"`
	Identity string `json:"identity"`
	Status   string `json:"status"`
}

// Error intentionally never wraps a driver, DSN, SQL or row error.
type Error struct{ Code string }

func (e *Error) Error() string { return "catalog-snapshot:" + e.Code }
func refuse(code string) error { return &Error{Code: code} }

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// CanonicalCatalog is shared by the synthetic golden exporter and comparator.
func CanonicalCatalog(c Catalog) ([]byte, error) {
	if err := validateCatalog(c); err != nil {
		return nil, err
	}
	c.Objects = sortedObjects(c.Objects)
	b, err := json.Marshal(c)
	if err != nil {
		return nil, refuse("encode")
	}
	return append(b, '\n'), nil
}

// Capture opens only runtimeDSN, with startup read-only enforced, and performs
// every observation in one bounded READ ONLY REPEATABLE READ transaction.
func Capture(ctx context.Context, runtimeDSN string, options Options) (Snapshot, error) {
	return capture(ctx, runtimeDSN, options)
}

// WritePublic emits the validated public representation only. "compare" and
// "sha256" never expose the catalog's attribute payloads.
func WritePublic(w io.Writer, snapshot Snapshot, catalog Catalog, mode string) error {
	return writePublic(w, snapshot, catalog, mode)
}

// UnknownIdentities is an explicit owner-console opt-in. The regular public
// representation never includes these identifiers or any non-public names.
func UnknownIdentities(snapshot Snapshot) []string {
	return append([]string(nil), snapshot.unknownIdentities...)
}
