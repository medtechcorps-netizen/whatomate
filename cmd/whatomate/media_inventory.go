package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/mediaretirement"
)

const mediaInventoryUsage = `Usage: rereply media-reference-inventory [options]
  -config string          Configuration file (default "config.toml")
  -organization-id UUID   Exact organization UUID
  -account-id UUID        Exact current WhatsApp account UUID
  -output /absolute/file  New private JSON file, in an existing owned 0700 directory

Run in the Linux application environment with the explicitly configured database
owner connection. No migration credential fallback is used. Output includes all
nine reference families in the organization, with current account selectors;
historical attribution remains unverified. Nothing is deleted or fetched from
object storage. The bounded snapshot is closed before the private file is saved.
Existing files, symlinks and partial/truncated inventories are refused.
`

type mediaInventoryArgs struct{ configPath, organizationID, accountID, output string }
type mediaInventoryCollector func(context.Context, config.DatabaseConfig, mediaInventoryArgs) ([]byte, error)
type mediaInventoryOutput interface {
	publish(context.Context, []byte) (bool, error)
	close() error
}

func parseMediaInventoryArgs(args []string) (mediaInventoryArgs, error) {
	var a mediaInventoryArgs
	f := flag.NewFlagSet("media-reference-inventory", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&a.configPath, "config", "config.toml", "")
	f.StringVar(&a.organizationID, "organization-id", "", "")
	f.StringVar(&a.accountID, "account-id", "", "")
	f.StringVar(&a.output, "output", "", "")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return a, flag.ErrHelp
		}
		return a, mediaretirement.ErrIncomplete
	}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
			return a, mediaretirement.ErrIncomplete
		}
		name, _, equal := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if seen[name] {
			return a, mediaretirement.ErrIncomplete
		}
		seen[name] = true
		switch name {
		case "config", "organization-id", "account-id", "output":
		default:
			return a, mediaretirement.ErrIncomplete
		}
		if !equal {
			i++
			if i >= len(args) {
				return a, mediaretirement.ErrIncomplete
			}
		}
	}
	for _, value := range []string{a.organizationID, a.accountID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return a, mediaretirement.ErrIncomplete
		}
	}
	if f.NArg() != 0 || a.configPath == "" || !filepath.IsAbs(a.output) || filepath.Clean(a.output) != a.output || filepath.Base(a.output) == "." || filepath.Base(a.output) == string(filepath.Separator) || strings.ContainsAny(a.output, "\x00\r\n") {
		return a, mediaretirement.ErrIncomplete
	}
	return a, nil
}

func runMediaReferenceInventory(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := mediaInventoryCommand(ctx, args, os.Stdout, os.Stderr, config.Load, collectMediaInventory, openMediaInventoryOutput)
	stop()
	if code != 0 {
		os.Exit(code)
	}
}

func mediaInventoryCommand(parent context.Context, args []string, stdout, stderr io.Writer, load func(string) (*config.Config, error), collect mediaInventoryCollector, open func(string) (mediaInventoryOutput, error)) (code int) {
	a, err := parseMediaInventoryArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		if _, err := io.WriteString(stdout, mediaInventoryUsage); err != nil {
			return 1
		}
		return 0
	}
	if err != nil {
		_, _ = io.WriteString(stderr, "media-reference-inventory:invalid-arguments; use -help\n")
		return 2
	}
	if parent == nil || parent.Err() != nil {
		_, _ = io.WriteString(stderr, "media-reference-inventory:capture-failed\n")
		return 1
	}
	ctx, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()
	output, err := open(a.output)
	if err != nil || output == nil {
		_, _ = io.WriteString(stderr, "media-reference-inventory:private-output-unavailable\n")
		return 1
	}
	defer func() {
		if output.close() != nil {
			_, _ = io.WriteString(stderr, "media-reference-inventory:output-close-failed\n")
			code = 1
		}
	}()
	cfg, err := load(a.configPath)
	if err != nil || cfg == nil {
		_, _ = io.WriteString(stderr, "media-reference-inventory:configuration-unavailable\n")
		return 1
	}
	raw, err := collect(ctx, cfg.Database, a)
	if err != nil || ctx.Err() != nil || len(raw) == 0 || len(raw) > mediaretirement.MaxPrivateBytes || !json.Valid(raw) {
		_, _ = io.WriteString(stderr, "media-reference-inventory:capture-failed\n")
		return 1
	}
	published, err := output.publish(ctx, raw)
	if err != nil || !published {
		if published {
			_, _ = io.WriteString(stderr, "media-reference-inventory:complete-file-durability-unconfirmed\n")
		} else {
			_, _ = io.WriteString(stderr, "media-reference-inventory:output-failed\n")
		}
		return 1
	}
	if _, err := io.WriteString(stdout, "media-reference-inventory:saved-private-manifest\n"); err != nil {
		return 1
	}
	return 0
}

func collectMediaInventory(parent context.Context, cfg config.DatabaseConfig, a mediaInventoryArgs) ([]byte, error) {
	if parent == nil || parent.Err() != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	until := time.Now().Add(33 * time.Second)
	if deadline, ok := parent.Deadline(); ok && deadline.Before(until) {
		until = deadline
	}
	d := planDeadline{workAt: until.Add(-3 * time.Second), finalAt: until}
	ctx, cancel := context.WithDeadline(parent, d.workAt)
	defer cancel()
	dsn, err := catalogRuntimeDSN(cfg)
	if err != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	observer, err := seedDiagnosticConfig(parsed, d, ctx)
	if err != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	observer.RuntimeParams["application_name"] = database.MediaInventoryApplication
	if observer.ConnectTimeout > 3*time.Second {
		observer.ConnectTimeout = 3 * time.Second
	}
	conn, err := pgx.ConnectConfig(ctx, observer)
	if err != nil {
		if conn != nil {
			_, _ = database.CollectMediaReferenceInventory(ctx, conn, database.MediaInventoryScope{})
		}
		return nil, mediaretirement.ErrIncomplete
	}
	raw, err := database.CollectMediaReferenceInventory(ctx, conn, database.MediaInventoryScope{OrganizationID: a.organizationID, AccountID: a.accountID})
	if err != nil || parent.Err() != nil || !time.Now().Before(d.finalAt) {
		return nil, mediaretirement.ErrIncomplete
	}
	return raw, nil
}
