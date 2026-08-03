package metarelay

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestRedisStoreStaleProcessingClaimBecomesAmbiguous(t *testing.T) {
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL not set")
	}

	store, err := NewRedisStore(&Config{
		RedisURL:          redisURL,
		RedisPrefix:       "test:meta-relay:stale-outbound:" + time.Now().UTC().Format("20060102150405.000000000") + ":",
		InboundRetention:  time.Hour,
		OutboundRetention: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewRedisStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Ping(ctx); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}

	const (
		key    = "mailbox@example.test\ndelivery-42"
		digest = "request-digest-42"
	)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = store.client.Del(cleanupCtx, store.outboundKey(key)).Err()
	})

	startedAt := time.Date(2026, time.August, 3, 1, 0, 0, 0, time.UTC)
	claim, err := store.ClaimOutbound(ctx, key, digest, startedAt)
	if err != nil || claim.State != OutboundClaimAcquired {
		t.Fatalf("initial ClaimOutbound() = %#v, %v; want acquired", claim, err)
	}

	claim, err = store.ClaimOutbound(ctx, key, digest, startedAt.Add(outboundProcessingStaleAfter-time.Nanosecond))
	if err != nil || claim.State != OutboundClaimInFlight {
		t.Fatalf("fresh replay ClaimOutbound() = %#v, %v; want in-flight", claim, err)
	}

	claim, err = store.ClaimOutbound(ctx, key, digest, startedAt.Add(outboundProcessingStaleAfter))
	if err != nil || claim.State != OutboundClaimAmbiguous {
		t.Fatalf("stale replay ClaimOutbound() = %#v, %v; want ambiguous", claim, err)
	}

	claim, err = store.ClaimOutbound(ctx, key, digest, startedAt.Add(24*time.Hour))
	if err != nil || claim.State != OutboundClaimAmbiguous {
		t.Fatalf("later replay ClaimOutbound() = %#v, %v; want persistently ambiguous", claim, err)
	}
	if claim.State == OutboundClaimAcquired {
		t.Fatal("a stale claim was reacquired and could repeat the provider side effect")
	}

	result := []byte(`{"id":"gmail-message-42"}`)
	if err := store.ResolveOutboundAmbiguous(ctx, key, digest, 200, result); err != nil {
		t.Fatalf("ResolveOutboundAmbiguous() error = %v", err)
	}
	claim, err = store.ClaimOutbound(ctx, key, digest, startedAt.Add(24*time.Hour))
	if err != nil || claim.State != OutboundClaimCompleted || claim.Status != 200 || string(claim.Result) != string(result) {
		t.Fatalf("reconciled ClaimOutbound() = %#v, %v; want cached completion", claim, err)
	}
}
