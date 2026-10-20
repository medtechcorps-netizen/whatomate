/** @vitest-environment happy-dom */

// Synthetic binding test for reply-with-attachment. The media handler
// (internal/handlers/contacts.go SendMediaMessage) reads the multipart field
// "reply_to_message_id"; its DB-backed behaviour is covered by
// TestLaneESendMediaMessageCarriesReplyContext / ...RejectsForeignReply.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, messagesService } from './api'

function pdf() {
  return new File(['%PDF-1.4'], 'consent.pdf', { type: 'application/pdf' })
}

describe('messagesService.sendMedia', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('sends reply_to_message_id as the multipart field the media handler reads', async () => {
    const post = vi.spyOn(api, 'post').mockResolvedValue({ data: { data: { id: 'm-new' } } })
    const signal = new AbortController().signal

    await messagesService.sendMedia(
      {
        file: pdf(),
        contact_id: 'contact-a',
        type: 'document',
        caption: 'Your consent form',
        whatsapp_account: 'clinic-main',
        reply_to_message_id: 'message-original',
      },
      { signal },
    )

    expect(post).toHaveBeenCalledTimes(1)
    const [url, body, config] = post.mock.calls[0]
    expect(url).toBe('/messages/media')
    const form = body as FormData
    expect(form.get('reply_to_message_id')).toBe('message-original')
    expect(form.get('contact_id')).toBe('contact-a')
    expect(form.get('type')).toBe('document')
    expect(form.get('caption')).toBe('Your consent form')
    expect(form.get('whatsapp_account')).toBe('clinic-main')
    expect((form.get('file') as File).name).toBe('consent.pdf')
    expect(config).toMatchObject({
      timeout: 0,
      signal,
      headers: { 'Content-Type': 'multipart/form-data' },
    })
  })

  it('omits the reply field when the batch is not a reply', async () => {
    const post = vi.spyOn(api, 'post').mockResolvedValue({ data: { data: {} } })
    await messagesService.sendMedia({ file: pdf(), contact_id: 'contact-a', type: 'document' })
    const form = post.mock.calls[0][1] as FormData
    expect(form.has('reply_to_message_id')).toBe(false)
    expect(form.has('caption')).toBe(false)
    expect(form.has('whatsapp_account')).toBe(false)
  })

  it('reports upload progress', async () => {
    const post = vi.spyOn(api, 'post').mockResolvedValue({ data: { data: {} } })
    const onUploadProgress = vi.fn()
    await messagesService.sendMedia({ file: pdf(), contact_id: 'contact-a', type: 'document' }, { onUploadProgress })
    const config = post.mock.calls[0][2] as { onUploadProgress: (event: { loaded: number; total?: number }) => void }
    config.onUploadProgress({ loaded: 5, total: 10 })
    expect(onUploadProgress).toHaveBeenCalledWith(5, 10)
  })
})
