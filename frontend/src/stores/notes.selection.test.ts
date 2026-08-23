/** @vitest-environment happy-dom */

import { createPinia, setActivePinia } from 'pinia'
import { nextTick } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { ConversationNote } from '@/services/api'
import { useNotesStore } from './notes'

const mocks = vi.hoisted(() => ({
  listNotes: vi.fn(),
  createNote: vi.fn(),
  updateNote: vi.fn(),
  deleteNote: vi.fn(),
}))

vi.mock('@/services/api', () => ({
  notesService: {
    list: mocks.listNotes,
    create: mocks.createNote,
    update: mocks.updateNote,
    delete: mocks.deleteNote,
  },
}))

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(res => {
    resolve = res
  })
  return { promise, resolve }
}

function note(id: string, contactId: string): ConversationNote {
  return {
    id,
    contact_id: contactId,
    created_by_id: 'agent-1',
    created_by_name: 'Agent One',
    content: id,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  }
}

describe('notes store conversation selection', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    mocks.listNotes.mockResolvedValue({
      data: { data: { notes: [], has_more: false } },
    })
  })

  it('ignores a late notes response from the previously selected contact', async () => {
    const store = useNotesStore()
    const firstResponse = deferred<any>()
    const secondResponse = deferred<any>()
    mocks.listNotes.mockImplementation((contactId: string) =>
      contactId === 'contact-a' ? firstResponse.promise : secondResponse.promise,
    )

    const firstLoad = store.fetchNotes('contact-a')
    const secondLoad = store.fetchNotes('contact-b')

    secondResponse.resolve({
      data: {
        data: {
          notes: [note('note-b', 'contact-b')],
          has_more: false,
        },
      },
    })
    await secondLoad
    firstResponse.resolve({
      data: {
        data: {
          notes: [note('note-a-stale', 'contact-a')],
          has_more: true,
        },
      },
    })
    await firstLoad

    expect(store.currentContactId).toBe('contact-b')
    expect(store.notes.map(item => item.id)).toEqual(['note-b'])
    expect(store.hasMore).toBe(false)
    expect(store.isLoading).toBe(false)
  })

  it('invalidates an in-flight notes request when the chat is cleared', async () => {
    const store = useNotesStore()
    const response = deferred<any>()
    mocks.listNotes.mockReturnValue(response.promise)

    const load = store.fetchNotes('contact-a')
    store.clearNotes()
    response.resolve({
      data: {
        data: {
          notes: [note('note-a-stale', 'contact-a')],
          has_more: true,
        },
      },
    })
    await load

    expect(store.currentContactId).toBeNull()
    expect(store.notes).toEqual([])
    expect(store.hasMore).toBe(false)
    expect(store.isLoading).toBe(false)
  })

  it('does not append a late create response after the contact changes', async () => {
    const store = useNotesStore()
    await store.fetchNotes('contact-a')
    const createResponse = deferred<any>()
    mocks.createNote.mockReturnValue(createResponse.promise)

    const create = store.createNote('contact-a', 'private note')
    await store.fetchNotes('contact-b')
    createResponse.resolve({ data: { data: note('note-a-created', 'contact-a') } })
    await create

    expect(store.currentContactId).toBe('contact-b')
    expect(store.notes).toEqual([])
  })

  it('does not apply a late update response after the contact changes', async () => {
    const store = useNotesStore()
    mocks.listNotes.mockResolvedValueOnce({
      data: { data: { notes: [note('note-a', 'contact-a')], has_more: false } },
    })
    await store.fetchNotes('contact-a')
    const updateResponse = deferred<any>()
    mocks.updateNote.mockReturnValue(updateResponse.promise)

    const update = store.updateNote('contact-a', 'note-a', 'updated private note')
    mocks.listNotes.mockResolvedValueOnce({
      data: { data: { notes: [note('note-b', 'contact-b')], has_more: false } },
    })
    await store.fetchNotes('contact-b')
    updateResponse.resolve({
      data: {
        data: { ...note('note-a', 'contact-a'), content: 'updated private note' },
      },
    })
    await update

    expect(store.currentContactId).toBe('contact-b')
    expect(store.notes.map(item => item.id)).toEqual(['note-b'])
  })

  it('does not apply a late delete response after identity/contact reset', async () => {
    const store = useNotesStore()
    mocks.listNotes.mockResolvedValueOnce({
      data: { data: { notes: [note('note-a', 'contact-a')], has_more: false } },
    })
    await store.fetchNotes('contact-a')
    const deleteResponse = deferred<any>()
    mocks.deleteNote.mockReturnValue(deleteResponse.promise)

    const remove = store.deleteNote('contact-a', 'note-a')
    store.clearNotes()
    mocks.listNotes.mockResolvedValueOnce({
      data: { data: { notes: [note('note-b', 'contact-b')], has_more: false } },
    })
    await store.fetchNotes('contact-b')
    deleteResponse.resolve({ data: { status: 'success' } })
    await remove

    expect(store.currentContactId).toBe('contact-b')
    expect(store.notes.map(item => item.id)).toEqual(['note-b'])
  })

  it('invalidates a mutation when the active organization identity changes', async () => {
    const { useOrganizationsStore } = await import('./organizations')
    const organizationsStore = useOrganizationsStore()
    organizationsStore.selectOrganization('organization-a')
    const store = useNotesStore()
    await store.fetchNotes('contact-a')
    const createResponse = deferred<any>()
    mocks.createNote.mockReturnValue(createResponse.promise)

    const create = store.createNote('contact-a', 'old organization note')
    organizationsStore.selectOrganization('organization-b')
    await nextTick()
    createResponse.resolve({ data: { data: note('note-old-org', 'contact-a') } })
    await create

    expect(store.currentContactId).toBeNull()
    expect(store.notes).toEqual([])
  })
})
