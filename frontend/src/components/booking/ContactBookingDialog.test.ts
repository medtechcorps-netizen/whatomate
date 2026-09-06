/** @vitest-environment happy-dom */

import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ContactBookingDialog from './ContactBookingDialog.vue'

const mocks = vi.hoisted(() => ({
  allAvailability: vi.fn(),
  createBooking: vi.fn(),
}))

vi.mock('@/services/productSuite', () => ({
  bookingService: {
    allAvailability: mocks.allAvailability,
    createBooking: mocks.createBooking,
  },
}))

const Passthrough = defineComponent({
  template: '<div><slot /></div>',
})

const ButtonStub = defineComponent({
  props: {
    disabled: Boolean,
    type: String,
  },
  emits: ['click'],
  template: '<button :type="type || \'button\'" :disabled="disabled" @click="$emit(\'click\')"><slot /></button>',
})

const slot = {
  id: 'event-1',
  service_id: 'service-1',
  resource_id: 'resource-1',
  starts_at: '2026-09-01T01:30:00Z',
  ends_at: '2026-09-01T02:30:00Z',
  local_starts_at: '2026-09-01T09:30:00',
  local_ends_at: '2026-09-01T10:30:00',
  timezone: 'Asia/Kuala_Lumpur',
  capacity: 4,
  booked_quantity: 2,
  remaining_capacity: 2,
  status: 'scheduled',
  location: 'Ampang consultation room',
  version: 1,
  service: {
    id: 'service-1',
    name: 'Initial assessment',
    kind: 'appointment',
    duration_minutes: 60,
    default_capacity: 1,
    price_minor: 80000,
    currency: 'MYR',
    is_active: true,
    version: 1,
  },
  resource: {
    id: 'resource-1',
    name: 'Dr Jeff',
    kind: 'practitioner',
    timezone: 'Asia/Kuala_Lumpur',
    location: 'Ampang',
    is_active: true,
    version: 1,
  },
}

const booking = {
  id: 'booking-1',
  event_id: slot.id,
  contact_id: 'contact-1',
  status: 'reserved',
  quantity: 1,
  source: 'agent',
  created_at: '2026-08-30T00:00:00Z',
  version: 1,
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

function mountDialog() {
  return mount(ContactBookingDialog, {
    props: {
      open: true,
      contactId: 'contact-1',
      contactName: 'Aina Rahman',
      surface: 'omnichannel',
    },
    global: {
      stubs: {
        Dialog: Passthrough,
        DialogContent: Passthrough,
        DialogDescription: Passthrough,
        DialogFooter: Passthrough,
        DialogHeader: Passthrough,
        DialogTitle: Passthrough,
        Button: ButtonStub,
      },
    },
  })
}

function button(wrapper: ReturnType<typeof mountDialog>, label: string) {
  const match = wrapper.findAll('button').find((candidate) => candidate.text().includes(label))
  if (!match) throw new Error(`Button not found: ${label}`)
  return match
}

describe('ContactBookingDialog', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-08-30T00:00:00Z'))
    mocks.allAvailability.mockReset().mockResolvedValue([slot])
    mocks.createBooking.mockReset().mockResolvedValue({ data: { data: booking } })
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('prefills the selected contact and reserves a chosen available slot without waitlisting', async () => {
    const wrapper = mountDialog()
    await flushPromises()

    expect(mocks.allAvailability).toHaveBeenCalledWith({
      from: '2026-08-30T00:00:00.000Z',
      to: '2026-09-29T00:00:00.000Z',
    })
    expect(wrapper.text()).toContain('Aina Rahman')
    expect(wrapper.text()).toContain('Initial assessment')
    expect(wrapper.text()).toContain('Dr Jeff')
    expect(wrapper.text()).toContain('Ampang consultation room')
    expect(wrapper.text()).toContain('2 places left')

    await wrapper.get('input[type="radio"]').setValue(slot.id)
    await button(wrapper, 'Review appointment').trigger('click')
    expect(wrapper.text()).toContain('Confirm booking')
    expect(wrapper.text()).toContain('Reserved · 1 place')

    await button(wrapper, 'Confirm booking').trigger('click')
    await flushPromises()

    expect(mocks.createBooking).toHaveBeenCalledWith(slot.id, {
      contact_id: 'contact-1',
      quantity: 1,
      status: 'reserved',
      source: 'agent',
      allow_waitlist: false,
      idempotency_key: expect.any(String),
    })
    expect(wrapper.emitted('booked')?.[0]).toEqual([booking])
    expect(wrapper.emitted('update:open')?.at(-1)).toEqual([false])
  })

  it('refreshes availability and explains when another user takes the last place', async () => {
    mocks.allAvailability.mockResolvedValueOnce([slot]).mockResolvedValueOnce([])
    mocks.createBooking.mockRejectedValueOnce({
      isAxiosError: true,
      response: { status: 409, data: { message: 'Schedule capacity is no longer available' } },
    })
    const wrapper = mountDialog()
    await flushPromises()

    await wrapper.get('input[type="radio"]').setValue(slot.id)
    await button(wrapper, 'Review appointment').trigger('click')
    await button(wrapper, 'Confirm booking').trigger('click')
    await flushPromises()

    expect(mocks.allAvailability).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('Schedule capacity is no longer available')
    expect(wrapper.text()).toContain('No available appointments')
    expect(wrapper.find('input[type="radio"]').exists()).toBe(false)
  })

  it('ignores a deferred success for the previous contact without finalizing the reopened contact submission', async () => {
    const firstRequest = deferred<{ data: { data: typeof booking } }>()
    const secondRequest = deferred<{ data: { data: typeof booking } }>()
    mocks.createBooking
      .mockReturnValueOnce(firstRequest.promise)
      .mockReturnValueOnce(secondRequest.promise)
    const wrapper = mountDialog()
    await flushPromises()

    await wrapper.get('input[type="radio"]').setValue(slot.id)
    await button(wrapper, 'Review appointment').trigger('click')
    await button(wrapper, 'Confirm booking').trigger('click')

    await wrapper.setProps({ open: false, contactId: 'contact-2', contactName: 'Bella Tan' })
    await wrapper.setProps({ open: true })
    await flushPromises()
    await wrapper.get('input[type="radio"]').setValue(slot.id)
    await button(wrapper, 'Review appointment').trigger('click')
    await button(wrapper, 'Confirm booking').trigger('click')

    expect(mocks.createBooking).toHaveBeenNthCalledWith(1, slot.id, expect.objectContaining({
      contact_id: 'contact-1',
    }))
    expect(mocks.createBooking).toHaveBeenNthCalledWith(2, slot.id, expect.objectContaining({
      contact_id: 'contact-2',
    }))

    firstRequest.resolve({ data: { data: booking } })
    await flushPromises()

    expect(wrapper.emitted('booked')).toBeUndefined()
    expect(wrapper.emitted('update:open')).toBeUndefined()
    expect(button(wrapper, 'Booking…').attributes('disabled')).toBeDefined()

    const secondBooking = { ...booking, id: 'booking-2', contact_id: 'contact-2' }
    secondRequest.resolve({ data: { data: secondBooking } })
    await flushPromises()

    expect(wrapper.emitted('booked')).toEqual([[secondBooking]])
    expect(wrapper.emitted('update:open')).toEqual([[false]])
  })

  it('ignores a deferred capacity conflict for the previous contact after another contact reopens', async () => {
    const firstRequest = deferred<{ data: { data: typeof booking } }>()
    mocks.createBooking.mockReturnValueOnce(firstRequest.promise)
    const wrapper = mountDialog()
    await flushPromises()

    await wrapper.get('input[type="radio"]').setValue(slot.id)
    await button(wrapper, 'Review appointment').trigger('click')
    await button(wrapper, 'Confirm booking').trigger('click')

    await wrapper.setProps({ open: false, contactId: 'contact-2', contactName: 'Bella Tan' })
    await wrapper.setProps({ open: true })
    await flushPromises()
    await wrapper.get('input[type="radio"]').setValue(slot.id)
    await button(wrapper, 'Review appointment').trigger('click')

    expect(mocks.allAvailability).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('Bella Tan')
    expect(wrapper.text()).toContain('Confirm booking')

    firstRequest.reject({
      isAxiosError: true,
      response: { status: 409, data: { message: 'Schedule capacity is no longer available' } },
    })
    await flushPromises()

    expect(mocks.allAvailability).toHaveBeenCalledTimes(2)
    expect(wrapper.text()).toContain('Bella Tan')
    expect(wrapper.text()).toContain('Confirm booking')
    expect(wrapper.text()).not.toContain('Schedule capacity is no longer available')
    expect(wrapper.emitted('booked')).toBeUndefined()
    expect(wrapper.emitted('update:open')).toBeUndefined()
  })
})
