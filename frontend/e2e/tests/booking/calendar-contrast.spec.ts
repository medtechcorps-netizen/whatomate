import { expect, test, type Page } from '@playwright/test'

const organizationId = '11111111-1111-4111-8111-111111111111'
const serviceId = '22222222-2222-4222-8222-222222222222'
const resourceId = '33333333-3333-4333-8333-333333333333'
const eventId = '44444444-4444-4444-8444-444444444444'

function localDateString(value: Date) {
  return [
    value.getFullYear(),
    String(value.getMonth() + 1).padStart(2, '0'),
    String(value.getDate()).padStart(2, '0'),
  ].join('-')
}

async function mockCalendar(page: Page) {
  const user = {
    id: '55555555-5555-4555-8555-555555555555',
    email: 'calendar@example.test',
    full_name: 'Calendar Reviewer',
    organization_id: organizationId,
    organization_name: 'ReReply Test Clinic',
    role: {
      id: '66666666-6666-4666-8666-666666666666',
      name: 'Calendar Reviewer',
      is_system: false,
      permissions: [
        {
          id: 'booking-read',
          resource: 'bookings',
          action: 'read',
        },
        {
          id: 'booking-settings-read',
          resource: 'booking.settings',
          action: 'read',
        },
      ],
    },
  }
  const eventDate = localDateString(new Date())
  const service = {
    id: serviceId,
    name: 'Initial consultation',
    kind: 'appointment',
    duration_minutes: 60,
    default_capacity: 1,
    price_minor: 15000,
    currency: 'MYR',
    is_active: true,
    resource_ids: [resourceId],
    version: 1,
  }
  const resource = {
    id: resourceId,
    name: 'Dr Amina',
    kind: 'practitioner',
    timezone: 'Asia/Kuala_Lumpur',
    location: 'Room 2',
    is_active: true,
    version: 1,
  }
  const event = {
    id: eventId,
    service_id: serviceId,
    resource_id: resourceId,
    starts_at: `${eventDate}T10:00:00+08:00`,
    ends_at: `${eventDate}T11:00:00+08:00`,
    capacity: 1,
    booked_quantity: 0,
    status: 'scheduled',
    location: 'Room 2',
    service,
    resource,
    version: 1,
  }

  await page.addInitScript((mockUser) => {
    window.localStorage.setItem('user', JSON.stringify(mockUser))
    window.localStorage.setItem('color-mode', 'light')
  }, user)

  await page.route('**/api/**', (route) =>
    route.fulfill({ json: { data: {} } }),
  )
  await page.route(/\/api\/me(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: user } }),
  )
  await page.route('**/api/product/entitlements**', (route) =>
    route.fulfill({
      json: {
        data: {
          mode: 'licensed',
          entitlements: { 'bookings.enabled': true },
        },
      },
    }),
  )
  await page.route('**/api/auth/ws-token**', (route) =>
    route.fulfill({ json: { data: { token: '' } } }),
  )
  await page.route('**/api/booking/services**', (route) =>
    route.fulfill({
      json: { data: { services: [service], total: 1 } },
    }),
  )
  await page.route('**/api/booking/resources**', (route) =>
    route.fulfill({
      json: { data: { resources: [resource], total: 1 } },
    }),
  )
  await page.route('**/api/booking/events**', (route) =>
    route.fulfill({
      json: { data: { events: [event], total: 1 } },
    }),
  )
  await page.route('**/api/bookings**', (route) =>
    route.fulfill({
      json: { data: { bookings: [], total: 0 } },
    }),
  )
}

test('appointment card remains readable in light mode', async ({ page }) => {
  await mockCalendar(page)
  await page.goto('/calendar')

  await expect(page.locator('html')).toHaveClass(/light/)
  const eventCard = page.getByTestId('calendar-event-card')
  await expect(eventCard).toBeVisible()
  await expect(eventCard).toHaveCSS('color', 'rgb(17, 24, 39)')
  await expect(page.getByTestId('calendar-event-time')).toHaveCSS('opacity', '1')
  await expect(page.getByTestId('calendar-event-meta')).toHaveCSS('opacity', '1')

  await eventCard.focus()
  await expect(eventCard).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(page.getByText('Attendee desk')).toBeVisible()
})
