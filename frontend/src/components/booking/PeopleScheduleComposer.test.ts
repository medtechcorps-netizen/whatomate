/** @vitest-environment happy-dom */
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { BookingResource, BookingService } from '@/services/productSuite'
import PeopleScheduleComposer from './PeopleScheduleComposer.vue'

const mocks = vi.hoisted(() => ({
  organization: 'org-one',
  createResource: vi.fn(),
  createService: vi.fn(),
  createEvent: vi.fn(),
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get organizationId() {
      return mocks.organization
    },
  }),
}))
vi.mock('@/lib/browserIdentity', () => ({
  readSelectedOrganizationId: () => mocks.organization,
}))
vi.mock('@/services/productSuite', () => ({
  bookingService: {
    createResource: mocks.createResource,
    createService: mocks.createService,
    createEvent: mocks.createEvent,
  },
}))
const ButtonStub = defineComponent({
  props: { disabled: Boolean, type: String },
  emits: ['click'],
  template:
    '<button :type="type || \'button\'" :disabled="disabled" @click="$emit(\'click\', $event)"><slot /></button>',
})
const resource = (id = 'person-one', kind: BookingResource['kind'] = 'practitioner'): BookingResource => ({
  id,
  name: id === 'person-one' ? 'Dr Aina' : 'Instructor Ben',
  kind,
  timezone: 'Asia/Kuala_Lumpur',
  is_active: true,
  version: 1,
  metadata: {},
})
const service = (id = 'service-one', ids = ['person-one']): BookingService => ({
  id,
  name: id,
  kind: 'appointment',
  duration_minutes: 45,
  default_capacity: 2,
  price_minor: 0,
  currency: 'MYR',
  resource_ids: ids,
  is_active: true,
  version: 1,
})
const envelope = <T>(value: T) => ({ data: { data: value } })
function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => {
    resolve = done
  })
  return { promise, resolve }
}
let wrapper: ReturnType<typeof mount> | undefined
function open(props: Record<string, unknown> = {}) {
  wrapper = mount(PeopleScheduleComposer, {
    props: {
      organization: 'org-one',
      resources: [],
      services: [],
      initialMode: 'schedule',
      canManage: true,
      canSchedule: true,
      ...props,
    },
    global: { stubs: { Button: ButtonStub } },
  })
  return wrapper
}
async function fillPerson(kind = 'practitioner') {
  await wrapper!.get('[name="person-name"]').setValue('Aina')
  await wrapper!.get('[name="person-role"]').setValue(kind)
  await wrapper!.get('[name="person-timezone"]').setValue('Asia/Kuala_Lumpur')
}
async function fillService() {
  await wrapper!.get('[name="inline-service-name"]').setValue('Consultation')
  await wrapper!.get('[name="inline-service-duration"]').setValue(45)
  await wrapper!.get('[name="inline-service-capacity"]').setValue(2)
}
function button(text: string) {
  const value = wrapper!.findAll('button').find((item) => item.text().trim() === text)
  if (!value) throw new Error('Missing button: ' + text)
  return value
}

beforeEach(() => {
  vi.clearAllMocks()
  mocks.organization = 'org-one'
  mocks.createResource.mockReset()
  mocks.createService.mockReset()
  mocks.createEvent.mockReset()
  mocks.createResource.mockImplementation((payload) =>
    Promise.resolve(envelope({ ...resource(), ...payload })),
  )
  mocks.createService.mockImplementation((payload) => Promise.resolve(envelope({ ...service(), ...payload })))
  mocks.createEvent.mockResolvedValue(envelope({ id: 'event-one' }))
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

describe('Person-first scheduling', () => {
  it('accepts the backend capacity limit and blocks values above it for services and schedules', async () => {
    open({ resources: [resource()] })
    await flushPromises()
    await fillService()
    const serviceCapacity = wrapper!.get('[name="inline-service-capacity"]')
    expect(serviceCapacity.attributes('max')).toBe('10000')
    await serviceCapacity.setValue('10001')
    await wrapper!.get('form[aria-label="Add service for person"]').trigger('submit')
    expect(mocks.createService).not.toHaveBeenCalled()
    await serviceCapacity.setValue('10000')
    await wrapper!.get('form[aria-label="Add service for person"]').trigger('submit')
    await flushPromises()
    expect(mocks.createService).toHaveBeenCalledWith(
      expect.objectContaining({ default_capacity: 10000 }),
      'org-one',
    )
    const scheduleCapacity = wrapper!.get('[name="schedule-capacity"]')
    expect(scheduleCapacity.attributes('max')).toBe('10000')
    await scheduleCapacity.setValue('10001')
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    expect(mocks.createEvent).not.toHaveBeenCalled()
    await scheduleCapacity.setValue('10000')
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    await flushPromises()
    expect(mocks.createEvent).toHaveBeenCalledWith(
      expect.objectContaining({ capacity: 10000 }),
      'org-one',
    )
  })

  it('carries local wall time across midnight and leaves DST rejection to the backend', async () => {
    open({
      resources: [{ ...resource(), timezone: 'America/New_York' }],
      services: [{ ...service(), duration_minutes: 90 }],
    })
    await flushPromises()
    await wrapper!.get('[name="schedule-date"]').setValue('2026-10-31')
    await wrapper!.get('[name="schedule-time"]').setValue('23:30')
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    await flushPromises()
    expect(mocks.createEvent).toHaveBeenCalledWith(
      expect.objectContaining({
        local_starts_at: '2026-10-31T23:30',
        local_ends_at: '2026-11-01T01:00',
        timezone: 'America/New_York',
      }),
      'org-one',
    )
    mocks.createEvent.mockRejectedValueOnce(new Error('Local time is ambiguous in America/New_York'))
    await wrapper!.get('[name="schedule-date"]').setValue('2026-11-01')
    await wrapper!.get('[name="schedule-time"]').setValue('01:30')
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    await flushPromises()
    expect(wrapper!.get('[role="alert"]').text()).toContain('Local time is ambiguous')
    expect(wrapper!.get<HTMLSelectElement>('[name="schedule-person"]').element.value).toBe('person-one')
    expect(mocks.createResource).not.toHaveBeenCalled()
  })

  it.each(['practitioner', 'instructor', 'staff'])(
    'creates %s and a schedule from an empty catalogue',
    async (kind) => {
      open()
      await fillPerson(kind)
      await wrapper!.get('form[aria-label="Add person"]').trigger('submit')
      await flushPromises()
      expect(mocks.createResource).toHaveBeenCalledWith(
        {
          name: 'Aina',
          kind,
          timezone: 'Asia/Kuala_Lumpur',
          location: '',
          is_active: true,
        },
        'org-one',
      )
      expect(wrapper!.find('form[aria-label="Add person"]').exists()).toBe(false)
      expect(wrapper!.get<HTMLSelectElement>('[name="schedule-person"]').element.value).toBe('person-one')
      await fillService()
      await wrapper!.get('form[aria-label="Add service for person"]').trigger('submit')
      await flushPromises()
      expect(mocks.createService).toHaveBeenCalledWith(
        expect.objectContaining({
          name: 'Consultation',
          resource_ids: ['person-one'],
          duration_minutes: 45,
          default_capacity: 2,
        }),
        'org-one',
      )
      await wrapper!.get('[name="schedule-date"]').setValue('2026-11-02')
      await wrapper!.get('[name="schedule-time"]').setValue('09:15')
      await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
      await flushPromises()
      expect(mocks.createEvent).toHaveBeenCalledWith(
        {
          service_id: 'service-one',
          resource_id: 'person-one',
          local_starts_at: '2026-11-02T09:15',
          local_ends_at: '2026-11-02T10:00',
          timezone: 'Asia/Kuala_Lumpur',
          capacity: 2,
          status: 'scheduled',
          location: '',
        },
        'org-one',
      )
      expect(wrapper!.emitted('scheduled')).toEqual([[{ id: 'event-one' }]])
    },
  )

  it('filters incompatible and inactive services and automatically selects the only eligible one', async () => {
    open({
      resources: [resource(), resource('person-two', 'instructor')],
      services: [service(), service('other', ['person-two']), { ...service('inactive'), is_active: false }],
    })
    await wrapper!.get('[name="schedule-person"]').setValue('person-one')
    expect(wrapper!.get<HTMLSelectElement>('[name="schedule-service"]').element.value).toBe('service-one')
    expect(
      wrapper!.findAll('[name="schedule-service"] option').map((option) => option.attributes('value')),
    ).toEqual(['', 'service-one'])
    await wrapper!.get('[name="schedule-person"]').setValue('person-two')
    expect(wrapper!.get<HTMLSelectElement>('[name="schedule-service"]').element.value).toBe('other')
    expect(mocks.createService).not.toHaveBeenCalled()
  })

  it('requires a deliberate choice when multiple compatible services exist', async () => {
    open({
      resources: [resource()],
      services: [service(), service('global', [])],
    })
    await flushPromises()
    expect(wrapper!.get<HTMLSelectElement>('[name="schedule-service"]').element.value).toBe('')
    expect(button('Save schedule').attributes('disabled')).toBeDefined()
    await wrapper!.get('[name="schedule-service"]').setValue('global')
    expect(button('Save schedule').attributes('disabled')).toBeUndefined()
  })

  it('preserves the acknowledged person and service across later failures and retries', async () => {
    open()
    await fillPerson('staff')
    await wrapper!.get('form[aria-label="Add person"]').trigger('submit')
    await flushPromises()
    mocks.createService.mockRejectedValueOnce(new Error('Service unavailable'))
    await fillService()
    await wrapper!.get('form[aria-label="Add service for person"]').trigger('submit')
    await flushPromises()
    expect(wrapper!.get('[role="alert"]').text()).toContain('Service unavailable')
    expect(wrapper!.get<HTMLInputElement>('[name="inline-service-name"]').element.value).toBe('Consultation')
    await wrapper!.get('form[aria-label="Add service for person"]').trigger('submit')
    await flushPromises()
    mocks.createEvent.mockRejectedValueOnce(new Error('Time is unavailable'))
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    await flushPromises()
    expect(wrapper!.get('[role="alert"]').text()).toContain('Time is unavailable')
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    await flushPromises()
    expect(mocks.createResource).toHaveBeenCalledTimes(1)
    expect(mocks.createService).toHaveBeenCalledTimes(2)
    expect(mocks.createEvent).toHaveBeenCalledTimes(2)
    expect(
      mocks.createEvent.mock.calls.every(
        ([payload]) => payload.resource_id === 'person-one' && payload.service_id === 'service-one',
      ),
    ).toBe(true)
  })

  it('schedules existing entries without setup write permission and explains unavailable creation', async () => {
    open({ resources: [resource()], services: [service()], canManage: false })
    await flushPromises()
    expect(wrapper!.text()).toContain('Adding people or services requires booking setup permission')
    expect(wrapper!.find('form[aria-label="Add person"]').exists()).toBe(false)
    expect(wrapper!.text()).not.toContain('Add doctor / instructor / staff')
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    await flushPromises()
    expect(mocks.createEvent).toHaveBeenCalledTimes(1)
    expect(mocks.createResource).not.toHaveBeenCalled()
    expect(mocks.createService).not.toHaveBeenCalled()
  })

  it('explains an empty catalogue without silently granting setup permission', async () => {
    open({ canManage: false })
    await flushPromises()
    expect(wrapper!.text()).toContain('No people have been added yet')
    expect(button('Save schedule').attributes('disabled')).toBeDefined()
    expect(mocks.createResource).not.toHaveBeenCalled()
  })

  it('does not issue a duplicate person create while the first response is pending', async () => {
    const pending = deferred<unknown>()
    mocks.createResource.mockReturnValue(pending.promise)
    open()
    await fillPerson()
    await wrapper!.get('form[aria-label="Add person"]').trigger('submit')
    await wrapper!.get('form[aria-label="Add person"]').trigger('submit')
    expect(mocks.createResource).toHaveBeenCalledTimes(1)
    pending.resolve(envelope(resource()))
    await flushPromises()
    expect(wrapper!.emitted('resourceCreated')).toHaveLength(1)
  })

  it('does not issue duplicate schedule submissions', async () => {
    const pending = deferred<unknown>()
    mocks.createEvent.mockReturnValue(pending.promise)
    open({ resources: [resource()], services: [service()] })
    await flushPromises()
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    expect(mocks.createEvent).toHaveBeenCalledTimes(1)
    pending.resolve(envelope({ id: 'event-one' }))
    await flushPromises()
  })

  it('refuses a submission after the organization changes', async () => {
    open({ resources: [resource()], services: [service()] })
    await flushPromises()
    mocks.organization = 'org-two'
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    expect(mocks.createEvent).not.toHaveBeenCalled()
    expect(wrapper!.get('[role="alert"]').text()).toContain('workspace or permission changed')
  })

  it('ignores an old organization create response and makes no dependent request', async () => {
    const pending = deferred<unknown>()
    mocks.createResource.mockReturnValue(pending.promise)
    open()
    await fillPerson()
    await wrapper!.get('form[aria-label="Add person"]').trigger('submit')
    mocks.organization = 'org-two'
    pending.resolve(envelope(resource()))
    await flushPromises()
    expect(wrapper!.emitted('resourceCreated')).toBeUndefined()
    expect(mocks.createService).not.toHaveBeenCalled()
    expect(mocks.createEvent).not.toHaveBeenCalled()
  })

  it('rechecks both creation and scheduling permissions at submit', async () => {
    open()
    await fillPerson()
    await wrapper!.setProps({ canManage: false })
    expect(wrapper!.find('form[aria-label="Add person"]').exists()).toBe(false)
    expect(mocks.createResource).not.toHaveBeenCalled()
    wrapper!.unmount()
    open({ resources: [resource()], services: [service()] })
    await flushPromises()
    await wrapper!.setProps({ canSchedule: false })
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    expect(mocks.createEvent).not.toHaveBeenCalled()
  })

  it('invalidates a selected person or service when the catalogue no longer permits it', async () => {
    open({ resources: [resource()], services: [service()] })
    await flushPromises()
    await wrapper!.setProps({
      services: [{ ...service(), resource_ids: ['someone-else'] }],
    })
    expect(wrapper!.get<HTMLSelectElement>('[name="schedule-service"]').element.value).toBe('')
    await wrapper!.get('form[aria-label="Schedule details"]').trigger('submit')
    expect(mocks.createEvent).not.toHaveBeenCalled()
    await wrapper!.setProps({
      resources: [{ ...resource(), is_active: false }],
    })
    expect(wrapper!.get<HTMLSelectElement>('[name="schedule-person"]').element.value).toBe('')
  })

  it('supports person creation by setup-only users without exposing schedule creation', async () => {
    open({ initialMode: 'person', canSchedule: false })
    await fillPerson('staff')
    await wrapper!.get('form[aria-label="Add person"]').trigger('submit')
    await flushPromises()
    expect(wrapper!.emitted('resourceCreated')).toHaveLength(1)
    expect(wrapper!.find('form[aria-label="Schedule details"]').exists()).toBe(false)
    expect(mocks.createEvent).not.toHaveBeenCalled()
  })
})
