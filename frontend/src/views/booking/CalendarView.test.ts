/** @vitest-environment happy-dom */

import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, nextTick, reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { BookingEvent, BookingResource, BookingService } from '@/services/productSuite'
import CalendarView from './CalendarView.vue'

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  put: vi.fn(),
  delete: vi.fn(),
  hasPermission: vi.fn(),
  success: vi.fn(),
  error: vi.fn(),
  warning: vi.fn(),
  organization: 'organization-one',
}))
vi.mock('@/services/api', () => ({
  api: {
    get: mocks.get,
    post: mocks.post,
    put: mocks.put,
    delete: mocks.delete,
  },
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    hasPermission: mocks.hasPermission,
    get organizationId() {
      return mocks.organization
    },
  }),
}))
vi.mock('@/lib/browserIdentity', () => ({
  readSelectedOrganizationId: () => mocks.organization,
}))
vi.mock('@/composables/useAppToast', () => ({
  useAppToast: () => ({
    success: mocks.success,
    error: mocks.error,
    warning: mocks.warning,
  }),
}))

const Passthrough = defineComponent({ template: '<div><slot /></div>' })
const ButtonStub = defineComponent({
  props: { disabled: Boolean, type: String },
  emits: ['click'],
  template:
    '<button :type="type || \'button\'" :disabled="disabled" @click="$emit(\'click\', $event)"><slot /></button>',
})
const AlertStub = defineComponent({
  props: { open: Boolean },
  emits: ['update:open'],
  template: '<section v-if="open" role="alertdialog"><slot /></section>',
})
function mountCalendar() {
  return mount(CalendarView, {
    attachTo: document.body,
    global: {
      stubs: {
        PageHeader: defineComponent({
          template: '<header><slot name="actions" /></header>',
        }),
        Button: ButtonStub,
        Badge: Passthrough,
        ContactPicker: true,
        ResourceAvailabilityPanel: true,
        AlertDialog: AlertStub,
        AlertDialogCancel: ButtonStub,
        AlertDialogContent: Passthrough,
        AlertDialogDescription: Passthrough,
        AlertDialogFooter: Passthrough,
        AlertDialogHeader: Passthrough,
        AlertDialogTitle: Passthrough,
      },
    },
  })
}

let wrapper: ReturnType<typeof mountCalendar> | undefined
let permissions: Set<string>
let services: BookingService[]
let resources: BookingResource[]
function envelope<T>(data: T) {
  return { data: { data } }
}
function copy<T>(data: T): T {
  return JSON.parse(JSON.stringify(data))
}
function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((done, fail) => {
    resolve = done
    reject = fail
  })
  return { promise, resolve, reject }
}
function readFixture(url: string) {
  if (url === '/booking/services') return envelope({ services: copy(services), total: services.length })
  if (url === '/booking/resources') return envelope({ resources: copy(resources), total: resources.length })
  if (url === '/booking/events') return envelope({ events: [], total: 0 })
  throw new Error('Unexpected mocked GET: ' + url)
}
function updateFixture(url: string, payload: Partial<BookingService & BookingResource>) {
  const list = url.includes('/services/') ? services : resources
  const item = list.find((value) => url.endsWith('/' + value.id))
  if (!item) throw new Error('Unexpected mocked PUT: ' + url)
  Object.assign(item, payload, { version: item.version + 1 })
  return envelope(copy(item))
}
function button(text: string) {
  const found = wrapper!.findAll('button').find((item) => item.text().trim() === text)
  if (!found) throw new Error('Missing button: ' + text)
  return found
}
function action(label: string) {
  return wrapper!.get('button[aria-label="' + label + '"]')
}
async function open() {
  wrapper = mountCalendar()
  await flushPromises()
  expect(wrapper.find('[data-testid="booking-service-service-one"]').exists()).toBe(true)
}

beforeEach(() => {
  vi.clearAllMocks()
  mocks.get.mockReset()
  mocks.post.mockReset()
  mocks.put.mockReset()
  mocks.delete.mockReset()
  mocks.organization = 'organization-one'
  permissions = reactive(
    new Set(['booking.settings:read', 'booking.settings:write', 'booking.settings:delete']),
  )
  mocks.hasPermission.mockImplementation((resource: string, verb: string) =>
    permissions.has(resource + ':' + verb),
  )
  services = [
    {
      id: 'service-one',
      name: 'Consultation',
      description: 'Keep description',
      kind: 'appointment',
      duration_minutes: 45,
      buffer_before_minutes: 7,
      buffer_after_minutes: 11,
      default_capacity: 2,
      price_minor: 12345,
      currency: 'MYR',
      is_active: false,
      resource_ids: [],
      version: 4,
      reminder_policy: { hours: [24, 2] },
      metadata: { requires_review: true, clinical: { keep: true } },
    },
  ]
  resources = [
    {
      id: 'resource-one',
      name: 'Dr Aina',
      kind: 'practitioner',
      timezone: 'Asia/Kuala_Lumpur',
      location: 'Room 2',
      user_id: 'staff-one',
      is_active: false,
      version: 6,
      metadata: { specialty: { keep: true } },
    },
  ]
  mocks.get.mockImplementation(readFixture)
  mocks.put.mockImplementation(updateFixture)
  mocks.delete.mockImplementation((url: string, options: { data: { version: number } }) => {
    const id = url.split('/').pop()!
    services = services.filter((value) => value.id !== id)
    resources = resources.filter((value) => value.id !== id)
    return envelope({ id, version: options.data.version + 1, deleted: true })
  })
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
  vi.useRealTimers()
})

describe('Booking catalogue lifecycle', () => {
  it('edits an inactive service without activating or losing buffers, empty links and metadata', async () => {
    const original = copy(services[0]!)
    await open()
    await action('Edit service Consultation').trigger('click')
    await wrapper!.get('#booking-service-name').setValue('Reviewed consultation')
    await wrapper!.get('form[aria-label="Service details"]').trigger('submit')
    await flushPromises()
    expect(mocks.put).toHaveBeenCalledTimes(1)
    expect(mocks.put).toHaveBeenCalledWith(
      '/booking/services/service-one',
      {
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
        version: 4,
        resource_ids: [],
        metadata: original.metadata,
        reminder_policy: original.reminder_policy,
      },
      { headers: { 'X-Organization-ID': 'organization-one' } },
    )
    expect(wrapper!.text()).toContain('Inactive')
  })

  it('edits a resource preserving its linked user, activity, metadata and optimistic version', async () => {
    const original = copy(resources[0]!)
    await open()
    await action('Edit resource Dr Aina').trigger('click')
    await wrapper!.get('#booking-resource-name').setValue('Dr Aina updated')
    await button('Save resource').trigger('click')
    await flushPromises()
    expect(mocks.put).toHaveBeenCalledWith(
      '/booking/resources/resource-one',
      {
        name: 'Dr Aina updated',
        kind: original.kind,
        timezone: original.timezone,
        location: original.location,
        user_id: original.user_id,
        is_active: false,
        metadata: original.metadata,
        version: 6,
      },
      { headers: { 'X-Organization-ID': 'organization-one' } },
    )
  })

  it.each(['service', 'resource'] as const)(
    'reactivates then deactivates %s only through explicit confirmation',
    async (kind) => {
      const name = kind === 'service' ? 'Consultation' : 'Dr Aina'
      const original = copy(kind === 'service' ? services[0]! : resources[0]!)
      await open()
      await action('Reactivate ' + kind + ' ' + name).trigger('click')
      expect(mocks.put).not.toHaveBeenCalled()
      await button('Reactivate record').trigger('click')
      await flushPromises()
      expect(mocks.put.mock.calls[0]![1]).toEqual({
        ...original,
        is_active: true,
      })
      expect(wrapper!.find('button[aria-label="Delete ' + kind + ' ' + name + '"]').exists()).toBe(false)
      await action('Deactivate ' + kind + ' ' + name).trigger('click')
      await button('Deactivate record').trigger('click')
      await flushPromises()
      expect(mocks.put.mock.calls[1]![1]).toEqual({
        ...original,
        is_active: false,
        version: original.version + 1,
      })
    },
  )

  it.each(['service', 'resource'] as const)(
    'deletes inactive %s with exact confirmation, version and organization',
    async (kind) => {
      const id = kind + '-one'
      const name = kind === 'service' ? 'Consultation' : 'Dr Aina'
      await open()
      await action('Delete ' + kind + ' ' + name).trigger('click')
      expect(wrapper!.get('[role="alertdialog"]').text()).toContain(
        'no booking, schedule or purchase history',
      )
      expect(mocks.delete).not.toHaveBeenCalled()
      await button('Delete unused record').trigger('click')
      await flushPromises()
      expect(mocks.delete).toHaveBeenCalledWith('/booking/' + kind + 's/' + id, {
        headers: { 'X-Organization-ID': 'organization-one' },
        data: { version: kind === 'service' ? 4 : 6, confirm_delete: true },
      })
      expect(wrapper!.find('[data-testid="booking-' + kind + '-' + id + '"]').exists()).toBe(false)
    },
  )

  it('read-only and write-only roles cannot delete; delete-only cannot edit or reactivate', async () => {
    permissions = reactive(new Set(['booking.settings:read']))
    await open()
    expect(
      wrapper!.findAll(
        'button[aria-label^="Edit"],button[aria-label^="Delete"],button[aria-label^="Reactivate"]',
      ),
    ).toHaveLength(0)
    permissions.add('booking.settings:write')
    await nextTick()
    expect(wrapper!.findAll('button[aria-label^="Edit"]')).toHaveLength(2)
    expect(wrapper!.findAll('button[aria-label^="Delete"]')).toHaveLength(0)
    permissions.delete('booking.settings:write')
    permissions.add('booking.settings:delete')
    await nextTick()
    expect(wrapper!.findAll('button[aria-label^="Delete"]')).toHaveLength(2)
    expect(wrapper!.findAll('button[aria-label^="Edit"],button[aria-label^="Reactivate"]')).toHaveLength(0)
  })

  it('rechecks a revoked permission before a confirmation can submit', async () => {
    await open()
    await action('Delete service Consultation').trigger('click')
    permissions.delete('booking.settings:delete')
    await nextTick()
    expect(button('Delete unused record').attributes('disabled')).toBeDefined()
    await button('Delete unused record').trigger('click')
    expect(mocks.delete).not.toHaveBeenCalled()
  })

  it('blocks duplicate confirmation, cancellation and other editing while deletion is pending', async () => {
    const pending = deferred<ReturnType<typeof envelope>>()
    mocks.delete.mockReturnValue(pending.promise)
    await open()
    await action('Delete service Consultation').trigger('click')
    await button('Delete unused record').trigger('click')
    await button('Delete unused record').trigger('click')
    expect(mocks.delete).toHaveBeenCalledTimes(1)
    expect(button('Keep as is').attributes('disabled')).toBeDefined()
    expect(action('Edit resource Dr Aina').attributes('disabled')).toBeDefined()
    wrapper!.findComponent(AlertStub).vm.$emit('update:open', false)
    await nextTick()
    expect(wrapper!.find('[role="alertdialog"]').exists()).toBe(true)
    pending.reject(new Error('Used by a schedule'))
    await flushPromises()
    expect(wrapper!.get('[role="alertdialog"]').text()).toContain('Used by a schedule')
    expect(wrapper!.find('[data-testid="booking-service-service-one"]').exists()).toBe(true)
  })

  it('keeps an edit draft and version after a conflict instead of silently retrying', async () => {
    mocks.put.mockRejectedValue({
      response: { status: 409, data: { error: 'Version conflict' } },
    })
    await open()
    await action('Edit service Consultation').trigger('click')
    await wrapper!.get('#booking-service-name').setValue('Unsaved name')
    await wrapper!.get('form[aria-label="Service details"]').trigger('submit')
    await flushPromises()
    expect((wrapper!.get('#booking-service-name').element as HTMLInputElement).value).toBe('Unsaved name')
    expect(wrapper!.get('[role="alert"]').text()).toContain('draft is preserved')
    expect(mocks.put).toHaveBeenCalledTimes(1)
    expect(mocks.put.mock.calls[0]![1].version).toBe(4)
  })

  it('does not resurrect an acknowledged deletion when the following refresh fails', async () => {
    await open()
    await action('Delete service Consultation').trigger('click')
    mocks.get.mockRejectedValue(new Error('Refresh unavailable'))
    await button('Delete unused record').trigger('click')
    await flushPromises()
    expect(wrapper!.find('[data-testid="booking-service-service-one"]').exists()).toBe(false)
    expect(mocks.error).toHaveBeenCalledWith('Calendar could not be loaded', 'Refresh unavailable')
  })

  it('rejects an edit after the selected organization changes', async () => {
    await open()
    await action('Edit service Consultation').trigger('click')
    mocks.organization = 'organization-two'
    await wrapper!.get('form[aria-label="Service details"]').trigger('submit')
    expect(mocks.put).not.toHaveBeenCalled()
    expect(wrapper!.get('[role="alert"]').text()).toContain('organization or permission changed')
  })

  it('does not apply a mutation response after leaving its organization', async () => {
    const pending = deferred<ReturnType<typeof envelope>>()
    mocks.put.mockReturnValue(pending.promise)
    await open()
    await action('Edit resource Dr Aina').trigger('click')
    await button('Save resource').trigger('click')
    mocks.organization = 'organization-two'
    pending.resolve(
      envelope({
        ...resources[0]!,
        name: 'Old organization response',
        version: 7,
      }),
    )
    await flushPromises()
    expect(wrapper!.text()).not.toContain('Old organization response')
    expect(mocks.success).not.toHaveBeenCalled()
  })

  it('accepts only the latest catalogue read when week requests complete out of order', async () => {
    await open()
    const old = deferred<ReturnType<typeof envelope>>()
    mocks.get.mockImplementationOnce(() => old.promise)
    await action('Next week').trigger('click')
    services[0]!.name = 'Fresh catalogue'
    await action('Previous week').trigger('click')
    await flushPromises()
    expect(wrapper!.text()).toContain('Fresh catalogue')
    old.resolve(
      envelope({
        services: [{ ...services[0]!, name: 'Stale catalogue' }],
        total: 1,
      }),
    )
    await flushPromises()
    expect(wrapper!.text()).toContain('Fresh catalogue')
    expect(wrapper!.text()).not.toContain('Stale catalogue')
  })

  it('canceling an edit performs no mutation and creation still defaults to active', async () => {
    mocks.post.mockImplementation((_url: string, body: Partial<BookingResource>) =>
      envelope({ ...body, id: 'new-resource', version: 1 }),
    )
    await open()
    await action('Edit resource Dr Aina').trigger('click')
    await wrapper!.get('#booking-resource-name').setValue('Not saved')
    await button('Cancel resource edit').trigger('click')
    expect(mocks.put).not.toHaveBeenCalled()
    await wrapper!.get('#booking-resource-name').setValue('New room')
    await button('Add resource').trigger('click')
    await flushPromises()
    expect(mocks.post.mock.calls[0]![1]).toMatchObject({
      name: 'New room',
      is_active: true,
    })
    expect(mocks.delete).not.toHaveBeenCalled()
  })
})


describe('Calendar person-first entry points', () => {
  it.each([true, false])('shows a saved next-week schedule using its resource timezone (nested resource: %s)', async (includeResource) => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date('2026-10-26T02:00:00Z'))
    permissions.add('bookings:write')
    services[0]!.is_active = true
    resources[0]!.is_active = true
    resources[0]!.timezone = 'Pacific/Auckland'
    const saved: BookingEvent = {
      id: 'next-week-event', service_id: 'service-one', resource_id: 'resource-one',
      starts_at: '2026-11-02T01:15:00Z', ends_at: '2026-11-02T02:00:00Z',
      capacity: 2, status: 'scheduled', version: 1, service: copy(services[0]!),
      ...(includeResource ? { resource: copy(resources[0]!) } : {}),
    }
    let savedOnServer = false
    mocks.post.mockImplementation(() => {
      savedOnServer = true
      return envelope(saved)
    })
    mocks.get.mockImplementation((url: string, options?: { params?: Record<string, string> }) => {
      if (url !== '/booking/events') return readFixture(url)
      const from = Date.parse(options?.params?.overlap_from || '')
      const to = Date.parse(options?.params?.overlap_to || '')
      const start = Date.parse(saved.starts_at)
      const listed = savedOnServer && from < Date.parse(saved.ends_at) && start < to ? [saved] : []
      return envelope({ events: listed, total: listed.length })
    })
    await open()
    expect(wrapper!.text()).toContain('26 Oct')
    await button('Add schedule').trigger('click')
    await flushPromises()
    await wrapper!.get('[name="schedule-date"]').setValue('2026-11-02')
    await wrapper!.get('[name="schedule-time"]').setValue('14:15')
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    await flushPromises()
    expect(wrapper!.text()).toContain('2 Nov')
    expect(wrapper!.text()).toContain('8 Nov 2026')
    expect(wrapper!.get('[data-testid="calendar-event-card"]').text()).toContain('Consultation')
    expect(wrapper!.get('[data-testid="calendar-event-time"]').text()).toBe(
      new Intl.DateTimeFormat('en-MY', { hour: '2-digit', minute: '2-digit', timeZone: 'Pacific/Auckland' }).format(new Date(saved.starts_at)) + ' – ' +
        new Intl.DateTimeFormat('en-MY', { hour: '2-digit', minute: '2-digit', timeZone: 'Pacific/Auckland' }).format(new Date(saved.ends_at)),
    )
  })

  it('lets a booking writer with setup read but no setup write schedule existing people', async () => {
    permissions.clear()
    permissions.add('bookings:read')
    permissions.add('bookings:write')
    permissions.add('booking.settings:read')
    services[0]!.is_active = true
    resources[0]!.is_active = true
    mocks.post.mockResolvedValue(envelope({ id: 'new-event' }))
    await open()
    expect(wrapper!.findAll('button').some(item => item.text().trim() === 'Advanced setup')).toBe(false)
    await button('Add schedule').trigger('click')
    await flushPromises()
    expect(wrapper!.text()).toContain('Adding people or services requires booking setup permission')
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    await flushPromises()
    expect(mocks.post).toHaveBeenCalledTimes(1)
    expect(mocks.post).toHaveBeenCalledWith('/booking/events', expect.objectContaining({
      resource_id: 'resource-one', service_id: 'service-one', timezone: 'Asia/Kuala_Lumpur',
    }), { headers: { 'X-Organization-ID': 'organization-one' } })
  })

  it('opens Add schedule on an empty catalogue without opening advanced setup', async () => {
    services = []
    resources = []
    permissions.add('bookings:write')
    wrapper = mountCalendar()
    await flushPromises()
    expect(button('Add schedule').attributes('disabled')).toBeUndefined()
    await button('Add schedule').trigger('click')
    expect(wrapper.find('form[aria-label="Add person"]').exists()).toBe(true)
    expect(wrapper.find('form[aria-label="Service details"]').exists()).toBe(false)
  })

  it('exposes direct person creation to setup writers without booking write permission', async () => {
    await open()
    await button('Add doctor / instructor / staff').trigger('click')
    expect(wrapper!.find('form[aria-label="Add person"]').exists()).toBe(true)
    expect(wrapper!.findAll('button').some(item => item.text().trim() === 'Add schedule')).toBe(false)
    expect(wrapper!.find('form[aria-label="Service details"]').exists()).toBe(false)
  })
})


describe('Calendar weeks across resource timezones', () => {
  it.each([true, false])('includes both distant week boundaries and excludes margin-only slots (nested resource: %s)', async (includeResource) => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date(2026, 9, 28, 12))
    resources = [
      { ...resources[0]!, id: 'east', timezone: 'Pacific/Kiritimati' },
      { ...resources[0]!, id: 'west', timezone: 'Etc/GMT+12' },
    ]
    const cases = [
      { id: 'monday-east', resource: resources[0]!, starts_at: '2026-10-25T10:15:00Z', capacity: 2, booked_quantity: 1 },
      { id: 'sunday-west', resource: resources[1]!, starts_at: '2026-11-02T11:45:00Z', capacity: 3, booked_quantity: 2 },
      { id: 'previous-sunday', resource: resources[1]!, starts_at: '2026-10-26T11:45:00Z', capacity: 100, booked_quantity: 90 },
      { id: 'next-monday', resource: resources[0]!, starts_at: '2026-11-01T10:15:00Z', capacity: 200, booked_quantity: 190 },
    ]
    const candidates: BookingEvent[] = cases.map((item) => ({
      id: item.id, service_id: 'service-one', resource_id: item.resource.id,
      starts_at: item.starts_at,
      ends_at: new Date(Date.parse(item.starts_at) + 15 * 60_000).toISOString(),
      capacity: item.capacity, booked_quantity: item.booked_quantity,
      status: 'scheduled', version: 1,
      service: { ...services[0]!, name: item.id },
      ...(includeResource ? { resource: copy(item.resource) } : {}),
    }))
    // Match the opt-in half-open interval filters, rather than returning
    // fixtures the old query would never receive.
    mocks.get.mockImplementation((url: string, options?: { params?: Record<string, string> }) => {
      if (url !== '/booking/events') return readFixture(url)
      const from = Date.parse(options?.params?.overlap_from || '')
      const to = Date.parse(options?.params?.overlap_to || '')
      const listed = candidates.filter(item => from < Date.parse(item.ends_at) && Date.parse(item.starts_at) < to)
      return envelope({ events: listed, total: listed.length })
    })
    await open()
    const cards = wrapper!.findAll('[data-testid="calendar-event-card"]')
    expect(cards).toHaveLength(2)
    expect(cards[0]!.text()).toContain('monday-east')
    expect(cards[1]!.text()).toContain('sunday-west')
    expect(wrapper!.text()).toContain('3/5 seats booked')
    expect(wrapper!.text()).not.toContain('previous-sunday')
    expect(wrapper!.text()).not.toContain('next-monday')
    await action('Next week').trigger('click')
    await flushPromises()
    expect(wrapper!.findAll('[data-testid="calendar-event-card"]')).toHaveLength(1)
    expect(wrapper!.get('[data-testid="calendar-event-card"]').text()).toContain('next-monday')
    expect(wrapper!.text()).toContain('190/200 seats booked')
  })

  it('keeps both occurrences of the repeated resource-local hour on the Sunday clock change', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date(2026, 9, 28, 12))
    resources[0]!.timezone = 'America/New_York'
    const candidates: BookingEvent[] = ['2026-11-01T05:30:00Z', '2026-11-01T06:30:00Z'].map((start, index) => ({
      id: `repeated-${index}`, service_id: 'service-one', resource_id: 'resource-one',
      starts_at: start, ends_at: new Date(Date.parse(start) + 15 * 60_000).toISOString(),
      capacity: 1, status: 'scheduled', version: 1, service: copy(services[0]!),
    }))
    mocks.get.mockImplementation((url: string, options?: { params?: Record<string, string> }) => {
      if (url !== '/booking/events') return readFixture(url)
      const from = Date.parse(options?.params?.overlap_from || '')
      const to = Date.parse(options?.params?.overlap_to || '')
      const listed = candidates.filter(item => from < Date.parse(item.ends_at) && Date.parse(item.starts_at) < to)
      return envelope({ events: listed, total: listed.length })
    })
    await open()
    expect(wrapper!.findAll('[data-testid="calendar-event-card"]')).toHaveLength(2)
    const times = wrapper!.findAll('[data-testid="calendar-event-time"]').map(item => item.text())
    expect(times[0]).toBe(times[1])
    expect(wrapper!.text()).toContain('0/2 seats booked')
    await action('Next week').trigger('click')
    await flushPromises()
    expect(wrapper!.findAll('[data-testid="calendar-event-card"]')).toHaveLength(0)
    expect(wrapper!.text()).toContain('0/0 seats booked')
  })
})

describe('Calendar occupied intervals and incomplete loads', () => {
  function event(id: string, starts_at: string, ends_at: string): BookingEvent {
    return { id, service_id: 'service-one', resource_id: 'resource-one', starts_at, ends_at,
      capacity: 1, booked_quantity: 0, status: 'scheduled', version: 1,
      service: { ...services[0]!, name: id } }
  }

  function serve(values: BookingEvent[]) {
    mocks.get.mockImplementation((url: string, options?: { params?: Record<string, string | number> }) => {
      if (url === '/bookings') return envelope({ bookings: [], total: 0 })
      if (url !== '/booking/events') return readFixture(url)
      const params = options?.params ?? {}
      expect(params.from).toBeUndefined()
      expect(params.to).toBeUndefined()
      const from = Date.parse(String(params.overlap_from))
      const to = Date.parse(String(params.overlap_to))
      const matches = values.filter(item => Date.parse(item.starts_at) < to && Date.parse(item.ends_at) > from)
        .sort((a, b) => Date.parse(a.starts_at) - Date.parse(b.starts_at) || a.id.localeCompare(b.id))
      const page = Number(params.page), limit = Number(params.limit)
      return envelope({ events: matches.slice((page - 1) * limit, page * limit), total: matches.length })
    })
  }

  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date(2026, 10, 4, 12))
    resources[0]!.timezone = 'UTC'
  })

  it('shows previous-week continuations, clips at midnight and counts a multi-day event once', async () => {
    serve([
      event('overnight', '2026-11-01T23:30:00Z', '2026-11-02T00:30:00Z'),
      event('whole-week', '2026-10-01T00:00:00Z', '2026-12-01T00:00:00Z'),
      event('ends-at-start', '2026-11-01T23:30:00Z', '2026-11-02T00:00:00Z'),
      event('starts-at-end', '2026-11-09T00:00:00Z', '2026-11-09T00:30:00Z'),
    ])
    await open()
    expect(wrapper!.findAll('[data-testid="calendar-event-card"]')).toHaveLength(8)
    expect(wrapper!.text()).toContain('2 schedules')
    expect(wrapper!.text()).toContain('0/2 seats booked')
    expect(wrapper!.text()).not.toContain('ends-at-start')
    expect(wrapper!.text()).not.toContain('starts-at-end')
    const overnight = wrapper!.findAll('[data-testid="calendar-event-card"]').find(card => card.text().includes('overnight'))!
    expect(overnight.get('[data-testid="calendar-event-time"]').text()).toContain('Continues')
    await overnight.trigger('click')
    await flushPromises()
    expect(wrapper!.text()).toContain('Attendee desk')
  })

  it('keeps repeated-midnight occupancy and exact-end labels distinct from the first midnight', async () => {
    vi.setSystemTime(new Date(2026, 9, 28, 12))
    resources[0]!.timezone = 'America/Havana'
    serve([
      event('repeated-midnight', '2026-11-01T04:30:00Z', '2026-11-01T05:00:00Z'),
      event('first-midnight', '2026-11-01T03:30:00Z', '2026-11-01T04:00:00Z'),
      event('spans-repeated-midnight', '2026-11-01T03:30:00Z', '2026-11-01T05:00:00Z'),
    ])
    await open()
    const cards = wrapper!.findAll('[data-testid="calendar-event-card"]')
    expect(cards).toHaveLength(4)
    expect(wrapper!.text()).toContain('3 schedules')
    const times = cards.map(card => card.get('[data-testid="calendar-event-time"]').text())
    const label = (value: string) => new Intl.DateTimeFormat('en-MY', {
      hour: '2-digit', minute: '2-digit', timeZone: 'America/Havana',
    }).format(new Date(value))
    expect(times).toContain(`${label('2026-11-01T04:30:00Z')} – ${label('2026-11-01T05:00:00Z')}`)
    expect(times).toContain(`${label('2026-11-01T03:30:00Z')} – 24:00`)
    expect(times).toContain(`${label('2026-11-01T03:30:00Z')} – continues`)
    expect(times).toContain(`Continues – ${label('2026-11-01T05:00:00Z')}`)
  })

  it('shows the exact next-day end at a skipped midnight without adding an unoccupied date', async () => {
    vi.setSystemTime(new Date(2026, 8, 2, 12))
    resources[0]!.timezone = 'America/Santiago'
    serve([event('skipped-midnight', '2026-09-06T03:30:00Z', '2026-09-06T04:00:00Z')])
    await open()
    const cards = wrapper!.findAll('[data-testid="calendar-event-card"]')
    expect(cards).toHaveLength(1)
    expect(wrapper!.text()).toContain('1 schedules')
    const label = (value: string) => new Intl.DateTimeFormat('en-MY', {
      hour: '2-digit', minute: '2-digit', timeZone: 'America/Santiago',
    }).format(new Date(value))
    expect(cards[0]!.get('[data-testid="calendar-event-time"]').text())
      .toBe(`${label('2026-09-06T03:30:00Z')} – ${label('2026-09-06T04:00:00Z')} next day`)
  })

  it('loads more than100 equal-start intervals without duplicate totals', async () => {
    serve(Array.from({ length: 101 }, (_, n) => event(`slot-${String(n).padStart(3, '0')}`, '2026-11-04T09:00:00Z', '2026-11-04T10:00:00Z')))
    await open()
    expect(wrapper!.findAll('[data-testid="calendar-event-card"]')).toHaveLength(101)
    expect(wrapper!.text()).toContain('101 schedules')
    expect(wrapper!.text()).toContain('0/101 seats booked')
    expect(mocks.get.mock.calls.filter(([url]) => url === '/booking/events')).toHaveLength(2)
  })

  it.each(['', 'invalid', '2026-11-04T08:00:00Z'])('shows an incomplete-state error for invalid end %s', async end => {
    const invalid = event('bad-interval', '2026-11-04T09:00:00Z', end)
    mocks.get.mockImplementation((url: string) => url === '/booking/events'
      ? envelope({ events: [invalid], total: 1 }) : readFixture(url))
    wrapper = mountCalendar()
    await flushPromises()
    expect(wrapper!.get('[role="alert"]').text()).toContain('could not be loaded completely')
    expect(wrapper!.findAll('[data-testid="calendar-event-card"]')).toHaveLength(0)
    expect(wrapper!.text()).not.toContain('Open day')
    expect(wrapper!.text()).not.toContain('0 schedules')
  })

  it('does not display the previous week as a successful refresh after the next read fails', async () => {
    serve([event('old-week', '2026-11-04T09:00:00Z', '2026-11-04T10:00:00Z')])
    await open()
    expect(wrapper!.text()).toContain('old-week')
    mocks.get.mockRejectedValue(new Error('Read failed'))
    await action('Next week').trigger('click')
    await flushPromises()
    expect(wrapper!.get('[role="alert"]').text()).toContain('could not be loaded completely')
    expect(wrapper!.text()).not.toContain('old-week')
    expect(wrapper!.text()).not.toContain('Open day')
  })
})
