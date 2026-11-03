package mediaretirementcmd

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/mediaretirement"
)

func mediaInventoryDSN(cfg config.DatabaseConfig) (string, error) {
	if cfg.URL != "" {
		return cfg.URL, nil
	}
	if cfg.Host == "" || cfg.Port < 1 || cfg.Port > 65535 || cfg.User == "" || cfg.Name == "" {
		return "", mediaretirement.ErrIncomplete
	}
	u := &url.URL{Scheme: "postgres", Host: net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Path: "/" + cfg.Name, User: url.UserPassword(cfg.User, cfg.Password)}
	query := url.Values{}
	query.Set("sslmode", cfg.SSLMode)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func mediaInventoryConfig(parsed *pgx.ConnConfig, workAt time.Time, ctx context.Context) (*pgx.ConnConfig, error) {
	if parsed == nil || ctx == nil || ctx.Err() != nil {
		return nil, mediaretirement.ErrIncomplete
	}
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(workAt) {
		return nil, mediaretirement.ErrIncomplete
	}
	cfg := parsed.Copy()
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = make(map[string]string)
	}
	if cfg.RuntimeParams["options"] != "" || cfg.RuntimeParams["role"] != "" {
		return nil, mediaretirement.ErrIncomplete
	}
	remaining := time.Until(workAt)
	if remaining < time.Millisecond {
		return nil, mediaretirement.ErrIncomplete
	}
	if cfg.ConnectTimeout <= 0 || cfg.ConnectTimeout > remaining {
		cfg.ConnectTimeout = remaining
	}
	if cfg.ConnectTimeout > 3*time.Second {
		cfg.ConnectTimeout = 3 * time.Second
	}
	cfg.RuntimeParams["search_path"] = "pg_catalog"
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	for key, cap := range map[string]time.Duration{"statement_timeout": 3 * time.Second, "lock_timeout": time.Second, "idle_in_transaction_session_timeout": 5 * time.Second} {
		if remaining < cap {
			cap = remaining
		}
		if cap.Milliseconds() < 1 {
			return nil, mediaretirement.ErrIncomplete
		}
		cfg.RuntimeParams[key] = strconv.FormatInt(cap.Milliseconds(), 10) + "ms"
	}
	return cfg, nil
}
