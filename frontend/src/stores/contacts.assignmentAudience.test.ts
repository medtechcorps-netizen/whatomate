/** @vitest-environment happy-dom */

import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import { useContactsStore, type Contact, type Message } from './contacts'
import { useAuthStore } from './auth'

const mocks = vi.hoisted(() => ({ get: vi.fn(), list: vi.fn(), messages: vi.fn() }))
vi.mock('@/services/api', () => ({
  contactsService: { get: mocks.get, list: mocks.list },
  messagesService: { list: mocks.messages, send: vi.fn(), sendTemplate: vi.fn() },
}))
const contact = (id: string): Contact => ({
  id, name: id, phone_number: id, status: 'active', tags: [], metadata: {}, unread_count: 0,
  identity_review_ai_state: { known: true, ai_allowed: true, blocked: false, open_hold_count: 0, reason: 'no_open_identity_review' },
  created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
})
const message = (id: string): Message => ({ id, contact_id: 'old', direction: 'incoming', status: 'delivered', content: { body: id },
  created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', message_type: 'text' })
const response = (data: unknown) => ({ data: { data } })
function deferred() {
  let resolve!: (value: any) => void
  let reject!: (error: unknown) => void
  const promise = new Promise<any>((ok, fail) => { resolve = ok; reject = fail })
  return { promise, resolve, reject }
}
async function fixture() {
  const auth = useAuthStore()
  auth.user = { id: 'user-1', organization_id: 'org-1', email: 'test@example.com', full_name: 'Test reader' }
  const store = useContactsStore()
  store.contacts = [contact('old')]
  store.setCurrentContact(store.contacts[0]!)
  store.messages = [message('retained')]
  store.replyingTo = store.messages[0]!
  mocks.list.mockResolvedValue(response({ contacts: [], total: 0 }))
  return { auth, store }
}
describe('assignment invalidation revalidates exact current authority', () => {
  beforeEach(() => { vi.useFakeTimers(); setActivePinia(createPinia()); vi.resetAllMocks(); localStorage.clear() })
  afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers() })

  it.each([403, 404])('clears only the denied selected chat on scoped %s and prevents late message resurrection', async status => {
    const { store, auth } = await fixture()
    const pending = deferred()
    mocks.messages.mockReturnValue(pending.promise)
    const loading = store.fetchMessages('old')
    mocks.get.mockRejectedValue({ response: { status } })
    store.scheduleContactsRefresh('old', 'assignment')
    await vi.advanceTimersByTimeAsync(300)
    expect(mocks.get).toHaveBeenCalledWith('old', 'org-1')
    expect(store.currentContact).toBeNull()
    expect(store.messages).toEqual([])
    expect(store.replyingTo).toBeNull()
    pending.resolve(response({ messages: [message('late')], has_more: false }))
    await loading
    expect(store.messages).toEqual([])
    expect(auth.isAuthenticated).toBe(true)
  })

  it.each([503, 500, 429, undefined])('preserves chat and identity on transient or unknown status %s', async status => {
    const { store, auth } = await fixture()
    mocks.get.mockRejectedValue(status ? { response: { status } } : new Error('offline'))
    store.scheduleContactsRefresh('old', 'assignment')
    await vi.advanceTimersByTimeAsync(300)
    expect(store.currentContact?.id).toBe('old')
    expect(store.messages.map(x => x.id)).toEqual(['retained'])
    expect(store.replyingTo?.id).toBe('retained')
    expect(auth.isAuthenticated).toBe(true)
  })

  it('preserves a filtered-out but currently readable contact after a 200 refetch', async () => {
    const { store } = await fixture()
    mocks.get.mockResolvedValue(response({ ...contact('old'), assigned_user_name: 'Current agent' }))
    store.scheduleContactsRefresh('old', 'assignment')
    await vi.advanceTimersByTimeAsync(300)
    expect(store.currentContact?.id).toBe('old')
    expect(store.currentContact?.assigned_user_name).toBe('Current agent')
    expect(store.messages).toHaveLength(1)
  })

  it('revalidates even when a stale list response still contains the selected row', async () => {
    const { store } = await fixture()
    mocks.list.mockResolvedValue(response({ contacts: [contact('old')], total: 1 }))
    mocks.get.mockRejectedValue({ response: { status: 404 } })
    store.scheduleContactsRefresh('old', 'assignment')
    await vi.advanceTimersByTimeAsync(300)
    expect(store.currentContact).toBeNull()
    expect(store.contacts).toEqual([])
    expect(store.contactsTotal).toBe(0)
  })

  it.each(['selection', 'organization', 'user', 'new_assignment'] as const)('ignores old denial after %s changes', async change => {
    const { store, auth } = await fixture()
    const old = deferred()
    mocks.get.mockReturnValueOnce(old.promise).mockResolvedValue(response(contact('old')))
    store.scheduleContactsRefresh('old', 'assignment')
    await vi.advanceTimersByTimeAsync(300)
    if (change === 'selection') {
      store.setCurrentContact(contact('new'))
      store.setCurrentContact(contact('old'))
    } else if (change === 'organization') {
      auth.user = { ...auth.user!, organization_id: 'org-2' }
      await nextTick()
      store.setCurrentContact(contact('new-org'))
    } else if (change === 'user') {
      auth.user = { ...auth.user!, id: 'user-2' }
    } else {
      store.scheduleContactsRefresh('old', 'assignment')
      await vi.advanceTimersByTimeAsync(300)
    }
    old.reject({ response: { status: 404 } })
    await vi.advanceTimersByTimeAsync(1)
    expect(store.currentContact).not.toBeNull()
  })

  it('aborts a late list page after confirmed denial without replacing the current scope', async () => {
    const { store } = await fixture()
    const denied = deferred()
    mocks.get.mockReturnValue(denied.promise)
    store.scheduleContactsRefresh('old', 'assignment')
    await vi.advanceTimersByTimeAsync(300)
    const stale = deferred()
    mocks.list.mockReturnValue(stale.promise)
    const loading = store.fetchContacts()
    denied.reject({ response: { status: 404 } })
    await vi.advanceTimersByTimeAsync(1)
    stale.resolve(response({ contacts: [contact('old')], total: 1 }))
    await loading
    expect(store.contacts).toEqual([])
    expect(store.currentContact).toBeNull()
    expect(store.isLoading).toBe(false)
  })
})
