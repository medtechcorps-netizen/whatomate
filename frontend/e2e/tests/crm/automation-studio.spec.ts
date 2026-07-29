import { test, expect } from '@playwright/test'
import { loginAsAdmin } from '../../helpers'
import { AutomationStudioPage } from '../../pages'

test.describe('CRM Automation Studio', () => {
  let studio: AutomationStudioPage

  test.beforeEach(async ({ page }) => {
    await loginAsAdmin(page)
    studio = new AutomationStudioPage(page)
    await studio.gotoList()
  })

  test('offers the four task-first starter policies', async ({ page }) => {
    await expect(studio.policyList).toBeVisible()
    for (const key of ['post-visit', 'no-show', 'package-care', 'overdue-invoice']) {
      await expect(page.getByTestId(`automation-template-${key}`)).toBeVisible()
    }
    await expect(page.getByText('Customer messages stay locked')).toBeVisible()
  })

  test('opens the post-visit visual graph with a meaningful safe preview', async ({ page }) => {
    await studio.useTemplate('post-visit')

    await expect(studio.palette).toBeVisible()
    await expect(studio.canvas).toBeVisible()
    await expect(page.getByLabel('Policy name')).toHaveValue('Post-visit care')
    await expect(page.getByText('booking.status_changed')).toBeVisible()
    await studio.canvas.getByText('Create follow-up', { exact: true }).click()
    await expect(page.getByText('Internal action only')).toBeVisible()
    await expect(studio.saveDraft).toBeEnabled()
    await expect(studio.activate).toBeVisible()
  })

  test('package care listens for both low balance and expiry events', async ({ page }) => {
    await studio.useTemplate('package-care')

    await expect(page.getByText(/package\.balance_low or package\.expiring/)).toBeVisible()
    await expect(page.getByRole('button', { name: /Send message/ })).toBeDisabled()
  })

  test('keeps the builder usable on a narrow viewport', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await studio.useTemplate('no-show')

    await expect(studio.canvas).toBeVisible()
    await expect(studio.palette).toBeVisible()
    await expect(page.getByRole('button', { name: 'Wait' })).toBeVisible()
  })
})
