package channel

import "time"

// SetLegacyMetaPolicyFenceQueueWaitForTest replaces how long a sharer queues
// for the policy fence key and returns a function that restores it. Only
// sequential tests may call it.
func SetLegacyMetaPolicyFenceQueueWaitForTest(wait time.Duration) func() {
	previous := legacyMetaPolicyFenceQueueWait
	legacyMetaPolicyFenceQueueWait = wait
	return func() { legacyMetaPolicyFenceQueueWait = previous }
}

// SetLegacyMetaPolicyFenceMemberWaitForTest replaces how long a sharer queues
// behind a fence that is blocked only by members, and returns a function that
// restores it. Only sequential tests may call it.
func SetLegacyMetaPolicyFenceMemberWaitForTest(wait time.Duration) func() {
	previous := legacyMetaPolicyFenceMemberWait
	legacyMetaPolicyFenceMemberWait = wait
	return func() { legacyMetaPolicyFenceMemberWait = previous }
}

// ForgetLegacyMetaPolicyFenceStatesForTest drops the process cache of fence
// states, so the next check reads the lock table. State-sensitive tests must
// call this after establishing their blocker chain and before measuring it.
func ForgetLegacyMetaPolicyFenceStatesForTest() {
	legacyMetaPolicyFenceStates.Clear()
}

// LegacyMetaPolicyFenceStateQueriesForTest reports how many fence state
// queries this process has run.
func LegacyMetaPolicyFenceStateQueriesForTest() int64 {
	return legacyMetaPolicyFenceStateQueries.Load()
}

// SetLegacyMetaPolicyFenceStateTTLForTest replaces how long a fence state is
// reused and returns a function that restores it. Only sequential tests may
// call it.
func SetLegacyMetaPolicyFenceStateTTLForTest(ttl time.Duration) func() {
	previous := legacyMetaPolicyFenceStateTTL
	legacyMetaPolicyFenceStateTTL = ttl
	return func() { legacyMetaPolicyFenceStateTTL = previous }
}
