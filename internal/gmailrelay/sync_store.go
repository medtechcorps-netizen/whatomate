package gmailrelay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// SyncStore is the small durable state boundary required by Gmail history
// polling. The message queue itself uses the existing relay queue store.
type SyncStore interface {
	GetHistoryCursor(context.Context) (string, error)
	SetHistoryCursor(context.Context, string) error
	GetLastSuccessfulSync(context.Context) (time.Time, error)
	MarkSuccessfulSync(context.Context, time.Time) error
	TryAcquireSyncLease(context.Context, string, time.Duration) (bool, error)
	RenewSyncLease(context.Context, string, time.Duration) (bool, error)
	ReleaseSyncLease(context.Context, string) error
}

var releaseSyncLeaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

var renewSyncLeaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

var commitSuccessfulSyncScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then
	return -1
end

local current = redis.call("GET", KEYS[2])
local proposed = ARGV[2]
if current then
	if string.len(current) > string.len(proposed) or
		(string.len(current) == string.len(proposed) and current > proposed) then
		return -2
	end
end

redis.call("SET", KEYS[2], proposed)
redis.call("SET", KEYS[3], ARGV[3])
return 1
`)

var (
	ErrSyncLeaseLost           = errors.New("gmail sync lease was lost")
	ErrHistoryCursorRegression = errors.New("gmail history cursor would regress")
)

type syncLeaseReleaser interface {
	CompareAndDelete(context.Context, string, string) (bool, error)
}

type syncLeaseRenewer interface {
	CompareAndExpire(context.Context, string, string, time.Duration) (bool, error)
}

type syncCommitBackend interface {
	CommitSuccessfulSync(context.Context, string, string, string, string, string, string) (int64, error)
}

type fencedSyncStore interface {
	CommitSuccessfulSync(context.Context, string, string, time.Time) error
}

func (b *goRedisBackend) CompareAndDelete(ctx context.Context, key, value string) (bool, error) {
	result, err := releaseSyncLeaseScript.Run(ctx, b.client, []string{key}, value).Int()
	return result == 1, err
}

func (b *goRedisBackend) CompareAndExpire(
	ctx context.Context,
	key, value string,
	ttl time.Duration,
) (bool, error) {
	result, err := renewSyncLeaseScript.Run(
		ctx,
		b.client,
		[]string{key},
		value,
		ttl.Milliseconds(),
	).Int()
	return result == 1, err
}

func (b *goRedisBackend) CommitSuccessfulSync(
	ctx context.Context,
	leaseKey, cursorKey, lastSuccessKey, leaseToken, cursor, lastSuccess string,
) (int64, error) {
	return commitSuccessfulSyncScript.Run(
		ctx,
		b.client,
		[]string{leaseKey, cursorKey, lastSuccessKey},
		leaseToken,
		cursor,
		lastSuccess,
	).Int64()
}

func (s *RedisStore) GetHistoryCursor(ctx context.Context) (string, error) {
	if s == nil || s.backend == nil {
		return "", errors.New("redis is not configured")
	}
	value, err := s.backend.Get(ctx, s.syncKey("cursor"))
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load Gmail history cursor: %w", err)
	}
	return strings.TrimSpace(value), nil
}

func (s *RedisStore) SetHistoryCursor(ctx context.Context, cursor string) error {
	if s == nil || s.backend == nil || strings.TrimSpace(cursor) == "" {
		return errors.New("gmail history cursor is invalid")
	}
	if err := s.backend.Set(ctx, s.syncKey("cursor"), strings.TrimSpace(cursor), 0); err != nil {
		return fmt.Errorf("store Gmail history cursor: %w", err)
	}
	return nil
}

func (s *RedisStore) GetLastSuccessfulSync(ctx context.Context) (time.Time, error) {
	if s == nil || s.backend == nil {
		return time.Time{}, errors.New("redis is not configured")
	}
	value, err := s.backend.Get(ctx, s.syncKey("last_success"))
	if errors.Is(err, redis.Nil) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("load Gmail sync time: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, errors.New("stored Gmail sync time is invalid")
	}
	return parsed.UTC(), nil
}

func (s *RedisStore) MarkSuccessfulSync(ctx context.Context, at time.Time) error {
	if s == nil || s.backend == nil || at.IsZero() {
		return errors.New("gmail sync time is invalid")
	}
	if err := s.backend.Set(ctx, s.syncKey("last_success"), at.UTC().Format(time.RFC3339Nano), 0); err != nil {
		return fmt.Errorf("store Gmail sync time: %w", err)
	}
	return nil
}

// TryAcquireSyncLease uses an expiring single-writer lease. Cursor commits and
// lease release both verify the opaque holder token atomically.
func (s *RedisStore) TryAcquireSyncLease(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	if s == nil || s.backend == nil || strings.TrimSpace(token) == "" || ttl <= 0 {
		return false, errors.New("gmail sync lease is invalid")
	}
	acquired, err := s.backend.SetNX(ctx, s.syncKey("lease"), token, ttl)
	if err != nil {
		return false, fmt.Errorf("acquire Gmail sync lease: %w", err)
	}
	return acquired, nil
}

// RenewSyncLease extends only the lease held by token. A delayed holder can
// never extend a newer worker's lease after its own token has expired.
func (s *RedisStore) RenewSyncLease(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	if s == nil || s.backend == nil || strings.TrimSpace(token) == "" || ttl <= 0 {
		return false, errors.New("gmail sync lease is invalid")
	}
	renewer, ok := s.backend.(syncLeaseRenewer)
	if !ok {
		return false, errors.New("redis backend does not support Gmail sync lease renewal")
	}
	renewed, err := renewer.CompareAndExpire(ctx, s.syncKey("lease"), token, ttl)
	if err != nil {
		return false, fmt.Errorf("renew Gmail sync lease: %w", err)
	}
	return renewed, nil
}

// CommitSuccessfulSync fences a poller's final state transition with its lease
// token and rejects cursor regression. If a slow worker outlives its lease, it
// cannot overwrite progress committed by a newer worker.
func (s *RedisStore) CommitSuccessfulSync(ctx context.Context, leaseToken, cursor string, at time.Time) error {
	leaseToken = strings.TrimSpace(leaseToken)
	cursor = strings.TrimSpace(cursor)
	if s == nil || s.backend == nil || leaseToken == "" || !validHistoryCursor(cursor) || at.IsZero() {
		return errors.New("gmail sync commit is invalid")
	}
	committer, ok := s.backend.(syncCommitBackend)
	if !ok {
		return errors.New("redis backend does not support fenced Gmail sync commits")
	}
	result, err := committer.CommitSuccessfulSync(
		ctx,
		s.syncKey("lease"),
		s.syncKey("cursor"),
		s.syncKey("last_success"),
		leaseToken,
		cursor,
		at.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("commit Gmail sync state: %w", err)
	}
	switch result {
	case 1:
		return nil
	case -1:
		return ErrSyncLeaseLost
	case -2:
		return ErrHistoryCursorRegression
	default:
		return errors.New("commit Gmail sync state returned an invalid result")
	}
}

func (s *RedisStore) ReleaseSyncLease(ctx context.Context, token string) error {
	if s == nil || s.backend == nil || strings.TrimSpace(token) == "" {
		return errors.New("gmail sync lease is invalid")
	}
	releaser, ok := s.backend.(syncLeaseReleaser)
	if !ok {
		// Test doubles from the OAuth-only store suite predate sync leasing. A
		// production goRedisBackend always implements atomic compare-and-delete.
		return nil
	}
	if _, err := releaser.CompareAndDelete(ctx, s.syncKey("lease"), token); err != nil {
		return fmt.Errorf("release Gmail sync lease: %w", err)
	}
	return nil
}

func (s *RedisStore) syncKey(name string) string {
	return s.prefix + "sync:" + s.mailboxKey + ":" + name
}

func validHistoryCursor(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
