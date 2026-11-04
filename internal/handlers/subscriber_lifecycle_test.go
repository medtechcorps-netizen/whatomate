package handlers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type appSubscriberTestDrain struct {
	done chan struct{}
	err  error
}

func (s *appSubscriberTestDrain) Done() <-chan struct{} { return s.done }
func (s *appSubscriberTestDrain) Stop(ctx context.Context) error {
	select {
	case <-s.done:
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func appSubscriberTestOwner() (*App, *appSubscriber, *appSubscriberTestDrain) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &appSubscriberTestDrain{done: make(chan struct{})}
	o := &appSubscriber{ctx: ctx, cancel: cancel, subscriber: s, startDone: make(chan struct{}), monitorDone: make(chan struct{})}
	a := &App{realtimeOwned: o, RealtimeSubCancel: cancel}
	return a, o, s
}

func TestAppSubscriberDrainWaitsForStartupAndMonitor(t *testing.T) {
	for _, stage := range []string{"startup", "monitor"} {
		t.Run(stage, func(t *testing.T) {
			a, o, s := appSubscriberTestOwner()
			defer o.cancel()
			close(s.done)
			blocked := o.startDone
			if stage == "startup" {
				close(o.monitorDone)
			} else {
				close(o.startDone)
				blocked = o.monitorDone
			}
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(blocked) }) })
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			require.ErrorIs(t, a.DrainRealtimeSubscriber(ctx), context.DeadlineExceeded)
			require.ErrorIs(t, o.ctx.Err(), context.Canceled)
			once.Do(func() { close(blocked) })
			require.NoError(t, a.DrainRealtimeSubscriber(context.Background()))
		})
	}
}

func TestAppSubscriberDrainOwnsUnexpectedMonitorReport(t *testing.T) {
	a, o, s := appSubscriberTestOwner()
	defer o.cancel()
	close(o.startDone)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	a.realtimeSubscriberLive.Store(true)
	go a.monitorRealtimeSubscriber(o, func() { close(entered); <-release })
	close(s.done)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("monitor did not report unexpected exit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, a.DrainRealtimeSubscriber(ctx), context.DeadlineExceeded)
	require.False(t, a.realtimeSubscriberLive.Load())
	once.Do(func() { close(release) })
	require.NoError(t, a.DrainRealtimeSubscriber(context.Background()))
}

func TestAppSubscriberDrainIntentionalStopDoesNotReportFailure(t *testing.T) {
	a, o, s := appSubscriberTestOwner()
	defer o.cancel()
	close(o.startDone)
	reported := make(chan struct{}, 1)
	go a.monitorRealtimeSubscriber(o, func() { reported <- struct{}{} })
	o.cancel()
	close(s.done)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, a.DrainRealtimeSubscriber(ctx))
	select {
	case <-reported:
		t.Fatal("intentional stop reported failure")
	default:
	}
}

func TestAppSubscriberDrainPreservesCleanupErrorAndRootOwnership(t *testing.T) {
	a, o, s := appSubscriberTestOwner()
	defer o.cancel()
	s.err = errors.New("synthetic cleanup failure")
	close(s.done)
	close(o.startDone)
	close(o.monitorDone)
	scoped := a.scopedApp(nil, uuid.New())
	require.Nil(t, scoped.realtimeOwned)
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- scoped.DrainRealtimeSubscriber(context.Background()) }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.ErrorIs(t, err, s.err)
	}
	require.Nil(t, a.RealtimeSubCancel)
	require.True(t, a.realtimeStopped)
	_, _, err := scoped.beginSubscriber(true)
	require.Error(t, err)
}

func TestAppSubscriberDrainBeforeStartFreezesAdmission(t *testing.T) {
	for _, realtime := range []bool{false, true} {
		a := &App{}
		require.NoError(t, a.drainSubscriber(context.Background(), realtime))
		_, _, err := a.beginSubscriber(realtime)
		require.Error(t, err)
	}
}

func TestAppSubscriberDrainUsesOneDeadlineAndPreservesCloseFailure(t *testing.T) {
	a, o, s := appSubscriberTestOwner()
	defer o.cancel()
	s.err = errors.New("synthetic cleanup failure")
	close(s.done)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := a.DrainRealtimeSubscriber(ctx)
	require.ErrorIs(t, err, s.err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(o.startDone)
	close(o.monitorDone)
	require.ErrorIs(t, a.DrainRealtimeSubscriber(context.Background()), s.err)
}
