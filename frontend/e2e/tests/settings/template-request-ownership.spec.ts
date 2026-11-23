import { test, expect, type Page, type Route, type APIRequestContext } from '@playwright/test'
import { ApiHelper, logout } from '../../helpers'
import { createTestScope, createUserWithPermissions, loginAs, SUPER_ADMIN, type TestUserHandle } from '../../framework'

// Run only with the existing disposable e2e-tests.yml PG17/Redis setup.
// Its global setup cleans/seeds the database before these routes are installed.
// Intercepted template responses prove UI ownership, not provider or DB semantics.
const scope = createTestScope('template-request-ownership')
const permissions = [
  { resource: 'templates', action: 'read' },
  { resource: 'templates', action: 'sync' },
  { resource: 'templates', action: 'delete' },
  { resource: 'accounts', action: 'read' },
]
const accounts = [
  { id: 'ownership-account-a', name: 'Ownership account A', phone_id: 'ownership-phone-a' },
  { id: 'ownership-account-b', name: 'Ownership account B', phone_id: 'ownership-phone-b' },
]

function template(id: string, name: string) {
  return {
    id, name, display_name: name, whatsapp_account: accounts[0]!.name,
    meta_template_id: 'synthetic-only', language: 'en', category: 'UTILITY',
    status: 'DRAFT', header_type: 'NONE', header_content: '', body_content: 'Synthetic template',
    footer_content: '', buttons: [], sample_values: [],
    created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
  }
}
const alpha = template('ownership-template-a', 'Ownership template A')
const beta = template('ownership-template-b', 'Ownership template B')

type Reply = (route: Route) => Promise<void>
const listReply = (rows = [alpha, beta]): Reply => route =>
  route.fulfill({ json: { data: { templates: rows, total: rows.length } } })

function heldReply(json: unknown, status = 200, allowNavigationAbort = false) {
  let started = false
  let allowAbort = allowNavigationAbort
  let markSeen!: () => void
  let release!: () => void
  let markSettled!: () => void
  let outcome: 'pending' | 'delivered' | 'aborted' = 'pending'
  const seen = new Promise<void>(resolve => { markSeen = resolve })
  const gate = new Promise<void>(resolve => { release = resolve })
  const settled = new Promise<void>(resolve => { markSettled = resolve })
  const reply: Reply = async route => {
    expect(started, 'Each held fixture response is consumed once').toBe(false)
    started = true
    markSeen()
    await gate
    try {
      await route.fulfill({ status, json })
      outcome = 'delivered'
    } catch (error) {
      outcome = 'aborted'
      if (!allowAbort) throw error
    } finally {
      markSettled()
    }
  }
  const cleanup = async () => {
    allowAbort = true
    release()
    if (started) await settled
  }
  return { reply, seen, release, settled, cleanup, outcome: () => outcome }
}

async function installTemplates(page: Page) {
  const fixture = {
    accounts: ((route: Route) => route.fulfill({ json: { data: { accounts } } })) as Reply,
    list: listReply(),
    sync: null as Reply | null,
    remove: null as Reply | null,
    lists: [] as Array<{ account: string | null; org: string | undefined }>,
    syncs: [] as string[],
    deletes: [] as string[],
    unexpected: [] as string[],
  }
  // A single exhaustive route prevents accidental provider writes, including
  // an unplanned retry or a different template ID. Auth/org traffic stays real.
  await page.route(/\/api\/(?:templates|accounts)(?:\/[^?]*)?(?:\?.*)?$/, async route => {
    const request = route.request()
    const url = new URL(request.url())
    if (request.method() === 'GET' && url.pathname === '/api/accounts') {
      await fixture.accounts(route)
    } else if (request.method() === 'GET' && url.pathname === '/api/templates') {
      fixture.lists.push({
        account: url.searchParams.get('account'),
        org: request.headers()['x-organization-id'],
      })
      await fixture.list(route)
    } else if (request.method() === 'POST' && url.pathname === '/api/templates/sync') {
      const account = request.postDataJSON()?.whatsapp_account
      fixture.syncs.push(typeof account === 'string' ? account : 'INVALID')
      if (fixture.sync) return fixture.sync(route)
      fixture.unexpected.push('unplanned sync')
      await route.abort('blockedbyclient')
    } else if (request.method() === 'DELETE' && /^\/api\/templates\/[^/]+$/.test(url.pathname)) {
      fixture.deletes.push(url.pathname.split('/').at(-1)!)
      if (fixture.remove) return fixture.remove(route)
      fixture.unexpected.push('unplanned delete')
      await route.abort('blockedbyclient')
    } else {
      fixture.unexpected.push(request.method() + ' ' + url.pathname)
      await route.abort('blockedbyclient')
    }
  })
  return fixture
}
type TemplatesFixture = Awaited<ReturnType<typeof installTemplates>>

function syncButton(page: Page) {
  return page.locator('header').getByRole('button', { name: /^Sync (?:all accounts|from Meta)$/ })
}
function accountSelect(page: Page) {
  return page.locator('button[role="combobox"]:not([data-testid="organization-switcher"])').first()
}
async function selectAccount(page: Page, name: string) {
  await accountSelect(page).click()
  await page.getByRole('option', { name, exact: true }).click()
}
async function openTemplates(page: Page) {
  await page.goto('/templates', { waitUntil: 'domcontentloaded' })
  await expect(page.getByRole('heading', { name: 'Message Templates', exact: true })).toBeVisible()
}
async function openDelete(page: Page, name: string) {
  await page.locator('tbody tr').filter({ hasText: name }).getByRole('button', { name: 'Delete', exact: true }).click()
  const dialog = page.getByRole('alertdialog')
  await expect(dialog).toBeVisible()
  await expect(dialog).toContainText(name)
  return dialog
}
async function switchOrganization(page: Page, name: string, id: string) {
  await page.getByTestId('organization-switcher').click()
  const response = page.waitForResponse(r =>
    new URL(r.url()).pathname === '/api/auth/refresh/switch-org' && r.request().method() === 'POST')
  const reloaded = page.waitForEvent('domcontentloaded')
  await page.getByRole('option', { name, exact: true }).click()
  expect((await response).status()).toBe(200)
  await reloaded
  await expect(page.getByTestId('organization-switcher')).toContainText(name)
  const me = await page.request.get('/api/me')
  expect(me.ok()).toBe(true)
  expect((await me.json()).data.organization_id).toBe(id)
}

test.describe('Message Templates - Request ownership', () => {
  test.describe.configure({ mode: 'serial', retries: 0, timeout: 30_000 })

  let setupRequest: APIRequestContext
  let api: ApiHelper
  let user: TestUserHandle
  let firstOrg: { id: string; name: string }
  let secondOrg: { id: string; name: string } | undefined
  let secondRole: { id: string } | undefined
  let memberAdded = false
  let fixture: TemplatesFixture
  let pending: Array<ReturnType<typeof heldReply>>

  test.beforeAll(async ({ playwright }) => {
    // This is an additional fixture boundary, not a replacement for global
    // setup's disposable-database requirement.
    expect(process.env.CI, 'Use the existing disposable CI job').toBeTruthy()
    expect(new URL(process.env.BASE_URL!).hostname).toMatch(/^(localhost|127\.0\.0\.1)$/)
    expect(new URL(process.env.TEST_DATABASE_URL!).pathname).toBe('/whatomate_test')
    setupRequest = await playwright.request.newContext()
    api = new ApiHelper(setupRequest)
    await api.login(SUPER_ADMIN.email, SUPER_ADMIN.password)
    firstOrg = await api.getCurrentOrg()
    user = await createUserWithPermissions(api, scope, { permissions })
    secondOrg = await api.createOrganization(scope.name('second-org'))
    // Roles are scoped to the organization. Do not reuse A's role ID in B.
    const role = await api.post('/api/roles', {
      name: scope.name('second-role'), description: 'Template ownership browser fixture',
      permissions: await api.findPermissionKeys(permissions),
    }, { 'X-Organization-ID': secondOrg.id })
    expect(role.ok()).toBe(true)
    secondRole = (await role.json()).data
    await api.addOrgMember(user.user.id, secondRole!.id, secondOrg.id)
    memberAdded = true
  })

  test.afterAll(async () => {
    if (!setupRequest) return
    const failures: string[] = []
    // Keep cleanup finite and attempt every owned fixture even after a failure.
    const cleanup = async (name: string, action: () => Promise<void>) => {
      try { await action() } catch { failures.push(name) }
    }
    try {
      if (memberAdded) await cleanup('membership', () => api.removeOrgMember(user.user.id, secondOrg!.id))
      if (user) await cleanup('user', async () => { expect((await api.del('/api/users/' + user.user.id)).ok()).toBe(true) })
      if (secondRole) await cleanup('second role', async () => {
        expect((await api.del('/api/roles/' + secondRole!.id, { 'X-Organization-ID': secondOrg!.id })).ok()).toBe(true)
      })
      if (user) await cleanup('first role', async () => { expect((await api.del('/api/roles/' + user.role.id)).ok()).toBe(true) })
      if (secondOrg) await cleanup('second organization', async () => {
        expect((await api.del('/api/organizations/' + secondOrg!.id)).ok()).toBe(true)
      })
    } finally {
      await setupRequest.dispose()
    }
    expect(failures).toEqual([])
  })

  test.beforeEach(async ({ page }) => {
    pending = []
    fixture = undefined!
    await loginAs(page, user)
    // Only the template filter preference is cleared; auth/session/org state
    // is established by the real login and switcher paths.
    await page.evaluate(() => localStorage.removeItem('templates_selected_account'))
    fixture = await installTemplates(page)
  })

  test.afterEach(async ({ page }, testInfo) => {
    // Close the page before releasing leftovers so cleanup cannot send work
    // through a removed route. Normal scenario releases are asserted below.
    await page.close()
    for (const response of pending) await response.cleanup()
    if (fixture) {
      await testInfo.attach('template-request-summary', {
        body: JSON.stringify({ lists: fixture.lists, syncs: fixture.syncs, deletes: fixture.deletes, unexpected: fixture.unexpected }),
        contentType: 'application/json',
      })
      expect(fixture.unexpected).toEqual([])
    }
  })

  test('keeps pending accounts in loading state without an empty-results conclusion', async ({ page }) => {
    const accountsReply = heldReply({ data: { accounts: [] } })
    pending.push(accountsReply)
    fixture.accounts = accountsReply.reply
    fixture.list = listReply([])
    await openTemplates(page)
    await accountsReply.seen
    await expect(page.locator('tbody .skeleton-shimmer').first()).toBeVisible()
    await expect(page.getByText('No templates found', { exact: true })).toBeHidden()
    await expect(syncButton(page)).toBeDisabled()
    expect(fixture.lists).toHaveLength(0)

    accountsReply.release()
    await accountsReply.settled
    await expect(page.getByText('No templates found', { exact: true })).toBeVisible()
    await expect(page.locator('tbody .skeleton-shimmer')).toHaveCount(0)
    expect(fixture.syncs).toEqual([])
    expect(accountsReply.outcome()).toBe('delivered')
  })

  test('keeps one captured sync account after a filter change and shows its outcome', async ({ page }) => {
    const first = heldReply({ message: 'Synthetic sync refusal' }, 400)
    pending.push(first)
    fixture.sync = route => {
      expect(fixture.syncs).toEqual([accounts[0]!.name])
      return first.reply(route)
    }
    await openTemplates(page)
    await expect(syncButton(page)).toBeDisabled()
    expect(fixture.syncs).toEqual([])
    await selectAccount(page, accounts[0]!.name)
    await expect(syncButton(page)).toBeEnabled()
    await syncButton(page).click()
    await first.seen
    await expect(page.locator('p[role="status"]')).toContainText(accounts[0]!.name)
    await expect(syncButton(page)).toBeDisabled()
    expect(fixture.syncs).toEqual([accounts[0]!.name])

    await selectAccount(page, accounts[1]!.name)
    await expect(page.locator('tbody tr').filter({ hasText: alpha.display_name })).toBeVisible()
    const readsBeforeCompletion = fixture.lists.length
    first.release()
    await first.settled
    const results = page.locator('[aria-label="Template sync results"]')
    await expect(results.locator('p')).toHaveCount(1)
    await expect(results.locator('p').nth(0)).toHaveText(accounts[0]!.name + ': Synthetic sync refusal')
    await expect(syncButton(page)).toBeEnabled()
    await expect(page.locator('tbody .skeleton-shimmer')).toHaveCount(0)
    expect(fixture.lists).toHaveLength(readsBeforeCompletion + 1)
    expect(fixture.lists.at(-1)!.account).toBe(accounts[1]!.name)
    expect(fixture.syncs).toEqual([accounts[0]!.name])
    expect(first.outcome()).toBe('delivered')
  })

  test('ignores an older list response after selecting a different account', async ({ page }) => {
    const stale = heldReply({ data: { templates: [template('stale', 'Stale account A row')], total: 999 } })
    pending.push(stale)
    await openTemplates(page)
    await expect(page.locator('tbody tr').filter({ hasText: alpha.display_name })).toBeVisible()
    fixture.list = route => new URL(route.request().url()).searchParams.get('account') === accounts[0]!.name
      ? stale.reply(route) : listReply([beta])(route)
    await selectAccount(page, accounts[0]!.name)
    await stale.seen
    await selectAccount(page, accounts[1]!.name)
    await expect(page.locator('tbody tr').filter({ hasText: beta.display_name })).toBeVisible()
    stale.release()
    await stale.settled
    await expect(page.getByText('Stale account A row', { exact: true })).toHaveCount(0)
    await expect(page.locator('tbody tr').filter({ hasText: beta.display_name })).toBeVisible()
    await expect(page.locator('tbody .skeleton-shimmer')).toHaveCount(0)
    expect(fixture.syncs).toEqual([])
    expect(stale.outcome()).toBe('delivered')
  })

  test('uses actual A to B to A organization reloads without restoring an old list', async ({ page }) => {
    const stale = heldReply({ data: { templates: [template('old-org', 'Old workspace response')], total: 999 } }, 200, true)
    pending.push(stale)
    let holdNext = false
    fixture.list = route => {
      if (holdNext) { holdNext = false; return stale.reply(route) }
      const org = route.request().headers()['x-organization-id'] || firstOrg.id
      expect([firstOrg.id, secondOrg!.id]).toContain(org)
      return listReply([template('workspace-row', org === firstOrg.id ? 'Current workspace A' : 'Current workspace B')])(route)
    }
    await openTemplates(page)
    await expect(page.getByTestId('organization-switcher')).toContainText(firstOrg.name)
    await expect(page.locator('tbody tr').filter({ hasText: 'Current workspace A' })).toBeVisible()
    holdNext = true
    await selectAccount(page, accounts[0]!.name)
    await stale.seen

    await switchOrganization(page, secondOrg!.name, secondOrg!.id)
    await expect(page.locator('tbody tr').filter({ hasText: 'Current workspace B' })).toBeVisible()
    await switchOrganization(page, firstOrg.name, firstOrg.id)
    await expect(page.locator('tbody tr').filter({ hasText: 'Current workspace A' })).toBeVisible()
    stale.release()
    await stale.settled
    await expect(page.getByText('Old workspace response', { exact: true })).toHaveCount(0)
    await expect(page.getByText('Current workspace B', { exact: true })).toHaveCount(0)
    await expect(page.locator('tbody .skeleton-shimmer')).toHaveCount(0)
    expect(fixture.syncs).toEqual([])
    // A real reload can abort the held request; no same-component ABA claim.
    expect(['delivered', 'aborted']).toContain(stale.outcome())
  })

  test('ignores in-flight sync after real logout in a second tab', async ({ page, context }) => {
    const stale = heldReply({ message: 'Old session sync refusal' }, 400, true)
    pending.push(stale)
    fixture.sync = route => {
      expect(fixture.syncs).toEqual([accounts[0]!.name])
      return stale.reply(route)
    }
    await openTemplates(page)
    await selectAccount(page, accounts[0]!.name)
    await expect(syncButton(page)).toBeEnabled()
    await syncButton(page).click()
    await stale.seen
    const other = await context.newPage()
    try {
      await other.goto('/profile', { waitUntil: 'domcontentloaded' })
      await logout(other)
      await page.waitForURL(/\/login/)
      stale.release()
      await stale.settled
      expect(fixture.syncs).toEqual([accounts[0]!.name])

      await loginAs(page, user)
      await openTemplates(page)
      await selectAccount(page, accounts[0]!.name)
      await expect(syncButton(page)).toBeEnabled()
      await expect(page.locator('[aria-label="Template sync results"]')).toHaveCount(0)
      await expect(page.getByText('Old session sync refusal', { exact: true })).toHaveCount(0)
      expect(fixture.syncs).toEqual([accounts[0]!.name])
    } finally {
      await other.close()
    }
  })

  test('cancel sends no delete and an old pending delete cannot own the returned dialog', async ({ page }) => {
    const stale = heldReply({ data: {} }, 200, true)
    const current = heldReply({ data: {} })
    pending.push(stale, current)
    fixture.remove = route => {
      expect(fixture.deletes.length).toBeLessThanOrEqual(2)
      expect(fixture.deletes.at(-1)).toBe(fixture.deletes.length === 1 ? alpha.id : beta.id)
      return (fixture.deletes.length === 1 ? stale : current).reply(route)
    }
    await openTemplates(page)
    let dialog = await openDelete(page, alpha.display_name)
    await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
    await expect(dialog).toBeHidden()
    expect(fixture.deletes).toEqual([])

    dialog = await openDelete(page, alpha.display_name)
    await dialog.getByRole('button', { name: 'Delete', exact: true }).click()
    await stale.seen
    await expect(dialog.getByRole('button', { name: 'Cancel', exact: true })).toBeDisabled()
    await expect(dialog.getByRole('button', { name: 'Delete', exact: true })).toBeDisabled()

    await page.goto('/profile', { waitUntil: 'domcontentloaded' })
    await openTemplates(page)
    dialog = await openDelete(page, beta.display_name)
    await dialog.getByRole('button', { name: 'Delete', exact: true }).click()
    await current.seen
    const readsBeforeOldCompletion = fixture.lists.length
    stale.release()
    await stale.settled
    await expect(dialog).toContainText(beta.display_name)
    await expect(dialog.getByRole('button', { name: 'Cancel', exact: true })).toBeDisabled()
    await expect(dialog.getByRole('button', { name: 'Delete', exact: true })).toBeDisabled()
    expect(fixture.lists).toHaveLength(readsBeforeOldCompletion)
    expect(fixture.deletes).toEqual([alpha.id, beta.id])

    current.release()
    await current.settled
    await expect(dialog).toBeHidden()
    expect(current.outcome()).toBe('delivered')
  })

  test('a current delete success closes its dialog and refreshes exactly once', async ({ page }) => {
    fixture.remove = async route => {
      expect(fixture.deletes).toEqual([alpha.id])
      fixture.list = listReply([beta])
      await route.fulfill({ json: { data: {} } })
    }
    await openTemplates(page)
    const dialog = await openDelete(page, alpha.display_name)
    const readsBeforeDelete = fixture.lists.length
    await dialog.getByRole('button', { name: 'Delete', exact: true }).click()
    await expect(dialog).toBeHidden()
    await expect(page.locator('tbody tr').filter({ hasText: beta.display_name })).toBeVisible()
    await expect(page.locator('tbody tr').filter({ hasText: alpha.display_name })).toHaveCount(0)
    expect(fixture.lists).toHaveLength(readsBeforeDelete + 1)
    expect(fixture.deletes).toEqual([alpha.id])
  })

  test('a current delete refusal retains its dialog and stops loading without retry', async ({ page }) => {
    fixture.remove = async route => {
      expect(fixture.deletes).toEqual([alpha.id])
      await route.fulfill({ status: 400, json: { message: 'Synthetic delete refusal' } })
    }
    await openTemplates(page)
    const dialog = await openDelete(page, alpha.display_name)
    const readsBeforeDelete = fixture.lists.length
    await dialog.getByRole('button', { name: 'Delete', exact: true }).click()
    await expect(page.locator('[data-sonner-toast]').filter({ hasText: 'Synthetic delete refusal' })).toBeVisible()
    await expect(dialog).toContainText(alpha.display_name)
    await expect(dialog.getByRole('button', { name: 'Delete', exact: true })).toBeEnabled()
    await expect(dialog.getByRole('button', { name: 'Cancel', exact: true })).toBeEnabled()
    expect(fixture.lists).toHaveLength(readsBeforeDelete)
    expect(fixture.deletes).toEqual([alpha.id])
    await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
    await expect(dialog).toBeHidden()
  })
})
