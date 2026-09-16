/** @vitest-environment happy-dom */

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  listTransfers: vi.fn(),
}))

vi.mock('@/services/api', () => ({
  chatbotService: { listTransfers: mocks.listTransfers },
}))

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>((res) => {
    resolve = res
  })
  return { promise, resolve }
}

function transfer(id: string, contactId: string) {
  return {
    id,
    contact_id: contactId,
    contact_name: `Contact ${contactId}`,
    phone_number: '60123456789',
    whatsapp_account: 'clinic-whatsapp',
    status: 'active' as const,
    source: 'manual' as const,
    transferred_by: 'agent-1',
    transferred_at: '2026-08-30T00:00:00Z',
    sla_breached: false,
    escalation_level: 0,
  }
}

describe('transfers store exact contact state', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
  })

  it('loads an exact active contact state without replacing the global queue', async () => {
    const { useTransfersStore } = await import('./transfers')
    const store = useTransfersStore()
    const queueTransfer = transfer('queue-transfer', 'queue-contact')
    const selectedTransfer = transfer('selected-transfer', 'selected-contact')
    store.transfers.push(queueTransfer)
    mocks.listTransfers.mockResolvedValue({
      data: { data: { transfers: [selectedTransfer] } },
    })

    const result = await store.fetchActiveTransferForContact('selected-contact')

    expect(mocks.listTransfers).toHaveBeenCalledWith({
      status: 'active',
      contact_id: 'selected-contact',
      limit: 1,
      include: 'agent,team,transferred_by',
    })
    expect(result).toEqual(selectedTransfer)
    expect(store.getActiveTransferForContact('selected-contact')).toEqual(selectedTransfer)
    expect(store.transfers).toEqual([queueTransfer])
  })

  it('ignores an older exact-contact response that resolves after a newer lookup', async () => {
    const firstResponse = deferred<any>()
    const secondResponse = deferred<any>()
    mocks.listTransfers.mockReturnValueOnce(firstResponse.promise).mockReturnValueOnce(secondResponse.promise)
    const { useTransfersStore } = await import('./transfers')
    const store = useTransfersStore()

    const firstLoad = store.fetchActiveTransferForContact('selected-contact')
    const secondLoad = store.fetchActiveTransferForContact('selected-contact')
    secondResponse.resolve({ data: { data: { transfers: [] } } })
    await secondLoad
    firstResponse.resolve({
      data: {
        data: {
          transfers: [transfer('stale-transfer', 'selected-contact')],
        },
      },
    })
    await firstLoad

    expect(store.getActiveTransferForContact('selected-contact')).toBeUndefined()
    expect(store.transfers).toEqual([])
  })

  it('keeps an uncached resume event authoritative over an older exact response', async () => {
    const originalExact = deferred<any>()
    const reconciliationExact = deferred<any>()
    const globalRefresh = deferred<any>()
    mocks.listTransfers
      .mockReturnValueOnce(originalExact.promise)
      .mockReturnValueOnce(reconciliationExact.promise)
      .mockReturnValueOnce(globalRefresh.promise)
    const { useTransfersStore } = await import('./transfers')
    const store = useTransfersStore()
    const staleActive = transfer('uncached-transfer', 'selected-contact')

    const originalLoad = store.fetchActiveTransferForContact('selected-contact')
    expect(store.updateTransfer(
      staleActive.id,
      { status: 'resumed' },
      staleActive.contact_id,
    )).toBe(false)
    const reconciliation = store.fetchActiveTransferForContact('selected-contact')
    const refresh = store.fetchTransfers({ status: 'active' })

    reconciliationExact.resolve({ data: { data: { transfers: [] } } })
    globalRefresh.resolve({ data: { data: { transfers: [], total_count: 0 } } })
    await Promise.all([reconciliation, refresh])
    originalExact.resolve({ data: { data: { transfers: [staleActive] } } })

    expect(await originalLoad).toBeUndefined()
    expect(store.getActiveTransferForContact('selected-contact')).toBeUndefined()
    expect(store.transfers).toEqual([])
  })

  it('keeps an uncached reassignment after bounded refresh and an older exact response', async () => {
    const originalExact = deferred<any>()
    const reconciliationExact = deferred<any>()
    const globalRefresh = deferred<any>()
    mocks.listTransfers
      .mockReturnValueOnce(originalExact.promise)
      .mockReturnValueOnce(reconciliationExact.promise)
      .mockReturnValueOnce(globalRefresh.promise)
    const { useTransfersStore } = await import('./transfers')
    const store = useTransfersStore()
    const staleAssignment = {
      ...transfer('uncached-transfer', 'selected-contact'),
      agent_id: 'agent-old',
    }
    const freshAssignment = { ...staleAssignment, agent_id: 'agent-new' }
    const queueTransfer = transfer('queue-transfer', 'queue-contact')

    const originalLoad = store.fetchActiveTransferForContact('selected-contact')
    expect(store.updateTransfer(
      staleAssignment.id,
      { agent_id: 'agent-new' },
      staleAssignment.contact_id,
    )).toBe(false)
    const reconciliation = store.fetchActiveTransferForContact('selected-contact')
    const refresh = store.fetchTransfers({ status: 'active' })

    reconciliationExact.resolve({ data: { data: { transfers: [freshAssignment] } } })
    await reconciliation
    globalRefresh.resolve({
      data: { data: { transfers: [queueTransfer], total_count: 20 } },
    })
    await refresh
    originalExact.resolve({ data: { data: { transfers: [staleAssignment] } } })

    expect((await originalLoad)?.agent_id).toBe('agent-new')
    expect(store.getActiveTransferForContact('selected-contact')?.agent_id).toBe('agent-new')
    expect(store.transfers).toEqual([queueTransfer])
  })

  it('keeps a newer active transfer when an older exact-contact empty response resolves', async () => {
    const response = deferred<any>()
    mocks.listTransfers.mockReturnValue(response.promise)
    const { useTransfersStore } = await import('./transfers')
    const store = useTransfersStore()
    const activeTransfer = transfer('new-transfer', 'selected-contact')

    const load = store.fetchActiveTransferForContact('selected-contact')
    store.upsertTransfer(activeTransfer)
    response.resolve({ data: { data: { transfers: [] } } })
    const result = await load

    expect(result).toEqual(activeTransfer)
    expect(store.getActiveTransferForContact('selected-contact')).toEqual(activeTransfer)
  })

  it('does not resurrect a resumed transfer from an older exact-contact response', async () => {
    const response = deferred<any>()
    mocks.listTransfers.mockReturnValue(response.promise)
    const { useTransfersStore } = await import('./transfers')
    const store = useTransfersStore()
    const activeTransfer = transfer('active-transfer', 'selected-contact')
    store.upsertTransfer(activeTransfer)

    const load = store.fetchActiveTransferForContact('selected-contact')
    expect(store.updateTransfer(activeTransfer.id, { status: 'resumed' })).toBe(true)
    response.resolve({ data: { data: { transfers: [activeTransfer] } } })
    const result = await load

    expect(result).toBeUndefined()
    expect(store.getActiveTransferForContact('selected-contact')).toBeUndefined()
    expect(store.transfers[0].status).toBe('resumed')
  })

  it('keeps a newer reassignment when an older active response resolves', async () => {
    const response = deferred<any>()
    mocks.listTransfers.mockReturnValue(response.promise)
    const { useTransfersStore } = await import('./transfers')
    const store = useTransfersStore()
    const oldAssignment = { ...transfer('active-transfer', 'selected-contact'), agent_id: 'agent-old' }
    store.upsertTransfer(oldAssignment)

    const load = store.fetchActiveTransferForContact('selected-contact')
    expect(store.updateTransfer(oldAssignment.id, { agent_id: 'agent-new' })).toBe(true)
    response.resolve({ data: { data: { transfers: [oldAssignment] } } })
    const result = await load

    expect(result?.agent_id).toBe('agent-new')
    expect(store.getActiveTransferForContact('selected-contact')?.agent_id).toBe('agent-new')
  })

  it('does not restore a removed transfer from an older exact-contact response', async () => {
    const response = deferred<any>()
    mocks.listTransfers.mockReturnValue(response.promise)
    const { useTransfersStore } = await import('./transfers')
    const store = useTransfersStore()
    const activeTransfer = transfer('active-transfer', 'selected-contact')
    store.upsertTransfer(activeTransfer)

    const load = store.fetchActiveTransferForContact('selected-contact')
    store.removeTransfer(activeTransfer.id)
    response.resolve({ data: { data: { transfers: [activeTransfer] } } })
    const result = await load

    expect(result).toBeUndefined()
    expect(store.getActiveTransferForContact('selected-contact')).toBeUndefined()
    expect(store.transfers).toEqual([])
  })

  it('upserts a create response immediately and clears it on resume', async () => {
    const { useTransfersStore } = await import('./transfers')
    const store = useTransfersStore()
    const created = transfer('created-transfer', 'selected-contact')

    store.upsertTransfer(created)
    store.upsertTransfer({ ...created, notes: 'Human takeover' })

    expect(store.transfers).toHaveLength(1)
    expect(store.getActiveTransferForContact('selected-contact')?.notes).toBe('Human takeover')
    expect(store.updateTransfer(created.id, { status: 'resumed' })).toBe(true)
    expect(store.getActiveTransferForContact('selected-contact')).toBeUndefined()
  })

  it('ignores a late exact-contact response after an identity reset', async () => {
    const response = deferred<any>()
    mocks.listTransfers.mockReturnValue(response.promise)
    const { useTransfersStore } = await import('./transfers')
    const store = useTransfersStore()

    const load = store.fetchActiveTransferForContact('old-contact')
    store.resetForIdentityChange()
    response.resolve({
      data: { data: { transfers: [transfer('old-transfer', 'old-contact')] } },
    })
    await load

    expect(store.getActiveTransferForContact('old-contact')).toBeUndefined()
    expect(store.transfers).toEqual([])
  })
})
