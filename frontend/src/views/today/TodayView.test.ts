/** @vitest-environment happy-dom */

import { flushPromises, mount, RouterLinkStub } from '@vue/test-utils'
import { defineComponent, reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { formatCurrencyMinorUnits } from '@/lib/currency'
import type { CommerceInvoice, CRMLead, FollowUpTask } from '@/services/productSuite'
import TodayView from './TodayView.vue'

const mocks = vi.hoisted(() => ({
  allTasks: vi.fn(),
  allLeads: vi.fn(),
  allInvoices: vi.fn(),
  allEvents: vi.fn(),
  allBookings: vi.fn(),
  unreadRefresh: vi.fn(),
  hasPermission: vi.fn(),
  hasProductEntitlement: vi.fn(),
}))

const unreadState = vi.hoisted(() => ({
  value: null as null | Record<string, unknown>,
}))

vi.mock('@/services/productSuite', () => ({
  crmService: { allTasks: mocks.allTasks, allLeads: mocks.allLeads },
  commerceService: { allInvoices: mocks.allInvoices },
  bookingService: { allEvents: mocks.allEvents, allBookings: mocks.allBookings },
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    hasPermission: mocks.hasPermission,
    hasProductEntitlement: mocks.hasProductEntitlement,
  }),
}))
vi.mock('@/stores/omnichannelUnread', () => ({
  useOmnichannelUnreadStore: () => unreadState.value,
}))

const ButtonStub = defineComponent({
  props: { disabled: Boolean, type: String },
  emits: ['click'],
  template:
    '<button :type="type || \'button\'" :disabled="disabled" @click="$emit(\'click\', $event)"><slot /></button>',
})
const InvoiceDialogStub = defineComponent({
  name: 'InvoiceQuickDialog',
  props: {
    open: Boolean,
    contact: Object,
    lead: Object,
    canSellPackages: Boolean,
    source: String,
  },
  emits: ['update:open', 'created'],
  template: '<div data-testid="invoice-dialog" :data-open="String(open)" />',
})

let permissions: Set<string>
let entitlements: Set<string>
let wrapper: ReturnType<typeof mountToday> | undefined

function mountToday() {
  return mount(TodayView, {
    global: {
      stubs: {
        PageHeader: defineComponent({
          props: { title: String, description: String },
          template: '<header><h1>{{ title }}</h1><p>{{ description }}</p><slot name="actions" /></header>',
        }),
        Button: ButtonStub,
        InvoiceQuickDialog: InvoiceDialogStub,
        RouterLink: RouterLinkStub,
      },
    },
  })
}

function grant(...items: string[]) {
  for (const item of items) permissions.add(item.includes(':') ? item : `${item}:read`)
}

function at(dayOffset: number, hour = 12) {
  const now = new Date()
  return new Date(now.getFullYear(), now.getMonth(), now.getDate() + dayOffset, hour).toISOString()
}

function daysAgo(days: number) {
  return new Date(Date.now() - days * 24 * 60 * 60 * 1000).toISOString()
}

function task(overrides: Partial<FollowUpTask>): FollowUpTask {
  return {
    id: 'task-1',
    title: 'Call back',
    status: 'open',
    priority: 'normal',
    version: 1,
    ...overrides,
  }
}

function lead(overrides: Partial<CRMLead>): CRMLead {
  return {
    id: 'lead-1',
    contact_id: 'contact-1',
    pipeline_id: 'pipe-1',
    stage_id: 'stage-1',
    title: 'Lead',
    status: 'open',
    value_minor: 0,
    currency: 'MYR',
    version: 1,
    ...overrides,
  }
}

function invoice(overrides: Partial<CommerceInvoice>): CommerceInvoice {
  return {
    id: 'inv-1',
    contact_id: 'contact-1',
    invoice_number: 'INV-1',
    status: 'open',
    currency: 'MYR',
    total_minor: 0,
    paid_minor: 0,
    due_minor: 0,
    version: 1,
    ...overrides,
  }
}

function card(id: string) {
  return wrapper!.get(`[data-testid="today-card-${id}"]`)
}

function linksIn(id: string): Array<{ to: unknown; text: string; label?: string }> {
  const container = card(id).element
  return wrapper!
    .findAllComponents(RouterLinkStub)
    .filter((link) => container.contains(link.element))
    .map((link) => ({ to: link.props('to'), text: link.text(), label: link.attributes('aria-label') }))
}

beforeEach(() => {
  permissions = new Set()
  entitlements = new Set(['crm.enabled'])
  unreadState.value = reactive({
    unreadConversationCount: null,
    loading: false,
    stale: true,
    refresh: mocks.unreadRefresh,
  })
  mocks.hasPermission.mockImplementation((resource: string, action = 'read') =>
    permissions.has(`${resource}:${action}`),
  )
  mocks.hasProductEntitlement.mockImplementation((key?: string) => !key || entitlements.has(key))
  mocks.allTasks.mockResolvedValue([])
  mocks.allLeads.mockResolvedValue([])
  mocks.allInvoices.mockResolvedValue([])
  mocks.allEvents.mockResolvedValue([])
  mocks.allBookings.mockResolvedValue([])
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.clearAllMocks()
})

describe('TodayView', () => {
  it('shows the header, subtitle and today’s date', async () => {
    wrapper = mountToday()
    await flushPromises()
    expect(wrapper.get('h1').text()).toBe('Today')
    expect(wrapper.text()).toContain('What needs your attention right now.')
    expect(wrapper.get('[data-testid="today-date"]').text()).toContain(String(new Date().getFullYear()))
  })

  it('sends no request for a card the user cannot see', async () => {
    grant('tasks')
    wrapper = mountToday()
    await flushPromises()

    expect(mocks.allTasks).toHaveBeenCalledTimes(2)
    expect(mocks.allTasks).toHaveBeenCalledWith({ status: 'open' })
    expect(mocks.allTasks).toHaveBeenCalledWith({ status: 'in_progress' })
    expect(mocks.allLeads).not.toHaveBeenCalled()
    expect(mocks.allInvoices).not.toHaveBeenCalled()
    expect(mocks.allEvents).not.toHaveBeenCalled()
    expect(mocks.allBookings).not.toHaveBeenCalled()
    expect(wrapper.findAll('section[data-testid^="today-card-"]').map((s) => s.attributes('data-testid'))).toEqual([
      'today-card-follow-ups',
    ])
  })

  it('hides commerce and booking cards without their product entitlement', async () => {
    grant('payments', 'crm.leads', 'bookings')
    wrapper = mountToday()
    await flushPromises()

    expect(mocks.allInvoices).not.toHaveBeenCalled()
    expect(mocks.allEvents).not.toHaveBeenCalled()
    expect(mocks.allLeads).toHaveBeenCalledTimes(1)
    expect(mocks.allLeads).toHaveBeenCalledWith({ status: 'open' })
    expect(wrapper.find('[data-testid="today-card-won"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="today-card-unpaid"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="today-card-appointments"]').exists()).toBe(false)
  })

  it('shows a plain message and makes no request when no card is allowed', async () => {
    wrapper = mountToday()
    await flushPromises()
    expect(wrapper.find('[data-testid="today-no-cards"]').exists()).toBe(true)
    for (const fn of [mocks.allTasks, mocks.allLeads, mocks.allInvoices, mocks.allEvents, mocks.allBookings]) {
      expect(fn).not.toHaveBeenCalled()
    }
  })

  it('splits follow-ups into overdue and today and links to the chat', async () => {
    grant('tasks', 'chat')
    mocks.allTasks.mockResolvedValue([
      task({ id: 't-over', title: 'Send price list', due_at: at(-2), contact_id: 'c-1', contact: { id: 'c-1', profile_name: 'Aisyah' } }),
      task({ id: 't-today', title: 'Confirm visit', due_at: at(0), contact_id: 'c-2', contact: { id: 'c-2', profile_name: 'Ben' } }),
      task({ id: 't-later', title: 'Check in next week', due_at: at(3) }),
      task({ id: 't-none', title: 'No date' }),
    ])
    wrapper = mountToday()
    await flushPromises()

    const followUps = card('follow-ups')
    expect(followUps.get('[data-testid="today-card-count"]').text()).toBe('2')
    expect(followUps.get('[data-testid="today-group-overdue"]').text()).toBe('Overdue (1)')
    expect(followUps.get('[data-testid="today-group-today"]').text()).toBe('Today (1)')
    const rows = followUps.findAll('[data-testid="today-row"]').map((row) => row.text())
    expect(rows).toHaveLength(2)
    expect(rows[0]).toContain('Send price list')
    expect(rows[0]).toContain('Aisyah')
    expect(rows[1]).toContain('Confirm visit')
    expect(followUps.text()).not.toContain('Check in next week')

    const links = linksIn('follow-ups')
    expect(links).toContainEqual({ to: '/chat/c-1', text: 'Open chat', label: 'Open chat with Aisyah' })
    expect(links).toContainEqual({ to: '/chat/c-2', text: 'Open chat', label: 'Open chat with Ben' })
    expect(links.at(-1)).toMatchObject({ to: '/crm/tasks', text: 'Open follow-ups' })
  })

  it('hides the chat link without chat access and caps the list at five rows', async () => {
    grant('tasks')
    mocks.allTasks.mockResolvedValue(
      Array.from({ length: 7 }, (_, index) =>
        task({ id: `t-${index}`, title: `Task ${index}`, due_at: at(-1 - index), contact_id: `c-${index}` }),
      ),
    )
    wrapper = mountToday()
    await flushPromises()

    expect(card('follow-ups').get('[data-testid="today-card-count"]').text()).toBe('7')
    expect(card('follow-ups').findAll('[data-testid="today-row"]')).toHaveLength(5)
    expect(linksIn('follow-ups').map((link) => link.text)).toEqual(['Open follow-ups'])
  })

  it('counts follow-ups that are in progress', async () => {
    grant('tasks', 'crm.leads')
    mocks.allTasks.mockImplementation(async (params: { status: string }) =>
      params.status === 'in_progress'
        ? [
            task({ id: 't-busy', title: 'Chase reply', status: 'in_progress', due_at: at(-1) }),
            task({ id: 't-later', title: 'Later one', status: 'in_progress', lead_id: 'lead-busy', due_at: at(4) }),
          ]
        : [task({ id: 't-open', title: 'Book visit', due_at: at(0) })],
    )
    mocks.allLeads.mockResolvedValue([
      lead({ id: 'lead-busy', title: 'Being handled' }),
      lead({ id: 'lead-free', title: 'Nobody on it' }),
    ])
    wrapper = mountToday()
    await flushPromises()

    const followUps = card('follow-ups')
    expect(followUps.get('[data-testid="today-card-count"]').text()).toBe('2')
    expect(followUps.get('[data-testid="today-group-overdue"]').text()).toBe('Overdue (1)')
    expect(followUps.text()).toContain('Chase reply')
    expect(followUps.text()).toContain('Book visit')

    const leads = card('leads')
    expect(leads.get('[data-testid="today-card-count"]').text()).toBe('1')
    expect(leads.text()).toContain('Nobody on it')
    expect(leads.text()).not.toContain('Being handled')
  })

  it('lists open leads that have no open task and no next step date', async () => {
    grant('tasks', 'crm.leads', 'crm.pipelines')
    mocks.allTasks.mockResolvedValue([task({ id: 't-1', lead_id: 'lead-with-task', due_at: at(5) })])
    mocks.allLeads.mockResolvedValue([
      lead({ id: 'lead-with-task', title: 'Has a task' }),
      lead({ id: 'lead-with-date', title: 'Has a date', next_action_at: at(2) }),
      lead({ id: 'lead-alone', pipeline_id: 'pipe-9', title: 'Needs a call', contact: { id: 'c-9', profile_name: 'Chen' } }),
    ])
    wrapper = mountToday()
    await flushPromises()

    expect(mocks.allTasks).toHaveBeenCalledTimes(2)
    const leads = card('leads')
    expect(leads.get('[data-testid="today-card-count"]').text()).toBe('1')
    expect(leads.text()).toContain('Needs a call')
    expect(leads.text()).toContain('Chen')
    expect(leads.text()).not.toContain('Has a task')
    expect(leads.text()).not.toContain('Has a date')
    expect(linksIn('leads')[0]).toMatchObject({
      to: '/crm/pipeline?pipeline=pipe-9&lead=lead-alone',
      text: 'Needs a call',
    })
    expect(linksIn('leads').at(-1)).toMatchObject({ to: '/crm/pipeline', text: 'Open pipeline' })
  })

  it('shows leads as plain text when the user cannot open the pipeline', async () => {
    grant('tasks', 'crm.leads')
    mocks.allLeads.mockResolvedValue([lead({ id: 'lead-alone', title: 'Needs a call' })])
    wrapper = mountToday()
    await flushPromises()

    expect(card('leads').text()).toContain('Needs a call')
    expect(linksIn('leads')).toEqual([])
    expect(wrapper.find('[data-testid="today-card-leads-link"]').exists()).toBe(false)
  })

  it('explains when follow-ups could not be checked for the leads card', async () => {
    grant('tasks', 'crm.leads')
    mocks.allTasks.mockRejectedValue(new Error('down'))
    mocks.allLeads.mockResolvedValue([lead({ id: 'lead-1', title: 'Lonely lead' })])
    wrapper = mountToday()
    await flushPromises()

    expect(card('follow-ups').find('[data-testid="today-card-error"]').exists()).toBe(true)
    expect(card('leads').text()).toContain('Lonely lead')
    expect(card('leads').get('[data-testid="today-card-note"]').text()).toContain('could not be checked')
  })

  it('shows recent won leads without a linked invoice and opens the invoice dialog', async () => {
    grant('crm.leads', 'payments', 'payments:write', 'contacts')
    entitlements.add('commerce.enabled')
    mocks.allLeads.mockImplementation(async (params: { status: string }) =>
      params.status === 'won'
        ? [
            lead({ id: 'won-invoiced', title: 'Already billed', status: 'won', won_at: daysAgo(3) }),
            lead({ id: 'won-new', title: 'Facial package', status: 'won', won_at: daysAgo(2), contact: { id: 'contact-1', profile_name: 'Dana' } }),
            lead({ id: 'won-old', title: 'Old win', status: 'won', won_at: daysAgo(45) }),
            lead({ id: 'won-voided', title: 'Voided bill', status: 'won', won_at: daysAgo(1) }),
          ]
        : [],
    )
    mocks.allInvoices.mockResolvedValue([
      invoice({ id: 'inv-9', metadata: { lead_id: 'won-invoiced' } }),
      invoice({ id: 'inv-void', status: 'void', metadata: { lead_id: 'won-voided' } }),
    ])
    wrapper = mountToday()
    await flushPromises()

    expect(mocks.allLeads).toHaveBeenCalledWith({ status: 'won' })
    // One request for recent invoices (won card) and one for open ones (unpaid card).
    expect(mocks.allInvoices).toHaveBeenCalledTimes(2)
    expect(mocks.allInvoices).toHaveBeenCalledWith({ status: 'open' })
    const [invoiceParams] = mocks.allInvoices.mock.calls.find(([params]) => params && 'from' in params)!
    expect(Object.keys(invoiceParams)).toEqual(['from'])
    const lookbackDays = (Date.now() - new Date(invoiceParams.from).getTime()) / (24 * 60 * 60 * 1000)
    expect(lookbackDays).toBeGreaterThan(30)
    expect(lookbackDays).toBeLessThan(90)
    const won = card('won')
    expect(won.get('[data-testid="today-card-count"]').text()).toBe('2')
    expect(won.text()).toContain('Voided bill')
    expect(won.text()).toContain('Facial package')
    expect(won.text()).not.toContain('Already billed')
    expect(won.text()).not.toContain('Old win')

    const dialog = wrapper.getComponent(InvoiceDialogStub)
    expect(dialog.props('source')).toBe('today')
    expect(dialog.props('open')).toBe(false)
    await won.get('button[aria-label="Create invoice for Facial package"]').trigger('click')
    expect(dialog.props('open')).toBe(true)
    expect(dialog.props('contact')).toEqual({ id: 'contact-1', name: 'Dana' })
    expect((dialog.props('lead') as CRMLead).id).toBe('won-new')

    dialog.vm.$emit('created', invoice({ id: 'inv-new', metadata: { lead_id: 'won-new' } }))
    await flushPromises()
    expect(card('won').get('[data-testid="today-card-count"]').text()).toBe('1')
    expect(card('won').text()).not.toContain('Facial package')
  })

  it('links won leads to the pipeline when the user cannot create invoices', async () => {
    grant('crm.leads', 'crm.pipelines', 'payments')
    entitlements.add('commerce.enabled')
    mocks.allLeads.mockImplementation(async (params: { status: string }) =>
      params.status === 'won' ? [lead({ id: 'won-1', title: 'Peel', status: 'won', won_at: daysAgo(1) })] : [],
    )
    wrapper = mountToday()
    await flushPromises()

    expect(wrapper.findComponent(InvoiceDialogStub).exists()).toBe(false)
    expect(card('won').find('button[aria-label^="Create invoice"]').exists()).toBe(false)
    expect(linksIn('won')).toContainEqual(
      expect.objectContaining({ to: '/crm/pipeline?pipeline=pipe-1&lead=won-1', text: 'Open in pipeline' }),
    )
  })

  it('loads open invoices and recent invoices separately when both cards show', async () => {
    grant('crm.leads', 'payments')
    entitlements.add('commerce.enabled')
    mocks.allLeads.mockImplementation(async (params: { status: string }) =>
      params.status === 'won' ? [lead({ id: 'won-1', title: 'Peel', status: 'won', won_at: daysAgo(1) })] : [],
    )
    wrapper = mountToday()
    await flushPromises()

    expect(mocks.allInvoices).toHaveBeenCalledTimes(2)
    expect(mocks.allInvoices).toHaveBeenCalledWith({ status: 'open' })
    expect(mocks.allInvoices).toHaveBeenCalledWith({ from: expect.any(String) })
    expect(card('won').text()).toContain('Peel')
    expect(linksIn('won')).toEqual([])
    expect(wrapper.find('[data-testid="today-card-won-link"]').exists()).toBe(false)
  })

  it('totals unpaid invoices per currency', async () => {
    grant('payments')
    entitlements.add('commerce.enabled')
    mocks.allInvoices.mockResolvedValue([
      invoice({ id: 'a', invoice_number: 'INV-A', currency: 'MYR', due_minor: 10000 }),
      invoice({ id: 'b', invoice_number: 'INV-B', currency: 'MYR', due_minor: 5050 }),
      invoice({ id: 'c', invoice_number: 'INV-C', currency: 'SGD', due_minor: 2000 }),
      invoice({ id: 'd', invoice_number: 'INV-PAID', currency: 'MYR', due_minor: 0, status: 'paid' }),
      invoice({ id: 'e', invoice_number: 'INV-VOID', currency: 'MYR', due_minor: 999, status: 'void' }),
    ])
    wrapper = mountToday()
    await flushPromises()

    expect(mocks.allLeads).not.toHaveBeenCalled()
    expect(mocks.allInvoices).toHaveBeenCalledTimes(1)
    expect(mocks.allInvoices).toHaveBeenCalledWith({ status: 'open' })
    const unpaid = card('unpaid')
    expect(unpaid.get('[data-testid="today-card-count"]').text()).toBe('3')
    const summary = unpaid.get('[data-testid="today-card-summary"]').text()
    expect(summary).toContain(formatCurrencyMinorUnits('MYR', 15050))
    expect(summary).toContain(formatCurrencyMinorUnits('SGD', 2000))
    expect(unpaid.text()).toContain('INV-A')
    expect(unpaid.text()).not.toContain('INV-PAID')
    expect(unpaid.text()).not.toContain('INV-VOID')
    expect(linksIn('unpaid').at(-1)).toMatchObject({ to: '/commerce?tab=invoices', text: 'Open invoices' })
  })

  it('shows an error with a Try again button that reloads only that card', async () => {
    grant('payments', 'tasks')
    entitlements.add('commerce.enabled')
    mocks.allInvoices
      .mockRejectedValueOnce(new Error('Network down'))
      .mockResolvedValueOnce([invoice({ id: 'a', invoice_number: 'INV-A', due_minor: 100 })])
    wrapper = mountToday()
    await flushPromises()

    const unpaid = card('unpaid')
    expect(unpaid.get('[data-testid="today-card-error"]').text()).toContain('Invoices could not be loaded.')
    expect(card('follow-ups').find('[data-testid="today-card-error"]').exists()).toBe(false)

    const retry = unpaid.findAll('button').find((button) => button.text() === 'Try again')
    expect(retry).toBeDefined()
    await retry!.trigger('click')
    await flushPromises()

    expect(mocks.allInvoices).toHaveBeenCalledTimes(2)
    // Follow-ups were loaded once (open + in progress) and not reloaded.
    expect(mocks.allTasks).toHaveBeenCalledTimes(2)
    expect(card('unpaid').find('[data-testid="today-card-error"]').exists()).toBe(false)
    expect(card('unpaid').get('[data-testid="today-card-count"]').text()).toBe('1')
  })

  it('reads the unread count from the store without any request', async () => {
    grant('conversations', 'channel_accounts')
    entitlements.add('omnichannel.enabled')
    unreadState.value!.unreadConversationCount = 7
    wrapper = mountToday()
    await flushPromises()

    const unread = card('unread')
    expect(unread.get('[data-testid="today-card-count"]').text()).toBe('7')
    expect(unread.text()).toContain('7 conversations are waiting for a reply.')
    expect(linksIn('unread')).toEqual([expect.objectContaining({ to: '/inbox', text: 'Open inbox' })])
    expect(mocks.unreadRefresh).not.toHaveBeenCalled()
    for (const fn of [mocks.allTasks, mocks.allLeads, mocks.allInvoices, mocks.allEvents, mocks.allBookings]) {
      expect(fn).not.toHaveBeenCalled()
    }

    unreadState.value!.unreadConversationCount = 0
    await flushPromises()
    expect(card('unread').text()).toContain('All caught up')
  })

  it('hides the unread card without inbox access', async () => {
    grant('conversations')
    entitlements.add('omnichannel.enabled')
    wrapper = mountToday()
    await flushPromises()
    expect(wrapper.find('[data-testid="today-card-unread"]').exists()).toBe(false)
  })

  it('lists today’s appointments with the customer and service', async () => {
    grant('bookings', 'booking.settings')
    entitlements.add('bookings.enabled')
    mocks.allEvents.mockResolvedValue([
      { id: 'ev-2', service_id: 's', resource_id: 'r', starts_at: at(0, 15), ends_at: at(0, 16), capacity: 1, booked_quantity: 1, status: 'scheduled', service: { name: 'Consultation' }, version: 1 },
      { id: 'ev-1', service_id: 's', resource_id: 'r', starts_at: at(0, 9), ends_at: at(0, 10), capacity: 1, booked_quantity: 0, status: 'scheduled', service: { name: 'Laser' }, version: 1 },
      { id: 'ev-x', service_id: 's', resource_id: 'r', starts_at: at(0, 11), ends_at: at(0, 12), capacity: 1, status: 'cancelled', service: { name: 'Cancelled one' }, version: 1 },
    ])
    mocks.allBookings.mockResolvedValue([
      { id: 'b-1', event_id: 'ev-2', contact_id: 'c-1', status: 'confirmed', quantity: 1, source: 'agent', created_at: at(-1), version: 1, contact: { id: 'c-1', profile_name: 'Farah' } },
    ])
    wrapper = mountToday()
    await flushPromises()

    const [params] = mocks.allEvents.mock.calls[0]
    expect(new Date(params.from).getHours()).toBe(0)
    expect(new Date(params.to).getDate()).toBe(new Date().getDate())
    expect(mocks.allBookings).toHaveBeenCalledTimes(1)
    expect(mocks.allBookings).toHaveBeenCalledWith({ event_id: 'ev-2' })

    const appointments = card('appointments')
    expect(appointments.get('[data-testid="today-card-count"]').text()).toBe('2')
    const rows = appointments.findAll('[data-testid="today-row"]').map((row) => row.text())
    expect(rows[0]).toContain('Laser')
    expect(rows[1]).toContain('Consultation')
    expect(rows[1]).toContain('Farah')
    expect(appointments.text()).not.toContain('Cancelled one')
    expect(linksIn('appointments').at(-1)).toMatchObject({ to: '/calendar', text: 'Open calendar' })
  })

  it('hides the calendar link when the user cannot open the calendar', async () => {
    grant('bookings')
    entitlements.add('bookings.enabled')
    wrapper = mountToday()
    await flushPromises()

    expect(card('appointments').text()).toContain('No appointments today.')
    expect(wrapper.find('[data-testid="today-card-appointments-link"]').exists()).toBe(false)
  })
})
