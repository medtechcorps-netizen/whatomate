/** @vitest-environment happy-dom */

import { mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { afterEach, describe, expect, it } from 'vitest'
import LeadOutcomeDialog from './LeadOutcomeDialog.vue'
import type { CRMLead } from '@/services/productSuite'
import { formatCurrencyMinorUnits } from '@/lib/currency'

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

const lead: CRMLead = {
  id: 'lead-1',
  contact_id: 'contact-1',
  pipeline_id: 'pipeline-1',
  stage_id: 'stage-new',
  title: 'Test Customer – Consultation',
  status: 'open',
  value_minor: 150000,
  currency: 'MYR',
  version: 3,
}

type Props = InstanceType<typeof LeadOutcomeDialog>['$props']

function mountDialog(props: Partial<Props> = {}) {
  return mount(LeadOutcomeDialog, {
    attachTo: document.body,
    props: {
      open: true,
      outcome: 'won',
      lead,
      stageName: 'Won',
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
      },
    },
  })
}

type Wrapper = ReturnType<typeof mountDialog>

function confirmButton(wrapper: Wrapper) {
  return wrapper.get('[data-testid="lead-outcome-confirm"]')
}

async function confirm(wrapper: Wrapper) {
  await wrapper.get('form#lead-outcome-form').trigger('submit')
  return wrapper.emitted('confirm')?.at(-1)?.[0]
}

afterEach(() => {
  document.body.innerHTML = ''
})

describe('LeadOutcomeDialog - won', () => {
  it('summarises the lead and target stage', () => {
    const wrapper = mountDialog({ stageName: 'Paid' })
    expect(wrapper.find('[data-testid="lead-outcome-dialog"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('Mark as won?')
    expect(wrapper.text()).toContain(
      `Test Customer – Consultation (${formatCurrencyMinorUnits('MYR', 150000)}) moves to Paid.`,
    )
    expect(confirmButton(wrapper).text()).toContain('Mark as won')
  })

  it('omits a zero value from the summary', () => {
    const wrapper = mountDialog({ lead: { ...lead, value_minor: 0 } })
    expect(wrapper.text()).toContain('Test Customer – Consultation moves to Won.')
  })

  it('hides the invoice option without invoice access', async () => {
    const wrapper = mountDialog({ canInvoice: false })
    expect(wrapper.text()).not.toContain('Create the invoice next')
    expect(await confirm(wrapper)).toEqual({ createInvoice: false })
  })

  it('offers the invoice option checked by default', async () => {
    const wrapper = mountDialog({ canInvoice: true })
    expect(wrapper.text()).toContain('Create the invoice next')
    const checkbox = wrapper.get('input[type="checkbox"]')
    expect((checkbox.element as HTMLInputElement).checked).toBe(true)
    expect(wrapper.find('[data-testid="lead-lost-reason-hint"]').exists()).toBe(false)
    expect(await confirm(wrapper)).toEqual({ createInvoice: true })

    await checkbox.setValue(false)
    expect(await confirm(wrapper)).toEqual({ createInvoice: false })
  })

  it('does not confirm or close while saving', async () => {
    const wrapper = mountDialog({ saving: true })
    expect(confirmButton(wrapper).attributes('disabled')).toBeDefined()
    expect(await confirm(wrapper)).toBeUndefined()
    await wrapper.findAll('button').find((item) => item.text() === 'Cancel')!.trigger('click')
    expect(wrapper.emitted('update:open')).toBeUndefined()
  })
})

describe('LeadOutcomeDialog - lost', () => {
  it('requires a reason before confirming', async () => {
    const wrapper = mountDialog({ outcome: 'lost', stageName: 'Lost' })
    expect(wrapper.text()).toContain('Mark as lost')
    const reasons = wrapper.findAll('[data-testid="lead-lost-reason"]')
    expect(reasons.map((item) => item.text())).toEqual([
      'Price',
      'Timing not right',
      'Not suitable',
      'No response',
      'Chose another provider',
      'Other',
    ])
    expect(confirmButton(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.get('[data-testid="lead-lost-reason-hint"]').text()).toBe('Choose a reason')
    expect(confirmButton(wrapper).attributes('aria-describedby')).toBe('lead-lost-reason-hint')
    expect(await confirm(wrapper)).toBeUndefined()

    await reasons[0].trigger('click')
    expect(reasons[0].attributes('aria-pressed')).toBe('true')
    expect(confirmButton(wrapper).attributes('disabled')).toBeUndefined()
    expect(wrapper.find('[data-testid="lead-lost-reason-hint"]').exists()).toBe(false)
    expect(confirmButton(wrapper).attributes('aria-describedby')).toBeUndefined()
    expect(await confirm(wrapper)).toEqual({ reason: 'Price' })
  })

  it('adds the optional note to the reason', async () => {
    const wrapper = mountDialog({ outcome: 'lost' })
    expect(wrapper.text()).toContain('Add a note (optional)')
    await wrapper.findAll('[data-testid="lead-lost-reason"]')[1].trigger('click')
    await wrapper.get('textarea').setValue('  Will come back next year ')
    expect(await confirm(wrapper)).toEqual({ reason: 'Timing not right: Will come back next year' })
  })

  it('clears the reason when reopened', async () => {
    const wrapper = mountDialog({ outcome: 'lost' })
    await wrapper.findAll('[data-testid="lead-lost-reason"]')[2].trigger('click')
    await wrapper.get('textarea').setValue('note')
    await wrapper.setProps({ open: false })
    await wrapper.setProps({ open: true })
    expect(confirmButton(wrapper).attributes('disabled')).toBeDefined()
    expect((wrapper.get('textarea').element as HTMLTextAreaElement).value).toBe('')
  })

  it('does not show the reason hint while saving', () => {
    const wrapper = mountDialog({ outcome: 'lost', saving: true })
    expect(wrapper.find('[data-testid="lead-lost-reason-hint"]').exists()).toBe(false)
  })

  it('emits update:open when cancelled', async () => {
    const wrapper = mountDialog({ outcome: 'lost' })
    await wrapper.findAll('button').find((item) => item.text() === 'Cancel')!.trigger('click')
    expect(wrapper.emitted('update:open')?.[0]).toEqual([false])
  })
})
