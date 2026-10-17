package handlers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
)

// The root App owns admission for continuations and ordinary authenticated
// ingress. Scoped Apps never copy it. The two uncharged connections allow
// independent action claim/result and heartbeat/settlement transactions to make
// progress while model attempts hold their physical policy guards.
type inboundContinuationAdmission struct {
	mu      sync.Mutex
	used    int
	waiters []*inboundContinuationAdmissionWaiter
	changed chan struct{}
}

type inboundContinuationAdmissionWaiter struct {
	priority int
}

type inboundContinuationAdmissionContextKey struct{}

const (
	inboundContinuationApplyPriority = iota
	inboundContinuationPerformPriority
	inboundContinuationFreshPriority

	// Provisional accounting only: this does not reserve SQL connections from
	// other consumers and is not a demonstrated throughput configuration.
	inboundContinuationProgressAllowance = 2
)

var errInboundContinuationAdmissionStopped = errors.New("inbound continuation admission stopped")

// Bound ingress admission before it owns a tenant transaction. Cancelling this
// child after the grant affects neither the later fence statements nor COMMIT.
func (a *App) acquireIncomingMessageCapacity() (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	release, err := a.acquireInboundContinuationCapacity(ctx, nil, 1)
	if err != nil {
		return nil, fmt.Errorf("incoming message capacity admission: %w", err)
	}
	return release, nil
}

// checkInboundContinuationEntry must run before RunOnce takes runMu and before
// either public entry point claims a job. Internal phase transitions do not call
// this guard: they retain the marked context but release all DB/permit ownership
// before calling acquireInboundContinuationCapacity again.
func (a *App) checkInboundContinuationEntry(ctx context.Context) error {
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return errors.New("inbound continuation admission requires an app database")
	}
	if !root.rlsEnabled() {
		return nil
	}
	if a.inboundContinuation != nil {
		return errors.New("nested inbound continuation entry is not supported")
	}
	if ctx != nil && ctx.Value(inboundContinuationAdmissionContextKey{}) == root {
		return errors.New("recursive inbound continuation entry is not supported")
	}
	return a.requireInboundContinuationPoolEntry()
}

func (a *App) markInboundContinuationContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	root := a.rootApp()
	if root == nil || !root.rlsEnabled() {
		return ctx
	}
	return context.WithValue(ctx, inboundContinuationAdmissionContextKey{}, root)
}

func (a *App) requireInboundContinuationPoolEntry() error {
	root := a.rootApp()
	if root == nil || root.DB == nil || root.DB.Statement == nil || a.DB == nil || a.DB.Statement == nil {
		return errors.New("inbound continuation admission requires a root connection pool")
	}
	if _, ownsTx := a.DB.Statement.ConnPool.(gorm.TxCommitter); ownsTx {
		return errors.New("inbound continuation admission cannot wait inside a transaction")
	}
	if _, rootOwnsTx := root.DB.Statement.ConnPool.(gorm.TxCommitter); rootOwnsTx {
		return errors.New("inbound continuation admission requires a nontransactional root")
	}
	return nil
}

// acquireInboundContinuationCapacity never acquires a database connection.
// The caller must not hold a previous phase's permit or any DB connection when
// calling it, and must retain the returned permit until that phase has unwound.
// The receiver check cannot detect an unrelated connection held by its caller.
// Current proposed internal weights are 1 (perform) and 3 (apply); fresh entry
// may request either supported weight as the preparation design is finalized.
func (a *App) acquireInboundContinuationCapacity(ctx context.Context, stop <-chan struct{}, weight int) (func(), error) {
	root := a.rootApp()
	if root == nil || root.DB == nil {
		return nil, errors.New("inbound continuation admission requires an app database")
	}
	if !root.rlsEnabled() {
		return func() {}, nil
	}
	if err := a.requireInboundContinuationPoolEntry(); err != nil {
		return nil, err
	}
	pool, err := root.DB.DB()
	if err != nil || pool == nil {
		return nil, errors.New("inbound continuation admission cannot resolve the root SQL pool")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	priority := inboundContinuationFreshPriority
	if ctx.Value(inboundContinuationAdmissionContextKey{}) == root {
		if weight == 3 {
			priority = inboundContinuationApplyPriority
		} else {
			priority = inboundContinuationPerformPriority
		}
	}
	return root.continuationAdmission.acquire(ctx, stop, weight, priority, func() int {
		return pool.Stats().MaxOpenConnections
	})
}

// acquire gives apply work priority over perform work, and both priority over
// fresh claims; FIFO is retained within each class. A higher-priority waiter
// that needs more capacity blocks lower-priority grants, allowing released
// capacity to accumulate. Fresh claims can be delayed by admitted work; this
// prioritization is not a throughput or tenant-fairness guarantee.
func (g *inboundContinuationAdmission) acquire(ctx context.Context, stop <-chan struct{}, weight, priority int, maxOpen func() int) (func(), error) {
	if weight != 1 && weight != 3 {
		return nil, errors.New("inbound continuation admission weight must be 1 or 3")
	}
	if priority < inboundContinuationApplyPriority || priority > inboundContinuationFreshPriority || maxOpen == nil {
		return nil, errors.New("inbound continuation admission parameters are invalid")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	waiter := &inboundContinuationAdmissionWaiter{priority: priority}
	g.mu.Lock()
	g.waiters = append(g.waiters, waiter)
	g.notifyLocked()
	for {
		if err := inboundContinuationAdmissionWaitError(ctx, stop); err != nil {
			g.removeLocked(waiter)
			g.notifyLocked()
			g.mu.Unlock()
			return nil, err
		}
		maximum := maxOpen()
		if maximum < 0 || (maximum > 0 && maximum < 3 && (weight != 1 || priority != inboundContinuationFreshPriority)) {
			g.removeLocked(waiter)
			g.notifyLocked()
			g.mu.Unlock()
			return nil, fmt.Errorf("inbound continuation pool limit %d cannot admit weight %d with progress allowance %d", maximum, weight, inboundContinuationProgressAllowance)
		}
		budget := maximum - inboundContinuationProgressAllowance
		fits := maximum == 0 || g.used <= budget-weight
		if maximum > 0 && maximum <= 4 {
			// Tiny pools admit only fresh one-connection preparation; perform and
			// apply are refused above. All small pools serialize the admitted phase.
			// Keep its real weight so a later pool increase cannot over-admit.
			fits = g.used == 0
		}
		head := g.waiters[0]
		for _, queued := range g.waiters[1:] {
			if queued.priority < head.priority {
				head = queued
			}
		}
		if head == waiter && fits {
			g.removeLocked(waiter)
			g.used += weight
			g.notifyLocked()
			g.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					g.mu.Lock()
					g.used -= weight
					g.notifyLocked()
					g.mu.Unlock()
				})
			}, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-stop:
		case <-changed:
		case <-time.After(time.Second): // Observe an external SetMaxOpenConns change.
		}
		g.mu.Lock()
	}
}

func inboundContinuationAdmissionWaitError(ctx context.Context, stop <-chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-stop:
		return errInboundContinuationAdmissionStopped
	default:
		return nil
	}
}

func (g *inboundContinuationAdmission) removeLocked(waiter *inboundContinuationAdmissionWaiter) {
	for i, queued := range g.waiters {
		if queued == waiter {
			copy(g.waiters[i:], g.waiters[i+1:])
			g.waiters[len(g.waiters)-1] = nil
			g.waiters = g.waiters[:len(g.waiters)-1]
			return
		}
	}
}

func (g *inboundContinuationAdmission) notifyLocked() {
	if g.changed != nil {
		close(g.changed)
	}
	g.changed = make(chan struct{})
}
