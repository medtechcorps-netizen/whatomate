/** @vitest-environment happy-dom */

import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, nextTick, reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { BookingResource, BookingService } from '@/services/productSuite'
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
