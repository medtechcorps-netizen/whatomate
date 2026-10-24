/** @vitest-environment happy-dom */

import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ContactDetailView from './ContactDetailView.vue'

const mocks = vi.hoisted(() => ({
  getContact: vi.fn(),
  updateContact: vi.fn(),
  push: vi.fn(),
}))

vi.mock('@/services/api', () => ({
  contactsService: { get: mocks.getContact, update: mocks.updateContact, delete: vi.fn() },
  accountsService: { list: vi.fn(async () => ({ data: { data: { accounts: [] } } })) },
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ hasPermission: () => true }),
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

const Passthrough = defineComponent({ template: '<div><slot /></div>' })

function record(overrides: Record<string, unknown> = {}) {
  return {
    id: 'contact-1',
    phone_number: '60123456789',
    profile_name: 'Ali (Clinic)',
    name: 'Ali (Clinic)',
    name_source: 'wa_business',
    wa_profile_name: 'Ali WA',
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

let wrapper: ReturnType<typeof mount> | undefined

async function openDetail() {
  const view = mount(ContactDetailView, {
    global: {
      mocks: { $t: (key: string, fallback?: unknown) => (typeof fallback === 'string' ? fallback : key) },
      stubs: {
        DetailPageLayout: defineComponent({
          props: { title: String, isLoading: Boolean },
          template: '<main><h1>{{ title }}</h1><slot name="actions" /><slot v-if="!isLoading" /></main>',
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
  await flushPromises()
  await vi.waitFor(() => expect(view.find('[data-testid="contact-name-clear-hint"]').exists()).toBe(true))
  return view
}

function nameInput(view: ReturnType<typeof mount>) {
  return view.findAll('input').find(input => (input.element as HTMLInputElement).value === 'Ali (Clinic)')
}

async function save(view: ReturnType<typeof mount>) {
  const button = view.findAll('button').find(item => item.text().includes('common.save'))
  expect(button, 'the save button shows after a change').toBeTruthy()
  await button!.trigger('click')
  await flushPromises()
  expect(mocks.updateContact).toHaveBeenCalledTimes(1)
  return mocks.updateContact.mock.calls[0][1] as Record<string, unknown>
}

beforeEach(() => {
  mocks.getContact.mockReset().mockResolvedValue({ data: { data: record() } })
  mocks.updateContact.mockReset().mockResolvedValue({ data: { data: record() } })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

describe('ContactDetailView name editing', () => {
  it('shows where the name came from and how clearing it works', async () => {
    const view = await openDetail()
    expect(view.find('[data-testid="contact-name-source-wa-business"]').exists()).toBe(true)
    expect(view.get('[data-testid="contact-wa-profile-name"]').text()).toContain('contacts.nameSource.whatsappProfile')
    expect(view.get('[data-testid="contact-name-clear-hint"]').text()).toBe('contacts.nameSource.clearHint')
  })

  it('does not re-send an unchanged name, so saving other fields is not a rename', async () => {
    const view = await openDetail()
    ;(view.vm as any).form.tags.push('vip')
    await flushPromises()
    const payload = await save(view)
    expect(payload).not.toHaveProperty('profile_name')
    expect(payload.tags).toEqual(['vip'])
  })

  it('sends a name the user changed, including a cleared one', async () => {
    const view = await openDetail()
    await nameInput(view)!.setValue('Dr Ali')
    expect((await save(view)).profile_name).toBe('Dr Ali')

    mocks.updateContact.mockClear()
    // The saved form reloads the contact, so the field shows the stored name again.
    await nameInput(view)!.setValue('')
    expect((await save(view)).profile_name).toBe('')
  })
})
