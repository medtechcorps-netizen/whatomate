package channel

// This file is the lock protocol between the legacy Meta bridge and the
// tenant policy fence (database.LockOrganizationPolicyScope). Comments
// elsewhere point here instead of restating it.
//
// # Lock order
//
// Every transaction that takes more than one of these locks takes them in
// this order:
//
//  1. the policy fence key, an advisory lock per organization: fences take it
//     exclusively, everyone else in shared mode through the queue below (or,
//     past the queue, the bypass key in shared mode);
//  2. the organization row: fences FOR UPDATE, bridge transactions FOR SHARE,
//     AI attempt fences FOR KEY SHARE; the platform-compliance write guard
//     and foreign-key checks take it implicitly when they insert
//     organization-scoped rows, such as an audit row;
//  3. the ChannelAccount shadow, FOR UPDATE;
//  4. the whatsapp_accounts row: account updates and renames FOR UPDATE,
//     sends FOR SHARE across their Meta call;
//  5. the contact identity;
//  6. the inbox conversation;
//  7. the contact;
//  8. the message.
//
// A transaction that would take a later lock and then an earlier one must
// take the earlier one first, even when it needs it only for an implicit
// check: a transaction that updates an account row and then audits takes the
// organization first (LockLegacyMetaOrganizationAndWhatsAppAccount), and a
// contact write that fires the contact-selector trigger, which takes the
// organization FOR UPDATE NOWAIT and tries the fence key, takes the policy
// fence first.
//
// # Waiting
//
// A transaction never waits for one of these locks while it holds a later
// one, and a bridge transaction does not wait for the shadow or the account
// row while it holds the organization:
// lockLegacyMetaOrganizationThenShadow takes them in savepointed rounds and
// falls back to plain order only after lockLegacyMetaShadowRounds lost
// rounds. Fence owners are the exception: they wait for later locks behind
// their own fence by design, and re-enter the organization without waiting
// (reenterLegacyMetaPolicyFenceOrganization).
//
// The only waits PostgreSQL's deadlock detector cannot see are the polls for
// the fence key in joinLegacyMetaPolicyFence. They matter because a goroutine
// can hold a lock on one connection while it waits for its own work on
// another: an AI attempt fence holds the organization FOR KEY SHARE while it
// waits for its outbound mirror. Each call bounds them with one set of
// deadlines fixed at its start (legacyMetaPolicyFenceDeadlines), so such a
// cycle costs at most one bound per call.
//
// # The queue
//
// Fences take the key exclusively before the organization FOR UPDATE. While
// one holds the key or waits for it, a shared try-lock fails, so sharers poll
// instead of joining PostgreSQL's queue for the row, where each new FOR SHARE
// would overtake the waiting FOR UPDATE. A poller classifies the fences
// (legacyMetaPolicyFenceQueueStateSQL), where a member is a backend that
// holds the fence key or the bypass key:
//   - waiting: it queues up to legacyMetaPolicyFenceQueueWait (250 ms);
//   - member-blocked, the key holder waits only for running members: those
//     finish within milliseconds, so it queues up to
//     legacyMetaPolicyFenceMemberWait (1 s). Bypassing there is what lets
//     overlapping FOR SHAREs keep the fence out for as long as they overlap;
//   - stuck, a fence waits for a running backend that is not a member: it does
//     not queue at all, since nothing it could wait for would free the fence.
//
// Past its limit a poller bypasses: it takes the bypass key and records the
// organization in a transaction-local setting, then takes the row without the
// fence key. A transaction that went past the queue does not queue again when
// it takes the organization a second time.
//
// # Call sites
//
//   - Mirrors and backfill: ensureLegacyMetaAccount.
//   - Fence owners (Coexistence admission, identity review, the chatbot's
//     legacy bridge): EnsureLegacyMetaWhatsAppAccount.
//   - Strict and Omnichannel replies: LockLegacyMetaOrganizationAndShadow
//     (internal/handlers/legacy_meta_bridge.go, messages.go).
//   - Delivery recovery: LockLegacyMetaOrganization (internal/handlers/messages.go).
//   - Account updates, renames and their subscription status:
//     StageLegacyMetaWhatsAppAccountRename and
//     LockLegacyMetaOrganizationAndWhatsAppAccount (internal/handlers/accounts.go).
//   - Contact-selector writes: they take database.LockOrganizationPolicyScope
//     first (internal/handlers/canonical_contact_writes.go).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// lockLegacyMetaShadowRounds bounds the savepointed rounds of
// lockLegacyMetaOrganizationThenShadow. A round is lost only to a policy fence,
// an organization update or another lock holder that made progress, so the
// bound is reached only under sustained traffic on one tenant; the caller then
// waits in plain order instead of failing.
const lockLegacyMetaShadowRounds = 64

var (
	errLegacyMetaPolicyFenceQueued = errors.New("legacy Meta policy fence is queued")
	// errLegacyMetaLockRoundLost is returned by a lockRows callback that, after
	// waiting for one row holding nothing, found a later one busy.
	errLegacyMetaLockRoundLost = errors.New("legacy Meta lock round lost")
	errLegacyMetaRowAvailable  = errors.New("legacy Meta row is available")
)

// legacyMetaPolicyFenceQueueWait bounds how long one lock acquisition queues
// for the fence key behind a fence that is not blocked, or waits for the key,
// summed over all its rounds. Fences normally own the key and the organization
// row within milliseconds, so this serves them before sharers that arrive
// later on any account while capping what each acquisition pays when
// admissions keep arriving.
var legacyMetaPolicyFenceQueueWait = 250 * time.Millisecond

// legacyMetaPolicyFenceMemberWait bounds the queue behind a fence that holds
// the key and is blocked only by running members. It is a bound only for a
// cycle through a member's own goroutine; otherwise the members finish first.
var legacyMetaPolicyFenceMemberWait = time.Second

// legacyMetaPolicyFenceStateTTL is how long a process reuses one organization's
// fence state. The check reads the whole lock table and takes every
// lock-manager partition lock, so waiting sharers of one tenant share it. It
// also bounds how stale a state a poller acts on: twice the TTL while another
// caller refreshes it (legacyMetaPolicyFenceStateOf).
var legacyMetaPolicyFenceStateTTL = 25 * time.Millisecond

const (
	// legacyMetaPolicyFencePollInterval spaces a poller's try-locks. A fence
	// holds the organization for milliseconds, so a longer interval would
	// delay sharers after every fence; each try is one round trip that takes
	// no lock-manager partition lock but the key's own.
	legacyMetaPolicyFencePollInterval = 5 * time.Millisecond

	// legacyMetaPolicyFenceWalkDepth is how many levels of blockers
	// legacyMetaPolicyFenceQueueStateSQL follows from the fence.
	legacyMetaPolicyFenceWalkDepth = 4

	legacyMetaPolicyFenceBypassNamespace = "rereply:legacy_meta_policy_fence_bypass:v1:"
	// legacyMetaPolicyFenceBypassSetting holds, transaction-locally, the
	// organization this transaction went past the queue for.
	legacyMetaPolicyFenceBypassSetting = "rereply.legacy_meta_fence_bypass"
)

// legacyMetaPolicyFenceBypassKey is held in shared mode by a transaction that
// took the organization without the policy fence key. No one takes it
// exclusively, so taking it never waits. It tells fences waiting behind such a
// transaction apart from locks outside the queue.
func legacyMetaPolicyFenceBypassKey(organizationID uuid.UUID) string {
	return legacyMetaPolicyFenceBypassNamespace + organizationID.String()
}

// legacyMetaPolicyFenceState is what holds up the organization's fences
// (legacyMetaPolicyFenceQueueStateSQL).
type legacyMetaPolicyFenceState int

const (
	// legacyMetaPolicyFenceWaiting: no fence holds or waits for the key, the
	// fence is not blocked, it waits for the key behind running members, or
	// its chain of blockers is too long to tell.
	legacyMetaPolicyFenceWaiting legacyMetaPolicyFenceState = iota
	// legacyMetaPolicyFenceMemberBlocked: the fence that holds the key waits,
	// directly or through other waiting backends, only for running members.
	legacyMetaPolicyFenceMemberBlocked
	// legacyMetaPolicyFenceStuck: the fence that holds the key, or one that
	// waits for it, waits, directly or through other waiting backends, for a
	// running backend that is not a member.
	legacyMetaPolicyFenceStuck
)

// legacyMetaPolicyFenceDeadlines are one acquisition's queue limits, both
// counted from its start.
type legacyMetaPolicyFenceDeadlines struct {
	queue, member time.Time
}

func newLegacyMetaPolicyFenceDeadlines() legacyMetaPolicyFenceDeadlines {
	start := time.Now()
	return legacyMetaPolicyFenceDeadlines{
		queue:  start.Add(legacyMetaPolicyFenceQueueWait),
		member: start.Add(max(legacyMetaPolicyFenceQueueWait, legacyMetaPolicyFenceMemberWait)),
	}
}

func (deadlines legacyMetaPolicyFenceDeadlines) limit(state legacyMetaPolicyFenceState) time.Time {
	if state == legacyMetaPolicyFenceMemberBlocked {
		return deadlines.member
	}
	return deadlines.queue
}

// legacyMetaPolicyFenceQueueStateSQL classifies what holds up the fences of
// the organization: the one that holds the key exclusively (at most one does)
// and those that wait for it. It follows each chain of blockers, up to four
// deep, through every backend that is itself waiting for a lock, and
// classifies the backends at the ends of the chains:
//   - any running backend that is not a member makes the fences stuck: an AI
//     attempt fence idle in its transaction while its goroutine waits for
//     this mirror (the only shape a cycle through Go can take), this
//     transaction itself, or a send that holds a contact across its Meta call
//     while a member (which joined the key or bypassed) waits for that
//     contact. Queueing behind those would buy the fences nothing.
//   - a running member (one that holds the fence key or the bypass key and
//     waits for no lock) finishes within milliseconds. When the key holder's
//     chains end only at those, it is blocked only by members. A fence that
//     waits for the key behind them counts as waiting.
//
// So an organization writer queued between the key holder and a stream of
// sharers (organization settings take the row FOR UPDATE without the key)
// only passes the wait on to those sharers. pg_locks and pg_blocking_pids read
// the whole lock table and take every lock-manager partition lock, so
// legacyMetaPolicyFenceStateOf shares one result per organization and process.
// The deepest known chain is three below the key holder (organization writer,
// member, send-held contact), or four below a waiting fence. Truncation only
// loses evidence and waits until the per-call bound; each waiting node costs a
// pg_blocking_pids call per level. Cost grows with queued fences, lock-table
// size, and the number of application replicas and tenants polling.
const legacyMetaPolicyFenceQueueStateSQL = `WITH RECURSIVE keys AS (
	SELECT pg_catalog.hashtextextended(?, 0) AS fence,
	       pg_catalog.hashtextextended(?, 0) AS bypass,
	       (SELECT d.oid FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database()) AS database
), locks AS MATERIALIZED (
	SELECT l.pid, l.locktype, l.mode, l.granted, l.database, l.objsubid,
	       (l.classid::int8 << 32) | l.objid::int8 AS key
	FROM pg_catalog.pg_locks l
), members AS MATERIALIZED (
	SELECT DISTINCT locks.pid
	FROM locks CROSS JOIN keys
	WHERE locks.locktype = 'advisory' AND locks.objsubid = 1 AND locks.database = keys.database
	  AND locks.key IN (keys.fence, keys.bypass)
), lock_waiters AS MATERIALIZED (
	SELECT DISTINCT locks.pid FROM locks WHERE NOT locks.granted
), blockers(pid, depth, holder) AS (
	SELECT blocker.pid, 1, fence.granted
	FROM locks fence
	CROSS JOIN keys
	CROSS JOIN LATERAL pg_catalog.unnest(pg_catalog.pg_blocking_pids(fence.pid)) AS blocker(pid)
	WHERE fence.locktype = 'advisory' AND fence.objsubid = 1 AND fence.database = keys.database
	  AND fence.key = keys.fence AND fence.mode = 'ExclusiveLock'
	UNION
	SELECT next.pid, blockers.depth + 1, blockers.holder
	FROM blockers
	CROSS JOIN LATERAL pg_catalog.unnest(pg_catalog.pg_blocking_pids(blockers.pid)) AS next(pid)
	WHERE blockers.depth < ?
	  AND blockers.pid IN (SELECT lock_waiters.pid FROM lock_waiters)
), running AS (
	SELECT blockers.pid, blockers.holder, blockers.pid IN (SELECT members.pid FROM members) AS member
	FROM blockers
	WHERE blockers.pid NOT IN (SELECT lock_waiters.pid FROM lock_waiters)
)
SELECT CASE
	WHEN EXISTS (SELECT 1 FROM running WHERE NOT running.member) THEN 2
	WHEN EXISTS (SELECT 1 FROM running WHERE running.member AND running.holder) THEN 1
	ELSE 0
END`

type legacyMetaPolicyFenceStateSample struct {
	state legacyMetaPolicyFenceState
	at    time.Time
}

// legacyMetaPolicyFenceStateCache is one organization's last fence state in
// this process. refresh admits one query at a time.
type legacyMetaPolicyFenceStateCache struct {
	refresh sync.Mutex
	sample  atomic.Pointer[legacyMetaPolicyFenceStateSample]
}

// within returns the last state if it was sampled less than age ago, and
// waiting otherwise.
func (cache *legacyMetaPolicyFenceStateCache) within(age time.Duration) (legacyMetaPolicyFenceState, bool) {
	sample := cache.sample.Load()
	if sample == nil || time.Since(sample.at) >= age {
		return legacyMetaPolicyFenceWaiting, false
	}
	return sample.state, true
}

var (
	// legacyMetaPolicyFenceStates maps an organization to its
	// *legacyMetaPolicyFenceStateCache.
	legacyMetaPolicyFenceStates sync.Map
	// legacyMetaPolicyFenceStateQueries counts state queries, for tests.
	legacyMetaPolicyFenceStateQueries atomic.Int64
)

// legacyMetaPolicyFenceStateOf returns the organization's fence state. A sample
// younger than legacyMetaPolicyFenceStateTTL is reused; otherwise one caller
// per organization and process queries the lock table, while the others use
// the last sample if it is younger than twice the TTL. Anything older is not
// used, so a state left from an earlier episode, such as a stuck one, cannot
// send a burst of sharers past a later fence. The query fails safe: an error,
// like a missing or too old sample, counts as waiting, the short bound, and is
// cached like any other result.
func legacyMetaPolicyFenceStateOf(db *gorm.DB, organizationID uuid.UUID) legacyMetaPolicyFenceState {
	value, _ := legacyMetaPolicyFenceStates.LoadOrStore(organizationID, &legacyMetaPolicyFenceStateCache{})
	cache := value.(*legacyMetaPolicyFenceStateCache)
	if state, fresh := cache.within(legacyMetaPolicyFenceStateTTL); fresh {
		return state
	}
	if !cache.refresh.TryLock() {
		state, _ := cache.within(2 * legacyMetaPolicyFenceStateTTL)
		return state
	}
	defer cache.refresh.Unlock()
	if state, fresh := cache.within(legacyMetaPolicyFenceStateTTL); fresh {
		return state
	}
	legacyMetaPolicyFenceStateQueries.Add(1)
	state := legacyMetaPolicyFenceWaiting
	if err := legacyMetaSavepoint(db, "legacy_meta_fence_state", func(tx *gorm.DB) error {
		return tx.Raw(
			legacyMetaPolicyFenceQueueStateSQL,
			database.WhatsAppIdentityReviewContactSelectorFenceKey(organizationID),
			legacyMetaPolicyFenceBypassKey(organizationID),
			legacyMetaPolicyFenceWalkDepth,
		).Scan(&state).Error
	}); err != nil {
		state = legacyMetaPolicyFenceWaiting
	}
	cache.sample.Store(&legacyMetaPolicyFenceStateSample{state: state, at: time.Now()})
	return state
}

// legacyMetaSavepoint runs fn in a savepoint that it always releases, so a lost
// round or a fence check leaves no nesting level behind. GORM's nested
// Transaction never releases its savepoints, and once the transaction takes a
// row lock every open level becomes a subtransaction with its own XID; past 64
// of them a backend's subtransaction cache overflows and every snapshot in the
// cluster pays for it. A rollback to the savepoint releases what fn locked. A
// caller that is not in a transaction gets one.
func legacyMetaSavepoint(db *gorm.DB, name string, fn func(tx *gorm.DB) error) error {
	if !legacyMetaInTransaction(db) {
		return db.Transaction(fn)
	}
	if err := db.Exec("SAVEPOINT " + name).Error; err != nil {
		return err
	}
	if err := fn(db); err != nil {
		if rollbackErr := db.Exec("ROLLBACK TO SAVEPOINT " + name).Error; rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		if releaseErr := db.Exec("RELEASE SAVEPOINT " + name).Error; releaseErr != nil {
			return errors.Join(err, releaseErr)
		}
		return err
	}
	return db.Exec("RELEASE SAVEPOINT " + name).Error
}

func legacyMetaInTransaction(db *gorm.DB) bool {
	_, inTransaction := db.Statement.ConnPool.(gorm.TxCommitter)
	return inTransaction
}

// requireLegacyMetaTransaction rejects a db that is not inside a transaction:
// the exported lock helpers would otherwise return having held nothing.
func requireLegacyMetaTransaction(db *gorm.DB, what string) error {
	if !legacyMetaInTransaction(db) {
		return fmt.Errorf("%s must run inside a transaction", what)
	}
	return nil
}

// requireLegacyMetaPolicyFenceOwner checks that the transaction owns the
// organization's policy fence key, the precondition of
// EnsureLegacyMetaWhatsAppAccount: without it the caller would take the
// organization outside the queue and then wait for a busy shadow while holding
// it. The check reads the lock table, so it runs only in test binaries, which
// exercise every production caller.
func requireLegacyMetaPolicyFenceOwner(db *gorm.DB, organizationID uuid.UUID) error {
	if !testing.Testing() {
		return nil
	}
	var owned bool
	if err := db.Raw(
		`SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_locks l
			 WHERE l.pid = pg_catalog.pg_backend_pid() AND l.locktype = 'advisory'
			   AND l.mode = 'ExclusiveLock' AND l.granted AND l.objsubid = 1
			   AND l.database = (SELECT d.oid FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database())
			   AND (l.classid::int8 << 32) | l.objid::int8 = pg_catalog.hashtextextended(?, 0)
		)`,
		database.WhatsAppIdentityReviewContactSelectorFenceKey(organizationID),
	).Scan(&owned).Error; err != nil {
		return fmt.Errorf("check legacy Meta policy fence owner: %w", err)
	}
	if !owned {
		return errors.New("legacy Meta account-only bridge requires the caller to own the organization policy fence")
	}
	return nil
}

// reenterLegacyMetaPolicyFenceOrganization takes the organization row FOR
// SHARE for a transaction that already owns the policy fence, and so the row
// FOR UPDATE: it re-enters without waiting. Every other transaction must pass
// the queue first (lockLegacyMetaOrganizationShare): a plain FOR SHARE
// overtakes a waiting fence's FOR UPDATE.
func reenterLegacyMetaPolicyFenceOrganization(db *gorm.DB, organizationID uuid.UUID) error {
	return lockLegacyMetaOrganizationWith(db, organizationID, "")
}

func lockLegacyMetaOrganizationWith(db *gorm.DB, organizationID uuid.UUID, lockOptions string) error {
	var organization struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	result := db.Table("organizations").
		Clauses(clause.Locking{Strength: "SHARE", Options: lockOptions}).
		Select("id").
		Where("id = ?", organizationID).
		Limit(1).
		Scan(&organization)
	if result.Error != nil {
		return fmt.Errorf("lock legacy Meta organization: %w", result.Error)
	}
	if result.RowsAffected != 1 || organization.ID != organizationID {
		return fmt.Errorf("lock legacy Meta organization: %w", gorm.ErrRecordNotFound)
	}
	return nil
}

// joinLegacyMetaPolicyFence passes the policy fence queue. It takes the fence
// key in shared mode with try-locks. A try fails while a fence holds the key or
// waits for it, so a poller stays behind fences without joining PostgreSQL's
// lock queue, where it could not give up. With wait it polls until the limit
// that the fence's state sets (legacyMetaPolicyFenceDeadlines.limit); without
// it, it tries once and returns errLegacyMetaPolicyFenceQueued while that limit
// has not passed. Past the limit, or at once while the fence is stuck, it goes
// past the queue: it takes the bypass key and records the organization in the
// transaction-local legacyMetaPolicyFenceBypassSetting, so a later acquisition
// in the same transaction (re-entry) does not queue again. Both revert with a
// savepoint rollback, together with the organization lock they precede.
func joinLegacyMetaPolicyFence(
	db *gorm.DB,
	organizationID uuid.UUID,
	deadlines legacyMetaPolicyFenceDeadlines,
	wait bool,
) error {
	key := database.WhatsAppIdentityReviewContactSelectorFenceKey(organizationID)
	organization := organizationID.String()
	ctx := db.Statement.Context
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		var passed bool
		if err := db.Raw(
			`SELECT CASE
				WHEN pg_catalog.current_setting(?, true) = ? THEN true
				ELSE pg_catalog.pg_try_advisory_xact_lock_shared(pg_catalog.hashtextextended(?, 0))
			END`,
			legacyMetaPolicyFenceBypassSetting, organization, key,
		).Scan(&passed).Error; err != nil {
			return fmt.Errorf("join legacy Meta policy fence: %w", err)
		}
		if passed {
			return nil
		}
		state := legacyMetaPolicyFenceStateOf(db, organizationID)
		limit := deadlines.limit(state)
		if state == legacyMetaPolicyFenceStuck || !time.Now().Before(limit) {
			if err := db.Exec(
				"SELECT pg_catalog.pg_advisory_xact_lock_shared(pg_catalog.hashtextextended(?, 0)), pg_catalog.set_config(?, ?, true)",
				legacyMetaPolicyFenceBypassKey(organizationID), legacyMetaPolicyFenceBypassSetting, organization,
			).Error; err != nil {
				return fmt.Errorf("bypass legacy Meta policy fence: %w", err)
			}
			return nil
		}
		if !wait {
			return errLegacyMetaPolicyFenceQueued
		}
		timer := time.NewTimer(min(legacyMetaPolicyFencePollInterval, time.Until(limit)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("join legacy Meta policy fence: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// lockLegacyMetaOrganizationShare passes the tenant's policy fence queue
// (joinLegacyMetaPolicyFence) and then takes the organization row FOR SHARE.
// database.LockOrganizationPolicyScope takes the same advisory key exclusively
// before its FOR UPDATE, so a held or waiting fence holds back later sharers on
// every account. The row lock alone would not: a new FOR SHARE overtakes a
// waiting FOR UPDATE, and SHARE windows of mirrors on different accounts would
// overlap without end. The key wait ends at the deadlines' limit; after it the
// organization row is taken without the key. With nowait, a held or queued
// fence returns errLegacyMetaPolicyFenceQueued until that limit, and a
// conflicting row lock returns 55P03, instead of waiting.
func lockLegacyMetaOrganizationShare(
	db *gorm.DB,
	organizationID uuid.UUID,
	deadlines legacyMetaPolicyFenceDeadlines,
	nowait bool,
) error {
	if err := joinLegacyMetaPolicyFence(db, organizationID, deadlines, !nowait); err != nil {
		return err
	}
	return lockLegacyMetaOrganizationWith(db, organizationID, legacyMetaNowaitOption(nowait))
}

// legacyMetaRowWait tells a lockRows callback of
// lockLegacyMetaOrganizationThenShadow how it may wait for its rows.
type legacyMetaRowWait int

const (
	// legacyMetaRowsNowait: the organization is held; take every row NOWAIT.
	legacyMetaRowsNowait legacyMetaRowWait = iota
	// legacyMetaRowsAlone: nothing is held yet. Wait for a row only while
	// holding none of the others; once one is held, take a later busy one
	// NOWAIT and return errLegacyMetaLockRoundLost.
	legacyMetaRowsAlone
	// legacyMetaRowsInOrder: the organization is held (the fallback); wait for
	// each row in order.
	legacyMetaRowsInOrder
)

func (wait legacyMetaRowWait) lockOptions() string {
	return legacyMetaNowaitOption(wait == legacyMetaRowsNowait)
}

// waitForLegacyMetaRow waits until lock can take its row and returns holding
// nothing: it takes the row in a savepoint and rolls it back.
func waitForLegacyMetaRow(db *gorm.DB, lock func(tx *gorm.DB) error) error {
	err := legacyMetaSavepoint(db, "legacy_meta_row_wait", func(tx *gorm.DB) error {
		if err := lock(tx); err != nil {
			return err
		}
		return errLegacyMetaRowAvailable
	})
	if errors.Is(err, errLegacyMetaRowAvailable) {
		return nil
	}
	return err
}

// lockLegacyMetaOrganizationThenShadow ends owning the policy fence key in
// shared mode (or, past the queue, the bypass key), the organization row (FOR
// SHARE) and the rows lockRows takes (the shadow, the account row, or both)
// without ever waiting for one of them while it holds another, outside the
// final fallback. Waiting for the shadow while holding the organization would
// let queued holders on a busy shadow (other mirrors, or a strict reply
// holding it across a Meta call) keep LockOrganizationPolicyScope out; waiting
// for the organization while owning the shadow is the admission deadlock. Each
// round runs in savepoints, so a failed attempt releases what it took:
//
//  1. pass the fence queue and wait for the organization, then take the rows
//     NOWAIT (the usual case);
//  2. if a row is busy, wait for the rows holding nothing
//     (legacyMetaRowsAlone), then pass the fence queue and take the
//     organization NOWAIT, so the waiter that inherits the rows keeps them;
//  3. if a later row was taken again meanwhile, or a conflicting holder (a
//     held or queued policy fence, or an organization update) blocks step 2,
//     give the rows back and start the next round.
//
// Only a NOWAIT conflict loses a round; a lock_timeout on a blocking step is
// returned to the caller. After lockLegacyMetaShadowRounds lost rounds the
// caller passes the fence queue, takes the organization and then waits for the
// rows in plain order. That cannot deadlock inside PostgreSQL: a fence that
// owns the organization FOR UPDATE never coexists with this SHARE, and one
// still waiting for it does not own the rows yet. It may hold back the fence
// for one row owner.
//
// All waits for the fence key in one call share one set of deadlines, counted
// from the call's start, because the fence can in turn wait for a lock held by
// this caller's goroutine on another connection: legacyMetaPolicyFenceQueueWait
// in general, legacyMetaPolicyFenceMemberWait while the fence holds the key and
// is blocked only by members. Once the applicable one has passed, step 1 and
// the fallback try the key once and a queued fence no longer loses step 2 its
// round, so a call queues at most that long however many rounds busy rows
// cost it. Before it, step 2 gives the rows back rather than bypass, so a
// waiter that inherits them never overtakes a fence. A stuck fence is not
// queued for at all, and a transaction that already went past the queue does
// not queue again when it re-enters (a strict reply's pre-provider transaction
// mirrors after taking these locks). Every round's savepoint is released
// (legacyMetaSavepoint), so lost rounds do not deepen the transaction.
// lockRows must lock the same rows on every call. A caller that already owns
// the policy fence does not use this; it waits behind its own fence by design.
func lockLegacyMetaOrganizationThenShadow(
	db *gorm.DB,
	organizationID uuid.UUID,
	lockRows func(tx *gorm.DB, wait legacyMetaRowWait) error,
) error {
	deadlines := newLegacyMetaPolicyFenceDeadlines()
	for round := 0; round < lockLegacyMetaShadowRounds; round++ {
		rowsBusy := false
		err := legacyMetaSavepoint(db, "legacy_meta_round", func(tx *gorm.DB) error {
			if err := lockLegacyMetaOrganizationShare(tx, organizationID, deadlines, false); err != nil {
				return err
			}
			err := lockRows(tx, legacyMetaRowsNowait)
			rowsBusy = legacyMetaLockNotAvailable(err)
			return err
		})
		if err == nil || !rowsBusy {
			return err
		}
		roundLost := false
		err = legacyMetaSavepoint(db, "legacy_meta_round", func(tx *gorm.DB) error {
			if err := lockRows(tx, legacyMetaRowsAlone); err != nil {
				roundLost = errors.Is(err, errLegacyMetaLockRoundLost)
				return err
			}
			err := lockLegacyMetaOrganizationShare(tx, organizationID, deadlines, true)
			roundLost = errors.Is(err, errLegacyMetaPolicyFenceQueued) || legacyMetaLockNotAvailable(err)
			return err
		})
		if err == nil || !roundLost {
			return err
		}
	}
	if err := lockLegacyMetaOrganizationShare(db, organizationID, deadlines, false); err != nil {
		return err
	}
	return lockRows(db, legacyMetaRowsInOrder)
}

// LockLegacyMetaOrganization passes the policy fence queue, with the same
// limits as lockLegacyMetaOrganizationThenShadow, and takes the tenant
// organization row FOR SHARE, before any other lock, for a transaction that
// owns no legacy shadow. Outgoing delivery recovery uses it for every non-AI
// WhatsApp send, classic accounts included; such a send may run under an AI
// attempt fence that a waiting policy fence is itself queued behind.
func LockLegacyMetaOrganization(db *gorm.DB, organizationID uuid.UUID) error {
	if db == nil || organizationID == uuid.Nil {
		return errors.New("legacy Meta organization lock scope is required")
	}
	if err := requireLegacyMetaTransaction(db, "legacy Meta organization lock"); err != nil {
		return err
	}
	return lockLegacyMetaOrganizationShare(db, organizationID, newLegacyMetaPolicyFenceDeadlines(), false)
}

// LockLegacyMetaOrganizationAndShadow takes the organization row FOR SHARE and
// the legacy shadow FOR UPDATE for a transaction that does not own the policy
// fence and will write organization-scoped rows while it owns the shadow, using
// lockLegacyMetaOrganizationThenShadow. It locks exactly the live row that a
// later id-scoped shadow lock in the same transaction re-enters.
func LockLegacyMetaOrganizationAndShadow(
	db *gorm.DB,
	organizationID, channelAccountID uuid.UUID,
) error {
	if db == nil || organizationID == uuid.Nil || channelAccountID == uuid.Nil {
		return errors.New("legacy Meta shadow lock scope is required")
	}
	if err := requireLegacyMetaTransaction(db, "legacy Meta shadow lock"); err != nil {
		return err
	}
	return lockLegacyMetaOrganizationThenShadow(db, organizationID, func(tx *gorm.DB, wait legacyMetaRowWait) error {
		var shadows []models.ChannelAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: wait.lockOptions()}).
			Select("id").
			Where("id = ? AND organization_id = ?", channelAccountID, organizationID).
			Find(&shadows).Error; err != nil {
			return fmt.Errorf("lock legacy Meta shadow: %w", err)
		}
		return nil
	})
}

// LockLegacyMetaOrganizationAndWhatsAppAccount takes the organization row FOR
// SHARE behind the policy fence queue and then the whatsapp_accounts row FOR
// UPDATE, never waiting for the row while it holds the organization (sends
// hold it FOR SHARE across their Meta call), except in the bounded fallback of
// lockLegacyMetaOrganizationThenShadow. A transaction that updates the account
// row and then writes an audit row must take both first (see the lock order at
// the top of this file).
func LockLegacyMetaOrganizationAndWhatsAppAccount(db *gorm.DB, organizationID, accountID uuid.UUID) error {
	if db == nil || organizationID == uuid.Nil || accountID == uuid.Nil {
		return errors.New("legacy Meta account lock scope is required")
	}
	if err := requireLegacyMetaTransaction(db, "legacy Meta account lock"); err != nil {
		return err
	}
	return lockLegacyMetaOrganizationThenShadow(db, organizationID, func(tx *gorm.DB, wait legacyMetaRowWait) error {
		return lockLegacyMetaWhatsAppAccount(tx, organizationID, accountID, wait.lockOptions())
	})
}

func lockLegacyMetaWhatsAppAccount(db *gorm.DB, organizationID, accountID uuid.UUID, lockOptions string) error {
	var accounts []models.WhatsAppAccount
	if err := db.Clauses(clause.Locking{Strength: "UPDATE", Options: lockOptions}).
		Select("id").
		Where("id = ? AND organization_id = ?", accountID, organizationID).
		Find(&accounts).Error; err != nil {
		return fmt.Errorf("lock WhatsApp account: %w", err)
	}
	return nil
}

func legacyMetaNowaitOption(nowait bool) string {
	if nowait {
		return "NOWAIT"
	}
	return ""
}

func legacyMetaLockNotAvailable(err error) bool {
	var sqlState interface {
		SQLState() string
	}
	return errors.As(err, &sqlState) && sqlState.SQLState() == "55P03"
}
