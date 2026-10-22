import type { BrowserContext, Page, Request, Response, TestInfo } from '@playwright/test'

// Diagnostic only. Never wraps refresh, storage, Web Locks or the test oracle.
type Milestone = 'counter-installed' | 'ready-before-wake' | 'wake-returned' | 'poll-passed' | 'case-failed' | 'case-passed'
type Row = Record<string, string | number | boolean | null>
const BINDING = '__r3LongStayObservation'
const SNAPSHOT = '__r3LongStaySnapshot'

export function createLongStayObserver(context: BrowserContext, provider: Page) {
  const started = performance.now()
  const events: Row[] = []
  const issues: string[] = []
  const pages = new Map<Page, { id: number; label: string }>()
  const requests = new Map<Request, number>()
  const pending = new Set<Promise<void>>()
  let bytes = 0
  let dropped = 0
  let closed = false
  let installed = false
  let origin: string | undefined
  let snapshotCalls = 0
  let browserCapped = false
  const issue = (code: string) => { if (issues.length < 16) issues.push(code); else dropped++ }
  const add = (row: Row) => {
    if (closed) return
    const entry = { seq: events.length, received_ms: performance.now() - started, ...row }
    const size = Buffer.byteLength(JSON.stringify(entry))
    if (events.length >= 512 || bytes + size > 96 * 1024) { dropped++; return }
    bytes += size
    events.push(entry)
  }
  const register = (page: Page, label = 'additional-page') => {
    const known = pages.get(page)
    if (known) return known
    if (pages.size >= 8) { issue('page-cap'); return { id: 0, label: 'unavailable' } }
    const info = { id: pages.size + 1, label }
    pages.set(page, info)
    return info
  }
  register(provider, 'provider-return')
  const pageCreated = (page: Page) => { register(page) }
  context.on('page', pageCreated)
  const requestStarted = (request: Request) => {
    try {
      const url = new URL(request.url())
      if (!origin || url.origin !== origin) return
      const endpoints: Record<string, string> = {
        '/api/auth/refresh': 'refresh', '/api/auth/login': 'login',
        '/api/auth/logout': 'logout', '/api/auth/sso/google/init': 'sso-init', '/api/me': 'me',
      }
      const endpoint = endpoints[url.pathname]
      if (!endpoint) return
      if (requests.size >= 128) { issue('request-cap'); return }
      let tab = { id: 0, label: 'unavailable' }
      try { tab = register(request.frame().page()) } catch { /* A worker has no page frame. */ }
      let kind = endpoint
      if (endpoint === 'refresh') {
        const body = request.postData() ?? ''
        kind = body.includes('"revoke"') ? 'revoke' : body.includes('"sso_exchange"') ? 'sso-exchange' : 'refresh'
      }
      const id = requests.size + 1
      requests.set(request, id)
      add({ action: 'request-start', id, tab: tab.id, page: tab.label, endpoint, kind,
        matches_existing_refresh_predicate: request.url().endsWith('/api/auth/refresh') && !(request.postData() ?? '').includes('"revoke"'),
        method: ['GET', 'POST'].includes(request.method()) ? request.method() : 'other' })
    } catch { issue('request-observation-unavailable') }
  }
  const response = (value: Response) => {
    const id = requests.get(value.request())
    if (id !== undefined) add({ action: 'response', id, status: value.status() })
  }
  const finished = (request: Request) => {
    const id = requests.get(request)
    if (id !== undefined) add({ action: 'request-finished', id })
  }
  const failed = (request: Request) => {
    const id = requests.get(request)
    // Do not copy arbitrary failure text (it may contain URLs).
    if (id !== undefined) add({ action: 'request-failed', id })
  }
  context.on('request', requestStarted)
  context.on('response', response)
  context.on('requestfinished', finished)
  context.on('requestfailed', failed)

  return {
    async install(baseURL: string | undefined) {
      if (!baseURL) throw new Error('Long-stay observer requires an explicit fixture base URL')
      origin = new URL(baseURL).origin
      await context.exposeBinding(BINDING, ({ page, frame }, value: unknown) => {
        if (closed || frame !== page.mainFrame()) return
        if (!value || typeof value !== 'object') { issue('browser-shape'); return }
        const row = value as Record<string, unknown>
        const enums: Record<string, readonly string[]> = {
          action: ['document', 'visibility', 'focus', 'blur', 'storage', 'pagehide', 'pageshow', 'snapshot', 'locks', 'browser-cap'],
          milestone: ['none', 'counter-installed', 'ready-before-wake', 'wake-returned', 'poll-passed', 'case-failed', 'case-passed'],
          visibility: ['visible', 'hidden', 'unknown'], expiry: ['due', 'future', 'absent', 'unknown'],
          marker: ['live', 'ended', 'absent', 'unknown'],
          lock_state: ['not-requested', 'observed', 'unsupported', 'timeout', 'query-failed', 'busy', 'too-many'],
        }
        const safe: Row = {}
        for (const [key, allowed] of Object.entries(enums)) {
          if (typeof row[key] !== 'string' || !allowed.includes(row[key] as string)) { issue('browser-enum'); return }
          safe[key] = row[key] as string
        }
        for (const key of ['document_origin_ms', 'document_ms', 'document_seq', 'held', 'queued']) {
          if (typeof row[key] !== 'number' || !Number.isFinite(row[key]) || (row[key] as number) < -1) { issue('browser-number'); return }
          safe[key] = row[key] as number
        }
        if ((safe.document_seq as number) > 128 || (safe.held as number) > 64 || (safe.queued as number) > 64) { issue('browser-bound'); return }
        for (const key of ['focus', 'user_present', 'epoch_changed', 'session_changed']) {
          if (row[key] !== null && typeof row[key] !== 'boolean') { issue('browser-boolean'); return }
          safe[key] = row[key] as boolean | null
        }
        if (safe.action === 'browser-cap') browserCapped = true
        const tab = register(page)
        add({ ...safe, tab: tab.id, page: tab.label })
      })
      await context.addInitScript(({ expectedOrigin, binding, snapshot }) => {
        if (window !== window.top || location.origin !== expectedOrigin) return
        const target = window as unknown as Record<string, unknown>
        let sequence = 0
        let previousEpoch: string | null | undefined
        let previousSession: string | null | undefined
        let queryBusy = false
        const send = (action: string, milestone = 'none', lockState = 'not-requested', held = -1, queued = -1) => {
          if (sequence > 128) return
          if (sequence === 128) action = 'browser-cap'
          let user: boolean | null = null
          let expiry = 'unknown'
          let marker = 'unknown'
          let epochChanged: boolean | null = null
          let sessionChanged: boolean | null = null
          try {
            user = !!localStorage.getItem('user')
            const expires = Number(localStorage.getItem('whm_auth_expires_at'))
            expiry = !Number.isFinite(expires) || expires <= 0 ? 'absent' : expires - Date.now() <= 60_000 ? 'due' : 'future'
            const session = localStorage.getItem('whm_session_id')
            marker = !session ? 'absent' : session.startsWith('ended:') ? 'ended' : 'live'
            const epoch = localStorage.getItem('whm_auth_epoch')
            epochChanged = previousEpoch === undefined ? null : epoch !== previousEpoch
            sessionChanged = previousSession === undefined ? null : session !== previousSession
            previousEpoch = epoch
            previousSession = session
          } catch { /* In-memory product fallback state is deliberately not inspected. */ }
          const payload = { action, milestone, document_origin_ms: performance.timeOrigin,
            document_ms: performance.now(), document_seq: sequence++,
            visibility: document.visibilityState === 'visible' ? 'visible' : document.visibilityState === 'hidden' ? 'hidden' : 'unknown',
            focus: document.hasFocus(), user_present: user, expiry, marker,
            epoch_changed: epochChanged, session_changed: sessionChanged, lock_state: lockState, held, queued }
          const publish = target[binding] as (value: unknown) => Promise<unknown>
          void publish(payload).catch(() => undefined)
        }
        target[snapshot] = async (milestone: string) => {
          send('snapshot', milestone)
          if (!navigator.locks?.query) { send('locks', milestone, 'unsupported'); return }
          if (queryBusy) { send('locks', milestone, 'busy'); return }
          queryBusy = true
          let timer: ReturnType<typeof setTimeout> | undefined
          try {
            const query = navigator.locks.query().finally(() => { queryBusy = false })
            const result = await Promise.race([
              query,
              new Promise<null>(resolve => { timer = setTimeout(() => resolve(null), 250) }),
            ])
            if (!result) { send('locks', milestone, 'timeout'); return }
            const held = result.held ?? []
            const pending = result.pending ?? []
            if (held.length > 64 || pending.length > 64) { send('locks', milestone, 'too-many'); return }
            // Counts only: Web Lock client IDs do not map reliably to Playwright pages.
            send('locks', milestone, 'observed', held.filter(lock => lock.name === 'whm-token-refresh').length,
              pending.filter(lock => lock.name === 'whm-token-refresh').length)
          } catch { send('locks', milestone, 'query-failed') }
          finally { if (timer !== undefined) clearTimeout(timer) }
        }
        send('document')
        document.addEventListener('visibilitychange', () => send('visibility'))
        for (const action of ['focus', 'blur', 'pagehide', 'pageshow']) {
          window.addEventListener(action, () => send(action))
        }
        window.addEventListener('storage', event => {
          if (event.key === null || ['user', 'whm_auth_epoch', 'whm_auth_expires_at', 'whm_session_id'].includes(event.key)) send('storage')
        })
      }, { expectedOrigin: origin, binding: BINDING, snapshot: SNAPSHOT })
      installed = true
      add({ action: 'observer-installed' })
    },
    other(page: Page) {
      const info = register(page)
      info.label = 'other-app'
    },
    mark(milestone: Milestone) { add({ action: 'milestone', milestone }) },
    snapshot(page: Page, milestone: Milestone) {
      if (closed || !installed || snapshotCalls >= 6) { issue('snapshot-unavailable'); return }
      snapshotCalls++
      // Do not await this probe before the unchanged refresh trigger or assertion.
      const task = page.evaluate(async ({ key, milestone }) => {
        const capture = (window as unknown as Record<string, unknown>)[key]
        if (typeof capture !== 'function') return false
        await (capture as (value: string) => Promise<void>)(milestone)
        return true
      }, { key: SNAPSHOT, milestone }).then(available => {
        if (!available) issue('snapshot-document-unavailable')
      }, () => { issue('snapshot-evaluation-failed') }).finally(() => { pending.delete(task) })
      pending.add(task)
    },
    async attach(testInfo: TestInfo, originalFailed: boolean) {
      let timer: ReturnType<typeof setTimeout> | undefined
      try {
        // Post-oracle only. A stalled renderer cannot keep attachment waiting indefinitely.
        await Promise.race([Promise.allSettled([...pending]), new Promise<void>(resolve => { timer = setTimeout(resolve, 500) })])
      } finally {
        if (timer !== undefined) clearTimeout(timer)
        closed = true
        context.off('page', pageCreated)
        context.off('request', requestStarted)
        context.off('response', response)
        context.off('requestfinished', finished)
        context.off('requestfailed', failed)
      }
      try {
        await testInfo.attach('long-stay-first-attempt-observation', {
          contentType: 'application/json',
          body: Buffer.from(JSON.stringify({ schema_version: 1, diagnostic_only: true, retry: testInfo.retry,
            installed_before_navigation: installed, original_failed: originalFailed,
            events, issues, dropped, browser_capped: browserCapped, pending_snapshots: pending.size,
            snapshot_calls: snapshotCalls, request_count: requests.size,
            node_clock: 'performance.now-relative-to-observer', browser_clock: 'document-timeOrigin-and-performance.now',
            lock_ownership: 'unknown', internal_refresh_state: 'unobserved',
            limit: 'Point observations can perturb timing; missing evidence is unknown, not absence.' })),
        })
      } catch {
        testInfo.annotations.push({ type: 'long-stay-observation-attachment-failed' })
        if (!originalFailed) throw new Error('Long-stay observation attachment failed')
        // Never replace the original test assertion when diagnostic attachment fails.
      }
    },
  }
}
