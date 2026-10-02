/** @vitest-environment happy-dom */

import { flushPromises, mount, shallowMount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CustomerRevenueWorkspace from './CustomerRevenueWorkspace.vue'
import LeadCreateDialog from '@/components/crm/LeadCreateDialog.vue'
import LeadOutcomeDialog from '@/components/crm/LeadOutcomeDialog.vue'
import InvoiceQuickDialog from '@/components/commerce/InvoiceQuickDialog.vue'
import ContactBookingDialog from '@/components/booking/ContactBookingDialog.vue'
import ContactInfoPanel from './ContactInfoPanel.vue'

const mocks = vi.hoisted(() => ({
  getWorkspace: vi.fn(),
  pipelines: vi.fn(),
  createLead: vi.fn(),
  createTask: vi.fn(),
  moveLead: vi.fn(),
  completeTask: vi.fn(),
  runCopilot: vi.fn(),
  hasPermission: vi.fn(),
  hasProductEntitlement: vi.fn(),
  toastError: vi.fn(),
  toastSuccess: vi.fn(),
  toastInfo: vi.fn(),
  toastWarning: vi.fn(),
}))

vi.mock('@/services/productSuite', () => ({
  commerceService: {
    createInvoice: vi.fn(),
    sellPackage: vi.fn(),
    allPackages: vi.fn(),
  },
  copilotService: { run: mocks.runCopilot },
  crmService: {
    createLead: mocks.createLead,
    createTask: mocks.createTask,
    moveLead: mocks.moveLead,
    completeTask: mocks.completeTask,
    pipelines: mocks.pipelines,
  },
  customerWorkspaceService: { get: mocks.getWorkspace },
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    hasPermission: mocks.hasPermission,
    hasProductEntitlement: mocks.hasProductEntitlement,
    organizationId: 'org-test',
  }),
}))

vi.mock('@/stores/tags', () => ({
  useTagsStore: () => ({ tags: [], fetchTags: vi.fn(), getTagByName: () => undefined }),
}))

vi.mock('@/composables/useAppToast', () => ({
  useAppToast: () => ({
    error: mocks.toastError,
    success: mocks.toastSuccess,
    info: mocks.toastInfo,
    warning: mocks.toastWarning,
  }),
}))

const Passthrough = defineComponent({ template: '<div><slot /></div>' })

const DialogStub = defineComponent({
  props: { open: Boolean },
  template: '<div v-if="open"><slot /></div>',
})

const ButtonStub = defineComponent({
  inheritAttrs: false,
  props: { disabled: Boolean, loading: Boolean, type: String },
  emits: ['click'],
  template:
    '<button v-bind="$attrs" :type="type || \'button\'" :disabled="disabled || loading" @click="$emit(\'click\')"><slot /></button>',
})

const RouterLinkStub = defineComponent({
  props: { to: [String, Object] },
  template: '<a :data-to="typeof to === \'string\' ? to : JSON.stringify(to)"><slot /></a>',
})

function stage(id: string, name: string, kind: 'open' | 'won' | 'lost', order: number) {
  return {
    id,
    pipeline_id: 'pipeline-1',
    name,
    color: '#67e8f9',
    display_order: order,
    kind,
    probability: 0,
    sla_hours: 0,
    is_active: true,
    version: 1,
  }
}

const pipeline = {
  id: 'pipeline-1',
  name: 'Treatments',
  is_default: true,
  is_active: true,
  stages: [
    stage('stage-new', 'New', 'open', 1),
    stage('stage-contacted', 'Contacted', 'open', 2),
    stage('stage-qualified', 'Qualified', 'open', 3),
    stage('stage-won', 'Converted', 'won', 4),
    stage('stage-lost', 'Not proceeding', 'lost', 5),
  ],
}

function lead(overrides: Record<string, unknown> = {}) {
  return {
    id: 'lead-1',
    contact_id: 'contact-1',
    pipeline_id: 'pipeline-1',
    stage_id: 'stage-new',
    title: 'Test Customer - Treatments',
    status: 'open',
    value_minor: 370000,
    currency: 'MYR',
    version: 3,
    pipeline: { id: 'pipeline-1', name: 'Treatments' },
    stage: { id: 'stage-new', name: 'New', kind: 'open' },
    updated_at: '2026-09-30T00:00:00Z',
    ...overrides,
  }
}

function workspaceResponse(overrides: Record<string, unknown> = {}, contact: Record<string, unknown> = {}) {
  return {
    data: {
      data: {
        contact: {
          id: 'contact-1',
          phone_number: '+10000000000',
          profile_name: 'Test Customer',
          identity_review_ai_state: {
            known: true,
            ai_allowed: true,
            blocked: false,
            open_hold_count: 0,
            reason: 'no_open_identity_review',
          },
          ...contact,
        },
        identities: [],
        journeys: [],
        tasks: [],
        bookings: [],
        packages: [],
        invoices: [],
        payments: [],
        timeline: [],
        ...overrides,
      },
    },
  }
}

function mountWorkspace(props: Record<string, unknown> = {}) {
  return mount(CustomerRevenueWorkspace, {
    props: { contactId: 'contact-1', ...props },
    global: {
      stubs: {
        RouterLink: RouterLinkStub,
        Button: ButtonStub,
        Avatar: Passthrough,
        AvatarImage: true,
        AvatarFallback: Passthrough,
        ScrollArea: Passthrough,
        Tabs: Passthrough,
        TabsContent: Passthrough,
        TabsList: Passthrough,
        TabsTrigger: Passthrough,
        Dialog: DialogStub,
        DialogContent: Passthrough,
        DialogDescription: Passthrough,
        DialogFooter: Passthrough,
        DialogHeader: Passthrough,
        DialogTitle: Passthrough,
        ContactInfoPanel: true,
        LeadCreateDialog: true,
        LeadOutcomeDialog: true,
        InvoiceQuickDialog: true,
        ContactBookingDialog: true,
      },
    },
  })
}

function nextStep(wrapper: ReturnType<typeof mountWorkspace>) {
  return wrapper.get('[data-testid="lead-next-step"]')
}

beforeEach(() => {
  mocks.getWorkspace.mockReset().mockResolvedValue(workspaceResponse())
  mocks.pipelines.mockReset().mockResolvedValue({ data: { data: { pipelines: [pipeline] } } })
  mocks.createLead.mockReset().mockResolvedValue({ data: { data: { id: 'lead-new' } } })
  mocks.createTask.mockReset().mockResolvedValue({ data: { data: { id: 'task-new' } } })
  mocks.moveLead.mockReset().mockResolvedValue({ data: { data: {} } })
  mocks.completeTask.mockReset().mockResolvedValue({ data: { data: {} } })
  mocks.runCopilot.mockReset()
  mocks.hasPermission.mockReset().mockReturnValue(true)
  mocks.hasProductEntitlement.mockReset().mockReturnValue(true)
  mocks.toastError.mockReset()
  mocks.toastSuccess.mockReset()
  mocks.toastInfo.mockReset()
  mocks.toastWarning.mockReset()
  try {
    localStorage.clear()
  } catch {
    // storage may be unavailable
  }
})

describe('CustomerRevenueWorkspace next step', () => {
  it('asks to add a customer without a lead to the pipeline', async () => {
    const wrapper = mountWorkspace()
    await flushPromises()

    expect(nextStep(wrapper).attributes('data-state')).toBe('none')
    expect(nextStep(wrapper).text()).toContain('Not in the pipeline yet')
    expect(mocks.pipelines).not.toHaveBeenCalled()

    await wrapper.get('[data-testid="add-to-pipeline"]').trigger('click')
    await flushPromises()

    const dialog = wrapper.findComponent(LeadCreateDialog)
    expect(dialog.props('open')).toBe(true)
    expect(dialog.props('contact')).toEqual({ id: 'contact-1', name: 'Test Customer' })
    expect(dialog.props('defaultPipelineId')).toBe('pipeline-1')
    expect(mocks.pipelines).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).not.toMatch(/journey/i)
  })

  it('explains when the user cannot add leads', async () => {
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      !(resource === 'crm.leads' && action === 'write'),
    )
    const wrapper = mountWorkspace()
    await flushPromises()

    expect(wrapper.find('[data-testid="add-to-pipeline"]').exists()).toBe(false)
    expect(nextStep(wrapper).text()).toContain('You do not have access to add leads.')
  })

  it('creates the lead with source, conversation link and a follow-up task', async () => {
    const wrapper = mountWorkspace({ surface: 'omnichannel', channel: 'whatsapp', conversationId: 'conversation-1' })
    await flushPromises()
    await wrapper.get('[data-testid="add-to-pipeline"]').trigger('click')
    await flushPromises()

    wrapper.findComponent(LeadCreateDialog).vm.$emit('submit', {
      contact_id: 'contact-1',
      contact_name: 'Test Customer',
      pipeline_id: 'pipeline-1',
      stage_id: 'stage-contacted',
      title: 'Test Customer - Treatments',
      value: '120',
      currency: 'MYR',
      follow_up_at: '2099-01-02T10:00',
    })
    await flushPromises()

    expect(mocks.createLead).toHaveBeenCalledWith(expect.objectContaining({
      contact_id: 'contact-1',
      pipeline_id: 'pipeline-1',
      stage_id: 'stage-contacted',
      source: 'whatsapp',
      source_reference: 'conversation:conversation-1',
      value_minor: 12000,
      next_action_at: new Date('2099-01-02T10:00').toISOString(),
    }))
    expect(mocks.createTask).toHaveBeenCalledWith(expect.objectContaining({
      contact_id: 'contact-1',
      lead_id: 'lead-new',
      title: 'Follow up: Test Customer - Treatments',
    }))
    expect(mocks.toastSuccess).toHaveBeenCalledWith('Lead added', 'Added to Treatments - Contacted.')
    expect(localStorage.getItem('rereply.crm.lastPipeline.org-test')).toBe('pipeline-1')
    expect(wrapper.findComponent(LeadCreateDialog).props('open')).toBe(false)
  })

  it('warns when the lead is added but its follow-up fails', async () => {
    mocks.createTask.mockRejectedValueOnce(new Error('nope'))
    const wrapper = mountWorkspace({ surface: 'omnichannel', channel: 'instagram' })
    await flushPromises()
    await wrapper.get('[data-testid="add-to-pipeline"]').trigger('click')
    await flushPromises()

    wrapper.findComponent(LeadCreateDialog).vm.$emit('submit', {
      contact_id: 'contact-1',
      contact_name: 'Test Customer',
      pipeline_id: 'pipeline-1',
      stage_id: 'stage-new',
      title: 'Test lead',
      value: '',
      currency: 'MYR',
      follow_up_at: '2099-01-02T10:00',
    })
    await flushPromises()

    expect(mocks.createLead).toHaveBeenCalledWith(expect.objectContaining({ source: 'other' }))
    expect(mocks.createLead.mock.calls[0][0]).not.toHaveProperty('source_reference')
    expect(mocks.toastWarning).toHaveBeenCalledWith(
      'Lead added, but the follow-up was not scheduled',
      expect.any(String),
    )
  })

  it('shows the stage stepper for an open lead and moves it with the lead version', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({ journeys: [lead()] }))
    const wrapper = mountWorkspace()
    await flushPromises()

    expect(nextStep(wrapper).attributes('data-state')).toBe('open')
    const steps = wrapper.findAll('[data-testid="lead-stage-step"]')
    expect(steps.map((step) => step.text())).toEqual(['New', 'Contacted', 'Qualified'])
    expect(steps[0].attributes('aria-current')).toBe('step')
    expect(wrapper.get('[data-testid="lead-follow-up"]').text()).toContain('No follow-up scheduled')

    await steps[1].trigger('click')
    await flushPromises()

    expect(mocks.moveLead).toHaveBeenCalledWith('lead-1', 'stage-contacted', 3, undefined)
    expect(mocks.toastSuccess).toHaveBeenCalledWith('Moved to Contacted')
    expect(mocks.getWorkspace).toHaveBeenCalledTimes(2)
  })

  it('refreshes when another user changed the lead first', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({ journeys: [lead()] }))
    mocks.moveLead.mockRejectedValueOnce({ isAxiosError: true, response: { status: 409, data: {} } })
    const wrapper = mountWorkspace()
    await flushPromises()

    await wrapper.findAll('[data-testid="lead-stage-step"]')[2].trigger('click')
    await flushPromises()

    expect(mocks.toastInfo).toHaveBeenCalledWith('This lead changed elsewhere. Refreshed.')
    expect(mocks.getWorkspace).toHaveBeenCalledTimes(2)
  })

  it('falls back to a stage badge when pipelines cannot be read', async () => {
    mocks.hasPermission.mockImplementation((resource: string) => resource !== 'crm.pipelines')
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({ journeys: [lead()] }))
    const wrapper = mountWorkspace()
    await flushPromises()

    expect(mocks.pipelines).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="lead-stage-stepper"]').exists()).toBe(false)
    expect(nextStep(wrapper).text()).toContain('New')
    expect(wrapper.find('[data-testid="lead-mark-won"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="lead-move-unavailable"]').text()).toBe('Open the pipeline to move this lead.')
    expect(JSON.parse(wrapper.get('[data-testid="lead-view-in-pipeline"]').attributes('data-to')!)).toEqual({
      path: '/crm/pipeline',
      query: { pipeline: 'pipeline-1', lead: 'lead-1' },
    })
  })

  it('fetches pipelines for the stepper once per workspace load and offers a retry when that fails', async () => {
    mocks.pipelines.mockRejectedValueOnce(new Error('offline'))
    mocks.getWorkspace
      .mockResolvedValueOnce(workspaceResponse({ journeys: [lead()] }))
      .mockResolvedValue(workspaceResponse({ journeys: [lead({ id: 'lead-2' })] }))
    const wrapper = mountWorkspace()
    await flushPromises()

    expect(mocks.pipelines).toHaveBeenCalledTimes(1)
    expect(mocks.toastError).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="workspace-pipelines-error"]').text()).toContain('Pipelines could not be loaded.')
    expect(wrapper.get('[data-testid="lead-move-unavailable"]').text()).toBe('Open the pipeline to move this lead.')

    // A silent refresh with a different focus lead does not fetch again.
    await (wrapper.vm as any).$.setupState.loadWorkspace(true)
    await flushPromises()
    expect(mocks.pipelines).toHaveBeenCalledTimes(1)

    await wrapper.get('[data-testid="workspace-pipelines-retry"]').trigger('click')
    await flushPromises()
    expect(mocks.pipelines).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid="workspace-pipelines-error"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="lead-move-unavailable"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid="lead-stage-step"]')).toHaveLength(3)
  })

  it('fetches pipelines again after switching to another customer', async () => {
    mocks.pipelines.mockRejectedValueOnce(new Error('offline'))
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({ journeys: [lead()] }))
    const wrapper = mountWorkspace()
    await flushPromises()
    expect(mocks.pipelines).toHaveBeenCalledTimes(1)

    mocks.getWorkspace.mockResolvedValue(workspaceResponse(
      { journeys: [lead({ id: 'lead-b', contact_id: 'contact-2' })] },
      { id: 'contact-2' },
    ))
    await wrapper.setProps({ contactId: 'contact-2' })
    await flushPromises()

    expect(mocks.pipelines).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid="workspace-pipelines-error"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid="lead-stage-step"]')).toHaveLength(3)
  })

  it('closes the add-lead form when pipelines fail to load and reopens it after a retry', async () => {
    mocks.pipelines.mockRejectedValueOnce(new Error('offline'))
    const wrapper = mountWorkspace()
    await flushPromises()

    await wrapper.get('[data-testid="add-to-pipeline"]').trigger('click')
    await flushPromises()

    expect(wrapper.findComponent(LeadCreateDialog).props('open')).toBe(false)
    const alert = wrapper.get('[data-testid="workspace-pipelines-error"]')
    expect(alert.attributes('role')).toBe('alert')
    expect(alert.text()).toContain('Pipelines could not be loaded.')
    expect(alert.text()).toContain('Try again')

    await wrapper.get('[data-testid="workspace-pipelines-retry"]').trigger('click')
    await flushPromises()

    expect(mocks.pipelines).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid="workspace-pipelines-error"]').exists()).toBe(false)
    const dialog = wrapper.findComponent(LeadCreateDialog)
    expect(dialog.props('open')).toBe(true)
    expect(dialog.props('pipelines')).toHaveLength(1)
  })

  it('explains why Won is unavailable when the pipeline has no Won stage', async () => {
    mocks.pipelines.mockResolvedValue({
      data: { data: { pipelines: [{ ...pipeline, stages: pipeline.stages.filter((item) => item.kind !== 'won') }] } },
    })
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({ journeys: [lead()] }))
    const wrapper = mountWorkspace()
    await flushPromises()

    const won = wrapper.get('[data-testid="lead-mark-won"]')
    expect(won.attributes('disabled')).toBeDefined()
    expect(won.attributes('aria-describedby')).toBe('workspace-outcome-unavailable')
    const reason = wrapper.get('[data-testid="lead-outcome-unavailable"]')
    expect(reason.text()).toBe('This pipeline has no Won stage. Ask an admin to add one.')
    expect(reason.attributes('id')).toBe('workspace-outcome-unavailable')
    expect(wrapper.get('[data-testid="lead-mark-lost"]').attributes('disabled')).toBeUndefined()
  })

  it('shows no unavailable reason when Won and Lost both work', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({ journeys: [lead()] }))
    const wrapper = mountWorkspace()
    await flushPromises()

    expect(wrapper.find('[data-testid="lead-outcome-unavailable"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="lead-move-unavailable"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="lead-mark-won"]').attributes('aria-describedby')).toBeUndefined()
  })

  it('marks a lead as won and opens the invoice form for it', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({ journeys: [lead()] }))
    const wrapper = mountWorkspace()
    await flushPromises()

    await wrapper.get('[data-testid="lead-mark-won"]').trigger('click')
    const outcome = wrapper.findComponent(LeadOutcomeDialog)
    expect(outcome.props('open')).toBe(true)
    expect(outcome.props('outcome')).toBe('won')
    expect(outcome.props('stageName')).toBe('Converted')
    expect(outcome.props('canInvoice')).toBe(true)

    outcome.vm.$emit('confirm', { createInvoice: true })
    await flushPromises()

    expect(mocks.moveLead).toHaveBeenCalledWith('lead-1', 'stage-won', 3, undefined)
    expect(mocks.toastSuccess).toHaveBeenCalledWith('Marked as won', 'Moved to Converted.')
    expect(wrapper.findComponent(LeadOutcomeDialog).props('open')).toBe(false)
    const invoice = wrapper.findComponent(InvoiceQuickDialog)
    expect(invoice.props('open')).toBe(true)
    expect(invoice.props('contact')).toEqual({ id: 'contact-1', name: 'Test Customer' })
    expect(invoice.props('lead')).toEqual(expect.objectContaining({ id: 'lead-1', status: 'won' }))
  })

  it('does not open the invoice form when the user unticks it', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({ journeys: [lead()] }))
    const wrapper = mountWorkspace()
    await flushPromises()

    await wrapper.get('[data-testid="lead-mark-won"]').trigger('click')
    wrapper.findComponent(LeadOutcomeDialog).vm.$emit('confirm', { createInvoice: false })
    await flushPromises()

    expect(mocks.moveLead).toHaveBeenCalledTimes(1)
    expect(wrapper.findComponent(InvoiceQuickDialog).props('open')).toBe(false)
  })

  it('passes the lost reason when marking a lead as lost', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({ journeys: [lead()] }))
    const wrapper = mountWorkspace()
    await flushPromises()

    await wrapper.get('[data-testid="lead-mark-lost"]').trigger('click')
    expect(wrapper.findComponent(LeadOutcomeDialog).props('outcome')).toBe('lost')
    wrapper.findComponent(LeadOutcomeDialog).vm.$emit('confirm', { reason: 'Price: over budget' })
    await flushPromises()

    expect(mocks.moveLead).toHaveBeenCalledWith('lead-1', 'stage-lost', 3, 'Price: over budget')
    expect(mocks.toastSuccess).toHaveBeenCalledWith('Marked as lost', 'Moved to Not proceeding.')
  })

  it('offers Create invoice for a won lead without an invoice', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead({ status: 'won', stage_id: 'stage-won', won_at: '2026-09-30T00:00:00Z' })],
    }))
    const wrapper = mountWorkspace()
    await flushPromises()

    expect(nextStep(wrapper).attributes('data-state')).toBe('won')
    await wrapper.get('[data-testid="create-invoice"]').trigger('click')
    expect(wrapper.findComponent(InvoiceQuickDialog).props('open')).toBe(true)
    expect(wrapper.findComponent(InvoiceQuickDialog).props('lead')).toEqual(expect.objectContaining({ id: 'lead-1' }))
  })

  it('asks for billing access instead of offering an invoice', async () => {
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      !(resource === 'payments' && action === 'write'),
    )
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead({ status: 'won', stage_id: 'stage-won' })],
    }))
    const wrapper = mountWorkspace()
    await flushPromises()

    expect(wrapper.find('[data-testid="create-invoice"]').exists()).toBe(false)
    expect(nextStep(wrapper).text()).toContain('Ask a manager with billing access to create the invoice.')
  })

  it('shows the linked invoice for a won lead', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead({ status: 'won', stage_id: 'stage-won' })],
      invoices: [{
        id: 'invoice-1',
        contact_id: 'contact-1',
        invoice_number: 'INV-0001',
        status: 'open',
        currency: 'MYR',
        total_minor: 12000,
        paid_minor: 0,
        due_minor: 12000,
        issued_at: '2026-09-30T00:00:00Z',
        version: 1,
        metadata: { lead_id: 'lead-1' },
      }],
    }))
    const wrapper = mountWorkspace()
    await flushPromises()

    expect(wrapper.find('[data-testid="create-invoice"]').exists()).toBe(false)
    const invoice = wrapper.get('[data-testid="lead-invoice"]')
    expect(invoice.text()).toContain('Invoice INV-0001')
    expect(invoice.text()).toContain('Unpaid')
    expect(invoice.find('a').attributes('data-to')).toBe('/commerce?tab=invoices')
  })

  it('reopens a lost lead in the first open stage', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead({ status: 'lost', stage_id: 'stage-lost', lost_reason: 'Timing not right' })],
    }))
    const wrapper = mountWorkspace()
    await flushPromises()

    expect(nextStep(wrapper).attributes('data-state')).toBe('lost')
    expect(nextStep(wrapper).text()).toContain('Timing not right')
    await wrapper.get('[data-testid="lead-reopen"]').trigger('click')
    await flushPromises()

    expect(mocks.moveLead).toHaveBeenCalledWith('lead-1', 'stage-new', 3, undefined)
    expect(mocks.toastSuccess).toHaveBeenCalledWith('Lead reopened', 'Moved to New.')
  })

  it('links the focus lead to the pipeline page and hides the lead list when it is the only lead', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({ journeys: [lead()] }))
    const wrapper = mountWorkspace()
    await flushPromises()

    const link = nextStep(wrapper).get('[data-testid="lead-view-in-pipeline"]')
    expect(link.text()).toContain('View in pipeline')
    expect(JSON.parse(link.attributes('data-to')!)).toEqual({
      path: '/crm/pipeline',
      query: { pipeline: 'pipeline-1', lead: 'lead-1' },
    })
    expect(wrapper.find('[data-testid="workspace-lead"]').exists()).toBe(false)
    expect(wrapper.find('#workspace-leads-title').exists()).toBe(false)
  })

  it('lists only the leads other than the focus lead', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [
        lead({ last_activity_at: '2026-09-30T10:00:00Z' }),
        lead({ id: 'lead-old', title: 'Earlier lead', status: 'lost', stage_id: 'stage-lost' }),
      ],
    }))
    const wrapper = mountWorkspace()
    await flushPromises()

    const items = wrapper.findAll('[data-testid="workspace-lead"]')
    expect(items).toHaveLength(1)
    expect(items[0].text()).toContain('Earlier lead')
    expect(JSON.parse(items[0].get('a').attributes('data-to')!)).toEqual({
      path: '/crm/pipeline',
      query: { pipeline: 'pipeline-1', lead: 'lead-old' },
    })
  })

  it('links won and lost leads to the pipeline page from the Next step card', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead({ status: 'won', stage_id: 'stage-won' })],
    }))
    const won = mountWorkspace()
    await flushPromises()
    expect(nextStep(won).find('[data-testid="lead-view-in-pipeline"]').exists()).toBe(true)
    won.unmount()

    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead({ status: 'lost', stage_id: 'stage-lost' })],
    }))
    const lost = mountWorkspace()
    await flushPromises()
    expect(nextStep(lost).find('[data-testid="lead-view-in-pipeline"]').exists()).toBe(true)
    lost.unmount()

    mocks.getWorkspace.mockResolvedValue(workspaceResponse())
    const none = mountWorkspace()
    await flushPromises()
    expect(nextStep(none).find('[data-testid="lead-view-in-pipeline"]').exists()).toBe(false)
  })

  it('ticks off an overdue follow-up from the Next step card', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead()],
      tasks: [{
        id: 'task-late',
        lead_id: 'lead-1',
        title: 'Call back',
        status: 'open',
        priority: 'normal',
        due_at: '2020-01-01T09:00:00Z',
        version: 4,
      }],
    }))
    const wrapper = mountWorkspace()
    await flushPromises()

    const row = wrapper.get('[data-testid="lead-follow-up"]')
    expect(row.attributes('data-urgency')).toBe('overdue')
    expect(row.get('[data-testid="lead-set-follow-up"]').text()).toBe('Set a new date')
    const done = row.get('[data-testid="lead-follow-up-done"]')
    expect(done.text()).toContain('Done')

    await done.trigger('click')
    await flushPromises()

    expect(mocks.completeTask).toHaveBeenCalledWith('task-late', 4)
    expect(mocks.toastSuccess).toHaveBeenCalledWith('Follow-up done', 'Call back')
    expect(mocks.getWorkspace).toHaveBeenCalledTimes(2)
  })

  it('ticks off an undated follow-up, but not an upcoming one', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead()],
      tasks: [{ id: 'task-undated', lead_id: 'lead-1', title: 'Send prices', status: 'open', priority: 'normal', version: 2 }],
    }))
    const undated = mountWorkspace()
    await flushPromises()
    expect(undated.get('[data-testid="lead-set-follow-up"]').text()).toBe('Set a new date')
    await undated.get('[data-testid="lead-follow-up-done"]').trigger('click')
    await flushPromises()
    expect(mocks.completeTask).toHaveBeenCalledWith('task-undated', 2)
    undated.unmount()

    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead()],
      tasks: [{ id: 'task-later', lead_id: 'lead-1', title: 'Check in', status: 'open', priority: 'normal', due_at: '2099-01-01T10:00:00Z', version: 1 }],
    }))
    const upcoming = mountWorkspace()
    await flushPromises()
    expect(upcoming.find('[data-testid="lead-follow-up-done"]').exists()).toBe(false)
    expect(upcoming.find('[data-testid="lead-set-follow-up"]').exists()).toBe(false)
  })

  it('keeps Set follow-up when there is no follow-up task to tick off', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead({ next_action_at: '2020-01-01T09:00:00Z' })],
    }))
    const wrapper = mountWorkspace()
    await flushPromises()

    expect(wrapper.get('[data-testid="lead-follow-up"]').attributes('data-urgency')).toBe('overdue')
    expect(wrapper.find('[data-testid="lead-follow-up-done"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="lead-set-follow-up"]').text()).toBe('Set follow-up')
  })

  it('offers to mark the remaining follow-ups done after a lead is won', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead({ status: 'won', stage_id: 'stage-won' })],
      tasks: [
        { id: 'task-a', lead_id: 'lead-1', title: 'Call back', status: 'open', priority: 'normal', version: 1 },
        { id: 'task-b', lead_id: 'lead-1', title: 'Send prices', status: 'in_progress', priority: 'normal', version: 5 },
        { id: 'task-c', lead_id: 'lead-1', title: 'Old call', status: 'completed', priority: 'normal', version: 2 },
        { id: 'task-d', lead_id: 'lead-other', title: 'Other lead', status: 'open', priority: 'normal', version: 1 },
      ],
    }))
    const wrapper = mountWorkspace()
    await flushPromises()

    const line = nextStep(wrapper).get('[data-testid="lead-open-follow-ups"]')
    expect(line.text()).toContain('2 open follow-ups for this lead')

    await line.get('[data-testid="lead-open-follow-ups-done"]').trigger('click')
    await flushPromises()

    expect(mocks.completeTask).toHaveBeenCalledTimes(2)
    expect(mocks.completeTask).toHaveBeenNthCalledWith(1, 'task-a', 1)
    expect(mocks.completeTask).toHaveBeenNthCalledWith(2, 'task-b', 5)
    expect(mocks.toastSuccess).toHaveBeenCalledWith('Follow-ups done', '2 follow-ups marked done.')
    expect(mocks.getWorkspace).toHaveBeenCalledTimes(2)
  })

  it('uses the singular and hides Mark done without task write access', async () => {
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      !(resource === 'tasks' && action === 'write'),
    )
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead({ status: 'won', stage_id: 'stage-won' })],
      tasks: [{ id: 'task-a', lead_id: 'lead-1', title: 'Call back', status: 'open', priority: 'normal', version: 1 }],
    }))
    const wrapper = mountWorkspace()
    await flushPromises()

    const line = nextStep(wrapper).get('[data-testid="lead-open-follow-ups"]')
    expect(line.text()).toContain('1 open follow-up for this lead')
    expect(line.find('[data-testid="lead-open-follow-ups-done"]').exists()).toBe(false)
  })

  it('stops marking follow-ups done when another user changed one first', async () => {
    mocks.completeTask
      .mockResolvedValueOnce({ data: { data: {} } })
      .mockRejectedValueOnce({ isAxiosError: true, response: { status: 409, data: {} } })
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead({ status: 'won', stage_id: 'stage-won' })],
      tasks: [
        { id: 'task-a', lead_id: 'lead-1', title: 'Call back', status: 'open', priority: 'normal', version: 1 },
        { id: 'task-b', lead_id: 'lead-1', title: 'Send prices', status: 'open', priority: 'normal', version: 5 },
        { id: 'task-c', lead_id: 'lead-1', title: 'Book visit', status: 'open', priority: 'normal', version: 3 },
      ],
    }))
    const wrapper = mountWorkspace()
    await flushPromises()

    await wrapper.get('[data-testid="lead-open-follow-ups-done"]').trigger('click')
    await flushPromises()

    expect(mocks.completeTask).toHaveBeenCalledTimes(2)
    expect(mocks.toastInfo).toHaveBeenCalledWith('This follow-up changed elsewhere. Refreshed.')
    expect(mocks.getWorkspace).toHaveBeenCalledTimes(2)
  })

  it('completes an open follow-up with its version', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      tasks: [{ id: 'task-1', title: 'Call back', status: 'open', priority: 'normal', version: 7 }],
    }))
    const wrapper = mountWorkspace()
    await flushPromises()

    await wrapper.get('[data-testid="workspace-task-complete"]').trigger('click')
    await flushPromises()

    expect(mocks.completeTask).toHaveBeenCalledWith('task-1', 7)
    expect(mocks.getWorkspace).toHaveBeenCalledTimes(2)
  })

  it('shows one money line only when something is not zero', async () => {
    const empty = mountWorkspace()
    await flushPromises()
    expect(empty.find('[data-testid="workspace-money-summary"]').exists()).toBe(false)
    expect(empty.find('[data-testid="workspace-invoices"]').exists()).toBe(false)
    empty.unmount()

    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      journeys: [lead()],
      summary: { pipeline_value: [{ currency: 'MYR', amount_minor: 370000 }], outstanding: [] },
    }))
    const wrapper = mountWorkspace()
    await flushPromises()
    const line = wrapper.get('[data-testid="workspace-money-summary"]').text()
    expect(line).toContain('Open pipeline')
    expect(line).not.toContain('Unpaid')
  })
})

describe('CustomerRevenueWorkspace requested actions', () => {
  it('waits for the workspace, then runs each requested action once', async () => {
    let resolveWorkspace!: (value: unknown) => void
    mocks.getWorkspace.mockReturnValueOnce(new Promise((resolve) => { resolveWorkspace = resolve }))
    const wrapper = mountWorkspace({ requestedAction: { kind: 'lead', nonce: 1 } })
    await flushPromises()
    expect(wrapper.emitted('action-consumed')).toBeUndefined()

    resolveWorkspace(workspaceResponse())
    await flushPromises()

    expect(wrapper.findComponent(LeadCreateDialog).props('open')).toBe(true)
    expect(wrapper.emitted('action-consumed')).toEqual([[1]])

    await wrapper.setProps({ requestedAction: { kind: 'lead', nonce: 1 } })
    await flushPromises()
    expect(wrapper.emitted('action-consumed')).toEqual([[1]])

    await wrapper.setProps({ requestedAction: { kind: 'follow-up', nonce: 2 } })
    await flushPromises()
    const state = (wrapper.vm as any).$.setupState
    expect(state.showTaskDialog).toBe(true)
    expect(wrapper.emitted('action-consumed')).toEqual([[1], [2]])
  })

  it('opens the booking dialog or explains a missing permission', async () => {
    const wrapper = mountWorkspace({ requestedAction: { kind: 'booking', nonce: 5 } })
    await flushPromises()
    expect(wrapper.findComponent(ContactBookingDialog).props('open')).toBe(true)
    expect(wrapper.emitted('action-consumed')).toEqual([[5]])
    wrapper.unmount()

    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      !(resource === 'tasks' && action === 'write'),
    )
    const denied = mountWorkspace({ requestedAction: { kind: 'follow-up', nonce: 6 } })
    await flushPromises()
    expect(mocks.toastInfo).toHaveBeenCalledWith('You do not have access to schedule follow-ups.')
    expect((denied.vm as any).$.setupState.showTaskDialog).toBe(false)
    expect(denied.emitted('action-consumed')).toEqual([[6]])
  })
})

describe('CustomerRevenueWorkspace follow-up form', () => {
  it('ignores a second save while the first is still saving', async () => {
    let resolveTask!: (value: unknown) => void
    mocks.createTask.mockReturnValueOnce(new Promise((resolve) => { resolveTask = resolve }))
    const wrapper = mountWorkspace()
    await flushPromises()
    const state = (wrapper.vm as any).$.setupState

    state.openFollowUp()
    const first = state.createFollowUp()
    expect(state.savingTask).toBe(true)
    await state.createFollowUp()
    expect(mocks.createTask).toHaveBeenCalledTimes(1)

    resolveTask({ data: { data: { id: 'task-new' } } })
    await first
    await flushPromises()
    expect(mocks.createTask).toHaveBeenCalledTimes(1)
    expect(mocks.toastSuccess).toHaveBeenCalledWith('Follow-up scheduled')
  })
})

describe('CustomerRevenueWorkspace header', () => {
  it('shows the phone number when the customer shared one', async () => {
    const wrapper = mountWorkspace()
    await flushPromises()
    expect(wrapper.get('[data-testid="workspace-contact-phone"]').text()).toBe('+10000000000')
    expect(wrapper.find('[data-testid="workspace-contact-address"]').exists()).toBe(false)
  })

  it('shows a hidden-number badge for WhatsApp username contacts', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({}, {
      phone_number: 'bsuid:abc123',
      metadata: { coexistence_username: 'testcustomer' },
    }))
    const wrapper = mountWorkspace()
    await flushPromises()

    const badge = wrapper.get('[data-testid="workspace-contact-address"]')
    expect(badge.text()).toBe('WhatsApp number hidden (@testcustomer)')
    expect(badge.attributes('title')).toContain('WhatsApp did not share')
    expect(wrapper.get('[data-testid="workspace-contact-address-hint"]').text()).toBe(
      "WhatsApp did not share this customer's number (they message with a WhatsApp username).",
    )
    expect(wrapper.find('[data-testid="workspace-contact-phone"]').exists()).toBe(false)
  })

  it('shows no address hint when the phone number is known', async () => {
    const wrapper = mountWorkspace()
    await flushPromises()
    expect(wrapper.find('[data-testid="workspace-contact-address-hint"]').exists()).toBe(false)
  })

  it('never shows a raw placeholder as the customer name', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({}, {
      phone_number: 'bsuid:abc123',
      profile_name: '',
      name: 'bsuid:abc123',
    }))
    const wrapper = mountWorkspace({ requestedAction: { kind: 'follow-up', nonce: 1 } })
    await flushPromises()

    expect(wrapper.get('h2').text()).toBe('WhatsApp user')
    const state = (wrapper.vm as any).$.setupState
    expect(state.taskDraft.title).toBe('Follow up with WhatsApp user')
    expect(state.detailContact.name).toBe('WhatsApp user')
    expect(wrapper.findComponent(ContactBookingDialog).props('contactName')).toBe('WhatsApp user')

    state.showTaskDialog = false
    await state.openJourney()
    await flushPromises()
    expect(wrapper.findComponent(LeadCreateDialog).props('contact')).toEqual({ id: 'contact-1', name: 'WhatsApp user' })
    expect(wrapper.text()).not.toContain('bsuid:')
  })

  it('names a phone-less customer on another channel after the channel', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({}, { phone_number: '', profile_name: '' }))
    const wrapper = mountWorkspace({ surface: 'omnichannel', channel: 'instagram' })
    await flushPromises()
    expect(wrapper.get('h2').text()).toBe('Instagram customer')
  })

  it('names the channel for customers without a phone on other channels', async () => {
    mocks.getWorkspace.mockResolvedValue(workspaceResponse({
      identities: [{ id: 'identity-1', channel: 'instagram', display_name: 'Test Customer', is_verified: true }],
    }, { phone_number: '' }))
    const wrapper = mountWorkspace({ surface: 'omnichannel', channel: 'instagram' })
    await flushPromises()

    expect(wrapper.get('[data-testid="workspace-contact-address"]').text()).toBe('Instagram customer')
    const header = wrapper.get('header').text()
    expect(header).toContain('Instagram')
    // The identity name repeats the contact name, so it is not shown twice.
    expect(header.match(/Test Customer/g)).toHaveLength(1)
  })

  it('hides the Copilot tab without copilot access', async () => {
    mocks.hasPermission.mockImplementation((resource: string) => resource !== 'copilot')
    const wrapper = mountWorkspace()
    await flushPromises()
    expect(wrapper.text()).not.toContain('Copilot')
    expect(wrapper.text()).toContain('Details')
  })
})

describe('ContactInfoPanel name and address', () => {
  function panelContact(overrides: Record<string, unknown> = {}) {
    return {
      id: 'contact-1',
      phone_number: '+10000000000',
      name: 'Test Customer',
      status: 'active',
      tags: [],
      metadata: {},
      unread_count: 0,
      identity_review_ai_state: {
        known: true,
        ai_allowed: true,
        blocked: false,
        open_hold_count: 0,
        reason: 'no_open_identity_review',
      },
      created_at: '',
      updated_at: '',
      ...overrides,
    }
  }

  function mountPanel(contact: Record<string, unknown>, channel: string | null = null) {
    return shallowMount(ContactInfoPanel, {
      props: { contact: contact as any, channel, embedded: true },
      global: { stubs: { ScrollArea: Passthrough } },
    })
  }

  it('shows the saved name and phone number without an address hint', () => {
    const wrapper = mountPanel(panelContact({ profile_name: 'Profile Name' }))
    expect(wrapper.get('[data-testid="contact-display-name"]').text()).toBe('Test Customer')
    expect(wrapper.text()).toContain('+10000000000')
    expect(wrapper.find('[data-testid="contact-address-hint"]').exists()).toBe(false)
  })

  it('falls back to the profile name when no name is saved', () => {
    const wrapper = mountPanel(panelContact({ name: '', profile_name: 'Profile Name' }))
    expect(wrapper.get('[data-testid="contact-display-name"]').text()).toBe('Profile Name')
  })

  it('shows a readable name and a visible hint for a hidden WhatsApp number', () => {
    const wrapper = mountPanel(panelContact({ name: '', phone_number: 'bsuid:abc123' }))
    expect(wrapper.get('[data-testid="contact-display-name"]').text()).toBe('WhatsApp user')
    expect(wrapper.get('[data-testid="contact-address-hint"]').text()).toBe(
      "WhatsApp did not share this customer's number (they message with a WhatsApp username).",
    )
    expect(wrapper.text()).not.toContain('bsuid:')
  })

  it('explains a phone-less customer from another channel', () => {
    const wrapper = mountPanel(panelContact({ name: 'Test Customer', phone_number: '' }), 'instagram')
    expect(wrapper.get('[data-testid="contact-address-hint"]').text()).toBe(
      'This customer contacted you on Instagram; no phone number is linked.',
    )
  })
})
