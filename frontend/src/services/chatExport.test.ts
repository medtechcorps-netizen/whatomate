/** @vitest-environment happy-dom */
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { chatExportsService, chatExportError, exportDateBoundary, legacyExportAccount } from './chatExport'

const mocks = vi.hoisted(() => ({ post: vi.fn(), get: vi.fn(), headers: vi.fn() }))
vi.mock('@/services/api', () => ({ api: { post: mocks.post, get: mocks.get, defaults: { baseURL: '/api' } }, getRequestHeaders: mocks.headers }))

beforeEach(() => { vi.clearAllMocks(); vi.unstubAllGlobals(); mocks.headers.mockReturnValue({ 'X-CSRF-Token': 'synthetic' }) })

describe('chat export transport', () => {
  it('preserves Unicode account names and complete local date boundaries', () => {
    expect(legacyExportAccount('医院')).toBe('whatsapp:e58cbbe999a2')
    expect(legacyExportAccount(null)).toBe('')
    expect(exportDateBoundary('2024-02-29')).toBe(new Date(2024, 1, 29).toISOString())
    expect(exportDateBoundary('2024-02-29', true)).toBe(new Date(2024, 2, 1).toISOString())
    expect(() => exportDateBoundary('2025-02-29')).toThrow('valid date')
  })

  it('never treats a JSON error body as a completed archive', async () => {
    mocks.post.mockResolvedValue({ data: new Blob(['{"message":"denied"}'], { type: 'application/json' }) })
    await expect(chatExportsService.download({ scope: 'all' }, 'original-org', new AbortController().signal, vi.fn())).rejects.toThrow('complete chat archive')
    expect(mocks.post).toHaveBeenCalledWith('/chat-exports/download', { scope: 'all' }, expect.objectContaining({ headers: { 'X-Organization-ID': 'original-org' } }))
    expect(await chatExportError({ response: { data: new Blob(['{"error":{"message":"Permission changed"}}'], { type: 'application/json' }) } })).toBe('Permission changed')
  })

  it('cancels the captured organization and distinguishes delivery already started', async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ data: { cancelled: false, preparation_finished: true } }) })
    vi.stubGlobal('fetch', fetchMock)
    expect(await chatExportsService.cancel('request-id', 'original-org')).toEqual({ cancelled: false, preparation_finished: true })
    expect(fetchMock).toHaveBeenCalledWith('/api/chat-exports/request-id/cancel', expect.objectContaining({ method: 'POST', keepalive: true, headers: expect.objectContaining({ 'X-Organization-ID': 'original-org', 'X-CSRF-Token': 'synthetic' }) }))
  })
})
