import { expect, test, type Page, type Route } from '@playwright/test'

const organizationId = '31111111-1111-4111-8111-111111111111'
const pipelineId = '32222222-2222-4222-8222-222222222222'
const firstStageId = '33333333-3333-4333-8333-333333333331'
const secondStageId = '33333333-3333-4333-8333-333333333332'
const leadId = '34444444-4444-4444-8444-444444444444'
const taskId = '35555555-5555-4555-8555-555555555555'

const permission = (resource: string, action = 'read') => ({
  id: `${resource}-${action}`,
  resource,
  action,
})

const crmUser = {
  id: '36666666-6666-4666-8666-666666666666',
  email: 'crm-manager@example.test',
  full_name: 'CRM Manager',
  organization_id: organizationId,
  organization_name: 'Klinik Relive',
  is_super_admin: false,
  is_reseller_admin: false,
  role: {
    id: '37777777-7777-4777-8777-777777777777',
    name: 'crm_manager',
    is_system: false,
    permissions: [
      permission('crm.leads'),
      permission('crm.leads', 'write'),
      permission('crm.leads', 'delete'),
      permission('crm.pipelines'),
      permission('crm.pipelines', 'write'),
      permission('crm.pipelines', 'delete'),
      permission('tasks'),
      permission('tasks', 'write'),
      permission('bookings'),
      permission('bookings', 'write'),
      permission('booking.settings'),
      permission('booking.settings', 'write'),
      permission('contacts'),
    ],
  },
}

const stages = [
  {
    id: firstStageId,
    pipeline_id: pipelineId,
    name: 'New enquiry',
    color: '#67e8f9',
    display_order: 0,
    kind: 'open',
    probability: 10,
    sla_hours: 12,
    is_active: true,
    version: 3,
  },
  {
    id: secondStageId,
    pipeline_id: pipelineId,
    name: 'Qualified',
    color: '#a78bfa',
    display_order: 1,
    kind: 'open',
    probability: 50,
    sla_hours: 24,
    is_active: true,
    version: 2,
  },
]

const pipeline = {
  id: pipelineId,
  name: 'Patient journey',
  description: 'Clinic revenue pipeline',
  is_default: true,
  is_active: true,
  display_order: 0,
  version: 1,
  stages,
}

const lead = {
  id: leadId,
  contact_id: '38888888-8888-4888-8888-888888888888',
  pipeline_id: pipelineId,
  stage_id: firstStageId,
  title: 'Website enquiry',
  status: 'open',
  value_minor: 150000,
  currency: 'MYR',
  next_action_at: '2026-08-03T02:00:00Z',
  expected_close_date: '2026-08-10T00:00:00Z',
  version: 4,
  contact: {
    id: '38888888-8888-4888-8888-888888888888',
    profile_name: 'Aina Hassan',
  },
}

const task = {
  id: taskId,
  title: 'Confirm consultation slot',
  description: 'Call after lunch',
  status: 'open',
  priority: 'high',
  due_at: '2026-08-03T06:00:00Z',
  version: 7,
}

async function installBaseMocks(
  page: Page,
  leadActions = ['read', 'write', 'delete'],
  bookingActions = ['read', 'write'],
) {
  const user = {
    ...crmUser,
    role: {
      ...crmUser.role,
      permissions: [
        ...crmUser.role.permissions.filter(
          ({ resource }) => resource !== 'crm.leads' && resource !== 'booking.settings',
        ),
        ...leadActions.map((action) => permission('crm.leads', action)),
        ...bookingActions.map((action) => permission('booking.settings', action)),
      ],
    },
  }
  await page.addInitScript((user) => {
    window.localStorage.setItem('user', JSON.stringify(user))
    window.localStorage.setItem('locale', 'en')
  }, user)

  await page.route('**/api/**', (route) => {
    throw new Error(
      `Unexpected unmocked API request: ${route.request().method()} ${new URL(route.request().url()).pathname}`,
    )
  })
  await page.route(/\/api\/me(?:\?.*)?$/, (route) => route.fulfill({ json: { data: user } }))
  await page.route(/\/api\/me\/organizations(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: { data: { organizations: [{ id: organizationId, name: 'Synthetic clinic', role: user.role }] } },
    }),
  )
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          mode: 'licensed',
          plan_code: 'rereply-growth',
          entitlements: {
            'crm.enabled': true,
            'bookings.enabled': true,
          },
        },
      },
    }),
  )
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: '' } } }),
  )
}

async function installPipelineReads(page: Page) {
  await page.route(/\/api\/crm\/pipelines(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { pipelines: [pipeline], total: 1 } } }),
  )
  await page.route(/\/api\/crm\/leads(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { leads: [lead], total: 1 } } }),
  )
  await page.route(/\/api\/tasks(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { tasks: [task], total: 1 } } }),
  )
}

test('channel AI booking is default-off and native updates carry only its opt-in', async ({ page }) => {
  await installBaseMocks(page)
  const user = {
    ...crmUser,
    role: { ...crmUser.role, permissions: [...crmUser.role.permissions,
      permission('channel_accounts'), permission('channel_accounts', 'write'),
      permission('conversations'), permission('chatbot.ai', 'write'),
    ] },
  }
  await page.addInitScript(user => localStorage.setItem('user', JSON.stringify(user)), user)
  await page.route(/\/api\/me(?:\?.*)?$/, route => route.fulfill({ json: { data: user } }))
  await page.route(/\/api\/me\/organizations(?:\?.*)?$/, route => route.fulfill({
    json: { data: { organizations: [{ id: organizationId, name: 'Synthetic clinic', role: user.role }] } },
  }))
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, route => route.fulfill({
    json: { data: { mode: 'licensed', plan_code: 'rereply-growth',
      entitlements: { 'crm.enabled': true, 'bookings.enabled': true, 'omnichannel.enabled': true } } },
  }))
  const account = {
    id: '39999999-9999-4999-8999-999999999999', organization_id: organizationId,
    channel: 'whatsapp', provider: 'meta_legacy', name: 'Native booking test', status: 'active',
    external_account_id: 'legacy-account:30000000-0000-4000-8000-000000000001',
    capabilities: { text: true, replies: true },
    config: { legacy_read_only: true, outbound_enabled: false, reply_route: 'chat', ai_booking_enabled: false },
    has_credentials: false, outbox_pending: 0, outbox_failed: 0,
  }
  const updates: unknown[] = []
  await page.route(/\/api\/channel-accounts(?:\?.*)?$/, route => route.fulfill({
    json: { data: { accounts: [account] } },
  }))
  await page.route(/\/api\/channel-accounts\/39999999-9999-4999-8999-999999999999$/, async route => {
    expect(route.request().method()).toBe('PUT')
    const body = route.request().postDataJSON()
    expect(body).toEqual({ ai_booking_enabled: true })
    updates.push(body)
    account.config.ai_booking_enabled = true
    await route.fulfill({ json: { data: account } })
  })
  await page.route(/\/api\/conversations(?:\?.*)?$/, route => route.fulfill({
    json: { data: { conversations: [], total: 0 } },
  }))
  await page.route(/\/api\/conversations\/attention-summary(?:\?.*)?$/, route => route.fulfill({
    json: { data: { unread_conversations: 0 } },
  }))
  await page.route(/\/api\/integrations\/meta\/(?:messenger|instagram)\/onboarding\/status(?:\?.*)?$/, route => route.fulfill({
    json: { data: { configured: false } },
  }))
  await page.goto('/inbox')
  await page.getByText('Channel connections', { exact: true }).click()
  await page.getByRole('button', { name: 'Manage Native booking test', exact: true }).last().click()
  const dialog = page.getByRole('dialog', { name: 'Native booking test' })
  await expect(dialog.getByTestId('channel-ai-booking-enabled')).not.toBeChecked()
  await expect(dialog).toContainText('Connecting AI does not enable booking')
  await expect(dialog.getByText('Connection name', { exact: true })).toHaveCount(0)
  await expect(dialog.getByRole('button', { name: 'Disconnect connection' })).toHaveCount(0)
  await dialog.getByTestId('channel-ai-booking-enabled').check()
  await dialog.getByTestId('channel-ai-booking-save').click()
  await expect(dialog).toHaveCount(0)
  expect(updates).toEqual([{ ai_booking_enabled: true }])
})

test('lead editing keeps the draft open when optimistic concurrency rejects the save', async ({ page }) => {
  await installBaseMocks(page)
  await installPipelineReads(page)
  await page.route(new RegExp(`/api/crm/leads/${leadId}$`), async (route) => {
    expect(route.request().method()).toBe('PUT')
    const request = await route.request().postDataJSON()
    expect(request.version).toBe(4)
    expect(request.expected_close_date).toBe('2026-08-10T00:00:00.000Z')
    await route.fulfill({
      status: 409,
      json: { message: 'CRM lead was modified; refresh and retry' },
    })
  })

  await page.goto('/crm/pipeline')
  await page.getByRole('button', { name: 'Actions for Website enquiry' }).focus()
  await page.keyboard.press('Enter')
  await page.getByRole('menuitem', { name: 'Edit Website enquiry' }).focus()
  await page.keyboard.press('Enter')

  const dialog = page.getByRole('dialog', { name: 'Edit lead' })
  await expect(dialog).toBeVisible()
  const title = dialog.getByLabel('Lead title')
  await expect(title).toBeFocused()
  await title.fill('Website enquiry — revised')
  await dialog.getByRole('button', { name: 'Save changes' }).click()

  await expect(dialog.getByRole('alert')).toContainText('modified; refresh and retry')
  await expect(title).toHaveValue('Website enquiry — revised')
  await expect(dialog.getByRole('button', { name: 'Archive', exact: true })).toHaveCount(0)
  await expect(dialog.getByRole('button', { name: 'Save changes' })).toBeEnabled()
})

test('lead archival is confirmed, versioned, and remains recoverable', async ({ page }) => {
  await installBaseMocks(page)
  await installPipelineReads(page)
  let archiveRequest: Record<string, unknown> | undefined
  let archived = false
  await page.route(/\/api\/crm\/leads(?:\?.*)?$/, async (route) => {
    const showingArchived = new URL(route.request().url()).searchParams.get('status') === 'archived'
    const leads =
      showingArchived === archived
        ? [{ ...lead, status: archived ? 'archived' : 'open', version: archived ? 5 : 4 }]
        : []
    await route.fulfill({ json: { data: { leads, total: leads.length } } })
  })
  await page.route(new RegExp(`/api/crm/leads/${leadId}/archive$`), async (route) => {
    archiveRequest = await route.request().postDataJSON()
    archived = true
    await route.fulfill({
      json: { data: { ...lead, status: 'archived', version: 5 } },
    })
  })

  await page.goto('/crm/pipeline')
  await page.getByRole('button', { name: 'Actions for Website enquiry' }).click()
  await page.getByRole('menuitem', { name: 'Archive Website enquiry' }).click()
  await expect(page.getByRole('dialog', { name: 'Edit lead' })).toHaveCount(0)
  const confirmation = page.getByRole('alertdialog', {
    name: 'Archive this lead?',
  })
  await expect(confirmation.getByRole('button', { name: 'Keep as is' })).toBeFocused()
  await confirmation.getByLabel('Reason (optional)').fill('Duplicate enquiry confirmed by the customer')
  await confirmation.getByRole('button', { name: 'Archive lead' }).click()

  await expect
    .poll(() => archiveRequest)
    .toMatchObject({
      version: 4,
      reason: 'Duplicate enquiry confirmed by the customer',
      metadata: { source: 'crm_pipeline' },
    })
  expect(archiveRequest?.idempotency_key).toEqual(expect.any(String))
  await expect(page.getByText('Lead archived')).toBeVisible()
  await expect(page.getByText('Website enquiry', { exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: 'Show archived leads' }).click()
  await expect(page.getByText('Website enquiry', { exact: true })).toBeVisible()
  await expect(page.getByText('Aina Hassan', { exact: true })).toBeVisible()
})

test('pending lead save cannot dismiss its dialog or submit twice', async ({ page }) => {
  await installBaseMocks(page)
  await installPipelineReads(page)
  let resolveResponse!: () => void
  const responseAllowed = new Promise<void>((resolve) => {
    resolveResponse = resolve
  })
  let requestCount = 0
  await page.route(new RegExp(`/api/crm/leads/${leadId}$`), async (route) => {
    requestCount += 1
    await responseAllowed
    await route.fulfill({ json: { data: { ...lead, version: 5 } } })
  })
  await page.goto('/crm/pipeline')
  await page.getByRole('button', { name: 'Actions for Website enquiry' }).click()
  await page.getByRole('menuitem', { name: 'Edit Website enquiry' }).click()
  const dialog = page.getByRole('dialog', { name: 'Edit lead' })
  try {
    await dialog.getByRole('button', { name: 'Save changes' }).click()
    await expect.poll(() => requestCount).toBe(1)
    await page.keyboard.press('Escape')
    await expect(dialog).toBeVisible()
    await dialog.getByRole('button', { name: 'Close', exact: true }).click()
    await expect(dialog).toBeVisible()
    await expect(dialog.getByRole('button', { name: 'Save changes' })).toBeDisabled()
    expect(requestCount).toBe(1)
  } finally {
    resolveResponse()
  }
  await expect(dialog).toBeHidden()
  await expect(page.getByText('Lead updated', { exact: true })).toBeVisible()
})

test('archived leads can be reviewed and reopened without losing history', async ({ page }) => {
  await installBaseMocks(page, ['read', 'write'])
  await installPipelineReads(page)
  const archivedLead = { ...lead, status: 'archived', version: 9 }
  let reopened = false
  await page.route(/\/api\/crm\/leads(?:\?.*)?$/, async (route) => {
    const requestURL = new URL(route.request().url())
    const leads =
      requestURL.searchParams.get('status') === 'archived' ? (reopened ? [] : [archivedLead]) : [lead]
    await route.fulfill({ json: { data: { leads, total: leads.length } } })
  })
  let reopenRequest: Record<string, unknown> | undefined
  await page.route(new RegExp(`/api/crm/leads/${leadId}/reopen$`), async (route) => {
    reopenRequest = await route.request().postDataJSON()
    reopened = true
    await route.fulfill({
      json: { data: { ...lead, status: 'open', version: 10 } },
    })
  })

  await page.goto('/crm/pipeline')
  await page.getByRole('button', { name: 'Show archived leads' }).click()
  await page.getByRole('button', { name: 'Actions for Website enquiry' }).click()
  await page.getByRole('menuitem', { name: 'Edit Website enquiry' }).click()
  const dialog = page.getByRole('dialog', { name: 'Review archived lead' })
  await expect(dialog.getByLabel('Lead title')).toBeDisabled()
  await dialog.getByRole('button', { name: 'Reopen' }).click()
  const confirmation = page.getByRole('alertdialog', {
    name: 'Reopen this lead?',
  })
  await confirmation.getByLabel('Reason (optional)').fill('Customer asked to continue')
  await confirmation.getByRole('button', { name: 'Reopen lead' }).click()

  await expect
    .poll(() => reopenRequest)
    .toMatchObject({
      version: 9,
      reason: 'Customer asked to continue',
      metadata: { source: 'crm_pipeline' },
    })
  await expect(page.getByText('Lead reopened')).toBeVisible()
  await expect(page.getByText('Website enquiry', { exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: 'Show active leads' }).click()
  await expect(page.getByText('Website enquiry', { exact: true })).toBeVisible()
  await expect(page.getByText('Aina Hassan', { exact: true })).toBeVisible()
})

for (const { actions, edit, archive } of [
  { actions: ['read'], edit: false, archive: false },
  { actions: ['read', 'write'], edit: true, archive: false },
  { actions: ['read', 'delete'], edit: false, archive: true },
  { actions: ['read', 'write', 'delete'], edit: true, archive: true },
]) {
  test(`lead card actions respect independent permissions: ${actions.join('+')}`, async ({ page }) => {
    await installBaseMocks(page, actions)
    await installPipelineReads(page)
    await page.goto('/crm/pipeline')
    await expect(page.getByText('Website enquiry', { exact: true })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Show archived leads' })).toBeVisible()
    const trigger = page.getByRole('button', {
      name: 'Actions for Website enquiry',
    })
    if (!edit && !archive) {
      await expect(trigger).toHaveCount(0)
      return
    }
    await trigger.click()
    await expect(page.getByRole('menuitem', { name: 'Edit Website enquiry' })).toHaveCount(edit ? 1 : 0)
    await expect(page.getByRole('menuitem', { name: 'Archive Website enquiry' })).toHaveCount(archive ? 1 : 0)
  })
}

test('delete-only lead access can cancel archive but cannot edit or reopen', async ({ page }) => {
  await installBaseMocks(page, ['read', 'delete'])
  await installPipelineReads(page)
  const mutationRequests: string[] = []
  await page.route(new RegExp(`/api/crm/leads/${leadId}/(?:archive|reopen)$`), async (route) => {
    mutationRequests.push(route.request().url())
    await route.fulfill({
      status: 500,
      json: { message: 'Unexpected mutation' },
    })
  })
  await page.route(/\/api\/crm\/leads(?:\?.*)?$/, async (route) => {
    const archived = new URL(route.request().url()).searchParams.get('status') === 'archived'
    await route.fulfill({
      json: {
        data: {
          leads: [{ ...lead, status: archived ? 'archived' : 'open' }],
          total: 1,
        },
      },
    })
  })
  await page.goto('/crm/pipeline')
  await page.getByRole('button', { name: 'Actions for Website enquiry' }).click()
  await page.getByRole('menuitem', { name: 'Archive Website enquiry' }).click()
  const confirmation = page.getByRole('alertdialog', {
    name: 'Archive this lead?',
  })
  await confirmation.getByRole('button', { name: 'Keep as is' }).click()
  await expect(confirmation).toBeHidden()
  await expect(page.getByRole('button', { name: 'Actions for Website enquiry' })).toBeFocused()
  expect(mutationRequests).toEqual([])
  await page.getByRole('button', { name: 'Show archived leads' }).click()
  await expect(page.getByText('Website enquiry', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Actions for Website enquiry' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Reopen', exact: true })).toHaveCount(0)
  expect(mutationRequests).toEqual([])
})

for (const status of [409, 503]) {
  test(`archive ${status} preserves confirmation draft and bound retry identity`, async ({ page }) => {
    await installBaseMocks(page)
    await installPipelineReads(page)
    const requests: Record<string, unknown>[] = []
    await page.route(new RegExp(`/api/crm/leads/${leadId}/archive$`), async (route) => {
      requests.push(await route.request().postDataJSON())
      await route.fulfill({
        status,
        json: { message: 'Archive could not complete; review and retry' },
      })
    })
    await page.goto('/crm/pipeline')
    await page.getByRole('button', { name: 'Actions for Website enquiry' }).click()
    await page.getByRole('menuitem', { name: 'Archive Website enquiry' }).click()
    const confirmation = page.getByRole('alertdialog', {
      name: 'Archive this lead?',
    })
    const reason = confirmation.getByLabel('Reason (optional)')
    await reason.fill('Duplicate enquiry')
    await confirmation.getByRole('button', { name: 'Archive lead' }).click()
    await expect(confirmation.getByRole('alert')).toContainText('review and retry')
    await expect(reason).toHaveValue('Duplicate enquiry')
    await expect(confirmation.getByRole('button', { name: 'Archive lead' })).toBeEnabled()
    await confirmation.getByRole('button', { name: 'Archive lead' }).click()
    await expect.poll(() => requests.length).toBe(2)
    expect(requests[0]).toMatchObject({
      version: 4,
      reason: 'Duplicate enquiry',
      metadata: { source: 'crm_pipeline' },
    })
    expect(requests[0]?.idempotency_key).toEqual(expect.any(String))
    expect(requests[1]).toEqual(requests[0])
    await expect(confirmation).toBeVisible()
    await expect(page.getByText('Lead archived', { exact: true })).toHaveCount(0)
  })
}

test('acknowledged archive is removed even if the board refresh fails', async ({ page }) => {
  await installBaseMocks(page)
  await installPipelineReads(page)
  let archived = false
  await page.route(/\/api\/crm\/leads(?:\?.*)?$/, async (route) => {
    if (archived) {
      await route.fulfill({ status: 503, json: { message: 'Board unavailable' } })
    } else {
      await route.fulfill({ json: { data: { leads: [lead], total: 1 } } })
    }
  })
  await page.route(new RegExp(`/api/crm/leads/${leadId}/archive$`), async (route) => {
    archived = true
    await route.fulfill({ json: { data: { ...lead, status: 'archived', version: 5 } } })
  })
  await page.goto('/crm/pipeline')
  await page.getByRole('button', { name: 'Actions for Website enquiry' }).click()
  await page.getByRole('menuitem', { name: 'Archive Website enquiry' }).click()
  const confirmation = page.getByRole('alertdialog', { name: 'Archive this lead?' })
  await confirmation.getByRole('button', { name: 'Archive lead' }).click()
  await expect(confirmation).toBeHidden()
  await expect(page.getByText('Lead archived, but the board could not be refreshed')).toBeVisible()
  await expect(page.getByText('Website enquiry', { exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Actions for Website enquiry' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Refresh pipeline' })).toBeFocused()
})

test('pending reopen retains its confirmation and a failed draft', async ({ page }) => {
  await installBaseMocks(page, ['read', 'write'])
  await installPipelineReads(page)
  await page.route(/\/api\/crm\/leads(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: { data: { leads: [{ ...lead, status: 'archived', version: 9 }], total: 1 } },
    }),
  )
  let allowResponse!: () => void
  const held = new Promise<void>((resolve) => {
    allowResponse = resolve
  })
  let requestCount = 0
  await page.route(new RegExp(`/api/crm/leads/${leadId}/reopen$`), async (route) => {
    requestCount += 1
    await held
    await route.fulfill({ status: 503, json: { message: 'Reopen unavailable' } })
  })
  await page.goto('/crm/pipeline')
  await page.getByRole('button', { name: 'Show archived leads' }).click()
  await page.getByRole('button', { name: 'Actions for Website enquiry' }).click()
  await page.getByRole('menuitem', { name: 'Edit Website enquiry' }).click()
  const dialog = page.getByRole('dialog', { name: 'Review archived lead' })
  await dialog.getByRole('button', { name: 'Reopen', exact: true }).click()
  const confirmation = page.getByRole('alertdialog', { name: 'Reopen this lead?' })
  await confirmation.getByLabel('Reason (optional)').fill('Customer resumed enquiry')
  try {
    await confirmation.getByRole('button', { name: 'Reopen lead' }).click()
    await expect.poll(() => requestCount).toBe(1)
    await page.keyboard.press('Escape')
    await expect(confirmation).toBeVisible()
    await expect(confirmation.getByRole('button', { name: 'Reopen lead' })).toBeDisabled()
    await expect(confirmation.getByRole('button', { name: 'Keep as is' })).toBeDisabled()
    expect(requestCount).toBe(1)
  } finally {
    allowResponse()
  }
  await expect(confirmation.getByRole('alert')).toContainText('Reopen unavailable')
  await expect(confirmation.getByLabel('Reason (optional)')).toHaveValue('Customer resumed enquiry')
  await expect(confirmation.getByRole('button', { name: 'Reopen lead' })).toBeEnabled()
})

test('pipeline configuration reports a stage version conflict without closing', async ({ page }) => {
  await installBaseMocks(page)
  await installPipelineReads(page)
  await page.route(new RegExp(`/api/crm/pipelines/${pipelineId}/stages/${firstStageId}$`), async (route) => {
    expect(route.request().method()).toBe('PUT')
    expect((await route.request().postDataJSON()).version).toBe(3)
    await route.fulfill({
      status: 409,
      json: { message: 'CRM pipeline stage was modified; refresh and retry' },
    })
  })

  await page.goto('/crm/pipeline')
  await page.getByRole('button', { name: 'Configure' }).click()
  const dialog = page.getByRole('dialog', { name: 'Configure pipeline' })
  await expect(dialog).toBeVisible()
  await dialog.getByLabel('Stage name').first().fill('Fresh enquiry')
  await dialog.getByRole('button', { name: 'Save' }).first().click()

  await expect(dialog.getByRole('alert')).toContainText('modified; refresh and retry')
  await expect(dialog.getByLabel('Stage name').first()).toHaveValue('Fresh enquiry')
})

test('lead can be moved with the keyboard on a mobile viewport', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await installBaseMocks(page)
  await installPipelineReads(page)
  let moveRequest: Record<string, unknown> | undefined
  await page.route(new RegExp(`/api/crm/leads/${leadId}/move$`), async (route) => {
    moveRequest = await route.request().postDataJSON()
    await route.fulfill({
      json: {
        data: {
          ...lead,
          stage_id: secondStageId,
          status: 'open',
          version: 5,
        },
      },
    })
  })

  await page.goto('/crm/pipeline')
  const moveButton = page.getByRole('button', {
    name: 'Move Website enquiry to Qualified',
  })
  await moveButton.focus()
  await page.keyboard.press('Enter')

  await expect.poll(() => moveRequest).toEqual({ stage_id: secondStageId, version: 4 })
  await expect(page.getByText('Moved to Qualified')).toBeVisible()
})

test('task edit failure is recoverable and cancellation uses the audited update endpoint', async ({
  page,
}) => {
  await installBaseMocks(page)
  await page.route(/\/api\/tasks(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { tasks: [task], total: 1 } } }),
  )

  let cancellationBody: Record<string, unknown> | undefined
  await page.route(new RegExp(`/api/tasks/${taskId}$`), async (route: Route) => {
    const body = await route.request().postDataJSON()
    if (body.status === 'cancelled') {
      cancellationBody = body
      await route.fulfill({
        json: { data: { ...task, status: 'cancelled', version: 8 } },
      })
      return
    }
    await route.fulfill({
      status: 503,
      json: { message: 'Task service is temporarily unavailable' },
    })
  })

  await page.goto('/crm/tasks')
  const editButton = page.getByRole('button', {
    name: 'Edit Confirm consultation slot',
  })
  await editButton.focus()
  await page.keyboard.press('Enter')
  const dialog = page.getByRole('dialog', { name: 'Edit follow-up' })
  await dialog.getByLabel('Task').fill('Confirm consultation and deposit')
  await dialog.getByRole('button', { name: 'Save changes' }).click()
  await expect(dialog.getByRole('alert')).toContainText('temporarily unavailable')
  await expect(dialog.getByLabel('Task')).toHaveValue('Confirm consultation and deposit')

  await dialog.getByRole('button', { name: 'Cancel follow-up' }).click()
  const confirmation = page.getByRole('alertdialog', {
    name: 'Cancel this follow-up?',
  })
  await confirmation.getByRole('button', { name: 'Cancel follow-up' }).click()
  await expect.poll(() => cancellationBody).toEqual({ version: 7, status: 'cancelled' })
  await expect(dialog).toBeHidden()
})

test('booking setup persists recurring availability and timezone-safe time off', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await installBaseMocks(page)
  const resource = {
    id: '39999999-9999-4999-8999-999999999999',
    name: 'Dr Aina',
    kind: 'practitioner',
    timezone: 'Asia/Kuala_Lumpur',
    is_active: true,
    version: 1,
  }
  await page.route(/\/api\/booking\/services(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { services: [], total: 0 } } }),
  )
  await page.route(/\/api\/booking\/resources(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { resources: [resource], total: 1 } } }),
  )
  await page.route(/\/api\/booking\/events(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { events: [], total: 0 } } }),
  )
  let availabilityRequest: Record<string, unknown> | undefined
  await page.route(
    new RegExp(`/api/booking/resources/${resource.id}/availability-rules(?:\\?.*)?$`),
    async (route) => {
      if (route.request().method() === 'POST') {
        availabilityRequest = await route.request().postDataJSON()
        await route.fulfill({
          json: {
            data: {
              id: crypto.randomUUID(),
              ...availabilityRequest,
              version: 1,
            },
          },
        })
        return
      }
      await route.fulfill({
        json: {
          data: { availability_rules: [], total: 0, page: 1, limit: 100 },
        },
      })
    },
  )
  let timeOffRequest: Record<string, unknown> | undefined
  await page.route(new RegExp(`/api/booking/resources/${resource.id}/time-off(?:\\?.*)?$`), async (route) => {
    if (route.request().method() === 'POST') {
      timeOffRequest = await route.request().postDataJSON()
      await route.fulfill({
        json: {
          data: { id: crypto.randomUUID(), ...timeOffRequest, version: 1 },
        },
      })
      return
    }
    await route.fulfill({
      json: { data: { time_off: [], total: 0, page: 1, limit: 100 } },
    })
  })

  await page.goto('/calendar')
  await page.getByRole('button', { name: 'Service setup' }).click()
  const manager = page.getByTestId('booking-availability-manager')
  await expect(manager).toBeVisible()
  await expect(manager).toContainText('Scheduled events must fit an active weekly window')
  await manager.getByLabel('Availability weekday').selectOption('1')
  await manager.getByLabel('Availability opens').fill('08:30')
  await manager.getByLabel('Availability closes').fill('17:30')
  await manager.getByLabel('Availability effective from').fill('2026-08-01')
  await manager.getByLabel('Availability effective until').fill('2026-12-31')
  await manager.getByRole('button', { name: 'Add window' }).click()
  await expect
    .poll(() => availabilityRequest)
    .toEqual({
      weekday: 1,
      start_local_time: '08:30',
      end_local_time: '17:30',
      effective_from: '2026-08-01',
      effective_until: '2026-12-31',
      is_active: true,
    })

  await manager.getByLabel('Time off starts').fill('2026-08-04T09:00')
  await manager.getByLabel('Time off ends').fill('2026-08-04T17:00')
  await manager.getByLabel('Time off reason').fill('Training day')
  await manager.getByRole('button', { name: 'Add time off' }).click()
  await expect
    .poll(() => timeOffRequest)
    .toEqual({
      starts_at: '2026-08-04T01:00:00.000Z',
      ends_at: '2026-08-04T09:00:00.000Z',
      reason: 'Training day',
    })

  const bounds = await manager.boundingBox()
  expect(bounds?.width).toBeLessThanOrEqual(390)
})

async function installBookingLifecycle(page: Page, actions = ['read', 'write', 'delete']) {
  await installBaseMocks(page, ['read', 'write', 'delete'], actions)
  const service = {
    id: '3aaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
    name: 'Consultation',
    description: 'Retain this description',
    kind: 'appointment',
    duration_minutes: 45,
    buffer_before_minutes: 7,
    buffer_after_minutes: 11,
    default_capacity: 2,
    price_minor: 12345,
    currency: 'MYR',
    is_active: false,
    resource_ids: [],
    metadata: { requires_review: true, nested: { keep: true } },
    reminder_policy: { hours: [24, 2] },
    version: 4,
  }
  const resource = {
    id: '3bbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',
    name: 'Dr Aina',
    kind: 'practitioner',
    timezone: 'Asia/Kuala_Lumpur',
    location: 'Room 2',
    user_id: '3ccccccc-cccc-4ccc-8ccc-cccccccccccc',
    metadata: { nested: { keep: true } },
    is_active: false,
    version: 6,
  }
  const state = {
    service,
    resource,
    serviceDeleted: false,
    resourceDeleted: false,
    rejectDelete: false,
    refreshGate: null as Promise<void> | null,
    requests: [] as Array<{ method: string; path: string; body: Record<string, unknown> }>,
  }
  await page.route(/\/api\/booking\/services(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: { services: state.serviceDeleted ? [] : [service], total: state.serviceDeleted ? 0 : 1 },
      },
    }),
  )
  await page.route(/\/api\/booking\/resources(?:\?.*)?$/, async (route) => {
    await state.refreshGate
    await route.fulfill({
      json: {
        data: { resources: state.resourceDeleted ? [] : [resource], total: state.resourceDeleted ? 0 : 1 },
      },
    })
  })
  await page.route(/\/api\/booking\/events(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { events: [], total: 0 } } }),
  )
  await page.route(
    new RegExp('/api/booking/resources/' + resource.id + '/availability-rules(?:\\?.*)?$'),
    (route) => route.fulfill({ json: { data: { availability_rules: [], total: 0 } } }),
  )
  await page.route(new RegExp('/api/booking/resources/' + resource.id + '/time-off(?:\\?.*)?$'), (route) =>
    route.fulfill({ json: { data: { time_off: [], total: 0 } } }),
  )
  for (const [kind, item] of [
    ['services', service],
    ['resources', resource],
  ] as const) {
    await page.route(new RegExp('/api/booking/' + kind + '/' + item.id + '$'), async (route) => {
      const method = route.request().method()
      expect(['PUT', 'DELETE']).toContain(method)
      expect(route.request().headers()['x-organization-id']).toBe(organizationId)
      const body = route.request().postDataJSON() as Record<string, unknown>
      state.requests.push({ method, path: kind, body })
      if (body.version !== item.version || (method === 'DELETE' && state.rejectDelete)) {
        await route.fulfill({ status: 409, json: { message: 'Record has retained booking history' } })
        return
      }
      if (method === 'PUT') {
        Object.assign(item, body, { version: item.version + 1 })
        await route.fulfill({ json: { data: item } })
        return
      }
      expect(body).toEqual({ version: item.version, confirm_delete: true })
      expect(item.is_active).toBe(false)
      if (kind === 'services') state.serviceDeleted = true
      else state.resourceDeleted = true
      await route.fulfill({ json: { data: { id: item.id, version: item.version + 1, deleted: true } } })
    })
  }
  return state
}

test('mobile Booking catalogue edits preserve inactive state and all service policy fields', async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 })
  const state = await installBookingLifecycle(page)
  const original = JSON.parse(JSON.stringify(state.service))
  await page.goto('/calendar')
  const edit = page.getByRole('button', { name: 'Edit service Consultation', exact: true })
  await expect(edit).toBeVisible()
  const bounds = await edit.boundingBox()
  expect(bounds!.height).toBeGreaterThanOrEqual(44)
  await edit.click()
  await expect(page.getByLabel('Service name', { exact: true })).toBeFocused()
  await page.getByLabel('Service name', { exact: true }).fill('Reviewed consultation')
  await page.getByRole('button', { name: 'Save service', exact: true }).click()
  await expect.poll(() => state.requests.length).toBe(1)
  expect(state.requests[0]).toEqual({
    method: 'PUT',
    path: 'services',
    body: {
      name: 'Reviewed consultation',
      description: original.description,
      kind: original.kind,
      duration_minutes: 45,
      buffer_before_minutes: 7,
      buffer_after_minutes: 11,
      default_capacity: 2,
      price_minor: 12345,
      currency: 'MYR',
      is_active: false,
      resource_ids: [],
      metadata: original.metadata,
      reminder_policy: original.reminder_policy,
      version: 4,
    },
  })
  await expect(page.getByTestId('booking-service-' + state.service.id)).toContainText('Inactive')
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})

test('Booking resource editing, activation and deletion use separate versioned confirmations', async ({
  page,
}) => {
  await page.setViewportSize({ width: 1366, height: 900 })
  const state = await installBookingLifecycle(page)
  await page.goto('/calendar')
  await page.getByRole('button', { name: 'Edit resource Dr Aina', exact: true }).click()
  await page.getByLabel('Resource location', { exact: true }).fill('Room 3')
  await page.getByRole('button', { name: 'Save resource', exact: true }).click()
  await expect.poll(() => state.requests.length).toBe(1)
  expect(state.requests[0]!.body).toMatchObject({
    location: 'Room 3',
    user_id: state.resource.user_id,
    metadata: { nested: { keep: true } },
    is_active: false,
    version: 6,
  })
  await page.getByRole('button', { name: 'Reactivate resource Dr Aina', exact: true }).click()
  const dialog = page.getByRole('alertdialog')
  expect(state.requests).toHaveLength(1)
  await dialog.getByRole('button', { name: 'Reactivate record', exact: true }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByRole('button', { name: 'Delete resource Dr Aina', exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: 'Deactivate resource Dr Aina', exact: true }).click()
  await dialog.getByRole('button', { name: 'Deactivate record', exact: true }).click()
  await expect(dialog).toBeHidden()
  await page.getByRole('button', { name: 'Delete resource Dr Aina', exact: true }).click()
  await dialog.getByRole('button', { name: 'Keep as is', exact: true }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByRole('button', { name: 'Delete resource Dr Aina', exact: true })).toBeFocused()
  expect(state.requests).toHaveLength(3)
  await page.keyboard.press('Enter')
  let releaseRefresh!: () => void
  state.refreshGate = new Promise<void>((resolve) => { releaseRefresh = resolve })
  try {
    await dialog.getByRole('button', { name: 'Delete unused record', exact: true }).click()
    await expect(dialog).toBeHidden()
    const refresh = page.getByRole('button', { name: 'Refresh calendar', exact: true })
    await expect(refresh).toHaveAttribute('aria-disabled', 'true')
    await expect(refresh).toBeFocused()
  } finally {
    releaseRefresh()
  }
  await expect(page.getByTestId('booking-resource-' + state.resource.id)).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Refresh calendar', exact: true })).toBeFocused()
  expect(state.requests[3]).toEqual({
    method: 'DELETE',
    path: 'resources',
    body: { version: 9, confirm_delete: true },
  })
})

test('Booking dependency conflict retains record and confirmation for explicit cancel', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 })
  const state = await installBookingLifecycle(page)
  state.rejectDelete = true
  await page.goto('/calendar')
  await page.getByRole('button', { name: 'Delete service Consultation', exact: true }).click()
  const dialog = page.getByRole('alertdialog')
  await dialog.getByRole('button', { name: 'Delete unused record', exact: true }).click()
  await expect(dialog.getByRole('alert')).toContainText('Record has retained booking history')
  await expect(page.getByTestId('booking-service-' + state.service.id)).toHaveCount(1)
  expect(state.requests).toHaveLength(1)
  await dialog.getByRole('button', { name: 'Keep as is', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Delete service Consultation', exact: true })).toBeFocused()
})

test('Booking read-only role has no catalogue mutation controls', async ({ page }) => {
  await installBookingLifecycle(page, ['read'])
  await page.goto('/calendar')
  await expect(page.getByRole('complementary', { name: 'Services and resources' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Service setup', exact: true })).toHaveCount(0)
  await expect(
    page.getByRole('button', { name: /^(Edit|Delete|Reactivate|Deactivate) (service|resource) / }),
  ).toHaveCount(0)
})
