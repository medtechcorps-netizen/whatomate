import { expect, test, type Page } from '@playwright/test'

const organizationId = '11111111-1111-4111-8111-111111111111'
const id = (value: number) => `00000000-0000-4000-8000-${String(value).padStart(12, '0')}`
const fixedNow = '2026-10-28T12:00:00.000Z'

type ObservedWindow = {
  from: string
  to: string
  page: number
  limit: number
  returnedIds: string[]
}

async function mockCalendar(page: Page) {
  const user = {
    id: id(1),
    email: 'calendar-timezones@example.test',
    full_name: 'Calendar Timezone Reviewer',
    organization_id: organizationId,
    organization_name: 'Calendar Timezone Clinic',
    role: {
      id: id(2),
      name: 'Calendar Reader',
      is_system: false,
      permissions: [
        { id: id(3), resource: 'bookings', action: 'read' },
        { id: id(4), resource: 'booking.settings', action: 'read' },
      ],
    },
  }
  const resources = [
    { id: id(10), name: 'Eastern practitioner', timezone: 'Pacific/Kiritimati' },
    { id: id(11), name: 'Western practitioner', timezone: 'Etc/GMT+12' },
    { id: id(12), name: 'New York practitioner', timezone: 'America/New_York' },
  ].map(resource => ({ ...resource, kind: 'practitioner', is_active: true, version: 1 }))
  const cases = [
    { number: 20, name: 'Monday east boundary', resource: resources[0]!, starts_at: '2026-10-25T10:15:00Z', capacity: 2, booked_quantity: 1 },
    { number: 21, name: 'Sunday west boundary', resource: resources[1]!, starts_at: '2026-11-02T11:45:00Z', capacity: 3, booked_quantity: 2 },
    { number: 22, name: 'Previous Sunday margin', resource: resources[1]!, starts_at: '2026-10-26T11:45:00Z', capacity: 100, booked_quantity: 90 },
    { number: 23, name: 'Next Monday margin', resource: resources[0]!, starts_at: '2026-11-01T10:15:00Z', capacity: 200, booked_quantity: 190 },
    { number: 24, name: 'First repeated hour', resource: resources[2]!, starts_at: '2026-11-01T05:30:00Z', capacity: 1, booked_quantity: 0 },
    { number: 25, name: 'Second repeated hour', resource: resources[2]!, starts_at: '2026-11-01T06:30:00Z', capacity: 1, booked_quantity: 1 },
  ]
  const services = cases.map(item => ({
    id: id(item.number + 100),
    name: item.name,
    kind: 'appointment',
    duration_minutes: 15,
    default_capacity: item.capacity,
    price_minor: 0,
    currency: 'MYR',
    is_active: true,
    resource_ids: [item.resource.id],
    version: 1,
  }))
  const events = cases.map((item, index) => ({
    id: id(item.number),
    service_id: services[index]!.id,
    resource_id: item.resource.id,
    starts_at: item.starts_at,
    ends_at: new Date(Date.parse(item.starts_at) + 15 * 60_000).toISOString(),
    capacity: item.capacity,
    booked_quantity: item.booked_quantity,
    status: 'scheduled',
    service: services[index],
    // Deliberately omit nested Resource: load must use this response's newly
    // fetched resource catalogue, including the first load and navigation.
    version: 1,
  })).sort((left, right) => Date.parse(left.starts_at) - Date.parse(right.starts_at))
  const windows: ObservedWindow[] = []

  await page.addInitScript(mockUser => {
    window.localStorage.setItem('user', JSON.stringify(mockUser))
    window.localStorage.setItem('color-mode', 'light')
  }, user)
  // Same read-only authentication/entitlement setup as calendar-contrast.spec.
  await page.route('**/api/**', route => route.fulfill({ json: { data: {} } }))
  await page.route(/\/api\/me(?:\?.*)?$/, route => route.fulfill({ json: { data: user } }))
  await page.route('**/api/product/entitlements**', route => route.fulfill({
    json: { data: { mode: 'licensed', entitlements: { 'bookings.enabled': true } } },
  }))
  await page.route('**/api/auth/ws-token**', route => route.fulfill({ json: { data: { token: '' } } }))
  await page.route('**/api/booking/services**', route => route.fulfill({
    json: { data: { services, total: services.length } },
  }))
  await page.route('**/api/booking/resources**', route => route.fulfill({
    json: { data: { resources, total: resources.length } },
  }))
  await page.route('**/api/booking/events**', async route => {
    const request = route.request()
    expect(request.method()).toBe('GET')
    const query = new URL(request.url()).searchParams
    const from = query.get('from') ?? ''
    const to = query.get('to') ?? ''
    const fromTime = Date.parse(from)
    const toTime = Date.parse(to)
    const requestedPage = Number(query.get('page'))
    const limit = Number(query.get('limit'))
    expect(Number.isFinite(fromTime)).toBe(true)
    expect(Number.isFinite(toTime)).toBe(true)
    expect(fromTime).toBeLessThanOrEqual(toTime)
    expect(Number.isInteger(requestedPage) && requestedPage >= 1).toBe(true)
    expect(Number.isInteger(limit) && limit >= 1 && limit <= 100).toBe(true)
    // Match ListBookingEvents exactly: inclusive starts_at bounds, ascending
    // start order, then normal offset pagination. Never return all fixtures
    // regardless of bounds: the old browser-local query must lose an edge.
    const matching = events.filter(event => fromTime <= Date.parse(event.starts_at) && Date.parse(event.starts_at) <= toTime)
    const returned = matching.slice((requestedPage - 1) * limit, requestedPage * limit)
    windows.push({ from, to, page: requestedPage, limit, returnedIds: returned.map(event => event.id) })
    await route.fulfill({ json: { data: { events: returned, total: matching.length } } })
  })
  await page.route('**/api/bookings**', route => route.fulfill({ json: { data: { bookings: [], total: 0 } } }))
  return { windows, allEventIds: events.map(event => event.id) }
}

for (const timezoneId of ['Pacific/Kiritimati', 'Etc/GMT+12']) {
  test.describe(`Calendar week in browser ${timezoneId}`, () => {
    test.use({ timezoneId })

    test('keeps opposite resource boundaries and repeated hour without counting adjacent-week margins', async ({ page }) => {
      await page.clock.install({ time: new Date(fixedNow) })
      const { windows, allEventIds } = await mockCalendar(page)
      await page.goto('/calendar')
      expect(await page.evaluate(() => Intl.DateTimeFormat().resolvedOptions().timeZone)).toBe(timezoneId)

      const cards = page.getByTestId('calendar-event-card')
      const namedCard = (name: string) => cards.filter({ hasText: name })
      const assertDisplayedWeek = async () => {
        await expect(cards).toHaveCount(4)
        await expect(namedCard('Monday east boundary')).toBeVisible()
        await expect(namedCard('Sunday west boundary')).toBeVisible()
        await expect(namedCard('Previous Sunday margin')).toHaveCount(0)
        await expect(namedCard('Next Monday margin')).toHaveCount(0)
        await expect(page.getByText('4 schedules', { exact: true })).toBeVisible()
        await expect(page.getByText('4/7 seats booked', { exact: true })).toBeVisible()
        const firstHour = namedCard('First repeated hour').getByTestId('calendar-event-time')
        const secondHour = namedCard('Second repeated hour').getByTestId('calendar-event-time')
        await expect(firstHour).toBeVisible()
        await expect(secondHour).toBeVisible()
        expect(await firstHour.innerText()).toBe(await secondHour.innerText())
        await expect(firstHour).toContainText('01:30')
      }
      await assertDisplayedWeek()
      expect(windows).toHaveLength(1)
      expect(windows[0]).toMatchObject({ from: '2026-10-25T00:00:00.000Z', to: '2026-11-02T23:59:59.999Z', page: 1 })
      expect([...windows[0]!.returnedIds].sort()).toEqual([...allEventIds].sort())

      await page.getByRole('button', { name: 'Next week', exact: true }).click()
      await expect(cards).toHaveCount(1)
      await expect(namedCard('Next Monday margin')).toBeVisible()
      await expect(page.getByText('190/200 seats booked', { exact: true })).toBeVisible()
      expect(windows).toHaveLength(2)
      expect(windows[1]).toMatchObject({ from: '2026-11-01T00:00:00.000Z', to: '2026-11-09T23:59:59.999Z', page: 1 })

      await page.getByRole('button', { name: 'Previous week', exact: true }).click()
      await assertDisplayedWeek()
      expect(windows).toHaveLength(3)

      await page.getByRole('button', { name: 'Previous week', exact: true }).click()
      await expect(cards).toHaveCount(1)
      await expect(namedCard('Previous Sunday margin')).toBeVisible()
      await expect(page.getByText('90/100 seats booked', { exact: true })).toBeVisible()
      expect(windows).toHaveLength(4)
      expect(windows[3]).toMatchObject({ from: '2026-10-18T00:00:00.000Z', to: '2026-10-26T23:59:59.999Z', page: 1 })

      await page.getByRole('button', { name: 'Today', exact: true }).click()
      await assertDisplayedWeek()
      expect(windows).toHaveLength(5)
      expect(windows[4]).toMatchObject({ from: '2026-10-25T00:00:00.000Z', to: '2026-11-02T23:59:59.999Z', page: 1 })
    })
  })
}
