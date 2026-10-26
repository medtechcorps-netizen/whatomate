package handlers

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sessionIssueStoreHook struct {
	err    error
	mu     sync.Mutex
	writes int
}

func (h *sessionIssueStoreHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *sessionIssueStoreHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *sessionIssueStoreHook) ProcessHook(_ redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.writes++
		return h.err
	}
}

func TestRefreshTokenIssuerFailsClosedWithoutPersistence(t *testing.T) {
	user := &models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New()}
	for _, tc := range []struct {
		name    string
		cause   error
		missing bool
	}{
		{"missing client", nil, true},
		{"write rejected", errors.New("synthetic SET failure"), false},
		{"write acknowledgement lost", context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{Config: &config.Config{JWT: config.JWTConfig{Secret: testutil.TestJWTSecret, RefreshExpiryDays: 1}}}
			if !tc.missing {
				app.Redis = redis.NewClient(&redis.Options{Addr: "unused.invalid:6379", MaxRetries: -1})
				app.Redis.AddHook(&sessionIssueStoreHook{err: tc.cause})
				t.Cleanup(func() { _ = app.Redis.Close() })
			}
			token, err := app.generateRefreshToken(user)
			assert.Empty(t, token)
			require.ErrorIs(t, err, errRefreshTokenStoreUnavailable)
			if tc.cause != nil {
				assert.ErrorIs(t, err, tc.cause)
			}
		})
	}
}

func TestRefreshTokenIssuerConcurrentFailuresReturnNoCredential(t *testing.T) {
	hook := &sessionIssueStoreHook{err: context.DeadlineExceeded}
	client := redis.NewClient(&redis.Options{Addr: "unused.invalid:6379", MaxRetries: -1})
	client.AddHook(hook)
	t.Cleanup(func() { _ = client.Close() })
	app := &App{Redis: client, Config: &config.Config{JWT: config.JWTConfig{Secret: testutil.TestJWTSecret, RefreshExpiryDays: 1}}}
	user := &models.User{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: uuid.New()}
	type result struct {
		token string
		err   error
	}
	results := make(chan result, 12)
	for i := 0; i < 12; i++ {
		go func() { token, err := app.generateRefreshToken(user); results <- result{token, err} }()
	}
	for i := 0; i < 12; i++ {
		got := <-results
		assert.Empty(t, got.token)
		assert.ErrorIs(t, got.err, errRefreshTokenStoreUnavailable)
	}
	assert.Equal(t, 12, hook.writes)
}
