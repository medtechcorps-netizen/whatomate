import { expect, test, type Locator, type Page } from '@playwright/test'

const organizationId = '91111111-1111-4111-8111-111111111111'
const userId = '93333333-3333-4333-8333-333333333333'
const longAccountName =
  'Klinik Kesihatan Komuniti Bandar Baru Sri Petaling dan Cawangan Klinik Keluarga Sejahtera Putrajaya Presint Sembilan'

const user = {
  id: userId,
  email: 'calling-admin@example.test',
  full_name: 'Calling Admin',
  organization_id: organizationId,
  organization_name: 'Klinik Relive',
  is_super_admin: true,
  is_reseller_admin: false,
  role: {
    id: '94444444-4444-4444-8444-444444444444',
    name: 'admin',
    description: 'Administrator',
    is_system: true,
    permissions: [],
  },
}

async function installCallingMocks(page: Page) {
  await page.addInitScript((currentUser) => {
    window.localStorage.setItem('user', JSON.stringify(currentUser))
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
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: '' } } }),
  )
  await page.route(/\/api\/accounts(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          accounts: [
            { name: longAccountName },
            { name: 'Klinik Relive WhatsApp' },
          ],
        },
      },
    }),
  )
  await page.route(/\/api\/ivr-flows(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { ivr_flows: [], total: 0 } } }),
  )
}

async function expectWithinViewport(page: Page, locator: Locator) {
  const box = await locator.boundingBox()
  const viewport = page.viewportSize()
  expect(box).not.toBeNull()
  expect(viewport).not.toBeNull()
  if (!box || !viewport) return
  expect(box.x).toBeGreaterThanOrEqual(-1)
  expect(box.y).toBeGreaterThanOrEqual(-1)
  expect(box.x + box.width).toBeLessThanOrEqual(viewport.width + 1)
  expect(box.y + box.height).toBeLessThanOrEqual(viewport.height + 1)
}

for (const viewport of [
  { name: 'desktop', width: 1280, height: 720 },
  { name: 'mobile', width: 390, height: 844 },
  { name: 'narrow', width: 320, height: 500 },
  { name: 'short', width: 617, height: 320 },
]) {
  test(`create-flow dialog stays bounded with a long account name at ${viewport.name}`, async ({
    page,
  }) => {
    await page.setViewportSize({ width: viewport.width, height: viewport.height })
    await installCallingMocks(page)
    await page.goto('/calling/ivr-flows')

    await expect(page.getByRole('heading', { name: 'IVR Flows' })).toBeVisible()
    await page.getByRole('button', { name: /create.*flow/i }).first().click()

    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    await expect(dialog.getByLabel('Name')).toBeVisible()
    await expect(dialog.getByLabel('Account')).toBeVisible()
    await expect(dialog.getByLabel('Description')).toBeVisible()
    await expectWithinViewport(page, dialog)

    const trigger = dialog.getByRole('combobox')
    await expect(trigger).toHaveAttribute('title', longAccountName)
    await trigger.click()

    const option = page.getByRole('option', { name: longAccountName, exact: true })
    await expect(option).toBeVisible()
    await expectWithinViewport(page, option)
    const optionOverflow = await option.evaluate((element) => ({
      clientWidth: element.clientWidth,
      scrollWidth: element.scrollWidth,
    }))
    expect(optionOverflow.scrollWidth).toBeLessThanOrEqual(
      optionOverflow.clientWidth + 1,
    )

    await page.keyboard.press('Escape')
    await expect(option).toBeHidden()
    const createButton = dialog.getByRole('button', { name: 'Create', exact: true })
    await createButton.scrollIntoViewIfNeeded()
    await expect(createButton).toBeVisible()
    await expectWithinViewport(page, dialog)

    await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
    await expect(dialog).toBeHidden()
  })
}
