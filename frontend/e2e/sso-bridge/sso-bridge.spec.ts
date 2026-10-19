import {
  expect,
  request as playwrightRequest,
  test,
  type APIRequestContext,
  type APIResponse,
  type Cookie,
  type Page,
  type Route,
} from '@playwright/test'
import { writeFileSync } from 'node:fs'

// The real-app, two-callback SSO bridge (R1).
//
// Run only by the Go test TestSSOBridgeRealBrowserTwoCallbacks (cmd/whatomate,
// playwright.sso-bridge.config.ts). That test serves the real application
// (production routes and middleware, the embedded candidate frontend) over
// loopback TLS, with two organizations whose custom SSO provider is a local
// fake, and owns this browser process.
//
// Two pages of one browser context sign in, A then B, through the real
// InitSSO, CallbackSSO and session exchange. The only fake is the identity
// provider, served by the Go test: the browser follows the real InitSSO
// redirect to its made-up host (mapped by --host-resolver-rules), and its
// authorization page script-navigates back to the exact redirect URI with the
// real state and this identity's code; the token and userinfo calls go from
// the real CallbackSSO to the same fake. (A script navigation, not a 302,
// because Playwright does not route a request a redirect leads to, and the
// callback has to be held.) Callback and exchange answers are fetched from the
// real server by an isolated request client (its own jar, never the
// browser's), held, and handed to the browser unchanged (status, body and
// every Set-Cookie) with route.fulfill: nothing installs or replays a cookie.
//
// Schedule:
// 1. Both callbacks are answered by the server and neither is delivered: each
//    sets only its own binding cookie, no session cookie.
// 2. Callback A is delivered; its exchange is answered by the server (A's
//    session issued) and held from the browser, which keeps the session lock.
// 3. Callback B is delivered while A's exchange holds the lock: B's binding is
//    in the jar and B's exchange waits for the lock, unsent.
// 4. A's exchange is delivered: it expires A's binding only. B's exchange then
//    goes out with B's binding and is answered and held.
// 5. B's exchange is delivered. All three auth cookies are B's, exactly, and
//    the real /api/me (same context) is B's identity and organization.
// Exactly two callbacks and two exchanges reach the server, and no password
// sign-in.

type Who = 'A' | 'B'

function required(name: string): string {
  const value = process.env[name]
  if (!value) throw new Error(`${name} is required: run the SSO bridge through its Go test`)
  return value
}

const BASE = required('BASE_URL').replace(/\/$/, '')
const SIGN_IN: Record<Who, { slug: string; org: string; email: string; idp: string; client: string; code: string }> = {
  A: {
    slug: required('SSO_BRIDGE_SLUG_A'),
    org: required('SSO_BRIDGE_ORG_A'),
    email: required('SSO_BRIDGE_EMAIL_A'),
    idp: required('SSO_BRIDGE_IDP_A'),
    client: required('SSO_BRIDGE_CLIENT_A'),
    code: required('SSO_BRIDGE_CODE_A'),
  },
  B: {
    slug: required('SSO_BRIDGE_SLUG_B'),
    org: required('SSO_BRIDGE_ORG_B'),
    email: required('SSO_BRIDGE_EMAIL_B'),
    idp: required('SSO_BRIDGE_IDP_B'),
    client: required('SSO_BRIDGE_CLIENT_B'),
    code: required('SSO_BRIDGE_CODE_B'),
  },
}
const CALLBACK_PATH = '/api/auth/sso/custom/callback'
const REFRESH_PATH = '/api/auth/refresh'
const BIND_PREFIX = 'whm_sso_bind_'
const AUTH_COOKIES = ['whm_access', 'whm_refresh', 'whm_csrf'] as const
const AUTH_PATHS: Record<(typeof AUTH_COOKIES)[number], string> = {
  whm_access: '/api',
  whm_refresh: '/api/auth/refresh',
  whm_csrf: '/',
}
const LOCK_NAME = 'whm-token-refresh'
// Every step stays well inside the exchange request's own 15 s timeout and
// the server's 60 s code lifetime.
const STEP_MS = 10_000

type Latch<T> = { promise: Promise<T>; resolve: (value: T) => void }

function latch<T = void>(): Latch<T> {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => {
    resolve = done
  })
  return { promise, resolve }
}

// One request the browser sends, answered upstream and held from the browser.
type Held = {
  sent: boolean
  requestCookies: string
  captured: Latch<APIResponse>
  release: Latch<void>
  delivered: Latch<void>
}

function held(): Held {
  return { sent: false, requestCookies: '', captured: latch(), release: latch(), delivered: latch() }
}

// What the browser saw of the sign-in (auth and provider responses, failed
// requests, page errors), for the message of a step that times out.
const trail: string[] = []

async function within<T>(promise: Promise<T>, what: string, ms = STEP_MS): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_, reject) => {
        timer = setTimeout(
          () => reject(new Error(`timed out waiting for ${what}; the browser saw:\n${trail.join('\n')}`)),
          ms,
        )
      }),
    ])
  } finally {
    clearTimeout(timer)
  }
}

// Fetches the browser's request from the real server with an isolated request
// client: the browser's own headers (its Cookie header included), no
// redirects followed, and a jar of its own, so no Set-Cookie can reach the
// browser before route.fulfill hands it over.
async function forward(route: Route): Promise<{ upstream: APIRequestContext; response: APIResponse; cookies: string }> {
  const request = route.request()
  const upstream = await playwrightRequest.newContext({ ignoreHTTPSErrors: true })
  const headers = await request.allHeaders()
  delete headers['content-length']
  delete headers.host
  const response = await upstream.fetch(request.url(), {
    method: request.method(),
    headers,
    data: request.postDataBuffer() ?? undefined,
    maxRedirects: 0,
  })
  return { upstream, response, cookies: headers.cookie ?? '' }
}

async function holdThenDeliver(route: Route, slot: Held) {
  const { upstream, response, cookies } = await forward(route)
  slot.requestCookies = cookies
  slot.captured.resolve(response)
  await slot.release.promise
  // The browser may have gone away meanwhile: then nothing is delivered.
  await route.fulfill({ response }).catch(() => undefined)
  slot.delivered.resolve()
  await upstream.dispose()
}

type SetCookie = { name: string; value: string; attributes: string; expired: boolean }

function setCookies(response: APIResponse): SetCookie[] {
  return response
    .headersArray()
    .filter((header) => header.name.toLowerCase() === 'set-cookie')
    .map((header) => {
      const [pair, ...rest] = header.value.split(';')
      const at = pair.indexOf('=')
      const value = pair.slice(at + 1).trim()
      const attributes = rest.map((part) => part.trim()).join('; ')
      const maxAge = /(?:^|;\s*)max-age=(-?\d+)/i.exec(attributes)
      const expires = /(?:^|;\s*)expires=([^;]+)/i.exec(attributes)
      const expired =
        value === '' ||
        (maxAge !== null && Number(maxAge[1]) <= 0) ||
        (expires !== null && Date.parse(expires[1]) <= Date.now())
      return { name: pair.slice(0, at).trim(), value, attributes, expired }
    })
}

// The three auth cookie values an answer sets (all three, none expired).
function authValues(set: SetCookie[]): Record<string, string> {
  const values: Record<string, string> = {}
  for (const entry of set) {
    if ((AUTH_COOKIES as readonly string[]).includes(entry.name) && !entry.expired) values[entry.name] = entry.value
  }
  expect(Object.keys(values).sort()).toEqual([...AUTH_COOKIES].sort())
  return values
}

const bindingCookies = (cookies: Cookie[]) => cookies.filter((c) => c.name.startsWith(BIND_PREFIX))
const cookieNamed = (cookies: Cookie[], name: string) => cookies.find((c) => c.name === name)

// The session lock as the origin's lock manager sees it, from `page`.
async function sessionLock(page: Page): Promise<{ held: number; pending: number } | null> {
  try {
    return await page.evaluate(async (name) => {
      const state = await navigator.locks.query()
      return {
        held: (state.held ?? []).filter((lock) => lock.name === name).length,
        pending: (state.pending ?? []).filter((lock) => lock.name === name).length,
      }
    }, LOCK_NAME)
  } catch {
    return null
  }
}

test('two SSO callbacks in one browser: each exchange spends only its own binding and the later sign-in wins', async ({
  browser,
}) => {
  const context = await browser.newContext({ baseURL: BASE, ignoreHTTPSErrors: true })
  const callbacks: Record<Who, Held> = { A: held(), B: held() }
  const exchanges: Record<Who, Held> = { A: held(), B: held() }
  const exchangeCodes: Partial<Record<Who, string>> = {}
  const exchangeOrder: Who[] = []
  const hops: Record<Who, URL[]> = { A: [], B: [] }
  const unexpected: string[] = []
  let otherRefreshes = 0
  let logins = 0
  try {
    context.on('request', (request) => {
      const url = new URL(request.url())
      if (url.origin === BASE && url.pathname === '/api/auth/login') logins += 1
    })
    context.on('response', (response) => {
      const url = new URL(response.url())
      if (url.pathname.startsWith('/api/auth/') || url.origin === SIGN_IN.A.idp || url.origin === SIGN_IN.B.idp) {
        trail.push(`${response.status()} ${url.origin}${url.pathname} -> ${response.headers().location ?? ''}`)
      }
    })
    context.on('requestfailed', (request) => trail.push(`failed ${request.url()}: ${request.failure()?.errorText}`))
    context.on('page', (page) => page.on('pageerror', (error) => trail.push(`page error: ${error.message}`)))

    // The browser's visits to the fake identity provider (reached by
    // following the real InitSSO redirect; not routed).
    context.on('request', (request) => {
      const url = new URL(request.url())
      for (const who of ['A', 'B'] as const) {
        if (url.origin === SIGN_IN[who].idp && url.pathname === '/auth') hops[who].push(url)
      }
    })

    // The real callbacks, each held until it is delivered.
    await context.route(
      (url) => url.origin === BASE && url.pathname === CALLBACK_PATH,
      async (route) => {
        const code = new URL(route.request().url()).searchParams.get('code')
        const who: Who | null = code === SIGN_IN.A.code ? 'A' : code === SIGN_IN.B.code ? 'B' : null
        if (!who || callbacks[who].sent) {
          unexpected.push(`callback ${route.request().url()}`)
          await route.abort()
          return
        }
        callbacks[who].sent = true
        await holdThenDeliver(route, callbacks[who])
      },
    )

    // The real exchanges: POST /api/auth/refresh {"sso_exchange": code}.
    await context.route(
      (url) => url.origin === BASE && url.pathname === REFRESH_PATH,
      async (route) => {
        const body = route.request().postData() ?? ''
        if (!body.includes('"sso_exchange"')) {
          otherRefreshes += 1
          await route.continue()
          return
        }
        const code = (JSON.parse(body) as { sso_exchange?: string }).sso_exchange
        const who: Who | null = code === exchangeCodes.A ? 'A' : code === exchangeCodes.B ? 'B' : null
        if (!who || exchanges[who].sent) {
          unexpected.push(`exchange ${String(code)}`)
          await route.abort()
          return
        }
        exchanges[who].sent = true
        exchangeOrder.push(who)
        await holdThenDeliver(route, exchanges[who])
      },
    )

    const pageA = await context.newPage()
    const pageB = await context.newPage()
    // Web Locks (a secure context) are what order the two exchanges.
    await pageA.goto('/login')
    expect(await pageA.evaluate(() => typeof navigator.locks?.request === 'function')).toBe(true)

    // 1. Both sign-ins through the real InitSSO; both callbacks answered by
    //    the server, neither delivered.
    void pageA.goto(`/api/auth/sso/custom/init?organization=${SIGN_IN.A.slug}`).catch(() => undefined)
    const callbackA = await within(callbacks.A.captured.promise, 'callback A answered')
    void pageB.goto(`/api/auth/sso/custom/init?organization=${SIGN_IN.B.slug}`).catch(() => undefined)
    const callbackB = await within(callbacks.B.captured.promise, 'callback B answered')

    for (const who of ['A', 'B'] as const) {
      expect(hops[who]).toHaveLength(1)
      const hop = hops[who][0]
      expect(hop.searchParams.get('redirect_uri')).toBe(`${BASE}${CALLBACK_PATH}`)
      expect(hop.searchParams.get('client_id')).toBe(SIGN_IN[who].client)
      expect(hop.searchParams.get('response_type')).toBe('code')
      expect(hop.searchParams.get('state')).toBeTruthy()
    }
    expect(hops.A[0].searchParams.get('state')).not.toBe(hops.B[0].searchParams.get('state'))

    const binding: Partial<Record<Who, SetCookie>> = {}
    for (const [who, response] of [
      ['A', callbackA],
      ['B', callbackB],
    ] as const) {
      expect(response.status(), `callback ${who}`).toBe(307)
      const location = new URL(response.headers().location ?? '', BASE)
      expect(location.pathname).toBe('/auth/sso/callback')
      const code = location.searchParams.get('code')
      expect(code).toBeTruthy()
      exchangeCodes[who] = code!
      // Its own binding cookie, and no session cookie.
      const set = setCookies(response)
      expect(set, `callback ${who} cookies`).toHaveLength(1)
      expect(set[0].name.startsWith(BIND_PREFIX)).toBe(true)
      expect(set[0].expired).toBe(false)
      expect(set[0].attributes).toMatch(/path=\/api\/auth\/refresh/i)
      expect(set[0].attributes).toMatch(/httponly/i)
      binding[who] = set[0]
    }
    const bindA = binding.A!
    const bindB = binding.B!
    expect(bindA.name).not.toBe(bindB.name)
    expect(exchangeCodes.A).not.toBe(exchangeCodes.B)
    expect(bindingCookies(await context.cookies())).toEqual([])

    // 2. Callback A delivered; A's exchange answered upstream (A's session
    //    issued) and held, with the session lock held in page A.
    callbacks.A.release.resolve()
    await within(callbacks.A.delivered.promise, 'callback A delivered')
    const exchangeA = await within(exchanges.A.captured.promise, 'exchange A sent and answered')
    expect(exchanges.A.requestCookies).toContain(`${bindA.name}=${bindA.value}`)
    expect(exchanges.A.requestCookies).not.toContain(bindB.name)
    expect(exchangeA.status()).toBe(200)
    expect((await exchangeA.json()).data.user.email).toBe(SIGN_IN.A.email)
    const setA = setCookies(exchangeA)
    const authA = authValues(setA)
    // A's answer expires A's binding, and no other.
    expect(setA.filter((c) => c.name.startsWith(BIND_PREFIX)).map((c) => [c.name, c.expired])).toEqual([
      [bindA.name, true],
    ])
    let cookies = await context.cookies()
    expect(bindingCookies(cookies).map((c) => c.name)).toEqual([bindA.name])
    for (const name of AUTH_COOKIES) expect(cookieNamed(cookies, name), name).toBeUndefined()

    // 3. Callback B delivered while A's exchange holds the lock: B's binding
    //    is in the jar, and B's exchange waits for the lock, unsent.
    callbacks.B.release.resolve()
    await within(callbacks.B.delivered.promise, 'callback B delivered')
    await expect
      .poll(() => sessionLock(pageB), { timeout: STEP_MS, message: "B's exchange waits for the session lock" })
      .toEqual({ held: 1, pending: 1 })
    await pageB.waitForTimeout(1_000)
    expect(exchanges.B.sent, "B's exchange must not be sent while A's holds the lock").toBe(false)
    cookies = await context.cookies()
    expect(bindingCookies(cookies).map((c) => c.name).sort()).toEqual([bindA.name, bindB.name].sort())
    expect(cookieNamed(cookies, bindB.name)?.value).toBe(bindB.value)

    // 4. A's exchange delivered: only A's binding expires. B's exchange then
    //    goes out with B's binding (and no other), answered and held.
    exchanges.A.release.resolve()
    await within(exchanges.A.delivered.promise, 'exchange A delivered')
    const exchangeB = await within(exchanges.B.captured.promise, 'exchange B sent after A released the lock')
    expect(exchanges.B.requestCookies).toContain(`${bindB.name}=${bindB.value}`)
    expect(exchanges.B.requestCookies).not.toContain(bindA.name)
    cookies = await context.cookies()
    expect(bindingCookies(cookies).map((c) => c.name)).toEqual([bindB.name])
    for (const name of AUTH_COOKIES) expect(cookieNamed(cookies, name)?.value, name).toBe(authA[name])
    // Page A is done; it takes no further part.
    await pageA.close()

    // 5. B's exchange delivered.
    expect(exchangeB.status()).toBe(200)
    expect((await exchangeB.json()).data.user.email).toBe(SIGN_IN.B.email)
    const setB = setCookies(exchangeB)
    const authB = authValues(setB)
    expect(setB.filter((c) => c.name.startsWith(BIND_PREFIX)).map((c) => [c.name, c.expired])).toEqual([
      [bindB.name, true],
    ])
    for (const name of AUTH_COOKIES) expect(authB[name], name).not.toBe(authA[name])
    exchanges.B.release.resolve()
    await within(exchanges.B.delivered.promise, 'exchange B delivered')

    // All three auth cookies are B's answer's, exactly; no binding is left.
    await expect.poll(async () => cookieNamed(await context.cookies(), 'whm_access')?.value).toBe(authB.whm_access)
    cookies = await context.cookies()
    for (const name of AUTH_COOKIES) {
      const stored = cookieNamed(cookies, name)
      expect(stored?.value, name).toBe(authB[name])
      expect(stored?.path, name).toBe(AUTH_PATHS[name])
      expect(stored?.secure, name).toBe(true)
      expect(stored?.httpOnly, name).toBe(name !== 'whm_csrf')
    }
    expect(cookies.filter((c) => (AUTH_COOKIES as readonly string[]).includes(c.name))).toHaveLength(3)
    expect(bindingCookies(cookies)).toEqual([])

    // The real /api/me through the same browser context: B, in B's org.
    const me = await context.request.get('/api/me')
    expect(me.status()).toBe(200)
    const meData = (await me.json()).data as { email: string; organization_id: string }
    expect(meData.email).toBe(SIGN_IN.B.email)
    expect(meData.organization_id).toBe(SIGN_IN.B.org)

    // Two real callbacks, two real exchanges (A then B), no password sign-in,
    // no other refresh.
    expect(unexpected).toEqual([])
    expect([callbacks.A.sent, callbacks.B.sent]).toEqual([true, true])
    expect(exchangeOrder).toEqual(['A', 'B'])
    expect(logins).toBe(0)
    expect(otherRefreshes).toBe(0)

    writeFileSync(
      required('SSO_BRIDGE_SUMMARY'),
      JSON.stringify({
        callbacks: Number(callbacks.A.sent) + Number(callbacks.B.sent),
        exchanges: exchangeOrder,
        logins,
        otherRefreshes,
        attemptA: bindA.name,
        attemptB: bindB.name,
        meEmail: meData.email,
        meOrganization: meData.organization_id,
      }),
    )
  } finally {
    for (const slot of [callbacks.A, callbacks.B, exchanges.A, exchanges.B]) slot.release.resolve()
    await context.close()
  }
})
