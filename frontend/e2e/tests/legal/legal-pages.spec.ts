import { expect, test } from '@playwright/test'

const legalPages = [
  {
    path: '/privacy',
    heading: 'Privacy Policy',
    marker: 'How we use personal data'
  },
  {
    path: '/terms',
    heading: 'Terms of Service',
    marker: 'Messaging and channel rules'
  },
  {
    path: '/data-deletion',
    heading: 'Data Deletion Instructions',
    marker: 'How to request deletion'
  }
]

for (const legalPage of legalPages) {
  test(`${legalPage.heading} is public and complete`, async ({ page }) => {
    await page.goto(legalPage.path)

    await expect(page).toHaveURL(new RegExp(`${legalPage.path}/?$`))
    await expect(page.getByRole('heading', { name: legalPage.heading, level: 1 })).toBeVisible()
    await expect(page.getByRole('heading', { name: legalPage.marker })).toBeVisible()
    await expect(page.getByRole('link', { name: 'medtechcorps@gmail.com' })).toHaveAttribute(
      'href',
      /mailto:medtechcorps@gmail\.com/
    )
  })
}

test('Privacy Policy discloses the Google Search Console data lifecycle', async ({ page }) => {
  await page.goto('/privacy')

  const googleSection = page.locator('#google-search-console')
  await expect(
    googleSection.getByRole('heading', { name: '5. Google Search Console and Google API data' })
  ).toBeVisible()
  await expect(googleSection).toContainText('clicks, impressions, click-through rate (CTR), average position')
  await expect(googleSection).toContainText('encrypted at rest')
  await expect(googleSection).toContainText('disconnect Google Search Console')
  await expect(googleSection).toContainText('Google API Services User Data Policy')
  await expect(googleSection).toContainText('Limited Use requirements')
})
