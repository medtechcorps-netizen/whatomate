/** @vitest-environment happy-dom */

import axios, { AxiosError, type InternalAxiosRequestConfig } from 'axios'
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

const toastMock = vi.hoisted(() => ({
  loading: vi.fn(),
  error: vi.fn(),
  dismiss: vi.fn(),
  success: vi.fn(),
}))

vi.mock('vue-sonner', () => ({
  toast: Object.assign(vi.fn(), toastMock),
}))

type ApiModule = typeof import('./api')
type Adapter = (config: InternalAxiosRequestConfig) => Promise<unknown>

const ok = (config: InternalAxiosRequestConfig, data: unknown = { data: {} }) => ({
  data,
  status: 200,
  statusText: 'OK',
  headers: {},
  config,
})

function httpError(status: number, config?: InternalAxiosRequestConfig) {
  return new AxiosError(
    `Request failed with status code ${status}`,
    status >= 500 ? AxiosError.ERR_BAD_RESPONSE : AxiosError.ERR_BAD_REQUEST,
    config,
    null,
    {
      data: { status: 'error' },
      status,
      statusText: String(status),
      headers: {},
      config: config ?? ({ headers: {} } as InternalAxiosRequestConfig),
    },
  )
}

function networkError() {
  return new AxiosError('Network Error', AxiosError.ERR_NETWORK)
}

function refreshOk(expiresIn = 900) {
  return { data: { data: { expires_in: expiresIn } }, status: 200 }
}

// In-memory BroadcastChannel shared by every "tab" in a test.
class FakeBroadcastChannel {
  static channels = new Set<FakeBroadcastChannel>()
  onmessage: ((event: MessageEvent) => void) | null = null
  constructor(public name: string) {
    FakeBroadcastChannel.channels.add(this)
  }
  postMessage(data: unknown) {
    for (const channel of FakeBroadcastChannel.channels) {
      if (channel !== this && channel.name === this.name) {
        channel.onmessage?.({ data } as MessageEvent)
      }
    }
  }
  close() {
    FakeBroadcastChannel.channels.delete(this)
  }
}

// A FIFO Web Locks stand-in shared by every "tab" in a test. Like the real
// API, a request whose options.signal aborts while it waits is dropped from
// the queue and rejects with an AbortError.
function installWebLocks() {
  const tails = new Map<string, Promise<void>>()
  const request = vi.fn(async (name: string, ...rest: unknown[]) => {
    const callback = rest[rest.length - 1] as () => Promise<unknown>
    const options = (rest.length > 1 ? rest[0] : {}) as { signal?: AbortSignal }
    const previous = tails.get(name) ?? Promise.resolve()
    let release!: () => void
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    tails.set(name, previous.then(() => held))
    const signal = options.signal
    if (signal) {
      const aborted = new Promise<never>((_, reject) => {
        const abort = () => reject(new DOMException('Aborted', 'AbortError'))
        if (signal.aborted) abort()
        else signal.addEventListener('abort', abort, { once: true })
      })
      try {
        await Promise.race([previous, aborted])
      } catch (error) {
        // Never granted: free this place in the queue once it comes up.
        void previous.then(() => release())
        throw error
      }
    } else {
      await previous
    }
    try {
      return await callback()
    } finally {
      release()
    }
  })
  Object.defineProperty(navigator, 'locks', {
    configurable: true,
    value: { request },
  })
  return request
}

function removeWebLocks() {
  Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined })
}

const originalLocation = window.location
let assignedHref: string | null = null

function stubLocation(pathname = '/chat') {
  assignedHref = null
  const fake = {
    pathname,
    get href() {
      return assignedHref ?? `http://localhost:3000${pathname}`
    },
    set href(value: string) {
      assignedHref = value
    },
  }
  Object.defineProperty(window, 'location', { configurable: true, value: fake })
}

// Every tab loaded in a test; afterEach detaches them, so no earlier test's
// instance reacts to a later test's page events.
const loadedTabs: ApiModule[] = []

async function loadTab(adapter: Adapter): Promise<ApiModule> {
  vi.resetModules()
  const mod = await import('./api')
  mod.api.defaults.adapter = vi.fn(adapter) as unknown as typeof mod.api.defaults.adapter
  loadedTabs.push(mod)
  return mod
}

function setCookie(value: string) {
  document.cookie = value
}

describe('session refresh interceptor', { timeout: 20_000 }, () => {
  let postSpy: ReturnType<typeof vi.spyOn>
  let sessionEvents: string[]
  const onSessionEvent = (event: Event) => {
    sessionEvents.push((event as CustomEvent<{ state: string }>).detail.state)
  }

  // Transform the module graph (i18n locales included) once, so a cold first
  // import cannot eat into a test's own time budget.
  beforeAll(async () => {
    // Detached at once: this instance must not react to any test's events.
    ;(await import('./api')).disposeForTests()
  }, 60_000)

  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
    localStorage.setItem('user', JSON.stringify({ id: 'user-1', email: 'a@example.com' }))
    setCookie('whm_csrf=csrf-token-1; path=/')
    stubLocation('/chat')
    // Every target browser has Web Locks; tests of a browser without them
    // remove it.
    installWebLocks()
    vi.stubGlobal('BroadcastChannel', undefined)
    postSpy = vi.spyOn(axios, 'post')
    sessionEvents = []
    window.addEventListener('whm:auth-session', onSessionEvent)
    Object.values(toastMock).forEach((fn) => fn.mockReset())
  })

  afterEach(() => {
    while (loadedTabs.length) loadedTabs.pop()!.disposeForTests()
    window.removeEventListener('whm:auth-session', onSessionEvent)
    vi.clearAllTimers()
    vi.useRealTimers()
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
    setCookie('whm_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/')
  })

  it('sends the CSRF double-submit header on refresh and retries the request', async () => {
    let rotated = false
    const { api } = await loadTab(async (config) => {
      if (!rotated) throw httpError(401, config)
      return ok(config, { data: { contacts: [] } })
    })
    postSpy.mockImplementation(async () => {
      rotated = true
      return refreshOk()
    })

    const response = await api.get('/contacts')

    expect(response.status).toBe(200)
    expect(postSpy).toHaveBeenCalledTimes(1)
    const [url, , options] = postSpy.mock.calls[0] as [string, unknown, { headers: Record<string, string>; withCredentials: boolean }]
    expect(url).toMatch(/\/api\/auth\/refresh$/)
    expect(options.withCredentials).toBe(true)
    expect(options.headers['X-CSRF-Token']).toBe('csrf-token-1')
    expect(sessionEvents).toContain('refreshed')
  })

  it('refreshes once when two tabs hit 401 together (Web Locks queue)', async () => {
    const locks = installWebLocks()
    let rotated = false
    const adapter: Adapter = async (config) => {
      if (!rotated) throw httpError(401, config)
      return ok(config)
    }
    const tabA = await loadTab(adapter)
    const tabB = await loadTab(adapter)
    expect(tabA.api).not.toBe(tabB.api)

    postSpy.mockImplementation(async () => {
      // A second refresh with the already-rotated single-use token would be
      // rejected by the server and log the tab out.
      if (rotated) throw httpError(401)
      await new Promise((resolve) => setTimeout(resolve, 5))
      rotated = true
      return refreshOk()
    })

    const [a, b] = await Promise.all([tabA.api.get('/contacts'), tabB.api.get('/contacts')])

    expect(a.status).toBe(200)
    expect(b.status).toBe(200)
    expect(postSpy).toHaveBeenCalledTimes(1)
    expect(locks).toHaveBeenCalledTimes(2)
    expect(localStorage.getItem('user')).not.toBeNull()
    expect(assignedHref).toBeNull()
  })

  it('retries a stale 401 without refreshing again', async () => {
    let rotated = false
    let slowCalls = 0
    let releaseSlow!: () => void
    const slowGate = new Promise<void>((resolve) => {
      releaseSlow = resolve
    })
    const { api } = await loadTab(async (config) => {
      if (config.url === '/slow' && ++slowCalls === 1) {
        // Sent with the old cookie; its 401 only arrives after the refresh.
        await slowGate
        throw httpError(401, config)
      }
      if (!rotated) throw httpError(401, config)
      return ok(config)
    })
    postSpy.mockImplementation(async () => {
      rotated = true
      return refreshOk()
    })

    const slow = api.get('/slow')
    const fast = await api.get('/fast')
    expect(fast.status).toBe(200)
    expect(postSpy).toHaveBeenCalledTimes(1)

    releaseSlow()
    const slowResponse = await slow
    expect(slowResponse.status).toBe(200)
    expect(slowCalls).toBe(2)
    expect(postSpy).toHaveBeenCalledTimes(1)
  })

  it('keeps the user signed in when refresh fails with network errors or 5xx', async () => {
    vi.useFakeTimers()
    const { api } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    postSpy
      .mockRejectedValueOnce(networkError())
      .mockRejectedValueOnce(httpError(503))
      .mockRejectedValueOnce(httpError(504))
      .mockRejectedValueOnce(httpError(429))

    const request = api.get('/contacts')
    const settled = request.then(
      () => 'resolved',
      (error: AxiosError) => error.response?.status,
    )
    await vi.advanceTimersByTimeAsync(10_000)

    expect(await settled).toBe(401)
    expect(postSpy).toHaveBeenCalledTimes(4)
    expect(localStorage.getItem('user')).not.toBeNull()
    expect(assignedHref).toBeNull()
    expect(toastMock.loading).toHaveBeenCalledTimes(1)
    expect(toastMock.error).toHaveBeenCalledTimes(1)
    expect(sessionEvents).toEqual(['reconnecting', 'unavailable'])
  })

  it('recovers silently when a retry succeeds', async () => {
    vi.useFakeTimers()
    let rotated = false
    const { api } = await loadTab(async (config) => {
      if (!rotated) throw httpError(401, config)
      return ok(config)
    })
    postSpy.mockRejectedValueOnce(httpError(502)).mockImplementationOnce(async () => {
      rotated = true
      return refreshOk()
    })

    const request = api.get('/contacts')
    await vi.advanceTimersByTimeAsync(1_000)

    expect((await request).status).toBe(200)
    expect(postSpy).toHaveBeenCalledTimes(2)
    expect(toastMock.dismiss).toHaveBeenCalled()
    expect(sessionEvents).toEqual(['reconnecting', 'reconnected'])
  })

  it('logs out on a definitive 401 from /auth/refresh', async () => {
    const { api } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    postSpy.mockRejectedValue(httpError(401))

    await expect(api.get('/contacts')).rejects.toMatchObject({ response: { status: 401 } })

    expect(postSpy).toHaveBeenCalledTimes(1)
    expect(localStorage.getItem('user')).toBeNull()
    expect(assignedHref).toBe('/login')
    expect(sessionEvents).toEqual(['expired'])
  })

  // A transient server fault answers API requests with 401 (say, a database
  // error behind the tenant check): only a refresh can tell a dead session.
  it('an API 401 while the refresh answers 503 keeps the session: nothing cleared, ended or redirected', async () => {
    vi.useFakeTimers()
    localStorage.setItem('whm_session_id', 'session-a')
    localStorage.setItem('selected_organization_id', 'org-a')
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => {
      throw httpError(401, config)
    })
    const { api } = await loadTab(adapter)
    const order: string[] = []
    recordPosts(order, (label) => {
      if (label === 'refresh') throw httpError(503)
    })

    const settled = api.get('/contacts').then(
      () => 'resolved',
      (error: AxiosError) => error.response?.status,
    )
    await vi.advanceTimersByTimeAsync(30_000)

    // The request fails; the session stays as it is.
    expect(await settled).toBe(401)
    expect(adapter).toHaveBeenCalledTimes(1)
    expect(order.length).toBeGreaterThan(0)
    expect(order.every((label) => label === 'refresh')).toBe(true)
    expect(localStorage.getItem('user')).not.toBeNull()
    expect(localStorage.getItem('whm_session_id')).toBe('session-a')
    expect(localStorage.getItem('selected_organization_id')).toBe('org-a')
    expect(localStorage.getItem('whm_logout_pending')).toBeNull()
    expect(assignedHref).toBeNull()
    expect(sessionEvents).not.toContain('expired')
  })

  it('an API 401 that comes back after a good refresh is replayed once, no more, and ends nothing', async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => {
      throw httpError(401, config)
    })
    const { api } = await loadTab(adapter)
    const order: string[] = []
    recordPosts(order, (label) => (label === 'refresh' ? refreshOk() : undefined))

    await expect(api.get('/contacts')).rejects.toMatchObject({ response: { status: 401 } })
    await tick()

    // One refresh and one replay, then the 401 is the caller's.
    expect(adapter).toHaveBeenCalledTimes(2)
    expect(order).toEqual(['refresh'])
    expect(localStorage.getItem('user')).not.toBeNull()
    expect(localStorage.getItem('whm_session_id')).toBe('session-a')
    expect(localStorage.getItem('whm_logout_pending')).toBeNull()
    expect(assignedHref).toBeNull()
    expect(sessionEvents).toEqual(['refreshed'])
  })

  it('an API 401 with a refresh 401 ends the session (a genuinely invalid one)', async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    const { api } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const order: string[] = []
    recordPosts(order, (label) => {
      if (label === 'refresh') throw httpError(401)
    })

    await expect(api.get('/contacts')).rejects.toMatchObject({ response: { status: 401 } })

    expect(order).toEqual(['refresh'])
    expect(localStorage.getItem('user')).toBeNull()
    expect(localStorage.getItem('whm_session_id')).toBe('ended:session-a')
    expect(assignedHref).toBe('/login')
    expect(sessionEvents).toEqual(['expired'])
  })

  it('treats an HTML 403 from an edge or WAF as transient', async () => {
    vi.useFakeTimers()
    const { api } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const edge403 = () => {
      const error = httpError(403)
      ;(error.response as { data: unknown }).data = '<html><body>Request blocked</body></html>'
      return error
    }
    postSpy.mockImplementation(async () => {
      throw edge403()
    })

    const settled = api.get('/contacts').then(
      () => 'resolved',
      (error: AxiosError) => error.response?.status,
    )
    await vi.advanceTimersByTimeAsync(10_000)

    expect(await settled).toBe(401)
    expect(postSpy).toHaveBeenCalledTimes(4)
    expect(localStorage.getItem('user')).not.toBeNull()
    expect(assignedHref).toBeNull()
  })

  // Labels the bare-axios POSTs: revoke, logout or refresh.
  function postLabel(url: string, body: unknown, options: any) {
    if ((body as { revoke?: boolean })?.revoke) return `revoke ${options?.headers?.['X-CSRF-Token']}`
    if (url.endsWith('/auth/logout')) return `logout ${options?.headers?.['X-CSRF-Token'] ?? '-'}`
    return 'refresh'
  }

  function recordPosts(
    order: string[],
    onPost?: (label: string, options: any) => Promise<unknown> | unknown,
  ) {
    postSpy.mockImplementation(async (url: string, body: unknown, options: any) => {
      const label = postLabel(url, body, options)
      order.push(label)
      const custom = onPost ? await onPost(label, options) : undefined
      return custom ?? { data: { data: {} }, status: 200 }
    })
    recordFetches(async (url, body, options) => {
      const label = postLabel(url, body, options)
      order.push(label)
      return onPost ? await onPost(label, options) : undefined
    })
  }

  // Routes the API client's fetch calls (the keepalive revoke of a page
  // without Web Locks) to `onFetch`, answered like axios.post would be: a
  // thrown httpError becomes that status. A fetch spy a test installed itself
  // is left in place.
  function recordFetches(onFetch: (url: string, body: unknown, options: any) => Promise<unknown> | unknown) {
    const current = globalThis.fetch as unknown as { laneARecorder?: boolean }
    if (vi.isMockFunction(globalThis.fetch) && !current.laneARecorder) return
    const recorder = vi.fn(async (url: string, init: RequestInit = {}) => {
      const body = typeof init.body === 'string' ? JSON.parse(init.body) : init.body
      try {
        const custom = (await onFetch(String(url), body, { headers: init.headers })) as
          | { data?: unknown; status?: number }
          | undefined
        return new Response(JSON.stringify(custom?.data ?? { data: {} }), { status: custom?.status ?? 200 })
      } catch (error) {
        const status = (error as { response?: { status?: number } })?.response?.status
        if (status) return new Response(JSON.stringify({ status: 'error' }), { status })
        throw error
      }
    })
    Object.assign(recorder, { laneARecorder: true })
    vi.stubGlobal('fetch', recorder)
  }

  // Another tab holds the shared refresh lock until the returned release().
  function holdLockElsewhere() {
    let release!: () => void
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    void (navigator as unknown as { locks: { request: (name: string, cb: () => Promise<void>) => Promise<void> } })
      .locks.request('whm-token-refresh', () => held)
    return release
  }

  // A request held in the adapter until release() (then it gets a 401).
  function gatedAdapter() {
    let release!: () => void
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    let sent!: () => void
    const inAdapter = new Promise<void>((resolve) => {
      sent = resolve
    })
    let calls = 0
    const adapter = async (config: InternalAxiosRequestConfig) => {
      if (config.url === '/auth/login') {
        return ok(config, { data: { expires_in: 900, user: {} } })
      }
      calls++
      sent()
      await gate
      throw httpError(401, config)
    }
    return { adapter, release: () => release(), inAdapter, replayed: () => calls > 1 }
  }

  const tick = () => new Promise((resolve) => setTimeout(resolve, 20))

  it('logs out at once, then revokes and logs out in the background', async () => {
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config))
    const { api, whenLogoutSettled } = await loadTab(adapter)
    const order: string[] = []
    recordPosts(order)

    const response = await api.post('/auth/logout', {})

    expect(response.status).toBe(200)
    expect(adapter).not.toHaveBeenCalled()
    await whenLogoutSettled()
    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1'])
    const [url, , options] = postSpy.mock.calls[0] as [string, unknown, { withCredentials: boolean }]
    expect(url).toMatch(/\/api\/auth\/refresh$/)
    expect(options.withCredentials).toBe(true)
  })

  it('logs out with the CSRF cookie as it is after the revoke', async () => {
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    // The revoke rotates the cookie (another tab refreshed meanwhile)...
    recordPosts(order, (label) => {
      if (label.startsWith('revoke')) setCookie('whm_csrf=csrf-token-2; path=/')
    })
    await api.post('/auth/logout', {})
    await whenLogoutSettled()
    // ...or clears it, as a successful revoke does.
    setCookie('whm_csrf=csrf-token-3; path=/')
    recordPosts(order, (label) => {
      if (label.startsWith('revoke')) setCookie('whm_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/')
    })
    await api.post('/auth/logout', {})
    await whenLogoutSettled()

    expect(order).toEqual([
      'revoke csrf-token-1', 'logout csrf-token-2',
      'revoke csrf-token-3', 'logout -',
    ])
  })

  it('retries a revoke rejected for a CSRF cookie rotated mid-flight', async () => {
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    let first = true
    recordPosts(order, (label) => {
      if (label.startsWith('revoke') && first) {
        first = false
        setCookie('whm_csrf=csrf-token-2; path=/')
        throw httpError(403)
      }
    })

    await api.post('/auth/logout', {})
    await whenLogoutSettled()

    expect(order).toEqual(['revoke csrf-token-1', 'revoke csrf-token-2', 'logout csrf-token-2'])
  })

  it('stops when someone else signs in during the revoke backoff', async () => {
    vi.useFakeTimers()
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order, (label) => {
      if (label.startsWith('revoke')) {
        // While this logout waits out a 429, another tab signs a different
        // user in: new identity, new logical session, new cookies.
        localStorage.setItem('user', JSON.stringify({ id: 'user-2', email: 'b@example.com' }))
        localStorage.setItem('whm_session_id', 'session-of-user-2')
        setCookie('whm_csrf=csrf-of-user-2; path=/')
        throw httpError(429)
      }
    })

    await api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(10_000)
    await whenLogoutSettled()

    // Neither a second revoke nor a logout touches the new session.
    expect(order).toEqual(['revoke csrf-token-1'])
  })

  it('re-reads the CSRF cookie for each revoke attempt', async () => {
    vi.useFakeTimers()
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    let first = true
    recordPosts(order, (label) => {
      if (label.startsWith('revoke') && first) {
        first = false
        // Another tab refreshes meanwhile and rotates the CSRF cookie.
        setCookie('whm_csrf=csrf-token-2; path=/')
        throw httpError(429)
      }
    })

    await api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(1_000)
    await whenLogoutSettled()

    expect(order).toEqual(['revoke csrf-token-1', 'revoke csrf-token-2', 'logout csrf-token-2'])
  })

  it('retries a rate-limited revoke before logging out', async () => {
    vi.useFakeTimers()
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    let revokes = 0
    recordPosts(order, (label) => {
      if (label.startsWith('revoke') && ++revokes < 3) throw httpError(429)
    })

    await api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(3_000)
    await whenLogoutSettled()

    expect(order).toEqual([
      'revoke csrf-token-1', 'revoke csrf-token-1', 'revoke csrf-token-1', 'logout csrf-token-1',
    ])
  })

  it('logs out after the revoke stays rate-limited or fails', async () => {
    vi.useFakeTimers()
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order, (label) => {
      if (label.startsWith('revoke')) throw httpError(429)
    })
    await api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(10_000)
    await whenLogoutSettled()
    expect(order.filter((label) => label.startsWith('revoke'))).toHaveLength(3)
    expect(order[order.length - 1]).toBe('logout csrf-token-1')

    order.length = 0
    recordPosts(order, (label) => {
      if (label.startsWith('revoke')) throw networkError()
    })
    await api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(10_000)
    await whenLogoutSettled()
    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1'])
  })

  {
    it('a logout waits for a refresh in flight, which then leaves no trace', async () => {
      let releaseRefresh!: () => void
      const refreshGate = new Promise<void>((resolve) => {
        releaseRefresh = resolve
      })
      const { api, whenLogoutSettled } = await loadTab(async (config) => {
        throw httpError(401, config)
      })
      const order: string[] = []
      recordPosts(order, async (label) => {
        if (label === 'refresh') {
          await refreshGate
          order.push('refresh done')
          return refreshOk()
        }
      })

      const request = api.get('/contacts').then(
        () => 'resolved',
        (error: AxiosError) => error.code ?? error.response?.status,
      )
      await vi.waitFor(() => expect(order).toEqual(['refresh']))
      await api.post('/auth/logout', {})
      await new Promise((resolve) => setTimeout(resolve, 20))
      // The revoke waits: the refresh response must land first.
      expect(order).toEqual(['refresh'])

      releaseRefresh()
      await whenLogoutSettled()
      expect(order).toEqual(['refresh', 'refresh done', 'revoke csrf-token-1', 'logout csrf-token-1'])
      // The late refresh left no session bookkeeping behind.
      expect(localStorage.getItem('whm_auth_expires_at')).toBeNull()
      expect(await request).not.toBe('resolved')
    })
  }

  it('a logout stops a refresh that is backing off', async () => {
    vi.useFakeTimers()
    installWebLocks()
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const order: string[] = []
    recordPosts(order, (label) => {
      if (label === 'refresh') throw httpError(503)
    })

    const request = api.get('/contacts').then(() => 'resolved', () => 'rejected')
    await vi.advanceTimersByTimeAsync(10)
    expect(order).toEqual(['refresh'])
    await api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(10_000)
    await whenLogoutSettled()

    expect(order).toEqual(['refresh', 'revoke csrf-token-1', 'logout csrf-token-1'])
    expect(await request).toBe('rejected')
  })

  it('a refresh queued on the lock never sends once its session ended', async () => {
    const locks = installWebLocks()
    let releaseHolder!: () => void
    const held = new Promise<void>((resolve) => {
      releaseHolder = resolve
    })
    // Another tab holds the refresh lock.
    void (navigator as unknown as { locks: { request: (name: string, cb: () => Promise<void>) => Promise<void> } })
      .locks.request('whm-token-refresh', () => held)
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const order: string[] = []
    recordPosts(order)

    const request = api.get('/contacts').then(() => 'resolved', () => 'rejected')
    await vi.waitFor(() => expect(locks).toHaveBeenCalledTimes(2))
    await api.post('/auth/logout', {})
    await vi.waitFor(() => expect(locks).toHaveBeenCalledTimes(3))
    releaseHolder()
    await whenLogoutSettled()

    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1'])
    expect(await request).toBe('rejected')
  })

  it('a new sign-in waits for an earlier logout to finish', async () => {
    let releaseRevoke!: () => void
    const revokeGate = new Promise<void>((resolve) => {
      releaseRevoke = resolve
    })
    const order: string[] = []
    const { api } = await loadTab(async (config) => {
      order.push(`sign-in ${config.url}`)
      return ok(config, { data: { expires_in: 900, user: {} } })
    })
    recordPosts(order, async (label) => {
      if (label.startsWith('revoke')) await revokeGate
    })

    await api.post('/auth/logout', {})
    const login = api.post('/auth/login', { email: 'a@example.com', password: 'x' })
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(order).toEqual(['revoke csrf-token-1'])

    releaseRevoke()
    await login
    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1', 'sign-in /auth/login'])
    // The new session is live: its own bookkeeping applies.
    expect(Number(localStorage.getItem('whm_auth_expires_at'))).toBeGreaterThan(Date.now())
    expect(localStorage.getItem('whm_session_id')).toMatch(/^(?!ended:)./)
  })

  it('an org switch is ordered after a refresh already in flight', async () => {
    installWebLocks()
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const order: string[] = []
    let rotated = false
    const { api } = await loadTab(async (config) => {
      if (config.url === '/auth/switch-org') {
        order.push('switch-org')
        return ok(config, { data: { expires_in: 900, user: {} } })
      }
      if (!rotated) throw httpError(401, config)
      return ok(config)
    })
    recordPosts(order, async (label) => {
      if (label === 'refresh') {
        await refreshGate
        rotated = true
        order.push('refresh done')
        return refreshOk()
      }
    })

    localStorage.setItem('whm_session_id', 'session-a')
    const request = api.get('/contacts')
    await vi.waitFor(() => expect(order).toEqual(['refresh']))
    const switched = api.post('/auth/switch-org', { organization_id: 'org-2' })
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(order).toEqual(['refresh'])

    releaseRefresh()
    await switched
    await request
    expect(order.slice(0, 3)).toEqual(['refresh', 'refresh done', 'switch-org'])
    // An org switch is not a new login: the logical session carries on.
    expect(localStorage.getItem('whm_session_id')).toBe('session-a')
  })

  it('a failed sign-in queued ahead of a logout leaves that logout in force', async () => {
    const locks = installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const signIn = await loadTab(async (config) => {
      if (config.url === '/auth/login') throw httpError(401, config)
      return ok(config)
    })
    const tab = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order)

    // A typo on the login page, queued first behind another tab's refresh...
    const login = signIn.api.post('/auth/login', { email: 'a@example.com', password: 'typo' })
      .then(() => 'signed in', (error: AxiosError) => error.response?.status)
    await vi.waitFor(() => expect(locks).toHaveBeenCalledTimes(2))
    // ...then the user's logout in another tab.
    await tab.api.post('/auth/logout', {})
    await vi.waitFor(() => expect(locks).toHaveBeenCalledTimes(3))
    releaseHolder()

    expect(await login).toBe(401)
    await tab.whenLogoutSettled()
    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1'])
    expect(localStorage.getItem('whm_session_id')).toBe('ended:')
  })

  it('an org switch answered after the logout cap is revoked, not applied', async () => {
    vi.useFakeTimers()
    installWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    const order: string[] = []
    let switchTimeout: number | undefined
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      if (config.url === '/auth/switch-org') {
        switchTimeout = config.timeout
        // A stalled server that answers after the logout's 20 s cap.
        await new Promise((resolve) => setTimeout(resolve, 25_000))
        order.push('switch-org answered')
        return ok(config, { data: { expires_in: 900, user: {} } })
      }
      return ok(config)
    })
    recordPosts(order)

    const switched = api.post('/auth/switch-org', { organization_id: 'org-2' }).then(
      () => 'applied',
      (error: AxiosError) => error.code,
    )
    await vi.advanceTimersByTimeAsync(100)
    await api.post('/auth/logout', {})

    await vi.advanceTimersByTimeAsync(20_000)
    await whenLogoutSettled()
    // The cap ran out: the logout is deferred, never sent unlocked.
    expect(order).toEqual([])
    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!)).toMatchObject({ marker: 'ended:session-a' })

    await vi.advanceTimersByTimeAsync(10_000)
    expect(await switched).toBe('ERR_CANCELED')
    // What the late switch issued is revoked and cleared at once, which also
    // completes the deferred logout.
    expect(order).toEqual(['switch-org answered', 'revoke csrf-token-1', 'logout csrf-token-1'])
    expect(localStorage.getItem('whm_logout_pending')).toBeNull()
    expect(localStorage.getItem('whm_auth_expires_at')).toBeNull()
    expect(localStorage.getItem('whm_session_id')).toBe('ended:session-a')
    expect(switchTimeout).toBe(15_000)
  })

  it('sign-ins and org switches time out before the logout cap', async () => {
    const timeouts: Array<number | undefined> = []
    const { api } = await loadTab(async (config) => {
      timeouts.push(config.timeout)
      return ok(config, { data: { expires_in: 900, user: {} } })
    })
    for (const url of ['/auth/login', '/auth/register', '/auth/switch-org']) {
      await api.post(url, {})
    }
    expect(timeouts).toEqual([15_000, 15_000, 15_000])
  })

  it('aborts a revoke still pending at the budget before the next sign-in', async () => {
    vi.useFakeTimers()
    installWebLocks()
    const signals: AbortSignal[] = []
    let abortedWhenSignInSent: boolean | null = null
    const order: string[] = []
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      abortedWhenSignInSent = signals[2]?.aborted ?? null
      order.push('login')
      return ok(config, { data: { expires_in: 900, user: {} } })
    })
    let revokes = 0
    recordPosts(order, (label, options) => {
      if (!label.startsWith('revoke')) return undefined
      signals.push(options.signal)
      if (++revokes < 3) throw httpError(429)
      // The third attempt hangs until it is aborted.
      return new Promise((_, reject) => {
        options.signal.addEventListener('abort', () => reject(new AxiosError('canceled', AxiosError.ERR_CANCELED)))
      })
    })

    await api.post('/auth/logout', {})
    const login = api.post('/auth/login', { email: 'b@example.com', password: 'x' })
    await vi.advanceTimersByTimeAsync(12_000)
    await login
    await whenLogoutSettled()

    expect(order).toEqual([
      'revoke csrf-token-1', 'revoke csrf-token-1', 'revoke csrf-token-1',
      'logout csrf-token-1', 'login',
    ])
    expect(abortedWhenSignInSent).toBe(true)
  })

  it('stops the revoke when the same user signs in again (only the session changes)', async () => {
    vi.useFakeTimers()
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order, (label) => {
      if (label.startsWith('revoke')) {
        // Same user, same org: 'user' is unchanged; only the logical session
        // and the cookies are new.
        localStorage.setItem('whm_session_id', 'session-2')
        setCookie('whm_csrf=csrf-token-2; path=/')
        throw httpError(429)
      }
    })

    await api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(10_000)
    await whenLogoutSettled()

    expect(order).toEqual(['revoke csrf-token-1'])
  })

  it('sends nothing when a newer session began before the logout got the lock', async () => {
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order)

    await api.post('/auth/logout', {})
    // Another tab's sign-in succeeds first.
    localStorage.setItem('whm_session_id', 'session-b')
    setCookie('whm_csrf=csrf-of-b; path=/')
    releaseHolder()
    await whenLogoutSettled()

    expect(order).toEqual([])
  })

  it('sends nothing for the old session once another identity is stored', async () => {
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order)

    await api.post('/auth/logout', {})
    // Another user's identity arrives without a new marker (defence in depth:
    // the identity check also covers the first revoke attempt and the logout).
    localStorage.setItem('user', JSON.stringify({ id: 'user-2', email: 'b@example.com' }))
    releaseHolder()
    await whenLogoutSettled()

    expect(order).toEqual([])
  })

  it('a sign-in that cannot get the lock in time is refused, never sent unlocked', async () => {
    vi.useFakeTimers()
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) =>
      ok(config, { data: { expires_in: 900, user: {} } }),
    )
    const { api, exchangeSsoCode, SESSION_BUSY_CODE } = await loadTab(adapter)
    const marker = localStorage.getItem('whm_session_id')

    const login = api.post('/auth/login', { email: 'b@example.com', password: 'x' })
      .then(() => 'signed in', (error: AxiosError) => error.code)
    const exchange = exchangeSsoCode('code-b').then(() => 'signed in', (error: AxiosError) => error.code)
    await vi.advanceTimersByTimeAsync(20_500)

    expect(await login).toBe(SESSION_BUSY_CODE)
    expect(await exchange).toBe(SESSION_BUSY_CODE)
    expect(adapter).not.toHaveBeenCalled()
    expect(localStorage.getItem('whm_session_id')).toBe(marker)
    releaseHolder()
  })

  it('a logout whose lock wait runs out is deferred, then finished first by the next sign-in', async () => {
    vi.useFakeTimers()
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const order: string[] = []
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      order.push('login')
      return ok(config, { data: { expires_in: 900, user: {} } })
    })
    recordPosts(order)

    await api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(20_500)
    await whenLogoutSettled()
    // Deferred: nothing was sent unlocked, and the record keeps it pending.
    expect(order).toEqual([])
    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!)).toMatchObject({ marker: 'ended:' })

    releaseHolder()
    const login = api.post('/auth/login', { email: 'b@example.com', password: 'x' })
    await vi.advanceTimersByTimeAsync(100)
    await login
    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1', 'login'])
    expect(localStorage.getItem('whm_logout_pending')).toBeNull()
  })

  it("a session begun in another tab aborts this tab's logout requests in flight", async () => {
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const signals: AbortSignal[] = []
    const order: string[] = []
    recordPosts(order, (label, options) => {
      if (!label.startsWith('revoke')) return undefined
      signals.push(options.signal)
      return new Promise((_, reject) => {
        options.signal.addEventListener('abort', () => reject(new AxiosError('canceled', AxiosError.ERR_CANCELED)))
      })
    })

    await api.post('/auth/logout', {})
    await vi.waitFor(() => expect(signals).toHaveLength(1))
    // An SSO callback in another tab starts B's session.
    localStorage.setItem('whm_session_id', 'session-b')
    window.dispatchEvent(new StorageEvent('storage', { key: 'whm_session_id', newValue: 'session-b' }))
    // At once, not at the revoke's 7 s budget.
    expect(signals[0].aborted).toBe(true)
    await whenLogoutSettled()
    expect(order).toEqual(['revoke csrf-token-1'])
  })

  it('a closing tab with another tab open leaves its logout to that tab', async () => {
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    FakeBroadcastChannel.channels.clear()
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const fetchSpy = vi.fn(async () => new Response('{}'))
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem('whm_session_id', 'session-closing')
    const { api } = await loadTab(async (config) => ok(config))
    recordPosts([])
    const peer = new FakeBroadcastChannel('whm-tabs')
    const heard: Array<{ type: string }> = []
    peer.onmessage = (event) => heard.push(event.data)
    peer.postMessage({ type: 'hello', id: 'peer-tab' })
    // The other tab is alive: its liveness stamp is fresh.
    localStorage.setItem('whm_tab_alive:peer-tab', String(Date.now()))

    await api.post('/auth/logout', {})
    window.dispatchEvent(new Event('pagehide'))

    // No unordered keepalive: the open tab is told to take over instead.
    expect(fetchSpy).not.toHaveBeenCalled()
    expect(heard.map((message) => message.type)).toEqual(['here', 'bye'])
    releaseHolder()
    FakeBroadcastChannel.channels.clear()
  })

  it('a closing tab still sends its logout when the other tab is frozen or gone', async () => {
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    FakeBroadcastChannel.channels.clear()
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const fetchSpy = vi.fn(async () => new Response('{}'))
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem('whm_session_id', 'session-closing-2')
    const { api } = await loadTab(async (config) => ok(config))
    recordPosts([])
    const peer = new FakeBroadcastChannel('whm-tabs')
    peer.postMessage({ type: 'hello', id: 'frozen-tab' })
    // The other tab's liveness stamp is stale (frozen, discarded or crashed).
    localStorage.setItem('whm_tab_alive:frozen-tab', String(Date.now() - 60_000))

    await api.post('/auth/logout', {})
    window.dispatchEvent(new Event('pagehide'))

    const calls = fetchSpy.mock.calls as unknown as [string, RequestInit][]
    // One revoke, cookie-neutral: its late answer can never clear a newer
    // session's cookies. The next page load clears the old ones, ordered.
    expect(calls.map(([url]) => url)).toEqual(['/api/auth/refresh'])
    expect(JSON.parse(calls[0][1].body as string)).toEqual({ revoke: true, keep_cookies: true })
    expect(localStorage.getItem('whm_logout_pending')).not.toBeNull()
    releaseHolder()
    FakeBroadcastChannel.channels.clear()
  })

  it('an open tab finishes, in order, a logout handed over by a closing tab', async () => {
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    FakeBroadcastChannel.channels.clear()
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const open = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order)
    // The closing tab had logged out; its logout is still pending.
    localStorage.setItem('whm_session_id', 'ended:session-closing')
    localStorage.setItem('whm_logout_pending', JSON.stringify({ marker: 'ended:session-closing', csrf: 'csrf-token-1' }))
    new FakeBroadcastChannel('whm-tabs').postMessage({ type: 'bye', id: 'closing-tab' })

    await tick()
    expect(order).toEqual([])
    releaseHolder()
    await open.whenLogoutSettled()
    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1'])
    expect(localStorage.getItem('whm_logout_pending')).toBeNull()
    FakeBroadcastChannel.channels.clear()
  })

  it('the last tab sends its pending logout with keepalive when it closes', async () => {
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    FakeBroadcastChannel.channels.clear()
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const fetchSpy = vi.fn(async () => new Response('{}'))
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem('whm_session_id', 'session-last')
    const { api } = await loadTab(async (config) => ok(config))
    recordPosts([])

    await api.post('/auth/logout', {})
    window.dispatchEvent(new Event('pagehide'))

    const calls = fetchSpy.mock.calls as unknown as [string, RequestInit][]
    expect(calls.map(([url]) => url)).toEqual(['/api/auth/refresh'])
    expect(calls[0][1].keepalive).toBe(true)
    expect(JSON.parse(calls[0][1].body as string)).toEqual({ revoke: true, keep_cookies: true })
    // The record stays until a logout is known to have got through.
    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!)).toMatchObject({ marker: 'ended:session-last' })
    releaseHolder()
    FakeBroadcastChannel.channels.clear()
  })

  it('an SSO exchange or sign-in is refused while an earlier logout still fails, and that logout stays pending', async () => {
    installWebLocks()
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config, { data: { expires_in: 900, user: {} } }))
    const { api, exchangeSsoCode, SESSION_BUSY_CODE } = await loadTab(adapter)
    // An outage: the logout of session A never got through.
    localStorage.setItem('whm_session_id', 'ended:session-a')
    localStorage.setItem('whm_logout_pending', JSON.stringify({ marker: 'ended:session-a', csrf: 'csrf-token-1' }))
    const order: string[] = []
    recordPosts(order, (label) => {
      if (label.startsWith('revoke')) throw httpError(504)
      if (label.startsWith('logout')) throw networkError()
    })

    await expect(exchangeSsoCode('code-b')).rejects.toMatchObject({ code: SESSION_BUSY_CODE })
    await expect(api.post('/auth/login', { email: 'b@example.com', password: 'x' })).rejects.toMatchObject({ code: SESSION_BUSY_CODE })

    expect(adapter).not.toHaveBeenCalled()
    expect(localStorage.getItem('whm_session_id')).toBe('ended:session-a')
    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!)).toMatchObject({ marker: 'ended:session-a' })

    // Once the server is back, the next load finishes it.
    order.length = 0
    recordPosts(order)
    const later = await loadTab(adapter)
    await later.whenLogoutSettled()
    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1'])
    expect(localStorage.getItem('whm_logout_pending')).toBeNull()
  })

  it('an org switch is refused after the session ended, even with its logout still pending', async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config, { data: { expires_in: 900, user: {} } }))
    const other = await loadTab(adapter)
    const ending = await loadTab(adapter)
    recordPosts([], (label) => {
      if (label.startsWith('logout')) throw networkError()
    })
    await ending.api.post('/auth/logout', {})
    await ending.whenLogoutSettled()
    expect(localStorage.getItem('whm_logout_pending')).not.toBeNull()
    const user = localStorage.getItem('user')

    await expect(other.api.post('/auth/switch-org', { organization_id: 'org-2' }))
      .rejects.toMatchObject({ code: 'ERR_CANCELED' })
    expect(adapter).not.toHaveBeenCalled()
    expect(localStorage.getItem('user')).toBe(user)
  })

  it('a sign-in queued before a takeover still finishes the taken-over logout first', async () => {
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    FakeBroadcastChannel.channels.clear()
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const order: string[] = []
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      order.push(config.url!)
      return ok(config, { data: { expires_in: 900, user: {} } })
    })
    recordPosts(order)
    const login = api.post('/auth/login', { email: 'b@example.com', password: 'x' })
    await tick()
    // A closing tab hands over its pending logout.
    localStorage.setItem('whm_session_id', 'ended:session-closing')
    localStorage.setItem('whm_logout_pending', JSON.stringify({ marker: 'ended:session-closing', csrf: 'csrf-token-1' }))
    new FakeBroadcastChannel('whm-tabs').postMessage({ type: 'bye', id: 'closing-tab' })

    releaseHolder()
    await login
    await whenLogoutSettled()
    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1', '/auth/login'])
    FakeBroadcastChannel.channels.clear()
  })

  it('a sign-in that times out after its cookies were set undoes them before letting go', async () => {
    installWebLocks()
    const order: string[] = []
    const { api, SESSION_BUSY_CODE } = await loadTab(async (config) => {
      // The answer's cookies were stored, then the request timed out.
      setCookie('whm_csrf=csrf-of-b; path=/')
      order.push('login (no answer)')
      throw new AxiosError('timeout of 15000ms exceeded', AxiosError.ECONNABORTED, config)
    })
    recordPosts(order)
    const marker = localStorage.getItem('whm_session_id')

    const error = await api.post('/auth/login', { email: 'b@example.com', password: 'x' }).catch((e) => e)

    expect(error.code).toBe(AxiosError.ECONNABORTED)
    expect(error.code).not.toBe(SESSION_BUSY_CODE)
    expect(order).toEqual(['login (no answer)', 'revoke csrf-of-b', 'logout csrf-of-b'])
    // No session began.
    expect(localStorage.getItem('whm_session_id')).toBe(marker)
  })

  it("a newer session in another tab aborts this tab's undo of a late switch", async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    let answer!: () => void
    const gate = new Promise<void>((resolve) => {
      answer = resolve
    })
    const signals: AbortSignal[] = []
    const order: string[] = []
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      if (config.url === '/auth/switch-org') {
        await gate
        return ok(config, { data: { expires_in: 900, user: {} } })
      }
      return ok(config)
    })
    recordPosts(order, (label, options) => {
      if (label.startsWith('revoke') && signals.length === 0 && order.length === 1) {
        signals.push(options.signal)
        return new Promise((_, reject) => {
          options.signal.addEventListener('abort', () => reject(new AxiosError('canceled', AxiosError.ERR_CANCELED)))
        })
      }
      return undefined
    })
    const switched = api.post('/auth/switch-org', { organization_id: 'org-2' }).catch((e) => e.code)
    await tick()
    // The session ends (another tab), so the late switch answer must be undone...
    localStorage.setItem('whm_session_id', 'ended:session-a')
    answer()
    await vi.waitFor(() => expect(signals).toHaveLength(1))
    // ...but meanwhile B signs in elsewhere: the undo stops at once.
    localStorage.setItem('whm_session_id', 'session-b')
    window.dispatchEvent(new StorageEvent('storage', { key: 'whm_session_id', newValue: 'session-b' }))

    expect(await switched).toBe('ERR_CANCELED')
    await whenLogoutSettled()
    expect(signals[0].aborted).toBe(true)
    expect(order).toEqual(['revoke csrf-token-1'])
  })

  it('an SSO start keeps the organization selection a live tab may still use', async () => {
    installWebLocks()
    localStorage.setItem('selected_organization_id', 'org-a')
    const { beginSsoSignIn } = await loadTab(async (config) => ok(config))

    await beginSsoSignIn()

    // Only the callback, once the new session's cookies are in, drops it.
    expect(localStorage.getItem('selected_organization_id')).toBe('org-a')
  })

  it("the SSO exchange drops the identity and organization a previous session stored; /me then carries none", async () => {
    localStorage.setItem('selected_organization_id', 'org-a')
    localStorage.setItem('whm_session_id', 'ended:session-a')
    stubLocation('/auth/sso/callback')
    const seen: Array<string | undefined> = []
    const { api, exchangeSsoCode } = await loadTab(async (config) => {
      if (config.url === '/auth/refresh') {
        setCookie('whm_csrf=csrf-of-b; path=/')
        return ok(config, { data: { expires_in: 900, user: { id: 'user-b' } } })
      }
      seen.push(config.headers.get('X-Organization-ID') as string | undefined)
      return ok(config, { data: { id: 'user-b' } })
    })

    await exchangeSsoCode('code-b')
    expect(localStorage.getItem('user')).toBeNull()
    expect(localStorage.getItem('selected_organization_id')).toBeNull()
    await api.get('/me')

    expect(seen).toEqual([undefined])
  })

  it("with Web Locks too, another tab's session change aborts this tab's refresh in flight", async () => {
    installWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    const signals: AbortSignal[] = []
    recordPosts([], (label, options) => {
      if (label !== 'refresh') return undefined
      signals.push(options.signal)
      return new Promise((_, reject) => {
        options.signal.addEventListener('abort', () => reject(new AxiosError('canceled', AxiosError.ERR_CANCELED)))
      })
    })
    const { api } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const request = api.get('/contacts').then(() => 'resolved', () => 'rejected')
    await vi.waitFor(() => expect(signals).toHaveLength(1))

    localStorage.setItem('whm_session_id', 'session-b')
    window.dispatchEvent(new StorageEvent('storage', { key: 'whm_session_id', newValue: 'session-b' }))

    expect(signals[0].aborted).toBe(true)
    expect(await request).toBe('rejected')
  })

  it('a no-answer sign-in leaves alone a session another tab began meanwhile', async () => {
    installWebLocks()
    const order: string[] = []
    const { api } = await loadTab(async (config) => {
      // Meanwhile an IdP-initiated SSO callback (no lock) set B's cookies and
      // began B's session, as its page load does.
      setCookie('whm_csrf=csrf-of-b; path=/')
      localStorage.setItem('whm_session_id', 'session-b')
      localStorage.setItem('whm_session_begun', JSON.stringify({ marker: 'session-b', csrf: 'csrf-of-b' }))
      order.push('login (no answer)')
      throw new AxiosError('timeout of 15000ms exceeded', AxiosError.ECONNABORTED, config)
    })
    recordPosts(order)

    await api.post('/auth/login', { email: 'a@example.com', password: 'x' }).catch(() => undefined)

    expect(order).toEqual(['login (no answer)'])
    expect(localStorage.getItem('whm_session_id')).toBe('session-b')
  })

  it('a no-answer sign-in whose late cookies replaced a newer session undoes them', async () => {
    installWebLocks()
    const order: string[] = []
    const { api } = await loadTab(async (config) => {
      // B began elsewhere meanwhile...
      localStorage.setItem('whm_session_id', 'session-b')
      localStorage.setItem('whm_session_begun', JSON.stringify({ marker: 'session-b', csrf: 'csrf-of-b' }))
      // ...and then this request's own cookies landed over B's.
      setCookie('whm_csrf=csrf-of-this-sign-in; path=/')
      order.push('login (no answer)')
      throw new AxiosError('timeout of 15000ms exceeded', AxiosError.ECONNABORTED, config)
    })
    recordPosts(order)

    await api.post('/auth/login', { email: 'a@example.com', password: 'x' }).catch(() => undefined)

    expect(order).toEqual(['login (no answer)', 'revoke csrf-of-this-sign-in', 'logout csrf-of-this-sign-in'])
  })

  it('a switch-org without an answer and with the CSRF cookie unchanged undoes nothing', async () => {
    installWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    const order: string[] = []
    const { api } = await loadTab(async (config) => {
      order.push('switch-org (no answer)')
      throw new AxiosError('Network Error', AxiosError.ERR_NETWORK, config)
    })
    recordPosts(order)

    await api.post('/auth/switch-org', { organization_id: 'org-2' }).catch(() => undefined)

    expect(order).toEqual(['switch-org (no answer)'])
    expect(localStorage.getItem('whm_session_id')).toBe('session-a')
    expect(document.cookie).toContain('whm_csrf=csrf-token-1')
  })

  it('a switch-org is refused, sending nothing, once another tab began a new session', async () => {
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    localStorage.setItem('whm_session_id', 'session-a')
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config, { data: { expires_in: 900, user: {} } }))
    const { api } = await loadTab(adapter)
    const order: string[] = []
    recordPosts(order)

    const switched = api.post('/auth/switch-org', { organization_id: 'org-2' }).catch((e) => e.code)
    await tick()
    // B signs in elsewhere while the switch waits: a live marker, B's cookies.
    localStorage.setItem('whm_session_id', 'session-b')
    setCookie('whm_csrf=csrf-of-b; path=/')
    localStorage.setItem('whm_session_begun', JSON.stringify({ marker: 'session-b', csrf: 'csrf-of-b' }))
    const userBefore = localStorage.getItem('user')
    releaseHolder()

    expect(await switched).toBe('ERR_CANCELED')
    expect(adapter).not.toHaveBeenCalled()
    expect(order).toEqual([])
    expect(localStorage.getItem('user')).toBe(userBefore)
    expect(localStorage.getItem('whm_session_id')).toBe('session-b')
    expect(document.cookie).toContain('whm_csrf=csrf-of-b')
  })

  it('the lock is let go even when undoing an unanswered sign-in throws', async () => {
    vi.useFakeTimers()
    const cookie = Object.getOwnPropertyDescriptor(Document.prototype, 'cookie')!
    const signIn = await loadTab(async (config) => {
      // The jar cannot be read any more by the time the request fails (a
      // revoked storage backend, say): the undo throws.
      Object.defineProperty(document, 'cookie', {
        configurable: true,
        get() {
          throw new DOMException('Access denied', 'SecurityError')
        },
        set(value: string) {
          cookie.set!.call(document, value)
        },
      })
      throw new AxiosError('timeout of 15000ms exceeded', AxiosError.ECONNABORTED, config)
    })
    const other = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const order: string[] = []
    recordPosts(order, (label) => (label === 'refresh' ? refreshOk() : undefined))

    const login = signIn.api.post('/auth/login', { email: 'a@example.com', password: 'x' }).catch(() => undefined)
    await vi.advanceTimersByTimeAsync(10)
    await login
    delete (document as unknown as { cookie?: string }).cookie
    void other.api.get('/contacts').catch(() => undefined)
    await vi.advanceTimersByTimeAsync(100)

    // Not held until the 60 s backstop.
    expect(order).toEqual(['refresh'])
  })

  it('a no-answer sign-in is undone before the lock is let go', async () => {
    installWebLocks()
    const order: string[] = []
    const signIn = await loadTab(async (config) => {
      setCookie('whm_csrf=csrf-of-b; path=/')
      order.push('login (no answer)')
      throw new AxiosError('timeout of 15000ms exceeded', AxiosError.ECONNABORTED, config)
    })
    const other = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    let releaseRevoke!: () => void
    const revokeGate = new Promise<void>((resolve) => {
      releaseRevoke = resolve
    })
    recordPosts(order, async (label) => {
      if (label === 'refresh') return refreshOk()
      if (label.startsWith('revoke')) await revokeGate
      return undefined
    })

    const login = signIn.api.post('/auth/login', { email: 'b@example.com', password: 'x' }).catch(() => undefined)
    await vi.waitFor(() => expect(order).toEqual(['login (no answer)', 'revoke csrf-of-b']))
    // Another tab's refresh queues on the lock while the undo runs.
    const request = other.api.get('/contacts').catch(() => undefined)
    await tick()
    releaseRevoke()
    await login
    await request

    expect(order.slice(0, 4)).toEqual(['login (no answer)', 'revoke csrf-of-b', 'logout csrf-of-b', 'refresh'])
  })

  it("the begun session's CSRF record follows its org switches", async () => {
    installWebLocks()
    const { api } = await loadTab(async (config) => {
      if (config.url === '/auth/switch-org') setCookie('whm_csrf=csrf-after-switch; path=/')
      return ok(config, { data: { expires_in: 900, user: {} } })
    })
    await api.post('/auth/login', { email: 'a@example.com', password: 'x' })
    const marker = localStorage.getItem('whm_session_id')

    await api.post('/auth/switch-org', { organization_id: 'org-2' })

    expect(JSON.parse(localStorage.getItem('whm_session_begun')!)).toEqual({ marker, csrf: 'csrf-after-switch' })
  })

  it('a retried switch refused after a build without the begun record signed in B leaves B alone', async () => {
    installWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const order: string[] = []
    let switchCalls = 0
    const { api } = await loadTab(async (config) => {
      switchCalls++
      throw httpError(401, config)
    })
    recordPosts(order, async (label) => {
      if (label === 'refresh') {
        await refreshGate
        setCookie('whm_csrf=csrf-token-2; path=/')
        return refreshOk()
      }
    })

    const switched = api.post('/auth/switch-org', { organization_id: 'org-2' }).catch((e) => e.code)
    await vi.waitFor(() => expect(order).toEqual(['refresh']))
    // A tab of an older build signs in as B on the lock meanwhile, and writes
    // no begun record: only this request's own fields can tell it apart.
    let releaseHolder!: () => void
    const holding = new Promise<void>((acquired) => {
      void (navigator as unknown as { locks: { request: (name: string, cb: () => Promise<void>) => Promise<void> } })
        .locks.request('whm-token-refresh', () => {
          acquired()
          return new Promise<void>((resolve) => {
            releaseHolder = resolve
          })
        })
    })
    releaseRefresh()
    await holding
    // The switch's retry is queued on the lock by now.
    await tick()
    setCookie('whm_csrf=csrf-of-b; path=/')
    localStorage.setItem('whm_session_id', 'session-b')
    releaseHolder()

    expect(await switched).toBe('ERR_CANCELED')
    expect(switchCalls).toBe(1)
    expect(order).toEqual(['refresh'])
    expect(localStorage.getItem('whm_session_begun')).toBeNull()
    expect(document.cookie).toContain('whm_csrf=csrf-of-b')
  })

  it('a retried switch refused after another tab began B leaves B alone', async () => {
    installWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const order: string[] = []
    let switchCalls = 0
    const switching = await loadTab(async (config) => {
      switchCalls++
      // The first attempt hits an expired access token.
      throw httpError(401, config)
    })
    const signingIn = await loadTab(async (config) => {
      order.push('login B')
      setCookie('whm_csrf=csrf-of-b; path=/')
      return ok(config, { data: { expires_in: 900, user: {} } })
    })
    recordPosts(order, async (label) => {
      if (label === 'refresh') {
        await refreshGate
        // The refresh rotates the session's CSRF value.
        setCookie('whm_csrf=csrf-token-2; path=/')
        return refreshOk()
      }
    })

    const switched = switching.api.post('/auth/switch-org', { organization_id: 'org-2' }).catch((e) => e.code)
    await vi.waitFor(() => expect(order).toEqual(['refresh']))
    // B's sign-in queues on the lock during the refresh.
    const login = signingIn.api.post('/auth/login', { email: 'b@example.com', password: 'x' })
    await tick()
    releaseRefresh()
    await login
    const sessionB = localStorage.getItem('whm_session_id')

    expect(await switched).toBe('ERR_CANCELED')
    expect(switchCalls).toBe(1)
    expect(order).toEqual(['refresh', 'login B'])
    expect(localStorage.getItem('whm_session_id')).toBe(sessionB)
    expect(document.cookie).toContain('whm_csrf=csrf-of-b')
  })

  it("the begun session's CSRF record follows its refreshes", async () => {
    installWebLocks()
    let rotated = false
    const { api } = await loadTab(async (config) => {
      if (config.url === '/auth/login') return ok(config, { data: { expires_in: 900, user: {} } })
      if (!rotated) throw httpError(401, config)
      return ok(config)
    })
    recordPosts([], (label) => {
      if (label === 'refresh') {
        rotated = true
        setCookie('whm_csrf=csrf-token-2; path=/')
        return refreshOk()
      }
    })
    await api.post('/auth/login', { email: 'a@example.com', password: 'x' })
    const marker = localStorage.getItem('whm_session_id')
    expect(JSON.parse(localStorage.getItem('whm_session_begun')!)).toEqual({ marker, csrf: 'csrf-token-1' })

    await api.get('/contacts')

    expect(JSON.parse(localStorage.getItem('whm_session_begun')!)).toEqual({ marker, csrf: 'csrf-token-2' })
  })

  it('a refresh that lands after its session ended updates the pending logout\'s CSRF value', async () => {
    installWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const order: string[] = []
    recordPosts(order, async (label) => {
      if (label === 'refresh') {
        await refreshGate
        // The server rotated the session: new cookies.
        setCookie('whm_csrf=csrf-token-2; path=/')
        return refreshOk()
      }
      // The outage goes on: revoke and logout fail.
      if (label.startsWith('revoke')) throw httpError(503)
      throw networkError()
    })
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const request = api.get('/contacts').catch(() => undefined)
    await vi.waitFor(() => expect(order).toEqual(['refresh']))
    await api.post('/auth/logout', {})
    releaseRefresh()
    await request
    await whenLogoutSettled()

    expect(order).toEqual(['refresh', 'revoke csrf-token-2', 'logout csrf-token-2'])
    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!)).toEqual({
      marker: 'ended:session-a',
      csrf: 'csrf-token-2',
    })
  })

  it("a refresh that lands after its session ended updates the pending logout's CSRF value even when that logout is deferred", async () => {
    vi.useFakeTimers()
    installWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const order: string[] = []
    recordPosts(order, async (label) => {
      if (label === 'refresh') {
        await refreshGate
        setCookie('whm_csrf=csrf-token-2; path=/')
        return refreshOk()
      }
    })
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const request = api.get('/contacts').catch(() => undefined)
    await vi.waitFor(() => expect(order).toEqual(['refresh']))
    // A stuck holder queues behind the refresh: this logout's lock wait runs
    // out and it is deferred before it sends anything.
    holdLockElsewhere()
    await api.post('/auth/logout', {})
    releaseRefresh()
    await vi.advanceTimersByTimeAsync(20_500)
    await request
    await whenLogoutSettled()

    expect(order).toEqual(['refresh'])
    // What a later page (the replayed-callback check, the load retry) matches
    // against is the jar's rotated value.
    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!)).toEqual({
      marker: 'ended:session-a',
      csrf: 'csrf-token-2',
    })
  })

  it("another tab's SSO start leaves an open tab alone: it writes nothing, and the open tab refreshes as usual", async () => {
    localStorage.setItem('whm_session_id', 'session-1')
    let rotated = false
    const open = await loadTab(async (config) => {
      if (!rotated) throw httpError(401, config)
      return ok(config)
    })
    const starting = await loadTab(async (config) => ok(config))
    recordPosts([], (label) => {
      if (label === 'refresh') {
        rotated = true
        return refreshOk()
      }
    })
    const before = { ...localStorage }

    await starting.beginSsoSignIn()

    expect({ ...localStorage }).toEqual(before)
    expect((await open.api.get('/contacts')).status).toBe(200)
    expect(rotated).toBe(true)
    expect(localStorage.getItem('whm_session_id')).toBe('session-1')
  })

  it('an SSO exchange that starts while a logout is queued goes after it, and is never revoked by it', async () => {
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const ending = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order)
    await ending.api.post('/auth/logout', {})

    // In another tab, the callback page exchanges B's code meanwhile.
    stubLocation('/auth/sso/callback')
    const callback = await loadTab(async (config) => {
      order.push('exchange')
      setCookie('whm_csrf=csrf-of-b; path=/')
      return ok(config, { data: { expires_in: 900, user: { id: 'user-b' } } })
    })
    const exchange = callback.exchangeSsoCode('code-b')
    await tick()
    expect(order).toEqual([])
    releaseHolder()
    await exchange
    await ending.whenLogoutSettled()

    // The old logout went first, with the old cookies; nothing after B's.
    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1', 'exchange'])
    expect(localStorage.getItem('whm_session_id')).toMatch(/^(?!ended:)./)
  })

  it('starting SSO waits for nothing and changes nothing: the exchange does the ordering', async () => {
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const fetchSpy = vi.fn(async () => new Response('{}'))
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem('whm_session_id', 'session-a')
    const { beginSsoSignIn } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order)
    const before = { ...localStorage }

    await beginSsoSignIn()

    expect(order).toEqual([])
    expect({ ...localStorage }).toEqual(before)
    // Leaving for the provider sends nothing either.
    window.dispatchEvent(new Event('pagehide'))
    expect(fetchSpy).not.toHaveBeenCalled()
    releaseHolder()
    vi.unstubAllGlobals()
  })

  it('a callback URL replayed with the ended session\'s cookies finishes its logout', async () => {
    localStorage.setItem('whm_session_id', 'ended:session-a')
    localStorage.setItem('whm_logout_pending', JSON.stringify({ marker: 'ended:session-a', csrf: 'csrf-token-1' }))
    const order: string[] = []
    recordPosts(order)
    stubLocation('/auth/sso/callback')

    const { whenLogoutSettled } = await loadTab(async (config) => ok(config))
    await whenLogoutSettled()

    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1'])
    expect(localStorage.getItem('whm_session_id')).toBe('ended:session-a')
    expect(localStorage.getItem('whm_logout_pending')).toBeNull()
  })

  for (const locking of ['Web Locks', 'no Web Locks (a session from before)']) {
    it(`a 401 that arrives after the logout never refreshes or replays (${locking})`, async () => {
      if (locking !== 'Web Locks') removeWebLocks()
      localStorage.setItem('whm_session_id', 'session-a')
      const held = gatedAdapter()
      const { api, whenLogoutSettled } = await loadTab(held.adapter)
      const order: string[] = []
      recordPosts(order, (label) => (label === 'refresh' ? refreshOk() : undefined))

      const request = api.get('/contacts').then(() => 'resolved', () => 'rejected')
      await held.inAdapter
      await api.post('/auth/logout', {})
      held.release()

      expect(await request).toBe('rejected')
      await whenLogoutSettled()
      // Without Web Locks the logout is cookie-neutral: the revoke only.
      expect(order).toEqual(
        locking === 'Web Locks' ? ['revoke csrf-token-1', 'logout csrf-token-1'] : ['revoke csrf-token-1'],
      )
      expect(assignedHref).toBeNull()
    })
  }

  it('a 401 within one session still refreshes and retries', async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    let rotated = false
    const { api } = await loadTab(async (config) => {
      if (!rotated) throw httpError(401, config)
      return ok(config)
    })
    recordPosts([], (label) => {
      if (label === 'refresh') {
        rotated = true
        return refreshOk()
      }
    })

    expect((await api.get('/contacts')).status).toBe(200)
    expect(localStorage.getItem('whm_session_id')).toBe('session-a')
  })

  it('a page without Web Locks takes no pending logout over: nothing at load, at a hand-over or at pagehide', async () => {
    removeWebLocks()
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    FakeBroadcastChannel.channels.clear()
    const fetchSpy = vi.fn(async () => new Response('{}'))
    vi.stubGlobal('fetch', fetchSpy)
    // A logout of this browser is still pending (its cookies to be cleared).
    localStorage.setItem('whm_session_id', 'ended:session-a')
    localStorage.setItem('whm_logout_pending', JSON.stringify({ marker: 'ended:session-a', csrf: 'csrf-token-1' }))
    const order: string[] = []
    recordPosts(order)

    const { whenLogoutSettled } = await loadTab(async (config) => ok(config))
    await whenLogoutSettled()
    // A closing tab hands it over...
    new FakeBroadcastChannel('whm-tabs').postMessage({ type: 'bye', id: 'closing-tab' })
    await tick()
    await whenLogoutSettled()
    // ...and this page closes too.
    window.dispatchEvent(new Event('pagehide'))

    expect(order).toEqual([])
    expect(fetchSpy).not.toHaveBeenCalled()
    // Left for a page that holds the lock.
    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!).marker).toBe('ended:session-a')
    FakeBroadcastChannel.channels.clear()
  })

  it('without Web Locks the revoke goes out with keepalive, cookie-neutral (the page may leave at once)', async () => {
    removeWebLocks()
    const fetchSpy = vi.fn(async (..._args: unknown[]) => new Response('{}'))
    vi.stubGlobal('fetch', fetchSpy)
    localStorage.setItem('whm_session_id', 'session-a')
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    recordPosts([])

    await api.post('/auth/logout', {})
    await whenLogoutSettled()

    expect(postSpy).not.toHaveBeenCalled()
    expect(fetchSpy).toHaveBeenCalledTimes(1)
    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit]
    expect(url).toMatch(/\/api\/auth\/refresh$/)
    expect(init).toMatchObject({ method: 'POST', credentials: 'include', keepalive: true })
    expect((init.headers as Record<string, string>)['X-CSRF-Token']).toBe('csrf-token-1')
    expect(JSON.parse(init.body as string)).toEqual({ revoke: true, keep_cookies: true })
  })

  it('a page without Web Locks that logged out sends no keepalive revoke when it closes', async () => {
    removeWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order)

    await api.post('/auth/logout', {})
    await whenLogoutSettled()
    // Its own logout is still pending (the clearing waits for the lock).
    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!).marker).toBe('ended:session-a')
    window.dispatchEvent(new Event('pagehide'))

    // Its one revoke (a keepalive fetch, cookie-neutral), and nothing more.
    expect(order).toEqual(['revoke csrf-token-1'])
  })

  it('without Web Locks a revoke retried after a 429 still presents the recorded CSRF value', async () => {
    vi.useFakeTimers()
    removeWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    let first = true
    recordPosts(order, (label) => {
      if (label.startsWith('revoke') && first) {
        first = false
        // A newer session's cookies land meanwhile (its marker not yet).
        setCookie('whm_csrf=csrf-of-a-newer-session; path=/')
        throw httpError(429)
      }
    })

    await api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(1_000)
    await whenLogoutSettled()

    expect(order).toEqual(['revoke csrf-token-1', 'revoke csrf-token-1'])
  })

  it('without Web Locks the revoke presents the CSRF value recorded for its session, and never retries with a newer one', async () => {
    removeWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    const { api, whenLogoutSettled } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order, (label) => {
      if (label.startsWith('revoke')) {
        // A newer session's cookies land (another tab signed in) before its
        // marker does: the server sees a CSRF mismatch.
        setCookie('whm_csrf=csrf-of-a-newer-session; path=/')
        throw httpError(403)
      }
    })

    await api.post('/auth/logout', {})
    await whenLogoutSettled()

    expect(order).toEqual(['revoke csrf-token-1'])
  })

  it('an SSO exchange sent and never answered here (the tab reloads) leaves a note: the next page finds it interrupted', async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    // The answer never comes: the tab goes away while it is out.
    const tab = await loadTab(() => new Promise(() => undefined))
    recordPosts([])
    void tab.exchangeSsoCode('code-b').catch(() => undefined)
    await new Promise((resolve) => setTimeout(resolve, 20))

    const reloaded = await loadTab(async (config) => ok(config))
    expect(reloaded.ssoExchangeInterrupted()).toBe(true)
    // Read once.
    expect(reloaded.ssoExchangeInterrupted()).toBe(false)
  })

  it('an SSO exchange whose answer was seen leaves no note (answered, or refused)', async () => {
    for (const refused of [false, true]) {
      sessionStorage.clear()
      const { exchangeSsoCode, ssoExchangeInterrupted } = await loadTab(async (config) => {
        if (refused) throw httpError(401, config)
        setCookie('whm_csrf=csrf-of-b; path=/')
        return ok(config, { data: { expires_in: 900, user: { id: 'user-b' } } })
      })
      recordPosts([])
      await exchangeSsoCode('code-b').catch(() => undefined)

      // Cleared as the answer was seen.
      expect(sessionStorage.getItem('whm_sso_sent')).toBeNull()
      expect(ssoExchangeInterrupted()).toBe(false)
    }
  })

  it('a session that began in another tab after the exchange was sent, consistent with the jar, is not an interruption', async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    const tab = await loadTab(() => new Promise(() => undefined))
    recordPosts([])
    void tab.exchangeSsoCode('code-b').catch(() => undefined)
    await new Promise((resolve) => setTimeout(resolve, 20))
    // Another tab signs in: its marker, its cookies, its begun record.
    localStorage.setItem('whm_session_id', 'session-c')
    setCookie('whm_csrf=csrf-of-c; path=/')
    localStorage.setItem('whm_session_begun', JSON.stringify({ marker: 'session-c', csrf: 'csrf-of-c' }))

    const reloaded = await loadTab(async (config) => ok(config))
    expect(reloaded.ssoExchangeInterrupted()).toBe(false)
  })

  it.each([
    ['the session from before the exchange (nothing changed)', 'csrf-token-1'],
    ['a session whose cookies are not its own (the answer landed unseen)', 'csrf-of-the-unseen-answer'],
  ])('%s is an interruption', async (_label, jar) => {
    localStorage.setItem('whm_session_id', 'session-a')
    localStorage.setItem('whm_session_begun', JSON.stringify({ marker: 'session-a', csrf: 'csrf-token-1' }))
    setCookie('whm_csrf=csrf-token-1; path=/')
    const tab = await loadTab(() => new Promise(() => undefined))
    recordPosts([])
    void tab.exchangeSsoCode('code-b').catch(() => undefined)
    await new Promise((resolve) => setTimeout(resolve, 20))
    // The answer may have landed unseen (new cookies), or not at all.
    setCookie(`whm_csrf=${jar}; path=/`)
    if (jar !== 'csrf-token-1') {
      // A session marker begun elsewhere whose record does not match the jar.
      localStorage.setItem('whm_session_id', 'session-c')
      localStorage.setItem('whm_session_begun', JSON.stringify({ marker: 'session-c', csrf: 'csrf-of-c' }))
    }

    const reloaded = await loadTab(async (config) => ok(config))
    expect(reloaded.ssoExchangeInterrupted()).toBe(true)
  })

  it('a request sent as user A is never replayed after user B signs in', async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    const held = gatedAdapter()
    const { api, whenLogoutSettled } = await loadTab(held.adapter)
    const order: string[] = []
    recordPosts(order, (label) => (label === 'refresh' ? refreshOk() : undefined))

    const request = api.get('/contacts').then(() => 'resolved', () => 'rejected')
    await held.inAdapter
    await api.post('/auth/logout', {})
    await whenLogoutSettled()
    // B, another user in another organization, signs in.
    await api.post('/auth/login', { email: 'b@example.com', password: 'x' })
    localStorage.setItem('user', JSON.stringify({ id: 'user-b', email: 'b@example.com', organization_id: 'org-b' }))
    localStorage.setItem('selected_organization_id', 'org-b')
    held.release()

    expect(await request).toBe('rejected')
    expect(held.replayed()).toBe(false)
    expect(order).not.toContain('refresh')
    expect(JSON.parse(localStorage.getItem('user')!).id).toBe('user-b')
    expect(assignedHref).toBeNull()
  })

  it('a request sent under the old session is never replayed under the new one', async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    const held = gatedAdapter()
    const { api, whenLogoutSettled } = await loadTab(held.adapter)
    const order: string[] = []
    recordPosts(order, (label) => (label === 'refresh' ? refreshOk() : undefined))

    const request = api.get('/contacts').then(() => 'resolved', () => 'rejected')
    await held.inAdapter
    await api.post('/auth/logout', {})
    await whenLogoutSettled()
    await api.post('/auth/login', { email: 'b@example.com', password: 'x' })
    const sessionB = localStorage.getItem('whm_session_id')
    held.release()

    expect(await request).toBe('rejected')
    expect(order).not.toContain('refresh')
    expect(held.replayed()).toBe(false)
    // B's session is untouched: no expiry, no replay.
    expect(localStorage.getItem('whm_session_id')).toBe(sessionB)
    expect(localStorage.getItem('user')).not.toBeNull()
    expect(assignedHref).toBeNull()
  })

  it('a tab still showing a session another tab ended leaves it without refreshing', async () => {
    const { api } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const order: string[] = []
    recordPosts(order)
    localStorage.setItem('whm_session_id', 'ended:')

    await expect(api.get('/contacts')).rejects.toBeTruthy()

    expect(order).toEqual([])
    expect(assignedHref).toBe('/login')
    expect(localStorage.getItem('whm_session_id')).toBe('ended:')
  })

  it("a logout waits for this tab's org switch, which it then undoes", async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    let answer!: () => void
    const gate = new Promise<void>((resolve) => {
      answer = resolve
    })
    const order: string[] = []
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      if (config.url === '/auth/switch-org') {
        await gate
        order.push('switch-org answered')
        return ok(config, { data: { expires_in: 900, user: {} } })
      }
      return ok(config)
    })
    recordPosts(order)

    const switched = api.post('/auth/switch-org', { organization_id: 'org-2' }).then(
      () => 'applied',
      (error: AxiosError) => error.code,
    )
    await tick()
    await api.post('/auth/logout', {})
    await tick()
    expect(order).toEqual([])

    answer()
    expect(await switched).toBe('ERR_CANCELED')
    await whenLogoutSettled()
    expect(order[0]).toBe('switch-org answered')
    expect(order.slice(1, 3)).toEqual(['revoke csrf-token-1', 'logout csrf-token-1'])
  })

  for (const failure of ['throws', 'rejects'] as const) {
    it(`a lock manager that ${failure} counts as none: the sign-in is refused at once, a logout is cookie-neutral`, async () => {
      vi.useFakeTimers()
      localStorage.setItem('whm_session_id', 'session-a')
      const request = vi.fn((..._args: unknown[]) => {
        if (failure === 'throws') throw new DOMException('Lock manager unavailable', 'SecurityError')
        return Promise.reject(new DOMException('Lock manager unavailable', 'SecurityError'))
      })
      Object.defineProperty(navigator, 'locks', { configurable: true, value: { request } })
      const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config, { data: { expires_in: 900, user: {} } }))
      const { api, sessionsSupported, whenLogoutSettled, SESSION_UNSUPPORTED_CODE } = await loadTab(adapter)
      const order: string[] = []
      recordPosts(order)
      expect(sessionsSupported()).toBe(true)

      const login = api.post('/auth/login', { email: 'b@example.com', password: 'x' }).catch((e) => e.code)
      await vi.advanceTimersByTimeAsync(10)

      // Not "busy" after 20 s: unsupported, now, and nothing was sent.
      expect(await login).toBe(SESSION_UNSUPPORTED_CODE)
      expect(adapter).not.toHaveBeenCalled()
      expect(sessionsSupported()).toBe(false)
      // A session that already existed still logs out, cookie-neutral: the
      // revoke only; the clearing stays pending for a page that holds the lock.
      await api.post('/auth/logout', {})
      await vi.advanceTimersByTimeAsync(10)
      await whenLogoutSettled()
      expect(order).toEqual(['revoke csrf-token-1'])
      expect(JSON.parse(localStorage.getItem('whm_logout_pending')!).marker).toBe('ended:session-a')
    })
  }

  it('a lock manager that rejects ends a session that existed when it would need a refresh', async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    Object.defineProperty(navigator, 'locks', {
      configurable: true,
      value: { request: vi.fn(() => Promise.reject(new DOMException('Lock manager unavailable', 'SecurityError'))) },
    })
    const { api, sessionsSupported, whenLogoutSettled } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const order: string[] = []
    recordPosts(order)

    await expect(api.get('/contacts')).rejects.toMatchObject({ response: { status: 401 } })
    await whenLogoutSettled()

    expect(order).toEqual(['revoke csrf-token-1'])
    expect(sessionsSupported()).toBe(false)
    expect(localStorage.getItem('whm_session_id')).toBe('ended:session-a')
    expect(localStorage.getItem('user')).toBeNull()
    expect(assignedHref).toBe('/login')
  })

  it('a registration answered after the session changed elsewhere reports that it was sent; a refused switch does not', async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    let answer!: () => void
    const gate = new Promise<void>((resolve) => {
      answer = resolve
    })
    const { api, sentBeforeSessionChange } = await loadTab(async (config) => {
      if (config.url === '/auth/register') {
        await gate
        return ok(config, { data: { expires_in: 900, user: {} } })
      }
      return ok(config)
    })
    recordPosts([])

    const registration = api.post('/auth/register', { email: 'b@example.com' }).catch((e) => e)
    await tick()
    // An unrelated stored session ends in another tab meanwhile.
    localStorage.setItem('whm_session_id', 'ended:session-a')
    answer()
    const error = await registration

    expect(sentBeforeSessionChange(error)).toBe(true)
    // Refused before it was sent: nothing reached the server.
    const switched = await api.post('/auth/switch-org', { organization_id: 'org-2' }).catch((e) => e)
    expect(sentBeforeSessionChange(switched)).toBe(false)
    expect(sentBeforeSessionChange(new Error('other'))).toBe(false)
  })

  for (const probe of ['rejects', 'throws'] as const) {
    it(`a lock manager whose query ${probe} counts as none from load, before any sign-in`, async () => {
      Object.defineProperty(navigator, 'locks', {
        configurable: true,
        value: {
          request: vi.fn(),
          query: vi.fn(() => {
            if (probe === 'throws') throw new DOMException('Lock manager unavailable', 'SecurityError')
            return Promise.reject(new DOMException('Lock manager unavailable', 'SecurityError'))
          }),
        },
      })
      const { sessionsSupported } = await loadTab(async (config) => ok(config))
      await tick()

      expect(sessionsSupported()).toBe(false)
    })
  }

  it('a working lock manager passes the load probe', async () => {
    Object.defineProperty(navigator, 'locks', {
      configurable: true,
      value: { request: vi.fn(), query: vi.fn(async () => ({ held: [], pending: [] })) },
    })
    const { sessionsSupported } = await loadTab(async (config) => ok(config))
    await tick()

    expect(sessionsSupported()).toBe(true)
  })

  it('without Web Locks no sign-in, registration, org switch or SSO start is sent', async () => {
    removeWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config, { data: { expires_in: 900, user: {} } }))
    const { api, beginSsoSignIn, SESSION_UNSUPPORTED_CODE } = await loadTab(adapter)
    const order: string[] = []
    recordPosts(order)

    for (const url of ['/auth/login', '/auth/register', '/auth/switch-org']) {
      await expect(api.post(url, { email: 'b@example.com', password: 'x' })).rejects.toMatchObject({
        code: SESSION_UNSUPPORTED_CODE,
      })
    }
    await expect(beginSsoSignIn()).rejects.toMatchObject({ code: SESSION_UNSUPPORTED_CODE })

    expect(SESSION_UNSUPPORTED_CODE).not.toBe('ERR_SESSION_BUSY')
    expect(adapter).not.toHaveBeenCalled()
    expect(order).toEqual([])
    expect(localStorage.getItem('whm_session_id')).toBe('session-a')
    expect(document.cookie).toContain('whm_csrf=csrf-token-1')
  })

  it("the logout's own note records a CSRF value rotated by a refresh that then failed", async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    let failRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      failRefresh = resolve
    })
    const order: string[] = []
    recordPosts(order, async (label) => {
      if (label === 'refresh') {
        await refreshGate
        // The server rotated the session, but the answer never reached this
        // tab's code (cancelled): only the jar shows it.
        setCookie('whm_csrf=csrf-token-2; path=/')
        throw new AxiosError('canceled', AxiosError.ERR_CANCELED)
      }
      // The outage goes on: revoke and logout fail.
      if (label.startsWith('revoke')) throw httpError(503)
      throw networkError()
    })
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const request = api.get('/contacts').catch(() => undefined)
    await vi.waitFor(() => expect(order).toEqual(['refresh']))
    await api.post('/auth/logout', {})
    failRefresh()
    await request
    await whenLogoutSettled()

    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!)).toEqual({
      marker: 'ended:session-a',
      csrf: 'csrf-token-2',
    })
  })

  // A transport that never answers (it ignores the request timeout) but, like
  // XHR and fetch, settles once aborted, after `settleMs`.
  function hungUntilAborted(settleMs = 0) {
    return (config: InternalAxiosRequestConfig) =>
      new Promise((_, reject) => {
        config.signal?.addEventListener?.('abort', () => {
          setTimeout(() => reject(new AxiosError('canceled', AxiosError.ERR_CANCELED, config)), settleMs)
        })
      })
  }

  it('the SSO code exchange begins the new session under the lock, like a password sign-in', async () => {
    localStorage.setItem('whm_session_id', 'ended:session-a')
    localStorage.setItem('user', JSON.stringify({ id: 'user-a', email: 'a@example.com' }))
    const sent: Array<{ url?: string; data: unknown }> = []
    const { exchangeSsoCode } = await loadTab(async (config) => {
      sent.push({ url: config.url, data: JSON.parse(config.data) })
      setCookie('whm_csrf=csrf-of-b; path=/')
      return ok(config, { data: { expires_in: 900, user: { id: 'user-b' } } })
    })
    recordPosts([])
    const request = vi.mocked(navigator.locks.request)

    await exchangeSsoCode('code-b')

    expect(request).toHaveBeenCalled()
    expect(sent).toEqual([{ url: '/auth/refresh', data: { sso_exchange: 'code-b' } }])
    const marker = localStorage.getItem('whm_session_id')
    expect(marker).toMatch(/^(?!ended:)./)
    expect(JSON.parse(localStorage.getItem('whm_session_begun')!)).toEqual({ marker, csrf: 'csrf-of-b' })
    expect(localStorage.getItem('user')).toBeNull()
    expect(Number(localStorage.getItem('whm_auth_expires_at'))).toBeGreaterThan(Date.now())
  })

  it('an SSO exchange goes after a logout that never got through (the page load finishes it first)', async () => {
    localStorage.setItem('whm_session_id', 'ended:session-a')
    localStorage.setItem('whm_logout_pending', JSON.stringify({ marker: 'ended:session-a', csrf: 'csrf-token-1' }))
    const order: string[] = []
    recordPosts(order)
    stubLocation('/auth/sso/callback')
    const { exchangeSsoCode } = await loadTab(async (config) => {
      order.push('exchange')
      return ok(config, { data: { expires_in: 900, user: {} } })
    })

    await exchangeSsoCode('code-b')

    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1', 'exchange'])
  })

  it('an SSO exchange answered after the session changed elsewhere is undone, and counts as sent', async () => {
    localStorage.setItem('whm_session_id', 'ended:session-a')
    let answer!: () => void
    const gate = new Promise<void>((resolve) => {
      answer = resolve
    })
    const { exchangeSsoCode, sentBeforeSessionChange } = await loadTab(async (config) => {
      await gate
      setCookie('whm_csrf=csrf-of-b; path=/')
      return ok(config, { data: { expires_in: 900, user: {} } })
    })
    const order: string[] = []
    recordPosts(order)

    const exchange = exchangeSsoCode('code-b').catch((e) => e)
    await tick()
    // Another tab signs C in meanwhile.
    localStorage.setItem('whm_session_id', 'session-c')
    answer()
    const error = await exchange

    expect(error.code).toBe('ERR_CANCELED')
    expect(sentBeforeSessionChange(error)).toBe(true)
    expect(order).toEqual(['revoke csrf-of-b', 'logout csrf-of-b'])
    expect(localStorage.getItem('whm_session_id')).toBe('session-c')
  })

  it('an unanswered SSO exchange whose cookies arrived is undone before the lock is let go', async () => {
    localStorage.setItem('whm_session_id', 'ended:session-a')
    const order: string[] = []
    const { exchangeSsoCode } = await loadTab(async (config) => {
      setCookie('whm_csrf=csrf-of-b; path=/')
      order.push('exchange (no answer)')
      throw new AxiosError('timeout of 15000ms exceeded', AxiosError.ECONNABORTED, config)
    })
    recordPosts(order)

    await exchangeSsoCode('code-b').catch(() => undefined)

    expect(order).toEqual(['exchange (no answer)', 'revoke csrf-of-b', 'logout csrf-of-b'])
    expect(localStorage.getItem('whm_session_id')).toBe('ended:session-a')
  })

  it('a used or expired SSO code begins nothing', async () => {
    localStorage.setItem('whm_session_id', 'ended:session-a')
    const { exchangeSsoCode } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const order: string[] = []
    recordPosts(order)

    await expect(exchangeSsoCode('used-code')).rejects.toMatchObject({ response: { status: 401 } })

    expect(order).toEqual([])
    expect(localStorage.getItem('whm_session_id')).toBe('ended:session-a')
    expect(assignedHref).toBeNull()
  })

  it('without Web Locks the SSO exchange is refused and nothing is sent', async () => {
    removeWebLocks()
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config))
    const { exchangeSsoCode, keepSsoCode, unsentSsoCode, SESSION_UNSUPPORTED_CODE } = await loadTab(adapter)
    recordPosts([])
    keepSsoCode('code-b')

    await expect(exchangeSsoCode('code-b')).rejects.toMatchObject({ code: SESSION_UNSUPPORTED_CODE })

    expect(adapter).not.toHaveBeenCalled()
    // Never sent: still kept.
    expect(unsentSsoCode()).toBe('code-b')
  })

  it('a kept SSO code stays kept while its exchange waits for the lock, and is dropped once it is sent', async () => {
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config, { data: { expires_in: 900, user: {} } }))
    const { exchangeSsoCode, keepSsoCode, unsentSsoCode } = await loadTab(adapter)
    recordPosts([])
    const release = holdLockElsewhere()
    keepSsoCode('code-b')

    const exchange = exchangeSsoCode('code-b')
    await new Promise((resolve) => setTimeout(resolve, 20))
    // A reload now would retry it: it was not sent.
    expect(adapter).not.toHaveBeenCalled()
    expect(unsentSsoCode()).toBe('code-b')

    release()
    await exchange
    expect(adapter).toHaveBeenCalledTimes(1)
    expect(unsentSsoCode()).toBeNull()
  })

  it('a kept SSO code is offered for its 60 s life only', async () => {
    vi.useFakeTimers()
    const { keepSsoCode, unsentSsoCode } = await loadTab(async (config) => ok(config))
    keepSsoCode('code-b')

    vi.advanceTimersByTime(59_000)
    expect(unsentSsoCode()).toBe('code-b')
    vi.advanceTimersByTime(2_000)
    expect(unsentSsoCode()).toBeNull()
    expect(sessionStorage.getItem('whm_sso_code')).toBeNull()
  })

  it('reports Web Locks support once the load probe of the lock manager has settled', async () => {
    let refuse!: () => void
    const query = vi.fn(
      () =>
        new Promise((_, reject) => {
          refuse = () => reject(new DOMException('Lock manager unavailable', 'SecurityError'))
        }),
    )
    Object.defineProperty(navigator, 'locks', { configurable: true, value: { request: vi.fn(), query } })
    const { sessionsSupported, whenSessionsSupportKnown } = await loadTab(async (config) => ok(config))
    const known = whenSessionsSupportKnown()
    // Before the probe answers, the page still counts as supported...
    expect(sessionsSupported()).toBe(true)

    refuse()
    // ...and the answer is what the probe found.
    expect(await known).toBe(false)
    expect(sessionsSupported()).toBe(false)
  })

  it('without Web Locks a session that existed is not refreshed: it ends, with a cookie-neutral revoke and no /auth/logout', async () => {
    removeWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const posts: Array<{ url: string; body: unknown }> = []
    postSpy.mockImplementation(async (url: string, body: unknown) => {
      posts.push({ url: url.replace(/^.*\/api/, ''), body })
      return { data: { data: {} }, status: 200 }
    })
    recordFetches((url, body) => {
      posts.push({ url: url.replace(/^.*\/api/, ''), body })
    })

    await expect(api.get('/contacts')).rejects.toMatchObject({ response: { status: 401 } })
    await whenLogoutSettled()

    expect(posts).toEqual([{ url: '/auth/refresh', body: { revoke: true, keep_cookies: true } }])
    expect(localStorage.getItem('whm_session_id')).toBe('ended:session-a')
    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!).marker).toBe('ended:session-a')
    expect(localStorage.getItem('user')).toBeNull()
    expect(assignedHref).toBe('/login')
  })

  it('a page that cannot hold the lock logs out cookie-neutral; a page that can then finishes the clearing', async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    // This page's lock manager refuses (a sandboxed page); the browser's
    // other pages have a working one.
    const refuse = () => Promise.reject(new DOMException('Lock manager unavailable', 'SecurityError'))
    Object.defineProperty(navigator, 'locks', {
      configurable: true,
      value: { request: vi.fn(refuse), query: vi.fn(refuse) },
    })
    const sandboxed = await loadTab(async (config) => ok(config))
    await tick()
    expect(sandboxed.sessionsSupported()).toBe(false)
    const posts: Array<{ url: string; body: unknown }> = []
    postSpy.mockImplementation(async (url: string, body: unknown) => {
      posts.push({ url: url.replace(/^.*\/api/, ''), body })
      return { data: { data: {} }, status: 200 }
    })
    recordFetches((url, body) => {
      posts.push({ url: url.replace(/^.*\/api/, ''), body })
    })

    await sandboxed.api.post('/auth/logout', {})
    await sandboxed.whenLogoutSettled()

    // No cookie change this page cannot order against the others.
    expect(posts).toEqual([{ url: '/auth/refresh', body: { revoke: true, keep_cookies: true } }])
    expect(localStorage.getItem('whm_session_id')).toBe('ended:session-a')
    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!).marker).toBe('ended:session-a')

    // The next page that holds the lock finishes the clearing, ordered.
    installWebLocks()
    const other = await loadTab(async (config) => ok(config))
    await other.whenLogoutSettled()
    expect(posts.slice(1)).toEqual([
      { url: '/auth/refresh', body: { revoke: true } },
      { url: '/auth/logout', body: {} },
    ])
    expect(localStorage.getItem('whm_logout_pending')).toBeNull()
  })

  it("a refresh rejected after this tab's own logout reports no expiry", async () => {
    localStorage.setItem('whm_session_id', 'session-a')
    let failRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      failRefresh = resolve
    })
    const order: string[] = []
    recordPosts(order, async (label) => {
      if (label === 'refresh') {
        await refreshGate
        throw httpError(401)
      }
    })
    const { api, whenLogoutSettled } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const request = api.get('/contacts').catch(() => undefined)
    await vi.waitFor(() => expect(order).toEqual(['refresh']))
    await api.post('/auth/logout', {})
    failRefresh()
    await request
    await whenLogoutSettled()

    // The user logged out: no "session expired" on top of it.
    expect(sessionEvents).not.toContain('expired')
    expect(assignedHref).toBeNull()
  })

  it('a held sign-in lock is released by the backstop if no response ever comes', async () => {
    vi.useFakeTimers()
    installWebLocks()
    const hung = hungUntilAborted()
    const signIn = await loadTab(async (config) => {
      if (config.url === '/auth/login') return hung(config)
      return ok(config)
    })
    const other = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const order: string[] = []
    recordPosts(order, (label) => (label === 'refresh' ? refreshOk() : undefined))

    const login = signIn.api.post('/auth/login', { email: 'a@example.com', password: 'x' }).catch((e) => e.code)
    await vi.advanceTimersByTimeAsync(10)
    void other.api.get('/contacts').catch(() => undefined)
    await vi.advanceTimersByTimeAsync(55_000)
    expect(order).toEqual([])

    await vi.advanceTimersByTimeAsync(6_000)
    expect(await login).toBe('ERR_CANCELED')
    expect(order).toEqual(['refresh'])
  })

  it('the backstop lets go of the lock only once the aborted sign-in has settled', async () => {
    vi.useFakeTimers()
    installWebLocks()
    // Aborted at 60 s, the transport takes 3 s more to settle.
    const hung = hungUntilAborted(3_000)
    let abortedAt = 0
    const signIn = await loadTab(async (config) => {
      if (config.url !== '/auth/login') return ok(config)
      config.signal?.addEventListener?.('abort', () => {
        abortedAt = Date.now()
      })
      return hung(config)
    })
    const other = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const order: string[] = []
    let refreshAt = 0
    recordPosts(order, (label) => {
      if (label === 'refresh') {
        refreshAt = Date.now()
        return refreshOk()
      }
    })
    const start = Date.now()

    void signIn.api.post('/auth/login', { email: 'a@example.com', password: 'x' }).catch(() => undefined)
    await vi.advanceTimersByTimeAsync(10)
    void other.api.get('/contacts').catch(() => undefined)
    await vi.advanceTimersByTimeAsync(61_000)
    // Aborted at the backstop, but still settling: the lock is still held.
    expect(abortedAt - start).toBeGreaterThanOrEqual(60_000)
    expect(order).toEqual([])

    await vi.advanceTimersByTimeAsync(3_000)
    expect(order).toEqual(['refresh'])
    expect(refreshAt - abortedAt).toBeGreaterThanOrEqual(3_000)
  })

  it('a logout that never got through is finished on the next load', async () => {
    const first = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order, (label) => {
      if (label.startsWith('logout')) throw networkError()
    })
    await first.api.post('/auth/logout', {})
    await first.whenLogoutSettled()
    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!)).toMatchObject({ marker: 'ended:' })

    order.length = 0
    recordPosts(order)
    const second = await loadTab(async (config) => ok(config))
    await second.whenLogoutSettled()

    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1'])
    expect(localStorage.getItem('whm_logout_pending')).toBeNull()
  })

  it('a sign-in first finishes a logout that never got through', async () => {
    const order: string[] = []
    recordPosts(order)
    stubLocation('/login')
    const { api } = await loadTab(async (config) => {
      order.push(config.url!)
      return ok(config, { data: { expires_in: 900, user: {} } })
    })
    // Another tab logged out and closed before its logout got through.
    localStorage.setItem('whm_session_id', 'ended:session-a')
    localStorage.setItem('whm_logout_pending', JSON.stringify({ marker: 'ended:session-a', csrf: 'csrf-token-1' }))

    await api.post('/auth/login', { email: 'b@example.com', password: 'x' })

    expect(order.indexOf('logout csrf-token-1')).toBeGreaterThanOrEqual(0)
    expect(order.indexOf('logout csrf-token-1')).toBeLessThan(order.indexOf('/auth/login'))
    expect(localStorage.getItem('whm_logout_pending')).toBeNull()
  })

  it('logs out on a definitive 403 from /auth/refresh', async () => {
    const { api } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    postSpy.mockRejectedValue(httpError(403))

    await expect(api.get('/contacts')).rejects.toBeTruthy()
    expect(localStorage.getItem('user')).toBeNull()
    expect(assignedHref).toBe('/login')
  })

  it('refreshes for /auth/ws-token but never for login/refresh/logout/sso', async () => {
    let rotated = false
    const { api } = await loadTab(async (config) => {
      if (!rotated) throw httpError(401, config)
      return ok(config, { data: { token: 'ws' } })
    })
    postSpy.mockImplementation(async () => {
      rotated = true
      return refreshOk()
    })

    const ws = await api.get('/auth/ws-token')
    expect(ws.status).toBe(200)
    expect(postSpy).toHaveBeenCalledTimes(1)

    rotated = false
    postSpy.mockClear()
    for (const url of ['/auth/login', '/auth/register', '/auth/sso/google/init']) {
      await expect(api.post(url, {})).rejects.toMatchObject({ response: { status: 401 } })
    }
    // No refresh was attempted. (Logout never reaches the adapter at all.)
    expect(postSpy).not.toHaveBeenCalled()
  })

  it('refreshes proactively before expiry while visible, not while hidden', async () => {
    vi.useFakeTimers()
    installWebLocks()
    let visibility: DocumentVisibilityState = 'visible'
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => visibility,
    })
    const { api } = await loadTab(async (config) => ok(config, { data: { expires_in: 900, user: {} } }))
    postSpy.mockResolvedValue(refreshOk(900))

    await api.post('/auth/login', { email: 'a@example.com', password: 'x' })
    expect(Number(localStorage.getItem('whm_auth_expires_at'))).toBeGreaterThan(Date.now())

    await vi.advanceTimersByTimeAsync(839_000)
    expect(postSpy).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1_000)
    expect(postSpy).toHaveBeenCalledTimes(1)

    // Hidden: the next window passes without a refresh…
    visibility = 'hidden'
    await vi.advanceTimersByTimeAsync(900_000)
    expect(postSpy).toHaveBeenCalledTimes(1)

    // …until the tab becomes visible again.
    visibility = 'visible'
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(0)
    expect(postSpy).toHaveBeenCalledTimes(2)

    Reflect.deleteProperty(document, 'visibilityState')
  })
})
