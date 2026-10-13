package dbcatalog

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The embedded interface deliberately has no implementation for Begin, Exec,
// Commit, Rollback, Conn or lifecycle methods. Any attempted ownership transfer
// panics. These unit contracts supplement, not replace, root's real PG17 tests.
type planCaptureTx struct {
	pgx.Tx
	readOnly, isolation, searchPath string
	major                           int
	sameRole                        bool
	rowsRead, queriesRead, closed   int
	failQuery, failRow              int
	rowError, scanError             bool
	cancel                          context.CancelFunc
}

func newPlanCaptureTx() *planCaptureTx {
	return &planCaptureTx{readOnly: "on", isolation: "repeatable read", searchPath: "pg_catalog", major: 17, failQuery: -1, failRow: -1}
}

type planCaptureRow struct {
	values []any
	err    error
}

func (r planCaptureRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("fixture destination mismatch")
	}
	for i, value := range r.values {
		switch ptr := dest[i].(type) {
		case *string:
			v, ok := value.(string)
			if !ok {
				return errors.New("fixture string")
			}
			*ptr = v
		case *int:
			v, ok := value.(int)
			if !ok {
				return errors.New("fixture int")
			}
			*ptr = v
		case *int64:
			v, ok := value.(int64)
			if !ok {
				return errors.New("fixture int64")
			}
			*ptr = v
		case *bool:
			v, ok := value.(bool)
			if !ok {
				return errors.New("fixture bool")
			}
			*ptr = v
		default:
			return errors.New("fixture unexpected destination")
		}
	}
	return nil
}

func (tx *planCaptureTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	index := tx.rowsRead
	tx.rowsRead++
	if index == tx.failRow {
		return planCaptureRow{err: errors.New("private driver/DSN/row text")}
	}
	switch index {
	case 0:
		if !strings.Contains(sql, "current_setting('transaction_read_only')") || len(args) != 0 {
			panic("unexpected settings query")
		}
		return planCaptureRow{values: []any{tx.readOnly, tx.isolation, tx.searchPath, tx.major}}
	case 1:
		if sql != identitySQL || len(args) != 0 {
			panic("unexpected identity query")
		}
		runtime := "456"
		if tx.sameRole {
			runtime = "123"
		}
		return planCaptureRow{values: []any{"123", runtime, true, true}}
	case 2:
		if sql != countsSQL || !reflect.DeepEqual(args, []any{"123", "456"}) {
			panic("unexpected counts query")
		}
		return planCaptureRow{values: []any{int64(7), int64(0), int64(3), int64(0), int64(100), int64(0), int64(0), int64(0)}}
	default:
		panic("unexpected seed/nullable or other query")
	}
}

func (tx *planCaptureTx) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	index := tx.queriesRead
	tx.queriesRead++
	if index >= len(queries) || sql != queries[index].sql || len(args) != 0 {
		panic("catalog query sequence changed")
	}
	if index == tx.failQuery {
		return nil, errors.New("private driver/DSN/row text")
	}
	rows := &planCaptureRows{tx: tx, rowError: tx.rowError, scanError: tx.scanError}
	if tx.cancel != nil {
		tx.cancel()
	}
	return rows, nil
}

type planCaptureRows struct {
	pgx.Rows
	tx                                *planCaptureTx
	rowError, scanError, seen, closed bool
}

func (r *planCaptureRows) Next() bool {
	if r.scanError && !r.seen {
		r.seen = true
		return true
	}
	return false
}
func (r *planCaptureRows) Scan(...any) error { return errors.New("private row value") }
func (r *planCaptureRows) Err() error {
	if r.rowError {
		return errors.New("private streaming error")
	}
	return nil
}
func (r *planCaptureRows) Close() {
	if !r.closed {
		r.closed = true
		r.tx.closed++
	}
}

func TestCapturePlanBorrowsTransactionAndRunsCompleteCatalogPass(t *testing.T) {
	tx := newPlanCaptureTx()
	snapshot, err := CapturePlanInTx(context.Background(), tx, testCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if tx.rowsRead != 3 || tx.queriesRead != len(queries) || tx.closed != len(queries) {
		t.Fatal("incomplete pass or leaked rows", tx.rowsRead, tx.queriesRead, tx.closed)
	}
	if len(snapshot.Objects) != 1 || !snapshot.Objects[0].Missing || len(snapshot.Seeds) != 0 || len(snapshot.CountOnly) != len(countKeys) || snapshot.CountOnly["non_public_schemas"] != 7 || snapshot.CountOnly["other_acl_entries"] != 0 {
		t.Fatal("projection/tail mismatch", snapshot)
	}
	canonical, err := CanonicalCatalog(Catalog{Version: Version, Objects: snapshot.Objects})
	if err != nil || snapshot.SHA256 != digest(canonical) {
		t.Fatal("canonical digest changed", err)
	}
	// The ordinary path still permits both previously supported major versions.
	for _, major := range []int{14, 17} {
		ordinary := newPlanCaptureTx()
		ordinary.major = major
		observed, err := captureCatalogInTx(context.Background(), ordinary, testCatalog(), false)
		if err != nil || observed.major != major || ordinary.queriesRead != len(queries) {
			t.Fatal("ordinary version contract", err)
		}
	}
}

func TestCapturePlanRequiresPG17ReadOnlyRepeatableReadAndCatalogSearchPath(t *testing.T) {
	for name, edit := range map[string]func(*planCaptureTx){
		"write transaction":  func(tx *planCaptureTx) { tx.readOnly = "off" },
		"read committed":     func(tx *planCaptureTx) { tx.isolation = "read committed" },
		"public search path": func(tx *planCaptureTx) { tx.searchPath = "public, pg_catalog" },
		"PG14":               func(tx *planCaptureTx) { tx.major = 14 },
		"PG18":               func(tx *planCaptureTx) { tx.major = 18 },
	} {
		t.Run(name, func(t *testing.T) {
			tx := newPlanCaptureTx()
			edit(tx)
			_, err := CapturePlanInTx(context.Background(), tx, testCatalog())
			requireError(t, err, "transaction")
			if tx.rowsRead != 1 || tx.queriesRead != 0 {
				t.Fatal("unsafe transaction was queried further")
			}
		})
	}
	tx := newPlanCaptureTx()
	tx.sameRole = true
	_, err := CapturePlanInTx(context.Background(), tx, testCatalog())
	requireError(t, err, "runtime-is-owner")
	if tx.rowsRead != 2 || tx.queriesRead != 0 {
		t.Fatal("owner connection used for runtime capture")
	}
	for _, ctx := range []context.Context{nil, func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }()} {
		tx = newPlanCaptureTx()
		_, err = CapturePlanInTx(ctx, tx, testCatalog())
		requireError(t, err, "transaction")
		if tx.rowsRead != 0 {
			t.Fatal("cancelled/nil context queried")
		}
	}
	_, err = CapturePlanInTx(context.Background(), nil, testCatalog())
	requireError(t, err, "transaction")
}

func TestCapturePlanClosesFailedRowsAndSuppressesDriverDetails(t *testing.T) {
	for _, test := range []struct {
		name   string
		edit   func(*planCaptureTx)
		code   string
		closed int
	}{
		{"settings", func(tx *planCaptureTx) { tx.failRow = 0 }, "transaction", 0},
		{"identity", func(tx *planCaptureTx) { tx.failRow = 1 }, "identity", 0},
		{"counts", func(tx *planCaptureTx) { tx.failRow = 2 }, "catalog-read", 0},
		{"middle query", func(tx *planCaptureTx) { tx.failQuery = 2 }, "catalog-read", 2},
		{"row iteration", func(tx *planCaptureTx) { tx.rowError = true }, "catalog-read", 1},
		{"scan", func(tx *planCaptureTx) { tx.scanError = true }, "catalog-read", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := newPlanCaptureTx()
			test.edit(tx)
			snapshot, err := CapturePlanInTx(context.Background(), tx, testCatalog())
			requireError(t, err, test.code)
			if !reflect.DeepEqual(snapshot, Snapshot{}) || tx.closed != test.closed {
				t.Fatal("partial result or leaked rows")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx := newPlanCaptureTx()
	tx.cancel = cancel
	tx.scanError = true
	_, err := CapturePlanInTx(ctx, tx, testCatalog())
	requireError(t, err, "catalog-cancelled")
	if tx.closed != 1 || tx.queriesRead != 1 {
		t.Fatal("cancelled rows not closed")
	}
}

func TestCapturePlanRejectsMalformedExpectedCatalogBeforeAnyQuery(t *testing.T) {
	for _, expected := range []Catalog{{}, {Version: Version, Objects: []Object{{Kind: "relations", Identity: "organizations", Attributes: map[string]string{}, Missing: true}}}, {Version: Version, Objects: []Object{{Kind: "relations", Identity: "organizations", Attributes: map[string]string{}}}}} {
		tx := newPlanCaptureTx()
		if _, err := CapturePlanInTx(context.Background(), tx, expected); err == nil {
			t.Fatal("malformed expectation accepted")
		}
		if tx.rowsRead != 0 || tx.queriesRead != 0 {
			t.Fatal("queried before expectation validation")
		}
	}
}
