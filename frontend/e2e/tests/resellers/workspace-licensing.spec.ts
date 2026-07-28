import { expect, test, type Page } from '@playwright/test'

const organization = {
  id: '11111111-1111-4111-8111-111111111111',
  reseller_id: '22222222-2222-4222-8222-222222222222',
  reseller_name: 'Omnitech Partner',
  name: 'ReAlign Kajang',
  slug: 'realign-kajang',
  created_at: '2026-07-20T08:00:00Z',
}

const reseller = {
  id: organization.reseller_id,
  name: 'Omnitech Partner',
  slug: 'omnitech-partner',
  status: 'active',
  plan: 'growth',
  max_organizations: 50,
  brand_name: 'Omnitech',
  logo_url: '',
  primary_color: '#0f766e',
  accent_color: '#f59e0b',
  support_email: 'support@example.test',
  custom_domain: 'partner.example.test',
  organization_count: 1,
  member_count: 1,
  created_at: '2026-07-01T08:00:00Z',
}

const growthPlan = {
  id: '33333333-3333-4333-8333-333333333333',
  code: 'omnitech-growth',
  name: 'Omnitech Growth',
  description: 'CRM and omnichannel workspace',
  vertical: 'general',
  status: 'active',
  trial_days: 14,
  is_public: false,
  entitlements: { 'omnichannel.enabled': true },
  prices: [
    {
      id: '77777777-7777-4777-8777-777777777777',
      code: 'omnitech-growth-myr-year',
      currency: 'MYR',
      unit_amount_minor: 299000,
      setup_amount_minor: 0,
      interval: 'year',
      interval_count: 1,
      tax_behavior: 'exclusive',
    },
    {
      id: '88888888-8888-4888-8888-888888888888',
      code: 'omnitech-growth-myr-month',
      currency: 'MYR',
      unit_amount_minor: 29900,
      setup_amount_minor: 0,
      interval: 'month',
      interval_count: 1,
      tax_behavior: 'exclusive',
    },
  ],
}

const platformOwner = {
  id: '55555555-5555-4555-8555-555555555555',
  email: 'owner@example.test',
  full_name: 'Platform Owner',
  organization_id: organization.id,
  organization_name: organization.name,
  is_super_admin: true,
  is_reseller_admin: false,
}

const resellerAdmin = {
  ...platformOwner,
  id: '66666666-6666-4666-8666-666666666666',
  email: 'partner-admin@example.test',
  full_name: 'Partner Administrator',
  is_super_admin: false,
  is_reseller_admin: true,
}

async function installWorkspaceSession(page: Page, user = platformOwner) {
  await page.addInitScript((user) => {
    window.localStorage.setItem('user', JSON.stringify(user))
  }, user)

  await page.route(/\/api\/me(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: user } }),
  )
  await page.route(/\/api\/me\/organizations(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          organizations: [
            {
              organization_id: organization.id,
              name: organization.name,
              slug: organization.slug,
              role_name: 'Platform Owner',
              is_default: true,
            },
          ],
        },
      },
    }),
  )
  await page.route(/\/api\/organizations(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { organizations: [organization] } } }),
  )
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          plan_code: growthPlan.code,
          mode: 'subscription',
          entitlements: {},
          overridden_keys: [],
          evaluated_at: '2026-07-28T08:00:00Z',
        },
      },
    }),
  )
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: '' } } }),
  )
}

async function mockPartnerPortfolio(
  page: Page,
  plans: (typeof growthPlan)[] = [growthPlan],
  user = platformOwner,
  initialPlanPriceId = growthPlan.prices[1].id,
) {
  await installWorkspaceSession(page, user)

  let license = {
    id: '44444444-4444-4444-8444-444444444444',
    plan_id: growthPlan.id,
    plan_price_id: initialPlanPriceId,
    plan_code: growthPlan.code,
    plan_name: growthPlan.name,
    status: 'active',
    provider: 'manual',
    current_period_start: '2026-07-20T08:00:00Z',
    current_period_end: '2026-08-20T08:00:00Z',
    cancel_at_period_end: false,
  }
  let assignment: Record<string, unknown> | null = null
  let catalogRequests = 0

  await page.route(/\/api\/resellers(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { resellers: [reseller] } } }),
  )
  await page.route(
    new RegExp(`/api/resellers/${reseller.id}/usage(?:\\?.*)?$`),
    (route) =>
      route.fulfill({
        json: {
          data: {
            reseller_id: reseller.id,
            plan: reseller.plan,
            max_organizations: reseller.max_organizations,
            organizations: [organization],
            organization_count: 1,
            user_count: 4,
            whatsapp_accounts: 1,
            contacts: 12,
            messages: 48,
          },
        },
      }),
  )
  await page.route(
    new RegExp(`/api/resellers/${reseller.id}/members(?:\\?.*)?$`),
    (route) => route.fulfill({ json: { data: { members: [] } } }),
  )
  await page.route(
    new RegExp(
      `/api/admin/organizations/${organization.id}/product/plans(?:\\?.*)?$`,
    ),
    (route) => {
      catalogRequests += 1
      return route.fulfill({ json: { data: { plans } } })
    },
  )
  await page.route(
    new RegExp(
      `/api/admin/organizations/${organization.id}/subscription(?:\\?.*)?$`,
    ),
    async (route) => {
      if (route.request().method() === 'PUT') {
        assignment = route.request().postDataJSON() as Record<string, unknown>
        license = {
          ...license,
          plan_id: String(assignment.plan_id),
          plan_price_id: String(assignment.plan_price_id),
          plan_name: growthPlan.name,
          status: String(assignment.status),
          current_period_end: '2026-08-18T08:00:00Z',
          ...(assignment.status === 'trialing'
            ? { trial_ends_at: '2026-08-18T08:00:00Z' }
            : {}),
        }
      }
      await route.fulfill({ json: { data: license } })
    },
  )

  return {
    assignment: () => assignment,
    catalogRequests: () => catalogRequests,
  }
}

test.describe('Partner Console workspace licensing', () => {
  test('platform owner assigns a private trialing plan by exact manual price identity', async ({
    page,
  }) => {
    const state = await mockPartnerPortfolio(page)
    await page.goto('/resellers')

    await expect(page.getByTestId('workspace-license-row')).toContainText(
      'ReAlign Kajang',
    )
    await expect(page.getByTestId('workspace-license-row')).toContainText(
      'Omnitech Growth',
    )
    await expect(page.getByTestId('workspace-license-panel')).toContainText(
      'Active',
    )
    await expect(page.getByTestId('workspace-license-plan')).toHaveValue(
      `${growthPlan.id}::${growthPlan.prices[1].id}`,
    )

    await page
      .getByTestId('workspace-license-plan')
      .selectOption(`${growthPlan.id}::${growthPlan.prices[1].id}`)
    await page.getByTestId('workspace-license-status').selectOption('trialing')
    await page.getByTestId('workspace-license-trial').fill('29')
    await page
      .getByTestId('workspace-license-reference')
      .fill('REALIGN-2026-001')
    await page.getByTestId('workspace-license-submit').click()

    await expect.poll(state.assignment).toMatchObject({
      plan_id: growthPlan.id,
      plan_price_id: growthPlan.prices[1].id,
      status: 'trialing',
      trial_days: 29,
      manual_reference: 'REALIGN-2026-001',
    })
    expect(state.assignment()).not.toHaveProperty('provider')
    expect(state.assignment()).not.toHaveProperty('provider_data')
    await expect(page.getByTestId('workspace-license-panel')).toContainText(
      'Trialing',
    )
  })

  test('shows a clear empty state when no active product plans are available', async ({
    page,
  }) => {
    await mockPartnerPortfolio(page, [])
    await page.goto('/resellers')

    await expect(page.getByTestId('workspace-license-no-plans')).toContainText(
      'No assignable product plans',
    )
    await expect(page.getByTestId('workspace-license-submit')).toHaveCount(0)
  })

  test('reseller administrator retains read-only visibility for its workspace licenses', async ({
    page,
  }) => {
    const state = await mockPartnerPortfolio(page, [growthPlan], resellerAdmin)
    await page.goto('/resellers')

    await expect(page.getByTestId('workspace-license-row')).toContainText(
      'Omnitech Growth',
    )
    await expect(page.getByTestId('workspace-license-panel')).toContainText(
      'License changes are reserved for the platform owner',
    )
    await expect(page.getByTestId('workspace-license-submit')).toHaveCount(0)
    expect(state.catalogRequests()).toBe(0)
  })

  test('fails closed when the current subscription price cannot be resolved', async ({
    page,
  }) => {
    await mockPartnerPortfolio(
      page,
      [growthPlan],
      platformOwner,
      '99999999-9999-4999-8999-999999999999',
    )
    await page.goto('/resellers')

    await expect(
      page.getByTestId('workspace-license-price-unresolved'),
    ).toContainText('not in this workspace’s assignable catalog')
    await expect(page.getByTestId('workspace-license-plan')).toHaveValue('')
    await page
      .getByTestId('workspace-license-reference')
      .fill('REALIGN-2026-UNRESOLVED')
    await expect(page.getByTestId('workspace-license-submit')).toBeDisabled()
  })
})
