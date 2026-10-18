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

// A FIFO Web Locks stand-in shared by every "tab" in a test.
function installWebLocks() {
  const tails = new Map<string, Promise<void>>()
  const request = vi.fn(async (name: string, ...rest: unknown[]) => {
    const callback = rest[rest.length - 1] as () => Promise<unknown>
    const previous = tails.get(name) ?? Promise.resolve()
    let release!: () => void
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    tails.set(name, previous.then(() => held))
    await previous
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
    removeWebLocks()
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

  function recordPosts(order: string[], onPost?: (label: string) => Promise<unknown> | unknown) {
    postSpy.mockImplementation(async (url: string, body: unknown, options: any) => {
      const label = postLabel(url, body, options)
      order.push(label)
      const custom = onPost ? await onPost(label) : undefined
      return custom ?? { data: { data: {} }, status: 200 }
    })
  }

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

  for (const locking of ['Web Locks', 'no Web Locks (fallback)']) {
    it(`a logout waits for a refresh in flight, which then leaves no trace (${locking})`, async () => {
      if (locking === 'Web Locks') installWebLocks()
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

  for (const locking of ['Web Locks', 'no Web Locks (fallback)']) {
    it(`a new sign-in waits for an earlier logout to finish (${locking})`, async () => {
      if (locking === 'Web Locks') installWebLocks()
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
  }

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
