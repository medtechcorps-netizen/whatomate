// Shared state for the organization policy "Sign out after inactivity".
//
// The last-activity time and the last-known policy live in localStorage, so
// every tab sees the same clock and a page load can end a stale session
// before the app renders anything. The policy itself arrives on the
// X-Session-Idle-Minutes header of GET /api/me. It is cached per user and
// organization, so a policy learned for one never applies to another.

// Allowed values in minutes; 0 turns the policy off.
export const SESSION_IDLE_TIMEOUT_OPTIONS = [0, 30, 60, 120, 240, 480] as const
export const DEFAULT_SESSION_IDLE_TIMEOUT_MINUTES = 240

export const LAST_ACTIVITY_KEY = 'whm_last_activity'
export const SESSION_IDLE_POLICY_KEY = 'whm_session_idle_minutes'
export const SIGNED_OUT_BROADCAST_KEY = 'whm_signed_out_at'
export const SESSION_CHANNEL_NAME = 'whm-session'
/**
 * Window event fired with `{ minutes, identity }` when this tab learns a new
 * policy for the identity it is signed in as.
 */
export const SESSION_POLICY_EVENT = 'whm:session-policy'
/**
 * Window event fired synchronously right before a session's identity is
 * cleared and the app leaves for the login page, so views can drop unsaved-
 * work guards and abort uploads first (contract with the chat composer).
 * This is its only definition; services/api.ts fires it, once per end.
 */
export const SESSION_ENDING_EVENT = 'rereply:session-ending'

export function announceSessionEnding() {
  try {
    window.dispatchEvent(new CustomEvent(SESSION_ENDING_EVENT))
  } catch {
    // Listeners are optional; the sign-out goes ahead regardless.
  }
}

export function normalizeSessionIdleTimeout(value: unknown): number {
  const minutes = typeof value === 'string' && value.trim() !== '' ? Number(value) : value
  return typeof minutes === 'number' &&
    (SESSION_IDLE_TIMEOUT_OPTIONS as readonly number[]).includes(minutes)
    ? minutes
    : DEFAULT_SESSION_IDLE_TIMEOUT_MINUTES
}

function readNumber(key: string): number | null {
  try {
    const raw = localStorage.getItem(key)
    if (raw === null || raw === '') return null
    const value = Number(raw)
    return Number.isFinite(value) ? value : null
  } catch {
    return null
  }
}

function write(key: string, value: string | null) {
  try {
    if (value === null) localStorage.removeItem(key)
    else localStorage.setItem(key, value)
  } catch {
    // Each tab still tracks its own state.
  }
}

// Policies of the most recent identities this browser has seen.
const MAX_CACHED_POLICIES = 8

/**
 * The identity a policy belongs to: "<user id>|<organization id>" of the
 * signed-in user, with the organization the session's own /me names (the
 * token's; also for a super admin viewing another organization), the same key
 * the policy is cached under. "" when nobody is signed in.
 */
export function currentPolicyIdentity(): string {
  try {
    const user = JSON.parse(localStorage.getItem('user') || 'null')
    if (!user?.id) return ''
    return `${user.id}|${user.organization_id || ''}`
  } catch {
    return ''
  }
}

function readPolicies(): Record<string, number> {
  try {
    const parsed = JSON.parse(localStorage.getItem(SESSION_IDLE_POLICY_KEY) || 'null')
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed : {}
  } catch {
    return {}
  }
}

/**
 * The last-known policy in minutes for an identity (the signed-in one by
 * default), or the 4 hour default until one is known.
 */
export function readSessionIdlePolicy(identity = currentPolicyIdentity()): number {
  const policies = readPolicies()
  return normalizeSessionIdleTimeout(
    Object.prototype.hasOwnProperty.call(policies, identity) ? policies[identity] : null,
  )
}

/**
 * Caches a policy learned from the server for the identity it belongs to,
 * and tells this tab about it if that is the identity it is signed in as.
 */
export function rememberSessionIdlePolicy(value: unknown, identity: string) {
  if (value === null || value === undefined || value === '' || !identity) return
  const minutes = normalizeSessionIdleTimeout(value)
  const policies = readPolicies()
  if (policies[identity] !== minutes) {
    delete policies[identity]
    policies[identity] = minutes
    const kept = Object.keys(policies).slice(-MAX_CACHED_POLICIES)
    write(
      SESSION_IDLE_POLICY_KEY,
      JSON.stringify(Object.fromEntries(kept.map((key) => [key, policies[key]]))),
    )
  }
  if (identity !== currentPolicyIdentity()) return
  try {
    window.dispatchEvent(new CustomEvent(SESSION_POLICY_EVENT, { detail: { minutes, identity } }))
  } catch {
    // Observers are optional.
  }
}

/** The shared last-activity time (ms), or 0 when none is recorded. */
export function readLastActivity(): number {
  const value = readNumber(LAST_ACTIVITY_KEY)
  return value && value > 0 ? value : 0
}

export function recordActivity(at = Date.now()) {
  write(LAST_ACTIVITY_KEY, String(Math.max(at, readLastActivity())))
}

/** Forgets the clock when a session ends, so the next one starts fresh. */
export function clearLastActivity() {
  write(LAST_ACTIVITY_KEY, null)
}

/**
 * Whether the recorded activity is older than the policy allows. A session
 * with no recorded activity is not stale: its clock starts now.
 */
export function isIdleSessionStale(now = Date.now(), minutes = readSessionIdlePolicy()): boolean {
  const last = readLastActivity()
  return minutes > 0 && last > 0 && now - last >= minutes * 60_000
}
