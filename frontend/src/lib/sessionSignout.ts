// Why the last session ended, shown on the login page. Stored in
// localStorage so the tab that ends the session and every other open tab land
// on a login page that can explain it. The next sign-in clears it.

export const SIGN_OUT_REASON_KEY = 'whm_signout_reason'
// A reason older than this is stale (e.g. the browser was left on the login
// page) and is dropped instead of shown.
const SIGN_OUT_REASON_TTL_MS = 10 * 60 * 1000

export type SignOutReason =
  | { reason: 'idle'; minutes: number; at: number }
  | { reason: 'expired'; at: number }

// An inactivity sign-out ends the session in every tab; a refresh that fails
// in another tab moments later must not relabel it as "expired".
const IDLE_REASON_PRECEDENCE_MS = 60 * 1000

export function recordSignOutReason(reason: { reason: 'idle'; minutes: number } | { reason: 'expired' }) {
  try {
    if (reason.reason === 'expired') {
      const existing = JSON.parse(localStorage.getItem(SIGN_OUT_REASON_KEY) || 'null')
      if (existing?.reason === 'idle' && Date.now() - Number(existing.at) < IDLE_REASON_PRECEDENCE_MS) return
    }
    localStorage.setItem(SIGN_OUT_REASON_KEY, JSON.stringify({ ...reason, at: Date.now() }))
  } catch {
    // The banner is informational; a denied storage backend just skips it.
  }
}

export function clearSignOutReason() {
  try {
    localStorage.removeItem(SIGN_OUT_REASON_KEY)
  } catch {
    // Nothing to clear.
  }
}

export function readSignOutReason(now = Date.now()): SignOutReason | null {
  let raw: string | null = null
  try {
    raw = localStorage.getItem(SIGN_OUT_REASON_KEY)
  } catch {
    return null
  }
  if (!raw) return null
  try {
    const parsed = JSON.parse(raw)
    if (!parsed || typeof parsed.at !== 'number' || now - parsed.at > SIGN_OUT_REASON_TTL_MS) {
      clearSignOutReason()
      return null
    }
    if (parsed.reason === 'expired') return { reason: 'expired', at: parsed.at }
    if (parsed.reason === 'idle' && typeof parsed.minutes === 'number' && parsed.minutes > 0) {
      return { reason: 'idle', minutes: parsed.minutes, at: parsed.at }
    }
  } catch {
    // Ignore malformed values.
  }
  return null
}

type Translate = (key: string, params?: Record<string, unknown>) => string

/** "30 minutes", "1 hour", "4 hours" for an inactivity timeout in minutes. */
export function formatIdleDuration(minutes: number, t: Translate): string {
  if (minutes < 60) return t('sessionSecurity.durationMinutes', { count: minutes })
  const hours = minutes / 60
  return hours === 1
    ? t('sessionSecurity.durationHour')
    : t('sessionSecurity.durationHours', { count: hours })
}
