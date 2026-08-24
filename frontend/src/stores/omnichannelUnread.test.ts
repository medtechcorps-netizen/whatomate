/** @vitest-environment happy-dom */

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useOmnichannelUnreadStore } from './omnichannelUnread'

const mocks = vi.hoisted(() => ({
  attentionSummary: vi.fn(),
}))

vi.mock('@/services/productSuite', () => ({
  channelsService: {
    attentionSummary: mocks.attentionSummary,
  },
}))

function summary(unreadConversations: number) {
  return {
    data: {
      data: {
        unread_conversations: unreadConversations,
        as_of: '2026-08-24T12:00:00Z',
      },
    },
  }
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(res => {
    resolve = res
  })
  return { promise, resolve }
}

describe('omnichannel unread store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
  })

  it('parses the attention summary and caps the display count at 99+', async () => {
    const store = useOmnichannelUnreadStore()
    mocks.attentionSummary
      .mockResolvedValueOnce(summary(12))
      .mockResolvedValueOnce(summary(145))
      .mockResolvedValueOnce(summary(0))

    expect(store.unreadConversationCount).toBeNull()
    expect(store.hasUnread).toBe(false)
    expect(store.displayCount).toBe('')
    expect(store.stale).toBe(true)

    await expect(store.refresh(' organization-1 ', ' agent-1 ')).resolves.toBe(true)
    expect(store.organizationId).toBe('organization-1')
    expect(store.userId).toBe('agent-1')
    expect(store.unreadConversationCount).toBe(12)
    expect(store.hasUnread).toBe(true)
    expect(store.displayCount).toBe('12')
    expect(store.loading).toBe(false)
    expect(store.stale).toBe(false)
    expect(mocks.attentionSummary.mock.calls[0]?.[0]).toBe('organization-1')
    expect(mocks.attentionSummary.mock.calls[0]?.[1]).toBeInstanceOf(AbortSignal)

    await store.refresh('organization-1', 'agent-1')
    expect(store.unreadConversationCount).toBe(145)
    expect(store.displayCount).toBe('99+')

    await store.refresh('organization-1', 'agent-1')
    expect(store.unreadConversationCount).toBe(0)
    expect(store.hasUnread).toBe(false)
    expect(store.displayCount).toBe('0')
  })

  it('preserves the last good count across an error and recovers on retry', async () => {
    const store = useOmnichannelUnreadStore()
    mocks.attentionSummary
      .mockResolvedValueOnce(summary(7))
      .mockRejectedValueOnce(new Error('temporary network failure'))
      .mockResolvedValueOnce(summary(3))

    await store.refresh('organization-1', 'agent-1')
    expect(store.unreadConversationCount).toBe(7)
    expect(store.stale).toBe(false)

    await expect(store.refresh('organization-1', 'agent-1')).resolves.toBe(false)
    expect(store.unreadConversationCount).toBe(7)
    expect(store.displayCount).toBe('7')
    expect(store.stale).toBe(true)
    expect(store.loading).toBe(false)

    await expect(store.refresh('organization-1', 'agent-1')).resolves.toBe(true)
    expect(store.unreadConversationCount).toBe(3)
    expect(store.displayCount).toBe('3')
    expect(store.stale).toBe(false)
  })

  it('clears a previous count when API access is revoked', async () => {
    const store = useOmnichannelUnreadStore()
    mocks.attentionSummary
      .mockResolvedValueOnce(summary(7))
      .mockRejectedValueOnce({ response: { status: 403 } })

    await store.refresh('organization-1', 'agent-1')
    await expect(store.refresh('organization-1', 'agent-1')).resolves.toBe(false)

    expect(store.organizationId).toBe('organization-1')
    expect(store.unreadConversationCount).toBeNull()
    expect(store.hasUnread).toBe(false)
    expect(store.stale).toBe(true)
  })

  it('aborts and ignores a late response from the previous organization', async () => {
    const store = useOmnichannelUnreadStore()
    const oldOrganization = deferred<ReturnType<typeof summary>>()
    let oldOrganizationSignal: AbortSignal | undefined
    mocks.attentionSummary
      .mockImplementationOnce((_organizationId: string, signal?: AbortSignal) => {
        oldOrganizationSignal = signal
        return oldOrganization.promise
      })
      .mockResolvedValueOnce(summary(2))

    const oldRefresh = store.refresh('organization-1', 'agent-1')
    expect(store.loading).toBe(true)

    const newRefresh = store.refresh('organization-2', 'agent-1')
    expect(oldOrganizationSignal?.aborted).toBe(true)
    await expect(newRefresh).resolves.toBe(true)
    expect(store.organizationId).toBe('organization-2')
    expect(store.unreadConversationCount).toBe(2)
    expect(store.stale).toBe(false)

    oldOrganization.resolve(summary(88))
    await expect(oldRefresh).resolves.toBe(false)
    expect(store.organizationId).toBe('organization-2')
    expect(store.unreadConversationCount).toBe(2)
    expect(store.stale).toBe(false)
  })

  it('clears immediately on identity reset and ignores the aborted response', async () => {
    const store = useOmnichannelUnreadStore()
    const pending = deferred<ReturnType<typeof summary>>()
    let signal: AbortSignal | undefined
    mocks.attentionSummary.mockImplementationOnce((
      _organizationId: string,
      requestSignal?: AbortSignal,
    ) => {
      signal = requestSignal
      return pending.promise
    })

    store.unreadConversationCount = 9
    const refresh = store.refresh('organization-1', 'agent-1')
    expect(store.loading).toBe(true)

    store.resetForIdentityChange()
    expect(signal?.aborted).toBe(true)
    expect(store.organizationId).toBeNull()
    expect(store.userId).toBeNull()
    expect(store.unreadConversationCount).toBeNull()
    expect(store.hasUnread).toBe(false)
    expect(store.displayCount).toBe('')
    expect(store.loading).toBe(false)
    expect(store.stale).toBe(true)

    pending.resolve(summary(91))
    await expect(refresh).resolves.toBe(false)
    expect(store.organizationId).toBeNull()
    expect(store.unreadConversationCount).toBeNull()
  })

  it('clears a count immediately when the authenticated user changes in the same organization', async () => {
    const store = useOmnichannelUnreadStore()
    const nextUser = deferred<ReturnType<typeof summary>>()
    mocks.attentionSummary
      .mockResolvedValueOnce(summary(8))
      .mockImplementationOnce(() => nextUser.promise)

    await store.refresh('organization-1', 'agent-1')
    expect(store.unreadConversationCount).toBe(8)

    const refresh = store.refresh('organization-1', 'agent-2')
    expect(store.organizationId).toBe('organization-1')
    expect(store.userId).toBe('agent-2')
    expect(store.unreadConversationCount).toBeNull()
    expect(store.stale).toBe(true)

    nextUser.resolve(summary(3))
    await expect(refresh).resolves.toBe(true)
    expect(store.unreadConversationCount).toBe(3)
  })
})
