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
  const request = vi.fn(async (name: string, callback: () => Promise<unknown>) => {
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

  it('revokes the refresh token before logging out', async () => {
    const order: string[] = []
    const { api } = await loadTab(async (config) => {
      order.push(`adapter ${config.url}`)
      return ok(config)
    })
    postSpy.mockImplementation(async (_url: string, body: unknown, options: any) => {
      order.push(`revoke ${JSON.stringify(body)} ${options.headers['X-CSRF-Token']}`)
      return { data: { data: { status: 'revoked' } }, status: 200 }
    })

    await api.post('/auth/logout', {})

    expect(order).toEqual(['revoke {"revoke":true} csrf-token-1', 'adapter /auth/logout'])
    const [url, , options] = postSpy.mock.calls[0] as [string, unknown, { withCredentials: boolean }]
    expect(url).toMatch(/\/api\/auth\/refresh$/)
    expect(options.withCredentials).toBe(true)
  })

  it('stamps the logout with the CSRF cookie as it is after the revoke', async () => {
    const seen: Array<string | undefined> = []
    const { api } = await loadTab(async (config) => {
      seen.push(config.headers.get('X-CSRF-Token') as string | undefined)
      return ok(config)
    })

    // The revoke rotates the cookie (another tab refreshed meanwhile)...
    postSpy.mockImplementationOnce(async () => {
      setCookie('whm_csrf=csrf-token-2; path=/')
      return { data: { data: {} }, status: 200 }
    })
    await api.post('/auth/logout', {})
    // ...or clears it, as a successful revoke does.
    postSpy.mockImplementationOnce(async () => {
      setCookie('whm_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/')
      return { data: { data: {} }, status: 200 }
    })
    setCookie('whm_csrf=csrf-token-3; path=/')
    await api.post('/auth/logout', {})

    expect(seen).toEqual(['csrf-token-2', undefined])
  })

  it('retries a revoke rejected for a CSRF cookie rotated mid-flight', async () => {
    const { api } = await loadTab(async (config) => ok(config))
    const sent: string[] = []
    postSpy.mockImplementation(async (_url: string, _body: unknown, options: any) => {
      sent.push(options.headers['X-CSRF-Token'])
      if (sent.length === 1) {
        setCookie('whm_csrf=csrf-token-2; path=/')
        throw httpError(403)
      }
      return { data: { data: {} }, status: 200 }
    })

    await api.post('/auth/logout', {})

    expect(sent).toEqual(['csrf-token-1', 'csrf-token-2'])
  })

  it('stops revoking when someone else signs in during the backoff', async () => {
    vi.useFakeTimers()
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config))
    const { api } = await loadTab(adapter)
    postSpy.mockImplementation(async () => {
      // While this logout waits out a 429, another tab signs a different
      // user in (new cookies, new stored identity).
      localStorage.setItem('user', JSON.stringify({ id: 'user-2', email: 'b@example.com' }))
      setCookie('whm_csrf=csrf-of-user-2; path=/')
      throw httpError(429)
    })

    const logout = api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(3_000)
    await logout

    expect(postSpy).toHaveBeenCalledTimes(1)
    expect(adapter).toHaveBeenCalledTimes(1)
  })

  it('re-reads the CSRF cookie for each revoke attempt', async () => {
    vi.useFakeTimers()
    const { api } = await loadTab(async (config) => ok(config))
    const sent: string[] = []
    postSpy.mockImplementation(async (_url: string, _body: unknown, options: any) => {
      sent.push(options.headers['X-CSRF-Token'])
      if (sent.length === 1) {
        // Another tab refreshes meanwhile and rotates the CSRF cookie.
        setCookie('whm_csrf=csrf-token-2; path=/')
        throw httpError(429)
      }
      return { data: { data: {} }, status: 200 }
    })

    const logout = api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(1_000)
    await logout

    expect(sent).toEqual(['csrf-token-1', 'csrf-token-2'])
  })

  it('retries a rate-limited revoke before logging out', async () => {
    vi.useFakeTimers()
    const order: string[] = []
    const { api } = await loadTab(async (config) => {
      order.push('logout')
      return ok(config)
    })
    postSpy
      .mockImplementationOnce(async () => {
        order.push('revoke 429')
        throw httpError(429)
      })
      .mockImplementationOnce(async () => {
        order.push('revoke 429')
        throw httpError(429)
      })
      .mockImplementationOnce(async () => {
        order.push('revoke ok')
        return { data: { data: {} }, status: 200 }
      })

    const logout = api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(3_000)
    await logout

    expect(order).toEqual(['revoke 429', 'revoke 429', 'revoke ok', 'logout'])
  })

  it('logs out after the revoke stays rate-limited', async () => {
    vi.useFakeTimers()
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config))
    const { api } = await loadTab(adapter)
    postSpy.mockRejectedValue(httpError(429))

    const logout = api.post('/auth/logout', {})
    await vi.advanceTimersByTimeAsync(3_000)

    expect((await logout).status).toBe(200)
    expect(postSpy).toHaveBeenCalledTimes(3)
    expect(adapter).toHaveBeenCalledTimes(1)
  })

  it('still logs out when the revoke fails', async () => {
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ok(config))
    const { api } = await loadTab(adapter)
    postSpy.mockRejectedValue(networkError())

    const response = await api.post('/auth/logout', {})

    expect(response.status).toBe(200)
    expect(adapter).toHaveBeenCalledTimes(1)
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
    postSpy.mockImplementation(async (_url: string, body: unknown) => {
      // Logout's revoke call; it rotates nothing.
      if ((body as { revoke?: boolean })?.revoke) return { data: { data: {} }, status: 200 }
      rotated = true
      return refreshOk()
    })
    for (const url of ['/auth/login', '/auth/register', '/auth/logout', '/auth/sso/google/init']) {
      await expect(api.post(url, {})).rejects.toMatchObject({ response: { status: 401 } })
    }
    // No refresh was attempted; the only /auth/refresh call is logout's revoke.
    expect(postSpy.mock.calls.map((call: unknown[]) => call[1])).toEqual([{ revoke: true }])
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
