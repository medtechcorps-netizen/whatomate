package mediaretirementcmd

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/stretchr/testify/require"
)

func TestMediaInventoryConfigPreservesTLSAndBounds(t *testing.T) {
	parsed, err := pgx.ParseConfig("postgres://fixture:secret@localhost/fixture?sslmode=verify-full&connect_timeout=9")
	require.NoError(t, err)
	before := parsed.Copy()
	deadline := time.Now().Add(2 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	got, err := mediaInventoryConfig(parsed, deadline, ctx)
	require.NoError(t, err)
	require.Equal(t, before.RuntimeParams, parsed.RuntimeParams)
	require.Equal(t, before.ConnectTimeout, parsed.ConnectTimeout)
	require.Equal(t, before.TLSConfig.ServerName, parsed.TLSConfig.ServerName)
	require.NotSame(t, parsed, got)
	require.NotNil(t, got.TLSConfig)
	require.False(t, got.TLSConfig.InsecureSkipVerify)
	require.Equal(t, parsed.TLSConfig.ServerName, got.TLSConfig.ServerName)
	require.Equal(t, parsed.User, got.User)
	require.Equal(t, parsed.Password, got.Password)
	require.Equal(t, parsed.Database, got.Database)
	require.Positive(t, got.ConnectTimeout)
	require.LessOrEqual(t, got.ConnectTimeout, 2*time.Second)
	require.Equal(t, "pg_catalog", got.RuntimeParams["search_path"])
	require.Equal(t, "on", got.RuntimeParams["default_transaction_read_only"])
	require.Equal(t, "repeatable read", got.RuntimeParams["default_transaction_isolation"])
	for key, cap := range map[string]time.Duration{"statement_timeout": 2 * time.Second, "lock_timeout": time.Second, "idle_in_transaction_session_timeout": 2 * time.Second} {
		value, err := time.ParseDuration(got.RuntimeParams[key])
		require.NoError(t, err)
		require.Positive(t, value)
		require.LessOrEqual(t, value, cap)
	}
}

func TestMediaInventoryConfigRefusesStartupOverridesAndUnboundedContext(t *testing.T) {
	for _, key := range []string{"role", "options"} {
		t.Run(key, func(t *testing.T) {
			parsed, err := pgx.ParseConfig("postgres://fixture@localhost/fixture?sslmode=verify-full")
			require.NoError(t, err)
			parsed.RuntimeParams[key] = "override"
			deadline := time.Now().Add(time.Second)
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			got, err := mediaInventoryConfig(parsed, deadline, ctx)
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
	parsed, err := pgx.ParseConfig("postgres://fixture@localhost/fixture?sslmode=verify-full")
	require.NoError(t, err)
	got, err := mediaInventoryConfig(parsed, time.Now().Add(time.Second), context.Background())
	require.Error(t, err)
	require.Nil(t, got)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	cancel()
	got, err = mediaInventoryConfig(parsed, time.Now().Add(time.Second), ctx)
	require.Error(t, err)
	require.Nil(t, got)
}

func TestMediaInventoryDSNUsesConfiguredConnectionOnly(t *testing.T) {
	const runtime = "postgres://runtime@localhost/fixture?sslmode=verify-full"
	got, err := mediaInventoryDSN(config.DatabaseConfig{URL: runtime, MigrationURL: "postgres://owner@localhost/fixture"})
	require.NoError(t, err)
	require.Equal(t, runtime, got)
	got, err = mediaInventoryDSN(config.DatabaseConfig{MigrationURL: "postgres://owner@localhost/fixture"})
	require.Error(t, err)
	require.Empty(t, got)
	got, err = mediaInventoryDSN(config.DatabaseConfig{Host: "::1", Port: 5432, User: "configured", Password: "a'@&", Name: "fixture", SSLMode: "verify-full"})
	require.NoError(t, err)
	parsed, err := pgx.ParseConfig(got)
	require.NoError(t, err)
	require.Equal(t, "::1", parsed.Host)
	require.Equal(t, "configured", parsed.User)
	require.Equal(t, "a'@&", parsed.Password)
	require.NotNil(t, parsed.TLSConfig)
	require.False(t, parsed.TLSConfig.InsecureSkipVerify)
}
