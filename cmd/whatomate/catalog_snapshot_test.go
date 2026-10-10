package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
)

func TestCatalogSnapshotArgumentsAreValidatedBeforeConfiguration(t *testing.T) {
	for _, args := range [][]string{
		{"-mode", "person@example.invalid"},
		{"-seed-counts=private-value"},
		{"-not-a-flag", "private-value"},
		{"-config"},
		{"unexpected-private-positional"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			called := false
			load := func(string) (*config.Config, error) { called = true; return nil, nil }
			code := catalogSnapshotCommand(context.Background(), args, &stdout, &stderr, load, nil)
			if code != 2 || called || stdout.Len() != 0 || stderr.String() != "catalog-snapshot:invalid-arguments; use -help\n" {
				t.Fatalf("unsafe invalid-argument handling: code=%d called=%t output=%q diagnostic=%q", code, called, stdout.String(), stderr.String())
			}
		})
	}
}

func TestCatalogSnapshotHelpDoesNotLoadConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	load := func(string) (*config.Config, error) { t.Fatal("help loaded config"); return nil, nil }
	code := catalogSnapshotCommand(context.Background(), []string{"-help"}, &stdout, &stderr, load, nil)
	if code != 0 || stdout.String() != catalogSnapshotUsage || stderr.Len() != 0 {
		t.Fatal("help must succeed without configuration or a database connection")
	}
}

func TestCatalogSnapshotFailuresDoNotExposePrivateInput(t *testing.T) {
	privateError := errors.New("postgres://user:private-password@private-host/db person@example.invalid 11111111-2222-4333-8444-555555555555")
	for _, phase := range []string{"configuration", "capture"} {
		t.Run(phase, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			load := func(string) (*config.Config, error) {
				if phase == "configuration" {
					return nil, privateError
				}
				return &config.Config{}, nil
			}
			collect := func(context.Context, config.DatabaseConfig, catalogSnapshotArgs) ([]byte, error) {
				return []byte("partial-private-result"), privateError
			}
			code := catalogSnapshotCommand(context.Background(), []string{"-config", "private-config-name"}, &stdout, &stderr, load, collect)
			if code != 1 || stdout.Len() != 0 {
				t.Fatal("failure published partial result")
			}
			want := "catalog-snapshot:capture-failed\n"
			if phase == "configuration" {
				want = "catalog-snapshot:configuration-unavailable\n"
			}
			if stderr.String() != want {
				t.Fatal("diagnostic included private input")
			}
		})
	}
}

func TestCatalogSnapshotPassesExplicitOptionsWithBoundedContext(t *testing.T) {
	var stdout, stderr bytes.Buffer
	load := func(path string) (*config.Config, error) {
		if path != "local.toml" {
			t.Fatal("wrong config path")
		}
		return &config.Config{Database: config.DatabaseConfig{URL: "runtime-only", MigrationURL: "must-not-be-used"}}, nil
	}
	collect := func(ctx context.Context, cfg config.DatabaseConfig, args catalogSnapshotArgs) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > catalogSnapshotTimeout {
			t.Fatal("capture context is not bounded")
		}
		if cfg.URL != "runtime-only" || args.mode != "json" || !args.seedCounts || !args.listUnknown {
			t.Fatal("options changed")
		}
		return []byte("validated-report\n"), nil
	}
	code := catalogSnapshotCommand(context.Background(), []string{"-config", "local.toml", "-mode", "json", "-seed-counts", "-list-unknown"}, &stdout, &stderr, load, collect)
	if code != 0 || stdout.String() != "validated-report\n" || stderr.Len() != 0 {
		t.Fatal("successful output changed")
	}
}

func TestCatalogSnapshotDefaultsDoNotOptIntoPrivateNames(t *testing.T) {
	args, err := parseCatalogSnapshotArgs(nil)
	if err != nil || args.configPath != "config.toml" || args.mode != "compare" || args.listUnknown || args.seedCounts {
		t.Fatal("unsafe command defaults")
	}
}

func TestCatalogRuntimeDSNNeverFallsBackToMigrationCredentials(t *testing.T) {
	if _, err := catalogRuntimeDSN(config.DatabaseConfig{MigrationURL: "postgres://migration:private@host/db"}); err == nil {
		t.Fatal("accepted migration-only configuration")
	}
	dsn, err := catalogRuntimeDSN(config.DatabaseConfig{URL: "postgres://runtime@host/db", MigrationURL: "postgres://migration:private@host/db"})
	if err != nil || dsn != "postgres://runtime@host/db" {
		t.Fatal("did not choose runtime URL")
	}
}

func TestCatalogRuntimeDSNQuotesCredentialFields(t *testing.T) {
	cfg := config.DatabaseConfig{Host: "::1", Port: 5432, User: "runtime user", Password: "synthetic @/?#%", Name: "catalog test", SSLMode: "require", MigrationURL: "never-use-this"}
	dsn, err := catalogRuntimeDSN(cfg)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("runtime URI is invalid")
	}
	password, _ := u.User.Password()
	if u.User.Username() != cfg.User || password != cfg.Password || u.Hostname() != cfg.Host || u.Path != "/"+cfg.Name || u.Query().Get("sslmode") != "require" {
		t.Fatal("runtime field escaping changed data")
	}
}

type catalogFailWriter struct{}

func (catalogFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCatalogSnapshotOutputFailureReturnsFailure(t *testing.T) {
	var stderr bytes.Buffer
	load := func(string) (*config.Config, error) { return &config.Config{}, nil }
	collect := func(context.Context, config.DatabaseConfig, catalogSnapshotArgs) ([]byte, error) {
		return []byte("report\n"), nil
	}
	if catalogSnapshotCommand(context.Background(), nil, catalogFailWriter{}, &stderr, load, collect) != 1 || stderr.String() != "catalog-snapshot:output-failed\n" {
		t.Fatal("output failure was hidden")
	}
}
