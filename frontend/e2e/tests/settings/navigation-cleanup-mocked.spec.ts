import { expect, test, type Page } from '@playwright/test'

const organizationId = '31111111-1111-4111-8111-111111111111'

const permission = (resource: string) => ({
  id: `${resource}-read`,
  resource,
  action: 'read',
})

function userWithPermissions(resources: string[]) {
  return {
    id: '32222222-2222-4222-8222-222222222222',
    email: 'navigation-cleanup@example.test',
    full_name: 'Navigation Test User',
    organization_id: organizationId,
    organization_name: 'Navigation Test Workspace',
    is_super_admin: false,
    is_reseller_admin: false,
    role: {
      id: '33333333-3333-4333-8333-333333333333',
      name: 'navigation-test',
      description: 'Focused navigation regression role',
      is_system: false,
      permissions: resources.map(permission),
    },
  }
}

async function installNavigationMocks(page: Page, resources: string[]) {
  const user = userWithPermissions(resources)

  await page.addInitScript((storedUser) => {
    window.localStorage.setItem('user', JSON.stringify(storedUser))
    window.localStorage.setItem('locale', 'en')
  }, user)

  await page.route('**/api/**', (route) =>
    route.fulfill({ json: { data: {} } }),
  )
  await page.route(/\/api\/me(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: user } }),
  )
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          mode: 'licensed',
          plan_code: 'rereply-growth',
          entitlements: {},
        },
      },
    }),
  )
  await page.route(/\/api\/audit-logs(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { audit_logs: [], total: 0 } } }),
  )
}

test('sidebar exposes one destination per page and distinguishes both flow products', async ({ page }) => {
  await installNavigationMocks(page, [
    'settings.general',
    'settings.chatbot',
    'flows.chatbot',
    'flows.whatsapp',
  ])

  await page.goto('/chatbot')
  const sidebar = page.locator('aside')

  await expect(sidebar.getByRole('menuitem', { name: 'Chatbot', exact: true })).toHaveCount(1)
  await expect(sidebar.getByRole('menuitem', { name: 'Overview', exact: true })).toHaveCount(0)
  await expect(sidebar.getByRole('menuitem', { name: 'Chatbot Flows', exact: true })).toHaveCount(1)
  await expect(sidebar.getByRole('menuitem', { name: 'WhatsApp Flows', exact: true })).toHaveCount(1)
  await expect(sidebar.getByRole('menuitem', { name: 'Flows', exact: true })).toHaveCount(0)

  await page.goto('/settings')
  await expect(sidebar.getByRole('menuitem', { name: 'Settings', exact: true })).toHaveCount(1)
  await expect(sidebar.getByRole('menuitem', { name: 'General', exact: true })).toHaveCount(0)
})

test('an audit-only user falls back to Audit Logs', async ({ page }) => {
  await installNavigationMocks(page, ['audit_logs'])

  await page.goto('/')

  await expect(page).toHaveURL(/\/settings\/audit-logs$/)
  await expect(page.locator('aside').getByRole('menuitem', { name: 'Audit Logs', exact: true })).toHaveCount(1)
})
