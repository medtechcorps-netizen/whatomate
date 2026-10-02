/** @vitest-environment happy-dom */

import { mount, RouterLinkStub } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ContactsView from './ContactsView.vue'
import ContactDetailView from './ContactDetailView.vue'

const mocks = vi.hoisted(() => ({
  listContacts: vi.fn(),
  getContact: vi.fn(),
  listAccounts: vi.fn(),
  hasPermission: vi.fn(),
  push: vi.fn(),
}))

vi.mock('@/services/api', () => ({
  contactsService: {
    list: mocks.listContacts,
    get: mocks.getContact,
    update: vi.fn(),
    delete: vi.fn(),
  },
  accountsService: { list: mocks.listAccounts },
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ hasPermission: mocks.hasPermission }),
}))
vi.mock('@/stores/tags', () => ({
  useTagsStore: () => ({ tags: [], fetchTags: vi.fn(async () => {}), getTagByName: () => undefined }),
}))
vi.mock('@/stores/users', () => ({
  useUsersStore: () => ({ users: [], fetchUsers: vi.fn(async () => {}) }),
}))
vi.mock('vue-router', () => ({
  useRouter: () => ({ push: mocks.push, beforeEach: () => () => {} }),
  useRoute: () => ({ params: { id: 'contact-1' } }),
}))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string, fallback?: unknown) => (typeof fallback === 'string' ? fallback : key) }),
}))
vi.mock('vue-sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

// The shared barrel pulls in many unrelated components; stub what the list uses.
vi.mock('@/components/shared', async () => {
  const { defineComponent: define } = await import('vue')
  const Empty = define({ template: '<div><slot /></div>' })
  return {
    PageHeader: define({ template: '<header><slot name="actions" /></header>' }),
    SearchInput: Empty,
    DeleteConfirmDialog: Empty,
    CreateContactDialog: Empty,
    ImportExportDialog: Empty,
    ErrorState: Empty,
    IconButton: define({ props: { label: String }, template: '<button :aria-label="label"><slot /></button>' }),
    // Render each item's cells through the same slots the real table uses.
    DataTable: define({
      props: { items: { type: Array, default: () => [] }, columns: { type: Array, default: () => [] } },
      template: `<table><tbody>
        <tr v-for="item in items" :key="item.id" data-testid="contact-row">
          <td v-for="column in columns" :key="column.key" :data-column="column.key">
            <slot :name="'cell-' + column.key" :item="item" />
          </td>
        </tr>
      </tbody></table>`,
    }),
  }
})

const Passthrough = defineComponent({ template: '<div><slot /></div>' })

function contact(overrides: Record<string, unknown>) {
  return {
    id: 'contact-1',
    phone_number: '',
    profile_name: '',
    name: '',
    whatsapp_account: '',
    tags: [],
    metadata: {},
    assigned_user_id: null,
    last_message_at: null,
    last_message_preview: '',
    unread_count: 0,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

const HIDDEN_HINT = "WhatsApp did not share this customer's number (they message with a WhatsApp username)."

// Text a sighted user sees: drop screen-reader-only helpers.
function visibleText(node: { element: Element }) {
  const clone = node.element.cloneNode(true) as Element
  clone.querySelectorAll('.sr-only').forEach((item) => item.remove())
  return (clone.textContent ?? '').replace(/\s+/g, ' ').trim()
}

let wrapper: { unmount: () => void } | undefined

beforeEach(() => {
  for (const mock of Object.values(mocks)) mock.mockReset()
  mocks.hasPermission.mockReturnValue(true)
  mocks.listAccounts.mockResolvedValue({ data: { data: { accounts: [] } } })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

describe('ContactsView phone column', () => {
  async function openList(contacts: ReturnType<typeof contact>[]) {
    mocks.listContacts.mockResolvedValue({ data: { data: { contacts, total: contacts.length } } })
    const view = mount(ContactsView, {
      global: {
        mocks: { $t: (key: string) => key },
        stubs: {
          RouterLink: RouterLinkStub,
          ScrollArea: Passthrough,
          Card: Passthrough,
          CardContent: Passthrough,
          CardDescription: Passthrough,
          CardHeader: Passthrough,
          CardTitle: Passthrough,
          TagBadge: Passthrough,
        },
      },
    })
    wrapper = view
    await vi.waitFor(() => expect(view.findAll('[data-testid="contact-row"]')).toHaveLength(contacts.length))
    return view
  }

  it('keeps showing a real phone number as plain text', async () => {
    const view = await openList([contact({ phone_number: '+10000000001', profile_name: 'Test Customer' })])
    const cell = view.get('[data-column="phone_number"]')
    expect(cell.get('code').text()).toBe('+10000000001')
    expect(cell.find('[data-testid="contact-phone-missing"]').exists()).toBe(false)
    expect(view.get('[data-column="profile_name"]').text()).toContain('Test Customer')
  })

  it('explains a missing WhatsApp number instead of leaving the cell blank', async () => {
    const view = await openList([contact({ phone_number: '' })])
    const badge = view.get('[data-column="phone_number"] [data-testid="contact-phone-missing"]')
    expect(visibleText(badge)).toBe('WhatsApp number hidden')
    expect(badge.attributes('title')).toBe(HIDDEN_HINT)
    expect(view.get('[data-column="profile_name"]').text()).toContain('WhatsApp user')
  })

  it('reads the hidden-number hint to screen readers, not only as a hover title', async () => {
    const view = await openList([contact({ phone_number: '' })])
    const badge = view.get('[data-column="phone_number"] [data-testid="contact-phone-missing"]')
    const hint = badge.get('[data-testid="contact-phone-missing-hint"]')
    expect(hint.classes()).toContain('sr-only')
    expect(hint.text()).toBe(HIDDEN_HINT)
    expect(badge.attributes('title')).toBe(HIDDEN_HINT)
  })

  it('never shows a raw placeholder as the name', async () => {
    const view = await openList([
      contact({ id: 'contact-1', profile_name: 'bsuid:abc123', phone_number: '+10000000004' }),
      contact({ id: 'contact-2', profile_name: '', name: 'user:xyz', phone_number: 'bsuid:xyz' }),
    ])
    const names = view.findAll('[data-column="profile_name"]').map((cell) => cell.text())
    expect(names[0]).toBe('+10000000004')
    expect(names[1]).toBe('WhatsApp user')
    expect(view.text()).not.toContain('bsuid:')
    expect(view.text()).not.toContain('user:xyz')
  })

  it('uses the saved name when there is no profile name', async () => {
    const view = await openList([contact({ name: 'Saved Name', phone_number: '+10000000005' })])
    expect(view.get('[data-column="profile_name"]').text()).toBe('Saved Name')
  })

  it('treats placeholder numbers as hidden and shows the WhatsApp username', async () => {
    const view = await openList([
      contact({
        phone_number: 'bsuid:abc123',
        profile_name: 'Test Customer',
        metadata: { coexistence_username: 'testcustomer' },
      }),
    ])
    const cell = view.get('[data-column="phone_number"]')
    expect(cell.find('code').exists()).toBe(false)
    expect(visibleText(cell)).toBe('WhatsApp number hidden (@testcustomer)')
    expect(view.get('[data-column="profile_name"]').text()).toContain('Test Customer')
  })

  it('falls back to the phone number as the name when there is no name', async () => {
    const view = await openList([contact({ phone_number: '+10000000002' })])
    expect(view.get('[data-column="profile_name"]').text()).toContain('+10000000002')
  })
})

describe('ContactDetailView phone field', () => {
  async function openDetail(record: ReturnType<typeof contact>) {
    mocks.getContact.mockResolvedValue({ data: { data: record } })
    const view = mount(ContactDetailView, {
      global: {
        mocks: { $t: (key: string, fallback?: unknown) => (typeof fallback === 'string' ? fallback : key) },
        stubs: {
          DetailPageLayout: defineComponent({
            props: { title: String, isLoading: Boolean },
            template: '<main><h1>{{ title }}</h1><slot v-if="!isLoading" /></main>',
          }),
          MetadataPanel: true,
          AuditLogPanel: true,
          UnsavedChangesDialog: true,
          Select: true,
          Popover: true,
          AlertDialog: true,
          Card: Passthrough,
          CardContent: Passthrough,
          CardHeader: Passthrough,
          CardTitle: Passthrough,
          Badge: Passthrough,
        },
      },
    })
    wrapper = view
    await vi.waitFor(() => expect(view.find('input[disabled]').exists()).toBe(true))
    return view
  }

  it('shows why the number is hidden under the disabled phone input', async () => {
    const view = await openDetail(contact({ phone_number: '' }))
    const hint = view.get('[data-testid="contact-phone-hint"]')
    expect(hint.text()).toBe(
      "WhatsApp did not share this customer's number (they message with a WhatsApp username).",
    )
    const phoneInput = view.get('input[disabled]')
    expect(phoneInput.attributes('placeholder')).toBe('WhatsApp number hidden')
    expect(phoneInput.attributes('aria-describedby')).toBe('contact-phone-hint')
    expect(view.get('h1').text()).toBe('WhatsApp user')
  })

  it('does not show the hint for a contact with a phone number', async () => {
    const view = await openDetail(contact({ phone_number: '+10000000003', profile_name: 'Test Customer' }))
    expect(view.find('[data-testid="contact-phone-hint"]').exists()).toBe(false)
    expect((view.get('input[disabled]').element as HTMLInputElement).value).toBe('+10000000003')
    expect(view.get('h1').text()).toBe('Test Customer')
  })

  it('titles the page with the phone number instead of a placeholder name', async () => {
    const view = await openDetail(contact({ phone_number: '+10000000006', profile_name: 'bsuid:abc123' }))
    expect(view.get('h1').text()).toBe('+10000000006')
  })
})
