/** @vitest-environment happy-dom */

import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, nextTick, reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { PackageDefinition } from '@/services/productSuite'
import CommerceView from './CommerceView.vue'

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
  post: vi.fn(),
  hasPermission: vi.fn(),
  success: vi.fn(),
  error: vi.fn(),
  warning: vi.fn(),
}))

// Exercise the real commerce client and pagination contract, with no transport.
vi.mock('@/services/api', () => ({
  api: { get: mocks.get, put: mocks.put, post: mocks.post },
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ hasPermission: mocks.hasPermission }),
}))
vi.mock('@/composables/useAppToast', () => ({
  useAppToast: () => ({
    success: mocks.success,
    error: mocks.error,
    warning: mocks.warning,
  }),
}))

const Passthrough = defineComponent({ template: '<div><slot /></div>' })
const ButtonStub = defineComponent({
  props: { disabled: Boolean, type: String },
  emits: ['click'],
  template:
    '<button :type="type || \'button\'" :disabled="disabled" @click="$emit(\'click\', $event)"><slot /></button>',
})
const DialogStub = defineComponent({
  props: { open: Boolean },
  emits: ['update:open'],
  template: '<section v-if="open" role="dialog"><slot /></section>',
})
const AlertDialogStub = defineComponent({
  props: { open: Boolean },
  emits: ['update:open'],
  template: '<section v-if="open" role="alertdialog"><slot /></section>',
})
const ContactPickerStub = defineComponent({
  props: { modelValue: String },
  emits: ['update:modelValue'],
  template:
    '<input data-testid="contact-picker" :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />',
})

function mountCommerce() {
  return mount(CommerceView, {
    global: {
      stubs: {
        PageHeader: defineComponent({
          template: '<header><slot name="actions" /></header>',
        }),
        ContactPicker: ContactPickerStub,
        Badge: Passthrough,
        Button: ButtonStub,
        Dialog: DialogStub,
        DialogContent: Passthrough,
        DialogDescription: Passthrough,
        DialogFooter: Passthrough,
        DialogHeader: Passthrough,
        DialogTitle: Passthrough,
        AlertDialog: AlertDialogStub,
        AlertDialogCancel: ButtonStub,
        AlertDialogContent: Passthrough,
        AlertDialogDescription: Passthrough,
        AlertDialogFooter: Passthrough,
        AlertDialogHeader: Passthrough,
        AlertDialogTitle: Passthrough,
      },
    },
  })
}

let wrapper: ReturnType<typeof mountCommerce> | undefined
let permissions: Set<string>
let definitions: PackageDefinition[]

function envelope<T>(data: T) {
  return { data: { data } }
}

function activeDefinition(): PackageDefinition {
  return {
    id: 'package-active',
    name: 'Consultation plan',
    description: 'Five consultations',
    price_minor: 45000,
    currency: 'MYR',
    validity_days: 90,
    is_active: true,
    version: 7,
    metadata: { source: 'reviewed-catalogue', clinical_note: { keep: true } },
    entitlements: [
      {
        id: 'rule-1',
        booking_service_id: 'service-1',
        credits: 5,
        is_unlimited: false,
      },
    ],
  }
}

function readFixture(url: string, config?: { params?: Record<string, unknown> }) {
  if (url === '/packages') {
    const active = config?.params?.active
    const items = definitions.filter((item) => active === undefined || item.is_active === active)
    return envelope({
      packages: items.map((item) => ({ ...item })),
      total: items.length,
    })
  }
  if (url === '/contact-packages') {
    return envelope({
      contact_packages: [
        {
          id: 'owned-plan',
          contact_id: 'customer-1',
          package_definition_id: 'package-active',
          status: 'active',
          contact: { profile_name: 'Aina Hassan' },
          package_definition: definitions[0],
          balances: [{ available: 3, reserved: 1, used: 1 }],
          invoice_id: 'invoice-paid',
        },
      ],
      total: 1,
    })
  }
  if (url === '/booking/services') {
    return envelope({
      services: [{ id: 'service-1', name: 'Consultation', is_active: true }],
      total: 1,
    })
  }
  if (url === '/invoices') return envelope({ invoices: [], total: 0 })
  if (url === '/payments') return envelope({ payments: [], total: 0 })
  if (url === '/commerce/summary') {
    return envelope({
      packages_visible: true,
      active_packages: definitions.filter((item) => item.is_active).length,
      outstanding: [],
      collected_charges: [],
    })
  }
  throw new Error(`Unexpected mocked request: GET ${url}`)
}

function commitUpdate(url: string, body: Partial<PackageDefinition>) {
  const id = decodeURIComponent(url.split('/').pop() ?? '')
  const index = definitions.findIndex((item) => item.id === id)
  if (!url.startsWith('/packages/') || index < 0) throw new Error(`Unexpected mocked request: PUT ${url}`)
  const updated = {
    ...definitions[index]!,
    ...body,
    version: definitions[index]!.version + 1,
  }
  definitions[index] = updated
  return envelope(updated)
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((done, fail) => {
    resolve = done
    reject = fail
  })
  return { promise, resolve, reject }
}

function button(view: ReturnType<typeof mountCommerce>, text: string) {
  const found = view.findAll('button').find((item) => item.text().trim() === text)
  if (!found) throw new Error(`Button not found: ${text}`)
  return found
}

async function openCommerce() {
  wrapper = mountCommerce()
  const view = wrapper
  await vi.waitFor(() => expect(view.find('[data-testid="package-card-package-active"]').exists()).toBe(true))
  return view
}

describe('Commerce package management', () => {
  beforeEach(() => {
    for (const mock of Object.values(mocks)) mock.mockReset()
    permissions = reactive(
      new Set([
        'packages:read',
        'packages:write',
        'packages:delete',
        'booking.settings:read',
        'contacts:read',
        'payments:read',
        'payments:write',
      ]),
    )
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      permissions.has(`${resource}:${action}`),
    )
    definitions = [
      activeDefinition(),
      {
        ...activeDefinition(),
        id: 'package-retired',
        name: 'Previous care plan',
        is_active: false,
        version: 11,
      },
    ]
    mocks.get.mockImplementation(async (url, config) => readFixture(url, config))
    mocks.put.mockImplementation(async (url, body) => commitUpdate(url, body))
    mocks.post.mockImplementation(async (url) => {
      if (!['/packages', '/package-sales', '/contact-packages'].includes(url))
        throw new Error(`Unexpected mocked request: POST ${url}`)
      return envelope({ id: 'created-record' })
    })
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = undefined
    vi.restoreAllMocks()
  })

  it('requests Active and Retired catalogues while keeping active sale choices and existing plan history separate', async () => {
    const view = await openCommerce()
    expect(mocks.get).toHaveBeenCalledWith('/packages', {
      params: { active: true, page: 1, limit: 100 },
    })
    expect(view.find('[data-testid="package-card-package-retired"]').exists()).toBe(false)
    await view.get('[data-testid="package-status-filter"]').setValue('retired')
    await vi.waitFor(() => {
      expect(view.find('[data-testid="package-card-package-retired"]').exists()).toBe(true)
      expect(view.find('[data-testid="package-card-package-active"]').exists()).toBe(false)
    })
    expect(mocks.get).toHaveBeenCalledWith('/packages', {
      params: { active: false, page: 1, limit: 100 },
    })
    expect(view.text()).toContain('Existing customer plans, credits, invoices and booking history are kept.')
    await button(view, 'Customer plans').trigger('click')
    expect(view.get('[data-testid="customer-plan-owned-plan"]').text()).toContain('Aina Hassan')
    expect(view.get('[data-testid="customer-plan-owned-plan"]').text()).toMatch(/Available credits\s*3/)
    expect(
      view
        .get('[data-testid="package-sale-selection"]')
        .findAll('option')
        .map((item) => item.attributes('value')),
    ).toEqual(['', 'package-active'])
  })

  it('ignores a late Retired response after the operator has switched back to Active', async () => {
    const view = await openCommerce()
    const pending = deferred<ReturnType<typeof envelope>>()
    mocks.get.mockImplementation((url, config) =>
      url === '/packages' && config?.params?.active === false
        ? pending.promise
        : Promise.resolve(readFixture(url, config)),
    )
    await view.get('[data-testid="package-status-filter"]').setValue('retired')
    await view.get('[data-testid="package-status-filter"]').setValue('active')
    await vi.waitFor(() => expect(view.find('[data-testid="package-card-package-active"]').exists()).toBe(true))
    pending.resolve(envelope({ packages: [definitions[1]], total: 1 }))
    // Drain the resolved mock request before checking that it cannot overwrite
    // the newer catalogue; an assertion before this completion could pass early.
    await flushPromises()
    await vi.waitFor(() => {
      expect(view.get('[data-testid="package-status-filter"]').element).toHaveProperty('value', 'active')
      expect(view.find('[data-testid="package-card-package-active"]').exists()).toBe(true)
      expect(view.find('[data-testid="package-card-package-retired"]').exists()).toBe(false)
    })
  })

  it.each([
    ['read only', false, false, false, false],
    ['write only', true, false, true, false],
    ['delete without write', false, true, false, false],
    ['write and delete', true, true, true, true],
  ])('gates edit and retirement independently for %s', async (_label, write, remove, editVisible, retireVisible) => {
    if (!write) permissions.delete('packages:write')
    if (!remove) permissions.delete('packages:delete')
    const view = await openCommerce()
    expect(view.find('button[aria-label="Edit Consultation plan"]').exists()).toBe(editVisible)
    expect(view.find('button[aria-label="Retire Consultation plan"]').exists()).toBe(retireVisible)
    expect(mocks.put).not.toHaveBeenCalled()
  })

  it('waits for an acknowledged versioned edit and preserves metadata, active state and purchased credit rules', async () => {
    const view = await openCommerce()
    const pending = deferred<ReturnType<typeof envelope>>()
    mocks.put.mockReturnValueOnce(pending.promise)
    await view.get('button[aria-label="Edit Consultation plan"]').trigger('click')
    await view.get('[data-testid="package-edit-name"]').setValue('Updated consultation plan')
    await view.get('[data-testid="package-edit-price"]').setValue('123.45')
    await view.get('[data-testid="package-edit-validity"]').setValue('120')
    await view.get('#package-edit-form').trigger('submit')
    const expected = {
      name: 'Updated consultation plan',
      description: 'Five consultations',
      price_minor: 12345,
      currency: 'MYR',
      validity_days: 120,
      version: 7,
      metadata: activeDefinition().metadata,
    }
    expect(mocks.put).toHaveBeenCalledExactlyOnceWith('/packages/package-active', expected)
    expect(view.find('[role="dialog"]').exists()).toBe(true)
    expect(button(view, 'Save package').attributes('disabled')).toBeDefined()
    expect(mocks.success).not.toHaveBeenCalled()
    pending.resolve(commitUpdate('/packages/package-active', expected))
    await vi.waitFor(() => {
      expect(view.find('[role="dialog"]').exists()).toBe(false)
      expect(view.get('[data-testid="package-card-package-active"]').text()).toContain('Updated consultation plan')
      expect(mocks.success).toHaveBeenCalledWith('Package updated', expect.any(String))
    })
    expect(definitions[0]!.entitlements).toEqual(activeDefinition().entitlements)
    expect(definitions[0]!.is_active).toBe(true)
  })

  it('lets write-only users edit a retired package without sending false or reactivating it', async () => {
    permissions.delete('packages:delete')
    const view = await openCommerce()
    await view.get('[data-testid="package-status-filter"]').setValue('retired')
    await vi.waitFor(() => expect(view.find('button[aria-label="Edit Previous care plan"]').exists()).toBe(true))
    await view.get('button[aria-label="Edit Previous care plan"]').trigger('click')
    expect(view.get('[role="dialog"]').text()).toContain('saving does not reactivate it')
    await view.get('[data-testid="package-edit-name"]').setValue('Previous care plan — reference')
    await view.get('#package-edit-form').trigger('submit')
    await vi.waitFor(() => {
      expect(view.find('[role="dialog"]').exists()).toBe(false)
      expect(view.get('[data-testid="package-card-package-retired"]').text()).toContain(
        'Previous care plan — reference',
      )
    })
    expect(mocks.put).toHaveBeenCalledExactlyOnceWith('/packages/package-retired', {
      name: 'Previous care plan — reference',
      description: 'Five consultations',
      price_minor: 45000,
      currency: 'MYR',
      validity_days: 90,
      version: 11,
      metadata: activeDefinition().metadata,
    })
    expect(definitions[1]!.is_active).toBe(false)
    expect(view.find('button[aria-label="Retire Previous care plan — reference"]').exists()).toBe(false)
  })

  it('retains a rejected edit draft and original version until the operator cancels', async () => {
    const view = await openCommerce()
    mocks.put.mockRejectedValueOnce(new Error('Package was modified; refresh and retry'))
    await view.get('button[aria-label="Edit Consultation plan"]').trigger('click')
    await view.get('[data-testid="package-edit-name"]').setValue('Unsaved clinical plan')
    await view.get('#package-edit-form').trigger('submit')
    await vi.waitFor(() => {
      expect(view.get('[role="dialog"] [role="alert"]').text()).toContain('Package was modified')
      expect(button(view, 'Save package').attributes('disabled')).toBeUndefined()
    })
    expect(view.get('[data-testid="package-edit-name"]').element).toHaveProperty('value', 'Unsaved clinical plan')
    expect(definitions[0]!.name).toBe('Consultation plan')
    expect(mocks.put.mock.calls[0]![1]).toHaveProperty('version', 7)
    expect(mocks.success).not.toHaveBeenCalled()
    await button(view, 'Cancel edit').trigger('click')
    expect(view.find('[role="dialog"]').exists()).toBe(false)
    expect(mocks.put).toHaveBeenCalledTimes(1)
  })

  it('allows an explicit retry of a recoverable edit failure without losing the draft', async () => {
    const view = await openCommerce()
    mocks.put.mockRejectedValueOnce(new Error('Temporary save failure'))
    await view.get('button[aria-label="Edit Consultation plan"]').trigger('click')
    await view.get('[data-testid="package-edit-name"]').setValue('Recovered care plan')
    await view.get('#package-edit-form').trigger('submit')
    await vi.waitFor(() => {
      expect(view.get('[role="dialog"] [role="alert"]').text()).toContain('Temporary save failure')
      expect(button(view, 'Save package').attributes('disabled')).toBeUndefined()
    })
    expect(view.get('[data-testid="package-edit-name"]').element).toHaveProperty('value', 'Recovered care plan')
    await view.get('#package-edit-form').trigger('submit')
    await vi.waitFor(() => {
      expect(view.find('[role="dialog"]').exists()).toBe(false)
      expect(view.get('[data-testid="package-card-package-active"]').text()).toContain('Recovered care plan')
    })
    expect(mocks.put).toHaveBeenCalledTimes(2)
    expect(mocks.put.mock.calls[1]![1]).toEqual(mocks.put.mock.calls[0]![1])
    expect(mocks.success).toHaveBeenCalledExactlyOnceWith('Package updated', expect.any(String))
  })

  it('cancels an edit without sending any mutation', async () => {
    const view = await openCommerce()
    await view.get('button[aria-label="Edit Consultation plan"]').trigger('click')
    await view.get('[data-testid="package-edit-name"]').setValue('Not saved')
    await button(view, 'Cancel edit').trigger('click')
    expect(view.find('[role="dialog"]').exists()).toBe(false)
    expect(mocks.put).not.toHaveBeenCalled()
    expect(definitions[0]!.name).toBe('Consultation plan')
  })

  it('rechecks write permission before submitting an already-open edit', async () => {
    const view = await openCommerce()
    await view.get('button[aria-label="Edit Consultation plan"]').trigger('click')
    permissions.delete('packages:write')
    await nextTick()
    await view.get('#package-edit-form').trigger('submit')
    expect(view.get('[role="dialog"] [role="alert"]').text()).toContain('write access is required')
    expect(mocks.put).not.toHaveBeenCalled()
  })

  it('requires confirmation, supports cancellation, and retires without deleting plans, credits or history', async () => {
    const view = await openCommerce()
    await view.get('button[aria-label="Retire Consultation plan"]').trigger('click')
    expect(view.get('[role="alertdialog"]').text()).toContain('Nothing is deleted.')
    expect(mocks.put).not.toHaveBeenCalled()
    await button(view, 'Keep package active').trigger('click')
    expect(view.find('[role="alertdialog"]').exists()).toBe(false)
    expect(mocks.put).not.toHaveBeenCalled()
    await view.get('button[aria-label="Retire Consultation plan"]').trigger('click')
    const pending = deferred<ReturnType<typeof envelope>>()
    mocks.put.mockReturnValueOnce(pending.promise)
    await button(view, 'Retire package').trigger('click')
    const expected = {
      name: 'Consultation plan',
      description: 'Five consultations',
      price_minor: 45000,
      currency: 'MYR',
      validity_days: 90,
      version: 7,
      metadata: activeDefinition().metadata,
      is_active: false,
    }
    expect(mocks.put).toHaveBeenCalledExactlyOnceWith('/packages/package-active', expected)
    expect(view.find('[role="alertdialog"]').exists()).toBe(true)
    expect(button(view, 'Retire package').attributes('disabled')).toBeDefined()
    await button(view, 'Retire package').trigger('click')
    expect(mocks.put).toHaveBeenCalledTimes(1)
    pending.resolve(commitUpdate('/packages/package-active', expected))
    await vi.waitFor(() => {
      expect(view.find('[role="alertdialog"]').exists()).toBe(false)
      expect(view.find('[data-testid="package-card-package-active"]').exists()).toBe(false)
      expect(mocks.success).toHaveBeenCalledWith('Package retired', expect.any(String))
    })
    await view.get('[data-testid="package-status-filter"]').setValue('retired')
    await vi.waitFor(() => expect(view.get('[data-testid="package-card-package-active"]').text()).toContain('Retired'))
    await button(view, 'Customer plans').trigger('click')
    expect(view.get('[data-testid="customer-plan-owned-plan"]').text()).toMatch(/Available credits\s*3/)
    expect(view.get('[data-testid="customer-plan-owned-plan"]').text()).toContain('Invoice-backed sale')
    expect(
      view
        .get('[data-testid="package-sale-selection"]')
        .findAll('option')
        .map((item) => item.attributes('value')),
    ).toEqual([''])
    expect(definitions[0]!.entitlements).toEqual(activeDefinition().entitlements)
    expect(mocks.post).not.toHaveBeenCalled()
  })

  it('keeps a failed retirement confirmation open and leaves the active catalogue unchanged', async () => {
    const view = await openCommerce()
    mocks.put.mockRejectedValueOnce(new Error('Retirement was rejected'))
    await view.get('button[aria-label="Retire Consultation plan"]').trigger('click')
    await button(view, 'Retire package').trigger('click')
    await vi.waitFor(() => {
      expect(view.get('[role="alertdialog"] [role="alert"]').text()).toContain('Retirement was rejected')
      expect(button(view, 'Retire package').attributes('disabled')).toBeUndefined()
    })
    expect(definitions[0]!.is_active).toBe(true)
    expect(view.find('[data-testid="package-card-package-active"]').exists()).toBe(true)
    expect(mocks.success).not.toHaveBeenCalled()
    await button(view, 'Keep package active').trigger('click')
    expect(view.find('[role="alertdialog"]').exists()).toBe(false)
    expect(mocks.put).toHaveBeenCalledTimes(1)
  })

  it('blocks further sales after acknowledged retirement even when the catalogue refresh fails', async () => {
    const view = await openCommerce()
    await view.get('button[aria-label="Retire Consultation plan"]').trigger('click')
    mocks.get.mockRejectedValue(new Error('Catalogue refresh unavailable'))
    await button(view, 'Retire package').trigger('click')
    await vi.waitFor(() => {
      expect(view.find('[role="alertdialog"]').exists()).toBe(false)
      expect(view.get('[role="alert"]').text()).toContain('Catalogue refresh unavailable')
      expect(view.find('[data-testid="package-card-package-active"]').exists()).toBe(false)
    })
    await button(view, 'Customer plans').trigger('click')
    expect(view.get('[data-testid="customer-plan-owned-plan"]').text()).toMatch(/Available credits\s*3/)
    expect(
      view
        .get('[data-testid="package-sale-selection"]')
        .findAll('option')
        .map((item) => item.attributes('value')),
    ).toEqual([''])
    await view.get('[data-testid="contact-picker"]').setValue('customer-1')
    await view.get('[data-testid="package-sale-form"]').trigger('submit')
    expect(mocks.post).not.toHaveBeenCalled()
    expect(mocks.warning).toHaveBeenCalledWith('Choose an active package and customer')
  })

  it.each(['invoice', 'grant'])('blocks a tampered retired selection for a new %s operation', async (mode) => {
    const view = await openCommerce()
    await button(view, 'Customer plans').trigger('click')
    await view.get('[data-testid="contact-picker"]').setValue('customer-1')
    const form = view.get('[data-testid="package-sale-form"]')
    await form.findAll('select')[1]!.setValue(mode)
    const select = view.get('[data-testid="package-sale-selection"]')
    const option = document.createElement('option')
    option.value = 'package-retired'
    option.textContent = 'Injected retired selection'
    select.element.appendChild(option)
    await select.setValue('package-retired')
    await form.trigger('submit')
    expect(mocks.post).not.toHaveBeenCalled()
    expect(mocks.warning).toHaveBeenCalledWith('Choose an active package and customer')
  })

  it('keeps creation active-only with an explicit service-credit rule', async () => {
    const view = await openCommerce()
    const form = view.get('[data-testid="package-create-form"]')
    await form.get('input[maxlength="255"]').setValue('New care plan')
    await form.get('input[step="0.01"]').setValue('250.00')
    await form.trigger('submit')
    await vi.waitFor(() => expect(mocks.success).toHaveBeenCalledWith('Package created'))
    expect(mocks.post).toHaveBeenCalledExactlyOnceWith('/packages', {
      name: 'New care plan',
      description: '',
      price_minor: 25000,
      currency: 'MYR',
      validity_days: 30,
      is_active: true,
      entitlements: [{ booking_service_id: 'service-1', credits: 5, is_unlimited: false }],
    })
    expect(form.find('input[name="is_active"]').exists()).toBe(false)
  })
})
