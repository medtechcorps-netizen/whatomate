package metarelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	acceptInboundScript = redis.NewScript(`
if redis.call("EXISTS", KEYS[1]) == 1 then
	return 0
end
for i = 3, #KEYS do
	local arg_index = 3 + ((i - 3) * 2)
	redis.call("SET", KEYS[i], ARGV[arg_index + 1], "EX", ARGV[2])
	redis.call("ZADD", KEYS[2], "NX", ARGV[1], ARGV[arg_index])
end
redis.call("SET", KEYS[1], "1", "EX", ARGV[2])
return 1
`)

	claimInboundScript = redis.NewScript(`
local ids = redis.call("ZRANGEBYSCORE", KEYS[1], "-inf", ARGV[1], "LIMIT", 0, ARGV[3])
for _, id in ipairs(ids) do
	redis.call("ZREM", KEYS[1], id)
	redis.call("ZADD", KEYS[2], ARGV[2], id)
end
return ids
`)

	requeueExpiredScript = redis.NewScript(`
local ids = redis.call("ZRANGEBYSCORE", KEYS[1], "-inf", ARGV[1], "LIMIT", 0, ARGV[2])
for _, id in ipairs(ids) do
	redis.call("ZREM", KEYS[1], id)
	redis.call("ZADD", KEYS[2], ARGV[1], id)
end
return #ids
`)

	completeInboundScript = redis.NewScript(`
redis.call("ZREM", KEYS[1], ARGV[1])
redis.call("ZREM", KEYS[2], ARGV[1])
redis.call("HDEL", KEYS[3], ARGV[1])
redis.call("DEL", KEYS[4])
redis.call("SET", KEYS[5], "1", "EX", ARGV[2])
return 1
`)

	retryInboundScript = redis.NewScript(`
if redis.call("EXISTS", KEYS[4]) == 0 then
	redis.call("ZREM", KEYS[1], ARGV[1])
	redis.call("ZREM", KEYS[2], ARGV[1])
	redis.call("HDEL", KEYS[3], ARGV[1])
	return 0
end
local attempts = redis.call("HINCRBY", KEYS[3], ARGV[1], 1)
if attempts >= tonumber(ARGV[3]) then
	redis.call("ZREM", KEYS[1], ARGV[1])
	redis.call("ZREM", KEYS[2], ARGV[1])
	redis.call("HDEL", KEYS[3], ARGV[1])
	redis.call("DEL", KEYS[4])
	redis.call("SET", KEYS[5], ARGV[5], "EX", ARGV[4])
	return -attempts
end
redis.call("ZREM", KEYS[1], ARGV[1])
redis.call("ZADD", KEYS[2], ARGV[2], ARGV[1])
return attempts
`)

	deadInboundScript = redis.NewScript(`
redis.call("ZREM", KEYS[1], ARGV[1])
redis.call("ZREM", KEYS[2], ARGV[1])
redis.call("HDEL", KEYS[3], ARGV[1])
redis.call("DEL", KEYS[4])
redis.call("SET", KEYS[5], ARGV[2], "EX", ARGV[3])
return 1
`)

	claimOutboundScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if current then
	return current
end
redis.call("SET", KEYS[1], ARGV[1], "EX", ARGV[2])
return ""
`)

	replaceOutboundScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if not current then
	return 0
end
local decoded = cjson.decode(current)
if decoded["digest"] ~= ARGV[1] or decoded["state"] ~= "processing" then
	return -1
end
redis.call("SET", KEYS[1], ARGV[2], "EX", ARGV[3])
return 1
`)

	releaseOutboundScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if not current then
	return 1
end
local decoded = cjson.decode(current)
if decoded["digest"] ~= ARGV[1] or decoded["state"] ~= "processing" then
	return 0
end
redis.call("DEL", KEYS[1])
return 1
`)
)

type outboundRecord struct {
	Digest    string          `json:"digest"`
	State     string          `json:"state"`
	StartedAt int64           `json:"started_at,omitempty"`
	Status    int             `json:"status,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

// RedisStore is the production durability boundary. The service deliberately
// has no in-memory implementation or fallback.
type RedisStore struct {
	client            *redis.Client
	prefix            string
	inboundRetention  time.Duration
	outboundRetention time.Duration
}

func NewRedisStore(config *Config) (*RedisStore, error) {
	if config == nil {
		return nil, errors.New("relay config is required")
	}
	options, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		return nil, errors.New("redis configuration is invalid")
	}
	return &RedisStore{
		client:            redis.NewClient(options),
		prefix:            config.RedisPrefix,
		inboundRetention:  config.InboundRetention,
		outboundRetention: config.OutboundRetention,
	}, nil
}

func (s *RedisStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *RedisStore) Ping(ctx context.Context) error {
	if s == nil || s.client == nil {
		return errors.New("redis is not configured")
	}
	return s.client.Ping(ctx).Err()
}

func (s *RedisStore) AcceptInbound(ctx context.Context, acceptanceID string, jobs []InboundJob) (bool, error) {
	keys := []string{
		s.prefix + "inbound:accepted:" + acceptanceID,
		s.prefix + "inbound:due",
	}
	args := []any{time.Now().UTC().UnixMilli(), durationSeconds(s.inboundRetention)}
	for _, job := range jobs {
		if strings.TrimSpace(job.ID) == "" || strings.TrimSpace(job.AccountKey) == "" || len(job.Body) == 0 {
			return false, errors.New("inbound job is incomplete")
		}
		payload, err := json.Marshal(job)
		if err != nil {
			return false, fmt.Errorf("encode inbound job: %w", err)
		}
		keys = append(keys, s.inboundJobKey(job.ID))
		args = append(args, job.ID, payload)
	}
	result, err := acceptInboundScript.Run(ctx, s.client, keys, args...).Int64()
	if err != nil {
		return false, fmt.Errorf("durably accept inbound delivery: %w", err)
	}
	return result == 1, nil
}

func (s *RedisStore) ClaimInbound(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]InboundJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	ids, err := claimInboundScript.Run(
		ctx,
		s.client,
		[]string{s.prefix + "inbound:due", s.prefix + "inbound:processing"},
		now.UTC().UnixMilli(),
		now.Add(lease).UTC().UnixMilli(),
		limit,
	).StringSlice()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("claim inbound jobs: %w", err)
	}

	jobs := make([]InboundJob, 0, len(ids))
	for _, id := range ids {
		raw, getErr := s.client.Get(ctx, s.inboundJobKey(id)).Bytes()
		if errors.Is(getErr, redis.Nil) {
			_ = s.CompleteInbound(ctx, id)
			continue
		}
		if getErr != nil {
			return nil, fmt.Errorf("load inbound job: %w", getErr)
		}
		var job InboundJob
		if decodeErr := json.Unmarshal(raw, &job); decodeErr != nil {
			_ = s.DeadInbound(ctx, id, "invalid_job")
			continue
		}
		attempts, attemptsErr := s.client.HGet(ctx, s.prefix+"inbound:attempts", id).Int()
		if attemptsErr != nil && !errors.Is(attemptsErr, redis.Nil) {
			return nil, fmt.Errorf("load inbound attempts: %w", attemptsErr)
		}
		job.ID = id
		job.Attempts = attempts
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *RedisStore) RequeueExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	result, err := requeueExpiredScript.Run(
		ctx,
		s.client,
		[]string{s.prefix + "inbound:processing", s.prefix + "inbound:due"},
		now.UTC().UnixMilli(),
		limit,
	).Int()
	if err != nil {
		return 0, fmt.Errorf("requeue expired inbound jobs: %w", err)
	}
	return result, nil
}

func (s *RedisStore) CompleteInbound(ctx context.Context, jobID string) error {
	_, err := completeInboundScript.Run(
		ctx,
		s.client,
		[]string{
			s.prefix + "inbound:due",
			s.prefix + "inbound:processing",
			s.prefix + "inbound:attempts",
			s.inboundJobKey(jobID),
			s.prefix + "inbound:completed:" + jobID,
		},
		jobID,
		durationSeconds(s.inboundRetention),
	).Result()
	if err != nil {
		return fmt.Errorf("complete inbound job: %w", err)
	}
	return nil
}

func (s *RedisStore) RetryInbound(
	ctx context.Context,
	jobID string,
	next time.Time,
	maxAttempts int,
	reason string,
) (int, bool, error) {
	reason = safeReason(reason)
	result, err := retryInboundScript.Run(
		ctx,
		s.client,
		[]string{
			s.prefix + "inbound:processing",
			s.prefix + "inbound:due",
			s.prefix + "inbound:attempts",
			s.inboundJobKey(jobID),
			s.prefix + "inbound:dead:" + jobID,
		},
		jobID,
		next.UTC().UnixMilli(),
		maxAttempts,
		durationSeconds(s.inboundRetention),
		reason,
	).Int()
	if err != nil {
		return 0, false, fmt.Errorf("retry inbound job: %w", err)
	}
	if result < 0 {
		return -result, true, nil
	}
	return result, false, nil
}

func (s *RedisStore) DeadInbound(ctx context.Context, jobID, reason string) error {
	_, err := deadInboundScript.Run(
		ctx,
		s.client,
		[]string{
			s.prefix + "inbound:processing",
			s.prefix + "inbound:due",
			s.prefix + "inbound:attempts",
			s.inboundJobKey(jobID),
			s.prefix + "inbound:dead:" + jobID,
		},
		jobID,
		safeReason(reason),
		durationSeconds(s.inboundRetention),
	).Result()
	if err != nil {
		return fmt.Errorf("dead-letter inbound job: %w", err)
	}
	return nil
}

func (s *RedisStore) ClaimOutbound(ctx context.Context, key, digest string, now time.Time) (OutboundClaim, error) {
	redisKey := s.outboundKey(key)
	initial, err := json.Marshal(outboundRecord{
		Digest:    digest,
		State:     "processing",
		StartedAt: now.UTC().UnixMilli(),
	})
	if err != nil {
		return OutboundClaim{}, err
	}
	existing, err := claimOutboundScript.Run(
		ctx,
		s.client,
		[]string{redisKey},
		initial,
		durationSeconds(s.outboundRetention),
	).Text()
	if err != nil && !errors.Is(err, redis.Nil) {
		return OutboundClaim{}, fmt.Errorf("claim outbound delivery: %w", err)
	}
	if existing == "" {
		return OutboundClaim{State: OutboundClaimAcquired}, nil
	}

	var record outboundRecord
	if err := json.Unmarshal([]byte(existing), &record); err != nil {
		return OutboundClaim{}, fmt.Errorf("decode outbound idempotency record: %w", err)
	}
	if record.Digest != digest {
		return OutboundClaim{State: OutboundClaimCollision}, nil
	}
	switch record.State {
	case "completed":
		return OutboundClaim{State: OutboundClaimCompleted, Status: record.Status, Result: record.Result}, nil
	case "rejected":
		return OutboundClaim{State: OutboundClaimRejected, Status: record.Status, Result: record.Result}, nil
	case "ambiguous":
		return OutboundClaim{State: OutboundClaimAmbiguous}, nil
	case "processing":
		return OutboundClaim{State: OutboundClaimInFlight}, nil
	default:
		return OutboundClaim{}, errors.New("outbound idempotency record has an invalid state")
	}
}

func (s *RedisStore) CompleteOutbound(
	ctx context.Context,
	key, digest string,
	status int,
	result []byte,
	rejected bool,
) error {
	state := "completed"
	if rejected {
		state = "rejected"
	}
	record, err := json.Marshal(outboundRecord{
		Digest: digest,
		State:  state,
		Status: status,
		Result: append(json.RawMessage(nil), result...),
	})
	if err != nil {
		return fmt.Errorf("encode outbound result: %w", err)
	}
	updated, err := replaceOutboundScript.Run(
		ctx,
		s.client,
		[]string{s.outboundKey(key)},
		digest,
		record,
		durationSeconds(s.outboundRetention),
	).Int()
	if err != nil {
		return fmt.Errorf("store outbound result: %w", err)
	}
	if updated != 1 {
		return ErrOutboundStoreFailure
	}
	return nil
}

func (s *RedisStore) ReleaseOutbound(ctx context.Context, key, digest string) error {
	released, err := releaseOutboundScript.Run(
		ctx,
		s.client,
		[]string{s.outboundKey(key)},
		digest,
	).Int()
	if err != nil {
		return fmt.Errorf("release outbound delivery: %w", err)
	}
	if released != 1 {
		return ErrOutboundStoreFailure
	}
	return nil
}

func (s *RedisStore) MarkOutboundAmbiguous(ctx context.Context, key, digest string) error {
	record, err := json.Marshal(outboundRecord{
		Digest: digest,
		State:  "ambiguous",
	})
	if err != nil {
		return err
	}
	updated, err := replaceOutboundScript.Run(
		ctx,
		s.client,
		[]string{s.outboundKey(key)},
		digest,
		record,
		durationSeconds(s.outboundRetention),
	).Int()
	if err != nil {
		return fmt.Errorf("mark outbound delivery ambiguous: %w", err)
	}
	if updated != 1 {
		return ErrOutboundStoreFailure
	}
	return nil
}

func (s *RedisStore) inboundJobKey(id string) string {
	return s.prefix + "inbound:job:" + id
}

func (s *RedisStore) outboundKey(key string) string {
	return s.prefix + "outbound:" + digestHex([]byte(key))
}

func durationSeconds(duration time.Duration) int64 {
	return int64(math.Max(1, math.Ceil(duration.Seconds())))
}

func safeReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "unspecified"
	}
	if len(reason) > 96 {
		reason = reason[:96]
	}
	for _, char := range reason {
		if (char < 'a' || char > 'z') &&
			(char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') &&
			!strings.ContainsRune("._:-", char) {
			return "redacted"
		}
	}
	return reason
}
