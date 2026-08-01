import { expect, test, type Page } from '@playwright/test'

const organizationId = '41111111-1111-4111-8111-111111111111'

const permission = (resource: string, action = 'read') => ({
  id: `${resource}-${action}`,
  resource,
  action,
})

const flowAuthor = {
  id: '42222222-2222-4222-8222-222222222222',
  email: 'flow-author@example.test',
  full_name: 'Flow Author',
  organization_id: organizationId,
  organization_name: 'Flow Test Workspace',
  is_super_admin: false,
  is_reseller_admin: false,
  role: {
    id: '43333333-3333-4333-8333-333333333333',
    name: 'flow_author',
    is_system: false,
    permissions: [
      permission('flows.chatbot'),
      permission('flows.chatbot', 'write'),
      permission('settings.chatbot'),
      permission('teams'),
    ],
  },
}

async function installMocks(page: Page) {
  await page.addInitScript((user) => {
    window.localStorage.setItem('user', JSON.stringify(user))
    window.localStorage.setItem('locale', 'en')
  }, flowAuthor)

  await page.route('**/api/**', (route) => route.fulfill({ json: { data: {} } }))
  await page.route(/\/api\/me(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: flowAuthor } }),
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
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: '' } } }),
  )
  await page.route(/\/api\/teams(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { teams: [], total: 0 } } }),
  )
  await page.route(/\/api\/chatbot\/flows(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { flows: [], total: 0 } } }),
  )
}

test.beforeEach(async ({ page }) => {
  await installMocks(page)
  await page.goto('/chatbot/flows/new')
  await expect(page.getByText('Add node:')).toBeVisible()
})

test('set-variable authoring validates assignments and previews stored values', async ({ page }) => {
  const paletteButton = page.getByRole('button', { name: 'Set variable', exact: true })
  await paletteButton.focus()
  await page.keyboard.press('Enter')

  await expect(page.getByRole('heading', { name: 'Set variable' })).toBeVisible()
  const nameInput = page.getByLabel('Name')
  await expect(nameInput).toHaveValue('variable_1')
  await nameInput.fill('customer_tier')
  await nameInput.press('Tab')

  await page.getByLabel('Value').fill('premium')
  await page.getByRole('button', { name: 'Preview', exact: true }).click()
  await page.locator('button[title="Start"]').click()

  await expect(page.getByText('customer_tier:', { exact: true })).toBeVisible()
  await expect(page.getByText('premium', { exact: true }).first()).toBeVisible()
})

test('set-variable save is blocked when all assignments are removed', async ({ page }) => {
  let createRequests = 0
  page.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname === '/api/chatbot/flows') {
      createRequests++
    }
  })

  await page.getByRole('button', { name: 'Set variable', exact: true }).click()
  await page.getByRole('button', { name: 'Remove variable variable_1' }).click()
  await expect(page.getByRole('alert')).toContainText('Add at least one variable assignment.')

  await page.getByPlaceholder('Enter flow name').fill('Invalid variable flow')
  await page.getByRole('button', { name: 'Save Flow', exact: true }).click()

  await expect(page.getByRole('alert')).toContainText('Add at least one variable assignment.')
  expect(createRequests).toBe(0)
})

test('AI response exposes only the backend prompt contract and a safe preview', async ({ page }) => {
  await page.getByRole('button', { name: 'AI response', exact: true }).click()

  const prompt = page.getByLabel('Prompt template (optional)')
  await expect(prompt).toBeVisible()
  await prompt.fill('Summarise the customer request in one sentence.')
  await expect(page.getByText(/uses the chatbot AI provider already configured/i)).toBeVisible()
  await expect(page.getByText(/Preview never calls the AI provider/i)).toBeVisible()

  await page.getByRole('button', { name: 'Preview', exact: true }).click()
  await page.locator('button[title="Start"]').click()
  await expect(page.getByText(/AI response preview would use rendered prompt "Summarise the customer request in one sentence\."/i)).toBeVisible()
  await expect(page.getByText(/Model calls are disabled in preview/i)).toBeVisible()
})
