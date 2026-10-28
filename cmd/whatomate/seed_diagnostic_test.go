package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/stretchr/testify/require"
)

func TestSeedDiagnosticArgumentsAndFailurePrivacy(t *testing.T) {
	for _, args := range [][]string{
		{"-seed-diff"}, {"-seed-diff", "-mode", "sha256"},
		{"-seed-diff", "-mode", "nullable-v2-baseline"},
		{"-seed-diff", "-mode", "json", "-seed-counts"},
		{"-seed-diff", "-mode", "json", "-list-unknown"},
		{"-seed-diff", "-mode", "json", "private-positional"},
	} {
		var stdout, stderr bytes.Buffer
		load := func(string) (*config.Config, error) { t.Fatal("invalid arguments loaded config"); return nil, nil }
		require.Equal(t, 2, catalogSnapshotCommand(context.Background(), args, &stdout, &stderr, load, nil))
		require.Empty(t, stdout.String())
		require.Equal(t, "catalog-snapshot:invalid-arguments; use -help\n", stderr.String())
	}
	parsed, err := parseCatalogSnapshotArgs([]string{"-seed-diff", "-mode", "json"})
	require.NoError(t, err)
	require.True(t, parsed.seedDiff)
	var stdout, stderr bytes.Buffer
	load := func(string) (*config.Config, error) { return &config.Config{}, nil }
	collect := func(context.Context, config.DatabaseConfig, catalogSnapshotArgs) ([]byte, error) {
		return []byte("private-partial"), errors.New("private-driver-detail")
	}
	require.Equal(t, 1, catalogSnapshotCommand(context.Background(), []string{"-seed-diff", "-mode", "json"}, &stdout, &stderr, load, collect))
	require.Empty(t, stdout.String())
	require.Equal(t, "catalog-snapshot:capture-failed\n", stderr.String())
}

func TestSeedDiagnosticStartupAndRuntimeOnlyConfiguration(t *testing.T) {
	parsed, err := pgx.ParseConfig("host=127.0.0.1 user=fixture dbname=fixture application_name=old")
	require.NoError(t, err)
	d, ok := newPlanDeadline(context.Background(), time.Now())
	require.True(t, ok)
	observer, err := seedDiagnosticConfig(parsed, d, context.Background())
	require.NoError(t, err)
	require.Equal(t, "old", parsed.RuntimeParams["application_name"])
	require.Equal(t, database.SeedDiagnosticApplication, observer.RuntimeParams["application_name"])
	require.Equal(t, "on", observer.RuntimeParams["default_transaction_read_only"])
	require.Equal(t, "repeatable read", observer.RuntimeParams["default_transaction_isolation"])
	require.Equal(t, "pg_catalog", observer.RuntimeParams["search_path"])
	for _, key := range []string{"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout"} {
		value, err := time.ParseDuration(observer.RuntimeParams[key])
		require.NoError(t, err)
		require.Greater(t, value, time.Duration(0))
		require.LessOrEqual(t, value, 5*time.Second)
	}
	for _, key := range []string{"options", "role"} {
		candidate := parsed.Copy()
		candidate.RuntimeParams[key] = "private-override"
		_, err = seedDiagnosticConfig(candidate, d, context.Background())
		require.Error(t, err)
	}
	raw, err := collectSeedDiagnostic(context.Background(), config.DatabaseConfig{MigrationURL: "postgres://migration:secret@unreachable/db"})
	require.Error(t, err)
	require.Nil(t, raw)
	require.NotContains(t, err.Error(), "secret")
	short, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw, err = collectSeedDiagnostic(short, config.DatabaseConfig{URL: "private-invalid"})
	require.Error(t, err)
	require.Nil(t, raw)
}
