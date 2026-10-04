/** @vitest-environment happy-dom */

import type { InternalAxiosRequestConfig } from 'axios'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, contactsService, messagesService } from './api'

describe('legacy Chat message service', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('forces every transcript GET to opt out of read acknowledgement', async () => {
    const signal = new AbortController().signal
    const get = vi.spyOn(api, 'get').mockResolvedValue({ data: { data: { messages: [] } } })

    await messagesService.list('contact-1', {
      page: 2,
      limit: 50,
      before_id: 'message-cursor',
      account: 'clinic-line',
    }, signal)

    expect(get).toHaveBeenCalledWith('/contacts/contact-1/messages', {
      params: {
        page: 2,
        limit: 50,
        before_id: 'message-cursor',
        account: 'clinic-line',
        acknowledge: false,
      },
      signal,
    })
  })

  it('pins an exact Chat read cursor to the captured organization', async () => {
    const post = vi.spyOn(api, 'post').mockResolvedValue({ data: { data: { cursor_synced: true } } })

    await contactsService.markRead(
      'contact/with spaces',
      'message-visible',
      'organization-captured',
    )

    expect(post).toHaveBeenCalledWith(
      '/contacts/contact%2Fwith%20spaces/mark-read',
      { last_visible_message_id: 'message-visible' },
      { headers: { 'X-Organization-ID': 'organization-captured' } },
    )
  })

  it('pins chat media to the transcript workspace over the persisted selection', async () => {
    const signal = new AbortController().signal
    let sent: InternalAxiosRequestConfig | undefined
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => {
      sent = config
      return { data: new Blob(['photo'], { type: 'image/jpeg' }), status: 200, statusText: 'OK', headers: {}, config }
    })
    const previousAdapter = api.defaults.adapter
    api.defaults.adapter = adapter
    localStorage.setItem('selected_organization_id', 'organization-persisted')
    try {
      const response = await messagesService.getMedia('message/1', 'organization-transcript', signal)
      expect(response.data).toBeInstanceOf(Blob)
    } finally {
      api.defaults.adapter = previousAdapter
      localStorage.removeItem('selected_organization_id')
    }

    expect(adapter).toHaveBeenCalledTimes(1)
    expect(sent?.url).toBe('/media/message%2F1')
    expect(sent?.responseType).toBe('blob')
    expect(sent?.signal).toBe(signal)
    // The request interceptor must not replace the explicit workspace.
    expect(sent?.headers.get('X-Organization-ID')).toBe('organization-transcript')
  })

  it('fails closed instead of loading chat media from the login workspace', () => {
    const get = vi.spyOn(api, 'get')

    expect(() => messagesService.getMedia('message-1', ' ')).toThrow(
      'Organization is required to load chat media',
    )
    expect(get).not.toHaveBeenCalled()
  })

  it('fails closed instead of inheriting another organization for a Chat read cursor', () => {
    const post = vi.spyOn(api, 'post')

    expect(() => contactsService.markRead('contact-1', 'message-visible', '  ')).toThrow(
      'Organization is required to mark a chat conversation read',
    )
    expect(post).not.toHaveBeenCalled()
  })
})
