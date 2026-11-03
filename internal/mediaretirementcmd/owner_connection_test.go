package mediaretirementcmd

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/mediaretirement"
	"github.com/stretchr/testify/require"
)

func TestMediaOwnerInputExplicitArguments(t *testing.T) {
	base := mediaCommandArgs(filepath.Join(t.TempDir(), "manifest.json"))
	a, err := parseMediaInventoryArgs(base)
	require.NoError(t, err)
	require.False(t, a.ownerStdin)
	for _, flag := range []string{"-owner-stdin", "--owner-stdin", "-owner-stdin=true"} {
		a, err := parseMediaInventoryArgs(append(append([]string{}, base...), flag))
		require.NoError(t, err)
		require.True(t, a.ownerStdin)
	}
	for _, flags := range [][]string{{"-owner-stdin=false"}, {"-owner-stdin=TRUE"}, {"-owner-stdin=1"}, {"-owner-stdin", "true"}, {"-owner-stdin", "--owner-stdin"}, {"-owner-password=synthetic"}, {"-owner-file=synthetic"}} {
		_, err := parseMediaInventoryArgs(append(append([]string{}, base...), flags...))
		require.ErrorIs(t, err, mediaretirement.ErrIncomplete)
	}
}

func TestMediaOwnerInputConnectionSelectionAndBufferDisposal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cfg := config.DatabaseConfig{URL: "postgres://runtime:runtime-secret@runtime.example:25060/app?sslmode=require", MigrationURL: "postgres://forbidden:forbidden-secret@other.example/forbidden"}
	before := cfg
	defaultConfig, err := prepareMediaInventoryConnection(ctx, cfg, false, func(context.Context) ([]byte, error) {
		t.Fatal("default mode must not read owner input")
		return nil, nil
	})
	require.NoError(t, err)
	require.Equal(t, "runtime", defaultConfig.User)
	require.Equal(t, "runtime.example", defaultConfig.Host)
	for _, scenario := range []string{"valid", "reader-error", "malformed", "empty", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			work, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			raw := mediaOwnerTestEnvelope(t)
			if scenario == "malformed" {
				raw = []byte("synthetic malformed payload")
			}
			if scenario == "empty" {
				raw = nil
			}
			called := 0
			parsed, err := prepareMediaInventoryConnection(work, cfg, true, func(context.Context) ([]byte, error) {
				called++
				if scenario == "canceled" {
					stop()
				}
				if scenario == "reader-error" {
					return raw, errors.New("synthetic private driver error")
				}
				return raw, nil
			})
			require.Equal(t, 1, called)
			for _, b := range raw {
				require.Zero(t, b)
			}
			require.Equal(t, before, cfg)
			if scenario == "valid" {
				require.NoError(t, err)
				require.Equal(t, "inventory_owner", parsed.User)
				require.Equal(t, defaultConfig.Host, parsed.Host)
				require.Equal(t, defaultConfig.Port, parsed.Port)
				require.Equal(t, defaultConfig.Database, parsed.Database)
				require.False(t, parsed.TLSConfig.InsecureSkipVerify)
				require.Equal(t, parsed.Host, parsed.TLSConfig.ServerName)
			} else {
				require.ErrorIs(t, err, mediaretirement.ErrIncomplete)
				require.Nil(t, parsed)
			}
		})
	}
	parsed, err := prepareMediaInventoryConnection(ctx, cfg, true, nil)
	require.ErrorIs(t, err, mediaretirement.ErrIncomplete)
	require.Nil(t, parsed)
	for _, c := range []context.Context{nil, context.Background()} {
		parsed, err := prepareMediaInventoryConnection(c, cfg, true, func(context.Context) ([]byte, error) { t.Fatal("unbounded input must not be read"); return nil, nil })
		require.ErrorIs(t, err, mediaretirement.ErrIncomplete)
		require.Nil(t, parsed)
	}
}
