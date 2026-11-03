package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/mediaretirement"
	"github.com/stretchr/testify/require"
)

func mediaCommandArgs(path string) []string {
	return []string{"-organization-id", "10000000-0000-0000-0000-000000000001", "-account-id", "60000000-0000-0000-0000-000000000001", "-output", path}
}

func TestMediaInventoryArgumentsAreExact(t *testing.T) {
	good := mediaCommandArgs(filepath.Join(t.TempDir(), "manifest.json"))
	a, err := parseMediaInventoryArgs(good)
	require.NoError(t, err)
	require.Equal(t, "config.toml", a.configPath)
	_, err = parseMediaInventoryArgs([]string{"-help"})
	require.ErrorIs(t, err, flag.ErrHelp)
	for name, args := range map[string][]string{
		"duplicate": append(append([]string{}, good...), "--output=private"),
		"missing":   good[:4], "positional": append(append([]string{}, good...), "private"),
		"unknown":         append(append([]string{}, good...), "-delete"),
		"zero-id":         {"-organization-id", "00000000-0000-0000-0000-000000000000", "-account-id", good[3], "-output", good[5]},
		"abbreviated-id":  {"-organization-id", "10000000000000000000000000000001", "-account-id", good[3], "-output", good[5]},
		"relative-output": {"-organization-id", good[1], "-account-id", good[3], "-output", "private.json"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseMediaInventoryArgs(args)
			require.ErrorIs(t, err, mediaretirement.ErrIncomplete)
		})
	}
}

type mediaMemoryOutput struct {
	raw    []byte
	closes int
}

func (o *mediaMemoryOutput) publish(_ context.Context, raw []byte) (bool, error) {
	o.raw = append([]byte{}, raw...)
	return true, nil
}
func (o *mediaMemoryOutput) close() error { o.closes++; return nil }

func TestMediaInventoryCommandNeverPrintsPrivatePayload(t *testing.T) {
	args := mediaCommandArgs(filepath.Join(t.TempDir(), "manifest.json"))
	for _, scenario := range []string{"success", "config-failure", "capture-failure", "partial-capture", "invalid-json", "over-budget", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			out := &mediaMemoryOutput{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			load := func(string) (*config.Config, error) {
				if scenario == "config-failure" {
					return nil, errors.New("DO_NOT_TRANSFER")
				}
				return &config.Config{}, nil
			}
			collect := func(ctx context.Context, _ config.DatabaseConfig, _ mediaInventoryArgs) ([]byte, error) {
				_, ok := ctx.Deadline()
				require.True(t, ok)
				switch scenario {
				case "capture-failure":
					return nil, errors.New("DO_NOT_TRANSFER")
				case "partial-capture":
					return []byte(`{"private":"DO_NOT_TRANSFER"}`), errors.New("private driver error")
				case "invalid-json":
					return []byte("DO_NOT_TRANSFER"), nil
				case "over-budget":
					return []byte(`"` + strings.Repeat("x", mediaretirement.MaxPrivateBytes) + `"`), nil
				case "cancelled":
					cancel()
				}
				return []byte(`{"private":"DO_NOT_TRANSFER"}`), nil
			}
			code := mediaInventoryCommand(ctx, args, &stdout, &stderr, load, collect, func(string) (mediaInventoryOutput, error) { return out, nil })
			require.NotContains(t, stdout.String()+stderr.String(), "DO_NOT_TRANSFER")
			require.NotContains(t, stdout.String()+stderr.String(), args[5])
			require.Equal(t, 1, out.closes)
			if scenario == "success" {
				require.Zero(t, code)
				require.NotEmpty(t, out.raw)
				require.Equal(t, "media-reference-inventory:saved-private-manifest\n", stdout.String())
			} else {
				require.Equal(t, 1, code)
				require.Empty(t, out.raw)
				require.Empty(t, stdout.String())
			}
		})
	}
}

func TestMediaInventoryOutputAdmissionPrecedesConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := mediaInventoryCommand(context.Background(), mediaCommandArgs(filepath.Join(t.TempDir(), "manifest.json")), &stdout, &stderr,
		func(string) (*config.Config, error) {
			t.Fatal("must not load credentials after output refusal")
			return nil, nil
		},
		func(context.Context, config.DatabaseConfig, mediaInventoryArgs) ([]byte, error) {
			t.Fatal("must not connect after output refusal")
			return nil, nil
		},
		func(string) (mediaInventoryOutput, error) { return nil, errors.New("DO_NOT_TRANSFER") })
	require.Equal(t, 1, code)
	require.Empty(t, stdout.String())
	require.Equal(t, "media-reference-inventory:private-output-unavailable\n", stderr.String())
}
