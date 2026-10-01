/** @vitest-environment happy-dom */

import { mount } from '@vue/test-utils'
import { defineComponent, nextTick } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'
import LeadCreateDialog from './LeadCreateDialog.vue'
import type { CRMLead, Pipeline, PipelineStage } from '@/services/productSuite'
import type { LeadDraft } from '@/lib/crmFlow'

const Passthrough = defineComponent({
  template: '<div><slot /></div>',
})

const ButtonStub = defineComponent({
  props: {
    disabled: Boolean,
    type: String,
  },
  emits: ['click'],
  template:
    '<button :type="type || \'button\'" :disabled="disabled" @click="$emit(\'click\')"><slot /></button>',
})

const ContactPickerStub = defineComponent({
  name: 'ContactPicker',
  props: { modelValue: String, disabled: Boolean },
  emits: ['update:modelValue', 'selected'],
  template: '<div data-testid="contact-picker-stub" />',
})

function stage(overrides: Partial<PipelineStage> & { id: string; name: string }): PipelineStage {
  return {
    pipeline_id: 'pipeline-1',
    color: '#ffffff',
    display_order: 0,
    kind: 'open',
    probability: 0,
    sla_hours: 0,
    is_active: true,
    version: 1,
    ...overrides,
  }
}

const consultation: Pipeline = {
  id: 'pipeline-1',
  name: 'Consultation',
  description: 'First visits',
  is_default: true,
  is_active: true,
  display_order: 1,
  stages: [
    stage({ id: 'stage-new', name: 'New', display_order: 1 }),
    stage({ id: 'stage-contacted', name: 'Contacted', display_order: 2 }),
    stage({ id: 'stage-won', name: 'Won', kind: 'won', display_order: 3 }),
  ],
}

const treatment: Pipeline = {
  id: 'pipeline-2',
  name: 'Treatment plan',
  is_default: false,
  is_active: true,
  display_order: 2,
  stages: [
    stage({ id: 'stage-t-booked', pipeline_id: 'pipeline-2', name: 'Booked', display_order: 2 }),
    stage({ id: 'stage-t-new', pipeline_id: 'pipeline-2', name: 'Interested', display_order: 1 }),
  ],
}

const empty: Pipeline = {
  id: 'pipeline-3',
  name: 'Unconfigured',
  is_default: false,
  is_active: true,
  display_order: 3,
  stages: [stage({ id: 'stage-x-won', pipeline_id: 'pipeline-3', name: 'Won', kind: 'won' })],
}

const archived: Pipeline = {
  id: 'pipeline-9',
  name: 'Old pipeline',
  is_default: false,
  is_active: false,
  stages: [],
}

type Props = InstanceType<typeof LeadCreateDialog>['$props']

function mountDialog(props: Partial<Props> = {}) {
  return mount(LeadCreateDialog, {
    attachTo: document.body,
    props: {
      open: true,
      contact: { id: 'contact-1', name: 'Test Customer' },
      pipelines: [consultation, treatment, archived],
      ...props,
    } as Props,
    global: {
      stubs: {
        Dialog: Passthrough,
        DialogContent: Passthrough,
        DialogDescription: Passthrough,
        DialogFooter: Passthrough,
        DialogHeader: Passthrough,
        DialogTitle: Passthrough,
        Button: ButtonStub,
        ContactPicker: ContactPickerStub,
      },
    },
  })
}

type Wrapper = ReturnType<typeof mountDialog>

function submitButton(wrapper: Wrapper) {
  return wrapper.get('[data-testid="lead-create-submit"]')
}

function missingReason(wrapper: Wrapper) {
  const line = wrapper.find('[data-testid="lead-create-missing-reason"]')
  return line.exists() ? line.text() : ''
}

function titleInput(wrapper: Wrapper) {
  return wrapper.get('input[maxlength="255"]')
}

async function submit(wrapper: Wrapper) {
  await wrapper.get('form#lead-create-form').trigger('submit')
  return wrapper.emitted('submit')?.at(-1)?.[0] as LeadDraft | undefined
}

afterEach(() => {
  document.body.innerHTML = ''
  vi.useRealTimers()
})

describe('LeadCreateDialog', () => {
  it('renders the fixed-customer copy and active pipeline cards', () => {
    const wrapper = mountDialog()
    expect(wrapper.text()).toContain('Add to pipeline')
    expect(wrapper.text()).toContain(
      'Create a lead for Test Customer. You can move it through the stages later.',
    )
    expect(wrapper.find('[data-testid="contact-picker-stub"]').exists()).toBe(false)

    const cards = wrapper.findAll('[data-testid="lead-pipeline-option"]')
    expect(cards).toHaveLength(2)
    expect(cards[0].text()).toContain('Consultation')
    expect(cards[0].text()).toContain('First visits')
    expect(cards[0].text()).toContain('New → Contacted')
    expect(cards[1].text()).toContain('Interested → Booked')
    expect(wrapper.find('[role="radiogroup"]').exists()).toBe(true)
    expect((cards[0].get('input[type="radio"]').element as HTMLInputElement).checked).toBe(true)
  })

  it('labels the title field "Lead title"', () => {
    const wrapper = mountDialog()
    const label = titleInput(wrapper).element.closest('label')
    expect(label?.textContent).toContain('Lead title')
    expect(wrapper.text()).not.toContain('Lead name')
    expect(missingReason(wrapper)).toBe('')
  })

  it('defaults the stage and lead name, then follows the chosen pipeline', async () => {
    const wrapper = mountDialog()
    const stages = wrapper.findAll('[data-testid="lead-stage-option"]')
    expect(stages.map((item) => item.text())).toEqual(['New', 'Contacted'])
    expect(stages[0].attributes('aria-pressed')).toBe('true')
    expect((titleInput(wrapper).element as HTMLInputElement).value).toBe('Test Customer – Consultation')

    await wrapper.findAll('[data-testid="lead-pipeline-option"]')[1].get('input').trigger('change')
    expect(wrapper.findAll('[data-testid="lead-stage-option"]').map((item) => item.text())).toEqual([
      'Interested',
      'Booked',
    ])
    expect((titleInput(wrapper).element as HTMLInputElement).value).toBe('Test Customer – Treatment plan')

    await wrapper.findAll('[data-testid="lead-stage-option"]')[1].trigger('click')
    const draft = await submit(wrapper)
    expect(draft).toEqual({
      contact_id: 'contact-1',
      contact_name: 'Test Customer',
      pipeline_id: 'pipeline-2',
      stage_id: 'stage-t-booked',
      title: 'Test Customer – Treatment plan',
      value: '',
      currency: 'MYR',
      follow_up_at: '',
    })
  })

  it('keeps a lead name the user typed when the pipeline changes', async () => {
    const wrapper = mountDialog()
    await titleInput(wrapper).setValue('Teeth whitening')
    await wrapper.findAll('[data-testid="lead-pipeline-option"]')[1].get('input').trigger('change')
    expect((titleInput(wrapper).element as HTMLInputElement).value).toBe('Teeth whitening')
  })

  it('uses the defaultTitle and defaultPipelineId props', () => {
    const wrapper = mountDialog({ defaultTitle: 'Follow-up visit', defaultPipelineId: 'pipeline-2' })
    expect((titleInput(wrapper).element as HTMLInputElement).value).toBe('Follow-up visit')
    const cards = wrapper.findAll('[data-testid="lead-pipeline-option"]')
    expect((cards[1].get('input').element as HTMLInputElement).checked).toBe(true)
  })

  it('shows a read-only line when there is one active pipeline', () => {
    const wrapper = mountDialog({ pipelines: [consultation, archived] })
    expect(wrapper.findAll('[data-testid="lead-pipeline-option"]')).toHaveLength(0)
    expect(wrapper.text()).toContain('Pipeline: Consultation')
  })

  it('blocks submit when the pipeline has no open stage', async () => {
    const wrapper = mountDialog({ pipelines: [empty] })
    expect(wrapper.text()).toContain('This pipeline has no active open stage. Ask an admin to configure it.')
    expect(submitButton(wrapper).attributes('disabled')).toBeDefined()
    expect(await submit(wrapper)).toBeUndefined()
  })

  it('disables submit while saving or when the name is empty', async () => {
    const saving = mountDialog({ saving: true })
    expect(submitButton(saving).attributes('disabled')).toBeDefined()
    saving.unmount()

    const wrapper = mountDialog()
    await titleInput(wrapper).setValue('   ')
    expect(submitButton(wrapper).attributes('disabled')).toBeDefined()
    expect(missingReason(wrapper)).toBe('Enter a lead title')
    expect(submitButton(wrapper).attributes('aria-describedby')).toBe('lead-create-missing-reason')

    await titleInput(wrapper).setValue('Consultation visit')
    expect(missingReason(wrapper)).toBe('')
    expect(submitButton(wrapper).attributes('aria-describedby')).toBeUndefined()
  })

  it('does not show a missing reason while saving', () => {
    const wrapper = mountDialog({ saving: true, contact: null })
    expect(missingReason(wrapper)).toBe('')
  })

  it('asks for a pipeline when none is selected', async () => {
    const wrapper = mountDialog({ pipelines: [consultation, treatment] })
    // Normally a default is always picked; force the empty state to check the hint.
    ;(wrapper.vm as unknown as { draft: LeadDraft }).draft.pipeline_id = ''
    await nextTick()
    expect(submitButton(wrapper).attributes('disabled')).toBeDefined()
    expect(missingReason(wrapper)).toBe('Choose a pipeline')
  })

  it('does not close while saving', async () => {
    const wrapper = mountDialog({ saving: true })
    await wrapper.findAll('button').find((item) => item.text() === 'Cancel')!.trigger('click')
    expect(wrapper.emitted('update:open')).toBeUndefined()
    await wrapper.setProps({ saving: false })
    await wrapper.findAll('button').find((item) => item.text() === 'Cancel')!.trigger('click')
    expect(wrapper.emitted('update:open')?.[0]).toEqual([false])
  })

  it('shows a loading state while pipelines load and adopts them when they arrive', async () => {
    const wrapper = mountDialog({ pipelines: [], pipelinesLoading: true })
    expect(wrapper.text()).toContain('Loading pipelines')
    expect(submitButton(wrapper).attributes('disabled')).toBeDefined()
    expect(missingReason(wrapper)).toBe('')
    await wrapper.setProps({ pipelines: [consultation, treatment], pipelinesLoading: false })
    expect((titleInput(wrapper).element as HTMLInputElement).value).toBe('Test Customer – Consultation')
    expect(submitButton(wrapper).attributes('disabled')).toBeUndefined()
  })

  it('only offers follow-up presets when allowed', () => {
    expect(mountDialog().findAll('[data-testid="lead-follow-up-preset"]')).toHaveLength(0)
  })

  it('sets the follow-up from presets and a custom date', async () => {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date(2026, 9, 1, 15, 0))
    const wrapper = mountDialog({ canScheduleFollowUp: true })
    const chips = wrapper.findAll('[data-testid="lead-follow-up-preset"]')
    expect(chips.map((item) => item.text())).toEqual([
      'No follow-up',
      'Tomorrow',
      'In 3 days',
      'Next week',
      'Pick a date',
    ])
    expect(chips[0].attributes('aria-pressed')).toBe('true')

    await chips[1].trigger('click')
    expect(wrapper.text()).toContain('Follow-up on')
    expect(wrapper.text()).not.toContain('Reminder')
    expect((await submit(wrapper))?.follow_up_at).toBe('2026-10-02T10:00')

    await chips[3].trigger('click')
    expect((await submit(wrapper))?.follow_up_at).toBe('2026-10-08T10:00')

    await chips[4].trigger('click')
    const picker = wrapper.get('input[type="datetime-local"]')
    expect(submitButton(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.get('[data-testid="lead-create-follow-up-missing"]').text()).toBe('Choose a follow-up date')
    expect(missingReason(wrapper)).toBe('Choose a follow-up date')
    await picker.setValue('2026-10-20T09:30')
    expect(wrapper.find('[data-testid="lead-create-follow-up-missing"]').exists()).toBe(false)
    expect(missingReason(wrapper)).toBe('')
    expect((await submit(wrapper))?.follow_up_at).toBe('2026-10-20T09:30')

    await chips[0].trigger('click')
    expect(wrapper.find('input[type="datetime-local"]').exists()).toBe(false)
    expect((await submit(wrapper))?.follow_up_at).toBe('')
  })

  it('shows a non-blocking notice when the customer already has an open lead there', () => {
    const existing = {
      id: 'lead-1',
      contact_id: 'contact-1',
      pipeline_id: 'pipeline-1',
      stage_id: 'stage-new',
      title: 'Existing consultation',
      status: 'open',
      value_minor: 0,
      currency: 'MYR',
      version: 1,
    } as CRMLead
    const wrapper = mountDialog({ existingOpenLeads: [existing] })
    expect(wrapper.text()).toContain('already has an open lead in this pipeline')
    expect(wrapper.text()).toContain('Existing consultation')
    expect(submitButton(wrapper).attributes('disabled')).toBeUndefined()

    const other = mountDialog({ existingOpenLeads: [{ ...existing, status: 'won' }] })
    expect(other.text()).not.toContain('already has an open lead')
  })

  it('rejects a negative value', async () => {
    const wrapper = mountDialog()
    await wrapper.get('input[type="number"]').setValue('-5')
    expect(wrapper.text()).toContain('Enter an amount of zero or more.')
    expect(submitButton(wrapper).attributes('disabled')).toBeDefined()
  })

  it('lets the user pick the customer when none is fixed', async () => {
    const wrapper = mountDialog({ contact: null })
    expect(wrapper.text()).toContain('Create a lead and choose where it starts.')
    const picker = wrapper.getComponent(ContactPickerStub)
    expect(submitButton(wrapper).attributes('disabled')).toBeDefined()
    expect(missingReason(wrapper)).toBe('Choose a customer')
    expect((titleInput(wrapper).element as HTMLInputElement).value).toBe('Consultation')

    picker.vm.$emit('update:modelValue', 'contact-2')
    picker.vm.$emit('selected', { id: 'contact-2', profile_name: 'Another Customer', phone_number: '' })
    await nextTick()
    expect((titleInput(wrapper).element as HTMLInputElement).value).toBe('Another Customer – Consultation')
    const draft = await submit(wrapper)
    expect(draft?.contact_id).toBe('contact-2')
    expect(draft?.contact_name).toBe('Another Customer')
    expect(missingReason(wrapper)).toBe('')
  })

  it('never puts a raw channel placeholder in the suggested title', async () => {
    const wrapper = mountDialog({ contact: null })
    const picker = wrapper.getComponent(ContactPickerStub)
    picker.vm.$emit('update:modelValue', 'contact-3')
    picker.vm.$emit('selected', { id: 'contact-3', name: 'bsuid:abc', profile_name: '', phone_number: 'bsuid:abc' })
    await nextTick()
    const title = (titleInput(wrapper).element as HTMLInputElement).value
    expect(title).toBe('WhatsApp user – Consultation')
    expect(title).not.toContain('bsuid:')
    expect((await submit(wrapper))?.contact_name).toBe('WhatsApp user')
  })

  it('falls back to the phone number when the customer has no name', async () => {
    const wrapper = mountDialog({ contact: null })
    const picker = wrapper.getComponent(ContactPickerStub)
    picker.vm.$emit('update:modelValue', 'contact-4')
    picker.vm.$emit('selected', { id: 'contact-4', name: '', phone_number: '60000000000' })
    await nextTick()
    expect((titleInput(wrapper).element as HTMLInputElement).value).toBe('60000000000 – Consultation')
  })

  it('hides a placeholder name passed for a fixed customer', () => {
    const wrapper = mountDialog({ contact: { id: 'contact-5', name: 'user:abc' } })
    expect(wrapper.text()).not.toContain('user:abc')
    expect((titleInput(wrapper).element as HTMLInputElement).value).toBe('WhatsApp user – Consultation')
  })

  it('does not mount the customer picker while closed', () => {
    const wrapper = mountDialog({ contact: null, open: false })
    expect(wrapper.find('[data-testid="contact-picker-stub"]').exists()).toBe(false)
  })

  it('resets the draft each time it opens', async () => {
    const wrapper = mountDialog()
    await titleInput(wrapper).setValue('Changed')
    await wrapper.get('input[type="number"]').setValue('100')
    await wrapper.setProps({ open: false })
    await wrapper.setProps({ open: true })
    expect((titleInput(wrapper).element as HTMLInputElement).value).toBe('Test Customer – Consultation')
    expect((wrapper.get('input[type="number"]').element as HTMLInputElement).value).toBe('')
  })
})
