import { expect, test, type Page } from '@playwright/test'
import { createTestScope, loginAs, loginAsSuperAdmin } from '../framework'

const scope = createTestScope('booking-people-persistence')

async function calendarWrite(
  page: Page,
  origin: string,
  path: string,
  organizationID: string,
  click: () => Promise<void>,
) {
  const [response] = await Promise.all([
    page.waitForResponse(candidate => {
      const url = new URL(candidate.url())
      return url.origin === origin && url.pathname === path && candidate.request().method() === 'POST'
    }),
    click(),
  ])
  expect(response.request().headers()['x-organization-id']).toBe(organizationID)
  return response
}

test('creates a staff schedule from an empty catalogue and reuses the person and service after a real overlap refusal', async ({ page, baseURL }) => {
  const origin = new URL(baseURL!).origin
  // Use the actual application and APIs. Only block traffic beyond this
  // runner's loopback application; pass booking requests to the real server.
  await page.context().route('**/*', route => {
    return new URL(route.request().url()).origin === origin ? route.continue() : route.abort()
  })

  // The runner proves this built application owns the disposable database
  // and preseeds one empty, licensed, synthetic workspace for this sole run.
  const workspace = {
    id: process.env.BOOKING_QUALIFICATION_WORKSPACE_ID!,
    name: process.env.BOOKING_QUALIFICATION_WORKSPACE_NAME!,
  }
  const workspaceHeaders = { 'X-Organization-ID': workspace.id }
  const request = page.context().request
  const get = (path: string) => request.get(new URL(path, origin).href, {
    headers: workspaceHeaders,
    maxRedirects: 0,
  })

  await loginAsSuperAdmin(page)
  if (await page.evaluate(() => localStorage.getItem('selected_organization_id')) !== workspace.id) {
    await page.getByTestId('organization-switcher').click()
    await Promise.all([
      page.waitForEvent('domcontentloaded'),
      page.getByRole('option', { name: workspace.name, exact: true }).click(),
    ])
  }
  await expect.poll(() => page.evaluate(() => localStorage.getItem('selected_organization_id'))).toBe(workspace.id)
  await expect(page.getByTestId('organization-switcher')).toContainText(workspace.name)
  const current = await get('/api/organizations/current')
  expect(current.ok()).toBe(true)
  expect((await current.json()).data).toMatchObject({ id: workspace.id, name: workspace.name })
  const entitlements = await get('/api/product/entitlements')
  expect(entitlements.ok()).toBe(true)
  const { data: access } = await entitlements.json()
  expect(access.mode).toBe('subscription')
  expect(access.entitlements['bookings.enabled']).toBe(true)

  for (const collection of ['resources', 'services', 'events']) {
    const before = await get('/api/booking/' + collection)
    expect(before.ok()).toBe(true)
    expect((await before.json()).data[collection]).toEqual([])
  }

  const [browserResources] = await Promise.all([
    page.waitForResponse(candidate => {
      const url = new URL(candidate.url())
      return url.origin === origin && url.pathname === '/api/booking/resources' &&
        candidate.request().method() === 'GET'
    }),
    page.goto('/calendar'),
  ])
  expect(browserResources.ok()).toBe(true)
  expect(browserResources.request().headers()['x-organization-id']).toBe(workspace.id)
  expect((await browserResources.json()).data.resources).toEqual([])

  await page.getByRole('button', { name: 'Add schedule', exact: true }).click()
  const composer = page.getByRole('region', { name: 'People and scheduling' })
  async function addPerson(name: string, location: string) {
    const form = composer.getByRole('form', { name: 'Add person' })
    await form.getByLabel('Name', { exact: true }).fill(name)
    await form.getByRole('combobox', { name: 'Role', exact: true }).selectOption('staff')
    await form.getByText('Optional details').click()
    await form.getByLabel('Timezone', { exact: true }).fill('Asia/Kuala_Lumpur')
    await form.getByLabel('Location', { exact: true }).fill(location)
    const response = await calendarWrite(page, origin, '/api/booking/resources', workspace.id,
      () => form.getByRole('button', { name: 'Add person', exact: true }).click())
    expect(response.ok()).toBe(true)
    const { data } = await response.json()
    expect(data).toMatchObject({ organization_id: workspace.id, name, location, kind: 'staff', timezone: 'Asia/Kuala_Lumpur' })
    expect(data.id).toBeTruthy()
    return data.id as string
  }

  async function addService(name: string, personID: string) {
    const form = composer.getByRole('form', { name: 'Add service for person' })
    await form.getByLabel('Service name', { exact: true }).fill(name)
    await form.getByLabel('Duration (minutes)', { exact: true }).fill('45')
    await form.getByLabel('Capacity', { exact: true }).fill('3')
    const response = await calendarWrite(page, origin, '/api/booking/services', workspace.id,
      () => form.getByRole('button', { name: 'Add service', exact: true }).click())
    expect(response.ok()).toBe(true)
    expect(response.request().postDataJSON()).toMatchObject({ resource_ids: [personID] })
    const { data } = await response.json()
    expect(data).toMatchObject({ organization_id: workspace.id, name, duration_minutes: 45, default_capacity: 3 })
    expect(data.id).toBeTruthy()
    return data.id as string
  }

  const northRoom = 'North office'
  const southRoom = 'South office'
  const northPerson = await addPerson(scope.name('staff'), northRoom)
  const serviceName = scope.name('appointment')
  const northService = await addService(serviceName, northPerson)
  await composer.getByRole('button', { name: 'Add doctor / instructor / staff', exact: true }).click()
  const southPerson = await addPerson(scope.name('second-staff'), southRoom)
  const southService = await addService(scope.name('second-appointment'), southPerson)
  expect(southPerson).not.toBe(northPerson)
  expect(southService).not.toBe(northService)

  // Keep the same form open after adding the second person. The North event
  // must use its selected person's room even though South was just created.
  const personPicker = composer.getByRole('combobox', { name: 'Person', exact: true })
  await expect(personPicker).toHaveValue(southPerson)
  await personPicker.selectOption(northPerson)

  const date = new Date(Date.now() + 7 * 86_400_000).toISOString().slice(0, 10)
  const csrf = (await page.context().cookies(origin)).find(cookie => cookie.name === 'whm_csrf')
  if (!csrf?.value) throw new Error('The authenticated fixture is missing its CSRF cookie')
  const conflict = await request.post(new URL('/api/booking/events', origin).href, {
    headers: { ...workspaceHeaders, 'X-CSRF-Token': csrf.value },
    maxRedirects: 0,
    data: {
      resource_id: northPerson,
      service_id: northService,
      timezone: 'Asia/Kuala_Lumpur',
      local_starts_at: date + 'T09:15',
      local_ends_at: date + 'T10:00',
      capacity: 3,
      status: 'scheduled',
    },
  })
  expect(conflict.ok()).toBe(true)
  const { data: blockingEvent } = await conflict.json()

  const schedule = composer.getByRole('form', { name: 'Schedule details' })
  await schedule.getByLabel('Date', { exact: true }).fill(date)
  await schedule.getByLabel('Start time', { exact: true }).fill('09:15')
  await expect(schedule.getByLabel('Capacity', { exact: true })).toHaveValue('3')
  const submit = () => schedule.getByRole('button', { name: 'Save schedule', exact: true }).click()

  const refused = await calendarWrite(page, origin, '/api/booking/events', workspace.id, submit)
  expect(refused.status()).toBe(409)
  expect(await refused.text()).toContain('Booking resource already has an overlapping event')
  await expect(composer).toBeVisible()
  await expect(schedule.getByLabel('Start time', { exact: true })).toHaveValue('09:15')
  expect(refused.request().postDataJSON()).toMatchObject({ resource_id: northPerson, service_id: northService })

  await schedule.getByLabel('Start time', { exact: true }).fill('11:15')
  const saved = await calendarWrite(page, origin, '/api/booking/events', workspace.id, submit)
  expect(saved.ok()).toBe(true)
  expect(saved.request().postDataJSON()).toMatchObject({ resource_id: northPerson, service_id: northService, location: northRoom })
  const { data: event } = await saved.json()
  expect(event).toMatchObject({
    organization_id: workspace.id, resource_id: northPerson, service_id: northService,
    status: 'scheduled', capacity: 3, version: 1, location: northRoom,
  })
  expect(event.id).not.toBe(blockingEvent.id)
  expect(new Date(event.starts_at).toISOString()).toBe(date + 'T03:15:00.000Z')
  expect(new Date(event.ends_at).toISOString()).toBe(date + 'T04:00:00.000Z')
  await expect(composer).toBeHidden()
  await expect(page.getByTestId('calendar-event-card').filter({ hasText: serviceName })).toHaveCount(2)

  await page.getByRole('button', { name: 'Add schedule', exact: true }).click()
  await personPicker.selectOption(southPerson)
  await schedule.getByLabel('Date', { exact: true }).fill(date)
  await schedule.getByLabel('Start time', { exact: true }).fill('11:15')
  const southSaved = await calendarWrite(page, origin, '/api/booking/events', workspace.id, submit)
  expect(southSaved.ok()).toBe(true)
  const expectedSouth = { resource_id: southPerson, service_id: southService, location: southRoom }
  expect(southSaved.request().postDataJSON()).toMatchObject(expectedSouth)
  const { data: southEvent } = await southSaved.json()
  expect(southEvent).toMatchObject({ ...expectedSouth, organization_id: workspace.id })
  expect(southEvent.id).toBeTruthy()
  await expect(composer).toBeHidden()

  const persistedResources = await get('/api/booking/resources')
  const persistedServices = await get('/api/booking/services')
  const persistedEvents = await get('/api/booking/events')
  expect(persistedResources.ok()).toBe(true)
  expect(persistedServices.ok()).toBe(true)
  expect(persistedEvents.ok()).toBe(true)
  const { resources } = (await persistedResources.json()).data
  const { services } = (await persistedServices.json()).data
  const { events } = (await persistedEvents.json()).data
  expect(resources).toHaveLength(2)
  expect(resources).toEqual(expect.arrayContaining([
    expect.objectContaining({ id: northPerson, location: northRoom }),
    expect.objectContaining({ id: southPerson, location: southRoom }),
  ]))
  expect(services.map((row: { id: string }) => row.id).sort()).toEqual([northService, southService].sort())
  expect(events).toHaveLength(3)
  expect(events).toEqual(expect.arrayContaining([
    expect.objectContaining({ id: blockingEvent.id, resource_id: northPerson, service_id: northService }),
    expect.objectContaining({ id: event.id, resource_id: northPerson, service_id: northService, location: northRoom }),
    expect.objectContaining({ id: southEvent.id, ...expectedSouth }),
  ]))
})

test('a booking-only user schedules an existing person and service without setup rights', async ({ page, baseURL }) => {
  const scheduler = JSON.parse(process.env.BOOKING_QUALIFICATION_SCHEDULER || 'null') as {
    workspace: { id: string; name: string }
    userID: string
    roleID: string
    email: string
    password: string
    personID: string
    serviceID: string
  } | null
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
  if (!scheduler || !/^E2E-booking-only-[0-9a-f]{24}$/.test(scheduler.workspace?.name) ||
      scheduler.workspace.id === process.env.BOOKING_QUALIFICATION_WORKSPACE_ID ||
      ![scheduler.workspace.id, scheduler.userID, scheduler.roleID, scheduler.personID, scheduler.serviceID].every(id => uuid.test(id)) ||
      !/^e2e-booking-only-[0-9a-f]{24}@example\.test$/.test(scheduler.email) || !/^[0-9a-f]{48}$/.test(scheduler.password)) {
    throw new Error('The runner did not supply the separate synthetic booking-only fixture')
  }

  const origin = new URL(baseURL!).origin
  await page.context().route('**/*', route => {
    return new URL(route.request().url()).origin === origin ? route.continue() : route.abort()
  })
  await loginAs(page, scheduler)

  const organizationID = scheduler.workspace.id
  const headers = { 'X-Organization-ID': organizationID }
  const request = page.context().request
  const get = async (path: string) => {
    const response = await request.get(new URL(path, origin).href, { headers, maxRedirects: 0 })
    expect(response.status()).toBe(200)
    return (await response.json()).data
  }
  const me = await get('/api/me')
  expect(me).toMatchObject({
    id: scheduler.userID, email: scheduler.email, organization_id: organizationID,
    role_id: scheduler.roleID, is_active: true, is_super_admin: false, is_reseller_admin: false,
  })
  expect(me.role.permissions.map((permission: { resource: string; action: string }) =>
    permission.resource + ':' + permission.action).sort()).toEqual(['booking.settings:read', 'bookings:read', 'bookings:write'])
  expect(await get('/api/organizations/current')).toMatchObject(scheduler.workspace)
  expect(await get('/api/product/entitlements')).toMatchObject({ mode: 'subscription', entitlements: { 'bookings.enabled': true } })

  const resourcePath = '/api/booking/resources'
  const servicePath = '/api/booking/services'
  const eventPath = '/api/booking/events'
  const resource = { name: 'Preseeded staff member', kind: 'staff', timezone: 'Asia/Kuala_Lumpur', is_active: true }
  const service = {
    name: 'Preseeded appointment', kind: 'appointment', duration_minutes: 30, default_capacity: 2,
    price_minor: 0, currency: 'MYR', is_active: true, resource_ids: [scheduler.personID],
  }
  const resources = (await get(resourcePath)).resources
  const services = (await get(servicePath)).services
  expect(resources).toMatchObject([{ ...resource, id: scheduler.personID, organization_id: organizationID, version: 1 }])
  expect(services).toMatchObject([{ ...service, id: scheduler.serviceID, organization_id: organizationID, version: 1 }])
  expect((await get(eventPath)).events).toEqual([])

  const csrf = (await page.context().cookies(origin)).find(cookie => cookie.name === 'whm_csrf')
  if (!csrf?.value) throw new Error('The booking-only session is missing its CSRF cookie')
  for (const [path, id, definition] of [
    [resourcePath, scheduler.personID, resource],
    [servicePath, scheduler.serviceID, service],
  ] as const) {
    const deniedCreate = await request.post(new URL(path, origin).href, {
      headers: { ...headers, 'X-CSRF-Token': csrf.value }, maxRedirects: 0,
      data: { ...definition, name: definition.name + ' unauthorized create' },
    })
    expect(deniedCreate.status()).toBe(403)
    expect(await deniedCreate.text()).toContain('Insufficient permissions')
    const deniedUpdate = await request.put(new URL(path + '/' + id, origin).href, {
      headers: { ...headers, 'X-CSRF-Token': csrf.value }, maxRedirects: 0,
      data: { ...definition, name: definition.name + ' unauthorized update', version: 1 },
    })
    expect(deniedUpdate.status()).toBe(403)
    expect(await deniedUpdate.text()).toContain('Insufficient permissions')
  }
  expect((await get(resourcePath)).resources).toEqual(resources)
  expect((await get(servicePath)).services).toEqual(services)

  await page.goto('/calendar')
  await expect(page.getByRole('heading', { name: 'Bookings & classes', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Advanced setup', exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Add doctor / instructor / staff', exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: 'Add schedule', exact: true }).click()

  const composer = page.getByRole('region', { name: 'People and scheduling' })
  await expect(composer.getByText('You can schedule existing people and services.', { exact: false })).toBeVisible()
  await expect(composer.getByRole('form', { name: 'Add person' })).toHaveCount(0)
  await expect(composer.getByRole('form', { name: 'Add service for person' })).toHaveCount(0)
  await composer.getByRole('combobox', { name: 'Person', exact: true }).selectOption(scheduler.personID)
  await composer.getByRole('combobox', { name: 'Service for ' + resource.name, exact: true }).selectOption(scheduler.serviceID)

  const date = new Date(Date.now() + 7 * 86_400_000).toISOString().slice(0, 10)
  const schedule = composer.getByRole('form', { name: 'Schedule details' })
  await schedule.getByLabel('Date', { exact: true }).fill(date)
  await schedule.getByLabel('Start time', { exact: true }).fill('10:30')
  await expect(schedule.getByLabel('Capacity', { exact: true })).toHaveValue('2')
  const saved = await calendarWrite(page, origin, eventPath, organizationID,
    () => schedule.getByRole('button', { name: 'Save schedule', exact: true }).click())
  expect(saved.status()).toBe(200)
  expect(saved.request().postDataJSON()).toMatchObject({
    resource_id: scheduler.personID, service_id: scheduler.serviceID, timezone: resource.timezone,
    local_starts_at: date + 'T10:30', local_ends_at: date + 'T11:00', capacity: 2, status: 'scheduled',
  })
  const { data: event } = await saved.json()
  expect(event.id).toMatch(uuid)
  const expectedEvent = {
    id: event.id, organization_id: organizationID, resource_id: scheduler.personID, service_id: scheduler.serviceID,
    created_by_id: scheduler.userID, updated_by_id: scheduler.userID, capacity: 2, status: 'scheduled', version: 1,
  }
  expect(event).toMatchObject(expectedEvent)
  await expect(composer).toBeHidden()
  await expect(page.getByTestId('calendar-event-card').filter({ hasText: service.name })).toHaveCount(1)

  const persisted = (await get(eventPath)).events
  expect(persisted).toMatchObject([expectedEvent])
  expect(new Date(persisted[0].starts_at).toISOString()).toBe(date + 'T02:30:00.000Z')
  expect(new Date(persisted[0].ends_at).toISOString()).toBe(date + 'T03:00:00.000Z')
})
