package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/stretchr/testify/require"
)

func identityInventoryArguments() []string {
	return []string{"-mode", "json", "-identity-inventory", "-organization-id", "11111111-1111-4111-8111-111111111111", "-account-id", "22222222-2222-4222-8222-222222222222"}
}

func TestIdentityInventoryArgumentsAndModeIsolation(t *testing.T) {
	valid := identityInventoryArguments()
	parsed, err := parseCatalogSnapshotArgs(valid)
	require.NoError(t, err)
	require.NoError(t, validateIdentityInventoryArguments(parsed))
	for _, args := range [][]string{
		{"-identity-inventory"},
		{"-mode", "json", "-identity-inventory=false", "-organization-id", parsed.organizationID, "-account-id", parsed.accountID},
		{"-mode", "json", "-organization-id", parsed.organizationID},
		append(append([]string{}, valid...), "-mode=json"),
		append(append([]string{}, valid...), "--account-id="+parsed.accountID),
		append(append([]string{}, valid...), "-identity-inventory"),
		append(append([]string{}, valid...), "positional"),
		append(append([]string{}, valid...), "-seed-diff"),
		append(append([]string{}, valid...), "-seed-counts=false"),
		append(append([]string{}, valid...), "-list-unknown=false"),
		append(append([]string{}, valid...), "-media-inventory"),
		append(append([]string{}, valid...), "-from=2026-01-01"),
		append(append([]string{}, valid...), "-limit=1"),
		append(append([]string{}, valid...), "-private-manifest=private"),
		append(append([]string{}, valid...), "-dsn=private"),
	} {
		var stdout, stderr bytes.Buffer
		code := catalogSnapshotCommand(context.Background(), args, &stdout, &stderr,
			func(string) (*config.Config, error) {
				t.Fatal("invalid arguments reached configuration")
				return nil, nil
			},
			func(context.Context, config.DatabaseConfig, catalogSnapshotArgs) ([]byte, error) {
				t.Fatal("invalid arguments reached collector")
				return nil, nil
			})
		require.Equal(t, 2, code)
		require.Empty(t, stdout.String())
		require.Equal(t, "catalog-snapshot:invalid-arguments; use -help\n", stderr.String())
	}
	for _, value := range []string{"", "00000000-0000-0000-0000-000000000000", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", " " + parsed.organizationID, strings.Repeat("x", 4096)} {
		bad := parsed
		bad.organizationID = value
		require.Error(t, validateIdentityInventoryArguments(bad))
		bad = parsed
		bad.accountID = value
		require.Error(t, validateIdentityInventoryArguments(bad))
	}
	for _, mode := range []string{"compare", "sha256", "nullable-v2-baseline"} {
		bad := parsed
		bad.mode = mode
		require.Error(t, validateIdentityInventoryArguments(bad))
	}
	for _, old := range [][]string{nil, {"-mode", "json"}, {"-mode", "json", "-seed-diff"}, {"-mode", "compare", "-mode", "sha256"}} {
		_, err := parseCatalogSnapshotArgs(old)
		require.NoError(t, err, "ordinary flag behavior remains unchanged")
	}
}

func TestIdentityInventoryRuntimeConfigurationAndPrivacy(t *testing.T) {
	args, err := parseCatalogSnapshotArgs(identityInventoryArguments())
	require.NoError(t, err)
	cfg := config.DatabaseConfig{URL: "postgres://private-user:private-password@private-host/private-db?sslmode=require", MigrationURL: "never-use-migration"}
	dsn, err := catalogRuntimeDSN(cfg)
	require.NoError(t, err)
	require.Equal(t, cfg.URL, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	d, ok := newPlanDeadline(ctx, time.Now())
	require.True(t, ok)
	parsed, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	before := parsed.Copy()
	observer, err := seedDiagnosticConfig(parsed, d, ctx)
	require.NoError(t, err)
	observer.RuntimeParams["application_name"] = database.IdentityInventoryApplication
	require.Equal(t, before.RuntimeParams, parsed.RuntimeParams, "caller config is not mutated")
	require.Equal(t, "on", observer.RuntimeParams["default_transaction_read_only"])
	require.Equal(t, "repeatable read", observer.RuntimeParams["default_transaction_isolation"])
	require.Equal(t, "pg_catalog", observer.RuntimeParams["search_path"])
	require.NotNil(t, observer.TLSConfig)
	require.LessOrEqual(t, observer.ConnectTimeout, 5*time.Second)
	for _, key := range []string{"options", "role"} {
		bad := parsed.Copy()
		bad.RuntimeParams[key] = "private-override"
		_, err := seedDiagnosticConfig(bad, d, ctx)
		require.Error(t, err)
	}
	var stdout, stderr bytes.Buffer
	code := catalogSnapshotCommand(context.Background(), identityInventoryArguments(), &stdout, &stderr,
		func(string) (*config.Config, error) { return &config.Config{Database: cfg}, nil },
		func(context.Context, config.DatabaseConfig, catalogSnapshotArgs) ([]byte, error) {
			return []byte("partial-private"), errors.New(cfg.URL + args.organizationID)
		})
	require.Equal(t, 1, code)
	require.Empty(t, stdout.String())
	require.Equal(t, "catalog-snapshot:capture-failed\n", stderr.String())
	short, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	raw, err := collectIdentityInventory(short, cfg, args)
	require.Error(t, err)
	require.Nil(t, raw)
	cancelled, stopCancelled := context.WithCancel(context.Background())
	stopCancelled()
	raw, err = collectIdentityInventory(cancelled, cfg, args)
	require.Error(t, err)
	require.Nil(t, raw)
}

func identityInventoryEmptyReport() database.IdentityInventoryReport {
	return database.IdentityInventoryReport{Version: 1, Purpose: "identity_inventory", Scope: "selected_account", ReasonSource: "derived_shape", ReasonsOverlap: true, ScopeFailurePolicy: "whole_report_refusal", CurrentDecidability: "not_observed", RecordedAuthentication: "stored_shape_not_fresh_verification", CompleteVisibility: true, ShadowState: "absent", QueryOne: []database.IdentityInventoryQueryOne{}}
}

func TestIdentityInventoryPublicReportClosedAndBounded(t *testing.T) {
	valid := identityInventoryEmptyReport()
	raw, err := json.Marshal(valid)
	require.NoError(t, err)
	_, err = database.DecodeIdentityInventoryReport(raw)
	require.NoError(t, err)
	for _, mutate := range []func(map[string]any){
		func(v map[string]any) { v["sender"] = "private-selector" },
		func(v map[string]any) { v["scope"] = "private-organization" },
		func(v map[string]any) { v["shadow_state"] = "new-unknown-state" },
		func(v map[string]any) { v["counts"].(map[string]any)["holds"] = nil },
		func(v map[string]any) { delete(v["counts"].(map[string]any), "holds") },
		func(v map[string]any) { v["counts"].(map[string]any)["holds"] = -1 },
		func(v map[string]any) { v["counts"].(map[string]any)["holds"] = 10001 },
		func(v map[string]any) { v["counts"].(map[string]any)["member_rows"] = 100001 },
		func(v map[string]any) { v["counts"].(map[string]any)["receipt_rows"] = 20001 },
		func(v map[string]any) { v["counts"].(map[string]any)["holds"] = json.Number("9223372036854775808") },
		func(v map[string]any) { v["query_one"] = nil },
		func(v map[string]any) { v["query_one"] = make([]database.IdentityInventoryQueryOne, 257) },
		func(v map[string]any) { v["query_one"] = make([]database.IdentityInventoryQueryOne, 513) },
	} {
		var value map[string]any
		require.NoError(t, json.Unmarshal(raw, &value))
		mutate(value)
		bad, err := json.Marshal(value)
		require.NoError(t, err)
		got, err := database.DecodeIdentityInventoryReport(bad)
		require.Error(t, err)
		require.False(t, got.CompleteVisibility)
	}
	_, err = database.DecodeIdentityInventoryReport(append(raw, []byte(" {}")...))
	require.Error(t, err)
	_, err = database.DecodeIdentityInventoryReport([]byte(strings.Repeat(" ", 65537)))
	require.Error(t, err)
	padded := append(append([]byte{}, raw...), []byte(strings.Repeat(" ", 65536-len(raw)))...)
	_, err = database.DecodeIdentityInventoryReport(padded)
	require.NoError(t, err, "exact public decoder byte bound")
	_, err = database.DecodeIdentityInventoryReport(append(padded, ' '))
	require.Error(t, err)
	private, err := json.Marshal(map[string]any{"witness": database.IdentityInventoryWitness{}})
	require.NoError(t, err)
	require.JSONEq(t, `{"witness":{}}`, string(private), "scope witness has no exportable fields through a public JSON container")
}
