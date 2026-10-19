/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { reactive, ref } from 'vue'
import ConversationNotes from './ConversationNotes.vue'

const mocks = vi.hoisted(() => ({
  notesStore: null as any,
  authStore: { user: { id: 'me' } },
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params ? `${key}:${JSON.stringify(params)}` : key,
  }),
}))

vi.mock('@/stores/notes', () => ({
  useNotesStore: () => mocks.notesStore,
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => mocks.authStore,
}))

vi.mock('@/composables/useInfiniteScroll', () => ({
  useInfiniteScroll: () => ({
    scrollAreaRef: ref(null),
    setup: vi.fn(),
    cleanup: vi.fn(),
    preserveScrollPosition: async (fn: () => Promise<void>) => fn(),
  }),
}))

vi.mock('vue-sonner', () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}))

function note(overrides: Record<string, unknown>) {
  return {
    id: 'note-1',
    contact_id: 'contact-1',
    created_by_id: 'me',
    created_by_name: 'Me Myself',
    content: 'Follow up on lab results',
    created_at: '2026-10-15T08:00:00Z',
    updated_at: '2026-10-15T08:00:00Z',
    ...overrides,
  }
}

function mountNotes(notes: ReturnType<typeof note>[]) {
  mocks.notesStore = reactive({
    notes,
    isLoading: false,
    isLoadingOlder: false,
    hasMore: false,
    currentContactId: 'contact-1',
    fetchNotes: vi.fn(),
    fetchOlderNotes: vi.fn(),
    createNote: vi.fn(),
    updateNote: vi.fn().mockResolvedValue(undefined),
    deleteNote: vi.fn().mockResolvedValue(undefined),
  })
  return mount(ConversationNotes, {
    props: { contactId: 'contact-1' },
    global: {
      stubs: {
        ScrollArea: { template: '<div><slot /></div>' },
        Avatar: { template: '<div><slot /></div>' },
        AvatarFallback: { template: '<span><slot /></span>' },
      },
    },
  })
}

describe('ConversationNotes', () => {
  let wrapper: VueWrapper | null = null

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    vi.restoreAllMocks()
  })

  it('always shows labelled edit and delete buttons on my own notes', async () => {
    wrapper = mountNotes([note({})])
    await flushPromises()

    const edit = wrapper.get('[data-testid="note-edit-button"]')
    const del = wrapper.get('[data-testid="note-delete-button"]')
    expect(edit.attributes('aria-label')).toBe('conversationNotes.editNote')
    expect(edit.attributes('title')).toBe('conversationNotes.editNote')
    expect(edit.attributes('type')).toBe('button')
    expect(del.attributes('aria-label')).toBe('conversationNotes.deleteNote')
    expect(del.attributes('type')).toBe('button')
    // Not hidden behind hover.
    expect(edit.element.parentElement?.className).not.toContain('opacity-0')
    // The e2e ChatPage helpers select the action buttons by these classes.
    expect(edit.classes()).toEqual(expect.arrayContaining(['h-5', 'w-5']))
    expect(del.classes()).toEqual(expect.arrayContaining(['h-5', 'w-5']))
  })

  it("hides the actions on other people's notes", async () => {
    wrapper = mountNotes([note({ created_by_id: 'someone-else', created_by_name: 'Other' })])
    await flushPromises()

    expect(wrapper.find('[data-testid="note-edit-button"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="note-delete-button"]').exists()).toBe(false)
  })

  it('marks edited notes only', async () => {
    wrapper = mountNotes([
      note({ id: 'fresh' }),
      note({ id: 'edited', created_by_id: 'someone-else', updated_at: '2026-10-15T09:30:00Z' }),
    ])
    await flushPromises()

    const markers = wrapper.findAll('[data-testid="note-edited-marker"]')
    expect(markers).toHaveLength(1)
    expect(markers[0].text()).toBe('conversationNotes.edited')
  })

  it('opens the editor and saves the edit', async () => {
    wrapper = mountNotes([note({})])
    await flushPromises()

    await wrapper.get('[data-testid="note-edit-button"]').trigger('click')
    const textarea = wrapper.get('textarea')
    expect((textarea.element as HTMLTextAreaElement).value).toBe('Follow up on lab results')
    await textarea.setValue('Lab results reviewed')
    const save = wrapper.findAll('button').find((button) => button.text().includes('common.save'))
    expect(save).toBeTruthy()
    await save!.trigger('click')
    await flushPromises()

    expect(mocks.notesStore.updateNote).toHaveBeenCalledWith('contact-1', 'note-1', 'Lab results reviewed')
  })
})
