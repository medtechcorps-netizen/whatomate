import { test, expect } from '@playwright/test'
import { loginAsAdmin, logout } from '../../helpers'

// A logout must not race a token refresh that is already in flight. The
// refresh response sets new auth cookies; if it lands after /auth/logout
// cleared them, the browser is signed back in. The client therefore exits at
// once but sends the revoke and /auth/logout only after that response landed.
test.describe('Logout during a token refresh', () => {
  test('a refresh response that lands after the click cannot revive the session', async ({
    page,
    context,
    playwright,
    baseURL,
  }) => {
    await loginAsAdmin(page)
    const originalRefresh = (await context.cookies()).find((c) => c.name === 'whm_refresh')?.value
    expect(originalRefresh).toBeTruthy()

    const order: string[] = []
    let successorRefresh: string | undefined
    let releaseRefresh!: () => void
    const refreshGate = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    let refreshSeen!: () => void
    const refreshStarted = new Promise<void>((resolve) => {
      refreshSeen = resolve
    })

    await page.route('**/api/auth/refresh', async (route) => {
      if ((route.request().postData() ?? '').includes('"revoke"')) {
        order.push('revoke')
        await route.continue()
        return
      }
      refreshSeen()
      // The server rotates the token now; the browser sees the new cookies
      // only when the gate opens.
      const response = await route.fetch()
      successorRefresh ??= response
        .headersArray()
        .filter((header) => header.name.toLowerCase() === 'set-cookie')
        .map((header) => /^whm_refresh=([^;]+)/.exec(header.value)?.[1])
        .find(Boolean)
      await refreshGate
      order.push('refresh landed')
      await route.fulfill({ response })
    })
    await page.route('**/api/auth/logout', async (route) => {
      order.push('logout')
      await route.continue()
    })

    // Make the proactive refresh due and wake it.
    await page.evaluate(() => {
      localStorage.setItem('whm_auth_expires_at', String(Date.now()))
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await refreshStarted

    // The user is out at once, refresh or not.
    await logout(page)
    await expect(page).toHaveURL(/\/login/)
    // Give an unordered logout every chance to go out first.
    await page.waitForTimeout(1_000)
    expect(order).not.toContain('revoke')
    expect(order).not.toContain('logout')

    const loggedOut = page.waitForResponse('**/api/auth/logout')
    releaseRefresh()
    await loggedOut
    expect(order.indexOf('refresh landed')).toBeGreaterThanOrEqual(0)
    expect(order.indexOf('refresh landed')).toBeLessThan(order.indexOf('revoke'))
    expect(order.indexOf('revoke')).toBeLessThan(order.indexOf('logout'))

    const cookies = await context.cookies()
    for (const name of ['whm_access', 'whm_refresh']) {
      expect(cookies.find((c) => c.name === name && c.value !== '')).toBeUndefined()
    }
    expect((await page.request.get('/api/me')).status()).toBe(401)

    // Neither the original refresh token nor its successor mints a session.
    expect(successorRefresh).toBeTruthy()
    for (const token of [originalRefresh, successorRefresh]) {
      const outsider = await playwright.request.newContext({
        baseURL,
        extraHTTPHeaders: { Cookie: `whm_refresh=${token}` },
      })
      expect((await outsider.post('/api/auth/refresh')).status()).toBe(401)
      await outsider.dispose()
    }
  })
})
