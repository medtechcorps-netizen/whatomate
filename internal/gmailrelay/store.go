package gmailrelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
)

const OAuthStateTTL = 10 * time.Minute

var (
	// ErrOAuthStateNotFound covers an absent, expired, or already-consumed state.
	// Callers deliberately receive the same error for all three cases.
	ErrOAuthStateNotFound   = errors.New("OAuth state is invalid or expired")
	ErrOAuthStateCollision  = errors.New("OAuth state already exists")
	ErrRefreshTokenNotFound = errors.New("Gmail refresh token is not configured")
)

// OAuthState is short-lived callback state. The code verifier is secret and
// must never be returned to a browser or written to logs.
type OAuthState struct {
	Nonce        string    `json:"nonce"`
	CodeVerifier string    `json:"code_verifier"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// OAuthStore is the persistence boundary required by OAuthService. Production
// uses RedisStore; the interface also keeps OAuth protocol tests deterministic.
type OAuthStore interface {
	SaveOAuthState(ctx context.Context, state OAuthState) error
	ConsumeOAuthState(ctx context.Context, nonce string) (OAuthState, error)
	SaveRefreshToken(ctx context.Context, refreshToken string) error
	LoadRefreshToken(ctx context.Context) (string, error)
}

type redisBackend interface {
	Ping(ctx context.Context) error
	Set(ctx context.Context, key, value string, expiration time.Duration) error
	SetNX(ctx context.Context, key, value string, expiration time.Duration) (bool, error)
	Get(ctx context.Context, key string) (string, error)
	GetDel(ctx context.Context, key string) (string, error)
	Del(ctx context.Context, key string) error
	Close() error
}

type goRedisBackend struct {
	client *redis.Client
}

func (b *goRedisBackend) Ping(ctx context.Context) error {
	return b.client.Ping(ctx).Err()
}

func (b *goRedisBackend) Set(ctx context.Context, key, value string, expiration time.Duration) error {
	return b.client.Set(ctx, key, value, expiration).Err()
}

func (b *goRedisBackend) SetNX(ctx context.Context, key, value string, expiration time.Duration) (bool, error) {
	return b.client.SetNX(ctx, key, value, expiration).Result()
}

func (b *goRedisBackend) Get(ctx context.Context, key string) (string, error) {
	return b.client.Get(ctx, key).Result()
}

func (b *goRedisBackend) GetDel(ctx context.Context, key string) (string, error) {
	return b.client.GetDel(ctx, key).Result()
}

func (b *goRedisBackend) Del(ctx context.Context, key string) error {
	return b.client.Del(ctx, key).Err()
}

func (b *goRedisBackend) Close() error {
	return b.client.Close()
}

// RedisStore is the production OAuth state and credential store. It has no
// in-memory fallback: losing Redis availability fails closed.
type RedisStore struct {
	backend       redisBackend
	prefix        string
	mailboxKey    string
	encryptionKey string
}

// NewRedisStore constructs a Redis-backed store from the mandatory Redis URL.
func NewRedisStore(config *Config) (*RedisStore, error) {
	if config == nil {
		return nil, errors.New("Gmail relay config is required")
	}
	options, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		return nil, errors.New("Redis configuration is invalid")
	}
	return newRedisStore(config, &goRedisBackend{client: redis.NewClient(options)})
}

func newRedisStore(config *Config, backend redisBackend) (*RedisStore, error) {
	if config == nil || backend == nil {
		return nil, errors.New("Gmail relay Redis store configuration is incomplete")
	}
	if strings.TrimSpace(config.RedisPrefix) == "" || len(config.EncryptionKey) < minimumEncryptionKeyLen {
		return nil, errors.New("Gmail relay Redis store security configuration is incomplete")
	}
	if err := validateMailbox(config.Mailbox); err != nil {
		return nil, errors.New("Gmail relay Redis store mailbox is invalid")
	}
	digest := sha256.Sum256([]byte(config.Mailbox))
	return &RedisStore{
		backend:       backend,
		prefix:        config.RedisPrefix,
		mailboxKey:    hex.EncodeToString(digest[:]),
		encryptionKey: config.EncryptionKey,
	}, nil
}

func (s *RedisStore) Ping(ctx context.Context) error {
	if s == nil || s.backend == nil {
		return errors.New("Redis is not configured")
	}
	if err := s.backend.Ping(ctx); err != nil {
		return fmt.Errorf("ping Redis: %w", err)
	}
	return nil
}

func (s *RedisStore) Close() error {
	if s == nil || s.backend == nil {
		return nil
	}
	return s.backend.Close()
}

func (s *RedisStore) SaveOAuthState(ctx context.Context, state OAuthState) error {
	if s == nil || s.backend == nil {
		return errors.New("Redis is not configured")
	}
	if !validOAuthOpaqueValue(state.Nonce) || !validOAuthOpaqueValue(state.CodeVerifier) || state.ExpiresAt.IsZero() {
		return errors.New("OAuth state is invalid")
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return errors.New("encode OAuth state")
	}
	stored, err := s.backend.SetNX(ctx, s.stateKey(state.Nonce), string(payload), OAuthStateTTL)
	if err != nil {
		return fmt.Errorf("store OAuth state: %w", err)
	}
	if !stored {
		return ErrOAuthStateCollision
	}
	return nil
}

// ConsumeOAuthState atomically uses Redis GETDEL. Concurrent callbacks and
// replay attempts can therefore never both exchange an authorization code.
func (s *RedisStore) ConsumeOAuthState(ctx context.Context, nonce string) (OAuthState, error) {
	if s == nil || s.backend == nil || !validOAuthOpaqueValue(nonce) {
		return OAuthState{}, ErrOAuthStateNotFound
	}
	payload, err := s.backend.GetDel(ctx, s.stateKey(nonce))
	if errors.Is(err, redis.Nil) {
		return OAuthState{}, ErrOAuthStateNotFound
	}
	if err != nil {
		return OAuthState{}, fmt.Errorf("consume OAuth state: %w", err)
	}
	var state OAuthState
	if err := json.Unmarshal([]byte(payload), &state); err != nil ||
		state.Nonce != nonce ||
		!validOAuthOpaqueValue(state.CodeVerifier) ||
		state.ExpiresAt.IsZero() {
		return OAuthState{}, ErrOAuthStateNotFound
	}
	return state, nil
}

// SaveRefreshToken encrypts the refresh token before crossing the Redis
// boundary. Access tokens and Google profile data are never persisted here.
func (s *RedisStore) SaveRefreshToken(ctx context.Context, refreshToken string) error {
	if s == nil || s.backend == nil {
		return errors.New("Redis is not configured")
	}
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return errors.New("Gmail refresh token is empty")
	}
	encrypted, err := appcrypto.Encrypt(refreshToken, s.encryptionKey)
	if err != nil || !appcrypto.IsEncrypted(encrypted) {
		return errors.New("encrypt Gmail refresh token")
	}
	if err := s.backend.Set(ctx, s.refreshTokenKey(), encrypted, 0); err != nil {
		return fmt.Errorf("store Gmail refresh token: %w", err)
	}
	return nil
}

func (s *RedisStore) LoadRefreshToken(ctx context.Context) (string, error) {
	if s == nil || s.backend == nil {
		return "", errors.New("Redis is not configured")
	}
	encrypted, err := s.backend.Get(ctx, s.refreshTokenKey())
	if errors.Is(err, redis.Nil) {
		return "", ErrRefreshTokenNotFound
	}
	if err != nil {
		return "", fmt.Errorf("load Gmail refresh token: %w", err)
	}
	if !appcrypto.IsEncrypted(encrypted) {
		return "", errors.New("stored Gmail refresh token is not encrypted")
	}
	refreshToken, err := appcrypto.Decrypt(encrypted, s.encryptionKey)
	if err != nil || strings.TrimSpace(refreshToken) == "" {
		return "", errors.New("decrypt Gmail refresh token")
	}
	return refreshToken, nil
}

// DeleteRefreshToken disconnects the configured mailbox without affecting any
// other Gmail relay mailbox or OAuth grant.
func (s *RedisStore) DeleteRefreshToken(ctx context.Context) error {
	if s == nil || s.backend == nil {
		return errors.New("Redis is not configured")
	}
	if err := s.backend.Del(ctx, s.refreshTokenKey()); err != nil {
		return fmt.Errorf("delete Gmail refresh token: %w", err)
	}
	return nil
}

func (s *RedisStore) stateKey(nonce string) string {
	return s.prefix + "oauth:state:" + nonce
}

func (s *RedisStore) refreshTokenKey() string {
	return s.prefix + "oauth:refresh:" + s.mailboxKey
}

func validOAuthOpaqueValue(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' || character == '~' {
			continue
		}
		return false
	}
	return true
}
