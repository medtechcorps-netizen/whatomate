package queue_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublisherPublishRealtimeValidatesTenantAndSchema(t *testing.T) {
	publisher := queue.NewPublisher(nil, testutil.NopLogger())
	require.ErrorContains(t, publisher.PublishRealtime(context.Background(), &queue.RealtimeEvent{
		Kind: queue.RealtimeEventConversationChanged,
	}), "organization")
	require.ErrorContains(t, publisher.PublishRealtime(context.Background(), &queue.RealtimeEvent{
		OrganizationID: uuid.New(),
		Kind:           queue.RealtimeEventKind("raw_provider_payload"),
	}), "kind")

	messageID := uuid.New()
	event := queue.RealtimeEvent{
		EventID:        uuid.New(),
		OrganizationID: uuid.New(),
		Kind:           queue.RealtimeEventMessageCreated,
		MessageID:      &messageID,
		OccurredAt:     time.Now().UTC(),
	}
	payload, err := json.Marshal(event)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(payload, &decoded))
	assert.Contains(t, decoded, "organization_id")
	assert.Contains(t, decoded, "message_id")
	assert.NotContains(t, decoded, "content")
	assert.NotContains(t, decoded, "payload")
	assert.NotContains(t, decoded, "provider_payload")
}

func TestRealtimeSubscriberDiscardsMalformedAndFansOutTenantHints(t *testing.T) {
	rdb := testutil.SetupTestRedis(t)
	if rdb == nil {
		t.Skip("TEST_REDIS_URL not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	subscriber := queue.NewSubscriber(rdb, testutil.NopLogger())
	t.Cleanup(func() { _ = subscriber.Close() })
	received := make(chan queue.RealtimeEvent, 2)
	require.NoError(t, subscriber.SubscribeRealtime(ctx, func(event *queue.RealtimeEvent) {
		received <- *event
	}))

	malformed, err := json.Marshal(map[string]any{
		"organization_id": uuid.New(),
		"kind":            queue.RealtimeEventMessageCreated,
		"occurred_at":     time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, rdb.Publish(ctx, queue.RealtimeChannel, malformed).Err())

	want := &queue.RealtimeEvent{
		EventID:        uuid.New(),
		OrganizationID: uuid.New(),
		SourceID:       "worker",
		Kind:           queue.RealtimeEventMessageStatusChanged,
		Status:         "sent",
		OccurredAt:     time.Now().UTC(),
	}
	require.NoError(t, queue.NewPublisher(rdb, testutil.NopLogger()).PublishRealtime(ctx, want))

	select {
	case got := <-received:
		assert.Equal(t, want.EventID, got.EventID)
		assert.Equal(t, want.OrganizationID, got.OrganizationID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for realtime event")
	}
	select {
	case unexpected := <-received:
		t.Fatalf("malformed event reached subscriber: %#v", unexpected)
	case <-time.After(100 * time.Millisecond):
	}
}
