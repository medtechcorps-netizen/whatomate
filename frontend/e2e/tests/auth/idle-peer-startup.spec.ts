import { expect, test, type BrowserContext, type Page } from '@playwright/test'

declare global {
  interface Window {
    idleFixtureActivityAt?: number
    resumeIdleFixtureSwitch: () => Promise<void>
  }
}

const minute = 60_000
const start = Date.UTC(2026, 0, 10, 12)
const activityAt = start + 20 * minute
const sessionId = 'synthetic-idle-session'
const protectedPath = '/settings/audit-logs'
const otherWorkspace = { id: '93333333-3333-4333-8333-333333333333', name: 'Second Idle Workspace' }
const user = {
  id: '92111111-1111-4111-8111-111111111111',
  organization_id: '92222222-2222-4222-8222-222222222222',
  email: 'idle-agent@example.test',
  full_name: 'Idle Test Agent',
  organization_name: 'Idle Test Workspace',
  role: { name: 'agent', permissions: [{ resource: 'audit_logs', action: 'read' }] },
}

test.use({ storageState: { cookies: [], origins: [] } })

async function openActiveTab(page: Page, context: BrowserContext) {
  // Seed the original stored clock only once. In particular, loading the cold
  // page or the login page must not restore a user that the app signed out.
  await page.route('**/__idle_setup', route => route.fulfill({ contentType: 'text/html', body: '<!doctype html>' }))
  await page.goto('/__idle_setup')
  await page.evaluate(({ start, user, sessionId, otherWorkspace }) => {
    document.cookie = 'whm_csrf=synthetic-idle-csrf; Path=/; SameSite=Lax'
    localStorage.setItem('idleFixtureNow', String(start))
    localStorage.setItem('user', JSON.stringify(user))
    localStorage.setItem('whm_session_id', sessionId)
    localStorage.setItem('whm_session_idle_minutes', JSON.stringify({
      [`${user.id}|${user.organization_id}`]: 30, [`${user.id}|${otherWorkspace.id}`]: 30,
    }))
    localStorage.setItem('whm_last_activity', String(start))
    localStorage.setItem('whm_auth_expires_at', String(start + 2 * 60 * 60_000))
    localStorage.setItem('locale', 'en')
  }, { start, user, sessionId, otherWorkspace })

  await context.addInitScript(() => {
    // Advance only the wall clock. The app checks the browser's actual reload
    // entry and uses monotonic time for the 500 ms reply window.
    const realNow = Date.now
    Date.now = () => Number(localStorage.getItem('idleFixtureNow')) || realNow()
    const write = Storage.prototype.setItem
    Storage.prototype.setItem = function (key, value) {
      if (this === localStorage && key === 'whm_last_activity') {
        throw new DOMException('Activity write failed', 'QuotaExceededError')
      }
      return write.call(this, key, value)
    }
  })

  await page.goto(protectedPath)
  const heading = page.getByRole('heading', { name: 'Audit Logs', exact: true })
  await expect(heading).toBeVisible()
  await page.evaluate(() => {
    const observer = new BroadcastChannel('whm-session')
    observer.onmessage = ({ data }) => {
      if (data?.type === 'activity') window.idleFixtureActivityAt = data.at
      if (data?.type === 'activity-reply') sessionStorage.setItem('idleFixtureDeliveredAt', String(data.at))
    }
    window.addEventListener('pagehide', () => observer.close(), { once: true })
  })
  await page.evaluate(now => localStorage.setItem('idleFixtureNow', String(now)), activityAt)
  await heading.click()
  await expect.poll(() => page.evaluate(() => window.idleFixtureActivityAt)).toBe(activityAt)
  expect(await page.evaluate(() => localStorage.getItem('whm_last_activity'))).toBe(String(start))
  await page.evaluate(now => localStorage.setItem('idleFixtureNow', String(now)), start + 31 * minute)
}

async function serveSession(context: BrowserContext, onSwitch?: () => Promise<void>) {
  const teardown: string[] = []
  let current = user
  await context.route('**/api/**', async route => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (path === '/api/auth/logout') teardown.push('logout')
    if (path === '/api/auth/refresh' && request.postDataJSON()?.revoke) {
      expect(request.headers()['x-csrf-token']).toBe('synthetic-idle-csrf')
      teardown.push('revoke')
    }
    if (onSwitch && path === '/api/me/organizations') {
      return route.fulfill({ json: { data: { organizations: [
        { organization_id: user.organization_id, name: user.organization_name },
        { organization_id: otherWorkspace.id, name: otherWorkspace.name },
      ] } } })
    }
    if (onSwitch && path === '/api/auth/refresh/switch-org') {
      expect(request.postDataJSON()?.organization_id).toBe(otherWorkspace.id)
      await onSwitch()
      current = { ...user, organization_id: otherWorkspace.id, organization_name: otherWorkspace.name }
      return route.fulfill({ json: { data: { user: current, expires_in: 7_200 } } })
    }
    if (path === '/api/audit-logs') {
      return route.fulfill({ json: { data: { audit_logs: [], total: 0 } } })
    }
    if (path === '/api/me') {
      return route.fulfill({ headers: { 'X-Session-Idle-Minutes': '30' }, json: { data: current } })
    }
    if (path === '/api/product/entitlements') {
      return route.fulfill({ json: { data: { mode: 'licensed', entitlements: {} } } })
    }
    return route.fulfill({ json: { data: {} } })
  })
  return teardown
}

for (const deliverReply of [true, false]) {
  const title = deliverReply
    ? 'an active tab preserves a cold page when the shared activity clock cannot be written'
    : 'a cold page signs out when the active tab’s clock reply is lost'
  test(title, async ({ page, context }) => {
    let coldReads = 0
    const teardown = await serveSession(context)

    await openActiveTab(page, context)
    if (!deliverReply) {
      await page.evaluate(() => {
        const send = BroadcastChannel.prototype.postMessage
        BroadcastChannel.prototype.postMessage = function (data) {
          if (this.name === 'whm-session' && data?.type === 'activity-reply') {
            sessionStorage.setItem('idleFixtureDroppedAt', String(data.at))
            return
          }
          return send.call(this, data)
        }
      })
    }
    const cold = await context.newPage()
    cold.on('request', request => {
      if (new URL(request.url()).pathname === '/api/audit-logs') coldReads++
    })
    await cold.goto(protectedPath)

    if (deliverReply) {
      const protectedHeading = cold.getByRole('heading', { name: 'Audit Logs', exact: true })
      const signIn = cold.getByRole('button', { name: 'Sign in', exact: true })
      await expect(protectedHeading.or(signIn)).toBeVisible()
      expect(await cold.evaluate(() => localStorage.getItem('whm_session_id'))).toBe(sessionId)
      await expect(protectedHeading).toBeVisible()
      await expect.poll(() => coldReads).toBeGreaterThan(0)
      await expect.poll(() => page.evaluate(() => sessionStorage.getItem('idleFixtureDeliveredAt'))).toBe(String(activityAt))
      expect(await cold.evaluate(() => localStorage.getItem('whm_last_activity'))).toBe(String(start))
      expect(teardown).toEqual([])
    } else {
      await expect(cold).toHaveURL(/\/login(?:\?|$)/)
      await expect.poll(() => teardown).toEqual(['revoke', 'logout'])
      expect(await page.evaluate(() => sessionStorage.getItem('idleFixtureDroppedAt'))).toBe(String(activityAt))
      expect(coldReads).toBe(0)
      expect(await cold.evaluate(() => localStorage.getItem('user'))).toBeNull()
    }
  })
}

test('switching workspace while a cold page waits preserves the active workspace after reload', async ({ page, context }) => {
  let resume!: () => void
  const held = new Promise<void>(done => { resume = done })
  const teardown = await serveSession(context, () => held)
  await openActiveTab(page, context)
  await page.evaluate(() => {
    // Keep the cold page waiting for the original scope until the real switch response arrives.
    const post = BroadcastChannel.prototype.postMessage
    BroadcastChannel.prototype.postMessage = function (data) {
      if (this.name === 'whm-session' && data?.type === 'activity-reply') return
      return post.call(this, data)
    }
  })

  const cold = await context.newPage()
  await cold.exposeFunction('resumeIdleFixtureSwitch', resume)
  await cold.addInitScript(() => {
    const post = BroadcastChannel.prototype.postMessage
    BroadcastChannel.prototype.postMessage = function (data) {
      post.call(this, data)
      if (this.name === 'whm-session' && data?.type === 'activity-query') {
        sessionStorage.setItem('idleFixtureLookupDocument', String(performance.timeOrigin))
        void window.resumeIdleFixtureSwitch()
      }
    }
  })

  try {
    await page.getByTestId('organization-switcher').click()
    await Promise.all([
      page.waitForRequest(request => new URL(request.url()).pathname === '/api/auth/refresh/switch-org'),
      page.getByRole('option', { name: otherWorkspace.name }).click(),
    ])
    await Promise.all([page.waitForEvent('domcontentloaded'), cold.goto(protectedPath)])
    await expect(cold).toHaveURL(/\/login(?:\?|$)/)
    await expect(page.getByTestId('organization-switcher')).toContainText(otherWorkspace.name)
    await expect(page.getByRole('heading', { name: 'Audit Logs', exact: true })).toBeVisible()

    const result = await cold.evaluate(() => ({
      stayedInDocument: sessionStorage.getItem('idleFixtureLookupDocument') === String(performance.timeOrigin),
      workspace: JSON.parse(localStorage.getItem('user') || 'null')?.organization_id,
      session: localStorage.getItem('whm_session_id'),
    }))
    expect(result).toEqual({ stayedInDocument: true, workspace: otherWorkspace.id, session: sessionId })
    expect(teardown).toEqual([])
  } finally {
    resume()
  }
})
