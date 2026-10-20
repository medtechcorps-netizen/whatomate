import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Contact, Message } from './contacts'

vi.mock('@/lib/browserIdentity', async importOriginal => ({
  ...(await importOriginal<typeof import('@/lib/browserIdentity')>()),
  readSelectedOrganizationId: () => null,
}))

vi.mock('@/services/api', () => ({
  contactsService: { get: vi.fn(), list: vi.fn() },
  messagesService: { list: vi.fn(), send: vi.fn(), sendTemplate: vi.fn() },
}))

function contact(id: string): Contact {
  return {
    id,
    phone_number: `phone-${id}`,
    name: `Contact ${id}`,
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
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  }
}

function message(overrides: Partial<Message> & Record<string, unknown> = {}): Message {
  return {
    id: 'forwarded-1',
    contact_id: 'open',
    direction: 'outgoing',
    message_type: 'text',
    content: { body: 'forwarded text' },
    status: 'pending',
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  } as Message
}

describe('contacts store: a late send response merges into the socket copy', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  it('fills is_forwarded (and other missing fields) without regressing a newer status', async () => {
    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    store.contacts = [contact('open')]
    store.setCurrentContact(contact('open'))

    // The socket copy lands first and is already 'sent'; it lacks the flag.
    store.addMessage(message({ status: 'sent' }))
    // Then the HTTP response for the same message, still 'pending'.
    store.addMessage(message({ status: 'pending', is_forwarded: true, wamid: 'wamid.x' }))

    expect(store.messages).toHaveLength(1)
    const merged = store.messages[0] as Message & { is_forwarded?: boolean }
    expect(merged.is_forwarded).toBe(true)
    expect(merged.wamid).toBe('wamid.x')
    expect(merged.status).toBe('sent')
  })

  it('leaves an identical duplicate untouched', async () => {
    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    store.contacts = [contact('open')]
    store.setCurrentContact(contact('open'))
    store.addMessage(message())
    const first = store.messages[0]
    store.addMessage(message())
    expect(store.messages).toHaveLength(1)
    expect(store.messages[0]).toBe(first)
  })
})
