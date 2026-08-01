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

async function installBaseMocks(page: Page) {
  await page.addInitScript((user) => {
    window.localStorage.setItem('user', JSON.stringify(user))
    window.localStorage.setItem('locale', 'en')
  }, crmUser)

  await page.route('**/api/**', (route) => route.fulfill({ json: { data: {} } }))
  await page.route(/\/api\/me(?:\?.*)?$/, (route) => route.fulfill({ json: { data: crmUser } }))
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
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) => route.fulfill({ json: { data: { token: '' } } }))
}

async function installPipelineReads(page: Page) {
  await page.route(/\/api\/crm\/pipelines(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { pipelines: [pipeline], total: 1 } } }),
  )
  await page.route(/\/api\/crm\/leads(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { leads: [lead], total: 1 } } }),
  )
  await page.route(/\/api\/tasks(?:\?.*)?$/, (route) => route.fulfill({ json: { data: { tasks: [task], total: 1 } } }))
}

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
  const editButton = page.getByRole('button', { name: 'Edit Website enquiry' })
  await editButton.focus()
  await page.keyboard.press('Enter')

  const dialog = page.getByRole('dialog', { name: 'Edit lead' })
  await expect(dialog).toBeVisible()
  const title = dialog.getByLabel('Lead title')
  await title.fill('Website enquiry — revised')
  await dialog.getByRole('button', { name: 'Save changes' }).click()

  await expect(dialog.getByRole('alert')).toContainText('modified; refresh and retry')
  await expect(title).toHaveValue('Website enquiry — revised')
  await expect(dialog.getByRole('button', { name: 'Archive' })).toBeEnabled()
})

test('lead archival is confirmed, versioned, and remains recoverable', async ({ page }) => {
  await installBaseMocks(page)
  await installPipelineReads(page)
  let archiveRequest: Record<string, unknown> | undefined
  await page.route(new RegExp(`/api/crm/leads/${leadId}/archive$`), async (route) => {
    archiveRequest = await route.request().postDataJSON()
    await route.fulfill({
      json: { data: { ...lead, status: 'archived', version: 5 } },
    })
  })

  await page.goto('/crm/pipeline')
  await page.getByRole('button', { name: 'Edit Website enquiry' }).click()
  const dialog = page.getByRole('dialog', { name: 'Edit lead' })
  await dialog.getByRole('button', { name: 'Archive' }).click()
  const confirmation = page.getByRole('alertdialog', {
    name: 'Archive this lead?',
  })
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
})

test('archived leads can be reviewed and reopened without losing history', async ({ page }) => {
  await installBaseMocks(page)
  await installPipelineReads(page)
  const archivedLead = { ...lead, status: 'archived', version: 9 }
  await page.route(/\/api\/crm\/leads(?:\?.*)?$/, async (route) => {
    const requestURL = new URL(route.request().url())
    const leads = requestURL.searchParams.get('status') === 'archived' ? [archivedLead] : [lead]
    await route.fulfill({ json: { data: { leads, total: leads.length } } })
  })
  let reopenRequest: Record<string, unknown> | undefined
  await page.route(new RegExp(`/api/crm/leads/${leadId}/reopen$`), async (route) => {
    reopenRequest = await route.request().postDataJSON()
    await route.fulfill({
      json: { data: { ...lead, status: 'open', version: 10 } },
    })
  })

  await page.goto('/crm/pipeline')
  await page.getByRole('button', { name: 'Show archived leads' }).click()
  await page.getByRole('button', { name: 'Edit Website enquiry' }).click()
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

test('task edit failure is recoverable and cancellation uses the audited update endpoint', async ({ page }) => {
  await installBaseMocks(page)
  await page.route(/\/api\/tasks(?:\?.*)?$/, (route) => route.fulfill({ json: { data: { tasks: [task], total: 1 } } }))

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
  await page.route(new RegExp(`/api/booking/resources/${resource.id}/availability-rules(?:\\?.*)?$`), async (route) => {
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
  })
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
