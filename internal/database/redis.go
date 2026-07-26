package database

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/config"
)

// NewRedis creates a new Redis client
func NewRedis(cfg *config.RedisConfig) (*redis.Client, error) {
	opts, err := redisOptions(cfg)
	if err != nil {
		return nil, err
	}

	client := redis.NewClient(opts)

	// Test connection
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to redis: %w", err)
	}

	return client, nil
}

func redisOptions(cfg *config.RedisConfig) (*redis.Options, error) {
	if cfg.URL != "" {
		opts, err := redis.ParseURL(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("invalid redis URL: %w", err)
		}
		if opts.TLSConfig != nil && opts.TLSConfig.MinVersion < tls.VersionTLS12 {
			opts.TLSConfig.MinVersion = tls.VersionTLS12
		}
		return opts, nil
	}

	opts := &redis.Options{
		Addr:     fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Username: cfg.Username,
		Password: cfg.Password,
		DB:       cfg.DB,
	}

	if cfg.TLS {
		opts.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	return opts, nil
}
