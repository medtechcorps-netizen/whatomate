package gmailrelay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
)

type fakeRedisBackend struct {
	mutex       sync.Mutex
	values      map[string]string
	expirations map[string]time.Duration
	closed      bool
}

func newFakeRedisBackend() *fakeRedisBackend {
	return &fakeRedisBackend{
		values:      make(map[string]string),
		expirations: make(map[string]time.Duration),
	}
}

func (f *fakeRedisBackend) Ping(context.Context) error { return nil }

func (f *fakeRedisBackend) Set(_ context.Context, key, value string, expiration time.Duration) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.values[key] = value
	f.expirations[key] = expiration
	return nil
}

func (f *fakeRedisBackend) SetNX(_ context.Context, key, value string, expiration time.Duration) (bool, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if _, exists := f.values[key]; exists {
		return false, nil
	}
	f.values[key] = value
	f.expirations[key] = expiration
	return true, nil
}

func (f *fakeRedisBackend) Get(_ context.Context, key string) (string, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	value, exists := f.values[key]
	if !exists {
		return "", redis.Nil
	}
	return value, nil
}

func (f *fakeRedisBackend) GetDel(_ context.Context, key string) (string, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	value, exists := f.values[key]
	if !exists {
		return "", redis.Nil
	}
	delete(f.values, key)
	delete(f.expirations, key)
	return value, nil
}

func (f *fakeRedisBackend) CompareAndSet(
	_ context.Context,
	key, currentValue, replacementValue string,
) (bool, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.values[key] != currentValue {
		return false, nil
	}
	f.values[key] = replacementValue
	f.expirations[key] = 0
	return true, nil
}

func (f *fakeRedisBackend) Del(_ context.Context, key string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	delete(f.values, key)
	delete(f.expirations, key)
	return nil
}

func (f *fakeRedisBackend) Close() error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.closed = true
	return nil
}

func testRedisStore(t *testing.T) (*RedisStore, *fakeRedisBackend, *Config) {
	t.Helper()
	config, err := loadTestConfig(validConfigEnvironment())
	if err != nil {
		t.Fatalf("load test config: %v", err)
	}
	backend := newFakeRedisBackend()
	store, err := newRedisStore(config, backend)
	if err != nil {
		t.Fatalf("build test store: %v", err)
	}
	return store, backend, config
}

func TestRedisStoreConsumesOAuthStateExactlyOnceWithTenMinuteTTL(t *testing.T) {
	store, backend, _ := testRedisStore(t)
	state := OAuthState{
		Nonce:        strings.Repeat("n", 48),
		CodeVerifier: strings.Repeat("v", 64),
		ExpiresAt:    time.Now().UTC().Add(OAuthStateTTL),
	}
	if err := store.SaveOAuthState(context.Background(), state); err != nil {
		t.Fatalf("save OAuth state: %v", err)
	}
	stateKey := store.stateKey(state.Nonce)
	if backend.expirations[stateKey] != 10*time.Minute {
		t.Fatalf("state TTL = %s, want 10m", backend.expirations[stateKey])
	}

	consumed, err := store.ConsumeOAuthState(context.Background(), state.Nonce)
	if err != nil {
		t.Fatalf("consume OAuth state: %v", err)
	}
	if consumed != state {
		t.Fatalf("consumed state mismatch: %#v", consumed)
	}
	if _, err := store.ConsumeOAuthState(context.Background(), state.Nonce); !errors.Is(err, ErrOAuthStateNotFound) {
		t.Fatalf("replay should be rejected, got %v", err)
	}
}

func TestRedisStoreRejectsStateCollision(t *testing.T) {
	store, _, _ := testRedisStore(t)
	state := OAuthState{
		Nonce:        strings.Repeat("n", 48),
		CodeVerifier: strings.Repeat("v", 64),
		ExpiresAt:    time.Now().UTC().Add(OAuthStateTTL),
	}
	if err := store.SaveOAuthState(context.Background(), state); err != nil {
		t.Fatalf("save initial OAuth state: %v", err)
	}
	if err := store.SaveOAuthState(context.Background(), state); !errors.Is(err, ErrOAuthStateCollision) {
		t.Fatalf("expected collision rejection, got %v", err)
	}
}

func TestRedisStorePersistsOnlyEncryptedRefreshToken(t *testing.T) {
	store, backend, config := testRedisStore(t)
	const refreshToken = "google-refresh-token-value"
	if err := store.SaveRefreshToken(context.Background(), refreshToken); err != nil {
		t.Fatalf("save refresh token: %v", err)
	}
	key := store.refreshTokenKey()
	stored := backend.values[key]
	if stored == "" || stored == refreshToken || strings.Contains(stored, refreshToken) {
		t.Fatalf("refresh token was stored in plaintext: %q", stored)
	}
	if !appcrypto.IsEncrypted(stored) {
		t.Fatalf("stored value is not an encrypted envelope: %q", stored)
	}
	decrypted, err := appcrypto.Decrypt(stored, config.EncryptionKey)
	if err != nil || decrypted != refreshToken {
		t.Fatalf("decrypt stored token: value=%q err=%v", decrypted, err)
	}
	loaded, err := store.LoadRefreshToken(context.Background())
	if err != nil || loaded != refreshToken {
		t.Fatalf("load refresh token: value=%q err=%v", loaded, err)
	}
	if strings.Contains(key, config.Mailbox) {
		t.Fatalf("credential key exposes mailbox address: %q", key)
	}
}

func TestRedisStoreFailsClosedForPlaintextOrWrongKey(t *testing.T) {
	store, backend, _ := testRedisStore(t)
	backend.values[store.refreshTokenKey()] = "legacy-plaintext-token"
	if _, err := store.LoadRefreshToken(context.Background()); err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Fatalf("expected plaintext rejection, got %v", err)
	}

	encrypted, err := appcrypto.Encrypt("token", strings.Repeat("x", 32))
	if err != nil {
		t.Fatalf("prepare wrong-key ciphertext: %v", err)
	}
	backend.values[store.refreshTokenKey()] = encrypted
	if _, err := store.LoadRefreshToken(context.Background()); err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("expected wrong-key rejection, got %v", err)
	}
}

func TestRedisStoreReplacesOnlyTheStillCurrentRefreshToken(t *testing.T) {
	store, _, _ := testRedisStore(t)
	ctx := context.Background()
	if err := store.SaveRefreshToken(ctx, "current-refresh-token"); err != nil {
		t.Fatalf("save refresh token: %v", err)
	}
	replaced, err := store.ReplaceRefreshToken(ctx, "wrong-refresh-token", "replacement-refresh-token")
	if err != nil || replaced {
		t.Fatalf("replace mismatched token: replaced=%t err=%v", replaced, err)
	}
	value, err := store.LoadRefreshToken(ctx)
	if err != nil || value != "current-refresh-token" {
		t.Fatalf("mismatch changed token: value=%q err=%v", value, err)
	}
	replaced, err = store.ReplaceRefreshToken(ctx, "current-refresh-token", "replacement-refresh-token")
	if err != nil || !replaced {
		t.Fatalf("replace current token: replaced=%t err=%v", replaced, err)
	}
	value, err = store.LoadRefreshToken(ctx)
	if err != nil || value != "replacement-refresh-token" {
		t.Fatalf("replacement token: value=%q err=%v", value, err)
	}
	if err := store.DeleteRefreshToken(ctx); err != nil {
		t.Fatalf("delete refresh token: %v", err)
	}
	replaced, err = store.ReplaceRefreshToken(ctx, "replacement-refresh-token", "must-not-reappear")
	if err != nil || replaced {
		t.Fatalf("replace deleted token: replaced=%t err=%v", replaced, err)
	}
	if _, err := store.LoadRefreshToken(ctx); !errors.Is(err, ErrRefreshTokenNotFound) {
		t.Fatalf("deleted token was recreated: %v", err)
	}
}

func TestRedisStoreDeleteRefreshTokenIsMailboxLocal(t *testing.T) {
	store, backend, config := testRedisStore(t)
	otherConfig := *config
	otherConfig.Mailbox = "other@gmail.com"
	otherStore, err := newRedisStore(&otherConfig, backend)
	if err != nil {
		t.Fatalf("build other mailbox store: %v", err)
	}
	backend.values[store.refreshTokenKey()] = "value"
	backend.values[otherStore.refreshTokenKey()] = "keep"
	if err := store.DeleteRefreshToken(context.Background()); err != nil {
		t.Fatalf("delete refresh token: %v", err)
	}
	if _, exists := backend.values[store.refreshTokenKey()]; exists {
		t.Fatal("refresh token was not deleted")
	}
	if backend.values[otherStore.refreshTokenKey()] != "keep" {
		t.Fatal("delete affected another mailbox's refresh token")
	}
}
