package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/stretchr/testify/require"
)

func mediaInventoryTestArguments() []string {
	return []string{"-mode", "json", "-media-inventory", "-organization-id", "00000000-0000-4000-8000-000000000001", "-account-id", "00000000-0000-4000-8000-000000000002",
		"-from", "2026-01-01T00:00:00Z", "-to", "2026-01-02T00:00:00Z", "-private-manifest", "/private-owner/inventory.json"}
}

func TestMediaInventoryCLIValidationAndPrivateFailures(t *testing.T) {
	valid := mediaInventoryTestArguments()
	parsed, err := parseCatalogSnapshotArgs(valid)
	require.NoError(t, err)
	require.Equal(t, 100, parsed.mediaLimit)
	for _, tc := range []struct {
		name string
		tail []string
	}{
		{"duplicate_scope", []string{"-account-id", "private-secret"}},
		{"duplicate_bool", []string{"--media-inventory=true"}},
		{"invalid_cap", []string{"-limit", "501"}},
		{"zero_cap", []string{"-limit", "0"}},
		{"seed_diff", []string{"-seed-diff"}},
		{"seed_counts", []string{"-seed-counts"}},
		{"unknown", []string{"-apply"}},
		{"positional", []string{"private-secret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string{}, valid...), tc.tail...)
			var out, diagnostic bytes.Buffer
			load := func(string) (*config.Config, error) {
				t.Fatal("invalid invocation loaded configuration")
				return nil, nil
			}
			code := catalogSnapshotCommand(context.Background(), args, &out, &diagnostic, load, nil)
			require.Equal(t, 2, code)
			require.Empty(t, out.String())
			require.Equal(t, "catalog-snapshot:invalid-arguments; use -help\n", diagnostic.String())
		})
	}
	for _, tc := range []struct {
		index int
		value string
	}{
		{4, "00000000-0000-0000-0000-000000000000"}, {6, "not-a-uuid"}, {8, "2026-01-03T00:00:00Z"}, {10, "2026-01-10T00:00:00Z"}, {10, "9999-01-02T00:00:00Z"}, {12, "relative.json"}, {12, "/private/../escape.json"}, {12, "/private\\escape.json"},
	} {
		args := append([]string{}, valid...)
		args[tc.index] = tc.value
		_, err := parseCatalogSnapshotArgs(args)
		require.Error(t, err)
	}
	for _, args := range [][]string{{"-limit", "1"}, {"-organization-id", "private-secret"}, {"-private-manifest", "/private/inventory.json"}} {
		_, err := parseCatalogSnapshotArgs(args)
		require.Error(t, err)
	}
	var out, diagnostic bytes.Buffer
	code := catalogSnapshotCommand(context.Background(), valid, &out, &diagnostic, func(string) (*config.Config, error) { return &config.Config{}, nil },
		func(context.Context, config.DatabaseConfig, catalogSnapshotArgs) ([]byte, error) {
			return []byte("private partial result"), errors.New("private driver password")
		})
	require.Equal(t, 1, code)
	require.Empty(t, out.String())
	require.Equal(t, "catalog-snapshot:capture-failed\n", diagnostic.String())
}

func TestMediaInventoryExclusivePrivateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("private diagnostic requires native POSIX permissions")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	target := filepath.Join(dir, "inventory.json")
	raw := []byte(`{"inventory_only":true,"apply_authorized":false}`)
	require.NoError(t, writeMediaInventoryPrivate(context.Background(), target, raw, time.Now().Add(time.Second)))
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	require.Error(t, writeMediaInventoryPrivate(context.Background(), target, []byte("overwrite"), time.Now().Add(time.Second)))
	saved, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, raw, saved)
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "symlink.json")))
	require.Error(t, writeMediaInventoryPrivate(context.Background(), filepath.Join(dir, "symlink.json"), raw, time.Now().Add(time.Second)))
	unsafe := filepath.Join(dir, "unsafe")
	require.NoError(t, os.Mkdir(unsafe, 0755))
	require.Error(t, writeMediaInventoryPrivate(context.Background(), filepath.Join(unsafe, "inventory.json"), raw, time.Now().Add(time.Second)))
	alias := filepath.Join(dir, "alias")
	require.NoError(t, os.Symlink(dir, alias))
	require.Error(t, writeMediaInventoryPrivate(context.Background(), filepath.Join(alias, "new.json"), raw, time.Now().Add(time.Second)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, writeMediaInventoryPrivate(ctx, filepath.Join(dir, "cancelled.json"), raw, time.Now().Add(time.Second)))
	_, err = os.Stat(filepath.Join(dir, "cancelled.json"))
	require.True(t, os.IsNotExist(err))
	require.Error(t, writeMediaInventoryPrivate(context.Background(), filepath.Join(dir, "oversize.json"), make([]byte, 2*1024*1024+1), time.Now().Add(time.Second)))
}
