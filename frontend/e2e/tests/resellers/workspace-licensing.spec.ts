import { expect, test, type Page } from '@playwright/test'

const organization = {
  id: '11111111-1111-4111-8111-111111111111',
  reseller_id: '22222222-2222-4222-8222-222222222222',
  reseller_name: 'Omnitech Partner',
  name: 'ReAlign Kajang',
  slug: 'realign-kajang',
  created_at: '2026-07-20T08:00:00Z',
}

const deletableOrganization = {
  ...organization,
  id: '77777777-7777-4777-8777-777777777777',
  name: 'Klinik Archive',
  slug: 'klinik-archive',
  created_at: '2026-07-21T08:00:00Z',
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

const starterPlan = {
  ...growthPlan,
  id: '99999999-9999-4999-8999-999999999991',
  code: 'omnitech-starter',
  name: 'Omnitech Starter',
  description: 'Core messaging with a shared omnichannel inbox',
  entitlements: {
    'omnichannel.enabled': true,
    'crm.enabled': false,
    'bookings.enabled': false,
    'commerce.enabled': false,
    'copilot.enabled': false,
    'threads.public_engagement.enabled': false,
  },
  prices: [
    {
      ...growthPlan.prices[1],
      id: '99999999-9999-4999-8999-999999999992',
      code: 'omnitech-starter-myr-month',
      unit_amount_minor: 9900,
    },
  ],
}

const enterprisePlan = {
  ...growthPlan,
  id: '99999999-9999-4999-8999-999999999993',
  code: 'omnitech-enterprise',
  name: 'Omnitech Enterprise',
  description: 'Complete customer operations and AI workspace',
  entitlements: {
    'omnichannel.enabled': true,
    'crm.enabled': true,
    'bookings.enabled': true,
    'commerce.enabled': true,
    'copilot.enabled': true,
    'threads.public_engagement.enabled': true,
  },
  prices: [
    {
      ...growthPlan.prices[1],
      id: '99999999-9999-4999-8999-999999999994',
      code: 'omnitech-enterprise-myr-month',
      unit_amount_minor: 69900,
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

async function installWorkspaceSession(
  page: Page,
  user = platformOwner,
  portfolioOrganizations: (typeof organization)[] = [organization],
) {
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
            ...portfolioOrganizations.map((item, index) => ({
              organization_id: item.id,
              name: item.name,
              slug: item.slug,
              role_name: 'Platform Owner',
              is_default: index === 0,
            })),
          ],
        },
      },
    }),
  )
  await page.route(/\/api\/organizations(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: { data: { organizations: portfolioOrganizations } },
    }),
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
  initialProvider = 'manual',
  initialStatus = 'active',
  portfolioOrganizations: (typeof organization)[] = [organization],
) {
  const currentOrganizations = [...portfolioOrganizations]
  await installWorkspaceSession(page, user, currentOrganizations)

  let deletedOrganizationId = ''
  let license = {
    id: '44444444-4444-4444-8444-444444444444',
    plan_id: growthPlan.id,
    plan_price_id: initialPlanPriceId,
    plan_code: growthPlan.code,
    plan_name: growthPlan.name,
    status: initialStatus,
    provider: initialProvider,
    current_period_start: '2026-07-20T08:00:00Z',
    current_period_end: '2026-08-20T08:00:00Z',
    cancel_at_period_end: false,
  }
  let assignment: Record<string, unknown> | null = null
  let catalogRequests = 0
  let delayNextSubscription = false

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
            organizations: currentOrganizations,
            organization_count: currentOrganizations.length,
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
      `/api/admin/organizations/[^/]+/subscription(?:\\?.*)?$`,
    ),
    async (route) => {
      if (route.request().method() === 'PUT') {
        assignment = route.request().postDataJSON() as Record<string, unknown>
        const assignedPlan =
          plans.find((plan) => plan.id === assignment?.plan_id) ?? growthPlan
        license = {
          ...license,
          plan_id: String(assignment.plan_id),
          plan_price_id: String(assignment.plan_price_id),
          plan_code: assignedPlan.code,
          plan_name: assignedPlan.name,
          status: String(assignment.status),
          current_period_end: '2026-08-18T08:00:00Z',
          ...(assignment.status === 'trialing'
            ? { trial_ends_at: '2026-08-18T08:00:00Z' }
            : {}),
        }
      } else if (delayNextSubscription) {
        delayNextSubscription = false
        await new Promise((resolve) => setTimeout(resolve, 800))
      }
      await route.fulfill({ json: { data: license } })
    },
  )
  await page.route(/\/api\/organizations\/[^/?]+(?:\?.*)?$/, async (route) => {
    if (route.request().method() !== 'DELETE') {
      await route.fallback()
      return
    }
    deletedOrganizationId = new URL(route.request().url()).pathname
      .split('/')
      .pop()!
    const deletedIndex = currentOrganizations.findIndex(
      (item) => item.id === deletedOrganizationId,
    )
    if (deletedIndex >= 0) currentOrganizations.splice(deletedIndex, 1)
    await route.fulfill({
      json: {
        data: {
          message: 'Organization deleted',
          organization_id: deletedOrganizationId,
          recoverable: true,
        },
      },
    })
  })

  return {
    assignment: () => assignment,
    catalogRequests: () => catalogRequests,
    deletedOrganizationId: () => deletedOrganizationId,
    organizations: () => currentOrganizations,
    delayNextSubscription: () => {
      delayNextSubscription = true
    },
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
    await expect(
      page.getByRole('link', { name: 'Upgrade workspace' }),
    ).toHaveCount(0)
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

  test('updates portfolio capacity when the partner tier changes', async ({
    page,
  }) => {
    await mockPartnerPortfolio(page)
    await page.goto('/resellers')

    await page
      .getByRole('tab', { name: 'Brand & portfolio capacity' })
      .click()
    await page.locator('#partner-plan').selectOption('enterprise')
    await expect(page.locator('#org-limit')).toHaveValue('1000')
    await expect(page.getByText(/does not change feature access/)).toBeVisible()
  })

  test('keeps organization deletion visible and stable while licensing refreshes', async ({
    page,
  }) => {
    const state = await mockPartnerPortfolio(page)
    await page.goto('/resellers')

    await expect(page.getByTestId('workspace-danger-zone')).toBeVisible()
    state.delayNextSubscription()
    await page
      .getByRole('button', { name: 'Refresh selected workspace license' })
      .click()

    await expect(page.getByTestId('workspace-danger-zone')).toBeVisible()
    await page.getByTestId('workspace-delete-row').click()
    await expect(
      page.getByRole('dialog', { name: `Delete ${organization.name}?` }),
    ).toBeVisible()
    await page.waitForTimeout(900)
    await expect(
      page.getByRole('dialog', { name: `Delete ${organization.name}?` }),
    ).toBeVisible()
    await expect(page.getByTestId('workspace-delete-submit')).toBeDisabled()
  })

  test('deletes the exact confirmed non-active organization and refreshes the list', async ({
    page,
  }) => {
    const state = await mockPartnerPortfolio(
      page,
      [growthPlan],
      platformOwner,
      growthPlan.prices[1].id,
      'manual',
      'active',
      [organization, deletableOrganization],
    )
    await page.goto('/resellers')

    await page
      .getByRole('button', { name: `Delete ${deletableOrganization.name}` })
      .click()
    await expect(
      page.getByRole('dialog', {
        name: `Delete ${deletableOrganization.name}?`,
      }),
    ).toBeVisible()
    await page
      .getByTestId('workspace-delete-confirmation')
      .fill(deletableOrganization.name)
    await expect(page.getByTestId('workspace-delete-submit')).toBeEnabled()
    await page.getByTestId('workspace-delete-submit').click()

    await expect
      .poll(state.deletedOrganizationId)
      .toBe(deletableOrganization.id)
    await expect(page.getByTestId('workspace-license-row')).toHaveCount(1)
    await expect(
      page.getByTestId('workspace-license-row'),
    ).not.toContainText(deletableOrganization.name)
    expect(state.organizations()).toHaveLength(1)

    const organizationSwitcher = page
      .getByRole('navigation', { name: 'Main navigation' })
      .getByRole('combobox')
      .first()
    await organizationSwitcher.click()
    await expect(
      page.getByRole('option', { name: organization.name }),
    ).toBeVisible()
    await expect(
      page.getByRole('option', { name: deletableOrganization.name }),
    ).toHaveCount(0)
  })

  test('compares real workspace benefits and applies the selected plan', async ({
    page,
  }) => {
    const state = await mockPartnerPortfolio(page, [
      starterPlan,
      growthPlan,
      enterprisePlan,
    ])
    await page.goto('/resellers')
    const upgradeLink = page.getByRole('link', { name: 'Upgrade workspace' })
    await expect(upgradeLink).toHaveAttribute(
      'href',
      `/upgrade-workspace?organization_id=${organization.id}`,
    )
    await upgradeLink.click()

    await expect(
      page.getByRole('heading', { name: 'Upgrade workspace' }),
    ).toBeVisible()
    const comparison = page.getByLabel('Workspace plan comparison')
    await expect(
      comparison.getByRole('heading', { name: 'Omnitech Starter' }),
    ).toBeVisible()
    await expect(
      comparison.getByRole('heading', { name: 'Omnitech Growth' }),
    ).toBeVisible()
    await expect(
      comparison.getByRole('heading', { name: 'Omnitech Enterprise' }),
    ).toBeVisible()
    await expect(
      page.getByText('CRM pipeline, tasks, insights and automations').last(),
    ).toBeVisible()
    await expect(
      page.getByText('Packages, invoices and payment tracking').last(),
    ).toBeVisible()

    await page
      .getByRole('button', { name: 'Choose Omnitech Enterprise' })
      .click()
    await expect(page.getByTestId('upgrade-submit')).toBeDisabled()
    expect(state.assignment()).toBeNull()

    await page.getByTestId('upgrade-reference').fill('RELIVE-2026-ENTERPRISE')
    await page.getByTestId('upgrade-submit').click()

    await expect.poll(state.assignment).toMatchObject({
      plan_id: enterprisePlan.id,
      plan_price_id: enterprisePlan.prices[0].id,
      status: 'active',
      manual_reference: 'RELIVE-2026-ENTERPRISE',
    })
    await expect(page.getByText('Current', { exact: true })).toBeVisible()
  })

  test('does not offer manual changes for a provider-managed subscription', async ({
    page,
  }) => {
    const state = await mockPartnerPortfolio(
      page,
      [starterPlan, growthPlan, enterprisePlan],
      platformOwner,
      growthPlan.prices[1].id,
      'stripe',
      'canceled',
    )
    await page.goto('/resellers')
    await expect(
      page.getByTestId('workspace-license-panel'),
    ).toContainText('Manual plan change unavailable')
    await expect(page.getByTestId('workspace-license-submit')).toHaveCount(0)

    await page.goto(`/upgrade-workspace?organization_id=${organization.id}`)

    await page
      .getByRole('button', { name: 'Choose Omnitech Enterprise' })
      .click()
    await expect(page.getByText('Manual plan change unavailable')).toBeVisible()
    await expect(
      page.getByText(/Change or cancel it through that billing provider/),
    ).toBeVisible()
    await expect(page.getByTestId('upgrade-submit')).toHaveCount(0)
    expect(state.assignment()).toBeNull()
  })
})
