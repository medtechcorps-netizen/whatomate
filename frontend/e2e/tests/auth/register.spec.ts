import { test, expect } from '@playwright/test'
import { createTestScope } from '../../framework'

const scope = createTestScope('register')
const INVITATION_URL = '/register?invite=e2e-registration-invitation'

test.describe('Register', () => {
  test('should show invitation required message without org param', async ({ page }) => {
    await page.goto('/register')

    await expect(page.locator('input#fullName')).not.toBeVisible()
    await expect(page.locator('input#email')).not.toBeVisible()
    await expect(page.locator('input#password')).not.toBeVisible()

    await expect(page.locator('text=/invitation/i')).toBeVisible()
    await expect(page.getByRole('link', { name: /Sign in/i })).toBeVisible()
  })

  test('should display registration form with invitation token', async ({ page }) => {
    await page.goto(INVITATION_URL)

    await expect(page.locator('input#fullName')).toBeVisible()
    await expect(page.locator('input#email')).toBeVisible()
    await expect(page.locator('input#password')).toBeVisible()
    await expect(page.locator('input#confirmPassword')).toBeVisible()
    await expect(page.locator('button[type="submit"]')).toBeVisible()
  })

  test('should show error for empty fields', async ({ page }) => {
    await page.goto(INVITATION_URL)
    await page.locator('button[type="submit"]').click()

    const toast = page.locator('[data-sonner-toast]')
    await expect(toast).toBeVisible({ timeout: 5000 })
    await expect(toast).toContainText('fill in all fields')
  })

  test('should show error for mismatched passwords', async ({ page }) => {
    await page.goto(INVITATION_URL)
    await page.locator('input#fullName').fill('Test User')
    await page.locator('input#email').fill(scope.email('mismatch'))
    await page.locator('input#password').fill('password123')
    await page.locator('input#confirmPassword').fill('different123')
    await page.locator('button[type="submit"]').click()

    const toast = page.locator('[data-sonner-toast]')
    await expect(toast).toBeVisible({ timeout: 5000 })
    await expect(toast).toContainText('do not match')
  })

  test('should show error for short password', async ({ page }) => {
    await page.goto(INVITATION_URL)
    await page.locator('input#fullName').fill('Test User')
    await page.locator('input#email').fill(scope.email('short'))
    await page.locator('input#password').fill('short')
    await page.locator('input#confirmPassword').fill('short')
    await page.locator('button[type="submit"]').click()

    const toast = page.locator('[data-sonner-toast]')
    await expect(toast).toBeVisible({ timeout: 5000 })
    await expect(toast).toContainText('at least 12 characters')
  })

  test('should navigate to login page from invitation required', async ({ page }) => {
    await page.goto('/register')
    await page.getByRole('link', { name: /Sign in/i }).click()
    await expect(page).toHaveURL(/\/login/)
  })

  test('should navigate to login page from registration form', async ({ page }) => {
    await page.goto(INVITATION_URL)
    await page.locator('a').filter({ hasText: /Sign in/i }).click()
    await expect(page).toHaveURL(/\/login/)
  })
})
