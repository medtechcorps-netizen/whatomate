package handlers

import (
	"context"
	"errors"

	"github.com/shridarpatil/whatomate/internal/queue"
)

type appSubscriberDrain interface {
	Stop(context.Context) error
	Done() <-chan struct{}
}

// appSubscriber also owns startup logging and the readiness monitor, which
// live outside the queue subscriber's receive loop.
type appSubscriber struct {
	ctx         context.Context
	cancel      context.CancelFunc
	subscriber  appSubscriberDrain
	startDone   chan struct{}
	monitorDone chan struct{}
	stopping    bool // guarded by the root App's subscriberMu
}

func (a *App) beginSubscriber(realtime bool) (*appSubscriber, *queue.Subscriber, error) {
	root := a.rootApp()
	root.subscriberMu.Lock()
	defer root.subscriberMu.Unlock()
	slot := &root.campaignOwned
	stopped := root.campaignStopped
	if realtime {
		slot = &root.realtimeOwned
		stopped = root.realtimeStopped
	}
	if stopped {
		return nil, nil, errors.New("application subscriber is stopping")
	}
	if *slot != nil {
		return nil, nil, errors.New("application subscriber lifetime already used")
	}
	ctx, cancel := context.WithCancel(context.Background())
	subscriber := queue.NewSubscriber(root.Redis, root.Log)
	owner := &appSubscriber{ctx: ctx, cancel: cancel, subscriber: subscriber,
		startDone: make(chan struct{}), monitorDone: make(chan struct{})}
	*slot = owner
	if realtime {
		root.RealtimeSubCancel = cancel
		root.realtimeSubscriberLive.Store(false)
	} else {
		root.CampaignSubCancel = cancel
		close(owner.monitorDone)
	}
	return owner, subscriber, nil
}

func (a *App) monitorRealtimeSubscriber(owner *appSubscriber, report func()) {
	defer close(owner.monitorDone)
	<-owner.subscriber.Done()
	a.rootApp().realtimeSubscriberLive.Store(false)
	if owner.ctx.Err() == nil {
		report()
	}
}

func (a *App) drainSubscriber(ctx context.Context, realtime bool) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("subscriber drain context is required")
	}
	root := a.rootApp()
	root.subscriberMu.Lock()
	if realtime {
		root.realtimeStopped = true
	} else {
		root.campaignStopped = true
	}
	owner := root.campaignOwned
	if realtime {
		owner = root.realtimeOwned
	}
	if owner == nil {
		root.subscriberMu.Unlock()
		return nil
	}
	owner.stopping = true
	if realtime {
		root.realtimeSubscriberLive.Store(false)
		root.RealtimeSubCancel = nil
	} else {
		root.CampaignSubCancel = nil
	}
	root.subscriberMu.Unlock()
	owner.cancel()
	err := owner.subscriber.Stop(ctx)
	for _, done := range []<-chan struct{}{owner.startDone, owner.monitorDone} {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		}
	}
	return err
}

// DrainRealtimeSubscriber joins receive, startup and readiness-monitor work
// under the server's existing shared shutdown deadline.
func (a *App) DrainRealtimeSubscriber(ctx context.Context) error {
	return a.drainSubscriber(ctx, true)
}

func (a *App) DrainCampaignStatsSubscriber(ctx context.Context) error {
	return a.drainSubscriber(ctx, false)
}

func (a *App) stopSubscriber(realtime bool) {
	if a == nil {
		return
	}
	root := a.rootApp()
	root.subscriberMu.Lock()
	if realtime {
		root.realtimeStopped = true
	} else {
		root.campaignStopped = true
	}
	owner, legacy := root.campaignOwned, root.CampaignSubCancel
	if realtime {
		owner, legacy = root.realtimeOwned, root.RealtimeSubCancel
		root.RealtimeSubCancel = nil
	} else {
		root.CampaignSubCancel = nil
	}
	root.subscriberMu.Unlock()
	if owner != nil {
		_ = root.drainSubscriber(context.Background(), realtime)
	} else if legacy != nil {
		// Preserve independently constructed Apps' existing cancellation hook.
		legacy()
	}
}
