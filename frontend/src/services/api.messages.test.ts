/** @vitest-environment happy-dom */

import type { AxiosProgressEvent, InternalAxiosRequestConfig } from 'axios'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, contactsService, MEDIA_STALL_TIMEOUT_MS, messagesService } from './api'

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

  describe('chat media', () => {
    type Adapter = (config: InternalAxiosRequestConfig) => Promise<unknown>
    const ok = (config: InternalAxiosRequestConfig) => ({
      data: new Blob(['photo'], { type: 'image/jpeg' }),
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    })
    const progressEvent = (loaded: number, total: number) =>
      ({ loaded, total, bytes: loaded, lengthComputable: true }) as AxiosProgressEvent
    // Settles only when the request is aborted, like a connection that stalls.
    const untilAborted = (config: InternalAxiosRequestConfig) =>
      new Promise((_, reject) => {
        config.signal?.addEventListener?.('abort', () => reject(new Error('aborted')))
      })

    let previousAdapter: typeof api.defaults.adapter
    function useAdapter(adapter: Adapter) {
      const mock = vi.fn(adapter)
      api.defaults.adapter = mock as unknown as typeof api.defaults.adapter
      return mock
    }

    beforeEach(() => {
      previousAdapter = api.defaults.adapter
    })

    afterEach(() => {
      api.defaults.adapter = previousAdapter
      localStorage.removeItem('selected_organization_id')
      vi.useRealTimers()
    })

    it('pins chat media to the transcript workspace over the persisted selection', async () => {
      let sent: InternalAxiosRequestConfig | undefined
      const adapter = useAdapter(async config => {
        sent = config
        return ok(config)
      })
      localStorage.setItem('selected_organization_id', 'organization-persisted')

      const response = await messagesService.getMedia('message/1', 'organization-transcript')

      expect(response.data).toBeInstanceOf(Blob)
      expect(adapter).toHaveBeenCalledTimes(1)
      expect(sent?.url).toBe('/media/message%2F1')
      expect(sent?.responseType).toBe('blob')
      // The request interceptor must not replace the explicit workspace.
      expect(sent?.headers.get('X-Organization-ID')).toBe('organization-transcript')
      // Large media is limited by the stall watchdog, not a total timeout.
      expect(sent?.timeout).toBe(0)
    })

    it('aborts the request when the caller aborts', async () => {
      let sentSignal: AbortSignal | undefined
      useAdapter(config => {
        sentSignal = config.signal as AbortSignal
        return untilAborted(config)
      })
      const caller = new AbortController()

      const request = messagesService.getMedia('message-1', 'organization-transcript', { signal: caller.signal })
      const outcome = expect(request).rejects.toBeTruthy()
      await vi.waitFor(() => expect(sentSignal).toBeDefined())
      caller.abort()

      await outcome
      expect(sentSignal?.aborted).toBe(true)
    })

    it('gives up on a download that stops making progress', async () => {
      vi.useFakeTimers()
      let sentSignal: AbortSignal | undefined
      useAdapter(config => {
        sentSignal = config.signal as AbortSignal
        return untilAborted(config)
      })

      const request = messagesService.getMedia('message-1', 'organization-transcript')
      const outcome = expect(request).rejects.toThrow('Chat media download stalled')
      await vi.advanceTimersByTimeAsync(MEDIA_STALL_TIMEOUT_MS - 1)
      expect(sentSignal?.aborted).toBe(false)
      await vi.advanceTimersByTimeAsync(1)

      await outcome
      expect(sentSignal?.aborted).toBe(true)
    })

    it('keeps a slow download that is still receiving bytes and reports its progress', async () => {
      vi.useFakeTimers()
      const onProgress = vi.fn()
      useAdapter(async config => {
        // Each chunk arrives just inside the stall window; the whole transfer
        // takes far longer than any fixed timeout would allow.
        for (let chunk = 1; chunk <= 8; chunk++) {
          await new Promise(resolve => setTimeout(resolve, MEDIA_STALL_TIMEOUT_MS - 1000))
          config.onDownloadProgress?.(progressEvent(chunk * 2_000_000, 16_000_000))
        }
        return ok(config)
      })

      const request = messagesService.getMedia('message-1', 'organization-transcript', { onProgress })
      await vi.advanceTimersByTimeAsync(8 * MEDIA_STALL_TIMEOUT_MS)

      await expect(request).resolves.toMatchObject({ status: 200 })
      expect(onProgress).toHaveBeenCalledTimes(8)
      expect(onProgress).toHaveBeenLastCalledWith(16_000_000, 16_000_000)
    })
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
