import {
  test,
  expect,
  request as playwrightRequest,
  type APIRequestContext,
  type BrowserContext,
  type Page,
} from '@playwright/test'
import { ApiHelper, loginAsAdmin, logout, TEST_USERS, type TestUser } from '../../helpers'
import { createLongStayObserver } from '../../helpers/long-stay-observer'
import {
  createTestScope,
  createUserWithPermissions,
  loginAs,
  SUPER_ADMIN,
  type TestUserHandle,
} from '../../framework'

// No cookie-setting answer may land on the wrong session. A refresh, sign-in
// or org switch answer sets new auth cookies; if it lands after a logout or a
// newer sign-in, the browser holds the wrong session. So:
// - every cookie-changing request runs under one Web Lock shared by all tabs;
// - a session change in one tab aborts another tab's refresh in flight (its
//   late Set-Cookie is discarded);
// - the SSO callback sets no cookie: the callback page exchanges a one-time
//   code for the session, under the same lock, like a password sign-in;
// - without Web Locks the app signs nobody in (fail closed).
//
// Answers are held back for real: each is fetched upstream through a separate
// request context (its own cookie jar), so the browser's jar only changes when
// route.fulfill delivers it. Each test first proves that.
//
// Negative controls: against 3d086ef5 (before R1-AUTH-1) the ordering
// assertions here fail (the revoke and logout go out before the refresh lands,
// and the late Set-Cookie signs the browser back in). Against 04344c17 the 7 s
// case fails: that build only raced the revoke against the budget, so the
// pending attempt was never aborted; its held answer (which revokes and clears
// the cookies) lands after B has signed in and clears B's cookies, and the jar
// and /me checks fail. A build without the budget abort never fails the
// attempt at all (it gets its answer at ~7.5 s). Expected to fail, at gate
// time:
// - the "page cannot sign in" case against f9f8c65d (that build signed in
//   through a best-effort fallback);
// - the SSO exchange ordering cases against this build with the exchange not
//   session-issuing (isSessionIssuing without `|| config?._ssoExchange ===
//   true`): the pending-lock, order and refresh-count checks fail. (Against
//   fdb8937b they fail earlier, at the exchange that is never sent, which
//   shows nothing about the ordering checks.)
// - the no-Web-Locks logout cases against 3ee42429 (it sends a clearing
//   /auth/logout), and their login-page notice against 0938ea91.
//
// The identity provider is not run here: the callback page is opened with a
// code, and its exchange is answered with a real session of the user (a
// password sign-in upstream). The server side of the exchange (single use,
// binding cookie, expiry, no cookie on the callback) has its own Go tests.

type Held = {
  // The browser sent the request (it reached the route handler).
  started: Promise<void>
  // The upstream answer is in hand (status, headers and cookies captured):
  // the server has acted, the browser's jar is still unchanged.
  ready: Promise<void>
  release: () => void
  // The auth cookie values the held answer sets.
  cookies: () => Record<string, string>
}

// holdResponse holds the next answer of `page` to `path` (under /api) until
// release(). Revoke calls (POST /auth/refresh {revoke:true}) pass through.
async function holdResponse(
  page: Page,
  path: string,
  newContext: () => Promise<APIRequestContext>,
  order: string[],
  landed: string,
): Promise<Held> {
  let cookies: Record<string, string> = {}
  let release!: () => void
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  let seen!: () => void
  const started = new Promise<void>((resolve) => {
    seen = resolve
  })
  let captured!: () => void
  const ready = new Promise<void>((resolve) => {
    captured = resolve
  })
  let held = false
  await page.route(`**/api${path}`, async (route) => {
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
    cookies = setCookies(response.headersArray())
    captured()
    await gate
    order.push(landed)
    // The browser may have aborted it meanwhile: then nothing is delivered.
    await route.fulfill({ response }).catch(() => undefined)
    await upstream.dispose()
  })
  return { started, ready, release: () => release(), cookies: () => cookies }
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

function isRefresh(request: { url(): string; postData(): string | null }) {
  return request.url().endsWith('/api/auth/refresh') && !(request.postData() ?? '').includes('"revoke"')
}

// Counts the refresh requests (not revokes) `page` sends.
function countRefreshes(page: Page) {
  let count = 0
  page.on('request', (request) => {
    if (isRefresh(request)) count++
  })
  return () => count
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

// Fills the password form on a login page that is already open (no reload).
async function submitSignIn(page: Page, user: TestUser) {
  await page.locator('input[name="email"], input[type="email"]').fill(user.email)
  await page.locator('input[name="password"], input[type="password"]').fill(user.password)
  await page.getByTestId('password-sign-in').click()
}

// Signs in on a login page that is already open (no reload).
async function signInHere(page: Page, user: TestUser) {
  await submitSignIn(page, user)
  await page.waitForURL((url) => !url.pathname.includes('/login'), { timeout: 45_000 })
}

// The status a refresh with `token` gets now (from a context of its own).
async function refreshStatus(newContext: () => Promise<APIRequestContext>, token: string | undefined) {
  expect(token).toBeTruthy()
  const outsider = await newContext()
  try {
    return (await outsider.post('/api/auth/refresh', { headers: { Cookie: `whm_refresh=${token}` } })).status()
  } finally {
    await outsider.dispose()
  }
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

function isExchange(request: { url(): string; postData(): string | null }) {
  return request.url().endsWith('/api/auth/refresh') && (request.postData() ?? '').includes('"sso_exchange"')
}

// Answers the next SSO code exchange of `page` with a real session of `user`
// (as the server would for a valid code), held until release(); `status`
// answers it with a rejection instead.
async function answerExchange(
  page: Page,
  newContext: () => Promise<APIRequestContext>,
  user: TestUser,
  order: string[],
  options: { hold?: boolean; status?: number } = {},
): Promise<Held> {
  let cookies: Record<string, string> = {}
  let release!: () => void
  const gate = new Promise<void>((resolve) => {
    release = resolve
  })
  if (!options.hold) release()
  let seen!: () => void
  const started = new Promise<void>((resolve) => {
    seen = resolve
  })
  let captured!: () => void
  const ready = new Promise<void>((resolve) => {
    captured = resolve
  })
  await page.route('**/api/auth/refresh', async (route) => {
    if (!isExchange(route.request())) {
      await route.continue()
      return
    }
    order.push('exchange')
    seen()
    if (options.status) {
      captured()
      await route.fulfill({ status: options.status, json: { status: 'error', message: 'Invalid or expired sign-in, please try again' } })
      return
    }
    const upstream = await newContext()
    const response = await upstream.post('/api/auth/login', { data: { email: user.email, password: user.password } })
    cookies = setCookies(response.headersArray())
    captured()
    await gate
    order.push('exchange landed')
    await route.fulfill({ response }).catch(() => undefined)
    await upstream.dispose()
  })
  return { started, ready, release: () => release(), cookies: () => cookies }
}

// Lets `page` discover the Google SSO provider for a workspace code.
async function offerGoogleSso(page: Page) {
  await page.route(/\/api\/auth\/sso\/providers(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: [{ provider: 'google', name: 'Google' }] } }),
  )
}

async function startGoogleSso(page: Page) {
  await page.getByTestId('sso-workspace-code').fill('klinik-relive-a1b2c3d4')
  await page.getByTestId('sso-discover-button').click()
  await page.getByTestId('sso-provider-google').click()
}

// Opens, in `page`, a same-origin page that is never the app (answered by the
// test itself, whatever serves BASE_URL: the Go server or the dev server's SPA
// fallback). Such a page runs no session code: it holds a lock, or just waits.
async function openNonAppPage(page: Page) {
  await page.route('**/__lane_a_blank', (route) =>
    route.fulfill({ contentType: 'text/html', body: '<!doctype html><html><body></body></html>' }),
  )
  await page.goto('/__lane_a_blank', { waitUntil: 'domcontentloaded' })
}

// The Web Locks client id of `page`'s current document: it holds a probe lock
// of its own for a moment and reads its id back from navigator.locks.query().
async function lockClientId(page: Page): Promise<string> {
  return page.evaluate(async () => {
    const name = `lane-a-probe-${Math.random().toString(36).slice(2)}`
    return navigator.locks.request(name, async () => {
      const state = await navigator.locks.query()
      return state.held?.find((lock) => lock.name === name)?.clientId ?? ''
    })
  })
}

// Pages by their Web Locks client id, so a lock snapshot can name them.
type LockClients = Record<string, string>

async function lockClients(pages: Record<string, Page>): Promise<LockClients> {
  const clients: LockClients = {}
  for (const [label, page] of Object.entries(pages)) clients[await lockClientId(page)] = label
  return clients
}

// Who holds the shared session lock, and who waits for it (in order), as the
// origin sees it now, each client named.
async function sessionLockSnapshot(probe: Page, clients: LockClients) {
  const state = await probe.evaluate(() => navigator.locks.query())
  const named = (locks: Array<{ name?: string; clientId?: string }> | undefined) =>
    (locks ?? [])
      .filter((lock) => lock.name === 'whm-token-refresh')
      .map((lock) => clients[lock.clientId ?? ''] ?? `unknown client ${lock.clientId}`)
  return { held: named(state.held), pending: named(state.pending) }
}

// Expects exactly `count` requests of the page named `label` to be waiting for
// the shared session lock. A failure reports the whole snapshot.
async function expectWaitingSessionLocks(probe: Page, clients: LockClients, label: string, count: number) {
  let last: { held: string[]; pending: string[] } = { held: [], pending: [] }
  try {
    await expect
      .poll(async () => {
        last = await sessionLockSnapshot(probe, clients)
        return last.pending.filter((name) => name === label).length
      })
      .toBe(count)
  } catch (error) {
    throw new Error(
      `${(error as Error).message}\nsession lock: held by [${last.held.join(', ')}]; waiting: [${last.pending.join(', ')}]`,
    )
  }
}

// SPA navigation (no reload): the router opens `path` in the running app.
async function navigateInApp(page: Page, path: string) {
  await page.evaluate((to) => {
    window.history.pushState({}, '', to)
    window.dispatchEvent(new PopStateEvent('popstate', { state: {} }))
  }, path)
}

// Answers the next GET of `page` matching `pattern` with a 401 (once).
// Resolves once the route is in place, with `served`, which settles when the
// 401 has gone out (an object, so awaiting the route does not wait for it).
async function unauthorizedOnce(page: Page, pattern: RegExp): Promise<{ served: Promise<void> }> {
  let answered = false
  let served!: () => void
  const done = new Promise<void>((resolve) => {
    served = resolve
  })
  await page.route(pattern, async (route) => {
    if (answered || route.request().method() !== 'GET') {
      await route.continue()
      return
    }
    answered = true
    await route.fulfill({ status: 401, json: { status: 'error', message: 'Unauthorized' } })
    served()
  })
  return { served: done }
}

const UNSUPPORTED = "This browser can't keep sessions safe across tabs."

// Counts the "unsupported browser" messages a page shows (from its load on).
const countUnsupportedMessages = (text: string) => {
  const counter = window as unknown as { __unsupported: number }
  counter.__unsupported = 0
  new MutationObserver((records) => {
    for (const record of records) {
      for (const node of Array.from(record.addedNodes)) {
        if (node.textContent?.includes(text)) counter.__unsupported++
      }
    }
  }).observe(document, { subtree: true, childList: true })
}

async function unsupportedMessages(page: Page) {
  return page.evaluate(() => (window as unknown as { __unsupported: number }).__unsupported)
}

const withoutWebLocks = () => {
  Object.defineProperty(Navigator.prototype, 'locks', { configurable: true, get: () => undefined })
}

test.describe('Logout during a token refresh', () => {
  test.describe.configure({ timeout: 120_000 })

  test('a refresh response that lands after the click cannot revive the session', async ({
    page,
    context,
    playwright,
    baseURL,
  }) => {
    const newContext = () => playwright.request.newContext({ baseURL })
    await loginAsAdmin(page)
    expect(await page.evaluate(() => !!navigator.locks)).toBe(true)
    const originalRefresh = await cookieValue(context, 'whm_refresh')
    expect(originalRefresh).toBeTruthy()

    const order: string[] = []
    recordSessionRequests(context, order)
    const held = await holdResponse(page, '/auth/refresh', newContext, order, 'refresh landed')
    await startProactiveRefresh(page)
    await held.ready
    // The harness really holds the cookie write back.
    expect(await cookieValue(context, 'whm_refresh')).toBe(originalRefresh)
    expect(held.cookies().whm_refresh).toBeTruthy()

    // The user is out at once, refresh or not.
    await logout(page)
    await expect(page).toHaveURL(/\/login/)
    // Give an unordered logout every chance to go out first.
    await page.waitForTimeout(1_000)
    expect(order).toEqual([])

    const loggedOut = page.waitForResponse('**/api/auth/logout')
    held.release()
    await loggedOut
    expect(order).toEqual(['refresh landed', 'revoke', 'logout'])

    expect(await cookieValue(context, 'whm_access')).toBeUndefined()
    expect(await cookieValue(context, 'whm_refresh')).toBeUndefined()
    expect((await page.request.get('/api/me')).status()).toBe(401)
    // Neither the original refresh token nor its successor mints a session.
    await expectRefreshRejected(newContext, [originalRefresh, held.cookies().whm_refresh])
  })

  for (const ending of ['its first refresh need', 'a logout click'] as const) {
    test(`without Web Locks a session from before ends at ${ending}: a cookie-neutral revoke, no cookie cleared`, async ({
      page,
      context,
      playwright,
      baseURL,
    }) => {
      const newContext = () => playwright.request.newContext({ baseURL })
      await loginAsAdmin(page)
      // A session that already existed, in a page without Web Locks: it can
      // order no cookie change against other tabs, so it makes none.
      await page.addInitScript(withoutWebLocks)
      await page.reload({ waitUntil: 'domcontentloaded' })
      expect(await page.evaluate(() => !!navigator.locks)).toBe(false)
      const before: Record<string, string | undefined> = {}
      for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
        before[name] = await cookieValue(context, name)
        expect(before[name]).toBeTruthy()
      }
      const order: string[] = []
      recordSessionRequests(context, order)
      const refreshes = countRefreshes(page)
      const revokeBodies: unknown[] = []
      page.on('request', (request) => {
        const body = request.postData() ?? ''
        if (request.url().endsWith('/api/auth/refresh') && body.includes('"revoke"')) revokeBodies.push(JSON.parse(body))
      })

      // The revoke's request, not its answer: the page leaves for /login at
      // once, and its document goes away with the answer still out (the
      // keepalive request itself outlives the navigation).
      const revoked = page.waitForRequest(
        (request) => request.url().endsWith('/api/auth/refresh') && (request.postData() ?? '').includes('"revoke"'),
        { timeout: 10_000 },
      )
      if (ending === 'a logout click') await logout(page)
      else await startProactiveRefresh(page)
      await expect(page).toHaveURL(/\/login/)
      // The login page says why at once, not only after a sign-in attempt.
      await expect(page.getByTestId('login-unsupported-notice')).toContainText(UNSUPPORTED)
      await revoked
      // Give a clearing /auth/logout every chance to go out.
      await page.waitForTimeout(2_000)

      // Exactly one revoke, cookie-neutral (a page without the lock never
      // takes a pending logout over, at load or at pagehide); no refresh, no
      // /auth/logout, no cookie changed.
      expect(order).toEqual(['revoke'])
      expect(revokeBodies).toEqual([{ revoke: true, keep_cookies: true }])
      expect(refreshes()).toBe(0)
      for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
        expect(await cookieValue(context, name)).toBe(before[name])
      }
      // The refresh chain is revoked: the keepalive revoke survives the
      // navigation, and the server may finish it just after; the clearing
      // waits for a page that holds the lock.
      await expect.poll(() => refreshStatus(newContext, before.whm_refresh), { timeout: 10_000 }).toBe(401)
      const pending = await page.evaluate(() => localStorage.getItem('whm_logout_pending'))
      expect(JSON.parse(pending!).marker).toMatch(/^ended:/)
      expect(await page.evaluate(() => localStorage.getItem('whm_session_id'))).toMatch(/^ended:/)

      // The next page that holds the lock finishes the clearing, ordered:
      // its own revoke (clearing), then the one /auth/logout.
      const other = await context.newPage()
      const otherRevokeBodies: unknown[] = []
      other.on('request', (request) => {
        const body = request.postData() ?? ''
        if (request.url().endsWith('/api/auth/refresh') && body.includes('"revoke"')) otherRevokeBodies.push(JSON.parse(body))
      })
      const cleared = other.waitForResponse('**/api/auth/logout')
      await other.goto('/login', { waitUntil: 'domcontentloaded' })
      expect(await other.evaluate(() => !!navigator.locks)).toBe(true)
      await cleared
      expect(order).toEqual(['revoke', 'revoke', 'logout'])
      expect(otherRevokeBodies).toEqual([{ revoke: true }])
      expect(await cookieValue(context, 'whm_access')).toBeUndefined()
      expect(await cookieValue(context, 'whm_refresh')).toBeUndefined()
      await expect.poll(() => other.evaluate(() => localStorage.getItem('whm_logout_pending'))).toBeNull()
    })
  }

  test("another tab's refresh in flight is aborted by the logout, and a new sign-in survives", async ({
    page,
    context,
    playwright,
    baseURL,
  }) => {
    const episode = await observeLogoutEpisode(context, baseURL)
    const newContext = () => playwright.request.newContext({ baseURL })
    await loginAsAdmin(page)
    const originalRefresh = await cookieValue(context, 'whm_refresh')
    const other = await context.newPage()
    await other.goto('/', { waitUntil: 'domcontentloaded' })

    const order: string[] = []
    recordSessionRequests(context, order)
    other.on('requestfailed', (request) => {
      if (isRefresh(request)) order.push('refresh failed')
    })
    // This tab waits on a non-app page while the other tab's refresh comes
    // due (the expiry is shared: an app page here could take that refresh
    // first), and returns to the app once it is in flight, with the expiry
    // due no more.
    await openNonAppPage(page)
    const held = await holdResponse(other, '/auth/refresh', newContext, order, 'refresh landed')
    await startProactiveRefresh(other)
    await held.ready
    await other.evaluate(() => localStorage.setItem('whm_auth_expires_at', String(Date.now() + 60 * 60_000)))
    await page.goto('/', { waitUntil: 'domcontentloaded' })
    // The harness holds the other tab's rotated cookies back.
    expect(await cookieValue(context, 'whm_refresh')).toBe(originalRefresh)
    expect(held.cookies().whm_refresh).toBeTruthy()

    episode.markLogoutIntent()
    await logout(page)
    await expect(page).toHaveURL(/\/login/)
    // The logout aborts the other tab's refresh at once (its late Set-Cookie
    // can no longer land), so the revoke and the logout go right out, while
    // the refresh answer is still held.
    await expect.poll(() => order.includes('refresh failed'), { timeout: 5_000 }).toBe(true)
    // Count logical completion: an extra clearing attempt needs positive
    // interrupted/unacknowledged pending ownership evidence, never just a
    // permissive duplicate count. Settle A completely before starting B.
    await episode.settled(page)

    episode.markBeforeB()
    await signInHere(page, TEST_USERS.manager)
    const sessionB = await cookieValue(context, 'whm_refresh')
    // The held answer comes too late: nothing of it is delivered.
    held.release()
    await expect.poll(() => order.includes('refresh landed')).toBe(true)
    await other.waitForTimeout(1_000)
    expect(order.indexOf('logout')).toBeLessThan(order.indexOf('refresh landed'))

    // B's session is the one in the jar, and it works.
    expect(sessionB).toBeTruthy()
    expect(sessionB).not.toBe(originalRefresh)
    expect(sessionB).not.toBe(held.cookies().whm_refresh)
    expect(await cookieValue(context, 'whm_refresh')).toBe(sessionB)
    const me = await page.request.get('/api/me')
    expect(me.status()).toBe(200)
    expect((await me.json()).data.email).toBe(TEST_USERS.manager.email)
    await expectRefreshRejected(newContext, [originalRefresh, held.cookies().whm_refresh])
    episode.assertNoLateClear()
  })

  test('an SSO sign-in started during a queued logout is never revoked by it', async ({
    page,
    context,
    playwright,
    baseURL,
  }) => {
    const newContext = () => playwright.request.newContext({ baseURL })
    await loginAsAdmin(page)
    const originalRefresh = await cookieValue(context, 'whm_refresh')
    // The lock holder is a same-origin page that is not the app: an app tab
    // showing the session would leave it when it ends (a full navigation,
    // which lets go of the lock).
    const other = await context.newPage()
    await openNonAppPage(other)

    const order: string[] = []
    const sent = recordSessionRequests(context, order)
    // Another tab holds the session lock for 8 s: the logout queues behind it.
    await other.evaluate(() => {
      void navigator.locks.request('whm-token-refresh', () => new Promise((resolve) => setTimeout(resolve, 8_000)))
    })

    await logout(page)
    await expect(page).toHaveURL(/\/login/)

    // B's SSO: the provider sends the browser back with a one-time code; the
    // callback page's exchange is answered with B's session.
    const exchange = await answerExchange(page, newContext, TEST_USERS.manager, order)
    await offerGoogleSso(page)
    await page.route(/\/api\/auth\/sso\/google\/init(?:\?.*)?$/, (route) =>
      route.fulfill({ status: 302, headers: { location: '/auth/sso/callback?code=e2e-code-b' } }),
    )
    // The page leaves with its logout still queued behind the held lock.
    expect(order).not.toContain('logout')
    expect(await page.evaluate(() => localStorage.getItem('whm_logout_pending'))).toContain('ended:')
    await startGoogleSso(page)
    await exchange.started
    await page.waitForURL((url) => !url.pathname.includes('/auth/sso/callback'), { timeout: 30_000 })
    // Anything that would still go out after the exchange has had time to.
    await page.waitForTimeout(1_000)

    // The exchange waited for the logout ahead of it: the one /auth/logout,
    // and every revoke, went out before it, and nothing of the old logout
    // after it. (The page left for the provider while its logout waited: with
    // no other app tab to take it over, the closing page revoked with
    // keepalive, cookie-neutral, and the callback page finished the logout,
    // ordered, before its exchange.)
    const exchangeAt = order.indexOf('exchange')
    expect(exchangeAt).toBeGreaterThan(0)
    expect(order.filter((kind) => kind === 'logout')).toHaveLength(1)
    expect(order.indexOf('logout')).toBeLessThan(exchangeAt)
    expect(order.lastIndexOf('revoke')).toBeLessThan(exchangeAt)
    const issued = exchange.cookies()
    // Nothing of the old logout carried B's cookies or CSRF token...
    for (const request of sent) {
      expect(request.cookie).not.toContain(issued.whm_refresh)
      expect(request.csrf).not.toBe(issued.whm_csrf)
    }
    // ...and B's session is the one in the jar, exactly, and it works.
    for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
      expect(await cookieValue(context, name)).toBe(issued[name])
    }
    const me = await page.request.get('/api/me')
    expect(me.status()).toBe(200)
    expect((await me.json()).data.email).toBe(TEST_USERS.manager.email)
    await expectRefreshRejected(newContext, [originalRefresh])
  })

  for (const first of ['refresh', 'exchange'] as const) {
    test(`an SSO exchange and another tab's refresh never overlap (${first} first): the exchange's cookies stay`, async ({
      page,
      context,
      playwright,
      baseURL,
    }) => {
      const newContext = () => playwright.request.newContext({ baseURL })
      await loginAsAdmin(page)
      const other = await context.newPage()
      await other.goto('/', { waitUntil: 'domcontentloaded' })
      const order: string[] = []
      const exchange = await answerExchange(page, newContext, TEST_USERS.manager, order, { hold: first === 'exchange' })
      const refreshes = countRefreshes(other)

      if (first === 'refresh') {
        // The other tab's refresh (A's cookies) is in flight; then this tab's
        // callback page asks for B's session: the exchange waits for it. This
        // tab waits on a non-app page meanwhile: the expiry is shared, and an
        // app page here could take that refresh first.
        await openNonAppPage(page)
        const held = await holdResponse(other, '/auth/refresh', newContext, order, 'refresh landed')
        await startProactiveRefresh(other)
        await held.ready
        // The expiry is shared: left due, the callback page would also queue a
        // proactive refresh of the old session at load (a second waiting
        // request of its own, which the exchange's answer retires). Due no
        // more, the callback page's one waiting request is its exchange.
        await other.evaluate(() => localStorage.setItem('whm_auth_expires_at', String(Date.now() + 60 * 60_000)))
        await page.goto('/auth/sso/callback?code=e2e-code-b', { waitUntil: 'domcontentloaded' })
        const clients = await lockClients({ 'callback page': page, 'other tab': other })
        await expectWaitingSessionLocks(page, clients, 'callback page', 1)
        expect((await sessionLockSnapshot(page, clients)).held).toEqual(['other tab'])
        expect(order).toEqual([])
        held.release()
        await exchange.started
        expect(order.slice(0, 2)).toEqual(['refresh landed', 'exchange'])
      } else {
        // The exchange is in flight (its answer held); then the other tab's
        // API call gets a 401: its refresh waits, and sends nothing with A's
        // cookies.
        await page.goto('/auth/sso/callback?code=e2e-code-b', { waitUntil: 'domcontentloaded' })
        await exchange.ready
        const unauthorized = (await unauthorizedOnce(other, /\/api\/users(\?.*)?$/)).served
        await navigateInApp(other, '/settings/users')
        await unauthorized
        const clients = await lockClients({ 'callback page': page, 'other tab': other })
        await expectWaitingSessionLocks(other, clients, 'other tab', 1)
        await other.waitForTimeout(2_000)
        expect(refreshes()).toBe(0)
        exchange.release()
      }
      await page.waitForURL((url) => !url.pathname.includes('/auth/sso/callback'), { timeout: 30_000 })
      await other.waitForTimeout(3_000)

      // The jar holds exactly B's session (as the exchange issued it).
      if (first === 'exchange') expect(refreshes()).toBe(0)
      for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
        expect(await cookieValue(context, name)).toBe(exchange.cookies()[name])
      }
      const me = await page.request.get('/api/me')
      expect((await me.json()).data.email).toBe(TEST_USERS.manager.email)
    })
  }

  test('an SSO return after a long stay at the identity provider: open tabs refresh meanwhile, then the exchange', async ({
    page,
    context,
    playwright,
    baseURL,
  }, testInfo) => {
    const observation = createLongStayObserver(context, page)
    let originalFailed = false
    let observedOther: Page | undefined
    try {
      await observation.install(baseURL)
      const newContext = () => playwright.request.newContext({ baseURL })
      await loginAsAdmin(page)
      const other = await context.newPage()
      observation.other(other)
      observedOther = other
      await other.goto('/', { waitUntil: 'domcontentloaded' })
      const refreshes = countRefreshes(other)
      observation.mark('counter-installed')
      observation.snapshot(other, 'counter-installed')
      const order: string[] = []
      const exchange = await answerExchange(page, newContext, TEST_USERS.manager, order)

      // This tab leaves for the identity provider (a page without the app).
      await page.goto('/login', { waitUntil: 'domcontentloaded' })
      await offerGoogleSso(page)
      await page.route(/\/api\/auth\/sso\/google\/init(?:\?.*)?$/, (route) =>
        route.fulfill({ status: 302, headers: { location: '/__mock_idp' } }),
      )
      await page.route('**/__mock_idp', (route) =>
        route.fulfill({ contentType: 'text/html', body: '<html><body>Identity provider</body></html>' }),
      )
      await startGoogleSso(page)
      await page.waitForURL(/__mock_idp/)

      // Redirect chains bypass the destination route; load the non-app provider fixture directly.
      await page.goto('/__mock_idp', { waitUntil: 'domcontentloaded' })
      await expect(page.locator('body')).toHaveText('Identity provider')
      await expect(page.getByText('Identity provider', { exact: true })).toBeVisible()

      // Proactive refresh runs only in a visible, authenticated app tab.
      // Returning focus is a real browser action; the event below does not
      // override document visibility or wait for the application to mount.
      await other.bringToFront()
      const authenticatedNavigation = other.getByRole('navigation', { name: 'Main navigation', exact: true })
      await expect(authenticatedNavigation).toBeVisible()
      await expect(authenticatedNavigation.getByRole('button').filter({ hasText: TEST_USERS.admin.email })).toBeVisible()
      await expect.poll(() => other.evaluate(() => document.visibilityState)).toBe('visible')

      // Meanwhile the open tab's session needs a refresh: nothing holds it back.
      observation.mark('ready-before-wake')
      observation.snapshot(other, 'ready-before-wake')
      await startProactiveRefresh(other)
      observation.mark('wake-returned')
      observation.snapshot(other, 'wake-returned')
      await expect.poll(() => refreshes()).toBe(1)
      observation.mark('poll-passed')
      observation.snapshot(other, 'poll-passed')
      expect((await page.request.get('/api/me')).status()).toBe(200)

      // The provider sends the browser back, minutes later, with the code.
      await page.goto('/auth/sso/callback?code=e2e-code-b', { waitUntil: 'domcontentloaded' })
      await page.waitForURL((url) => !url.pathname.includes('/auth/sso/callback'), { timeout: 30_000 })

      for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
        expect(await cookieValue(context, name)).toBe(exchange.cookies()[name])
      }
      const me = await page.request.get('/api/me')
      expect((await me.json()).data.email).toBe(TEST_USERS.manager.email)
      observation.mark('case-passed')
      observation.snapshot(other, 'case-passed')
    } catch (error) {
      originalFailed = true
      observation.mark('case-failed')
      if (observedOther) observation.snapshot(observedOther, 'case-failed')
      throw error
    } finally {
      await observation.attach(testInfo, originalFailed)
    }
  })

  test('an SSO exchange cut off by a reload: every reload says so, and the session from before is never resumed', async ({
    page,
    context,
    playwright,
    baseURL,
  }) => {
    const newContext = () => playwright.request.newContext({ baseURL })
    await loginAsAdmin(page)
    const order: string[] = []
    const exchangesSent: string[] = []
    page.on('request', (request) => {
      if (isExchange(request)) exchangesSent.push(request.url())
    })
    // The exchange is answered with B's real session, held: the answer never
    // reaches the page...
    const exchange = await answerExchange(page, newContext, TEST_USERS.manager, order, { hold: true })
    await page.goto('/auth/sso/callback?code=e2e-code-b', { waitUntil: 'domcontentloaded' })
    await exchange.ready
    // ...but its cookies land unseen: B's session is the jar's.
    const issued = exchange.cookies()
    const host = new URL(baseURL!).hostname
    const paths: Record<string, string> = { whm_access: '/api', whm_refresh: '/api/auth/refresh', whm_csrf: '/' }
    await context.addCookies(
      Object.entries(paths).map(([name, path]) => ({ name, value: issued[name], domain: host, path })),
    )

    // Every reload of the callback page says the sign-in was interrupted;
    // none resumes A (still the stored identity) over B's jar.
    for (let reload = 0; reload < 2; reload++) {
      await page.reload({ waitUntil: 'domcontentloaded' })
      await expect(page.getByText(/interrupted before it finished/)).toBeVisible()
      expect(new URL(page.url()).pathname).toBe('/auth/sso/callback')
    }
    // Nor do the app's own routes, in this tab.
    await page.goto('/', { waitUntil: 'domcontentloaded' })
    await expect(page).toHaveURL(/\/login/)

    // The spent code was sent once, and nothing was revoked or cleared to get
    // here: B's session (the jar's) still works.
    expect(exchangesSent).toHaveLength(1)
    const me = await page.request.get('/api/me')
    expect(me.status()).toBe(200)
    expect((await me.json()).data.email).toBe(TEST_USERS.manager.email)
    exchange.release()
  })

  test('Back after an interrupted SSO exchange never restores A over the unseen B jar', async ({
    page,
    context,
    playwright,
    baseURL,
  }, testInfo) => {
    // The default project proves new-document Back. A separate, explicitly
    // selected natural-lifecycle project may remove only Playwright's cache
    // disable default; even then a cache restoration must be observed, never
    // assumed from the launch policy or forced with feature flags.
    const naturalLifecycle = testInfo.project.metadata.r1HistoryMode === 'natural'
    const cdp = await context.newCDPSession(page)
    await cdp.send('Page.enable')
    const { arguments: launchArguments } = await cdp.send('Browser.getBrowserCommandLine')
    const cacheFailures: string[][] = []
    cdp.on('Page.backForwardCacheNotUsed', (event) => {
      cacheFailures.push(event.notRestoredExplanations.map((reason) => reason.reason))
    })
    await page.addInitScript(() => {
      const observation = { id: crypto.randomUUID(), persistedShows: [] as boolean[] }
      ;(window as unknown as { __r1History: typeof observation }).__r1History = observation
      window.addEventListener('pageshow', (event) => observation.persistedShows.push(event.persisted))
    })
    const historyState = () => page.evaluate(() =>
      (window as unknown as { __r1History: { id: string; persistedShows: boolean[] } }).__r1History,
    )
    const newContext = () => playwright.request.newContext({ baseURL })
    let releaseExchange: (() => void) | undefined
    try {
      await loginAsAdmin(page)
      const authenticatedNavigation = page.getByRole('navigation', { name: 'Main navigation', exact: true })
      await expect(authenticatedNavigation).toBeVisible()
      await expect(authenticatedNavigation.getByRole('button').filter({ hasText: TEST_USERS.admin.email })).toBeVisible()
      const original = await historyState()
      const originalPath = new URL(page.url()).pathname
      const order: string[] = []
      recordSessionRequests(context, order)
      const exchangesSent: string[] = []
      page.on('request', (request) => {
        if (isExchange(request)) exchangesSent.push(request.url())
      })
      const exchange = await answerExchange(page, newContext, TEST_USERS.manager, order, { hold: true })
      releaseExchange = exchange.release
      await page.goto('/auth/sso/callback?code=e2e-history-code-b', { waitUntil: 'domcontentloaded' })
      await exchange.ready
      await expect.poll(() => page.evaluate(() => sessionStorage.getItem('whm_sso_sent'))).not.toBeNull()
      expect(await page.evaluate(() => JSON.parse(localStorage.getItem('user')!).email)).toBe(TEST_USERS.admin.email)

      // As in the reload uncertainty control, inject B's actual issued cookies
      // without delivering the response. This is not the separate real SSO
      // callback/exchange proof; it fixes the uncertain jar state under test.
      const issued = exchange.cookies()
      const paths: Record<string, string> = { whm_access: '/api', whm_refresh: '/api/auth/refresh', whm_csrf: '/' }
      const host = new URL(baseURL!).hostname
      for (const name of Object.keys(paths)) expect(issued[name]).toBeTruthy()
      await context.addCookies(
        Object.entries(paths).map(([name, path]) => ({ name, value: issued[name], domain: host, path })),
      )
      // Returning to the prior authenticated history entry must run the actual
      // router's interrupted-note check before it restores persisted user A.
      await page.goBack({ waitUntil: 'domcontentloaded' })
      let returned = original
      await expect.poll(async () => {
        let state: typeof original
        try {
          state = await historyState()
        } catch (error) {
          // Back can replace the execution context between poll attempts.
          // Only that transient observation failure is eligible for polling.
          if (error instanceof Error && error.message === 'page.evaluate: Execution context was destroyed, most likely because of a navigation') {
            return false
          }
          throw error
        }
        const ready = state.id !== original.id ? state.persistedShows.length > 0 : state.persistedShows.at(-1) === true
        if (ready) returned = state
        return ready
      }).toBe(true)
      const restoredFromCache = original.id === returned.id && returned.persistedShows.at(-1) === true
      const newDocument = original.id !== returned.id && returned.persistedShows.every((persisted) => !persisted)
      // Preserve the lifecycle observation even when the following security
      // oracle fails. A natural-launch new document leaves BFCache unqualified.
      await testInfo.attach('back-lifecycle-observation', {
        contentType: 'application/json',
        body: JSON.stringify({
          originalPath,
          returnedPath: new URL(page.url()).pathname,
          project: testInfo.project.name,
          browserVersion: context.browser()?.version(),
          naturalLifecycle,
          backForwardCacheDisabledByLaunch: launchArguments.includes('--disable-back-forward-cache'),
          newDocument,
          returnedPageshowPersisted: returned.persistedShows,
          cacheNotRestoredReasons: cacheFailures,
          cacheRestorationObserved: restoredFromCache,
          bfcacheSecurityQualified: false, // This observation precedes the assertions below.
        }),
      })
      if (naturalLifecycle) {
        expect(launchArguments).not.toContain('--disable-back-forward-cache')
        expect(restoredFromCache || newDocument).toBe(true)
      } else {
        expect(newDocument).toBe(true)
        expect(returned.persistedShows).toEqual([false])
      }
      await expect(page).toHaveURL(/\/login(?:\?|$)/)
      await expect(page.getByTestId('password-sign-in')).toBeVisible()
      await expect(authenticatedNavigation).toHaveCount(0)
      expect(await page.evaluate(() => sessionStorage.getItem('whm_sso_sent'))).not.toBeNull()
      expect(await page.evaluate(() => JSON.parse(localStorage.getItem('user')!).email)).toBe(TEST_USERS.admin.email)
      expect(exchangesSent).toHaveLength(1)
      expect(order.filter((kind) => kind === 'revoke' || kind === 'logout')).toEqual([])
      for (const name of Object.keys(paths)) expect(await cookieValue(context, name)).toBe(issued[name])
      const me = await page.request.get('/api/me')
      expect(me.status()).toBe(200)
      expect((await me.json()).data.email).toBe(TEST_USERS.manager.email)
    } finally {
      releaseExchange?.()
      await cdp.detach()
    }
  })

  test('a replayed or expired SSO code changes nothing', async ({ page, context, playwright, baseURL }) => {
    const newContext = () => playwright.request.newContext({ baseURL })
    await loginAsAdmin(page)
    const before: Record<string, string | undefined> = {}
    for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) before[name] = await cookieValue(context, name)
    const marker = await page.evaluate(() => localStorage.getItem('whm_session_id'))
    const order: string[] = []
    await answerExchange(page, newContext, TEST_USERS.manager, order, { status: 401 })

    await page.goto('/auth/sso/callback?code=a-used-code', { waitUntil: 'domcontentloaded' })
    await expect(page.getByText('Invalid or expired sign-in', { exact: false })).toBeVisible()

    expect(order).toEqual(['exchange'])
    for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
      expect(await cookieValue(context, name)).toBe(before[name])
    }
    expect(await page.evaluate(() => localStorage.getItem('whm_session_id'))).toBe(marker)
    expect((await (await page.request.get('/api/me')).json()).data.email).toBe(TEST_USERS.admin.email)
  })

  test('without Web Locks an SSO callback exchanges nothing and writes nothing', async ({ page, context }) => {
    await loginAsAdmin(page)
    const before: Record<string, string | undefined> = {}
    for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) before[name] = await cookieValue(context, name)
    const other = await context.newPage()
    await other.addInitScript(withoutWebLocks)
    const exchanges: string[] = []
    other.on('request', (request) => {
      if (isExchange(request)) exchanges.push(request.url())
    })

    await other.goto('/auth/sso/callback?code=e2e-code-b', { waitUntil: 'domcontentloaded' })
    await expect(other.getByText(UNSUPPORTED, { exact: false })).toBeVisible()

    expect(exchanges).toEqual([])
    for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
      expect(await cookieValue(context, name)).toBe(before[name])
    }
    expect((await (await page.request.get('/api/me')).json()).data.email).toBe(TEST_USERS.admin.email)
  })

  test('an API 401 while the refresh fails (a transient server fault) keeps the session', async ({
    page,
    context,
  }) => {
    // The websocket token request right after the sign-in gets a 401, and the
    // refresh answers 503 (every attempt).
    await unauthorizedOnce(page, /\/api\/auth\/ws-token(\?.*)?$/)
    let refreshes = 0
    await page.route('**/api/auth/refresh', async (route) => {
      if ((route.request().postData() ?? '').includes('"revoke"')) {
        await route.continue()
        return
      }
      refreshes++
      await route.fulfill({ status: 503, json: { status: 'error', message: 'Service unavailable' } })
    })
    await loginAsAdmin(page)
    const marker = await page.evaluate(() => localStorage.getItem('whm_session_id'))
    const refresh = await cookieValue(context, 'whm_refresh')
    // The refresh and its three retries (REFRESH_RETRY_DELAYS_MS) all fail...
    await expect.poll(() => refreshes, { timeout: 30_000 }).toBe(4)
    // ...and then nothing more is tried for this request.
    await page.waitForTimeout(1_000)
    expect(refreshes).toBe(4)

    // Still signed in: no sign-out, no ended session, the same cookies.
    expect(page.url()).not.toContain('/login')
    expect(await page.evaluate(() => localStorage.getItem('whm_session_id'))).toBe(marker)
    expect(await page.evaluate(() => localStorage.getItem('user'))).toBeTruthy()
    expect(await page.evaluate(() => localStorage.getItem('whm_logout_pending'))).toBeNull()
    expect(await cookieValue(context, 'whm_refresh')).toBe(refresh)
    await page.unroute('**/api/auth/refresh')
    expect((await page.request.get('/api/me')).status()).toBe(200)
  })

  test('an API 401 that comes back after a good refresh is replayed once, not looped', async ({ page }) => {
    // The websocket token request gets a 401 twice (the request and its one
    // replay); the refresh itself works.
    const tokenAt: number[] = []
    await page.route('**/api/auth/ws-token', async (route) => {
      tokenAt.push(Date.now())
      if (tokenAt.length <= 2) {
        await route.fulfill({ status: 401, json: { status: 'error', message: 'Unauthorized' } })
        return
      }
      await route.continue()
    })
    const refreshes = countRefreshes(page)
    await loginAsAdmin(page)
    await expect.poll(() => tokenAt.length, { timeout: 20_000 }).toBeGreaterThanOrEqual(2)
    await page.waitForTimeout(3_000)

    // One refresh, one replay right after it; anything later is the socket's
    // own reconnect (at least 1 s later), not a client loop.
    expect(refreshes()).toBe(1)
    if (tokenAt.length > 2) expect(tokenAt[2] - tokenAt[1]).toBeGreaterThanOrEqual(900)
    expect(page.url()).not.toContain('/login')
    expect(await page.evaluate(() => localStorage.getItem('user'))).toBeTruthy()
    expect((await page.request.get('/api/me')).status()).toBe(200)
  })

  test("a sign-in and another tab's refresh never overlap: the sign-in's cookies stay", async ({
    page,
    context,
    playwright,
    baseURL,
  }) => {
    const newContext = () => playwright.request.newContext({ baseURL })
    await loginAsAdmin(page)
    const other = await context.newPage()
    await other.goto('/', { waitUntil: 'domcontentloaded' })
    const refreshes = countRefreshes(other)

    // This tab signs in as B while A's session is live; B's answer is held.
    await page.goto('/login', { waitUntil: 'domcontentloaded' })
    const order: string[] = []
    const held = await holdResponse(page, '/auth/login', newContext, order, 'login landed')
    await submitSignIn(page, TEST_USERS.manager)
    await held.ready
    expect(held.cookies().whm_refresh).toBeTruthy()

    // An API call of the other tab gets a 401 while the sign-in holds the
    // lock (the sign-in already cleared the stored identity, so no proactive
    // refresh would run): its refresh waits for the lock, and sends nothing
    // with A's cookies.
    const unauthorized = (await unauthorizedOnce(other, /\/api\/users(\?.*)?$/)).served
    await navigateInApp(other, '/settings/users')
    await unauthorized
    const clients = await lockClients({ 'signing-in page': page, 'other tab': other })
    await expectWaitingSessionLocks(other, clients, 'other tab', 1)
    await other.waitForTimeout(2_000)
    expect(refreshes()).toBe(0)

    held.release()
    await page.waitForURL((url) => !url.pathname.includes('/login'), { timeout: 45_000 })
    await other.waitForTimeout(3_000)

    // The jar holds exactly the sign-in's cookies.
    expect(refreshes()).toBe(0)
    for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
      expect(await cookieValue(context, name)).toBe(held.cookies()[name])
    }
    const me = await page.request.get('/api/me')
    expect((await me.json()).data.email).toBe(TEST_USERS.manager.email)
  })

  test.describe('a user in two organizations', () => {
    const scope = createTestScope('logout-refresh-race-switch')
    let user: TestUserHandle
    let secondOrg: { id: string; name: string }

    test.beforeAll(async () => {
      const reqContext = await playwrightRequest.newContext()
      const api = new ApiHelper(reqContext)
      await api.login(SUPER_ADMIN.email, SUPER_ADMIN.password)
      user = await createUserWithPermissions(api, scope, {
        permissions: [{ resource: 'users', action: 'read' }],
      })
      const org = await api.createOrganization(scope.name('second-org'))
      secondOrg = { id: org.id, name: org.name }
      await api.addOrgMember(user.user.id, undefined, secondOrg.id)
      await reqContext.dispose()
    })

    test.afterAll(async () => {
      const reqContext = await playwrightRequest.newContext()
      const api = new ApiHelper(reqContext)
      await api.login(SUPER_ADMIN.email, SUPER_ADMIN.password)
      try { await api.removeOrgMember(user.user.id, secondOrg.id) } catch { /* ignore */ }
      try { await api.deleteUser(user.user.id) } catch { /* ignore */ }
      try { await api.deleteRole(user.role.id) } catch { /* ignore */ }
      await reqContext.dispose()
    })

    test("an org switch and another tab's refresh never overlap: the switch's cookies stay", async ({
      page,
      context,
      playwright,
      baseURL,
    }) => {
      const newContext = () => playwright.request.newContext({ baseURL })
      await loginAs(page, user)
      const other = await context.newPage()
      await other.goto('/profile', { waitUntil: 'domcontentloaded' })
      const refreshes = countRefreshes(other)
      const switchingTabRefreshes = countRefreshes(page)
      const oldRefresh = await cookieValue(context, 'whm_refresh')
      expect(oldRefresh).toBeTruthy()

      // This tab switches to the second organization; the answer is held.
      const order: string[] = []
      const held = await holdResponse(page, '/auth/refresh/switch-org', newContext, order, 'switch landed')
      await page.getByTestId('organization-switcher').click()
      await page.getByRole('option', { name: secondOrg.name }).click()
      await held.ready
      expect(held.cookies().whm_refresh).toBeTruthy()

      // The other tab's refresh comes due while the switch holds the lock: it
      // waits, and sends nothing with the pre-switch cookies. (The expiry is
      // shared, so the switching tab's own proactive refresh may come due
      // too, and wait the same way.)
      await startProactiveRefresh(other)
      const clients = await lockClients({ 'switching tab': page, 'other tab': other })
      await expectWaitingSessionLocks(other, clients, 'other tab', 1)
      await other.waitForTimeout(2_000)
      expect(refreshes()).toBe(0)
      expect(switchingTabRefreshes()).toBe(0)

      const reloaded = page.waitForEvent('domcontentloaded')
      held.release()
      await reloaded
      await other.waitForTimeout(3_000)

      // The jar holds exactly the switch's cookies, for the second organization;
      // neither tab refreshed (the switch's answer retired those refreshes).
      expect(refreshes()).toBe(0)
      expect(switchingTabRefreshes()).toBe(0)
      for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
        expect(await cookieValue(context, name)).toBe(held.cookies()[name])
      }
      const me = await page.request.get('/api/me')
      expect((await me.json()).data.organization_id).toBe(secondOrg.id)
      // Probe the old credential in an isolated jar: its rejection must not
      // clear or replace the browser's successful switch cookies.
      const replay = await newContext()
      try {
        const rejected = await replay.post('/api/auth/refresh', {
          headers: { Cookie: `whm_refresh=${oldRefresh}` },
          data: {},
        })
        expect(rejected.status()).toBe(401)
      } finally {
        await replay.dispose()
      }
      expect(await cookieValue(context, 'whm_refresh')).toBe(held.cookies().whm_refresh)
    })

    test('without Web Locks an org switch is refused, says why, and sends nothing', async ({ page, context }) => {
      await loginAs(page, user)
      const before: Record<string, string | undefined> = {}
      for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) before[name] = await cookieValue(context, name)
      const orgBefore = (await (await page.request.get('/api/me')).json()).data.organization_id
      const other = await context.newPage()
      await other.addInitScript(withoutWebLocks)
      await other.addInitScript(countUnsupportedMessages, UNSUPPORTED)
      await other.goto('/profile', { waitUntil: 'domcontentloaded' })
      expect(await other.evaluate(() => !!navigator.locks)).toBe(false)
      const switches: string[] = []
      other.on('request', (request) => {
        if (request.url().includes('/api/auth/refresh/switch-org')) switches.push(request.url())
      })
      let reloaded = false
      other.on('load', () => {
        reloaded = true
      })
      const selected = await other.evaluate(() => localStorage.getItem('selected_organization_id'))

      await other.getByTestId('organization-switcher').click()
      await other.getByRole('option', { name: secondOrg.name }).click()
      await expect.poll(() => unsupportedMessages(other)).toBeGreaterThan(0)
      await other.waitForTimeout(1_000)

      expect(switches).toEqual([])
      expect(reloaded).toBe(false)
      // Nothing was selected either: the stored selection is as it was.
      expect(await other.evaluate(() => localStorage.getItem('selected_organization_id'))).toBe(selected)
      for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
        expect(await cookieValue(context, name)).toBe(before[name])
      }
      const me = await page.request.get('/api/me', {
        headers: selected ? { 'X-Organization-ID': selected } : {},
      })
      expect((await me.json()).data.organization_id).toBe(orgBefore)
    })
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
    let firstAttemptAt = 0
    let lastAttempt: unknown = null
    let lastAttemptFailedAt = 0
    let loginSentAt = 0
    page.on('requestfailed', (request) => {
      if (request === lastAttempt) lastAttemptFailedAt = Date.now()
    })
    page.on('request', (request) => {
      if (request.url().endsWith('/api/auth/login') && !loginSentAt) loginSentAt = Date.now()
    })
    // Attempts 1 and 2 are rate-limited, so attempt 3 goes out at ~3 s (from
    // attempt 1). It reaches the real server, which revokes and clears the
    // cookies in its answer; that answer is held for 4.5 s, to land at ~7.5 s:
    // after the 7 s budget, before the attempt's own 5 s timeout (~8 s).
    await page.route('**/api/auth/refresh', async (route) => {
      const request = route.request()
      if (!isRevoke(request)) {
        await route.continue()
        return
      }
      attempts++
      if (attempts === 1) firstAttemptAt = Date.now()
      if (attempts <= 2) {
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
      await new Promise((resolve) => setTimeout(resolve, 4_500))
      await route.fulfill({ response }).catch(() => undefined)
      await upstream.dispose()
    })

    await logout(page)
    await expect(page).toHaveURL(/\/login/)
    // B signs in at once; the sign-in waits for the logout to let go.
    await signInHere(page, TEST_USERS.manager)

    // The pending attempt was aborted at the budget (~7 s after attempt 1),
    // before its answer could land and before the sign-in request went out.
    expect(attempts).toBe(3)
    expect(lastAttemptFailedAt).toBeGreaterThan(0)
    expect(lastAttemptFailedAt - firstAttemptAt).toBeGreaterThanOrEqual(6_800)
    expect(lastAttemptFailedAt - firstAttemptAt).toBeLessThan(7_450)
    expect(lastAttemptFailedAt).toBeLessThanOrEqual(loginSentAt)

    // After the moment the held answer would have landed, B's session is the
    // one in the jar and it works.
    await page.waitForTimeout(Math.max(0, firstAttemptAt + 12_000 - Date.now()))
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
    // The lock holder is a same-origin page that is not the app: an app tab
    // showing the session would leave it when it ends (a full navigation,
    // which lets go of the lock).
    const other = await context.newPage()
    await openNonAppPage(other)
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

  test('without Web Locks a page cannot sign in, says why, and leaves the cookies alone', async ({
    page,
    context,
  }) => {
    await loginAsAdmin(page)
    const before: Record<string, string | undefined> = {}
    for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
      before[name] = await cookieValue(context, name)
      expect(before[name]).toBeTruthy()
    }
    // The stored identity: who, and which organization. (The app may rewrite
    // the stored user in the background with a fuller profile of the same
    // user; that is not a change of identity.)
    const storedIdentity = () =>
      page.evaluate(() => {
        const user = JSON.parse(localStorage.getItem('user') ?? 'null')
        return {
          user: user && { id: user.id, email: user.email, organization_id: user.organization_id },
          organization: localStorage.getItem('selected_organization_id'),
        }
      })
    const stored = await storedIdentity()
    expect(stored.user).toBeTruthy()
    const other = await context.newPage()
    await other.addInitScript(withoutWebLocks)
    await other.addInitScript(countUnsupportedMessages, UNSUPPORTED)
    await other.goto('/login', { waitUntil: 'domcontentloaded' })
    expect(await other.evaluate(() => !!navigator.locks)).toBe(false)
    // It says why on arrival; each refused attempt says so once more.
    await expect(other.getByTestId('login-unsupported-notice')).toContainText(UNSUPPORTED)
    const onArrival = await unsupportedMessages(other)
    const issuing: string[] = []
    other.on('request', (request) => {
      if (/\/api\/auth\/(login|register|switch-org|refresh\/switch-org|sso\/[^/]+\/init)/.test(request.url())) issuing.push(request.url())
    })

    // A password sign-in as B is refused before anything is sent (or cleared).
    await submitSignIn(other, TEST_USERS.manager)
    await expect.poll(() => unsupportedMessages(other)).toBe(onArrival + 1)
    // So is an SSO start: the page never leaves for the provider.
    await offerGoogleSso(other)
    await startGoogleSso(other)
    await expect.poll(() => unsupportedMessages(other)).toBe(onArrival + 2)
    expect(other.url()).toContain('/login')
    // And a registration from an invitation.
    await other.goto('/register?invite=e2e-unsupported-browser', { waitUntil: 'domcontentloaded' })
    await other.locator('#fullName').fill('Unsupported Browser')
    await other.locator('#email').fill('unsupported-browser@e2e.test')
    await other.locator('#password').fill('a-long-password-1')
    await other.locator('#confirmPassword').fill('a-long-password-1')
    await other.locator('form button[type="submit"]').click()
    await expect.poll(() => unsupportedMessages(other)).toBe(1)
    expect(issuing).toEqual([])

    // The stored identity is untouched, and the first tab still shows A.
    expect(await storedIdentity()).toEqual(stored)
    await page.reload({ waitUntil: 'domcontentloaded' })
    expect(page.url()).not.toContain('/login')
    expect(JSON.parse((await page.evaluate(() => localStorage.getItem('user')))!).email).toBe(TEST_USERS.admin.email)

    // The browser's session is untouched: the same cookies, still A.
    for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
      expect(await cookieValue(context, name)).toBe(before[name])
    }
    const me = await page.request.get('/api/me')
    expect(me.status()).toBe(200)
    expect((await me.json()).data.email).toBe(TEST_USERS.admin.email)
  })
})


// Bounded, read-only diagnostics for the inherited cross-tab logout oracle.
// Keep the original test body intact: this observes request lifecycle only.
const AUTH_LIFECYCLE_TITLE =
  "another tab's refresh in flight is aborted by the logout, and a new sign-in survives"
type AuthLifecycleEvent = Record<string, string | number>
type AuthLifecycleState = {
  events: AuthLifecycleEvent[]
  overflow: boolean
  errors: string[]
  detach: Array<() => void>
  add: (event: AuthLifecycleEvent) => void
}
const authLifecycleStates = new WeakMap<import('@playwright/test').TestInfo, AuthLifecycleState>()

test.beforeEach(async ({ context }, testInfo) => {
  if (testInfo.title !== AUTH_LIFECYCLE_TITLE) return
  const started = performance.now()
  let eventBytes = 0
  const state: AuthLifecycleState = {
    events: [],
    overflow: false,
    errors: [],
    detach: [],
    add(event) {
      const row = { at_ms: Math.round(performance.now() - started), ...event }
      const bytes = Buffer.byteLength(JSON.stringify(row), 'utf8') + 1
      if (state.events.length >= 256 || eventBytes + bytes > 48 * 1024) {
        state.overflow = true
        return
      }
      eventBytes += bytes
      state.events.push(row)
    },
  }
  authLifecycleStates.set(testInfo, state)
  const tabs = new Map<Page, number>()
  const requests = new WeakMap<import('@playwright/test').Request, { id: number; tab: number; method: string; path: string; kind: string }>()
  let nextRequest = 0
  const error = (code: string) => {
    if (state.errors.length < 16) state.errors.push(code)
    else state.overflow = true
  }
  const navigation = (url: string) => {
    try {
      const path = new URL(url).pathname
      return ['/', '/login', '/profile', '/__lane_a_blank'].includes(path) ? path : 'other'
    } catch {
      return 'unparsed'
    }
  }
  const registerPage = (tab: Page) => {
    if (tabs.has(tab)) return
    const id = tabs.size + 1
    tabs.set(tab, id)
    state.add({ action: 'page-open', tab: id, navigation: navigation(tab.url()) })
    const moved = (frame: import('@playwright/test').Frame) => {
      if (frame === tab.mainFrame()) state.add({ action: 'navigation', tab: id, navigation: navigation(frame.url()) })
    }
    const closed = () => state.add({ action: 'page-close', tab: id })
    const crashed = () => state.add({ action: 'page-crash', tab: id })
    tab.on('framenavigated', moved)
    tab.on('close', closed)
    tab.on('crash', crashed)
    state.detach.push(() => {
      tab.off('framenavigated', moved)
      tab.off('close', closed)
      tab.off('crash', crashed)
    })
  }
  const sent = (request: import('@playwright/test').Request) => {
    try {
      const path = new URL(request.url()).pathname
      if (!['/api/auth/refresh', '/api/auth/logout', '/api/auth/login', '/api/me'].includes(path)) return
      const tab = request.frame().page()
      registerPage(tab)
      const kind = path === '/api/auth/refresh'
        ? ((request.postData() ?? '').includes('"revoke"') ? 'revoke' : 'refresh')
        : path === '/api/auth/logout' ? 'logout' : path === '/api/auth/login' ? 'login' : 'me'
      const row = { id: ++nextRequest, tab: tabs.get(tab)!, method: request.method(), path, kind }
      requests.set(request, row)
      state.add({ action: 'request-start', ...row })
    } catch {
      error('request-observer-error')
    }
  }
  const response = (answer: import('@playwright/test').Response) => {
    const row = requests.get(answer.request())
    if (row) state.add({ action: 'response', ...row, status: answer.status() })
  }
  const finished = (request: import('@playwright/test').Request) => {
    const row = requests.get(request)
    if (row) state.add({ action: 'request-finished', ...row })
  }
  const failed = (request: import('@playwright/test').Request) => {
    const row = requests.get(request)
    if (!row) return
    const raw = request.failure()?.errorText ?? ''
    const failure = /^net::ERR_[A-Z_]+$/.test(raw) ? raw : 'other-network-error'
    state.add({ action: 'request-failed', ...row, failure })
  }
  context.on('page', registerPage)
  context.on('request', sent)
  context.on('response', response)
  context.on('requestfinished', finished)
  context.on('requestfailed', failed)
  state.detach.push(() => {
    context.off('page', registerPage)
    context.off('request', sent)
    context.off('response', response)
    context.off('requestfinished', finished)
    context.off('requestfailed', failed)
  })
  for (const tab of context.pages()) registerPage(tab)
})

test.afterEach(async ({ context }, testInfo) => {
  if (testInfo.title !== AUTH_LIFECYCLE_TITLE) return
  const state = authLifecycleStates.get(testInfo)
  if (!state) {
    await testInfo.attach('auth-cross-tab-lifecycle', {
      contentType: 'application/json',
      body: Buffer.from(JSON.stringify({
        schema_version: 1, test_title: testInfo.title, events: [],
        overflow: false, errors: ['missing-beforeEach-capture'],
      })),
    })
    return
  }
  for (const detach of state.detach) detach()
  state.add({ action: 'test-end', status: testInfo.status ?? 'unknown', open_tabs: context.pages().length })
  authLifecycleStates.delete(testInfo)
  // Event byte/count caps above keep this below the runner's 64-KiB cap.
  await testInfo.attach('auth-cross-tab-lifecycle', {
    contentType: 'application/json',
    body: Buffer.from(JSON.stringify({
      schema_version: 1, test_title: testInfo.title,
      events: state.events, overflow: state.overflow, errors: state.errors,
    })),
  })
})

// Deterministic controls for transport attempts versus acknowledged logout.
// The original case and its request-count assertion above remain unchanged.
type RecoveryEvent = Record<string, string | number | boolean>
function recoveryLedger(context: BrowserContext) {
  const events: RecoveryEvent[] = []
  const errors: string[] = []
  const detach: Array<() => void> = []
  const requests = new Map<import('@playwright/test').Request, number>()
  const tabs = new Map<Page, number>()
  const start = performance.now()
  let bytes = 0
  let overflow = false
  const add = (row: RecoveryEvent) => {
    const event = { at_ms: Math.round(performance.now() - start), ...row }
    const size = Buffer.byteLength(JSON.stringify(event)) + 1
    if (events.length >= 256 || bytes + size > 48 * 1024) { overflow = true; return }
    bytes += size
    events.push(event)
  }
  const register = (page: Page) => {
    if (tabs.has(page)) return
    const tab = tabs.size + 1
    tabs.set(page, tab)
    const loaded = () => add({ action: 'document-ready', tab })
    const closed = () => add({ action: 'page-close', tab })
    page.on('domcontentloaded', loaded)
    page.on('close', closed)
    detach.push(() => { page.off('domcontentloaded', loaded); page.off('close', closed) })
  }
  const sent = (request: import('@playwright/test').Request) => {
    try {
      const path = new URL(request.url()).pathname
      if (!['/api/auth/refresh', '/api/auth/logout', '/api/auth/login', '/api/me'].includes(path)) return
      const page = request.frame().page()
      register(page)
      if (requests.size >= 256) { overflow = true; return }
      const id = requests.size + 1
      requests.set(request, id)
      const kind = path.endsWith('/refresh') ? ((request.postData() ?? '').includes('"revoke"') ? 'revoke' : 'refresh')
        : path.endsWith('/logout') ? 'logout' : path.endsWith('/login') ? 'login' : 'me'
      add({ action: 'request-start', id, tab: tabs.get(page)!, kind })
    } catch {
      if (errors.length < 16) errors.push('request-observer')
      else overflow = true
    }
  }
  const response = (value: import('@playwright/test').Response) => {
    const id = requests.get(value.request())
    if (id !== undefined) add({ action: 'response', id, status: value.status() })
  }
  const finished = (request: import('@playwright/test').Request) => {
    const id = requests.get(request)
    if (id !== undefined) add({ action: 'request-finished', id })
  }
  const failed = (request: import('@playwright/test').Request) => {
    const id = requests.get(request)
    if (id === undefined) return
    const value = request.failure()?.errorText ?? ''
    add({ action: 'request-failed', id, failure: /^net::ERR_[A-Z_]+$/.test(value) ? value : 'other-network-error' })
  }
  context.on('page', register)
  context.on('request', sent)
  context.on('response', response)
  context.on('requestfinished', finished)
  context.on('requestfailed', failed)
  detach.push(() => {
    context.off('page', register); context.off('request', sent); context.off('response', response)
    context.off('requestfinished', finished); context.off('requestfailed', failed)
  })
  for (const page of context.pages()) register(page)
  return {
    add, events, errors,
    id: (request: import('@playwright/test').Request) => requests.get(request),
    finish: () => { for (const fn of detach) fn(); return { events, errors, overflow } },
  }
}

// One real response, isolated upstream cookie jar, explicit gate, finite fetch.
// No response headers or cookie values enter the attachment.
async function holdRecoveryResponse(
  target: Page | BrowserContext,
  path: '/auth/refresh' | '/auth/logout',
  newContext: () => Promise<APIRequestContext>,
  note: (event: RecoveryEvent) => void,
) {
  type Request = import('@playwright/test').Request
  type Route = import('@playwright/test').Route
  let request: Request | undefined
  let cookies: Record<string, string> = {}
  let releaseGate!: (action: 'deliver' | 'abort') => void
  const gate = new Promise<'deliver' | 'abort'>((resolve) => { releaseGate = resolve })
  let readyResolve!: () => void
  let readyReject!: (error: Error) => void
  const ready = new Promise<void>((resolve, reject) => { readyResolve = resolve; readyReject = reject })
  // The readiness rejection is also awaited by the body; suppress only an
  // unhandled-promise warning if an earlier assertion has already failed.
  void ready.catch(() => undefined)
  let doneResolve!: () => void
  const done = new Promise<void>((resolve) => { doneResolve = resolve })
  const cleanupErrors: string[] = []
  const pattern = `**/api${path}`
  const handler = async (route: Route) => {
    const incoming = route.request()
    if (request || (path === '/auth/refresh' && (incoming.postData() ?? '').includes('"revoke"'))) {
      await route.continue()
      return
    }
    request = incoming
    let upstream: APIRequestContext | undefined
    try {
      upstream = await newContext()
      const headers = await incoming.allHeaders()
      delete headers['content-length']
      delete headers.host
      const response = await upstream.fetch(incoming.url(), {
        method: incoming.method(), headers, data: incoming.postDataBuffer() ?? undefined,
        maxRedirects: 0, timeout: 10_000,
      })
      if ((await response.body()).byteLength > 16 * 1024) throw new Error('upstream-response-cap')
      if (response.status() !== 200) throw new Error('upstream-not-200')
      cookies = setCookies(response.headersArray())
      note({ action: 'upstream-ready', path, status: response.status() })
      readyResolve()
      const action = await gate
      note({ action: 'release-response', path, delivery: action })
      try {
        if (action === 'deliver') await route.fulfill({ response })
        else await route.abort('aborted')
      } catch {
        // Page navigation may already have cancelled this route. The test
        // must prove the browser requestfailed event separately.
        note({ action: 'route-already-ended', path })
      }
    } catch {
      cleanupErrors.push('held-upstream-failed')
      readyReject(new Error('held-upstream-failed'))
      await route.abort('failed').catch(() => undefined)
    } finally {
      if (upstream) {
        try { await upstream.dispose() } catch { cleanupErrors.push('upstream-dispose') }
      }
      doneResolve()
    }
  }
  await target.route(pattern, handler)
  return {
    ready, done, cookies: () => cookies,
    request: () => { if (!request) throw new Error('held-request-missing'); return request },
    release: (action: 'deliver' | 'abort') => releaseGate(action),
    cleanup: async () => {
      releaseGate('abort')
      await target.unroute(pattern, handler)
      if (request) await done
      return cleanupErrors
    },
  }
}

test.describe('Logout recovery response controls', () => {
  test.describe.configure({ timeout: 120_000 })

  for (const arm of ['acknowledged', 'interrupted'] as const) {
    test(`logout recovery control: ${arm} first clearing response preserves a new sign-in`, async ({
      page, context, playwright, baseURL,
    }, testInfo) => {
      const ledger = recoveryLedger(context)
      const checks: Record<string, boolean> = {}
      const cleanupErrors: string[] = []
      let other: Page | undefined
      let heldRefresh: Awaited<ReturnType<typeof holdRecoveryResponse>> | undefined
      let heldLogout: Awaited<ReturnType<typeof holdRecoveryResponse>> | undefined
      const newContext = () => playwright.request.newContext({ baseURL })
      const pendingPresent = (tab: Page) => tab.evaluate(() => localStorage.getItem('whm_logout_pending') !== null)
      try {
        await loginAsAdmin(page)
        const originalRefresh = await cookieValue(context, 'whm_refresh')
        expect(Boolean(originalRefresh)).toBe(true)
        other = await context.newPage()
        await other.goto('/', { waitUntil: 'domcontentloaded' })
        const order: string[] = []
        recordSessionRequests(context, order)
        await openNonAppPage(page)
        heldRefresh = await holdRecoveryResponse(other, '/auth/refresh', newContext, ledger.add)
        await startProactiveRefresh(other)
        await heldRefresh.ready
        await other.evaluate(() => localStorage.setItem('whm_auth_expires_at', String(Date.now() + 60 * 60_000)))
        await page.goto('/', { waitUntil: 'domcontentloaded' })
        expect((await cookieValue(context, 'whm_refresh')) === originalRefresh).toBe(true)
        expect(Boolean(heldRefresh.cookies().whm_refresh)).toBe(true)
        heldLogout = await holdRecoveryResponse(context, '/auth/logout', newContext, ledger.add)
        await logout(page)
        await heldLogout.ready
        const firstRequest = heldLogout.request()
        const firstId = ledger.id(firstRequest)!
        expect(typeof firstId).toBe('number')
        const owner = firstRequest.frame().page()
        const refreshId = ledger.id(heldRefresh.request())!
        await expect.poll(() => ledger.events.some(e => e.action === 'request-failed' && e.id === refreshId), { timeout: 5_000 }).toBe(true)
        for (const name of ['whm_access', 'whm_refresh', 'whm_csrf']) {
          expect(Boolean(await cookieValue(context, name))).toBe(false)
        }
        expect(await pendingPresent(owner)).toBe(true)
        checks.real_revoke_cleared_cookies_pending_unacknowledged = true
        ledger.add({ action: 'pending-observation', present: true, phase: 'before-control' })

        if (arm === 'interrupted') {
          // Server has answered upstream, but no logout response is delivered
          // to the originating document before a deliberate full navigation.
          const failed = owner.waitForEvent('requestfailed', {
            predicate: request => request === firstRequest, timeout: 10_000,
          })
          await owner.goto('/login', { waitUntil: 'domcontentloaded' })
          await failed
          heldLogout.release('abort')
          await heldLogout.done
          expect(ledger.events.some(e => e.action === 'response' && e.id === firstId)).toBe(false)
          await expect.poll(() => order.filter(k => k === 'logout').length, { timeout: 10_000 }).toBe(2)
          const logoutIds = ledger.events.filter(e => e.action === 'request-start' && e.kind === 'logout').map(e => e.id)
          expect(logoutIds.length).toBe(2)
          const recoveryId = logoutIds[1]!
          await expect.poll(() => ledger.events.some(e => e.action === 'response' && e.id === recoveryId && e.status === 200), { timeout: 10_000 }).toBe(true)
          await expect.poll(() => ledger.events.some(e => e.action === 'request-finished' && e.id === recoveryId), { timeout: 10_000 }).toBe(true)
          checks.first_browser_response_interrupted = true
          checks.exact_one_recovery_completed = true
        } else {
          heldLogout.release('deliver')
          await heldLogout.done
          await expect.poll(() => ledger.events.some(e => e.action === 'response' && e.id === firstId && e.status === 200), { timeout: 10_000 }).toBe(true)
          await expect.poll(() => ledger.events.some(e => e.action === 'request-finished' && e.id === firstId), { timeout: 10_000 }).toBe(true)
          await expect.poll(() => pendingPresent(owner), { timeout: 10_000 }).toBe(false)
          ledger.add({ action: 'pending-observation', present: false, phase: 'acknowledged-before-navigation' })
          checks.acknowledged_before_navigation = true
          await owner.goto('/login', { waitUntil: 'domcontentloaded' })
        }
        await expect.poll(() => pendingPresent(page), { timeout: 10_000 }).toBe(false)
        ledger.add({ action: 'pending-observation', present: false, phase: 'before-B' })
        const expectedAttempts = arm === 'interrupted' ? ['revoke', 'logout', 'logout'] : ['revoke', 'logout']
        expect(order.filter(k => k === 'revoke' || k === 'logout')).toEqual(expectedAttempts)
        const preBEvents = ledger.events.length
        await signInHere(page, TEST_USERS.manager)
        const sessionB = await cookieValue(context, 'whm_refresh')
        heldRefresh.release('deliver')
        await heldRefresh.done
        await other.waitForTimeout(1_000)
        expect(Boolean(sessionB)).toBe(true)
        expect(sessionB !== originalRefresh).toBe(true)
        expect(sessionB !== heldRefresh.cookies().whm_refresh).toBe(true)
        expect((await cookieValue(context, 'whm_refresh')) === sessionB).toBe(true)
        const me = await page.request.get('/api/me')
        expect(me.status()).toBe(200)
        expect((await me.json()).data.email === TEST_USERS.manager.email).toBe(true)
        await expectRefreshRejected(newContext, [originalRefresh, heldRefresh.cookies().whm_refresh])
        expect(order.filter(k => k === 'revoke' || k === 'logout')).toEqual(expectedAttempts)
        const clearIds = ledger.events.filter(e => e.action === 'request-start' && (e.kind === 'logout' || e.kind === 'revoke')).map(e => e.id)
        expect(ledger.events.slice(preBEvents).some(e => clearIds.includes(e.id) && ['request-start', 'response', 'request-finished'].includes(String(e.action)))).toBe(false)
        checks.no_A_clearing_after_B_attempt = true
        checks.B_cookie_identity_and_me_preserved = true
        checks.both_old_refresh_tokens_rejected = true
        checks.exact_revoke_and_clearing_attempts = true
      } finally {
        for (const [name, held] of [['logout', heldLogout], ['refresh', heldRefresh]] as const) {
          if (held) {
            try { cleanupErrors.push(...await held.cleanup()) } catch { cleanupErrors.push(`${name}-cleanup`) }
          }
        }
        if (other && !other.isClosed()) {
          try { await other.close() } catch { cleanupErrors.push('other-page-close') }
        }
        ledger.add({ action: 'test-end', status: testInfo.status ?? 'unknown' })
        const captured = ledger.finish()
        await testInfo.attach('auth-logout-recovery-control', {
          contentType: 'application/json',
          body: Buffer.from(JSON.stringify({
            schema_version: 1, test_title: testInfo.title, arm, ...captured, checks, cleanup_errors: cleanupErrors,
          })),
        })
        expect(captured.overflow).toBe(false)
        expect(captured.errors).toEqual([])
        expect(cleanupErrors).toEqual([])
      }
    })
  }
})

// A clearing episode is acknowledged by removal of the pending boolean,
// not merely by receiving a 200. Only this original cross-tab case uses
// these read-only witnesses; the paired controls above remain unchanged.
type EpisodeEvent = {
  seq: number
  at_ms: number
  action: string
  tab?: number
  epoch?: number
  pending?: boolean
  id?: number
  kind?: 'revoke' | 'logout'
  status?: number
  failure?: string
}
const logoutEpisodes = new WeakMap<BrowserContext, Awaited<ReturnType<typeof observeLogoutEpisode>>>()

async function observeLogoutEpisode(context: BrowserContext, baseURL: string | undefined) {
  const origin = new URL(baseURL!).origin
  const started = Date.now()
  const events: EpisodeEvent[] = []
  const errors: string[] = []
  const pages = new Map<Page, number>()
  const epochs = new Map<Page, number>()
  const requests = new Map<import('@playwright/test').Request, EpisodeEvent>()
  const detach: Array<() => void> = []
  let bytes = 0
  let overflow = false
  let closed = false
  let beforeB: number | undefined
  let logoutIntent: number | undefined
  const error = (code: string) => {
    if (errors.length < 16) errors.push(code)
    else overflow = true
  }
  const add = (event: Omit<EpisodeEvent, 'seq' | 'at_ms'>) => {
    const value = { seq: events.length, at_ms: Date.now() - started, ...event }
    const size = Buffer.byteLength(JSON.stringify(value))
    if (closed) return
    if (events.length >= 256 || bytes + size > 48 * 1024) { overflow = true; return }
    bytes += size
    events.push(value)
  }
  const register = (page: Page) => {
    if (pages.has(page)) return
    if (pages.size >= 8) { overflow = true; return }
    pages.set(page, pages.size + 1)
    const close = () => add({ action: 'page-close', tab: pages.get(page) })
    page.on('close', close)
    detach.push(() => page.off('close', close))
  }
  context.on('page', register)
  detach.push(() => context.off('page', register))
  for (const page of context.pages()) register(page)
  await context.exposeBinding('__r3LogoutEpisodeWitness', ({ page, frame }, value: unknown) => {
    if (closed || frame !== page.mainFrame()) return
    if (!value || typeof value !== 'object') { error('witness-shape'); return }
    const row = value as Record<string, unknown>
    if (!['document', 'storage'].includes(String(row.action)) ||
        typeof row.epoch !== 'number' || !Number.isFinite(row.epoch) ||
        typeof row.pending !== 'boolean') { error('witness-fields'); return }
    register(page)
    if (row.action === 'document') {
      // Only a newer document may advance ownership. An equal callback is
      // redundant, and an older one cannot overwrite the current witness.
      const currentEpoch = epochs.get(page)
      if (currentEpoch !== undefined && row.epoch <= currentEpoch) return
      epochs.set(page, row.epoch)
    }
    // A delayed message from an old document cannot establish ownership.
    if (epochs.get(page) !== row.epoch) return
    add({ action: row.action === 'document' ? 'document-pending' : 'storage-pending',
      tab: pages.get(page), epoch: row.epoch, pending: row.pending })
  })
  await context.addInitScript(({ expectedOrigin }) => {
    if (window !== window.top || location.origin !== expectedOrigin) return
    const witness = (action: 'document' | 'storage') => {
      const target = window as unknown as {
        __r3LogoutEpisodeWitness: (value: { action: string; epoch: number; pending: boolean }) => Promise<void>
      }
      void target.__r3LogoutEpisodeWitness({
        action, epoch: performance.timeOrigin,
        pending: localStorage.getItem('whm_logout_pending') !== null,
      })
    }
    witness('document')
    window.addEventListener('storage', (event) => {
      if (event.key === 'whm_logout_pending') witness('storage')
    })
  }, { expectedOrigin: origin })
  const sent = (request: import('@playwright/test').Request) => {
    let page: Page
    try { page = request.frame().page() } catch { return }
    register(page)
    if (request.isNavigationRequest() && request.frame() === page.mainFrame()) {
      add({ action: 'document-navigation', tab: pages.get(page), epoch: epochs.get(page) })
    }
    const url = new URL(request.url())
    if (url.origin !== origin || request.method() !== 'POST') return
    let kind: 'revoke' | 'logout'
    if (url.pathname === '/api/auth/logout') kind = 'logout'
    else if (url.pathname === '/api/auth/refresh') {
      try { if (request.postDataJSON()?.revoke !== true) return } catch { return }
      kind = 'revoke'
    } else return
    if (requests.size >= 16) { overflow = true; return }
    const entry: EpisodeEvent = { seq: events.length, at_ms: Date.now() - started,
      action: 'request-start', id: requests.size + 1, kind,
      tab: pages.get(page), epoch: epochs.get(page) }
    requests.set(request, entry)
    add(entry)
  }
  const response = (response: import('@playwright/test').Response) => {
    const entry = requests.get(response.request())
    if (entry) add({ action: 'response', id: entry.id, status: response.status() })
  }
  const finished = (request: import('@playwright/test').Request) => {
    const entry = requests.get(request)
    if (entry) add({ action: 'request-finished', id: entry.id })
  }
  const failed = (request: import('@playwright/test').Request) => {
    const entry = requests.get(request)
    if (entry) {
      const failure = request.failure()?.errorText ?? ''
      add({ action: 'request-failed', id: entry.id,
        failure: /^net::ERR_[A-Z_]+$/.test(failure) ? failure : 'other-network-error' })
    }
  }
  context.on('request', sent); context.on('response', response)
  context.on('requestfinished', finished); context.on('requestfailed', failed)
  detach.push(() => {
    context.off('request', sent); context.off('response', response)
    context.off('requestfinished', finished); context.off('requestfailed', failed)
  })
  const starts = (kind: 'revoke' | 'logout') => events.filter(e => e.action === 'request-start' && e.kind === kind)
  const outcome = (id: number | undefined, action: string) => events.find(e => e.id === id && e.action === action)
  const validate = () => {
    expect(overflow).toBe(false)
    expect(errors).toEqual([])
    expect(logoutIntent).toBeDefined()
    const revokes = starts('revoke')
    const logouts = starts('logout')
    expect(revokes).toHaveLength(1)
    expect(logouts.length === 1 || logouts.length === 2).toBe(true)
    const revoke = revokes[0]!
    expect(revoke.seq).toBeGreaterThan(logoutIntent!)
    expect(outcome(revoke.id, 'response')?.status).toBe(200)
    expect(outcome(revoke.id, 'request-finished')).toBeDefined()
    expect(outcome(revoke.id, 'response')!.seq).toBeLessThan(logouts[0]!.seq)
    const final = logouts.at(-1)!
    expect(typeof final.epoch).toBe('number')
    expect(outcome(final.id, 'response')?.status).toBe(200)
    expect(outcome(final.id, 'request-finished')).toBeDefined()
    expect(outcome(final.id, 'request-failed')).toBeUndefined()
    if (logouts.length === 2) {
      const first = logouts[0]!
      const retry = logouts[1]!
      expect(typeof first.epoch).toBe('number')
      expect(typeof retry.epoch).toBe('number')
      // The recovering document can belong to another tab already alive.
      const pendingWitness = events.filter(e => ['document-pending', 'storage-pending'].includes(e.action) &&
        e.tab === retry.tab && e.epoch === retry.epoch && e.seq > logoutIntent! && e.seq < retry.seq).at(-1)
      expect(pendingWitness?.pending).toBe(true)
      const interrupted = outcome(first.id, 'request-failed')
      const replied = outcome(first.id, 'response')
      const replaced = events.some(e => e.action === 'document-navigation' && e.tab === first.tab &&
        e.epoch === first.epoch && e.seq > first.seq && e.seq < retry.seq)
      expect(Boolean(interrupted && interrupted.seq < retry.seq) ||
        Boolean(replied?.status === 200 && replied.seq < retry.seq && replaced &&
          pendingWitness?.action === 'document-pending' && pendingWitness.seq > replied.seq)).toBe(true)
      // An explicit acknowledgment anywhere in the shared context forbids
      // another clearing attempt. Missing causal witnesses never pass.
      expect(events.some(e => ['document-pending', 'storage-pending', 'settlement-pending'].includes(e.action) &&
        e.pending === false && e.seq > first.seq && e.seq < retry.seq)).toBe(false)
      expect(events.some(e => e.id === first.id && ['response', 'request-finished'].includes(e.action) && e.seq > retry.seq)).toBe(false)
    }
  }
  const api = {
    markLogoutIntent() { logoutIntent = events.length; add({ action: 'logout-intent' }) },
    async settled(page: Page) {
      await expect.poll(async () => {
        try {
          const state = await page.evaluate(() => ({
            epoch: performance.timeOrigin, pending: localStorage.getItem('whm_logout_pending') !== null,
          }))
          add({ action: 'settlement-pending', tab: pages.get(page), ...state })
          const last = starts('logout').at(-1)
          return !state.pending && !!last && outcome(last.id, 'response')?.status === 200 &&
            !!outcome(last.id, 'request-finished')
        } catch (error) {
          if (error instanceof Error && /Execution context was destroyed, most likely because of a navigation/.test(error.message)) return false
          throw error
        }
      }, { timeout: 10_000 }).toBe(true)
      validate()
    },
    markBeforeB() { validate(); beforeB = events.length; add({ action: 'B-intent' }) },
    assertNoLateClear() {
      expect(beforeB).toBeDefined()
      expect(events.slice(beforeB!).some(e => ['request-start', 'response', 'request-finished'].includes(e.action))).toBe(false)
      validate()
    },
    finish() {
      closed = true
      for (const fn of detach) fn()
      return { schema_version: 1, events, errors: errors.slice(0, 16), overflow: overflow || errors.length > 16,
        before_b_observed: beforeB !== undefined }
    },
  }
  logoutEpisodes.set(context, api)
  return api
}

test.afterEach(async ({ context }, testInfo) => {
  const episode = logoutEpisodes.get(context)
  if (!episode) return
  logoutEpisodes.delete(context)
  const observation = episode.finish()
  await testInfo.attach('auth-clearing-episode', {
    contentType: 'application/json',
    body: Buffer.from(JSON.stringify({ test_title: testInfo.title, ...observation })),
  })
  expect(observation.overflow).toBe(false)
  expect(observation.errors).toEqual([])
})
