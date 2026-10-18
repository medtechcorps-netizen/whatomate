import { test, expect, type APIRequestContext, type BrowserContext, type Page } from '@playwright/test'
import { loginAsAdmin, logout, TEST_USERS, type TestUser } from '../../helpers'

// A logout must not race refresh work already in flight. A refresh response
// sets new auth cookies; if it lands after /auth/logout cleared them, the
// browser is signed back in. The client exits at once but sends the revoke
// and /auth/logout only after that response landed (shared Web Lock), and a
// later sign-in (password or SSO) waits for the logout in turn.
//
// The refresh response is held back for real: it is fetched upstream through
// a separate request context (its own cookie jar), so the browser's jar only
// changes when route.fulfill delivers it. Each test first proves that.
//
// Negative controls: against 3d086ef5 (before R1-AUTH-1) the ordering
// assertions here fail (the revoke and logout go out before the refresh
// lands, and the late Set-Cookie signs the browser back in). Against 04344c17
// the 7 s case fails: the revoke was only raced, not aborted, so its late
// answer cleared the next session's cookies.

type Held = {
  started: Promise<void>
  release: () => void
  successorRefresh: () => string | undefined
}

// holdRefreshResponse holds the next refresh response of `page` until
// release(). Revoke calls (POST /auth/refresh {revoke:true}) pass through.
async function holdRefreshResponse(
  page: Page,
  newContext: () => Promise<APIRequestContext>,
  order: string[],
): Promise<Held> {
  let successor: string | undefined
  let release!: () => void
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  let seen!: () => void
  const started = new Promise<void>((resolve) => {
    seen = resolve
  })
  let held = false
  await page.route('**/api/auth/refresh', async (route) => {
    const request = route.request()
    if ((request.postData() ?? '').includes('"revoke"') || held) {
      await route.continue()
      return
    }
    held = true
    seen()
    const upstream = await newContext()
    const headers = await request.allHeaders()
    delete headers['content-length']
    delete headers.host
    const response = await upstream.fetch(request.url(), {
      method: request.method(),
      headers,
      data: request.postDataBuffer() ?? undefined,
      maxRedirects: 0,
    })
    successor = response
      .headersArray()
      .filter((header) => header.name.toLowerCase() === 'set-cookie')
      .map((header) => /^whm_refresh=([^;]+)/.exec(header.value)?.[1])
      .find(Boolean)
    await gate
    order.push('refresh landed')
    // The browser may have aborted it meanwhile: then nothing is delivered.
    await route.fulfill({ response }).catch(() => undefined)
    await upstream.dispose()
  })
  return { started, release: () => release(), successorRefresh: () => successor }
}

// Records the revoke and logout requests the browser sends, with the cookies
// and CSRF header they carry.
function recordSessionRequests(context: BrowserContext, order: string[]) {
  const sent: Array<{ kind: string; cookie: string; csrf: string }> = []
  context.on('request', async (request) => {
    const url = request.url()
    const isRevoke = url.endsWith('/api/auth/refresh') && (request.postData() ?? '').includes('"revoke"')
    const isLogout = url.endsWith('/api/auth/logout')
    if (!isRevoke && !isLogout) return
    const kind = isRevoke ? 'revoke' : 'logout'
    order.push(kind)
    const headers = await request.allHeaders()
    sent.push({ kind, cookie: headers.cookie ?? '', csrf: headers['x-csrf-token'] ?? '' })
  })
  return sent
}

// The auth cookie values a response sets.
function setCookies(headers: Array<{ name: string; value: string }>) {
  const values: Record<string, string> = {}
  for (const header of headers) {
    if (header.name.toLowerCase() !== 'set-cookie') continue
    const match = /^(whm_[a-z]+)=([^;]*)/.exec(header.value)
    if (match) values[match[1]] = match[2]
  }
  return values
}

async function cookieValue(context: BrowserContext, name: string) {
  return (await context.cookies()).find((c) => c.name === name && c.value !== '')?.value
}

// Make the proactive refresh due and wake it.
async function startProactiveRefresh(page: Page) {
  await page.evaluate(() => {
    localStorage.setItem('whm_auth_expires_at', String(Date.now()))
    document.dispatchEvent(new Event('visibilitychange'))
  })
}

// Signs in on a login page that is already open (no reload).
async function signInHere(page: Page, user: TestUser) {
  await page.locator('input[name="email"], input[type="email"]').fill(user.email)
  await page.locator('input[name="password"], input[type="password"]').fill(user.password)
  await page.getByTestId('password-sign-in').click()
  await page.waitForURL((url) => !url.pathname.includes('/login'), { timeout: 45_000 })
}

async function expectRefreshRejected(newContext: () => Promise<APIRequestContext>, tokens: Array<string | undefined>) {
  for (const token of tokens) {
    expect(token).toBeTruthy()
    const outsider = await newContext()
    const response = await outsider.post('/api/auth/refresh', { headers: { Cookie: `whm_refresh=${token}` } })
    expect(response.status()).toBe(401)
    await outsider.dispose()
  }
}

test.describe('Logout during a token refresh', () => {
  test.describe.configure({ timeout: 120_000 })

  for (const locking of ['Web Locks', 'no Web Locks']) {
    test(`a refresh response that lands after the click cannot revive the session (${locking})`, async ({
      page,
      context,
      playwright,
      baseURL,
    }) => {
      if (locking === 'no Web Locks') {
        await page.addInitScript(() => {
          Object.defineProperty(Navigator.prototype, 'locks', { configurable: true, get: () => undefined })
        })
      }
      const newContext = () => playwright.request.newContext({ baseURL })
      await loginAsAdmin(page)
      expect(await page.evaluate(() => !!navigator.locks)).toBe(locking === 'Web Locks')
      const originalRefresh = await cookieValue(context, 'whm_refresh')
      expect(originalRefresh).toBeTruthy()

      const order: string[] = []
      recordSessionRequests(context, order)
      const held = await holdRefreshResponse(page, newContext, order)
      await startProactiveRefresh(page)
      await held.started

      // The user is out at once, refresh or not.
      await logout(page)
      await expect(page).toHaveURL(/\/login/)
      // Give an unordered logout every chance to go out first.
      await page.waitForTimeout(1_000)
      expect(order).toEqual([])
      // The harness really holds the cookie write back.
      expect(await cookieValue(context, 'whm_refresh')).toBe(originalRefresh)
      expect(held.successorRefresh()).toBeTruthy()

      const loggedOut = page.waitForResponse('**/api/auth/logout')
      held.release()
      await loggedOut
      expect(order).toEqual(['refresh landed', 'revoke', 'logout'])

      expect(await cookieValue(context, 'whm_access')).toBeUndefined()
      expect(await cookieValue(context, 'whm_refresh')).toBeUndefined()
      expect((await page.request.get('/api/me')).status()).toBe(401)
      // Neither the original refresh token nor its successor mints a session.
      await expectRefreshRejected(newContext, [originalRefresh, held.successorRefresh()])
    })
  }

  test("another tab's refresh lands first, then the logout, then a new sign-in that survives", async ({
    page,
    context,
    playwright,
    baseURL,
  }) => {
    const newContext = () => playwright.request.newContext({ baseURL })
    await loginAsAdmin(page)
    const originalRefresh = await cookieValue(context, 'whm_refresh')
    const other = await context.newPage()
    await other.goto('/', { waitUntil: 'domcontentloaded' })

    const order: string[] = []
    recordSessionRequests(context, order)
    const held = await holdRefreshResponse(other, newContext, order)
    await startProactiveRefresh(other)
    await held.started

    await logout(page)
    await expect(page).toHaveURL(/\/login/)
    // Signing in as B waits (busy) for the refresh and the logout.
    const signedIn = signInHere(page, TEST_USERS.manager)
    await page.waitForTimeout(1_000)
    expect(order).toEqual([])
    expect(await cookieValue(context, 'whm_refresh')).toBe(originalRefresh)

    held.release()
    await signedIn
    expect(order.slice(0, 3)).toEqual(['refresh landed', 'revoke', 'logout'])

    // B's session is the one in the jar, and it works.
    const sessionB = await cookieValue(context, 'whm_refresh')
    expect(sessionB).toBeTruthy()
    expect(sessionB).not.toBe(originalRefresh)
    expect(sessionB).not.toBe(held.successorRefresh())
    const me = await page.request.get('/api/me')
    expect(me.status()).toBe(200)
    expect((await me.json()).data.email).toBe(TEST_USERS.manager.email)
    await expectRefreshRejected(newContext, [originalRefresh, held.successorRefresh()])
  })

  test('an SSO sign-in started during a queued logout is never revoked by it', async ({
    page,
    context,
    playwright,
    baseURL,
  }) => {
    const newContext = () => playwright.request.newContext({ baseURL })
    // B's real session, as an identity provider round trip would set it.
    const idp = await newContext()
    const signInB = await idp.post('/api/auth/login', {
      data: { email: TEST_USERS.manager.email, password: TEST_USERS.manager.password },
    })
    expect(signInB.status()).toBe(200)
    const cookiesB = (await idp.storageState()).cookies
    const refreshB = cookiesB.find((c) => c.name === 'whm_refresh')!.value
    const csrfB = cookiesB.find((c) => c.name === 'whm_csrf')!.value

    await loginAsAdmin(page)
    const originalRefresh = await cookieValue(context, 'whm_refresh')
    const other = await context.newPage()
    await other.goto('/', { waitUntil: 'domcontentloaded' })

    const order: string[] = []
    const sent = recordSessionRequests(context, order)
    const held = await holdRefreshResponse(other, newContext, order)
    await startProactiveRefresh(other)
    await held.started

    await logout(page)
    await expect(page).toHaveURL(/\/login/)

    await page.route(/\/api\/auth\/sso\/providers(?:\?.*)?$/, (route) =>
      route.fulfill({ json: { data: [{ provider: 'google', name: 'Google' }] } }),
    )
    await page.route(/\/api\/auth\/sso\/google\/init(?:\?.*)?$/, async (route) => {
      order.push('sso')
      await context.addCookies(cookiesB)
      await route.fulfill({ status: 302, headers: { location: '/auth/sso/callback' } })
    })
    await page.getByTestId('sso-workspace-code').fill('klinik-relive-a1b2c3d4')
    await page.getByTestId('sso-discover-button').click()
    await page.getByTestId('sso-provider-google').click()
    await page.waitForTimeout(1_000)
    // The SSO start waits for the refresh and the logout ahead of it.
    expect(order).toEqual([])

    held.release()
    await page.waitForURL(/\/auth\/sso\/callback/)
    await expect.poll(() => order.includes('sso')).toBe(true)
    expect(order.indexOf('logout')).toBeLessThan(order.indexOf('sso'))
    await page.waitForURL((url) => !url.pathname.includes('/auth/sso/callback'), { timeout: 15_000 })

    // Nothing of the old logout carried B's cookies or CSRF token...
    for (const request of sent) {
      expect(request.cookie).not.toContain(refreshB)
      expect(request.csrf).not.toBe(csrfB)
    }
    // ...and B's session is the one in the jar, and it works.
    expect(await cookieValue(context, 'whm_refresh')).toBe(refreshB)
    const me = await page.request.get('/api/me')
    expect(me.status()).toBe(200)
    expect((await me.json()).data.email).toBe(TEST_USERS.manager.email)
    await expectRefreshRejected(newContext, [originalRefresh, held.successorRefresh()])
    await idp.dispose()
  })

  test('a revoke still pending at the 7 s budget is aborted before the next sign-in goes out', async ({
    page,
    context,
    playwright,
    baseURL,
  }) => {
    const newContext = () => playwright.request.newContext({ baseURL })
    await loginAsAdmin(page)
    const originalRefresh = await cookieValue(context, 'whm_refresh')
    const isRevoke = (request: { url(): string; postData(): string | null }) =>
      request.url().endsWith('/api/auth/refresh') && (request.postData() ?? '').includes('"revoke"')
    let attempts = 0
    let lastAttempt: unknown = null
    let lastAttemptFailedAt = 0
    let loginSentAt = 0
    page.on('requestfailed', (request) => {
      if (request === lastAttempt) lastAttemptFailedAt = Date.now()
    })
    page.on('request', (request) => {
      if (request.url().endsWith('/api/auth/login') && !loginSentAt) loginSentAt = Date.now()
    })
    // Attempts 1 and 2 are rate-limited, each answer arriving after 1 s, so
    // attempt 3 goes out at ~5 s (its own 5 s timeout would fire at ~10 s).
    // It reaches the real server (which revokes and clears the cookies in its
    // answer), but that answer is held for 9 s: only the 7 s budget can end it.
    await page.route('**/api/auth/refresh', async (route) => {
      const request = route.request()
      if (!isRevoke(request)) {
        await route.continue()
        return
      }
      attempts++
      if (attempts <= 2) {
        await new Promise((resolve) => setTimeout(resolve, 1_000))
        await route.fulfill({ status: 429, json: { status: 'error', message: 'Too many requests' } })
        return
      }
      lastAttempt = request
      const upstream = await newContext()
      const headers = await request.allHeaders()
      delete headers['content-length']
      delete headers.host
      const response = await upstream.fetch(request.url(), {
        method: 'POST',
        headers,
        data: request.postDataBuffer() ?? undefined,
      })
      await new Promise((resolve) => setTimeout(resolve, 9_000))
      await route.fulfill({ response }).catch(() => undefined)
      await upstream.dispose()
    })

    const clickedAt = Date.now()
    await logout(page)
    await expect(page).toHaveURL(/\/login/)
    // B signs in at once; the sign-in waits for the logout to let go.
    await signInHere(page, TEST_USERS.manager)

    // The pending attempt was aborted at the budget (~7 s; not by its own
    // timeout, ~10 s), before the sign-in request went out.
    expect(attempts).toBe(3)
    expect(lastAttemptFailedAt).toBeGreaterThan(0)
    expect(lastAttemptFailedAt - clickedAt).toBeGreaterThanOrEqual(6_500)
    expect(lastAttemptFailedAt - clickedAt).toBeLessThanOrEqual(7_800)
    expect(lastAttemptFailedAt).toBeLessThanOrEqual(loginSentAt)

    // After the moment the held answer would have landed, B's session is the
    // one in the jar and it works.
    await page.waitForTimeout(Math.max(0, clickedAt + 16_000 - Date.now()))
    const refreshB = await cookieValue(context, 'whm_refresh')
    expect(refreshB).toBeTruthy()
    expect(refreshB).not.toBe(originalRefresh)
    const me = await page.request.get('/api/me')
    expect(me.status()).toBe(200)
    expect((await me.json()).data.email).toBe(TEST_USERS.manager.email)
    await expectRefreshRejected(newContext, [originalRefresh])
  })

  test('a logout that cannot get the lock in 20 s is deferred, and the next sign-in finishes it first', async ({
    page,
    context,
    playwright,
    baseURL,
  }) => {
    const newContext = () => playwright.request.newContext({ baseURL })
    await loginAsAdmin(page)
    const originalRefresh = await cookieValue(context, 'whm_refresh')
    const other = await context.newPage()
    await other.goto('/', { waitUntil: 'domcontentloaded' })
    const order: string[] = []
    recordSessionRequests(context, order)
    page.on('request', (request) => {
      if (request.url().endsWith('/api/auth/login')) order.push('login')
    })

    // Another tab is stuck holding the session lock for 27 s.
    await other.evaluate(() => {
      void navigator.locks.request('whm-token-refresh', () => new Promise((resolve) => setTimeout(resolve, 27_000)))
    })
    await logout(page)
    await expect(page).toHaveURL(/\/login/)
    await page.waitForTimeout(21_000)
    // Deferred, not sent unlocked: the jar still holds the old session and
    // the logout is recorded as pending.
    expect(order).toEqual([])
    expect(await cookieValue(context, 'whm_refresh')).toBe(originalRefresh)
    expect(await page.evaluate(() => localStorage.getItem('whm_logout_pending'))).toContain('ended:')

    await signInHere(page, TEST_USERS.manager)
    expect(order.slice(0, 3)).toEqual(['revoke', 'logout', 'login'])
    const me = await page.request.get('/api/me')
    expect((await me.json()).data.email).toBe(TEST_USERS.manager.email)
    await expectRefreshRejected(newContext, [originalRefresh])
  })

  test("a request sent as A that fails after B signed in is never replayed with B's cookies", async ({
    page,
    context,
  }) => {
    await loginAsAdmin(page)
    const stale = await context.newPage()
    // The stale tab's first websocket-token request (sent as A) is held.
    let releaseToken!: () => void
    const tokenGate = new Promise<void>((resolve) => {
      releaseToken = resolve
    })
    let held = false
    const tokenRequests: number[] = []
    let refreshes = 0
    await stale.route('**/api/auth/ws-token', async (route) => {
      tokenRequests.push(Date.now())
      if (held) {
        await route.continue()
        return
      }
      held = true
      await tokenGate
      await route.fulfill({ status: 401, json: { status: 'error', message: 'Unauthorized' } })
    })
    await stale.route('**/api/auth/refresh', async (route) => {
      if (!(route.request().postData() ?? '').includes('"revoke"')) refreshes++
      await route.continue()
    })
    await stale.goto('/', { waitUntil: 'domcontentloaded' })
    await expect.poll(() => tokenRequests.length).toBe(1)

    await logout(page)
    await signInHere(page, TEST_USERS.manager)
    const answeredAt = Date.now()
    releaseToken()
    await stale.waitForTimeout(2_000)

    // No refresh, and no immediate replay (a replay follows the 401 at once;
    // the socket's own reconnect waits at least 1 s).
    expect(refreshes).toBe(0)
    expect(tokenRequests.filter((at) => at > answeredAt && at < answeredAt + 500)).toEqual([])
    const me = await page.request.get('/api/me')
    expect((await me.json()).data.email).toBe(TEST_USERS.manager.email)
  })

  test('without Web Locks in any tab, a late refresh cannot replace a fresh sign-in', async ({
    page,
    context,
    playwright,
    baseURL,
  }) => {
    await context.addInitScript(() => {
      Object.defineProperty(Navigator.prototype, 'locks', { configurable: true, get: () => undefined })
    })
    const newContext = () => playwright.request.newContext({ baseURL })
    await loginAsAdmin(page)
    const originalRefresh = await cookieValue(context, 'whm_refresh')
    const other = await context.newPage()
    await other.goto('/', { waitUntil: 'domcontentloaded' })
    expect(await page.evaluate(() => !!navigator.locks)).toBe(false)
    expect(await other.evaluate(() => !!navigator.locks)).toBe(false)

    const order: string[] = []
    recordSessionRequests(context, order)
    const held = await holdRefreshResponse(other, newContext, order)
    await startProactiveRefresh(other)
    await held.started
    // The harness holds the other tab's rotated cookies back.
    expect(await cookieValue(context, 'whm_refresh')).toBe(originalRefresh)
    expect(held.successorRefresh()).toBeTruthy()

    await logout(page)
    await expect(page).toHaveURL(/\/login/)
    // The same user signs in again at once: a fresh session behind the same
    // /me, so only the exact cookies can show which session the jar holds.
    const signInAnswer = page.waitForResponse('**/api/auth/login')
    await signInHere(page, TEST_USERS.admin)
    const issued = setCookies(await (await signInAnswer).headersArray())
    expect(issued.whm_refresh).toBeTruthy()

    held.release()
    await other.waitForTimeout(3_000)

    // The jar holds exactly the fresh sign-in's cookies.
    for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
      expect(await cookieValue(context, name)).toBe(issued[name])
    }
    expect((await page.request.get('/api/me')).status()).toBe(200)
    await expectRefreshRejected(newContext, [originalRefresh, held.successorRefresh()])
  })
})
