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

function workspaceFixture(index: number) {
  const suffix = String(index).padStart(3, '0')
  return {
    ...organization,
    id: `aaaaaaaa-aaaa-4aaa-8aaa-${String(index).padStart(12, '0')}`,
    name: `Workspace ${suffix}`,
    slug: `workspace-${suffix}`,
    created_at: `2026-07-${String((index % 28) + 1).padStart(2, '0')}T08:00:00Z`,
  }
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
  code: 'rereply-growth',
  name: 'ReReply Growth',
  description: 'One customer journey across inbox, CRM and automations',
  vertical: 'general',
  status: 'active',
  trial_days: 14,
  is_public: true,
  entitlements: {
    'omnichannel.enabled': true,
    'crm.enabled': true,
    'bookings.enabled': true,
    'commerce.enabled': true,
    'copilot.enabled': true,
    'threads.public_engagement.enabled': false,
  },
  prices: [
    {
      id: '77777777-7777-4777-8777-777777777777',
      code: 'rereply-growth-myr-year-2026-v1',
      currency: 'MYR',
      unit_amount_minor: 600000,
      setup_amount_minor: 0,
      interval: 'year',
      interval_count: 1,
      tax_behavior: 'exclusive',
      assignable: true,
    },
    {
      id: '88888888-8888-4888-8888-888888888888',
      code: 'rereply-growth-myr-month-2026-v1',
      currency: 'MYR',
      unit_amount_minor: 60000,
      setup_amount_minor: 0,
      interval: 'month',
      interval_count: 1,
      tax_behavior: 'exclusive',
      assignable: true,
    },
  ],
}

const starterPlan = {
  ...growthPlan,
  id: '99999999-9999-4999-8999-999999999991',
  code: 'rereply-starter',
  name: 'ReReply Starter',
  description: 'WhatsApp chatbot, campaigns and customer conversations',
  entitlements: {
    'omnichannel.enabled': false,
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
      code: 'rereply-starter-myr-month-2026-v1',
      unit_amount_minor: 30000,
    },
  ],
}

const legacyGrowthPrice = {
  ...growthPlan.prices[1],
  id: '99999999-9999-4999-8999-999999999995',
  code: 'rereply-growth-myr-month',
  unit_amount_minor: 0,
  assignable: false,
}

const growthPlanWithLegacyPrice = {
  ...growthPlan,
  prices: [legacyGrowthPrice, ...growthPlan.prices],
}

const specialistPlan = {
  ...growthPlan,
  id: '99999999-9999-4999-8999-999999999996',
  code: 'clinic-operations-plus',
  name: 'Clinic Operations Plus',
  description: 'A private clinic operations package',
  is_public: false,
  entitlements: {
    ...growthPlan.entitlements,
    'copilot.enabled': false,
  },
  prices: [
    {
      ...growthPlan.prices[1],
      id: '99999999-9999-4999-8999-999999999997',
      code: 'clinic-operations-plus-myr-month-2026-v1',
      unit_amount_minor: 75000,
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
  await page.route(/\/api\/organizations(?:\?.*)?$/, async (route) => {
    if (route.request().method() !== 'GET') {
      await route.fallback()
      return
    }
    await route.fulfill({
      json: { data: { organizations: portfolioOrganizations } },
    })
  })
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
  maxOrganizations = reseller.max_organizations,
) {
  const currentOrganizations = [...portfolioOrganizations]
  await installWorkspaceSession(page, user, currentOrganizations)

  let deletedOrganizationId = ''
  let createdOrganizationId = ''
  const usagePages: number[] = []
  const usageRequests: Array<{ page: number; limit: number }> = []
  const initialPlan =
    plans.find((plan) =>
      plan.prices.some((price) => price.id === initialPlanPriceId),
    ) ?? growthPlan
  let license = {
    id: '44444444-4444-4444-8444-444444444444',
    plan_id: initialPlan.id,
    plan_price_id: initialPlanPriceId,
    plan_code: initialPlan.code,
    plan_name: initialPlan.name,
    status: initialStatus,
    provider: initialProvider,
    current_period_start: '2026-07-20T08:00:00Z',
    current_period_end: '2026-08-20T08:00:00Z',
    cancel_at_period_end: false,
  }
  let assignment: Record<string, unknown> | null = null
  let catalogRequests = 0
  let subscriptionGetRequests = 0
  let delayNextSubscription = false

  await page.route(/\/api\/organizations(?:\?.*)?$/, async (route) => {
    if (route.request().method() !== 'POST') {
      await route.fallback()
      return
    }
    const request = route.request().postDataJSON() as {
      name: string
      reseller_id: string
    }
    createdOrganizationId = `99999999-0000-4000-8000-${String(currentOrganizations.length + 1).padStart(12, '0')}`
    const createdOrganization = {
      ...organization,
      id: createdOrganizationId,
      reseller_id: request.reseller_id,
      name: request.name,
      slug: request.name
        .trim()
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, '-')
        .replace(/^-|-$/g, ''),
      created_at: '2026-08-01T08:00:00Z',
    }
    currentOrganizations.push(createdOrganization)
    await route.fulfill({ json: { data: createdOrganization } })
  })
  await page.route(/\/api\/resellers(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          resellers: [
            {
              ...reseller,
              max_organizations: maxOrganizations,
              organization_count: currentOrganizations.length,
            },
          ],
        },
      },
    }),
  )
  await page.route(
    new RegExp(`/api/resellers/${reseller.id}/usage(?:\\?.*)?$`),
    async (route) => {
      const requestUrl = new URL(route.request().url())
      const requestedPage = Number(requestUrl.searchParams.get('page') || '1')
      const requestedLimit = Number(requestUrl.searchParams.get('limit') || '50')
      const pageNumber = Number.isInteger(requestedPage)
        ? Math.max(1, requestedPage)
        : 1
      const pageLimit = Number.isInteger(requestedLimit)
        ? Math.min(100, Math.max(1, requestedLimit))
        : 50
      const orderedOrganizations = [...currentOrganizations].sort(
        (left, right) =>
          left.name.localeCompare(right.name) || left.id.localeCompare(right.id),
      )
      const pageStart = (pageNumber - 1) * pageLimit
      usagePages.push(pageNumber)
      usageRequests.push({ page: pageNumber, limit: pageLimit })
      await route.fulfill({
        json: {
          data: {
            reseller_id: reseller.id,
            plan: reseller.plan,
            max_organizations: maxOrganizations,
            organizations: orderedOrganizations
              .slice(pageStart, pageStart + pageLimit)
              .map((item) => ({
                ...item,
                subscription: license,
              })),
            page: pageNumber,
            limit: pageLimit,
            total: orderedOrganizations.length,
            organization_count: orderedOrganizations.length,
            user_count: 4,
            whatsapp_accounts: 1,
            contacts: 12,
            messages: 48,
          },
        },
      })
    },
  )
  await page.route(
    new RegExp(`/api/resellers/${reseller.id}/members(?:\\?.*)?$`),
    (route) => route.fulfill({ json: { data: { members: [] } } }),
  )
  await page.route(
    new RegExp(`/api/admin/organizations/[^/]+/product/plans(?:\\?.*)?$`),
    (route) => {
      catalogRequests += 1
      return route.fulfill({ json: { data: { plans } } })
    },
  )
  await page.route(
    new RegExp(`/api/admin/organizations/[^/]+/subscription(?:\\?.*)?$`),
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
      } else {
        subscriptionGetRequests += 1
        if (delayNextSubscription) {
          delayNextSubscription = false
          await new Promise((resolve) => setTimeout(resolve, 800))
        }
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
    subscriptionGetRequests: () => subscriptionGetRequests,
    createdOrganizationId: () => createdOrganizationId,
    deletedOrganizationId: () => deletedOrganizationId,
    organizations: () => currentOrganizations,
    usagePages: () => [...usagePages],
    usageRequests: () => [...usageRequests],
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
      'ReReply Growth',
    )
    await expect(page.getByTestId('workspace-license-panel')).toContainText(
      'Active',
    )
    await expect(page.getByTestId('workspace-license-plan')).toHaveValue(
      `${growthPlan.id}::${growthPlan.prices[1].id}`,
    )
    expect(state.subscriptionGetRequests()).toBe(0)

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
      'ReReply Growth',
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
    const state = await mockPartnerPortfolio(
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

    await page.goto(`/upgrade-workspace?organization_id=${organization.id}`)
    await expect(page.getByTestId('upgrade-price-unresolved')).toContainText(
      'Nothing will change automatically',
    )
    await expect(page.getByTestId('upgrade-plan-review')).toHaveCount(0)
    await expect(page.getByTestId('upgrade-submit')).toHaveCount(0)
    expect(state.assignment()).toBeNull()

    await page.getByRole('button', { name: 'Choose Growth' }).click()
    await expect(page.getByTestId('upgrade-price-unresolved')).toContainText(
      'You explicitly selected ReReply Growth',
    )
    await expect(page.getByTestId('upgrade-plan-review')).toBeVisible()
    await page.getByTestId('upgrade-reference').fill('REALIGN-2026-REPLACEMENT')
    await expect(page.getByTestId('upgrade-submit')).toBeEnabled()
    await page.getByTestId('upgrade-submit').click()
    await expect.poll(state.assignment).toMatchObject({
      plan_id: growthPlan.id,
      plan_price_id: growthPlan.prices[1].id,
      manual_reference: 'REALIGN-2026-REPLACEMENT',
    })
  })

  test('updates portfolio capacity when the partner tier changes', async ({
    page,
  }) => {
    await mockPartnerPortfolio(page)
    await page.goto('/resellers')

    await page.getByRole('tab', { name: 'Brand & portfolio capacity' }).click()
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
    const refreshButton = page.locator(
      'button[aria-label="Refresh selected workspace license"]',
    )
    const refreshedLicense = page.waitForResponse(
      (response) =>
        response.request().method() === 'GET' &&
        response
          .url()
          .includes(`/api/admin/organizations/${organization.id}/subscription`),
    )
    await refreshButton.click()

    await expect(page.getByTestId('workspace-danger-zone')).toBeVisible()
    await page.getByTestId('workspace-delete-row').click()
    await expect(
      page.getByRole('dialog', { name: `Delete ${organization.name}?` }),
    ).toBeVisible()
    await refreshedLicense
    await expect(refreshButton).toBeEnabled()
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
      .getByTestId('workspace-license-row')
      .filter({ hasText: deletableOrganization.name })
      .getByTestId('workspace-delete-row')
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
    await expect(page.getByTestId('workspace-license-row')).not.toContainText(
      deletableOrganization.name,
    )
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

  test('finds a deep-linked workspace across pages and recovers after deleting the last page', async ({
    page,
  }) => {
    const portfolioOrganizations = Array.from({ length: 51 }, (_, index) =>
      workspaceFixture(index + 1),
    )
    const targetOrganization = portfolioOrganizations[50]
    const state = await mockPartnerPortfolio(
      page,
      [growthPlan],
      platformOwner,
      growthPlan.prices[1].id,
      'manual',
      'active',
      portfolioOrganizations,
      100,
    )

    await page.goto(
      `/resellers?organization_id=${targetOrganization.id}`,
    )

    const pagination = page.getByTestId('workspace-pagination')
    await expect(pagination).toContainText('Showing 51–51 of 51 workspaces')
    await expect(pagination).toContainText('Page 2 of 2')
    await expect(page.getByTestId('workspace-license-row')).toHaveCount(1)
    await expect(page.getByTestId('workspace-license-row')).toContainText(
      targetOrganization.name,
    )
    await expect(page.getByTestId('workspace-license-panel')).toContainText(
      targetOrganization.name,
    )
    await expect(
      page.getByRole('button', { name: 'Previous workspace page' }),
    ).toBeEnabled()
    await expect(
      page.getByRole('button', { name: 'Next workspace page' }),
    ).toBeDisabled()
    expect(state.usageRequests()).toEqual(
      expect.arrayContaining([
        { page: 1, limit: 50 },
        { page: 2, limit: 50 },
      ]),
    )

    await page
      .getByRole('button', { name: 'Previous workspace page' })
      .click()
    await expect(pagination).toContainText('Showing 1–50 of 51 workspaces')
    await expect(pagination).toContainText('Page 1 of 2')
    await expect(page.getByTestId('workspace-license-row')).toHaveCount(50)

    await page.getByRole('button', { name: 'Next workspace page' }).click()
    await expect(pagination).toContainText('Showing 51–51 of 51 workspaces')
    await page
      .getByTestId('workspace-license-row')
      .filter({ hasText: targetOrganization.name })
      .getByTestId('workspace-delete-row')
      .click()
    await page
      .getByTestId('workspace-delete-confirmation')
      .fill(targetOrganization.name)
    await page.getByTestId('workspace-delete-submit').click()

    await expect.poll(state.deletedOrganizationId).toBe(targetOrganization.id)
    await expect(pagination).toContainText('Showing 1–50 of 50 workspaces')
    await expect(pagination).toContainText('Page 1 of 1')
    await expect(page.getByTestId('workspace-license-row')).toHaveCount(50)
    await expect(
      page.getByRole('button', { name: 'Previous workspace page' }),
    ).toBeDisabled()
    await expect(
      page.getByRole('button', { name: 'Next workspace page' }),
    ).toBeDisabled()
    expect(state.organizations()).toHaveLength(50)
    expect(state.usagePages()).toEqual(
      expect.arrayContaining([1, 2]),
    )
  })

  test('opens the page containing a newly provisioned workspace', async ({
    page,
  }) => {
    const portfolioOrganizations = Array.from({ length: 50 }, (_, index) =>
      workspaceFixture(index + 1),
    )
    const state = await mockPartnerPortfolio(
      page,
      [growthPlan],
      platformOwner,
      growthPlan.prices[1].id,
      'manual',
      'active',
      portfolioOrganizations,
      100,
    )
    await page.goto('/resellers')

    const pagination = page.getByTestId('workspace-pagination')
    await expect(pagination).toContainText('Showing 1–50 of 50 workspaces')
    await page.getByRole('button', { name: 'New workspace' }).click()
    const createDialog = page.getByRole('dialog', {
      name: 'Create customer workspace',
    })
    await createDialog.getByLabel('Business name').fill('Workspace 999')
    await createDialog
      .getByRole('button', { name: 'Create workspace' })
      .click()

    await expect.poll(state.createdOrganizationId).not.toBe('')
    const createdOrganizationId = state.createdOrganizationId()
    await expect(page).toHaveURL(
      new RegExp(`organization_id=${createdOrganizationId}`),
    )
    await expect(pagination).toContainText('Showing 51–51 of 51 workspaces')
    await expect(pagination).toContainText('Page 2 of 2')
    await expect(page.getByTestId('workspace-license-row')).toHaveCount(1)
    await expect(page.getByTestId('workspace-license-row')).toContainText(
      'Workspace 999',
    )
    await expect(page.getByTestId('workspace-license-panel')).toContainText(
      'Workspace 999',
    )
    expect(state.organizations()).toHaveLength(51)
    expect(state.usagePages()).toContain(2)
  })

  test('keeps a legacy pilot price visible while selecting its paid replacement', async ({
    page,
  }) => {
    await mockPartnerPortfolio(
      page,
      [starterPlan, growthPlanWithLegacyPrice],
      platformOwner,
      legacyGrowthPrice.id,
    )
    await page.goto('/resellers')

    await expect(
      page.getByTestId('workspace-license-price-unresolved'),
    ).toContainText('retired pilot price')
    await expect(page.getByTestId('workspace-license-plan')).toHaveValue(
      `${growthPlan.id}::${growthPlan.prices[1].id}`,
    )

    await page.goto(`/upgrade-workspace?organization_id=${organization.id}`)
    await expect(page.getByTestId('upgrade-retired-price')).toContainText(
      'no billing change happens automatically',
    )
    await expect(page.getByTestId('workspace-plan-growth')).toContainText(
      /RM\s*600/,
    )
    await expect(
      page
        .getByTestId('workspace-plan-growth')
        .getByRole('button', { name: 'Replacement price selected' }),
    ).toBeVisible()
    await expect(page.getByTestId('upgrade-plan-price')).toHaveValue(
      `${growthPlan.id}::${growthPlan.prices[1].id}`,
    )
  })

  test('compares real workspace benefits and applies the selected plan', async ({
    page,
  }) => {
    const state = await mockPartnerPortfolio(
      page,
      [starterPlan, growthPlan],
      platformOwner,
      starterPlan.prices[0].id,
    )
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
    await expect(comparison).toContainText('ReReply Starter')
    for (const key of ['starter', 'growth', 'sprint', 'enterprise']) {
      await expect(page.getByTestId(`workspace-plan-${key}`)).toBeVisible()
    }
    await expect(page.getByTestId('workspace-plan-starter')).toContainText(
      /RM\s*300/,
    )
    await expect(page.getByTestId('workspace-plan-growth')).toContainText(
      /RM\s*600/,
    )
    await expect(page.getByTestId('workspace-plan-growth')).toContainText(
      'Most popular',
    )
    await expect(page.getByTestId('workspace-plan-growth')).toContainText(
      'Shared inbox for WhatsApp, Instagram, Messenger, email and web chat',
    )
    await expect(page.getByTestId('workspace-plan-sprint')).toContainText(
      /RM\s*900/,
    )
    await expect(page.getByTestId('workspace-plan-sprint')).toContainText(
      'Coming soon',
    )
    await expect(
      page
        .getByTestId('workspace-plan-sprint')
        .getByRole('button', { name: 'Planned launch' }),
    ).toBeDisabled()
    await expect(
      page.getByText('any existing Calling beta access is separate', {
        exact: false,
      }),
    ).toBeVisible()
    await expect(page.getByTestId('workspace-plan-enterprise')).toContainText(
      'Call us',
    )
    await expect(
      page
        .getByTestId('workspace-plan-enterprise')
        .getByRole('link', { name: 'Email sales' }),
    ).toHaveAttribute(
      'href',
      'mailto:medtechcorps@gmail.com?subject=ReReply%20Enterprise%20Plan',
    )
    expect(state.assignment()).toBeNull()

    await page.getByRole('button', { name: 'Choose Growth' }).click()
    await expect(page.getByTestId('upgrade-submit')).toBeDisabled()
    expect(state.assignment()).toBeNull()

    await page.getByTestId('upgrade-reference').fill('RELIVE-2026-GROWTH')
    await page.getByTestId('upgrade-submit').click()

    await expect.poll(state.assignment).toMatchObject({
      plan_id: growthPlan.id,
      plan_price_id: growthPlan.prices[1].id,
      status: 'active',
      manual_reference: 'RELIVE-2026-GROWTH',
    })
    await expect(page.getByText('Current', { exact: true })).toBeVisible()
  })

  test('platform owner can select another assignable catalog plan', async ({
    page,
  }) => {
    const state = await mockPartnerPortfolio(
      page,
      [starterPlan, growthPlan, specialistPlan],
      platformOwner,
      starterPlan.prices[0].id,
    )
    await page.goto(`/upgrade-workspace?organization_id=${organization.id}`)

    await expect(page.getByTestId('upgrade-other-catalog-plan')).toBeVisible()
    await page.getByTestId('upgrade-other-plan').selectOption(specialistPlan.id)

    const review = page.getByTestId('upgrade-plan-review')
    await expect(
      review.getByRole('heading', { name: specialistPlan.name }),
    ).toBeVisible()
    await expect(page.getByTestId('upgrade-plan-price')).toHaveValue(
      `${specialistPlan.id}::${specialistPlan.prices[0].id}`,
    )
    await page.getByTestId('upgrade-reference').fill('REALIGN-2026-SPECIALIST')
    await page.getByTestId('upgrade-submit').click()

    await expect.poll(state.assignment).toMatchObject({
      plan_id: specialistPlan.id,
      plan_price_id: specialistPlan.prices[0].id,
      manual_reference: 'REALIGN-2026-SPECIALIST',
    })
  })

  test('keeps the four-plan comparison usable on a narrow mobile viewport', async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await mockPartnerPortfolio(page, [starterPlan, growthPlan])
    await page.goto(`/upgrade-workspace?organization_id=${organization.id}`)

    for (const key of ['starter', 'growth', 'sprint', 'enterprise']) {
      await expect(page.getByTestId(`workspace-plan-${key}`)).toBeVisible()
    }
    const chooseStarter = page.getByRole('button', {
      name: 'Choose Starter',
    })
    await chooseStarter.focus()
    await expect(chooseStarter).toBeFocused()
    await page.keyboard.press('Enter')

    const review = page.getByTestId('upgrade-plan-review')
    await expect(review).toBeVisible()
    await expect(
      review.getByRole('heading', { name: 'Starter', exact: true }),
    ).toBeVisible()
    await expect(page.getByTestId('upgrade-plan-price')).toHaveValue(
      `${starterPlan.id}::${starterPlan.prices[0].id}`,
    )
    await expect(page.getByTestId('upgrade-reference')).toBeVisible()
    await expect(page.getByTestId('upgrade-submit')).toBeDisabled()
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    )
    expect(overflow).toBeLessThanOrEqual(1)
  })

  test('does not offer manual changes for a provider-managed subscription', async ({
    page,
  }) => {
    const state = await mockPartnerPortfolio(
      page,
      [starterPlan, growthPlan],
      platformOwner,
      growthPlan.prices[1].id,
      'stripe',
      'canceled',
    )
    await page.goto('/resellers')
    await expect(page.getByTestId('workspace-license-panel')).toContainText(
      'Manual plan change unavailable',
    )
    await expect(page.getByTestId('workspace-license-submit')).toHaveCount(0)

    await page.goto(`/upgrade-workspace?organization_id=${organization.id}`)

    await page.getByRole('button', { name: 'Choose Starter' }).click()
    await expect(page.getByText('Manual plan change unavailable')).toBeVisible()
    await expect(
      page.getByText(/Change or cancel it through that billing provider/),
    ).toBeVisible()
    await expect(page.getByTestId('upgrade-submit')).toHaveCount(0)
    expect(state.assignment()).toBeNull()
  })
})
