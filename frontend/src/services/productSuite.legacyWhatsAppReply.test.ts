/** @vitest-environment happy-dom */

import type { AxiosAdapter, InternalAxiosRequestConfig } from 'axios'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/services/api'
import { channelsService } from './productSuite'

const payload = {
  type: 'text' as const,
  content: { body: 'Exact reply body' },
  idempotency_key: '00000000-0000-4000-8000-000000000091',
}

describe('conversation-scoped legacy WhatsApp reply organization header', () => {
  let originalAdapter: typeof api.defaults.adapter
  let request: InternalAxiosRequestConfig | null
  let adapter: AxiosAdapter

  beforeEach(() => {
    localStorage.clear()
    originalAdapter = api.defaults.adapter
    request = null
    adapter = vi.fn(async config => {
      request = config
      return {
        data: { data: { id: 'message-1' } },
        status: 200,
        statusText: 'OK',
        headers: {},
        config,
      }
    })
    api.defaults.adapter = adapter
  })

  afterEach(() => {
    api.defaults.adapter = originalAdapter
    localStorage.clear()
  })

  it.each([
    ['fresh login with no stored fallback', null],
    ['a mismatched stored fallback', 'organization-wrong'],
  ])('pins the captured organization through the interceptor for %s', async (_label, storedFallback) => {
    if (storedFallback) {
      localStorage.setItem('selected_organization_id', storedFallback)
    }

    await channelsService.replyLegacyWhatsApp(
      'conversation-1',
      payload,
      'organization-captured',
    )

    expect(adapter).toHaveBeenCalledTimes(1)
    expect(request?.url).toBe(
      '/conversations/conversation-1/legacy-whatsapp-replies',
    )
    expect(request?.headers.get('X-Organization-ID')).toBe(
      'organization-captured',
    )
  })

  it('fails closed instead of using a fallback when the captured organization is empty', () => {
    localStorage.setItem('selected_organization_id', 'organization-wrong')

    expect(() =>
      channelsService.replyLegacyWhatsApp('conversation-1', payload, '  '),
    ).toThrow('Organization is required for a WhatsApp reply')
    expect(adapter).not.toHaveBeenCalled()
  })

  it.each([
    ['attention summary', () => channelsService.attentionSummary('organization-captured')],
    ['read cursor', () => channelsService.markRead(
      'conversation-1',
      { last_visible_message_id: 'message-visible' },
      'organization-captured',
    )],
  ])('pins the captured organization for the %s request', async (_label, requestAction) => {
    localStorage.setItem('selected_organization_id', 'organization-wrong')

    await requestAction()

    expect(adapter).toHaveBeenCalledTimes(1)
    expect(request?.headers.get('X-Organization-ID')).toBe('organization-captured')
  })

  it('fails closed for read-state requests without a captured organization', () => {
    localStorage.setItem('selected_organization_id', 'organization-wrong')

    expect(() => channelsService.attentionSummary('  ')).toThrow(
      'Organization is required for an inbox attention summary',
    )
    expect(() => channelsService.markRead(
      'conversation-1',
      { last_visible_message_id: 'message-visible' },
      '  ',
    )).toThrow('Organization is required to mark an inbox conversation read')
    expect(adapter).not.toHaveBeenCalled()
  })
})
