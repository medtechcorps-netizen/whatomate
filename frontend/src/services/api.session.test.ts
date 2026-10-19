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

async function loadTab(adapter: Adapter): Promise<ApiModule> {
  vi.resetModules()
  const mod = await import('./api')
  mod.api.defaults.adapter = vi.fn(adapter) as unknown as typeof mod.api.defaults.adapter
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
    await import('./api')
  }, 60_000)

  beforeEach(() => {
    localStorage.clear()
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

  for (const locking of ['Web Locks', 'no Web Locks (a session from before)']) {
    it(`a logout waits for a refresh in flight, which then leaves no trace (${locking})`, async () => {
      if (locking !== 'Web Locks') removeWebLocks()
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
    const { api, beginSsoSignIn, SESSION_BUSY_CODE } = await loadTab(adapter)
    const marker = localStorage.getItem('whm_session_id')

    const login = api.post('/auth/login', { email: 'b@example.com', password: 'x' })
      .then(() => 'signed in', (error: AxiosError) => error.code)
    const sso = beginSsoSignIn().then(() => 'started', (error: AxiosError) => error.code)
    await vi.advanceTimersByTimeAsync(20_500)

    expect(await login).toBe(SESSION_BUSY_CODE)
    expect(await sso).toBe(SESSION_BUSY_CODE)
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

  it('an SSO start or sign-in is refused while an earlier logout still fails, and that logout stays pending', async () => {
    installWebLocks()
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config, { data: { expires_in: 900, user: {} } }))
    const { api, beginSsoSignIn, SESSION_BUSY_CODE } = await loadTab(adapter)
    // An outage: the logout of session A never got through.
    localStorage.setItem('whm_session_id', 'ended:session-a')
    localStorage.setItem('whm_logout_pending', JSON.stringify({ marker: 'ended:session-a', csrf: 'csrf-token-1' }))
    const order: string[] = []
    recordPosts(order, (label) => {
      if (label.startsWith('revoke')) throw httpError(504)
      if (label.startsWith('logout')) throw networkError()
    })

    await expect(beginSsoSignIn()).rejects.toMatchObject({ code: SESSION_BUSY_CODE })
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

  it("the SSO callback's /me carries no organization a previous session selected", async () => {
    localStorage.setItem('selected_organization_id', 'org-a')
    stubLocation('/auth/sso/callback')
    setCookie('whm_csrf=csrf-of-b; path=/')
    const seen: Array<string | undefined> = []
    const { api } = await loadTab(async (config) => {
      seen.push(config.headers.get('X-Organization-ID') as string | undefined)
      return ok(config, { data: { id: 'user-b' } })
    })

    await api.get('/me')

    expect(seen).toEqual([undefined])
    expect(localStorage.getItem('selected_organization_id')).toBeNull()
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

  it('a replayed callback page sends nothing with the ended session\'s cookies', async () => {
    localStorage.setItem('whm_session_id', 'ended:session-a')
    localStorage.setItem('whm_logout_pending', JSON.stringify({ marker: 'ended:session-a', csrf: 'csrf-token-1' }))
    recordPosts([])
    stubLocation('/auth/sso/callback')
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config, { data: { id: 'user-a' } }))
    const { api, whenLogoutSettled } = await loadTab(adapter)

    await expect(api.get('/me')).rejects.toMatchObject({ code: 'ERR_CANCELED' })
    await whenLogoutSettled()
    expect(adapter).not.toHaveBeenCalled()
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

  it("an SSO start in another tab aborts this tab's logout requests in flight", async () => {
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

    const fence = JSON.stringify({ at: Date.now(), marker: localStorage.getItem('whm_session_id') })
    localStorage.setItem('whm_sso_started', fence)
    window.dispatchEvent(new StorageEvent('storage', { key: 'whm_sso_started', newValue: fence }))

    expect(signals[0].aborted).toBe(true)
    await whenLogoutSettled()
    // No /auth/logout: it would clear the cookies the SSO callback is about to
    // set. The logout stays pending for the next sign-in or page load.
    expect(order).toEqual(['revoke csrf-token-1'])
    expect(JSON.parse(localStorage.getItem('whm_logout_pending')!)).toMatchObject({ marker: 'ended:' })
  })

  it("another tab's SSO start leaves an open tab's session alone", async () => {
    vi.useFakeTimers()
    installWebLocks()
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

    await starting.beginSsoSignIn()
    window.dispatchEvent(
      new StorageEvent('storage', { key: 'whm_sso_started', newValue: localStorage.getItem('whm_sso_started') }),
    )

    expect(localStorage.getItem('whm_session_id')).toBe('session-1')
    // While the SSO is under way the open tab keeps its session but refreshes
    // nothing (here once the SSO tab's lock is let go, which a real page does
    // when it leaves): its 401 is surfaced, nothing expires.
    const during = open.api.get('/contacts').catch((e: AxiosError) => e.response?.status)
    await vi.advanceTimersByTimeAsync(61_000)
    expect(await during).toBe(401)
    expect(rotated).toBe(false)
    expect(localStorage.getItem('user')).not.toBeNull()
    // An abandoned SSO lapses: the open tab refreshes and works again.
    await vi.advanceTimersByTimeAsync(5 * 60_000)
    expect((await open.api.get('/contacts')).status).toBe(200)
  })

  it('an SSO callback that completes while a logout is queued is never revoked', async () => {
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const ending = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order)
    await ending.api.post('/auth/logout', {})

    // In another tab the SSO redirect sets B's cookies and loads the callback.
    setCookie('whm_csrf=csrf-of-b; path=/')
    stubLocation('/auth/sso/callback')
    await loadTab(async (config) => ok(config, { data: { id: 'user-b' } }))
    releaseHolder()
    await ending.whenLogoutSettled()

    // Nothing was sent with B's cookies.
    expect(order).toEqual([])
    expect(localStorage.getItem('whm_session_id')).toMatch(/^(?!ended:)./)
  })

  it('starting SSO waits for this tab\'s logout, then fences other tabs without starting a session', async () => {
    installWebLocks()
    const releaseHolder = holdLockElsewhere()
    const fetchSpy = vi.fn(async () => new Response('{}'))
    vi.stubGlobal('fetch', fetchSpy)
    const { api, beginSsoSignIn } = await loadTab(async (config) => ok(config))
    const order: string[] = []
    recordPosts(order)

    await api.post('/auth/logout', {})
    let started = false
    const sso = beginSsoSignIn().then(() => {
      started = true
    })
    await tick()
    expect(started).toBe(false)
    releaseHolder()
    await sso

    expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1'])
    // The new session begins only at the callback, with the new cookies.
    expect(localStorage.getItem('whm_session_id')).toBe('ended:')
    expect(JSON.parse(localStorage.getItem('whm_sso_started')!)).toMatchObject({ marker: 'ended:' })
    // Leaving for the provider sends no unordered keepalive logout.
    window.dispatchEvent(new Event('pagehide'))
    expect(fetchSpy).not.toHaveBeenCalled()
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
      expect(order).toEqual(['revoke csrf-token-1', 'logout csrf-token-1'])
      expect(assignedHref).toBeNull()
    })

    it(`a 401 within one session still refreshes and retries (${locking})`, async () => {
      if (locking !== 'Web Locks') removeWebLocks()
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
  }

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

  it('a stale refresh failure in another tab never expires a newer session (no Web Locks)', async () => {
    removeWebLocks()
    localStorage.setItem('whm_session_id', 'session-a')
    let failRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      failRefresh = resolve
    })
    const stale = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const order: string[] = []
    recordPosts(order, async (label) => {
      if (label === 'refresh') {
        await refreshGate
        throw httpError(401)
      }
    })
    const request = stale.api.get('/contacts').then(() => 'resolved', () => 'rejected')
    await vi.waitFor(() => expect(order).toEqual(['refresh']))

    // Meanwhile, in another tab: logout, then B signs in with SSO (a new
    // session, no change of auth epoch).
    const active = await loadTab(async (config) => ok(config))
    await active.api.post('/auth/logout', {})
    await active.whenLogoutSettled()
    localStorage.setItem('whm_session_id', 'session-b')
    localStorage.setItem('user', JSON.stringify({ id: 'user-b', email: 'b@example.com' }))
    const sessionB = 'session-b'
    failRefresh()

    expect(await request).toBe('rejected')
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
    expect(localStorage.getItem('whm_sso_started')).toBeNull()
    expect(document.cookie).toContain('whm_csrf=csrf-token-1')
  })

  it("an SSO start in another tab aborts this tab's refresh in flight, and no retry goes out", async () => {
    vi.useFakeTimers()
    localStorage.setItem('whm_session_id', 'session-1')
    const { api } = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const signals: AbortSignal[] = []
    const order: string[] = []
    recordPosts(order, (label, options) => {
      if (label !== 'refresh') return undefined
      signals.push(options.signal)
      return new Promise((_, reject) => {
        options.signal.addEventListener('abort', () => reject(new AxiosError('canceled', AxiosError.ERR_CANCELED)))
      })
    })

    const request = api.get('/contacts').catch((e: AxiosError) => e.response?.status)
    await vi.waitFor(() => expect(signals).toHaveLength(1))
    const fence = JSON.stringify({ at: Date.now(), marker: 'session-1' })
    localStorage.setItem('whm_sso_started', fence)
    window.dispatchEvent(new StorageEvent('storage', { key: 'whm_sso_started', newValue: fence }))
    expect(signals[0].aborted).toBe(true)
    await vi.advanceTimersByTimeAsync(30_000)

    expect(await request).toBe(401)
    expect(order).toEqual(['refresh'])
    expect(localStorage.getItem('user')).not.toBeNull()
    expect(assignedHref).toBeNull()
  })

  it("an SSO start from another session's state does not hold this session's refreshes", async () => {
    localStorage.setItem('whm_session_id', 'session-1')
    // Started before this session began (the marker has moved on since).
    localStorage.setItem('whm_sso_started', JSON.stringify({ at: Date.now(), marker: 'ended:session-0' }))
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
  })

  it('a password sign-in on a replayed callback page ends the replay latch', async () => {
    localStorage.setItem('whm_session_id', 'ended:session-a')
    localStorage.setItem('whm_logout_pending', JSON.stringify({ marker: 'ended:session-a', csrf: 'csrf-token-1' }))
    recordPosts([])
    stubLocation('/auth/sso/callback')
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config, { data: { expires_in: 900, user: {} } }))
    const { api, whenLogoutSettled } = await loadTab(adapter)
    await expect(api.get('/me')).rejects.toMatchObject({ code: 'ERR_CANCELED' })
    await whenLogoutSettled()

    await api.post('/auth/login', { email: 'b@example.com', password: 'x' })
    await api.get('/contacts')

    expect(adapter.mock.calls.map(([config]) => config.url)).toEqual(['/auth/login', '/contacts'])
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

  it('a held sign-in lock is released by the backstop if no response ever comes', async () => {
    vi.useFakeTimers()
    installWebLocks()
    const signIn = await loadTab(async (config) => {
      if (config.url === '/auth/login') return new Promise(() => undefined)
      return ok(config)
    })
    const other = await loadTab(async (config) => {
      throw httpError(401, config)
    })
    const order: string[] = []
    recordPosts(order, (label) => (label === 'refresh' ? refreshOk() : undefined))

    void signIn.api.post('/auth/login', { email: 'a@example.com', password: 'x' })
    await vi.advanceTimersByTimeAsync(10)
    void other.api.get('/contacts').catch(() => undefined)
    await vi.advanceTimersByTimeAsync(55_000)
    expect(order).toEqual([])

    await vi.advanceTimersByTimeAsync(6_000)
    expect(order).toEqual(['refresh'])
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
