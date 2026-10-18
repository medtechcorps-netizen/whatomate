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
	phases  map[string]*inboundContinuationOrganizationPhase
}

type inboundContinuationAdmissionWaiter struct {
	priority     int
	organization string
	mode         int
	intake       bool
}

type inboundContinuationOrganizationPhase struct {
	mode         int
	activeMode   int
	active       int
	normalGrants int
	limit        int
	fixed        bool
	normalQueued [3]int
	applyQueued  [3]int
	firstNormal  int
	intakeQueued int
	// nil is unsealed; an empty nonnil map is an exhausted intake snapshot.
	intakeCohort map[*inboundContinuationAdmissionWaiter]struct{}
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

const (
	inboundContinuationUnscoped = iota
	inboundContinuationReader
	inboundContinuationWriter
)

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
	return a.acquireInboundContinuationPhase(ctx, stop, weight, "", inboundContinuationUnscoped)
}

// Scoped phase admission is only an in-process scheduling boundary. Callers
// release the previous phase completely before entering; database policy and
// account fences remain authoritative, including against other processes.
func (a *App) acquireInboundContinuationPhase(ctx context.Context, stop <-chan struct{}, weight int, organization string, mode int) (func(), error) {
	return a.acquireInboundContinuationPhaseClass(ctx, stop, weight, organization, mode, false)
}

// Only authenticated pre-ACK ingress uses this class, after bounded tenant
// discovery has released its generic permit and database connection. Ordinary
// fresh preparation remains on acquireInboundContinuationPhase.
func (a *App) acquireIncomingMessageIntake(ctx context.Context, organization string) (func(), error) {
	return a.acquireInboundContinuationPhaseClass(ctx, nil, 1, organization, inboundContinuationWriter, true)
}

func (a *App) acquireInboundContinuationPhaseClass(ctx context.Context, stop <-chan struct{}, weight int, organization string, mode int, intake bool) (func(), error) {
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
	if intake {
		if priority != inboundContinuationFreshPriority {
			return nil, errors.New("inbound continuation intake cannot enter a continuation phase")
		}
		return root.continuationAdmission.acquireIntake(ctx, stop, func() int {
			return pool.Stats().MaxOpenConnections
		}, organization)
	}
	return root.continuationAdmission.acquireScoped(ctx, stop, weight, priority, func() int {
		return pool.Stats().MaxOpenConnections
	}, organization, mode)
}

// acquire gives apply work priority so released capacity can accumulate for a
// completed result. Fresh ingress/preparation and new physical attempts share
// one FIFO below apply: neither class can overtake an already queued waiter.
// The phase labels still distinguish tiny-pool eligibility. This bounds queue
// overtaking, not wall-clock latency or throughput through the database fences.
func (g *inboundContinuationAdmission) acquire(ctx context.Context, stop <-chan struct{}, weight, priority int, maxOpen func() int) (func(), error) {
	return g.acquireScoped(ctx, stop, weight, priority, maxOpen, "", inboundContinuationUnscoped)
}

func (g *inboundContinuationAdmission) acquireScoped(ctx context.Context, stop <-chan struct{}, weight, priority int, maxOpen func() int, organization string, mode int) (func(), error) {
	return g.acquireScopedClass(ctx, stop, weight, priority, maxOpen, organization, mode, false)
}

func (g *inboundContinuationAdmission) acquireIntake(ctx context.Context, stop <-chan struct{}, maxOpen func() int, organization string) (func(), error) {
	return g.acquireScopedClass(ctx, stop, 1, inboundContinuationFreshPriority, maxOpen, organization, inboundContinuationWriter, true)
}

func (g *inboundContinuationAdmission) acquireScopedClass(ctx context.Context, stop <-chan struct{}, weight, priority int, maxOpen func() int, organization string, mode int, intake bool) (func(), error) {
	if weight != 1 && weight != 3 {
		return nil, errors.New("inbound continuation admission weight must be 1 or 3")
	}
	if priority < inboundContinuationApplyPriority || priority > inboundContinuationFreshPriority || maxOpen == nil {
		return nil, errors.New("inbound continuation admission parameters are invalid")
	}
	if (organization == "" && mode != inboundContinuationUnscoped) ||
		(organization != "" && mode != inboundContinuationReader && mode != inboundContinuationWriter) {
		return nil, errors.New("inbound continuation organization phase is invalid")
	}
	if intake && (weight != 1 || priority != inboundContinuationFreshPriority || organization == "" || mode != inboundContinuationWriter) {
		return nil, errors.New("inbound continuation intake phase is invalid")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	waiter := &inboundContinuationAdmissionWaiter{priority: priority, organization: organization, mode: mode, intake: intake}
	g.mu.Lock()
	g.waiters = append(g.waiters, waiter)
	g.notifyLocked()
	for {
		if err := inboundContinuationAdmissionWaitError(ctx, stop); err != nil {
			g.removeLocked(waiter)
			g.refreshPhasesLocked()
			g.notifyLocked()
			g.mu.Unlock()
			return nil, err
		}
		maximum := maxOpen()
		if maximum < 0 || (maximum > 0 && maximum < 3 && (weight != 1 || priority != inboundContinuationFreshPriority)) {
			g.removeLocked(waiter)
			g.refreshPhasesLocked()
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
		head := g.headLocked()
		if head == waiter && fits {
			if intake {
				phase := g.phases[organization]
				if len(phase.intakeCohort) == 0 {
					// Seal only on actual intake service. An empty writer turn may
					// first run discovery/preparation before scoped intake appears.
					// A sealed, exhausted snapshot can renew only after the normal
					// turn is complete and no reader is waiting (eligibleLocked).
					if phase.intakeCohort != nil {
						phase.normalGrants = 0
					}
					phase.intakeCohort = make(map[*inboundContinuationAdmissionWaiter]struct{})
					for _, queued := range g.waiters {
						if queued.organization == organization && queued.intake {
							phase.intakeCohort[queued] = struct{}{}
						}
					}
				}
			}
			g.removeLocked(waiter)
			g.used += weight
			if organization != "" {
				phase := g.phases[organization]
				phase.activeMode = mode
				phase.active++
				if priority != inboundContinuationApplyPriority {
					phase.fixed = true
					if !intake && phase.normalGrants < phase.limit {
						phase.normalGrants++
					}
				}
			}
			g.notifyLocked()
			g.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					g.mu.Lock()
					g.used -= weight
					if organization != "" {
						phase := g.phases[organization]
						phase.active--
						if phase.active == 0 {
							phase.activeMode = inboundContinuationUnscoped
						}
					}
					g.refreshPhasesLocked()
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

// headLocked preserves apply priority and FIFO among eligible waiters. An
// org-incompatible waiter owns no credits and cannot block another tenant.
// Once selected, an eligible weight-3 waiter still accumulates credits: no
// later small request bypasses it merely because it fits. Recompute on every
// queue/ownership change; a canceled or newly ineligible head reserves nothing.
func (g *inboundContinuationAdmission) headLocked() *inboundContinuationAdmissionWaiter {
	g.refreshPhasesLocked()
	var head *inboundContinuationAdmissionWaiter
	for _, queued := range g.waiters {
		if !g.eligibleLocked(queued) {
			continue
		}
		if head == nil || (queued.priority == inboundContinuationApplyPriority && head.priority != inboundContinuationApplyPriority) {
			head = queued
		}
	}
	return head
}

func (g *inboundContinuationAdmission) phaseWaitersLocked(organization string, mode int) (same, opposite, pending bool) {
	phase := g.phases[organization]
	for _, candidate := range []int{inboundContinuationReader, inboundContinuationWriter} {
		pending = pending || phase.normalQueued[candidate] != 0 || phase.applyQueued[candidate] != 0
		if candidate == mode {
			same = phase.normalQueued[candidate] != 0
		} else {
			opposite = phase.normalQueued[candidate] != 0
		}
	}
	return
}

// Reader/preparation phases retain the existing bounded recovery batch once
// the opposite mode is waiting. Writers additionally service one finite intake
// snapshot, sealed on its first grant. This is an unmeasured scheduling policy,
// not a throughput or successful-admission deadline guarantee. Apply grants
// finish finite admitted work without reopening either normal service cohort.
func (g *inboundContinuationAdmission) refreshPhasesLocked() {
	for _, phase := range g.phases {
		phase.normalQueued = [3]int{}
		phase.applyQueued = [3]int{}
		phase.firstNormal = inboundContinuationUnscoped
		phase.intakeQueued = 0
	}
	for _, queued := range g.waiters {
		if queued.organization == "" {
			continue
		}
		if g.phases == nil {
			g.phases = make(map[string]*inboundContinuationOrganizationPhase)
		}
		if g.phases[queued.organization] == nil {
			g.phases[queued.organization] = &inboundContinuationOrganizationPhase{
				mode: queued.mode, limit: defaultInboundContinuationBatch,
			}
		}
		phase := g.phases[queued.organization]
		if queued.priority == inboundContinuationApplyPriority {
			phase.applyQueued[queued.mode]++
		} else {
			phase.normalQueued[queued.mode]++
			if queued.intake {
				phase.intakeQueued++
			}
			if phase.firstNormal == inboundContinuationUnscoped {
				phase.firstNormal = queued.mode
			}
		}
	}
	for organization, phase := range g.phases {
		// Apply can temporarily own either mode, but never selects/reopens a
		// normal cohort. Fix the first cohort when normal work receives service;
		// an explicit opposite normal turn also stays fixed through interruption.
		if !phase.fixed && phase.firstNormal != inboundContinuationUnscoped {
			phase.mode = phase.firstNormal
		}
		same, opposite, pending := g.phaseWaitersLocked(organization, phase.mode)
		if phase.active != 0 {
			continue
		}
		if !pending {
			delete(g.phases, organization)
			continue
		}
		turnComplete := !same || phase.normalGrants >= phase.limit
		if phase.mode == inboundContinuationWriter {
			// Intake has its own finite snapshot, independent of the durable
			// preparation quota. A first or unfinished snapshot must not be
			// skipped merely because preparation already used its 50 grants.
			preparationQueued := phase.normalQueued[inboundContinuationWriter] - phase.intakeQueued
			turnComplete = !g.intakeReadyLocked(phase) && (preparationQueued == 0 || phase.normalGrants >= phase.limit)
		}
		if opposite && turnComplete {
			if phase.mode == inboundContinuationReader {
				phase.mode = inboundContinuationWriter
			} else {
				phase.mode = inboundContinuationReader
			}
			phase.normalGrants = 0
			phase.intakeCohort = nil
			phase.limit = defaultInboundContinuationBatch
			phase.fixed = true
		}
	}
}

func (g *inboundContinuationAdmission) eligibleLocked(waiter *inboundContinuationAdmissionWaiter) bool {
	if waiter.organization == "" {
		return true
	}
	phase := g.phases[waiter.organization]
	if phase == nil || (phase.active != 0 && (phase.activeMode != waiter.mode || waiter.mode == inboundContinuationWriter)) {
		return false
	}
	if waiter.priority == inboundContinuationApplyPriority {
		// At an owner-free boundary, apply may interrupt either normal cohort.
		// Only active ownership changes: the suspended quota is not reset.
		return true
	}
	if phase.mode != waiter.mode {
		return false
	}
	for _, mode := range []int{inboundContinuationReader, inboundContinuationWriter} {
		if mode != waiter.mode && phase.applyQueued[mode] != 0 {
			// Drain incompatible owners for accepted work rather than replenishing
			// up to another full normal cohort ahead of its existing priority.
			return false
		}
	}
	if waiter.mode == inboundContinuationWriter {
		intakeReady := g.intakeReadyLocked(phase)
		if waiter.intake {
			if !intakeReady {
				return false
			}
			if len(phase.intakeCohort) != 0 {
				_, captured := phase.intakeCohort[waiter]
				return captured
			}
			return true
		}
		if intakeReady {
			// Only same-organization preparation yields to this finite intake
			// set. Other eligible tenants and generic discovery retain FIFO.
			return false
		}
	}
	_, opposite, _ := g.phaseWaitersLocked(waiter.organization, phase.mode)
	return !opposite || phase.normalGrants < phase.limit
}

func (g *inboundContinuationAdmission) intakeReadyLocked(phase *inboundContinuationOrganizationPhase) bool {
	if phase.intakeQueued == 0 {
		return false
	}
	if phase.intakeCohort == nil || len(phase.intakeCohort) != 0 {
		return true
	}
	// Renew an exhausted snapshot only when this normal writer turn is done
	// and no opposite normal reader is waiting. This keeps ingress-only routes
	// work-conserving without refilling ahead of queued readers/preparations.
	preparationQueued := phase.normalQueued[inboundContinuationWriter] - phase.intakeQueued
	return phase.normalQueued[inboundContinuationReader] == 0 &&
		(preparationQueued == 0 || phase.normalGrants >= phase.limit)
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
	if waiter.organization != "" && g.phases[waiter.organization] != nil {
		delete(g.phases[waiter.organization].intakeCohort, waiter)
	}
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
