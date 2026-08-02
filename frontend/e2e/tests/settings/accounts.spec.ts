import { test, expect, type APIRequestContext, type Page } from '@playwright/test'
import { loginAsAdmin, expectMetadataVisible, expectActivityLogVisible, expectDeleteFromForm, ApiHelper } from '../../helpers'
import { AccountsPage } from '../../pages'
import { createTestScope, loginAsSuperAdmin, SUPER_ADMIN } from '../../framework'

const scope = createTestScope('accounts')

async function seedAdminAccount(request: APIRequestContext) {
  const api = new ApiHelper(request)
  await api.loginAsAdmin()
  const fixtureKey = scope.name().toLowerCase()

  return api.createWhatsAppAccount({
    name: fixtureKey,
    phone_id: `phone-${fixtureKey}`,
    business_id: `biz-${fixtureKey}`,
    access_token: 'e2e-local-fixture-token',
  })
}

async function gotoSeededAdminAccount(page: Page, request: APIRequestContext) {
  const account = await seedAdminAccount(request)
  await page.goto(`/settings/accounts/${account.id}`)
  await page.waitForLoadState('networkidle')
  return account
}

test.describe('WhatsApp Accounts - List View', () => {
  let accountsPage: AccountsPage

  test.beforeEach(async ({ page }) => {
    await loginAsAdmin(page)
    accountsPage = new AccountsPage(page)
    await accountsPage.goto()
  })

  test('should display accounts page', async () => {
    await accountsPage.expectPageVisible()
    await expect(accountsPage.addButton).toBeVisible()
  })

  test('should load create page', async ({ page }) => {
    await page.goto('/settings/accounts/new')
    await page.waitForLoadState('networkidle')
    expect(page.url()).toContain('/settings/accounts/new')
    await expect(page.locator('input').first()).toBeVisible()
  })

  test('should show delete confirmation from list', async ({ page }) => {
    // Find the destructive (red) delete button in the first data row
    const firstRow = page.locator('tbody tr').first()
    if (!(await firstRow.isVisible({ timeout: 3000 }).catch(() => false))) {
      test.skip(true, 'No accounts in list')
      return
    }
    const deleteBtn = firstRow.locator('button.text-destructive, button:has(svg.text-destructive)').first()
    if (!(await deleteBtn.isVisible({ timeout: 3000 }).catch(() => false))) {
      test.skip(true, 'No delete button found')
      return
    }
    await deleteBtn.click()
    await expect(accountsPage.alertDialog).toBeVisible({ timeout: 5000 })
    await accountsPage.cancelDelete()
  })

  test('should load detail page from list', async ({ page, request }) => {
    const account = await seedAdminAccount(request)
    await accountsPage.goto()

    const accountLink = page.locator(`tbody a[href="/settings/accounts/${account.id}"]`).first()
    await expect(accountLink).toBeVisible({ timeout: 15000 })
    await accountLink.click()

    await expect(page).toHaveURL(new RegExp(`/settings/accounts/${account.id}$`))
    await expect(page.getByText('Account Details')).toBeVisible()
  })
})

test.describe('WhatsApp Accounts - Detail Page CRUD', () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdmin(page)
  })

  test('should show form fields on create page', async ({ page }) => {
    await page.goto('/settings/accounts/new')
    await page.waitForLoadState('networkidle')

    await expect(page.locator('input').first()).toBeVisible()
    await expect(page.locator('input[type="password"]').first()).toBeVisible()
  })

  test('should show validation error for empty required fields', async ({ page }) => {
    await page.goto('/settings/accounts/new')
    await page.waitForLoadState('networkidle')

    // Fill something to trigger hasChanges
    const input = page.locator('input').first()
    if (await input.isDisabled()) { test.skip(true, 'No write permission'); return }

    await input.fill('test')
    await input.clear()
    await page.waitForTimeout(300)

    const createBtn = page.getByRole('button', { name: /Create/i })
    if (await createBtn.isVisible({ timeout: 3000 }).catch(() => false)) {
      await createBtn.click({ force: true })
      const toast = page.locator('[data-sonner-toast]').first()
      await expect(toast).toBeVisible({ timeout: 5000 })
    }
  })

  test('should direct Meta credential management to Integration Center', async ({ page, request }) => {
    await gotoSeededAdminAccount(page, request)

    await expect(page.getByTestId('meta-integration-management-notice')).toBeVisible()

    const integrationLink = page.getByTestId('meta-integration-management-link')
    await expect(integrationLink).toBeVisible()
    await expect(integrationLink).toHaveAttribute('href', '/settings/integrations')
  })

  test('should have test connection button', async ({ page, request }) => {
    await gotoSeededAdminAccount(page, request)
    await expect(page.getByRole('button', { name: /Test/i })).toBeVisible()
  })

  test('should have subscribe button', async ({ page, request }) => {
    await gotoSeededAdminAccount(page, request)
    await expect(page.getByRole('button', { name: /Subscribe/i })).toBeVisible()
  })

  test('should have business profile button', async ({ page, request }) => {
    await gotoSeededAdminAccount(page, request)
    await expect(page.getByRole('button', { name: /Profile/i })).toBeVisible()
  })

  test('should delete from detail page', async ({ page, request }) => {
    await gotoSeededAdminAccount(page, request)
    await expectDeleteFromForm(page, '/settings/accounts')
  })

  test('should show metadata', async ({ page, request }) => {
    await gotoSeededAdminAccount(page, request)
    await expectMetadataVisible(page)
  })

  test('should show activity log', async ({ page, request }) => {
    await gotoSeededAdminAccount(page, request)
    await expectActivityLogVisible(page)
  })

  test('should show setup guide', async ({ page, request }) => {
    await gotoSeededAdminAccount(page, request)
    await expect(page.getByText('Setup Guide')).toBeVisible({ timeout: 15000 })
  })

  test('should show connection details card upon successful test connection', async ({ page, request }) => {
    // Browser must share identity with the API session below; otherwise
    // /settings/accounts/:id 404s for the wrong org. See framework/auth.ts.
    await loginAsSuperAdmin(page)
    const api = new ApiHelper(request)
    await api.login(SUPER_ADMIN.email, SUPER_ADMIN.password)
    const acc = await api.createWhatsAppAccount({
      name: scope.name('conn-details').toLowerCase().replace(/\s/g, '-'),
      phone_id: `phone-conn-${Date.now()}`,
      business_id: `biz-conn-${Date.now()}`,
      access_token: 'test-token-e2e',
    })

    // Stub the connection test response
    await page.route(`**/api/accounts/${acc.id}/test`, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: {
            success: true,
            display_phone_number: '1234567890',
            verified_name: 'Test Verified Company Name',
            quality_rating: 'GREEN',
            messaging_limit_tier: 'TIER_250',
            code_verification_status: 'VERIFIED',
            account_mode: 'LIVE',
            is_test_number: false
          }
        })
      })
    })

    await page.goto(`/settings/accounts/${acc.id}`)
    await page.waitForLoadState('networkidle')

    // Click the Test button
    await page.getByRole('button', { name: /Test/i }).click()

    // Assert details card is shown and fields are correct
    await expect(page.getByText('Details', { exact: true })).toBeVisible()
    await expect(page.getByText('Test Verified Company Name')).toBeVisible()
    await expect(page.getByText('High')).toBeVisible() // GREEN is mapped to High
    await expect(page.getByText('250 msgs/day')).toBeVisible() // TIER_250 mapped to 250 msgs/day
    await expect(page.getByText('Verified', { exact: true })).toBeVisible() // VERIFIED mapped to Verified
  })

  test('should show connection details card with UNKNOWN quality rating translated to Unknown', async ({ page, request }) => {
    await loginAsSuperAdmin(page)
    const api = new ApiHelper(request)
    await api.login(SUPER_ADMIN.email, SUPER_ADMIN.password)
    const acc = await api.createWhatsAppAccount({
      name: scope.name('conn-details-unk').toLowerCase().replace(/\s/g, '-'),
      phone_id: `phone-conn-unk-${Date.now()}`,
      business_id: `biz-conn-unk-${Date.now()}`,
      access_token: 'test-token-e2e',
    })

    // Stub the connection test response with UNKNOWN quality rating
    await page.route(`**/api/accounts/${acc.id}/test`, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: {
            success: true,
            display_phone_number: '1234567890',
            verified_name: 'Test Company',
            quality_rating: 'UNKNOWN',
            messaging_limit_tier: 'TIER_250',
            code_verification_status: 'VERIFIED',
            account_mode: 'LIVE',
            is_test_number: false
          }
        })
      })
    })

    await page.goto(`/settings/accounts/${acc.id}`)
    await page.waitForLoadState('networkidle')

    // Click the Test button
    await page.getByRole('button', { name: /Test/i }).click()

    // Assert details card is shown and UNKNOWN is translated to Unknown
    await expect(page.getByText('Details', { exact: true })).toBeVisible()
    await expect(page.getByText('Unknown')).toBeVisible()
  })
})
