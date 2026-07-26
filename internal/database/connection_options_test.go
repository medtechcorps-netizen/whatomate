package database

import (
	"crypto/tls"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresDSNPrefersURL(t *testing.T) {
	cfg := &config.DatabaseConfig{
		URL:      "postgres://user:pass@private-db:25060/app?sslmode=require",
		Host:     "public-db",
		Port:     5432,
		User:     "ignored",
		Password: "ignored",
		Name:     "ignored",
		SSLMode:  "disable",
	}

	assert.Equal(t, cfg.URL, postgresDSN(cfg))
}

func TestPostgresDSNFallsBackToIndividualFields(t *testing.T) {
	cfg := &config.DatabaseConfig{
		Host:     "db.internal",
		Port:     5432,
		User:     "app",
		Password: "secret",
		Name:     "rereply",
		SSLMode:  "require",
	}

	assert.Equal(
		t,
		"host=db.internal port=5432 user=app password=secret dbname=rereply sslmode=require",
		postgresDSN(cfg),
	)
}

func TestRedisOptionsParsesPrivateTLSURL(t *testing.T) {
	cfg := &config.RedisConfig{
		URL:  "rediss://app:secret@private-cache:25061/2",
		Host: "public-cache",
		Port: 6379,
	}

	opts, err := redisOptions(cfg)
	require.NoError(t, err)
	assert.Equal(t, "private-cache:25061", opts.Addr)
	assert.Equal(t, "app", opts.Username)
	assert.Equal(t, "secret", opts.Password)
	assert.Equal(t, 2, opts.DB)
	require.NotNil(t, opts.TLSConfig)
	assert.GreaterOrEqual(t, opts.TLSConfig.MinVersion, uint16(tls.VersionTLS12))
}

func TestRedisOptionsRejectsInvalidURL(t *testing.T) {
	_, err := redisOptions(&config.RedisConfig{URL: "https://not-redis"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid redis URL")
}
