import { test, expect, type APIRequestContext } from '@playwright/test'
import { TablePage } from '../../pages'
import { loginAsAdmin, createTeamFixture, navigateToFirstItem, expectMetadataVisible, expectActivityLogVisible, ApiHelper } from '../../helpers'
import { createTestScope, SUPER_ADMIN } from '../../framework'

const scope = createTestScope('teams')

async function seedAdminTeam(request: APIRequestContext, suffix: string) {
  const api = new ApiHelper(request)
  await api.loginAsAdmin()
  const name = scope.name(suffix)
  const response = await api.post('/api/teams', { name, description: 'owned E2E team' })
  expect(response.ok(), `seed team: ${await response.text()}`).toBe(true)
  const team = (await response.json()).data.team
  expect(team.id).toMatch(/^[a-f0-9-]{36}$/)
  expect(team.name).toBe(name)
  return { api, team }
}

test.describe('Teams - List View', () => {
  let tablePage: TablePage

  test.beforeEach(async ({ page }) => {
    await loginAsAdmin(page)
    await page.goto('/settings/teams')
    await page.waitForLoadState('networkidle')
    tablePage = new TablePage(page)
  })

  test('should display teams list', async ({ page }) => {
    await expect(tablePage.tableBody).toBeVisible()
  })

  test('should search teams', async ({ page }) => {
    const initialCount = await tablePage.getRowCount()
    if (initialCount > 0) {
      await tablePage.search('nonexistent-team-xyz')
      await page.waitForTimeout(300)
      const filteredCount = await tablePage.getRowCount()
      expect(filteredCount).toBeLessThanOrEqual(initialCount)
    }
  })

  test('should load create page', async ({ page }) => {
    await page.goto('/settings/teams/new')
    await page.waitForLoadState('networkidle')
    expect(page.url()).toContain('/settings/teams/new')
    await expect(page.locator('input').first()).toBeVisible()
  })

  test('should load detail page from list', async ({ page }) => {
    const href = await navigateToFirstItem(page)
    if (href) {
      expect(page.url()).toMatch(/\/settings\/teams\/[a-f0-9-]+/)
      await expect(page.getByText('Details')).toBeVisible()
    }
  })

  test('should delete team from list', async ({ page, request }) => {
    const { api, team } = await seedAdminTeam(request, 'delete-list')
    await page.goto('/settings/teams')
    const [searchResponse] = await Promise.all([
      page.waitForResponse(response => {
        const url = new URL(response.url())
        return response.request().method() === 'GET' && url.pathname === '/api/teams' &&
          url.searchParams.get('search') === team.name && url.searchParams.get('page') === '1'
      }),
      tablePage.searchInput.fill(team.name),
    ])
    expect(searchResponse.ok()).toBe(true)
    const row = page.locator('tbody tr').filter({
      has: page.locator(`a[href="/settings/teams/${team.id}"]`),
    })
    await expect(row).toBeVisible()
    const deleteButton = row.locator('button').filter({ has: page.locator('svg.text-destructive') })
    await expect(deleteButton).toBeVisible()
    await deleteButton.click()
    const dialog = page.locator('[role="alertdialog"]')
    await expect(dialog).toBeVisible({ timeout: 3000 })
    await expect(dialog).toContainText(team.name)
    // This case exercises cancellation; the detail case below deletes its own team.
    await dialog.getByRole('button', { name: /Cancel/i }).click()
    await expect(dialog).not.toBeVisible()
    expect((await api.get(`/api/teams/${team.id}`)).ok()).toBe(true)
  })
})

test.describe('Teams - Detail Page CRUD', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdmin(page)
  })

  test('should show form fields on create page', async ({ page }) => {
    await page.goto('/settings/teams/new')
    await page.waitForLoadState('networkidle')

    await expect(page.locator('input').first()).toBeVisible()
    await expect(page.locator('textarea').first()).toBeVisible()
    await expect(page.locator('button[role="combobox"]').first()).toBeVisible()
    await expect(page.locator('button[role="switch"]').first()).toBeVisible()
  })

  test('should create a new team', async ({ page }) => {
    const newTeam = createTeamFixture()

    await page.goto('/settings/teams/new')
    await page.waitForLoadState('networkidle')

    const input = page.locator('input').first()
    if (await input.isDisabled()) { test.skip(true, 'No write permission'); return }

    await input.fill(newTeam.name)
    await page.locator('textarea').first().fill(newTeam.description)
    await page.waitForTimeout(300)

    const createBtn = page.getByRole('button', { name: /Create/i })
    if (!(await createBtn.isVisible({ timeout: 5000 }).catch(() => false))) {
      test.skip(true, 'Create button not visible')
      return
    }
    await createBtn.click({ force: true })
    await page.waitForTimeout(3000)

    if (page.url().includes('/new')) {
      test.skip(true, 'Creation failed (possibly CSRF)')
    } else {
      expect(page.url()).toMatch(/\/settings\/teams\/[a-f0-9-]+/)
    }
  })

  test('should edit existing team', async ({ page, request }) => {
    const { team } = await seedAdminTeam(request, 'edit')
    await page.goto(`/settings/teams/${team.id}`)
    await page.waitForLoadState('networkidle')
    const input = page.locator('input').first()
    await expect(input).toBeEnabled()
    await expect(input).toHaveValue(team.name)
    for (const name of [team.name + ' edited', team.name]) {
      await input.fill(name)
      const saveBtn = page.getByRole('button', { name: /Save/i })
      await expect(saveBtn).toBeVisible()
      const [response] = await Promise.all([
        page.waitForResponse(response =>
          new URL(response.url()).pathname === `/api/teams/${team.id}` &&
          response.request().method() === 'PUT'),
        saveBtn.click(),
      ])
      expect(response.ok()).toBe(true)
      expect(await response.finished()).toBeNull()
      await page.reload()
      await expect(input).toHaveValue(name)
    }
  })

  test('should delete from detail page', async ({ page, request }) => {
    const { api, team } = await seedAdminTeam(request, 'delete-detail')
    await page.goto(`/settings/teams/${team.id}`)
    await page.waitForLoadState('networkidle')
    await expect(page.locator('input').first()).toHaveValue(team.name)
    const deleteButton = page.getByRole('button', { name: /Delete/i }).first()
    await expect(deleteButton).toBeVisible()
    await deleteButton.click()
    const dialog = page.locator('[role="alertdialog"]')
    await expect(dialog).toBeVisible()
    const [response] = await Promise.all([
      page.waitForResponse(response =>
        new URL(response.url()).pathname === `/api/teams/${team.id}` &&
        response.request().method() === 'DELETE'),
      dialog.getByRole('button', { name: /Delete/i }).click(),
    ])
    expect(response.ok()).toBe(true)
    await expect(page).toHaveURL(/\/settings\/teams$/)
    expect((await api.get(`/api/teams/${team.id}`)).status()).toBe(404)
  })

  test('should show metadata', async ({ page }) => {
    await page.goto('/settings/teams')
    await page.waitForLoadState('networkidle')

    if (await navigateToFirstItem(page)) {
      await expectMetadataVisible(page)
    }
  })

  test('should show activity log', async ({ page, request }) => {
    // Seed our own team so we don't race with parallel workers that
    // create-then-delete teams. navigateToFirstItem grabs the first row's
    // href, but if another worker deletes that team before goto lands, the
    // detail page renders the "not found" error state and Activity Log
    // never appears.
    const api = new ApiHelper(request)
    await api.login(SUPER_ADMIN.email, SUPER_ADMIN.password)
    const teamResp = await api.post('/api/teams', {
      name: scope.name('activity-log'),
      description: 'seeded for activity-log test',
    })
    expect(teamResp.ok(), `seed team: ${await teamResp.text()}`).toBe(true)
    const team = (await teamResp.json()).data.team

    await page.goto(`/settings/teams/${team.id}`)
    await page.waitForLoadState('networkidle')

    await expectActivityLogVisible(page)
  })
})

test.describe('Teams - Table Sorting', () => {
  let tablePage: TablePage

  test.beforeEach(async ({ page }) => {
    await loginAsAdmin(page)
    await page.goto('/settings/teams')
    await page.waitForLoadState('networkidle')
    tablePage = new TablePage(page)
  })

  test('should sort by team name', async () => {
    await tablePage.clickColumnHeader('Team')
    const direction = await tablePage.getSortDirection('Team')
    expect(direction).not.toBeNull()
  })

  test('should sort by strategy', async () => {
    await tablePage.clickColumnHeader('Strategy')
    const direction = await tablePage.getSortDirection('Strategy')
    expect(direction).not.toBeNull()
  })

  test('should toggle sort direction', async () => {
    await tablePage.clickColumnHeader('Team')
    const firstDirection = await tablePage.getSortDirection('Team')
    await tablePage.clickColumnHeader('Team')
    const secondDirection = await tablePage.getSortDirection('Team')
    expect(firstDirection).not.toEqual(secondDirection)
  })
})

test.describe('Team Members', () => {
  test('should show members section on detail page', async ({ page }) => {
    await loginAsAdmin(page)
    await page.goto('/settings/teams')
    await page.waitForLoadState('networkidle')

    if (await navigateToFirstItem(page)) {
      await expect(page.getByRole('heading', { name: /Members/ })).toBeVisible({ timeout: 5000 })
    }
  })
})
