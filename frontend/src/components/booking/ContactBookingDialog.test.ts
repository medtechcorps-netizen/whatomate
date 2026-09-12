/** @vitest-environment happy-dom */

import { flushPromises, mount, shallowMount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ContactBookingDialog from './ContactBookingDialog.vue'
import CustomerRevenueWorkspace from '../chat/CustomerRevenueWorkspace.vue'

const mocks = vi.hoisted(() => ({
  allAvailability: vi.fn(),
  createBooking: vi.fn(),
  createLead: vi.fn(),
  createTask: vi.fn(),
  runCopilot: vi.fn(),
  pipelines: vi.fn(),
  getWorkspace: vi.fn(),
  hasPermission: vi.fn(),
  toastError: vi.fn(),
  toastSuccess: vi.fn(),
}))

vi.mock('@/services/productSuite', () => ({
  bookingService: {
    allAvailability: mocks.allAvailability,
    createBooking: mocks.createBooking,
  },
  copilotService: { run: mocks.runCopilot },
  crmService: {
    createLead: mocks.createLead,
    createTask: mocks.createTask,
    pipelines: mocks.pipelines,
  },
  customerWorkspaceService: { get: mocks.getWorkspace },
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ hasPermission: mocks.hasPermission }),
}))

vi.mock('@/composables/useAppToast', () => ({
  useAppToast: () => ({
    error: mocks.toastError,
    success: mocks.toastSuccess,
  }),
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

describe('CustomerRevenueWorkspace booking identity', () => {
  function workspaceResponse(
    contactId: string,
    capabilities?: Record<string, boolean>,
  ) {
    return {
      data: {
        data: {
          contact: {
            id: contactId,
            phone_number: '+60111111111',
            profile_name: `Customer ${contactId}`,
            identity_review_ai_state: {
              known: true,
              ai_allowed: true,
              blocked: false,
              open_hold_count: 0,
              reason: 'no_open_identity_review',
            },
          },
          capabilities,
          identities: [],
          journeys: [],
          tasks: [],
          bookings: [],
          packages: [],
          invoices: [],
          payments: [],
          timeline: [],
        },
      },
    }
  }

  function mountWorkspace(contactId = 'merged-contact-alias') {
    return shallowMount(CustomerRevenueWorkspace, {
      props: { contactId },
      global: {
        stubs: {
          RouterLink: true,
          Button: ButtonStub,
          ScrollArea: Passthrough,
          Tabs: Passthrough,
          TabsContent: Passthrough,
          TabsList: Passthrough,
          TabsTrigger: Passthrough,
        },
      },
    })
  }

  beforeEach(() => {
    mocks.createLead.mockReset().mockResolvedValue({ data: { data: { id: 'lead-1' } } })
    mocks.createTask.mockReset().mockResolvedValue({ data: { data: { id: 'task-1' } } })
    mocks.runCopilot.mockReset().mockResolvedValue({
      data: {
        data: {
          id: 'copilot-1',
          task_type: 'summary',
          result_text: 'Canonical summary',
        },
      },
    })
    mocks.pipelines.mockReset()
    mocks.hasPermission.mockReset().mockReturnValue(true)
    mocks.toastError.mockReset()
    mocks.toastSuccess.mockReset()
  })

  it('uses the canonical workspace contact for writes after an alias merge', async () => {
    mocks.getWorkspace.mockReset().mockResolvedValue(workspaceResponse('canonical-contact'))
    const wrapper = mountWorkspace()
    await flushPromises()

    expect(mocks.getWorkspace).toHaveBeenCalledWith('merged-contact-alias')
    expect(wrapper.findComponent(ContactBookingDialog).props('contactId'))
      .toBe('canonical-contact')

    const state = (wrapper.vm as any).$?.setupState
    state.pipelines = [{
      id: 'pipeline-1',
      is_default: true,
      stages: [{ id: 'stage-1', kind: 'open', display_order: 1 }],
    }]
    await state.openJourney()
    state.journeyDraft.title = 'Canonical journey'
    state.journeyDraft.pipeline_id = 'pipeline-1'
    await state.createJourney()
    state.openFollowUp()
    state.taskDraft.title = 'Canonical follow-up'
    await state.createFollowUp()
    await state.runCopilot('summary')

    expect(mocks.createLead).toHaveBeenCalledWith(expect.objectContaining({
      contact_id: 'canonical-contact',
    }))
    expect(mocks.createTask).toHaveBeenCalledWith(expect.objectContaining({
      contact_id: 'canonical-contact',
    }))
    expect(mocks.runCopilot).toHaveBeenCalledWith(
      'canonical-contact',
      'summary',
      expect.objectContaining({ idempotency_key: expect.any(String) }),
    )
  })

  it('discards a Copilot result after the workspace switches contacts', async () => {
    const firstRun = deferred<{ data: { data: Record<string, unknown> } }>()
    const secondRun = deferred<{ data: { data: Record<string, unknown> } }>()
    mocks.getWorkspace
      .mockReset()
      .mockResolvedValueOnce(workspaceResponse('canonical-a'))
      .mockResolvedValueOnce(workspaceResponse('canonical-b'))
    mocks.runCopilot
      .mockReset()
      .mockReturnValueOnce(firstRun.promise)
      .mockReturnValueOnce(secondRun.promise)
    const wrapper = mountWorkspace('alias-a')
    await flushPromises()
    const state = (wrapper.vm as any).$?.setupState

    const first = state.runCopilot('summary')
    await wrapper.setProps({ contactId: 'alias-b' })
    await flushPromises()
    const second = state.runCopilot('qualify')

    firstRun.resolve({
      data: { data: { id: 'run-a', task_type: 'summary', result_text: 'Customer A' } },
    })
    await first
    expect(state.copilotRun).toBeNull()
    expect(state.copilotResult).toBe('')
    expect(state.copilotRunning).toBe('qualify')

    secondRun.resolve({
      data: { data: { id: 'run-b', task_type: 'qualify', result_text: 'Customer B' } },
    })
    await second
    expect(state.copilotResult).toBe('Customer B')
    expect(state.copilotRunning).toBeNull()
    expect(mocks.runCopilot).toHaveBeenNthCalledWith(
      1,
      'canonical-a',
      'summary',
      expect.any(Object),
    )
    expect(mocks.runCopilot).toHaveBeenNthCalledWith(
      2,
      'canonical-b',
      'qualify',
      expect.any(Object),
    )
  })

  it('invalidates an open journey form before a contact switch can retarget it', async () => {
    mocks.getWorkspace
      .mockReset()
      .mockResolvedValueOnce(workspaceResponse('canonical-a'))
      .mockResolvedValueOnce(workspaceResponse('canonical-b'))
    const wrapper = mountWorkspace('alias-a')
    await flushPromises()
    const state = (wrapper.vm as any).$?.setupState
    state.pipelines = [{
      id: 'pipeline-1',
      name: 'Sales',
      is_default: true,
      stages: [{ id: 'stage-1', kind: 'open', display_order: 1 }],
    }]

    await state.openJourney()
    state.journeyDraft.title = 'A journey draft'
    state.journeyDraft.pipeline_id = 'pipeline-1'
    expect(state.showJourneyDialog).toBe(true)

    await wrapper.setProps({ contactId: 'alias-b' })
    await flushPromises()
    expect(state.showJourneyDialog).toBe(false)
    expect(state.journeyDraft.title).toBe('')

    // Even a direct stale submit cannot reuse the old context for contact B.
    state.journeyDraft.title = 'Stale journey draft'
    state.journeyDraft.pipeline_id = 'pipeline-1'
    await state.createJourney()
    expect(mocks.createLead).not.toHaveBeenCalled()
  })

  it('invalidates an open no-lead follow-up before a contact switch can retarget it', async () => {
    mocks.getWorkspace
      .mockReset()
      .mockResolvedValueOnce(workspaceResponse('canonical-a'))
      .mockResolvedValueOnce(workspaceResponse('canonical-b'))
    const wrapper = mountWorkspace('alias-a')
    await flushPromises()
    const state = (wrapper.vm as any).$?.setupState

    state.openFollowUp()
    expect(state.taskDraft.lead_id).toBe('')
    expect(state.showTaskDialog).toBe(true)

    await wrapper.setProps({ contactId: 'alias-b' })
    await flushPromises()
    expect(state.showTaskDialog).toBe(false)
    expect(state.taskDraft.title).toBe('')

    state.taskDraft.title = 'Stale no-lead follow-up'
    await state.createFollowUp()
    expect(mocks.createTask).not.toHaveBeenCalled()
  })

  it('isolates a stale journey completion after switching contacts', async () => {
    const firstCreate = deferred<{ data: { data: { id: string } } }>()
    mocks.getWorkspace
      .mockReset()
      .mockResolvedValueOnce(workspaceResponse('canonical-a'))
      .mockResolvedValueOnce(workspaceResponse('canonical-b'))
    mocks.createLead.mockReset().mockReturnValueOnce(firstCreate.promise)
    const wrapper = mountWorkspace('alias-a')
    await flushPromises()
    const state = (wrapper.vm as any).$?.setupState
    state.pipelines = [{
      id: 'pipeline-1',
      name: 'Sales',
      is_default: true,
      stages: [{ id: 'stage-1', kind: 'open', display_order: 1 }],
    }]

    await state.openJourney()
    state.journeyDraft.title = 'A journey draft'
    state.journeyDraft.pipeline_id = 'pipeline-1'
    const pendingCreate = state.createJourney()
    expect(mocks.createLead).toHaveBeenCalledWith(expect.objectContaining({
      contact_id: 'canonical-a',
    }))

    await wrapper.setProps({ contactId: 'alias-b' })
    await flushPromises()
    expect(state.showJourneyDialog).toBe(false)
    expect(state.savingJourney).toBe(false)

    firstCreate.resolve({ data: { data: { id: 'lead-a' } } })
    await pendingCreate
    await flushPromises()

    expect(mocks.toastSuccess).not.toHaveBeenCalledWith(
      'Journey created',
      expect.anything(),
    )
    expect(mocks.getWorkspace).toHaveBeenCalledTimes(2)
    expect(state.showJourneyDialog).toBe(false)
    expect(state.journeyDraft.title).toBe('')
  })

  it('shows booking entry only with server visibility and write permission', async () => {
    mocks.getWorkspace.mockReset().mockResolvedValue(workspaceResponse('canonical-contact', {
      bookings: false,
    }))
    const serverDenied = mountWorkspace()
    await flushPromises()
    expect(serverDenied.text()).not.toContain('Book appointment')
    serverDenied.unmount()

    mocks.getWorkspace.mockReset().mockResolvedValue(workspaceResponse('canonical-contact', {
      bookings: true,
    }))
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      !(resource === 'bookings' && action === 'write'),
    )
    const writeDenied = mountWorkspace()
    await flushPromises()
    expect(writeDenied.text()).not.toContain('Book appointment')
    writeDenied.unmount()

    mocks.hasPermission.mockReset().mockReturnValue(true)
    const authorized = mountWorkspace()
    await flushPromises()
    expect(authorized.text()).toContain('Book appointment')
    authorized.unmount()
  })
})
