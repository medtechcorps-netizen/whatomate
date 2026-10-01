/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { CRMLead, FollowUpTask, Pipeline } from '@/services/productSuite'

const mocks = vi.hoisted(() => ({
  pipelines: vi.fn(),
  allLeads: vi.fn(),
  allTasks: vi.fn(),
  moveLead: vi.fn(),
  completeTask: vi.fn(),
  createLead: vi.fn(),
  createTask: vi.fn(),
  archiveLead: vi.fn(),
  success: vi.fn(),
  error: vi.fn(),
  info: vi.fn(),
  warning: vi.fn(),
  permissions: new Set<string>(),
  entitlements: new Set<string>(),
  route: { query: {} as Record<string, string> },
}))

vi.mock('@/services/productSuite', () => ({
  crmService: {
    pipelines: mocks.pipelines,
    allLeads: mocks.allLeads,
    allTasks: mocks.allTasks,
    moveLead: mocks.moveLead,
    completeTask: mocks.completeTask,
    createLead: mocks.createLead,
    createTask: mocks.createTask,
    archiveLead: mocks.archiveLead,
  },
  commerceService: {},
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    organizationId: 'org-test',
    hasPermission: (resource: string, action = 'read') => mocks.permissions.has(`${resource}:${action}`),
    hasProductEntitlement: (key?: string) => !key || mocks.entitlements.has(key),
  }),
}))
vi.mock('@/composables/useAppToast', () => ({
  useAppToast: () => ({
    success: mocks.success,
    error: mocks.error,
    info: mocks.info,
    warning: mocks.warning,
  }),
}))
vi.mock('vue-router', async () => {
  const { reactive: makeReactive } = await import('vue')
  const route = makeReactive(mocks.route)
  return { useRoute: () => route }
})
vi.mock('vuedraggable', async () => {
  const { defineComponent: define } = await import('vue')
  return {
    default: define({
      name: 'draggable',
      props: { modelValue: { type: Array, default: () => [] } },
      template:
        '<div data-testid="draggable"><template v-for="item in modelValue" :key="item.id"><slot name="item" :element="item" /></template></div>',
    }),
  }
})
vi.mock('@/components/crm/LeadCreateDialog.vue', async () => {
  const { defineComponent: define } = await import('vue')
  return {
    default: define({
      name: 'LeadCreateDialog',
      props: {
        open: Boolean,
        saving: Boolean,
        pipelines: { type: Array, default: () => [] },
        defaultPipelineId: { type: String, default: '' },
        canScheduleFollowUp: Boolean,
        existingOpenLeads: { type: Array, default: () => [] },
      },
      emits: ['update:open', 'submit'],
      template: '<div v-if="open" data-testid="lead-create-dialog" />',
    }),
  }
})
vi.mock('@/components/crm/LeadOutcomeDialog.vue', async () => {
  const { defineComponent: define } = await import('vue')
  return {
    default: define({
      name: 'LeadOutcomeDialog',
      props: {
        open: Boolean,
        outcome: { type: String, default: 'won' },
        lead: { type: Object, default: null },
        stageName: { type: String, default: '' },
        saving: Boolean,
        canInvoice: Boolean,
      },
      emits: ['update:open', 'confirm'],
      template: '<div v-if="open" data-testid="lead-outcome-dialog" />',
    }),
  }
})
vi.mock('@/components/commerce/InvoiceQuickDialog.vue', async () => {
  const { defineComponent: define } = await import('vue')
  return {
    default: define({
      name: 'InvoiceQuickDialog',
      props: { open: Boolean, contact: Object, lead: Object, canSellPackages: Boolean, source: String },
      emits: ['update:open', 'created'],
      template: '<div v-if="open" data-testid="invoice-quick-dialog" />',
    }),
  }
})
vi.mock('@/components/crm/LeadEditDialog.vue', async () => {
  const { defineComponent: define } = await import('vue')
  return { default: define({ name: 'LeadEditDialog', template: '<div />' }) }
})
vi.mock('@/components/crm/PipelineSettingsDialog.vue', async () => {
  const { defineComponent: define } = await import('vue')
  return { default: define({ name: 'PipelineSettingsDialog', template: '<div />' }) }
})

import PipelineView from './PipelineView.vue'

const Passthrough = defineComponent({ template: '<div><slot /></div>' })
const ButtonStub = defineComponent({
  props: { disabled: Boolean, type: String },
  emits: ['click'],
  template:
    '<button :type="type || \'button\'" :disabled="disabled" @click="$emit(\'click\', $event)"><slot /></button>',
})
const OpenGate = defineComponent({
  props: { open: Boolean },
  emits: ['update:open'],
  template: '<section v-if="open"><slot /></section>',
})
const MenuItemStub = defineComponent({
  emits: ['select'],
  template: '<div role="menuitem" @click="$emit(\'select\')"><slot /></div>',
})

const pipelineOne: Pipeline = {
  id: 'pipeline-1',
  name: 'Consultations',
  description: 'First visit to treatment plan',
  is_default: true,
  is_active: true,
  display_order: 0,
  stages: [
    stage('stage-new', 'pipeline-1', 'New', 0, 'open'),
    stage('stage-qualified', 'pipeline-1', 'Qualified', 1, 'open'),
    stage('stage-won', 'pipeline-1', 'Won', 2, 'won'),
    stage('stage-lost', 'pipeline-1', 'Lost', 3, 'lost'),
  ],
}
const pipelineTwo: Pipeline = {
  id: 'pipeline-2',
  name: 'Packages',
  is_default: false,
  is_active: true,
  display_order: 1,
  stages: [stage('stage-p2-new', 'pipeline-2', 'New', 0, 'open')],
}

function stage(id: string, pipelineId: string, name: string, order: number, kind: 'open' | 'won' | 'lost') {
  return {
    id,
    pipeline_id: pipelineId,
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

function lead(id: string, title: string, stageId: string, overrides: Partial<CRMLead> = {}): CRMLead {
  return {
    id,
    contact_id: `contact-${id}`,
    pipeline_id: 'pipeline-1',
    stage_id: stageId,
    title,
    status: 'open',
    value_minor: 50000,
    currency: 'MYR',
    version: 4,
    contact: { id: `contact-${id}`, profile_name: 'Test Customer' },
    ...overrides,
  }
}

function task(id: string, overrides: Partial<FollowUpTask> = {}): FollowUpTask {
  return {
    id,
    title: `Task ${id}`,
    status: 'open',
    priority: 'normal',
    due_at: '2099-01-01T02:00:00Z',
    version: 7,
    ...overrides,
  }
}

let leads: CRMLead[]
let tasks: FollowUpTask[]
let wrapper: VueWrapper | undefined

function grant(...keys: string[]) {
  for (const key of keys) mocks.permissions.add(key)
}

async function mountView() {
  wrapper = mount(PipelineView, {
    global: {
      stubs: {
        PageHeader: defineComponent({
          props: { title: String, description: String },
          template: '<header><h1>{{ title }}</h1><p>{{ description }}</p><slot name="actions" /></header>',
        }),
        RouterLink: defineComponent({ props: { to: String }, template: '<a :href="to"><slot /></a>' }),
        Button: ButtonStub,
        Badge: Passthrough,
        Input: defineComponent({ template: '<input />' }),
        DropdownMenu: Passthrough,
        DropdownMenuTrigger: Passthrough,
        DropdownMenuContent: Passthrough,
        DropdownMenuItem: MenuItemStub,
        Sheet: OpenGate,
        SheetContent: Passthrough,
        SheetHeader: Passthrough,
        SheetTitle: Passthrough,
        SheetDescription: Passthrough,
        Dialog: OpenGate,
        DialogContent: Passthrough,
        DialogHeader: Passthrough,
        DialogTitle: Passthrough,
        DialogDescription: Passthrough,
        DialogFooter: Passthrough,
        AlertDialog: OpenGate,
        AlertDialogCancel: ButtonStub,
        AlertDialogContent: Passthrough,
        AlertDialogDescription: Passthrough,
        AlertDialogFooter: Passthrough,
        AlertDialogHeader: Passthrough,
        AlertDialogTitle: Passthrough,
      },
    },
  })
  await flushPromises()
  return wrapper
}

function button(label: string) {
  return wrapper!.find(`[aria-label="${label}"]`)
}

function buttonByText(text: string) {
  const match = wrapper!.findAll('button').find((item) => item.text().trim() === text)
  if (!match) throw new Error(`No button with text ${text}`)
  return match
}

beforeEach(() => {
  mocks.permissions.clear()
  mocks.entitlements.clear()
  mocks.route.query = {}
  globalThis.localStorage?.clear()
  grant('crm.leads:read', 'crm.leads:write', 'tasks:read', 'tasks:write', 'contacts:read')
  leads = [
    lead('lead-1', 'Lead One', 'stage-new'),
    lead('lead-2', 'Lead Two', 'stage-qualified'),
    lead('lead-3', 'Lead Three', 'stage-lost', { status: 'lost', lost_reason: 'Price' }),
  ]
  tasks = [
    task('task-board', { lead_id: 'lead-1', title: 'Call back about price' }),
    task('task-other', {
      lead_id: 'lead-elsewhere',
      title: 'Send package brochure',
      lead: { id: 'lead-elsewhere', pipeline_id: 'pipeline-2' },
    }),
    task('task-free', { title: 'General admin follow-up' }),
  ]
  mocks.pipelines.mockResolvedValue({ data: { data: { pipelines: [pipelineOne, pipelineTwo], total: 2 } } })
  mocks.allLeads.mockImplementation(async (params: Record<string, unknown>) =>
    leads.filter((item) => item.pipeline_id === params.pipeline_id).map((item) => ({ ...item })),
  )
  mocks.allTasks.mockImplementation(async () => tasks.map((item) => ({ ...item })))
  mocks.moveLead.mockImplementation(async (id: string, stageId: string, version: number) => ({
    data: { data: { ...leads.find((item) => item.id === id), stage_id: stageId, version: version + 1 } },
  }))
  mocks.completeTask.mockResolvedValue({ data: { data: {} } })
  mocks.createLead.mockResolvedValue({ data: { data: lead('lead-new', 'Test Customer – Consultations', 'stage-new') } })
  mocks.createTask.mockResolvedValue({ data: { data: {} } })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.useRealTimers()
  vi.clearAllMocks()
})

describe('PipelineView', () => {
  it('uses the plain-language title and shows the selected pipeline with a visible label', async () => {
    await mountView()
    expect(wrapper!.text()).toContain('Lead pipeline')
    expect(wrapper!.text()).toContain('Track every lead from first reply until it is won or lost.')
    expect(wrapper!.text()).not.toContain('enquiry')
    expect(wrapper!.find('label[for="pipeline-board-select"]').text()).toBe('Pipeline')
    expect(wrapper!.find('[data-testid="pipeline-description"]').text()).toBe('First visit to treatment plan')
    expect(wrapper!.text()).not.toContain('probability')
    // Only the board pipeline's leads are loaded, with no extra request on page load.
    expect(mocks.allLeads).toHaveBeenCalledTimes(1)
    expect(mocks.allLeads).toHaveBeenCalledWith({ pipeline_id: 'pipeline-1', include_archived: false })
  })

  it('honours ?pipeline= and remembers the board pipeline per organisation', async () => {
    mocks.route.query = { pipeline: 'pipeline-2' }
    await mountView()
    expect(mocks.allLeads).toHaveBeenCalledWith({ pipeline_id: 'pipeline-2', include_archived: false })

    const select = wrapper!.find('select[data-testid="pipeline-select"]')
    await select.setValue('pipeline-1')
    await flushPromises()
    expect(globalThis.localStorage.getItem('rereply.crm.lastPipeline.org-test')).toBe('pipeline-1')
  })

  it('shows the pipeline name as text when only one pipeline is active', async () => {
    mocks.pipelines.mockResolvedValue({ data: { data: { pipelines: [pipelineOne], total: 1 } } })
    await mountView()
    expect(wrapper!.find('select[data-testid="pipeline-select"]').exists()).toBe(false)
    expect(wrapper!.find('[data-testid="pipeline-name"]').text()).toContain('Consultations')
  })

  it('opens the follow-up sheet filtered to this pipeline, with an All follow-ups toggle', async () => {
    await mountView()
    expect(wrapper!.find('[data-testid="pipeline-follow-ups-count"]').text()).toBe('1')
    expect(wrapper!.findAll('[data-testid="pipeline-follow-up-row"]')).toHaveLength(0)

    await wrapper!.find('[data-testid="pipeline-follow-ups-button"]').trigger('click')
    let rows = wrapper!.findAll('[data-testid="pipeline-follow-up-row"]')
    expect(rows).toHaveLength(1)
    expect(rows[0]!.text()).toContain('Call back about price')
    expect(rows[0]!.text()).toContain('Test Customer')

    await wrapper!.find('[data-testid="pipeline-follow-ups-scope-all"]').trigger('click')
    rows = wrapper!.findAll('[data-testid="pipeline-follow-up-row"]')
    expect(rows).toHaveLength(3)
    expect(wrapper!.find('a[href="/crm/tasks"]').text()).toContain('Open follow-ups page')
  })

  it('reloads leads after completing a follow-up linked to a lead', async () => {
    await mountView()
    await wrapper!.find('[data-testid="pipeline-follow-ups-button"]').trigger('click')
    expect(mocks.allLeads).toHaveBeenCalledTimes(1)

    await button('Complete Call back about price').trigger('click')
    await flushPromises()

    expect(mocks.completeTask).toHaveBeenCalledWith('task-board', 7)
    expect(mocks.allLeads).toHaveBeenCalledTimes(2)
    expect(mocks.success).toHaveBeenCalledWith('Follow-up completed')
  })

  it('does not reload leads after completing a follow-up with no lead', async () => {
    await mountView()
    await wrapper!.find('[data-testid="pipeline-follow-ups-button"]').trigger('click')
    await wrapper!.find('[data-testid="pipeline-follow-ups-scope-all"]').trigger('click')
    await button('Complete General admin follow-up').trigger('click')
    await flushPromises()
    expect(mocks.completeTask).toHaveBeenCalledWith('task-free', 7)
    expect(mocks.allLeads).toHaveBeenCalledTimes(1)
  })

  it('only walks Previous/Next between open stages', async () => {
    await mountView()
    expect(button('Move Lead One to Qualified').exists()).toBe(true)
    expect(button('Move Lead One to New').exists()).toBe(false)
    // Qualified is the last open stage: no Next into Won.
    expect(button('Move Lead Two to New').exists()).toBe(true)
    expect(button('Move Lead Two to Won').exists()).toBe(false)
    // Lost leads get no Previous back into Won.
    expect(button('Move Lead Three to Won').exists()).toBe(false)
    expect(wrapper!.find('[data-testid="pipeline-lead-lost-reason"]').text()).toBe('Reason: Price')

    await button('Move Lead One to Qualified').trigger('click')
    await flushPromises()
    expect(mocks.moveLead).toHaveBeenCalledWith('lead-1', 'stage-qualified', 4)
    expect(mocks.success).toHaveBeenCalledWith('Moved to Qualified')
  })

  it('confirms Won through the outcome dialog and moves with the lead version', async () => {
    await mountView()
    await button('Mark Lead One as won').trigger('click')
    const dialog = wrapper!.findComponent({ name: 'LeadOutcomeDialog' })
    expect(dialog.props('open')).toBe(true)
    expect(dialog.props('outcome')).toBe('won')
    expect(dialog.props('stageName')).toBe('Won')
    expect(mocks.moveLead).not.toHaveBeenCalled()

    dialog.vm.$emit('confirm', { createInvoice: false })
    await flushPromises()

    expect(mocks.moveLead).toHaveBeenCalledWith('lead-1', 'stage-won', 4, undefined)
    expect(mocks.success).toHaveBeenCalledWith('Marked as won')
    expect(mocks.info).toHaveBeenCalledWith('Ask a manager with billing access to create the invoice.')
    expect(wrapper!.findComponent({ name: 'InvoiceQuickDialog' }).exists()).toBe(false)
  })

  it('opens the invoice dialog after a win when billing access is available', async () => {
    grant('payments:write')
    mocks.entitlements.add('commerce.enabled')
    await mountView()
    await button('Mark Lead One as won').trigger('click')
    wrapper!.findComponent({ name: 'LeadOutcomeDialog' }).vm.$emit('confirm', { createInvoice: true })
    await flushPromises()

    const invoice = wrapper!.findComponent({ name: 'InvoiceQuickDialog' })
    expect(invoice.props('open')).toBe(true)
    expect(invoice.props('source')).toBe('crm_pipeline')
    expect(invoice.props('contact')).toEqual({ id: 'contact-lead-1', name: 'Test Customer' })
    expect(mocks.info).not.toHaveBeenCalled()
  })

  it('passes the lost reason with the move', async () => {
    await mountView()
    await button('Mark Lead Two as lost').trigger('click')
    const dialog = wrapper!.findComponent({ name: 'LeadOutcomeDialog' })
    expect(dialog.props('outcome')).toBe('lost')

    dialog.vm.$emit('confirm', { reason: 'Timing not right: back in spring' })
    await flushPromises()

    expect(mocks.moveLead).toHaveBeenCalledWith('lead-2', 'stage-lost', 4, 'Timing not right: back in spring')
    expect(mocks.success).toHaveBeenCalledWith('Marked as lost')
  })

  it('opens LeadCreateDialog from New lead and creates the lead plus its follow-up', async () => {
    await mountView()
    expect(wrapper!.find('[data-testid="lead-create-dialog"]').exists()).toBe(false)

    await buttonByText('New lead').trigger('click')
    const dialog = wrapper!.findComponent({ name: 'LeadCreateDialog' })
    expect(dialog.props('open')).toBe(true)
    expect(dialog.props('defaultPipelineId')).toBe('pipeline-1')
    expect(dialog.props('canScheduleFollowUp')).toBe(true)

    dialog.vm.$emit('submit', {
      contact_id: 'contact-1',
      contact_name: 'Test Customer',
      pipeline_id: 'pipeline-1',
      stage_id: 'stage-new',
      title: 'Test Customer – Consultations',
      value: '120',
      currency: 'MYR',
      follow_up_at: '2099-01-02T10:00',
    })
    await flushPromises()

    expect(mocks.createLead).toHaveBeenCalledWith(
      expect.objectContaining({
        contact_id: 'contact-1',
        pipeline_id: 'pipeline-1',
        stage_id: 'stage-new',
        source: 'other',
        value_minor: 12000,
        idempotency_key: expect.any(String),
      }),
    )
    expect(mocks.createTask).toHaveBeenCalledWith(
      expect.objectContaining({ lead_id: 'lead-new', contact_id: 'contact-1', source: 'crm_pipeline' }),
    )
    expect(mocks.success).toHaveBeenCalledWith('Lead added')
    expect(dialog.props('open')).toBe(false)
  })

  it('reverts a card dropped into Won when the outcome dialog is cancelled', async () => {
    await mountView()
    const wonDraggable = wrapper!
      .findAllComponents({ name: 'draggable' })
      .find((item) => item.element.closest('section')?.getAttribute('aria-label') === 'Won')
    expect(wonDraggable).toBeTruthy()
    wonDraggable!.vm.$emit('change', { added: { element: leads[0] } })
    await flushPromises()

    const dialog = wrapper!.findComponent({ name: 'LeadOutcomeDialog' })
    expect(dialog.props('open')).toBe(true)
    dialog.vm.$emit('update:open', false)
    await flushPromises()

    expect(mocks.moveLead).not.toHaveBeenCalled()
    expect(mocks.allLeads).toHaveBeenCalledTimes(2)
  })

  it('hides the Actions menu for read-only users but keeps the follow-up badge', async () => {
    mocks.permissions.clear()
    grant('crm.leads:read', 'tasks:read')
    await mountView()
    expect(wrapper!.find('[aria-label="Actions for Lead One"]').exists()).toBe(false)
    expect(button('Mark Lead One as won').exists()).toBe(false)
    expect(wrapper!.findAll('[data-testid="pipeline-lead-follow-up"]').length).toBeGreaterThan(0)
  })

  it('refreshes follow-ups and leads when completing a follow-up that changed elsewhere', async () => {
    mocks.completeTask.mockRejectedValueOnce({ response: { status: 409, data: { error: 'version conflict' } } })
    await mountView()
    await wrapper!.find('[data-testid="pipeline-follow-ups-button"]').trigger('click')
    expect(mocks.allTasks).toHaveBeenCalledTimes(1)

    await button('Complete Call back about price').trigger('click')
    await flushPromises()

    expect(mocks.warning).toHaveBeenCalledWith('This follow-up changed elsewhere. Refreshed.')
    expect(mocks.error).not.toHaveBeenCalled()
    expect(mocks.allTasks).toHaveBeenCalledTimes(2)
    expect(mocks.allLeads).toHaveBeenCalledTimes(2)
    expect(button('Complete Call back about price').attributes('disabled')).toBeUndefined()
  })

  it('refreshes only follow-ups after a conflict on a follow-up with no lead', async () => {
    mocks.completeTask.mockRejectedValueOnce({ response: { status: 409 } })
    await mountView()
    await wrapper!.find('[data-testid="pipeline-follow-ups-button"]').trigger('click')
    await wrapper!.find('[data-testid="pipeline-follow-ups-scope-all"]').trigger('click')
    await button('Complete General admin follow-up').trigger('click')
    await flushPromises()

    expect(mocks.warning).toHaveBeenCalledWith('This follow-up changed elsewhere. Refreshed.')
    expect(mocks.allTasks).toHaveBeenCalledTimes(2)
    expect(mocks.allLeads).toHaveBeenCalledTimes(1)
  })

  it('keeps the generic error for other completion failures', async () => {
    mocks.completeTask.mockRejectedValueOnce({ response: { status: 500 }, message: 'Server error' })
    await mountView()
    await wrapper!.find('[data-testid="pipeline-follow-ups-button"]').trigger('click')
    await button('Complete Call back about price').trigger('click')
    await flushPromises()

    expect(mocks.error).toHaveBeenCalledWith('Follow-up was not completed', expect.any(String))
    expect(mocks.warning).not.toHaveBeenCalled()
    expect(mocks.allTasks).toHaveBeenCalledTimes(1)
  })

  it('counts open and overdue follow-ups with the shared calendar-day rule', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date(2026, 9, 1, 15, 0))
    tasks = [
      // Due earlier today: still "today", not overdue.
      task('task-today', { lead_id: 'lead-1', title: 'Morning call', due_at: new Date(2026, 9, 1, 9, 0).toISOString() }),
      // Due yesterday evening: overdue.
      task('task-late', { lead_id: 'lead-2', title: 'Late call', due_at: new Date(2026, 8, 30, 18, 0).toISOString() }),
    ]
    await mountView()

    const kpi = wrapper!.find('[data-testid="pipeline-kpi-follow-ups"]')
    expect(kpi.text()).toContain('Open follow-ups')
    expect(kpi.text()).toContain('2')
    expect(wrapper!.text()).not.toContain('Follow-ups due')
    expect(wrapper!.find('[data-testid="pipeline-kpi-overdue"]').text()).toBe('1 overdue')
    const headerButton = wrapper!.find('[data-testid="pipeline-follow-ups-button"]')
    expect(headerButton.attributes('aria-label')).toBe('Follow-ups, 2 open, 1 overdue')
    expect(headerButton.attributes('title')).toBe('Follow-ups, 2 open, 1 overdue')

    const badge = (leadId: string) =>
      wrapper!.find(`[data-lead-id="${leadId}"] [data-testid="pipeline-lead-follow-up"]`).text()
    expect(badge('lead-1')).toContain('Today')
    expect(badge('lead-2')).toContain('Overdue')
  })

  it('always states the overdue count in the follow-ups button name', async () => {
    await mountView()
    expect(wrapper!.find('[data-testid="pipeline-follow-ups-button"]').attributes('aria-label')).toBe(
      'Follow-ups, 1 open, 0 overdue',
    )
    expect(wrapper!.find('[data-testid="pipeline-kpi-overdue"]').text()).toBe('0 overdue')
  })

  it('names customers with the shared display name, never a raw placeholder', async () => {
    grant('chat:read')
    leads = [
      lead('lead-1', 'Lead One', 'stage-new', {
        contact: { id: 'contact-lead-1', profile_name: '  ', phone_number: 'bsuid:placeholder-1' },
      }),
      lead('lead-2', 'Lead Two', 'stage-qualified', {
        contact: { id: 'contact-lead-2', profile_name: 'bsuid:placeholder-2', phone_number: '+10000000002' },
      }),
    ]
    await mountView()

    const link = (leadId: string) => wrapper!.find(`[data-lead-id="${leadId}"] [data-testid="pipeline-lead-chat"]`)
    expect(link('lead-1').text()).toBe('WhatsApp user')
    expect(link('lead-1').attributes('aria-label')).toBe('Open chat with WhatsApp user')
    expect(link('lead-2').text()).toBe('+10000000002')
    expect(wrapper!.text()).not.toContain('bsuid:')
  })

  it('shows no value line on a card without a value', async () => {
    leads = [
      lead('lead-1', 'Lead One', 'stage-new'),
      lead('lead-2', 'Lead Two', 'stage-qualified', { value_minor: 0 }),
    ]
    await mountView()

    expect(wrapper!.text()).not.toContain('No value yet')
    expect(wrapper!.find('[data-lead-id="lead-1"] [data-testid="pipeline-lead-value"]').exists()).toBe(true)
    expect(wrapper!.find('[data-lead-id="lead-2"] [data-testid="pipeline-lead-value"]').exists()).toBe(false)
    // The follow-up badge still shows on its own.
    expect(wrapper!.find('[data-lead-id="lead-2"] [data-testid="pipeline-lead-follow-up"]').exists()).toBe(true)
  })

  it('uses compact icon-only Previous/Next buttons that keep their accessible names', async () => {
    await mountView()
    const next = button('Move Lead One to Qualified')
    expect(next.attributes('data-testid')).toBe('pipeline-lead-move-next')
    expect(next.attributes('title')).toBe('Move to Qualified')
    expect(next.text()).toBe('')
    const previous = button('Move Lead Two to New')
    expect(previous.attributes('data-testid')).toBe('pipeline-lead-move-previous')
    expect(previous.attributes('title')).toBe('Move to New')
    expect(previous.text()).toBe('')

    const actions = wrapper!.find('[data-lead-id="lead-1"] [data-testid="pipeline-lead-actions"]')
    const labels = actions.findAll('button').map((item) => item.text().trim())
    expect(labels).toEqual(['', 'Won', 'Lost', 'Follow-up'])
  })

  it('swaps Create invoice for an Invoice created badge after the invoice is saved', async () => {
    grant('payments:write')
    mocks.entitlements.add('commerce.enabled')
    leads = [
      lead('lead-1', 'Lead One', 'stage-new'),
      lead('lead-4', 'Lead Four', 'stage-won', { status: 'won', won_at: '2026-09-01T02:00:00Z' }),
      lead('lead-5', 'Lead Five', 'stage-won', { status: 'won', won_at: '2026-09-01T02:00:00Z' }),
    ]
    await mountView()
    expect(wrapper!.find('[data-testid="pipeline-lead-invoiced"]').exists()).toBe(false)

    await button('Create invoice for Lead Four').trigger('click')
    const invoice = wrapper!.findComponent({ name: 'InvoiceQuickDialog' })
    expect(invoice.props('open')).toBe(true)
    invoice.vm.$emit('created', { id: 'invoice-1' })
    await flushPromises()

    expect(invoice.props('open')).toBe(false)
    expect(button('Create invoice for Lead Four').exists()).toBe(false)
    const invoiced = wrapper!.find('[data-lead-id="lead-4"] [data-testid="pipeline-lead-invoiced"]')
    expect(invoiced.text()).toContain('Invoice created')
    expect(invoiced.find('a').attributes('href')).toBe('/commerce?tab=invoices')
    expect(invoiced.find('a').text()).toBe('Open invoices')
    // Other won leads keep their Create invoice button, and nothing is re-fetched.
    expect(button('Create invoice for Lead Five').exists()).toBe(true)
    expect(mocks.allLeads).toHaveBeenCalledTimes(1)
    expect(mocks.allTasks).toHaveBeenCalledTimes(1)
  })
})

describe('LeadEditDialog labels', () => {
  it('uses plain-language date labels and keeps the title and save labels', async () => {
    const { default: RealLeadEditDialog } =
      await vi.importActual<typeof import('@/components/crm/LeadEditDialog.vue')>(
        '@/components/crm/LeadEditDialog.vue',
      )
    const dialog = mount(RealLeadEditDialog, {
      props: { modelValue: true, lead: lead('lead-1', 'Lead One', 'stage-new') },
      global: {
        stubs: {
          Button: ButtonStub,
          Dialog: OpenGate,
          DialogContent: Passthrough,
          DialogHeader: Passthrough,
          DialogTitle: Passthrough,
          DialogDescription: Passthrough,
          DialogFooter: Passthrough,
          AlertDialog: OpenGate,
          AlertDialogCancel: ButtonStub,
          AlertDialogContent: Passthrough,
          AlertDialogDescription: Passthrough,
          AlertDialogFooter: Passthrough,
          AlertDialogHeader: Passthrough,
          AlertDialogTitle: Passthrough,
        },
      },
    })
    try {
      const text = dialog.text()
      expect(text).toContain('Lead title')
      expect(text).toContain('Follow-up date')
      expect(text).toContain('Expected decision date')
      expect(text).toContain('Save changes')
      expect(text).not.toContain('Next action')
      expect(text).not.toContain('Expected close')
    } finally {
      dialog.unmount()
    }
  })
})

