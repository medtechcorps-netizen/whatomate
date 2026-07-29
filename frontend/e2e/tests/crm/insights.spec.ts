import { expect, test, type Page } from '@playwright/test'

type PermissionResource =
  | 'crm.leads'
  | 'tasks'
  | 'bookings'
  | 'packages'
  | 'payments'
  | 'contacts'
  | 'chat'

interface MockCRMOptions {
  permissions: PermissionResource[]
  insightsStatus?: number
  insights?: Record<string, unknown>
  segments?: Array<{
    key: string
    label: string
    description: string
    count: number
  }>
  segmentContacts?: Record<string, unknown>
  onSegmentsRequest?: () => void
}

const fullInsights = {
  range: {
    from: '2026-07-01T00:00:00Z',
    to: '2026-07-30T23:59:59Z',
  },
  pipeline: {
    open_count: 12,
    won_count: 7,
    lost_count: 3,
    conversion_rate: 70,
    open_value: [{ currency: 'MYR', amount_minor: 1_200_000 }],
  },
  bookings: {
    total: 12,
    completed: 8,
    no_show: 2,
    cancelled: 2,
    attendance_rate: 80,
    no_show_rate: 20,
  },
  revenue: {
    collected: [{ currency: 'MYR', amount_minor: 250_000 }],
    outstanding: [{ currency: 'MYR', amount_minor: 45_000 }],
    overdue_invoices: 2,
  },
  packages: {
    active: 18,
    low_balance: 3,
    expiring_soon: 2,
  },
  tasks: {
    open: 9,
    overdue: 4,
  },
  generated_at: '2026-07-30T08:00:00Z',
}

async function mockCRMInsights(page: Page, options: MockCRMOptions) {
  const user = {
    id: '11111111-1111-4111-8111-111111111111',
    email: 'insights@example.test',
    full_name: 'Insights Reviewer',
    organization_id: '22222222-2222-4222-8222-222222222222',
    role: {
      id: '33333333-3333-4333-8333-333333333333',
      name: 'insights-reviewer',
      description: 'Mocked CRM insights role',
      is_system: false,
      permissions: options.permissions.map((resource, index) => ({
        id: `permission-${index}`,
        resource,
        action: 'read',
      })),
    },
  }

  await page.addInitScript((mockUser) => {
    window.localStorage.setItem('user', JSON.stringify(mockUser))
  }, user)

  // Register the catch-all first. Playwright gives later, more-specific routes
  // priority, while unrelated shell requests get a harmless empty envelope.
  await page.route('**/api/**', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ data: {} }),
    })
  })
  await page.route('**/api/me', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ data: user }),
    })
  })
  await page.route('**/api/product/entitlements**', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        data: {
          mode: 'licensed',
          entitlements: {
            'crm.enabled': true,
            'bookings.enabled': true,
            'commerce.enabled': true,
          },
        },
      }),
    })
  })
  await page.route('**/api/auth/ws-token', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ data: { token: '' } }),
    })
  })
  await page.route('**/api/crm/insights**', async (route) => {
    const status = options.insightsStatus ?? 200
    await route.fulfill({
      status,
      contentType: 'application/json',
      body: JSON.stringify(
        status >= 400
          ? { message: 'Report query timed out' }
          : { data: options.insights ?? fullInsights },
      ),
    })
  })
  await page.route('**/api/crm/segments', async (route) => {
    options.onSegmentsRequest?.()
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        data: {
          segments: options.segments ?? [],
        },
      }),
    })
  })
  await page.route('**/api/crm/segments/*/contacts**', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        data: options.segmentContacts ?? {
          segment: options.segments?.[0],
          contacts: [],
          total: 0,
          page: 1,
          limit: 50,
        },
      }),
    })
  })
}

test.describe('CRM insights', () => {
  test('renders exact metrics and live segment contacts without seeded data', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await mockCRMInsights(page, {
      permissions: [
        'crm.leads',
        'tasks',
        'bookings',
        'packages',
        'payments',
        'contacts',
        'chat',
      ],
      segments: [
        {
          key: 'needs_follow_up',
          label: 'Needs follow-up',
          description: 'Customers with an open follow-up task.',
          count: 2,
        },
      ],
      segmentContacts: {
        segment: {
          key: 'needs_follow_up',
          label: 'Needs follow-up',
          description: 'Customers with an open follow-up task.',
          count: 2,
        },
        contacts: [
          {
            id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
            profile_name: 'Aisha Rahman',
            phone_number: '60123456789',
            last_message_at: '2026-07-29T03:00:00Z',
          },
          {
            id: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',
            profile_name: 'Daniel Lim',
            phone_number: '60198765432',
            last_message_at: '2026-07-28T03:00:00Z',
          },
        ],
        total: 2,
        page: 1,
        limit: 50,
      },
    })

    await page.goto('/crm/insights')

    await expect(page.getByRole('heading', { name: 'CRM insights' })).toBeVisible()
    await expect(page.getByText('12 active journeys')).toBeVisible()
    await expect(page.getByText('70%')).toBeVisible()
    await expect(page.getByText('8 completed')).toBeVisible()
    await expect(page.getByText('Needs follow-up', { exact: true }).first()).toBeVisible()

    await page.getByRole('button', { name: /Needs follow-up/ }).click()
    await expect(page.getByText('Aisha Rahman')).toBeVisible()
    await expect(page.getByText('Daniel Lim')).toBeVisible()

    const hasHorizontalOverflow = await page.evaluate(
      () => document.documentElement.scrollWidth > document.documentElement.clientWidth + 1,
    )
    expect(hasHorizontalOverflow).toBe(false)
  })

  test('does not present permission-filtered sections as zero performance', async ({ page }) => {
    let segmentRequests = 0
    await mockCRMInsights(page, {
      permissions: ['tasks'],
      insights: {
        ...fullInsights,
        // Deliberately non-zero to prove the UI does not trust inaccessible
        // sections even if a malformed response contains values.
        pipeline: { ...fullInsights.pipeline, open_count: 99 },
        tasks: { open: 5, overdue: 3 },
      },
      onSegmentsRequest: () => {
        segmentRequests += 1
      },
    })

    await page.goto('/crm/insights')

    await expect(page.getByRole('heading', { name: 'CRM insights' })).toBeVisible()
    await expect(page.getByText('Overdue tasks')).toBeVisible()
    await expect(page.getByText('Contact segments require Contacts read access.')).toBeVisible()
    await expect(page.getByText('Open pipeline')).toHaveCount(0)
    await expect(page.getByText('Attendance', { exact: true })).toHaveCount(0)
    await expect(page.getByText('Collected', { exact: true })).toHaveCount(0)
    expect(segmentRequests).toBe(0)
  })

  test('shows a retryable error instead of a dashboard full of zeroes', async ({ page }) => {
    await mockCRMInsights(page, {
      permissions: ['tasks'],
      insightsStatus: 500,
    })

    await page.goto('/crm/insights')

    await expect(page.getByRole('alert')).toContainText('CRM insights are unavailable')
    await expect(page.getByRole('alert')).toContainText('Report query timed out')
    await expect(page.getByRole('button', { name: 'Try again' })).toBeVisible()
    await expect(page.getByText('Open pipeline')).toHaveCount(0)
  })
})
