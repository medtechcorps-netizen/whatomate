package mediaretirementcmd

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/mediaretirement"
)

// prepareMediaInventoryConnection keeps the default connection path intact.
// Owner input is opt-in and changes only authentication and the verified TLS
// root for this short-lived connection. No MigrationURL fallback is available.
func prepareMediaInventoryConnection(ctx context.Context, cfg config.DatabaseConfig, explicitOwner bool, readOwner func(context.Context) ([]byte, error)) (*pgx.ConnConfig, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, mediaretirement.ErrIncomplete
	}
	if explicitOwner {
		if readOwner == nil {
			return nil, mediaretirement.ErrIncomplete
		}
		raw, err := readOwner(ctx)
		defer clear(raw)
		if err != nil || ctx.Err() != nil {
			return nil, mediaretirement.ErrIncomplete
		}
		return mediaExplicitOwnerConfig(ctx, cfg, raw)
	}
	dsn, err := mediaInventoryDSN(cfg)
	if err != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	return parsed, nil
}
