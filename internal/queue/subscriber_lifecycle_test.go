package queue

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/logf"
)

type subscriberTestTimeout struct{}

func subscriberTestLogger() logf.Logger       { return logf.New(logf.Opts{Level: logf.ErrorLevel}) }
func (subscriberTestTimeout) Error() string   { return "synthetic read timeout" }
func (subscriberTestTimeout) Timeout() bool   { return true }
func (subscriberTestTimeout) Temporary() bool { return true }

type subscriberTestConnection struct {
	events    chan interface{}
	closed    chan struct{}
	closeGate <-chan struct{}
	closeErr  error
	closes    atomic.Int32
}

func subscriberTestConn(channels ...string) *subscriberTestConnection {
	p := &subscriberTestConnection{events: make(chan interface{}, 16), closed: make(chan struct{})}
	for _, channel := range channels {
		p.events <- &redis.Subscription{Kind: "subscribe", Channel: channel}
	}
	return p
}

func (p *subscriberTestConnection) ReceiveTimeout(_ context.Context, duration time.Duration) (interface{}, error) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-p.closed:
		return nil, errors.New("synthetic connection closed")
	case value := <-p.events:
		if err, ok := value.(error); ok {
			return nil, err
		}
		return value, nil
	case <-timer.C:
		return nil, subscriberTestTimeout{}
	}
}
func (p *subscriberTestConnection) Ping(context.Context, ...string) error { return nil }
func (p *subscriberTestConnection) Close() error {
	if p.closes.Add(1) == 1 {
		close(p.closed)
	}
	if p.closeGate != nil {
		<-p.closeGate
	}
	return p.closeErr
}

func subscriberTestOwner(p *subscriberTestConnection) *Subscriber {
	s := NewSubscriber(nil, subscriberTestLogger())
	s.open = func(context.Context, ...string) subscriberPubSub { return p }
	return s
}

func subscriberTestJoined(t *testing.T, s *Subscriber) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.Stop(ctx)
}

func subscriberTestStillRunning(t *testing.T, s *Subscriber) {
	t.Helper()
	select {
	case <-s.Done():
		t.Fatal("unfinished subscriber reported completion")
	default:
	}
}

func TestSubscriberOwnedDrainWaitsForAcceptedCampaignHandler(t *testing.T) {
	p := subscriberTestConn(CampaignStatsChannel)
	s := subscriberTestOwner(p)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }); _ = subscriberTestJoined(t, s) })
	var calls atomic.Int32
	require.NoError(t, s.SubscribeCampaignStats(context.Background(), func(*CampaignStatsUpdate) {
		calls.Add(1)
		close(entered)
		<-release
	}))
	subscriberTestStillRunning(t, s)
	p.events <- &redis.Message{Channel: CampaignStatsChannel, Payload: `{"campaign_id":"synthetic"}`}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.Stop(ctx), context.DeadlineExceeded)
	subscriberTestStillRunning(t, s)
	once.Do(func() { close(release) })
	require.NoError(t, subscriberTestJoined(t, s))
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, int32(1), p.closes.Load())
}

func TestSubscriberOwnedDrainWaitsForCloseAndPreservesFailure(t *testing.T) {
	for _, closeErr := range []error{nil, errors.New("synthetic close failure")} {
		name := "success"
		if closeErr != nil {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			p := subscriberTestConn(CampaignStatsChannel)
			release := make(chan struct{})
			p.closeGate = release
			p.closeErr = closeErr
			s := subscriberTestOwner(p)
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }); _ = subscriberTestJoined(t, s) })
			require.NoError(t, s.SubscribeCampaignStats(context.Background(), func(*CampaignStatsUpdate) {}))
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			require.ErrorIs(t, s.Stop(ctx), context.DeadlineExceeded)
			subscriberTestStillRunning(t, s)
			once.Do(func() { close(release) })
			err := subscriberTestJoined(t, s)
			if closeErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, closeErr)
			}
			require.Equal(t, int32(1), p.closes.Load())
		})
	}
}

func TestSubscriberOwnedDrainRejectsLateStartupConnection(t *testing.T) {
	s := NewSubscriber(nil, subscriberTestLogger())
	p := subscriberTestConn(RealtimeChannel, RealtimeUserChannel)
	opening, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.open = func(context.Context, ...string) subscriberPubSub { close(opening); <-release; return p }
	started := make(chan error, 1)
	go func() { started <- s.SubscribeRealtime(context.Background(), func(*RealtimeEvent) {}) }()
	t.Cleanup(func() { once.Do(func() { close(release) }); _ = subscriberTestJoined(t, s) })
	select {
	case <-opening:
	case <-time.After(time.Second):
		t.Fatal("startup did not begin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.Stop(ctx), context.DeadlineExceeded)
	subscriberTestStillRunning(t, s)
	require.False(t, s.RealtimeLive())
	once.Do(func() { close(release) })
	select {
	case err := <-started:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("startup did not return")
	}
	require.NoError(t, subscriberTestJoined(t, s))
	require.False(t, s.RealtimeLive())
	require.Equal(t, int32(1), p.closes.Load())
}

func TestSubscriberOwnedDrainConcurrentStopAndNoRestart(t *testing.T) {
	p := subscriberTestConn(RealtimeChannel, RealtimeUserChannel)
	s := subscriberTestOwner(p)
	require.NoError(t, s.SubscribeRealtime(context.Background(), func(*RealtimeEvent) {}))
	require.True(t, s.RealtimeLive())
	require.Error(t, s.SubscribeRealtime(context.Background(), func(*RealtimeEvent) {}))
	require.True(t, s.RealtimeLive(), "duplicate admission must not stop the existing subscription")
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			results <- s.Stop(ctx)
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.False(t, s.RealtimeLive())
	require.Equal(t, int32(1), p.closes.Load())
	require.Error(t, s.SubscribeCampaignStats(context.Background(), func(*CampaignStatsUpdate) {}))
}

func TestSubscriberOwnedDrainStopsConfirmationAndFailedStartup(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "cancel"
		if fail {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			p := subscriberTestConn()
			s := subscriberTestOwner(p)
			started := make(chan error, 1)
			if fail {
				p.events <- errors.New("synthetic confirmation failure")
			}
			go func() { started <- s.SubscribeRealtime(context.Background(), func(*RealtimeEvent) {}) }()
			if fail {
				select {
				case err := <-started:
					require.Error(t, err)
				case <-time.After(time.Second):
					t.Fatal("failed startup stuck")
				}
			} else {
				require.NoError(t, subscriberTestJoined(t, s))
				select {
				case err := <-started:
					require.Error(t, err)
				case <-time.After(time.Second):
					t.Fatal("cancelled startup stuck")
				}
			}
			require.NoError(t, subscriberTestJoined(t, s))
			require.False(t, s.RealtimeLive())
			require.LessOrEqual(t, p.closes.Load(), int32(1))
		})
	}
}

func TestSubscriberOwnedDrainCannotReviveDuringReconnect(t *testing.T) {
	first := subscriberTestConn(RealtimeChannel, RealtimeUserChannel)
	second := subscriberTestConn(RealtimeChannel, RealtimeUserChannel)
	s := NewSubscriber(nil, subscriberTestLogger())
	opening, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var opens atomic.Int32
	s.open = func(context.Context, ...string) subscriberPubSub {
		if opens.Add(1) == 1 {
			return first
		}
		close(opening)
		<-release
		return second
	}
	t.Cleanup(func() { once.Do(func() { close(release) }); _ = subscriberTestJoined(t, s) })
	require.NoError(t, s.SubscribeRealtime(context.Background(), func(*RealtimeEvent) {}))
	first.events <- errors.New("synthetic disconnect")
	select {
	case <-opening:
	case <-time.After(3 * time.Second):
		t.Fatal("reconnect did not begin")
	}
	require.False(t, s.RealtimeLive())
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.Stop(ctx), context.DeadlineExceeded)
	subscriberTestStillRunning(t, s)
	once.Do(func() { close(release) })
	require.NoError(t, subscriberTestJoined(t, s))
	require.False(t, s.RealtimeLive())
	require.Zero(t, s.RealtimeReconnectCount())
	require.Equal(t, int32(1), first.closes.Load())
	require.Equal(t, int32(1), second.closes.Load())
}

func TestSubscriberOwnedDrainDetectsUnansweredHealthCheck(t *testing.T) {
	first := subscriberTestConn(RealtimeChannel, RealtimeUserChannel)
	second := subscriberTestConn(RealtimeChannel, RealtimeUserChannel)
	s := NewSubscriber(nil, subscriberTestLogger())
	var opens atomic.Int32
	s.open = func(context.Context, ...string) subscriberPubSub {
		if opens.Add(1) == 1 {
			return first
		}
		return second
	}
	t.Cleanup(func() { _ = subscriberTestJoined(t, s) })
	require.NoError(t, s.SubscribeRealtime(context.Background(), func(*RealtimeEvent) {}))
	require.Eventually(t, func() bool { return s.RealtimeReconnectCount() == 1 }, 10*time.Second, 10*time.Millisecond)
	require.True(t, s.RealtimeLive())
	require.Equal(t, int32(1), first.closes.Load())
	require.NoError(t, subscriberTestJoined(t, s))
	require.Equal(t, int32(1), second.closes.Load())
}
