/** @vitest-environment happy-dom */

import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import InvoiceQuickDialog from './InvoiceQuickDialog.vue'
import type { CRMLead } from '@/services/productSuite'

const mocks = vi.hoisted(() => ({
  allPackages: vi.fn(),
  createInvoice: vi.fn(),
  sellPackage: vi.fn(),
  toastError: vi.fn(),
  toastSuccess: vi.fn(),
}))

vi.mock('@/services/productSuite', () => ({
  commerceService: {
    allPackages: mocks.allPackages,
    createInvoice: mocks.createInvoice,
    sellPackage: mocks.sellPackage,
  },
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
  template:
    '<button :type="type || \'button\'" :disabled="disabled" @click="$emit(\'click\')"><slot /></button>',
})

const lead: CRMLead = {
  id: 'lead-1',
  contact_id: 'contact-1',
  pipeline_id: 'pipeline-1',
  stage_id: 'stage-won',
  title: 'Consultation package',
  status: 'won',
  value_minor: 45000,
  currency: 'SGD',
  version: 2,
}

const invoice = {
  id: 'invoice-1',
  contact_id: 'contact-1',
  invoice_number: 'INV-0001',
  status: 'open',
  currency: 'SGD',
  total_minor: 45000,
  paid_minor: 0,
  due_minor: 45000,
  version: 1,
}

type Props = InstanceType<typeof InvoiceQuickDialog>['$props']

function mountDialog(props: Partial<Props> = {}) {
  return mount(InvoiceQuickDialog, {
    attachTo: document.body,
    props: {
      open: true,
      contact: { id: 'contact-1', name: 'Test Customer' },
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

async function submit(wrapper: Wrapper) {
  await wrapper.get('form#invoice-quick-form').trigger('submit')
  await flushPromises()
}

function button(wrapper: Wrapper, label: string) {
  const found = wrapper.findAll('button').find((item) => item.text().trim() === label)
  if (!found) throw new Error(`Button ${label} not found`)
  return found
}

let uuidCounter = 0

beforeEach(() => {
  uuidCounter = 0
  vi.spyOn(crypto, 'randomUUID').mockImplementation(
    () => `00000000-0000-4000-8000-00000000000${++uuidCounter}` as `${string}-${string}-${string}-${string}-${string}`,
  )
  mocks.createInvoice.mockResolvedValue({ data: { status: 'success', data: invoice } })
  mocks.sellPackage.mockResolvedValue({
    data: { status: 'success', data: { invoice: { ...invoice, invoice_number: 'INV-0002' }, contact_package: {} } },
  })
  mocks.allPackages.mockResolvedValue([
    {
      id: 'package-1',
      name: 'Five sessions',
      price_minor: 50000,
      currency: 'MYR',
      validity_days: 90,
      is_active: true,
      version: 1,
    },
    {
      id: 'package-2',
      name: 'Retired plan',
      price_minor: 10000,
      currency: 'MYR',
      validity_days: 30,
      is_active: false,
      version: 1,
    },
  ])
})

afterEach(() => {
  document.body.innerHTML = ''
  vi.restoreAllMocks()
  vi.clearAllMocks()
})

describe('InvoiceQuickDialog', () => {
  it('describes the customer and lead and prefills from the lead', () => {
    const wrapper = mountDialog({ lead })
    expect(wrapper.find('[data-testid="invoice-quick-dialog"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('Create invoice')
    expect(wrapper.text()).toContain('For Test Customer - Consultation package')
    expect((wrapper.get('input[maxlength="255"]').element as HTMLInputElement).value).toBe(
      'Consultation package',
    )
    expect((wrapper.get('input[type="number"]').element as HTMLInputElement).value).toBe('450.00')
    expect((wrapper.get('select').element as HTMLSelectElement).value).toBe('SGD')
    expect(wrapper.text()).not.toContain('Custom amount')
  })

  it('creates a custom invoice linked to the lead', async () => {
    const wrapper = mountDialog({ lead })
    await wrapper.get('input[type="date"]').setValue('2099-01-15')
    await submit(wrapper)

    expect(mocks.createInvoice).toHaveBeenCalledWith({
      contact_id: 'contact-1',
      currency: 'SGD',
      due_at: new Date('2099-01-15T23:59:59').toISOString(),
      idempotency_key: '00000000-0000-4000-8000-000000000001',
      lines: [{ description: 'Consultation package', quantity: 1, unit_amount_minor: 45000 }],
      metadata: { lead_id: 'lead-1', source: 'customer_workspace' },
    })
    expect(mocks.toastSuccess).toHaveBeenCalledWith('Invoice INV-0001 created')
    expect(wrapper.emitted('created')?.[0]).toEqual([invoice])
    expect(wrapper.emitted('update:open')?.at(-1)).toEqual([false])
  })

  it('omits lead metadata and due date when not given', async () => {
    const wrapper = mountDialog({ source: 'pipeline' })
    expect(wrapper.text()).toContain('For Test Customer')
    await wrapper.get('input[maxlength="255"]').setValue('Follow-up visit')
    await wrapper.get('input[type="number"]').setValue('120')
    await submit(wrapper)
    expect(mocks.createInvoice).toHaveBeenCalledWith(
      expect.objectContaining({
        currency: 'MYR',
        due_at: undefined,
        lines: [{ description: 'Follow-up visit', quantity: 1, unit_amount_minor: 12000 }],
        metadata: { source: 'pipeline' },
      }),
    )
  })

  it('shows inline validation instead of calling the API', async () => {
    const wrapper = mountDialog()
    await submit(wrapper)
    expect(wrapper.text()).toContain('Enter what this invoice is for.')
    expect(wrapper.text()).toContain('Enter the amount.')
    expect(mocks.createInvoice).not.toHaveBeenCalled()

    await wrapper.get('input[maxlength="255"]').setValue('Visit')
    await wrapper.get('input[type="number"]').setValue('0')
    await submit(wrapper)
    expect(wrapper.text()).toContain('Enter an amount greater than zero.')
    expect(mocks.createInvoice).not.toHaveBeenCalled()
  })

  it('rejects a due date in the past', async () => {
    const wrapper = mountDialog({ lead })
    await wrapper.get('input[type="date"]').setValue('2000-01-01')
    await submit(wrapper)
    expect(wrapper.text()).toContain('The due date cannot be in the past.')
    expect(mocks.createInvoice).not.toHaveBeenCalled()
  })

  it('reuses the idempotency key on retry and regenerates it when reopened', async () => {
    mocks.createInvoice.mockRejectedValueOnce(new Error('Network down'))
    const wrapper = mountDialog({ lead })
    await submit(wrapper)
    expect(mocks.toastError).toHaveBeenCalledWith('Invoice was not created', 'Network down')
    expect(wrapper.emitted('created')).toBeUndefined()
    expect(wrapper.emitted('update:open')).toBeUndefined()

    await submit(wrapper)
    expect(mocks.createInvoice).toHaveBeenCalledTimes(2)
    expect(mocks.createInvoice.mock.calls[0][0].idempotency_key).toBe(
      mocks.createInvoice.mock.calls[1][0].idempotency_key,
    )

    await wrapper.setProps({ open: false })
    await wrapper.setProps({ open: true })
    await submit(wrapper)
    expect(mocks.createInvoice.mock.calls[2][0].idempotency_key).not.toBe(
      mocks.createInvoice.mock.calls[0][0].idempotency_key,
    )
  })

  it('lazy-loads packages and sells one', async () => {
    const wrapper = mountDialog({ lead, canSellPackages: true })
    expect(mocks.allPackages).not.toHaveBeenCalled()
    expect(button(wrapper, 'Custom amount').attributes('aria-pressed')).toBe('true')

    await button(wrapper, 'Package').trigger('click')
    await flushPromises()
    expect(mocks.allPackages).toHaveBeenCalledWith({ active: true })

    const select = wrapper.get('select')
    const options = select.findAll('option').map((item) => item.text())
    expect(options).toHaveLength(2)
    expect(options[1]).toContain('Five sessions - ')
    expect(options.join(' ')).not.toContain('Retired plan')

    await submit(wrapper)
    expect(wrapper.text()).toContain('Choose a package.')
    expect(mocks.sellPackage).not.toHaveBeenCalled()

    await select.setValue('package-1')
    await submit(wrapper)
    expect(mocks.sellPackage).toHaveBeenCalledWith({
      contact_id: 'contact-1',
      package_definition_id: 'package-1',
      due_at: undefined,
      idempotency_key: '00000000-0000-4000-8000-000000000001',
      metadata: { lead_id: 'lead-1', source: 'customer_workspace' },
    })
    expect(mocks.createInvoice).not.toHaveBeenCalled()
    expect(mocks.toastSuccess).toHaveBeenCalledWith('Invoice INV-0002 created')
    expect((wrapper.emitted('created')?.[0]?.[0] as { invoice_number: string }).invoice_number).toBe('INV-0002')

    await button(wrapper, 'Custom amount').trigger('click')
    await button(wrapper, 'Package').trigger('click')
    expect(mocks.allPackages).toHaveBeenCalledTimes(1)
  })

  it('offers a retry when packages fail to load', async () => {
    mocks.allPackages.mockRejectedValueOnce(new Error('Packages offline'))
    const wrapper = mountDialog({ canSellPackages: true })
    await button(wrapper, 'Package').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Packages offline')
    await button(wrapper, 'Try again').trigger('click')
    await flushPromises()
    expect(mocks.allPackages).toHaveBeenCalledTimes(2)
    expect(wrapper.get('select').text()).toContain('Five sessions')
  })

  it('disables submit without a customer', () => {
    const wrapper = mountDialog({ contact: null })
    expect(wrapper.get('[data-testid="invoice-quick-submit"]').attributes('disabled')).toBeDefined()
  })
})
