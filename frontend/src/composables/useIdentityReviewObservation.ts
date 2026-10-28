import { onMounted, onScopeDispose, watch } from 'vue'
import { AUTH_SESSION_EVENT } from '@/services/api'
import { readSelectedOrganizationId } from '@/lib/browserIdentity'
import { currentTabSession, isTabSessionCurrent } from '@/lib/tabSession'
import { useAuthStore } from '@/stores/auth'
import { useOrganizationsStore } from '@/stores/organizations'

export interface IdentityReviewScope {
  organizationId: string
  userId: string
  session: string
  identity: unknown
  permissions: string
  key: string
  generation: number
}

export function useIdentityReviewObservation(options: {
  enabled: () => boolean
  query: () => unknown
  clear: () => void
  refresh: () => Promise<void>
  intervalMs?: () => number
  requireReview?: boolean
}) {
  const auth = useAuthStore()
  const organizations = useOrganizationsStore()
  let generation = 0
  let disposed = false
  let mounted = false
  let timer: ReturnType<typeof setInterval> | null = null
  let flight: symbol | null = null
  let followup = false
  let paused = false
  let observedSession: string | null = null
  const controllers = new Set<AbortController>()

  function permissions() {
    return JSON.stringify([auth.hasPermission('contacts', 'read'), auth.hasPermission('contacts', 'write'), auth.hasPermission('contacts.identity_review', 'write')])
  }

  function canReview() {
    return auth.hasPermission('contacts', 'write') && auth.hasPermission('contacts.identity_review', 'write')
  }

  function key() {
    return JSON.stringify([
      organizations.selectedOrgId || auth.organizationId || '', auth.user?.id || '',
      permissions(), options.enabled(), options.query(),
    ])
  }

  function capture(): IdentityReviewScope | null {
    const organizationId = organizations.selectedOrgId || auth.organizationId || ''
    const session = currentTabSession()
    const storedOrg = readSelectedOrganizationId()
    if (disposed || !options.enabled() || (options.requireReview && !canReview()) || !organizationId || !auth.user?.id || session === null ||
      !isTabSessionCurrent() || (storedOrg && storedOrg !== organizationId)) return null
    return { organizationId, userId: auth.user.id, session, identity: auth.user, permissions: permissions(), key: key(), generation }
  }

  function current(scope: IdentityReviewScope | null): scope is IdentityReviewScope {
    const now = capture()
    return scope !== null && now !== null && scope.generation === generation && scope.key === now.key &&
      scope.identity === now.identity && scope.userId === now.userId && scope.organizationId === now.organizationId && scope.session === now.session
  }

  // A sibling dialog has a different query/generation; only accept its event
  // while the same current identity and permission facts still own this view.
  function belongs(scope: IdentityReviewScope) {
    const now = capture()
    return now !== null && now.organizationId === scope.organizationId && now.userId === scope.userId &&
      now.session === scope.session && now.identity === scope.identity && now.permissions === scope.permissions
  }

  function controller() {
    const request = new AbortController()
    controllers.add(request)
    return request
  }

  function release(request: AbortController) { controllers.delete(request) }

  function clear() {
    generation++
    for (const request of controllers) request.abort()
    controllers.clear()
    flight = null
    followup = false
    paused = false
    options.clear()
  }

  async function refresh(force = false) {
    if (!mounted || disposed || document.visibilityState === 'hidden' || (!force && paused) || !capture()) return
    if (force) paused = false
    if (flight) { if (force) followup = true; return }
    const token = Symbol('review-observation')
    flight = token
    try { await options.refresh() } finally {
      if (flight === token) {
        flight = null
        if (followup) { followup = false; void refresh() }
      }
    }
  }

  function pause() { paused = true; followup = false }

  function stopTimer() {
    if (timer !== null) clearInterval(timer)
    timer = null
  }

  function startTimer() {
    stopTimer()
    const interval = options.intervalMs?.() ?? 60_000
    if (mounted && options.enabled() && document.visibilityState !== 'hidden' && interval > 0) {
      timer = setInterval(() => { void refresh() }, interval)
    }
  }

  function resample() {
    const session = currentTabSession()
    if (session !== observedSession || !capture()) {
      observedSession = session
      clear()
    }
    if (document.visibilityState === 'hidden') { clear(); stopTimer(); return }
    startTimer()
    void refresh()
  }

  watch(() => [auth.user, key()] as const, () => {
    clear()
    observedSession = currentTabSession()
    startTimer()
    void refresh()
  }, { flush: 'sync' })

  onMounted(() => {
    mounted = true
    observedSession = currentTabSession()
    document.addEventListener('visibilitychange', resample)
    window.addEventListener('focus', resample)
    window.addEventListener('storage', resample)
    window.addEventListener(AUTH_SESSION_EVENT, resample)
    window.addEventListener('pagehide', clear)
    startTimer()
    void refresh()
  })

  onScopeDispose(() => {
    disposed = true
    clear()
    stopTimer()
    document.removeEventListener('visibilitychange', resample)
    window.removeEventListener('focus', resample)
    window.removeEventListener('storage', resample)
    window.removeEventListener(AUTH_SESSION_EVENT, resample)
    window.removeEventListener('pagehide', clear)
  })

  return { capture, current, belongs, canReview, controller, release, refresh, pause, invalidate: clear }
}
