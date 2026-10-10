package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
)

const catalogSnapshotTimeout = 60 * time.Second

const catalogSnapshotUsage = `Usage: rereply catalog-snapshot [options]
  -config string  Configuration file (default "config.toml")
  -mode string    compare, json or sha256 (default "compare")
  -seed-counts    Include global seed counts and key hashes, never seed values
  -list-unknown   Show unknown public identifiers for the owner's console only

Uses the runtime database connection in a bounded read-only transaction.
Default output omits unknown identifiers, role names and non-public schema names.
Keep -list-unknown output private; review identifiers individually before sharing.
`

type catalogSnapshotArgs struct {
	configPath  string
	mode        string
	seedCounts  bool
	listUnknown bool
}

type catalogSnapshotCollector func(context.Context, config.DatabaseConfig, catalogSnapshotArgs) ([]byte, error)

func parseCatalogSnapshotArgs(args []string) (catalogSnapshotArgs, error) {
	var parsed catalogSnapshotArgs
	flags := flag.NewFlagSet("catalog-snapshot", flag.ContinueOnError)
	// The flag package includes supplied values in errors. These can be private.
	flags.SetOutput(io.Discard)
	flags.StringVar(&parsed.configPath, "config", "config.toml", "configuration file")
	flags.StringVar(&parsed.mode, "mode", "compare", "output mode")
	flags.BoolVar(&parsed.seedCounts, "seed-counts", false, "global seed counts")
	flags.BoolVar(&parsed.listUnknown, "list-unknown", false, "owner-only identifiers")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return parsed, flag.ErrHelp
		}
		return parsed, errors.New("invalid arguments")
	}
	if flags.NArg() != 0 || (parsed.mode != "compare" && parsed.mode != "json" && parsed.mode != "sha256") {
		return parsed, errors.New("invalid arguments")
	}
	return parsed, nil
}

func runCatalogSnapshot(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := catalogSnapshotCommand(ctx, args, os.Stdout, os.Stderr, config.Load, collectCatalogSnapshot)
	stop()
	if code != 0 {
		os.Exit(code)
	}
}

func catalogSnapshotCommand(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
	load func(string) (*config.Config, error),
	collect catalogSnapshotCollector,
) int {
	parsed, err := parseCatalogSnapshotArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		if _, err := io.WriteString(stdout, catalogSnapshotUsage); err != nil {
			return 1
		}
		return 0
	}
	if err != nil {
		_, _ = io.WriteString(stderr, "catalog-snapshot:invalid-arguments; use -help\n")
		return 2
	}
	cfg, err := load(parsed.configPath)
	if err != nil || cfg == nil {
		_, _ = io.WriteString(stderr, "catalog-snapshot:configuration-unavailable\n")
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, catalogSnapshotTimeout)
	defer cancel()
	report, err := collect(ctx, cfg.Database, parsed)
	if err != nil {
		// Never print a driver error, DSN, config path or database identifier.
		_, _ = io.WriteString(stderr, "catalog-snapshot:capture-failed\n")
		return 1
	}
	if _, err := stdout.Write(report); err != nil {
		_, _ = io.WriteString(stderr, "catalog-snapshot:output-failed\n")
		return 1
	}
	return 0
}

func catalogRuntimeDSN(cfg config.DatabaseConfig) (string, error) {
	if cfg.URL != "" {
		return cfg.URL, nil
	}
	if cfg.Host == "" || cfg.Port < 1 || cfg.Port > 65535 || cfg.User == "" || cfg.Name == "" {
		return "", errors.New("runtime database configuration required")
	}
	u := &url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path:   "/" + cfg.Name,
		User:   url.UserPassword(cfg.User, cfg.Password),
	}
	query := url.Values{}
	query.Set("sslmode", cfg.SSLMode)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func collectCatalogSnapshot(ctx context.Context, cfg config.DatabaseConfig, args catalogSnapshotArgs) ([]byte, error) {
	dsn, err := catalogRuntimeDSN(cfg)
	if err != nil {
		return nil, err
	}
	golden, err := dbcatalog.BuiltinCatalog()
	if err != nil {
		return nil, err
	}
	global, err := dbcatalog.BuiltinGlobalTables()
	if err != nil {
		return nil, err
	}
	snapshot, err := dbcatalog.Capture(ctx, dsn, dbcatalog.Options{
		Catalog: golden, GlobalTables: global, IncludeSeeds: args.seedCounts, Timeout: catalogSnapshotTimeout,
	})
	if err != nil {
		return nil, err
	}
	// Validate and finish every section before publishing any output.
	var report bytes.Buffer
	if err := dbcatalog.WritePublic(&report, snapshot, golden, args.mode); err != nil {
		return nil, err
	}
	if args.listUnknown {
		for _, identity := range dbcatalog.UnknownIdentities(snapshot) {
			// Quoting prevents unusual catalog names from injecting terminal lines.
			_, _ = fmt.Fprintf(&report, "owner_only_unknown %q\n", identity)
		}
	}
	return report.Bytes(), nil
}
