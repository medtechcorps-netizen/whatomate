package queue

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zerodha/logf"
)

// ReceiveTimeout avoids go-redis's Channel helpers. Those helpers own hidden
// goroutines and can remain blocked sending to a full channel after Close.
const subscriberReceiveInterval = 250 * time.Millisecond

type subscriberPubSub interface {
	ReceiveTimeout(context.Context, time.Duration) (interface{}, error)
	Ping(context.Context, ...string) error
	Close() error
}

type ownedSubscriberPubSub struct {
	connection subscriberPubSub
	once       sync.Once
	err        error
	report     func(error)
}

func (p *ownedSubscriberPubSub) close() error {
	p.once.Do(func() {
		p.err = p.connection.Close()
		p.report(p.err)
	})
	return p.err
}

// Subscriber owns one subscription lifetime, including startup, reconnect,
// the synchronous handler and connection cleanup. It cannot be restarted.
type Subscriber struct {
	log        logf.Logger
	open       func(context.Context, ...string) subscriberPubSub
	mu         sync.Mutex
	current    *ownedSubscriberPubSub
	cancel     context.CancelFunc
	started    bool
	running    bool
	stopping   bool
	closing    bool
	done       chan struct{}
	doneClosed bool
	closeErr   error
	live       atomic.Bool
	reconnects atomic.Uint64
}

func NewSubscriber(client *redis.Client, log logf.Logger) *Subscriber {
	s := &Subscriber{log: log, done: make(chan struct{})}
	if client != nil {
		s.open = func(ctx context.Context, channels ...string) subscriberPubSub {
			return client.Subscribe(ctx, channels...)
		}
	}
	return s
}

func (s *Subscriber) begin(parent context.Context) (context.Context, error) {
	if s == nil || parent == nil || s.open == nil {
		return nil, errors.New("subscriber is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopping {
		return nil, errors.New("subscriber lifetime already used")
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel, s.started, s.running = cancel, true, true
	return ctx, nil
}

func (s *Subscriber) completeLocked() {
	if s.stopping && !s.running && !s.closing && !s.doneClosed {
		s.doneClosed = true
		close(s.done)
	}
}

func (s *Subscriber) requestStop() {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return
	}
	s.stopping = true
	s.live.Store(false)
	cancel, current := s.cancel, s.current
	s.closing = current != nil
	s.completeLocked()
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if current != nil {
		go func() {
			_ = current.close()
			s.mu.Lock()
			s.closing = false
			s.completeLocked()
			s.mu.Unlock()
		}()
	}
}

func (s *Subscriber) finish() {
	s.requestStop()
	s.mu.Lock()
	s.running = false
	s.completeLocked()
	s.mu.Unlock()
}

func (s *Subscriber) confirm(ctx context.Context, channels []string) (*ownedSubscriberPubSub, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	connection := s.open(ctx, channels...)
	if connection == nil {
		return nil, errors.New("subscriber connection is unavailable")
	}
	p := &ownedSubscriberPubSub{connection: connection, report: func(err error) {
		if err != nil {
			s.mu.Lock()
			s.closeErr = errors.Join(s.closeErr, err)
			s.mu.Unlock()
		}
	}}
	s.mu.Lock()
	if s.stopping || ctx.Err() != nil {
		s.mu.Unlock()
		_ = p.close()
		return nil, context.Canceled
	}
	// Publish the connection before confirmation so stop can interrupt startup.
	s.current = p
	s.mu.Unlock()
	confirmed := make(map[string]bool, len(channels))
	for len(confirmed) < len(channels) {
		if err := ctx.Err(); err != nil {
			_ = p.close()
			return nil, err
		}
		value, err := connection.ReceiveTimeout(ctx, subscriberReceiveInterval)
		if subscriberTimeout(err) {
			continue
		}
		if err != nil {
			_ = p.close()
			return nil, err
		}
		if subscription, ok := value.(*redis.Subscription); ok && subscription.Kind == "subscribe" {
			for _, channel := range channels {
				if channel == subscription.Channel {
					confirmed[channel] = true
				}
			}
		}
	}
	return p, nil
}

func subscriberTimeout(err error) bool {
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

func (s *Subscriber) activate(ctx context.Context, realtime bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || ctx.Err() != nil {
		return false
	}
	if realtime {
		s.live.Store(true)
	}
	return true
}

func (s *Subscriber) subscribe(parent context.Context, channels []string, realtime bool, handler func(*redis.Message)) error {
	ctx, err := s.begin(parent)
	if err != nil {
		return err
	}
	launched := false
	defer func() {
		if !launched {
			s.finish()
		}
	}()
	p, err := s.confirm(ctx, channels)
	if err != nil {
		return err
	}
	if !s.activate(ctx, realtime) {
		return context.Canceled
	}
	s.log.Info("Redis subscriber started", "channels", channels)
	launched = true
	go s.run(ctx, p, channels, realtime, handler)
	return nil
}

func (s *Subscriber) run(ctx context.Context, p *ownedSubscriberPubSub, channels []string, realtime bool, handler func(*redis.Message)) {
	defer s.finish()
	lastPing := time.Now()
	var awaitingPong time.Time
	for ctx.Err() == nil {
		value, err := p.connection.ReceiveTimeout(ctx, subscriberReceiveInterval)
		if ctx.Err() != nil {
			return
		}
		if subscriberTimeout(err) {
			err = nil
			if !awaitingPong.IsZero() && time.Since(awaitingPong) >= realtimeSubscriberPingTimeout {
				err = errors.New("Redis subscriber health check timed out")
			} else if awaitingPong.IsZero() && time.Since(lastPing) >= realtimeSubscriberPingInterval {
				pingCtx, cancel := context.WithTimeout(ctx, realtimeSubscriberPingTimeout)
				err = p.connection.Ping(pingCtx)
				cancel()
				lastPing = time.Now()
				awaitingPong = lastPing
			}
		} else if err == nil {
			// Any received reply proves that this connection is responsive.
			lastPing = time.Now()
			awaitingPong = time.Time{}
		}
		if err == nil {
			if message, ok := value.(*redis.Message); ok {
				func() {
					defer func() {
						if recovered := recover(); recovered != nil {
							s.log.Error("Redis subscriber handler panicked", "panic", recovered)
						}
					}()
					handler(message)
				}()
			}
			continue
		}
		s.live.Store(false)
		_ = p.close()
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("Redis subscription interrupted", "error", err)
		delay := realtimeSubscriberRetryMin
		for {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			replacement, err := s.confirm(ctx, channels)
			if err == nil {
				if !s.activate(ctx, realtime) {
					return
				}
				p = replacement
				lastPing = time.Now()
				awaitingPong = time.Time{}
				if realtime {
					s.reconnects.Add(1)
				}
				s.log.Info("Redis subscription recovered", "channels", channels)
				break
			}
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("Redis subscription recovery failed", "error", err, "retry_in", delay)
			if delay < realtimeSubscriberRetryMax {
				delay *= 2
				if delay > realtimeSubscriberRetryMax {
					delay = realtimeSubscriberRetryMax
				}
			}
		}
	}
}

func (s *Subscriber) SubscribeCampaignStats(ctx context.Context, handler func(*CampaignStatsUpdate)) error {
	if handler == nil {
		return errors.New("campaign stats handler is required")
	}
	return s.subscribe(ctx, []string{CampaignStatsChannel}, false, func(message *redis.Message) {
		var update CampaignStatsUpdate
		if err := json.Unmarshal([]byte(message.Payload), &update); err != nil {
			s.log.Error("Failed to unmarshal campaign stats update", "error", err)
			return
		}
		handler(&update)
	})
}

func (s *Subscriber) SubscribeRealtime(ctx context.Context, handler func(*RealtimeEvent)) error {
	if handler == nil {
		return errors.New("realtime subscriber handler is required")
	}
	return s.subscribe(ctx, []string{RealtimeChannel, RealtimeUserChannel}, true, func(message *redis.Message) {
		var event RealtimeEvent
		if err := json.Unmarshal([]byte(message.Payload), &event); err != nil {
			s.log.Error("Failed to unmarshal realtime event", "error", err)
			return
		}
		if err := validateRealtimeEvent(&event); err != nil {
			s.log.Warn("Discarding invalid realtime event", "error", err)
			return
		}
		if (message.Channel == RealtimeUserChannel && event.UserID == nil) ||
			(message.Channel == RealtimeChannel && event.UserID != nil) {
			s.log.Warn("Discarding realtime event from mismatched scope channel", "channel", message.Channel, "event_id", event.EventID)
			return
		}
		handler(&event)
	})
}

func (s *Subscriber) RealtimeLive() bool { return s != nil && s.live.Load() }

func (s *Subscriber) RealtimeReconnectCount() uint64 {
	if s == nil {
		return 0
	}
	return s.reconnects.Load()
}

// Done closes only after startup, receive/handler work and owned close finish.
func (s *Subscriber) Done() <-chan struct{} {
	if s == nil || s.done == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return s.done
}

// Stop requests cancellation and joins under the caller's existing deadline.
// Expiry never marks an unfinished handler or close operation as complete.
func (s *Subscriber) Stop(ctx context.Context) error {
	if s == nil || s.done == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("subscriber stop context is required")
	}
	s.requestStop()
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Subscriber) Close() error { return s.Stop(context.Background()) }
