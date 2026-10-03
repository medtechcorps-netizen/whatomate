/** @vitest-environment happy-dom */

import { mount, RouterLinkStub } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { FollowUpTask } from '@/services/productSuite'
import TasksView from './TasksView.vue'

const mocks = vi.hoisted(() => ({
  allTasks: vi.fn(),
  completeTask: vi.fn(),
  createTask: vi.fn(),
  hasPermission: vi.fn(),
  success: vi.fn(),
  error: vi.fn(),
}))

vi.mock('@/services/productSuite', () => ({
  crmService: {
    allTasks: mocks.allTasks,
    completeTask: mocks.completeTask,
    createTask: mocks.createTask,
  },
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ hasPermission: mocks.hasPermission }),
}))
vi.mock('@/composables/useAppToast', () => ({
  useAppToast: () => ({ success: mocks.success, error: mocks.error }),
}))

const Passthrough = defineComponent({ template: '<span><slot /></span>' })
const ButtonStub = defineComponent({
  props: { disabled: Boolean, type: String },
  emits: ['click'],
  template:
    '<button :type="type || \'button\'" :disabled="disabled" @click="$emit(\'click\', $event)"><slot /></button>',
})

let permissions: Set<string>
let wrapper: ReturnType<typeof mountTasks> | undefined

function mountTasks() {
  return mount(TasksView, {
    global: {
      stubs: {
        PageHeader: defineComponent({
          props: { title: String, description: String },
          template: '<header><h1>{{ title }}</h1><p>{{ description }}</p><slot name="actions" /></header>',
        }),
        TaskEditDialog: true,
        Badge: Passthrough,
        Button: ButtonStub,
        RouterLink: RouterLinkStub,
      },
    },
  })
}

function daysFromNow(days: number) {
  const value = new Date()
  value.setDate(value.getDate() + days)
  value.setHours(10, 0, 0, 0)
  return value.toISOString()
}

function task(overrides: Partial<FollowUpTask>): FollowUpTask {
  return {
    id: 'task-1',
    title: 'Call back about consultation',
    status: 'open',
    priority: 'normal',
    version: 1,
    ...overrides,
  }
}

async function openTasks(tasks: FollowUpTask[]) {
  mocks.allTasks.mockResolvedValue(tasks)
  wrapper = mountTasks()
  const view = wrapper
  await vi.waitFor(() => expect(mocks.allTasks).toHaveBeenCalled())
  await vi.waitFor(() => expect(view.find('.animate-spin').exists()).toBe(false))
  return view
}

describe('TasksView', () => {
  beforeEach(() => {
    for (const mock of Object.values(mocks)) mock.mockReset()
    permissions = new Set(['tasks:read', 'tasks:write', 'chat:read', 'crm.leads:read'])
    mocks.hasPermission.mockImplementation((resource: string, action = 'read') =>
      permissions.has(`${resource}:${action}`),
    )
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = undefined
  })

  it('uses plain-language titles and metric labels', async () => {
    const view = await openTasks([])
    const text = view.text()
    expect(view.get('h1').text()).toBe('Follow-ups')
    const metrics = view.findAll('[data-testid="task-metric"]').map((item) => item.text())
    expect(metrics[0]).toContain('Open')
    expect(metrics[1]).toContain('Due today')
    expect(metrics[2]).toContain('Overdue')
    for (const jargon of ['Follow-up desk', 'Open promises', 'Needs rescue', 'auditable']) {
      expect(text).not.toContain(jargon)
    }
  })

  it('shows the customer and lead with links to the chat and the lead', async () => {
    const view = await openTasks([
      task({
        contact_id: 'contact-1',
        lead_id: 'lead-1',
        contact: { id: 'contact-1', profile_name: 'Test Customer', phone_number: '60000000000' },
        lead: { id: 'lead-1', title: 'Test Customer - Facial package', pipeline_id: 'pipeline-1' },
      }),
    ])
    const row = view.get('[data-testid="follow-up-row"]')
    expect(row.get('[data-testid="follow-up-customer"]').text()).toBe('Test Customer')
    expect(row.get('[data-testid="follow-up-lead"]').text()).toBe('Lead: Test Customer - Facial package')
    const links = row.findAllComponents(RouterLinkStub)
    expect(links.map((link: { text(): string }) => link.text())).toEqual(['Open chat', 'Open lead'])
    expect(links[0]!.props('to')).toBe('/chat/contact-1')
    expect(links[1]!.props('to')).toEqual({
      path: '/crm/pipeline',
      query: { pipeline: 'pipeline-1', lead: 'lead-1' },
    })
    // The accessible name used by the e2e edit flow is unchanged.
    expect(row.find('button[aria-label="Edit Call back about consultation"]').exists()).toBe(true)
  })

  it('explains a customer with no name and no phone number instead of showing a blank', async () => {
    const view = await openTasks([
      task({ contact_id: 'contact-1', contact: { id: 'contact-1', profile_name: '', phone_number: '' } }),
    ])
    expect(view.get('[data-testid="follow-up-customer"]').text()).toBe('WhatsApp user')
  })

  it('falls back to neutral labels when only ids are returned', async () => {
    const view = await openTasks([task({ contact_id: 'contact-1', lead_id: 'lead-1' })])
    expect(view.get('[data-testid="follow-up-customer"]').text()).toBe('Linked customer')
    expect(view.get('[data-testid="follow-up-lead"]').text()).toBe('Lead: Linked lead')
    const links = view.findAllComponents(RouterLinkStub)
    expect(links[1]!.props('to')).toEqual({ path: '/crm/pipeline', query: { lead: 'lead-1' } })
  })

  it('hides links the user is not allowed to open', async () => {
    permissions.delete('chat:read')
    permissions.delete('crm.leads:read')
    const view = await openTasks([
      task({
        contact_id: 'contact-1',
        lead_id: 'lead-1',
        contact: { id: 'contact-1', profile_name: 'Test Customer' },
        lead: { id: 'lead-1', title: 'Facial package', pipeline_id: 'pipeline-1' },
      }),
    ])
    expect(view.findAllComponents(RouterLinkStub)).toHaveLength(0)
    expect(view.get('[data-testid="follow-up-customer"]').text()).toBe('Test Customer')
  })

  it('shows no customer or lead line for an unlinked follow-up', async () => {
    const view = await openTasks([task({})])
    expect(view.find('[data-testid="follow-up-customer"]').exists()).toBe(false)
    expect(view.find('[data-testid="follow-up-lead"]').exists()).toBe(false)
    expect(view.findAllComponents(RouterLinkStub)).toHaveLength(0)
  })

  it('filters to overdue follow-ups from the Overdue card', async () => {
    const view = await openTasks([
      task({ id: 'late', title: 'Late call', due_at: daysFromNow(-3) }),
      task({ id: 'future', title: 'Future call', due_at: daysFromNow(3) }),
      task({ id: 'done', title: 'Done call', status: 'completed', due_at: daysFromNow(-5) }),
    ])
    expect(view.findAll('[data-testid="follow-up-row"]')).toHaveLength(2)
    const overdueCard = view.findAll('[data-testid="task-metric"]')[2]!
    expect(overdueCard.text()).toContain('1')
    await overdueCard.trigger('click')
    expect(overdueCard.attributes('aria-pressed')).toBe('true')
    const rows = view.findAll('[data-testid="follow-up-row"]')
    expect(rows).toHaveLength(1)
    expect(rows[0]!.text()).toContain('Late call')
    expect(rows[0]!.text()).toContain('Overdue')
  })

  it('shows a matching empty message for the overdue filter', async () => {
    const view = await openTasks([task({ due_at: daysFromNow(2) })])
    await view.findAll('[data-testid="task-metric"]')[2]!.trigger('click')
    expect(view.text()).toContain('Nothing overdue')
  })

  describe('due-date rule shared with the chat panel and pipeline', () => {
    beforeEach(() => {
      vi.useFakeTimers({ toFake: ['Date'] })
      vi.setSystemTime(new Date(2026, 9, 1, 15, 0, 0))
    })
    afterEach(() => {
      vi.useRealTimers()
    })

    const at = (day: number, hour: number) => new Date(2026, 9, day, hour, 0, 0).toISOString()

    it('counts a follow-up due earlier today as due today, not overdue', async () => {
      const view = await openTasks([task({ id: 'morning', title: 'Morning call', due_at: at(1, 9) })])
      const [, todayCard, overdueCard] = view.findAll('[data-testid="task-metric"]')
      expect(todayCard!.text()).toContain('1')
      expect(overdueCard!.text()).toContain('0')
      expect(view.get('[data-testid="follow-up-row"]').text()).not.toContain('Overdue')

      await todayCard!.trigger('click')
      expect(view.findAll('[data-testid="follow-up-row"]')).toHaveLength(1)
      await overdueCard!.trigger('click')
      expect(view.findAll('[data-testid="follow-up-row"]')).toHaveLength(0)
      expect(view.text()).toContain('Nothing overdue')
    })

    it('counts a follow-up due late yesterday as overdue', async () => {
      const view = await openTasks([task({ id: 'late', title: 'Late call', due_at: at(0, 23) })])
      const [, todayCard, overdueCard] = view.findAll('[data-testid="task-metric"]')
      expect(todayCard!.text()).toContain('0')
      expect(overdueCard!.text()).toContain('1')
      expect(view.get('[data-testid="follow-up-row"]').text()).toContain('Overdue')
    })

    it('keeps tomorrow and undated follow-ups out of both today and overdue', async () => {
      const view = await openTasks([
        task({ id: 'tomorrow', title: 'Tomorrow call', due_at: at(2, 9) }),
        task({ id: 'undated', title: 'Undated call' }),
        task({ id: 'cancelled', title: 'Cancelled call', status: 'cancelled', due_at: at(0, 9) }),
      ])
      const [openCard, todayCard, overdueCard] = view.findAll('[data-testid="task-metric"]')
      expect(openCard!.text()).toContain('2')
      expect(todayCard!.text()).toContain('0')
      expect(overdueCard!.text()).toContain('0')
    })
  })

  it('names each complete button after its follow-up and gives it a keyboard focus ring', async () => {
    const view = await openTasks([task({ title: 'Call back about consultation' })])
    const button = view.get('[data-testid="follow-up-complete"]')
    expect(button.attributes('aria-label')).toBe('Mark Call back about consultation as done')
    expect(button.attributes('type')).toBe('button')
    expect(button.classes()).toEqual(expect.arrayContaining(['focus-visible:ring-2', 'focus-visible:ring-violet-300']))
  })

  it('gives the list filter buttons a keyboard focus ring', async () => {
    const view = await openTasks([])
    const filters = view
      .findAll('button[aria-pressed]')
      .filter((button) => button.attributes('data-testid') !== 'task-metric')
    expect(filters.map((button) => button.text())).toEqual(['Open', 'Today', 'Overdue', 'Completed'])
    for (const filter of filters) {
      expect(filter.classes()).toEqual(expect.arrayContaining(['focus-visible:ring-2', 'focus-visible:ring-violet-300']))
    }
  })

  it('disables and spins the complete button while its request runs and ignores repeat clicks', async () => {
    let finish: (value?: unknown) => void = () => {}
    mocks.completeTask.mockImplementation(() => new Promise((resolve) => { finish = resolve }))
    const view = await openTasks([
      task({ id: 'task-1', title: 'First call', version: 3 }),
      task({ id: 'task-2', title: 'Second call' }),
    ])
    const [first, second] = view.findAll('[data-testid="follow-up-complete"]')
    await first!.trigger('click')
    await first!.trigger('click')
    expect(mocks.completeTask).toHaveBeenCalledTimes(1)
    expect(mocks.completeTask).toHaveBeenCalledWith('task-1', 3)
    expect(first!.attributes('disabled')).toBeDefined()
    expect(first!.attributes('aria-busy')).toBe('true')
    expect(first!.find('.animate-spin').exists()).toBe(true)
    expect(second!.attributes('disabled')).toBeUndefined()
    expect(second!.find('.animate-spin').exists()).toBe(false)

    // Another row's click is ignored until the first request finishes.
    await second!.trigger('click')
    expect(mocks.completeTask).toHaveBeenCalledTimes(1)

    mocks.allTasks.mockResolvedValue([task({ id: 'task-2', title: 'Second call' })])
    finish()
    await vi.waitFor(() => expect(mocks.success).toHaveBeenCalledWith('Follow-up completed'))
    await vi.waitFor(() => expect(view.findAll('[data-testid="follow-up-row"]')).toHaveLength(1))
    const remaining = view.get('[data-testid="follow-up-complete"]')
    expect(remaining.attributes('disabled')).toBeUndefined()
    expect(remaining.attributes('aria-busy')).toBeUndefined()
  })

  it('re-enables the complete button when the request fails', async () => {
    mocks.completeTask.mockRejectedValue(new Error('network'))
    const view = await openTasks([task({ title: 'First call' })])
    await view.get('[data-testid="follow-up-complete"]').trigger('click')
    await vi.waitFor(() => expect(mocks.error).toHaveBeenCalled())
    await vi.waitFor(() =>
      expect(view.get('[data-testid="follow-up-complete"]').attributes('disabled')).toBeUndefined(),
    )
    expect(mocks.completeTask).toHaveBeenCalledTimes(1)
  })

  it('points staff who can add follow-ups at the form when nothing is open', async () => {
    const view = await openTasks([])
    expect(view.text()).toContain("Use Add a follow-up, or schedule one from a customer's chat.")
  })

  it('does not point read-only staff at a form they cannot see', async () => {
    permissions.delete('tasks:write')
    const view = await openTasks([])
    expect(view.text()).toContain('Follow-ups your team schedules will show here.')
    expect(view.text()).not.toContain('Add a follow-up')
    expect(view.find('[data-testid="follow-up-complete"]').exists()).toBe(false)
  })

  it('never shows a raw placeholder as the customer name', async () => {
    const view = await openTasks([
      task({ contact_id: 'contact-1', contact: { id: 'contact-1', profile_name: 'bsuid:abc123', phone_number: '+10000000001' } }),
    ])
    expect(view.get('[data-testid="follow-up-customer"]').text()).toBe('+10000000001')
  })

  it('keeps plain-language copy', async () => {
    const view = await openTasks([])
    const text = view.text().toLowerCase()
    for (const word of ['reminder', 'journey', 'opportunity', 'enquiry', 'probability']) {
      expect(text).not.toContain(word)
    }
  })
})
