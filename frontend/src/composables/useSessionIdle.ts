import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import {
  checkSessionIdlePolicy,
  endSessionInBackground,
  keepSessionAlive,
  isTabSessionCurrent,
  markSessionEnding,
  readSessionId,
  readTabSession,
} from '@/services/api'
import { useAuthStore } from '@/stores/auth'
import { useCallingStore } from '@/stores/calling'
import { recordSignOutReason } from '@/lib/sessionSignout'
import { SELECTED_ORGANIZATION_STORAGE_KEY } from '@/lib/browserIdentity'
import {
  SESSION_CHANNEL_NAME,
  SESSION_IDLE_POLICY_KEY,
  SESSION_POLICY_EVENT,
  SIGNED_OUT_BROADCAST_KEY,
  clearLastActivity,
  currentPolicyIdentity,
  currentSessionActivityScope,
  readLastActivity,
  readSessionIdlePolicy,
  recordActivity,
  rememberSessionActivity,
} from '@/lib/sessionActivity'

export {
  DEFAULT_SESSION_IDLE_TIMEOUT_MINUTES,
  LAST_ACTIVITY_KEY,
  SESSION_IDLE_TIMEOUT_OPTIONS,
  normalizeSessionIdleTimeout,
} from '@/lib/sessionActivity'

/**
 * Organization policy "Sign out after inactivity".
 *
 * Real interaction (pointer, keyboard, touch, a tab becoming visible) or an
 * active call counts as activity, in any tab: the last-activity time is
 * shared through storage and session-scoped notices. Loading the app is not activity;
 * a session already idle past the policy ends before the app renders (see
 * services/api.ts). Two minutes before the deadline every tab shows a
 * warning; from then on only "Stay signed in" keeps the session. At the
 * deadline the identity is cleared and every tab returns to the login page at
 * once, while the logout runs in the background.
 *
 * A sign-out notice to the other tabs names the logical session it ends
 * (services/api.ts readSessionId). A tab acts only on a notice about the
 * session it shows (readTabSession), so a late notice never signs out a newer
 * login. If the browser has already moved on to another session, the tab
 * leaves without touching the shared state, which belongs to that session.
 */

export const SESSION_IDLE_WARNING_MS = 2 * 60 * 1000
export { SIGNED_OUT_BROADCAST_KEY, SESSION_CHANNEL_NAME }

// Pointer moves are frequent; other tabs only need the shared time to within
// a few seconds.
const ACTIVITY_SHARE_INTERVAL_MS = 5_000
const TICK_MS = 1_000
const POLICY_RECHECK_MS = 15 * 60 * 1000
// How long to let another tab's revoke land before checking this session.
const SIGN_OUT_VERIFY_DELAY_MS = 3_000
const ACTIVITY_EVENTS = ['pointerdown', 'pointermove', 'keydown', 'wheel', 'touchstart'] as const

type SignOutReason = 'idle' | 'manual'

function loginPath() {
  const basePath = ((window as any).__BASE_PATH__ ?? '').replace(/\/$/, '')
  return basePath + '/login'
}

export function useSessionIdle() {
  const authStore = useAuthStore()
  let callingStore: ReturnType<typeof useCallingStore> | null = null
  try {
    callingStore = useCallingStore()
  } catch {
    // Without a calling store there are no calls to count.
  }

  const timeoutMinutes = ref<number>(readSessionIdlePolicy())
  const warningOpen = ref(false)
  const remainingMs = ref(0)

  let localLastActivity = 0
  let lastSharedAt = 0
  let lastBroadcastAt = 0
  let signingOut = false
  let tickTimer: ReturnType<typeof setInterval> | null = null
  let policyTimer: ReturnType<typeof setInterval> | null = null
  let lastPolicyCheck = Date.now()
  let channel: BroadcastChannel | null = null
  let verifyTimer: ReturnType<typeof setTimeout> | null = null
  let verifySession: string | null = null

  const countdown = computed(() => {
    const seconds = Math.max(0, Math.ceil(remainingMs.value / 1000))
    return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`
  })

  function isActive() {
    return timeoutMinutes.value > 0 && authStore.isAuthenticated && !signingOut
  }

  function lastActivity() {
    return Math.max(localLastActivity, readLastActivity())
  }

  function shareActivity(at: number) {
    // An ending or replaced tab cannot restart any browser session's clock.
    if (signingOut || !authStore.isAuthenticated || !isTabSessionCurrent()) return
    localLastActivity = Math.max(localLastActivity, at)
    lastSharedAt = at
    recordActivity(localLastActivity)
    const scope = currentSessionActivityScope()
    const now = Date.now()
    // Page-hide/unmount flushes still persist promptly, but cannot turn rapid
    // visibility changes into an unbounded stream of cross-tab notices.
    if (!channel || !scope || now - lastBroadcastAt < ACTIVITY_SHARE_INTERVAL_MS) return
    lastBroadcastAt = now
    try {
      channel.postMessage({ type: 'activity', ...scope, at: localLastActivity })
    } catch {
      // Storage and this tab's scoped memory remain independent fallbacks.
    }
  }

  function remaining(now: number) {
    return timeoutMinutes.value * 60_000 - (now - lastActivity())
  }

  function announceSignOut(reason: SignOutReason, session: string) {
    try {
      channel?.postMessage({ type: 'signed-out', reason, session })
    } catch {
      // The storage key below still reaches the other tabs.
    }
    try {
      localStorage.setItem(
        SIGNED_OUT_BROADCAST_KEY,
        JSON.stringify({ reason, session, at: Date.now() }),
      )
    } catch {
      // Other tabs sign out at their own deadline instead.
    }
  }

  // Clears the identity and leaves for the login page at once; the server
  // session ends in the background.
  function signOut(reason: SignOutReason) {
    if (signingOut) return
    signingOut = true
    warningOpen.value = false
    // A tab that no longer shows the current session (another sign-in replaced
    // it) leaves locally: no end, no notice, nothing shared touched.
    const session = readTabSession()
    if (session === null || !isTabSessionCurrent()) {
      markSessionEnding()
      window.location.href = loginPath()
      return
    }
    if (reason === 'idle') {
      recordSignOutReason({ reason: 'idle', minutes: timeoutMinutes.value })
    }
    markSessionEnding()
    announceSignOut(reason, session)
    clearLastActivity()
    endSessionInBackground()
    authStore.clearAuth()
    window.location.href = loginPath()
  }

  // Another tab signed out of `session`. A notice for any other session (one
  // that arrives after a newer login) is ignored. An inactivity sign-out is
  // honoured at once only if this tab's view of the shared clock agrees: this
  // tab may have activity that a tab with failing storage never saw.
  // Otherwise the session is checked shortly, after the other tab's revoke
  // has landed; if it was revoked, the API client ends it cleanly instead of
  // leaving this tab on a dead session. That check is for the same session
  // only.
  function onSignedOutElsewhere(reason: unknown, session: unknown) {
    if (signingOut || !authStore.isAuthenticated) return
    // Only an end of the session this tab shows concerns it.
    if (typeof session !== 'string' || session !== readTabSession()) return
    if (readSessionId() !== session) {
      // A newer sign-in has replaced it meanwhile (this tab was frozen): leave
      // this tab only. The shared identity and settings are the newer
      // session's now, so nothing shared is cleared and nothing announced.
      signingOut = true
      warningOpen.value = false
      markSessionEnding()
      window.location.href = loginPath()
      return
    }
    if (reason === 'idle' && timeoutMinutes.value > 0 && remaining(Date.now()) > 0) {
      if (verifyTimer !== null && verifySession === session) return
      if (verifyTimer !== null) clearTimeout(verifyTimer)
      verifySession = session
      verifyTimer = setTimeout(() => {
        verifyTimer = null
        verifySession = null
        if (signingOut || readTabSession() !== session || readSessionId() !== session) return
        void keepSessionAlive().catch(() => undefined)
      }, SIGN_OUT_VERIFY_DELAY_MS)
      return
    }
    signingOut = true
    warningOpen.value = false
    markSessionEnding()
    authStore.clearAuth()
    window.location.href = loginPath()
  }

  function isOnCall() {
    try {
      return !!callingStore?.isOnCall
    } catch {
      return false
    }
  }

  function tick() {
    if (!isActive()) {
      warningOpen.value = false
      return
    }
    const now = Date.now()
    // A call in progress keeps the session alive.
    if (isOnCall()) {
      localLastActivity = now
      warningOpen.value = false
      if (now - lastSharedAt >= ACTIVITY_SHARE_INTERVAL_MS) shareActivity(now)
      return
    }
    const left = remaining(now)
    if (left <= 0) {
      signOut('idle')
      return
    }
    remainingMs.value = left
    warningOpen.value = left <= SESSION_IDLE_WARNING_MS
  }

  function onActivity() {
    if (!isActive()) return
    const now = Date.now()
    // Activity after the deadline (say, waking a laptop that slept through
    // it) must not revive the session.
    if (remaining(now) <= 0) {
      signOut('idle')
      return
    }
    // Once the warning is up, only "Stay signed in" keeps the session.
    if (warningOpen.value) return
    localLastActivity = now
    if (now - lastSharedAt >= ACTIVITY_SHARE_INTERVAL_MS) shareActivity(now)
  }

  function flushActivity() {
    if (!signingOut && localLastActivity > lastSharedAt) shareActivity(localLastActivity)
  }

  function checkPolicy() {
    lastPolicyCheck = Date.now()
    void checkSessionIdlePolicy()
  }

  function onVisibilityChange() {
    if (document.visibilityState === 'hidden') {
      flushActivity()
      return
    }
    onActivity()
    if (Date.now() - lastPolicyCheck >= POLICY_RECHECK_MS) checkPolicy()
  }

  function applyPolicy(minutes: number) {
    const previous = timeoutMinutes.value
    if (minutes === previous) return
    timeoutMinutes.value = minutes
    // A policy switched on starts its clock now, not from an old timestamp.
    // Changing an active policy keeps the clock it already has.
    if (previous === 0 && minutes > 0) shareActivity(Date.now())
    tick()
  }

  function onPolicyEvent(event: Event) {
    const detail = (event as CustomEvent<{ minutes?: number; identity?: string }>).detail
    if (typeof detail?.minutes === 'number' && detail.identity === currentPolicyIdentity()) {
      applyPolicy(detail.minutes)
    }
  }

  function onStorage(event: StorageEvent) {
    if (event.key === SIGNED_OUT_BROADCAST_KEY && event.newValue) {
      let notice: { reason?: unknown; session?: unknown } | null = null
      try {
        notice = JSON.parse(event.newValue)
      } catch {
        // Not a notice this version wrote: it names no session.
      }
      onSignedOutElsewhere(notice?.reason ?? 'manual', notice?.session)
    } else if (
      event.key === SESSION_IDLE_POLICY_KEY ||
      event.key === 'user' ||
      event.key === SELECTED_ORGANIZATION_STORAGE_KEY
    ) {
      // A policy learned, or the user or organization changed in another tab
      // (an org switch): apply the policy cached for who this tab now is. No
      // identity stored (a sign-in attempt elsewhere cleared it, and may still
      // fail) is no new policy: keep the current one.
      if (currentPolicyIdentity() === '') return
      applyPolicy(readSessionIdlePolicy())
    }
  }

  async function staySignedIn() {
    if (signingOut) return
    warningOpen.value = false
    shareActivity(Date.now())
    try {
      await keepSessionAlive()
    } catch {
      // The session itself is handled by the API client.
    }
  }

  function signOutNow() {
    signOut('manual')
  }

  onMounted(() => {
    // Loading the app is not activity. Only a session with no clock yet (a
    // fresh sign-in on this browser) starts one now; a stale clock signs out.
    if (readLastActivity() === 0) {
      shareActivity(Date.now())
    } else {
      localLastActivity = readLastActivity()
      lastSharedAt = localLastActivity
      if (isActive() && remaining(Date.now()) <= 0) {
        signOut('idle')
        return
      }
    }
    tickTimer = setInterval(tick, TICK_MS)
    policyTimer = setInterval(() => {
      if (document.visibilityState !== 'hidden') checkPolicy()
    }, POLICY_RECHECK_MS)
    for (const type of ACTIVITY_EVENTS) {
      window.addEventListener(type, onActivity, { capture: true, passive: true })
    }
    document.addEventListener('visibilitychange', onVisibilityChange)
    window.addEventListener('pagehide', flushActivity)
    window.addEventListener('storage', onStorage)
    window.addEventListener(SESSION_POLICY_EVENT, onPolicyEvent)
    if (typeof BroadcastChannel !== 'undefined') {
      try {
        channel = new BroadcastChannel(SESSION_CHANNEL_NAME)
        channel.onmessage = (event: MessageEvent) => {
          if (event.data?.type === 'signed-out') {
            onSignedOutElsewhere(event.data.reason, event.data.session)
          } else if (event.data?.type === 'activity' && isActive() && isTabSessionCurrent() &&
                     event.data.session === readTabSession() &&
                     rememberSessionActivity(event.data.at, event.data.session, event.data.identity)) {
            // Memory only: do not persist, refresh, or rebroadcast a received
            // notice. The API client reads this same scoped clock.
            tick()
          }
        }
      } catch {
        channel = null
      }
    }
  })

  watch(
    () => authStore.organizationId,
    (next, previous) => {
      if (next && previous && next !== previous) {
        // The new organization's last-known policy (or the default) applies
        // until its own /me arrives; the old one's never carries over.
        applyPolicy(readSessionIdlePolicy())
        checkPolicy()
      }
    },
  )

  onBeforeUnmount(() => {
    flushActivity()
    if (tickTimer !== null) clearInterval(tickTimer)
    if (policyTimer !== null) clearInterval(policyTimer)
    if (verifyTimer !== null) clearTimeout(verifyTimer)
    tickTimer = null
    policyTimer = null
    verifyTimer = null
    verifySession = null
    for (const type of ACTIVITY_EVENTS) {
      window.removeEventListener(type, onActivity, { capture: true })
    }
    document.removeEventListener('visibilitychange', onVisibilityChange)
    window.removeEventListener('pagehide', flushActivity)
    window.removeEventListener('storage', onStorage)
    window.removeEventListener(SESSION_POLICY_EVENT, onPolicyEvent)
    try {
      channel?.close()
    } catch {
      // Already closed.
    }
    channel = null
  })

  return {
    timeoutMinutes,
    warningOpen,
    remainingMs,
    countdown,
    staySignedIn,
    signOutNow,
  }
}
