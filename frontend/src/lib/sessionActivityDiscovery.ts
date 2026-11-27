import {
  SESSION_CHANNEL_NAME, activityUserIdentity, currentSessionActivityScope,
  readSessionIdlePolicy, rememberSessionActivity, type ActivityScope,
} from './sessionActivity'

const DISCOVERY_MS = 500
const BURST_INTERVAL_MS = 5_000
const MAX_REPLIES_PER_INTERVAL = 4
const MAX_RECENT_NONCES = 8
const NONCE = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
let pending: { decision: Promise<boolean>; cancel: () => void } | null = null

export function pendingIdleStartup(): Promise<boolean> | null {
  return pending?.decision ?? null
}

export function cancelIdleStartup() {
  pending?.cancel()
}

// Called once by the API at cold startup. Closing/releasing the gate happens
// synchronously before the ordinary stale decision, which can itself end the session.
export function discoverPeerActivity(scope: ActivityScope, isCurrent: () => boolean, decide: () => boolean): boolean {
  let nonce: string
  let channel: BroadcastChannel
  try {
    nonce = crypto.randomUUID()
    channel = new BroadcastChannel(SESSION_CHANNEL_NAME)
  } catch {
    return false
  }
  const deadline = performance.now() + DISCOVERY_MS
  let resolve!: (admitted: boolean) => void
  const decision = new Promise<boolean>(done => { resolve = done })
  let settled = false
  const timer = setTimeout(() => finish(false), DISCOVERY_MS)
  function finish(cancelled: boolean) {
    if (settled) return
    settled = true
    clearTimeout(timer)
    channel.onmessage = null
    try { channel.close() } catch { /* delivery has already stopped */ }
    pending = null
    resolve(!cancelled && decide())
  }
  pending = { decision, cancel: () => finish(true) }
  channel.onmessage = ({ data }) => {
    if (settled) return
    if (performance.now() >= deadline || !isCurrent()) return finish(false)
    if (data?.type !== 'activity-reply' || data.nonce !== nonce ||
        data.session !== scope.session || data.identity !== scope.identity) return
    const minutes = readSessionIdlePolicy(scope.identity)
    if (typeof data.at !== 'number' || (minutes > 0 && Date.now() - data.at >= minutes * 60_000)) return
    if (rememberSessionActivity(data.at, scope.session, scope.identity)) finish(false)
  }
  try {
    channel.postMessage({ type: 'activity-query', nonce, ...scope })
  } catch {
    finish(false)
  }
  return true
}

type User = { id?: unknown; organization_id?: unknown } | null
type ObservedActivity = ActivityScope & { at: number }
let lastLocalObservation: ObservedActivity | null = null
const RELOAD_ACTIVITY = 'rereplyWorkspaceReloadActivity'

// The switcher deliberately reloads this tab. Carry its actual input/call time
// across that one reload even when the shared activity write was denied.
export function prepareWorkspaceActivityReload(user: User, organizationId: string): (() => void) | undefined {
  const source = currentSessionActivityScope()
  const observed = lastLocalObservation
  const destination = activityUserIdentity({ id: user?.id, organization_id: organizationId })
  if (!source || !observed || !destination || source.identity !== activityUserIdentity(user) ||
      observed.session !== source.session || observed.identity !== source.identity) return
  return () => {
    const current = currentSessionActivityScope()
    if (current?.session !== source.session || current.identity !== destination) return
    try {
      history.replaceState({ ...history.state, [RELOAD_ACTIVITY]: { ...current, at: observed.at } }, '')
    } catch { /* The ordinary protected startup check still runs. */ }
  }
}

export function consumeWorkspaceReloadActivity() {
  try {
    if (!history.state || !(RELOAD_ACTIVITY in history.state)) return
    const { [RELOAD_ACTIVITY]: activity, ...routeState } = history.state
    history.replaceState(routeState, '')
    const [navigation] = performance.getEntriesByType('navigation') as PerformanceNavigationTiming[]
    if (navigation?.type === 'reload') rememberSessionActivity(activity?.at, activity?.session, activity?.identity)
  } catch { /* Without a usable, consumed entry the normal clock applies. */ }
}

/** Each mounted responder has a bounded duplicate window and a fixed burst budget. */
export function createActivityResponder() {
  const recent: string[] = []
  const replies: number[] = []
  let observed: ObservedActivity | null = null
  function adoptedScope(user: User): ActivityScope | null {
    const scope = currentSessionActivityScope()
    return scope && scope.identity === activityUserIdentity(user) ? scope : null
  }
  function record(at: number, user: User) {
    const scope = adoptedScope(user)
    if (scope) {
      observed = { ...scope, at }
      lastLocalObservation = observed
    }
  }
  function reply(data: { nonce?: unknown; session?: unknown; identity?: unknown }, channel: BroadcastChannel, user: User) {
    const scope = adoptedScope(user)
    if (!scope || !observed || observed.identity !== scope.identity || observed.session !== scope.session ||
        scope.session !== data.session || scope.identity !== data.identity ||
        typeof data.nonce !== 'string' || data.nonce.length !== 36 || !NONCE.test(data.nonce) ||
        recent.includes(data.nonce)) return
    const at = observed.at
    if (at > Date.now()) return
    const now = performance.now()
    while (replies.length && now - replies[0]! >= BURST_INTERVAL_MS) replies.shift()
    if (replies.length >= MAX_REPLIES_PER_INTERVAL) return
    if (recent.length === MAX_RECENT_NONCES) recent.shift()
    recent.push(data.nonce)
    replies.push(now)
    try { channel.postMessage({ type: 'activity-reply', nonce: data.nonce, ...scope, at }) } catch { /* unavailable */ }
  }
  return { record, reply }
}
