package handlers

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/queue"
	appwebsocket "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopedAppCreationDoesNotCopyOrRaceRootRealtimeState(t *testing.T) {
	const iterations = 256
	for range iterations {
		root := &App{
			CampaignSubCancel: func() {},
			RealtimeSubCancel: func() {},
		}
		organizationID := uuid.New()
		start := make(chan struct{})
		var wg sync.WaitGroup
		var scoped *App
		var sourceID string

		wg.Add(3)
		go func() {
			defer wg.Done()
			<-start
			scoped = root.scopedApp(nil, organizationID)
		}()
		go func() {
			defer wg.Done()
			<-start
			sourceID = root.realtimeSourceID()
		}()
		go func() {
			defer wg.Done()
			<-start
			root.StopRealtimeSubscriber()
		}()
		close(start)
		wg.Wait()

		require.NotNil(t, scoped)
		require.Nil(t, scoped.CampaignSubCancel)
		require.Nil(t, scoped.RealtimeSubCancel)
		require.Empty(t, scoped.RealtimeSourceID)
		require.NotEmpty(t, sourceID)
		assert.Equal(t, sourceID, scoped.realtimeSourceID())
	}
}

func TestPublishRealtimeEventDeliversLocallyAndScopesTenant(t *testing.T) {
	log := testutil.NopLogger()
	hub := appwebsocket.NewHub(log)
	go hub.Run()
	orgID := uuid.New()
	otherOrgID := uuid.New()
	client := appwebsocket.NewClient(hub, nil, uuid.New(), orgID)
	other := appwebsocket.NewClient(hub, nil, uuid.New(), otherOrgID)
	hub.Register(client)
	hub.Register(other)
	testutil.AssertEventually(t, func() bool { return hub.GetClientCount() == 2 }, 2*time.Second, "clients register")

	app := &App{Log: log, WSHub: hub, RealtimeSourceID: "api-local"}
	want := queue.RealtimeEvent{
		EventID:        uuid.New(),
		OrganizationID: orgID,
		Kind:           queue.RealtimeEventConversationChanged,
		OccurredAt:     time.Now().UTC(),
	}
	app.publishRealtimeEvent(want, nil)

	select {
	case data := <-client.SendChan():
		var envelope struct {
			Type    string              `json:"type"`
			Payload queue.RealtimeEvent `json:"payload"`
		}
		require.NoError(t, json.Unmarshal(data, &envelope))
		assert.Equal(t, appwebsocket.TypeRealtimeSync, envelope.Type)
		assert.Equal(t, want.EventID, envelope.Payload.EventID)
		assert.Equal(t, orgID, envelope.Payload.OrganizationID)
		assert.Equal(t, "api-local", envelope.Payload.SourceID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for local realtime event")
	}
	select {
	case data := <-other.SendChan():
		t.Fatalf("other tenant received event: %s", data)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStartRealtimeSubscriberFailsWhenRequiredDependenciesAreMissing(t *testing.T) {
	log := testutil.NopLogger()
	require.Error(t, (&App{Log: log}).StartRealtimeSubscriber())
	hub := appwebsocket.NewHub(log)
	require.Error(t, (&App{Log: log, WSHub: hub}).StartRealtimeSubscriber())
}

func TestRealtimeFanoutAcrossTwoReplicasWithoutOriginEcho(t *testing.T) {
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	options, err := redis.ParseURL(redisURL)
	require.NoError(t, err)
	redisA := redis.NewClient(options)
	redisB := redis.NewClient(options)
	t.Cleanup(func() {
		_ = redisA.Close()
		_ = redisB.Close()
	})
	require.NoError(t, redisA.Ping(context.Background()).Err())
	require.NoError(t, redisB.Ping(context.Background()).Err())
	require.NotSame(t, redisA, redisB)

	log := testutil.NopLogger()
	hubA := appwebsocket.NewHub(log)
	hubB := appwebsocket.NewHub(log)
	go hubA.Run()
	go hubB.Run()
	organizationA := uuid.New()
	organizationB := uuid.New()
	originClient := appwebsocket.NewClient(hubA, nil, uuid.New(), organizationA)
	remoteClient := appwebsocket.NewClient(hubB, nil, uuid.New(), organizationA)
	otherTenantClient := appwebsocket.NewClient(hubB, nil, uuid.New(), organizationB)
	hubA.Register(originClient)
	hubB.Register(remoteClient)
	hubB.Register(otherTenantClient)
	testutil.AssertEventually(t, func() bool {
		return hubA.GetClientCount() == 1 && hubB.GetClientCount() == 2
	}, 2*time.Second, "clients registered on both replicas")

	appA := &App{Redis: redisA, Log: log, WSHub: hubA, RealtimeSourceID: "replica-a"}
	appB := &App{Redis: redisB, Log: log, WSHub: hubB, RealtimeSourceID: "replica-b"}
	require.NoError(t, appA.StartRealtimeSubscriber())
	require.NoError(t, appB.StartRealtimeSubscriber())
	t.Cleanup(appA.StopRealtimeSubscriber)
	t.Cleanup(appB.StopRealtimeSubscriber)

	want := queue.RealtimeEvent{
		EventID:        uuid.New(),
		OrganizationID: organizationA,
		Kind:           queue.RealtimeEventConversationChanged,
		OccurredAt:     time.Now().UTC(),
	}
	appA.publishRealtimeEvent(want, nil)

	origin := receiveRealtimeEnvelope(t, originClient.SendChan())
	remote := receiveRealtimeEnvelope(t, remoteClient.SendChan())
	assert.Equal(t, appwebsocket.TypeRealtimeSync, origin.Type)
	assert.Equal(t, want.EventID, origin.Payload.EventID)
	assert.Equal(t, "replica-a", origin.Payload.SourceID)
	assert.Equal(t, appwebsocket.TypeRealtimeSync, remote.Type)
	assert.Equal(t, want.EventID, remote.Payload.EventID)
	assert.Equal(t, organizationA, remote.Payload.OrganizationID)

	select {
	case duplicate := <-originClient.SendChan():
		t.Fatalf("origin subscriber echoed its own event: %s", duplicate)
	case <-time.After(150 * time.Millisecond):
	}
	select {
	case leaked := <-otherTenantClient.SendChan():
		t.Fatalf("other tenant received cross-replica event: %s", leaked)
	case <-time.After(150 * time.Millisecond):
	}

	// Force replica B's confirmed Pub/Sub transport closed while its owning
	// context remains active. The subscriber must mark itself unavailable,
	// resubscribe, and resume cross-replica delivery without restarting the API.
	subscriberB := appB.realtimeSubscriber.Load()
	require.NotNil(t, subscriberB)
	reconnectsBefore := subscriberB.RealtimeReconnectCount()
	require.NoError(t, subscriberB.Close())
	testutil.AssertEventually(t, func() bool {
		return subscriberB.RealtimeReconnectCount() > reconnectsBefore && subscriberB.RealtimeLive()
	}, 5*time.Second, "replica B realtime subscriber recovered")
	recoveredEvent := queue.RealtimeEvent{
		EventID:        uuid.New(),
		OrganizationID: organizationA,
		Kind:           queue.RealtimeEventConversationChanged,
		OccurredAt:     time.Now().UTC(),
	}
	appA.publishRealtimeEvent(recoveredEvent, nil)
	recoveredRemote := receiveRealtimeEnvelope(t, remoteClient.SendChan())
	assert.Equal(t, recoveredEvent.EventID, recoveredRemote.Payload.EventID)
	assert.Equal(t, organizationA, recoveredRemote.Payload.OrganizationID)
	select {
	case leaked := <-otherTenantClient.SendChan():
		t.Fatalf("other tenant received recovered cross-replica event: %s", leaked)
	case <-time.After(150 * time.Millisecond):
	}
	appA.StopRealtimeSubscriber()
	assert.False(t, appA.realtimeSubscriberLive.Load())
}

type realtimeTestEnvelope struct {
	Type    string              `json:"type"`
	Payload queue.RealtimeEvent `json:"payload"`
}

func receiveRealtimeEnvelope(t *testing.T, messages <-chan []byte) realtimeTestEnvelope {
	t.Helper()
	select {
	case data := <-messages:
		var envelope realtimeTestEnvelope
		require.NoError(t, json.Unmarshal(data, &envelope))
		return envelope
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for realtime envelope")
		return realtimeTestEnvelope{}
	}
}
