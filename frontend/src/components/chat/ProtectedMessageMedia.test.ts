/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ProtectedMessageMedia from './ProtectedMessageMedia.vue'
import { chatMediaLoadLimiter } from '@/lib/chatMedia'

const mocks = vi.hoisted(() => ({
  getMedia: vi.fn(),
  openPdfPreview: vi.fn(),
  // Every way messagesService can send; the media bubble must never call one.
  sending: { list: vi.fn(), send: vi.fn(), sendTemplate: vi.fn(), sendReaction: vi.fn(), sendMedia: vi.fn() },
}))
vi.mock('@/services/api', () => ({ messagesService: { getMedia: mocks.getMedia, ...mocks.sending } }))
// Sniffing stays real; pdf.js itself is covered by pdfPreview.test.ts.
vi.mock('@/lib/pdfPreview', async importOriginal => ({
  ...(await importOriginal<typeof import('@/lib/pdfPreview')>()),
  openPdfPreview: mocks.openPdfPreview,
}))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

type MediaOptions = { signal: AbortSignal; onProgress?: (loaded: number, total: number | undefined) => void }

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

const imageMessage = {
  id: 'message-1',
  message_type: 'image',
  media_url: 'organizations/workspace-b/messages/images/photo.jpg',
  media_mime_type: 'image/jpeg',
  content: { body: 'Customer photo' },
}
const mediaResponse = (type = 'image/jpeg', body: BlobPart = 'media bytes') => ({ data: new Blob([body], { type }) })
const requestOptions = (call = 0) => mocks.getMedia.mock.calls[call][2] as MediaOptions
const pdfBody = '%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\n'
const jpegBytes = new Uint8Array([0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46])
const documentMessage = (overrides: Record<string, unknown> = {}) => ({
  ...imageMessage,
  message_type: 'document',
  media_mime_type: 'application/pdf',
  media_filename: 'lab-report.pdf',
  ...overrides,
})
const previewDialog = () => document.body.querySelector<HTMLElement>('[data-testid="chat-media-preview"]')
const pressEscape = () => document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))

describe('ProtectedMessageMedia', () => {
  let wrappers: VueWrapper[] = []
  let createURL: ReturnType<typeof vi.fn<(value: Blob | MediaSource) => string>>
  let revokeURL: ReturnType<typeof vi.fn<(url: string) => void>>

  function mountMedia(props: Partial<InstanceType<typeof ProtectedMessageMedia>['$props']> = {}) {
    const wrapper = mount(ProtectedMessageMedia, {
      props: { organizationId: 'workspace-b', message: imageMessage, ...props },
    })
    wrappers.push(wrapper)
    return wrapper
  }

  async function mountLoaded(props: Partial<InstanceType<typeof ProtectedMessageMedia>['$props']> = {}) {
    const view = mountMedia(props)
    await flushPromises()
    return view
  }

  function createdBlob(call = 0) {
    return createURL.mock.calls[call][0] as Blob
  }

  beforeEach(() => {
    mocks.getMedia.mockReset().mockResolvedValue(mediaResponse())
    mocks.openPdfPreview.mockReset().mockReturnValue(new Promise(() => undefined))
    createURL =vi.fn<(value: Blob | MediaSource) => string>().mockReturnValue('blob:protected-photo')
    revokeURL = vi.fn<(url: string) => void>()
    vi.stubGlobal('URL', class extends URL {
      static createObjectURL = createURL
      static revokeObjectURL = revokeURL
    })
  })

  afterEach(() => {
    for (const wrapper of wrappers) wrapper.unmount()
    wrappers = []
    document.body.innerHTML = ''
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
    // Every download slot is returned, whatever state a bubble was left in.
    expect(chatMediaLoadLimiter.active).toBe(0)
    expect(chatMediaLoadLimiter.waiting).toBe(0)
  })

  it('loads an image as soon as it renders, with bytes pinned to the transcript workspace', async () => {
    const pending = deferred<ReturnType<typeof mediaResponse>>()
    mocks.getMedia.mockReturnValueOnce(pending.promise)
    const view = mountMedia()
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
    expect(mocks.getMedia).toHaveBeenCalledWith('message-1', 'workspace-b', expect.objectContaining({
      signal: expect.any(AbortSignal),
    }))
    expect(view.attributes('aria-busy')).toBe('true')
    expect(view.text()).toBe('chat.mediaLoading')

    pending.resolve(mediaResponse())
    await flushPromises()
    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
    expect(view.get('img').attributes('alt')).toBe('Customer photo')
    expect(view.attributes('aria-busy')).toBeUndefined()
    // No native element is pointed at the header-less API URL or the storage key.
    expect(view.html()).not.toContain('/api/media/')
    expect(view.html()).not.toContain(imageMessage.media_url)
  })

  it('does not announce each loading placeholder to screen readers', () => {
    const view = mountMedia()
    expect(view.find('[role="status"]').exists()).toBe(false)
    expect(view.find('[role="alert"]').exists()).toBe(false)
  })

  it('downloads at most four images at once, newest first, and starts the next when one finishes', async () => {
    const responses = Array.from({ length: 6 }, () => deferred<ReturnType<typeof mediaResponse>>())
    responses.forEach(response => mocks.getMedia.mockReturnValueOnce(response.promise))
    // A transcript renders oldest first; the newest messages are the ones in view.
    for (let index = 0; index < 6; index++) {
      mountMedia({ message: { ...imageMessage, id: `message-${index}` } })
    }
    const requested = () => mocks.getMedia.mock.calls.map(call => call[0])

    await flushPromises()
    expect(requested()).toEqual(['message-5', 'message-4', 'message-3', 'message-2'])
    expect(chatMediaLoadLimiter.waiting).toBe(2)

    responses[0].resolve(mediaResponse())
    await flushPromises()
    expect(requested()).toHaveLength(5)
    expect(requested()[4]).toBe('message-1')

    responses[1].reject(new Error('network'))
    await flushPromises()
    expect(requested()).toHaveLength(6)
    expect(requested()[5]).toBe('message-0')
  })

  it('gives up its place in the queue when it unmounts before its turn', async () => {
    const responses = Array.from({ length: 4 }, () => deferred<ReturnType<typeof mediaResponse>>())
    responses.forEach(response => mocks.getMedia.mockReturnValueOnce(response.promise))
    const views = Array.from({ length: 5 }, (_, index) =>
      mountMedia({ message: { ...imageMessage, id: `message-${index}` } }),
    )
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(4)
    expect(views[0].attributes('data-media-status')).toBe('loading')
    expect(chatMediaLoadLimiter.waiting).toBe(1)

    views[0].unmount()
    wrappers = wrappers.filter(wrapper => wrapper !== views[0])
    expect(chatMediaLoadLimiter.waiting).toBe(0)
    responses[0].resolve(mediaResponse())
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(4)
  })

  it('lets media the agent asks for go ahead of images still waiting for a slot', async () => {
    const responses = Array.from({ length: 4 }, () => deferred<ReturnType<typeof mediaResponse>>())
    responses.forEach(response => mocks.getMedia.mockReturnValueOnce(response.promise))
    for (let index = 0; index < 5; index++) {
      mountMedia({ message: { ...imageMessage, id: `message-${index}` } })
    }
    const video = mountMedia({
      message: { ...imageMessage, id: 'video-1', message_type: 'video', media_mime_type: 'video/mp4' },
    })
    await flushPromises()
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined)
    await video.get('button').trigger('click')
    expect(chatMediaLoadLimiter.waiting).toBe(2)

    mocks.getMedia.mockResolvedValue(mediaResponse('video/mp4'))
    responses[0].resolve(mediaResponse())
    await flushPromises()
    expect(mocks.getMedia.mock.calls[4][0]).toBe('video-1')
  })

  it('opens a loaded raster image in the in-app preview instead of a new tab', async () => {
    const open = vi.fn()
    vi.stubGlobal('open', open)
    const view = await mountLoaded()
    expect(createdBlob().type).toBe('image/jpeg')
    expect(previewDialog()).toBeNull()

    const thumbnail = view.get('button')
    expect(thumbnail.attributes('aria-haspopup')).toBe('dialog')
    await thumbnail.trigger('click')
    await flushPromises()
    expect(open).not.toHaveBeenCalled()

    const preview = previewDialog()!
    expect(preview.dataset.previewKind).toBe('image')
    expect(preview.getAttribute('role')).toBe('dialog')
    // Portaled out of the bubble, which keeps only its thumbnail.
    expect(view.element.contains(preview)).toBe(false)
    expect(view.findAll('button')).toHaveLength(1)
    expect(preview.textContent).toContain('Customer photo')
    const image = preview.querySelector('img')!
    expect(image.getAttribute('src')).toBe('blob:protected-photo')
    expect(image.getAttribute('alt')).toBe('Customer photo')
    // Downloads the bytes already shown; nothing is fetched again.
    const download = preview.querySelector('a[download]')!
    expect(download.getAttribute('href')).toBe('blob:protected-photo')
    expect(download.getAttribute('download')).toBe('image.jpg')
    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
    expect(createURL).toHaveBeenCalledTimes(1)

    pressEscape()
    await flushPromises()
    expect(previewDialog()).toBeNull()
    expect(revokeURL).not.toHaveBeenCalled()
    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
  })

  it('names the preview and its download after the image file when it has one', async () => {
    const view = await mountLoaded({ message: { ...imageMessage, media_filename: 'IMG_0001.jpg' } })
    await view.get('button').trigger('click')
    await flushPromises()
    expect(previewDialog()!.querySelector('h2')?.textContent?.trim()).toBe('IMG_0001.jpg')
    expect(previewDialog()!.querySelector('a[download]')?.getAttribute('download')).toBe('IMG_0001.jpg')
  })

  it('never gives a scriptable image a renderable type or a new-tab preview', async () => {
    mocks.getMedia.mockResolvedValueOnce(
      mediaResponse('image/svg+xml', '<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>'),
    )
    const open = vi.fn()
    vi.stubGlobal('open', open)
    const view = await mountLoaded()

    // "Open image in new tab" on this object URL must download, not render.
    expect(createdBlob().type).toBe('application/octet-stream')
    expect(view.find('button').exists()).toBe(false)
    await view.get('img').trigger('click')
    await flushPromises()
    expect(open).not.toHaveBeenCalled()
    expect(previewDialog()).toBeNull()
  })

  it('downloads a customer HTML document as opaque bytes instead of a same-origin page', async () => {
    const html = '<script>localStorage.setItem("compromised", "1")</script>'
    mocks.getMedia.mockResolvedValueOnce(mediaResponse('text/html', html))
    const clicked = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined)
    const view = mountMedia({
      message: { ...imageMessage, message_type: 'document', media_mime_type: 'text/html', media_filename: 'visit-summary.html' },
    })

    await view.get('button').trigger('click')
    await flushPromises()

    const blob = createdBlob()
    expect(blob.type).toBe('application/octet-stream')
    expect(await blob.text()).toBe(html)
    expect(view.get('a[download]').attributes('download')).toBe('visit-summary.html')
    expect(clicked).toHaveBeenCalledTimes(1)
  })

  it('previews a PDF in the app from its opaque bytes instead of downloading it', async () => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse('application/pdf', pdfBody))
    const clicked = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined)
    const view = mountMedia({ message: documentMessage() })
    await view.get('button').trigger('click')
    await flushPromises()

    expect(clicked).not.toHaveBeenCalled()
    // The only object URL is the opaque download; no PDF-typed URL exists.
    expect(createURL).toHaveBeenCalledTimes(1)
    expect(createdBlob().type).toBe('application/octet-stream')
    expect(mocks.openPdfPreview).toHaveBeenCalledTimes(1)
    expect(await (mocks.openPdfPreview.mock.calls[0][0] as Blob).text()).toBe(pdfBody)

    const preview = previewDialog()!
    expect(preview.dataset.previewKind).toBe('pdf')
    expect(preview.querySelector('h2')?.textContent?.trim()).toBe('lab-report.pdf')
    const download = preview.querySelector('a[download]')!
    expect(download.getAttribute('href')).toBe('blob:protected-photo')
    expect(download.getAttribute('download')).toBe('lab-report.pdf')

    // The bubble keeps the filename, now as the way back into the preview.
    expect(view.attributes('data-media-status')).toBe('ready')
    expect(view.find('a[download]').exists()).toBe(false)
    const reopen = view.get('button')
    expect(reopen.text()).toContain('lab-report.pdf')
    expect(reopen.attributes('aria-haspopup')).toBe('dialog')
  })

  it('reopens a closed preview from the bubble without loading the file again', async () => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse('application/pdf', pdfBody))
    const view = mountMedia({ message: documentMessage() })
    await view.get('button').trigger('click')
    await flushPromises()
    pressEscape()
    await flushPromises()
    expect(previewDialog()).toBeNull()

    await view.get('button').trigger('click')
    await flushPromises()
    expect(previewDialog()?.dataset.previewKind).toBe('pdf')
    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
    expect(mocks.openPdfPreview).toHaveBeenCalledTimes(2)
  })

  it('previews a JPEG sent as a document, with an image type only on the preview URL', async () => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse('application/octet-stream', jpegBytes))
    createURL.mockReturnValueOnce('blob:opaque-scan').mockReturnValueOnce('blob:preview-scan')
    const clicked = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined)
    const view = mountMedia({
      message: documentMessage({ media_mime_type: 'application/octet-stream', media_filename: 'scan.jpg' }),
    })
    await view.get('button').trigger('click')
    await flushPromises()

    expect(clicked).not.toHaveBeenCalled()
    expect(createdBlob(0).type).toBe('application/octet-stream')
    expect(createdBlob(1).type).toBe('image/jpeg')
    const preview = previewDialog()!
    expect(preview.dataset.previewKind).toBe('image')
    expect(preview.querySelector('img')?.getAttribute('src')).toBe('blob:preview-scan')
    expect(preview.querySelector('img')?.getAttribute('alt')).toBe('scan.jpg')
    // The download stays the opaque copy.
    expect(preview.querySelector('a[download]')?.getAttribute('href')).toBe('blob:opaque-scan')
    expect(preview.querySelector('a[download]')?.getAttribute('download')).toBe('scan.jpg')
    expect(mocks.openPdfPreview).not.toHaveBeenCalled()
  })

  it.each([
    ['claims to be a PDF but is HTML', 'application/pdf', '<html><script>alert(1)</script></html>'],
    ['claims to be a PNG but is SVG', 'image/png', '<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>'],
    ['is a GIF', 'image/gif', 'GIF89a'],
  ])('downloads a document that %s instead of previewing it', async (_, mimeType, body) => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse(mimeType, body))
    const clicked = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined)
    const view = mountMedia({ message: documentMessage({ media_mime_type: mimeType }) })
    await view.get('button').trigger('click')
    await flushPromises()

    expect(clicked).toHaveBeenCalledTimes(1)
    expect(createURL).toHaveBeenCalledTimes(1)
    expect(createdBlob().type).toBe('application/octet-stream')
    expect(view.get('a[download]').attributes('download')).toBe('lab-report.pdf')
    expect(previewDialog()).toBeNull()
    expect(mocks.openPdfPreview).not.toHaveBeenCalled()
  })

  it('closes the preview and revokes both object URLs when the workspace changes', async () => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse('application/octet-stream', jpegBytes))
    createURL.mockReturnValueOnce('blob:opaque-scan').mockReturnValueOnce('blob:preview-scan')
    const view = mountMedia({ message: documentMessage({ media_filename: 'scan.jpg' }) })
    await view.get('button').trigger('click')
    await flushPromises()
    expect(previewDialog()).not.toBeNull()

    await view.setProps({ organizationId: 'workspace-c' })
    await flushPromises()
    expect(revokeURL).toHaveBeenCalledWith('blob:opaque-scan')
    expect(revokeURL).toHaveBeenCalledWith('blob:preview-scan')
    expect(previewDialog()).toBeNull()
    expect(view.attributes('data-media-status')).toBe('idle')
  })

  it('revokes the preview URL of a document when unmounted', async () => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse('application/octet-stream', jpegBytes))
    createURL.mockReturnValueOnce('blob:opaque-scan').mockReturnValueOnce('blob:preview-scan')
    const view = mountMedia({ message: documentMessage({ media_filename: 'scan.jpg' }) })
    await view.get('button').trigger('click')
    await flushPromises()

    view.unmount()
    wrappers = []
    expect(revokeURL).toHaveBeenCalledWith('blob:opaque-scan')
    expect(revokeURL).toHaveBeenCalledWith('blob:preview-scan')
  })

  it('fails closed instead of falling back to the login workspace when no workspace is known', async () => {
    const view = await mountLoaded({ organizationId: '  ' })
    expect(mocks.getMedia).not.toHaveBeenCalled()
    expect(view.get('[role="alert"]').text()).toContain('chat.mediaLoadFailed')
    expect(view.find('img').exists()).toBe(false)
  })

  it('aborts and ignores the previous workspace response after the workspace changes', async () => {
    const previous = deferred<ReturnType<typeof mediaResponse>>()
    const current = deferred<ReturnType<typeof mediaResponse>>()
    mocks.getMedia.mockReturnValueOnce(previous.promise).mockReturnValueOnce(current.promise)
    const view = await mountLoaded()
    const previousSignal = requestOptions(0).signal

    await view.setProps({ organizationId: 'workspace-c' })
    expect(previousSignal.aborted).toBe(true)
    await flushPromises()
    expect(mocks.getMedia.mock.calls[1][1]).toBe('workspace-c')

    current.resolve(mediaResponse())
    await flushPromises()
    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
    previous.resolve(mediaResponse())
    await flushPromises()
    expect(createURL).toHaveBeenCalledTimes(1)
  })

  it('revokes the loaded object URL when the workspace changes and when unmounted', async () => {
    const view = await mountLoaded()
    createURL.mockReturnValueOnce('blob:second-workspace')

    await view.setProps({ organizationId: 'workspace-c' })
    expect(revokeURL).toHaveBeenCalledWith('blob:protected-photo')
    await flushPromises()
    expect(view.get('img').attributes('src')).toBe('blob:second-workspace')

    view.unmount()
    wrappers = []
    expect(revokeURL).toHaveBeenCalledWith('blob:second-workspace')
  })

  it('does not reload when a transcript refresh replaces the message object unchanged', async () => {
    const view = await mountLoaded()
    await view.setProps({ message: { ...imageMessage } })
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
    expect(revokeURL).not.toHaveBeenCalled()
  })

  it('does not create an object URL for a response that arrives after unmount', async () => {
    const pending = deferred<ReturnType<typeof mediaResponse>>()
    mocks.getMedia.mockReturnValueOnce(pending.promise)
    const view = await mountLoaded()
    const { signal } = requestOptions(0)

    view.unmount()
    wrappers = []
    expect(signal.aborted).toBe(true)
    pending.resolve(mediaResponse())
    await flushPromises()
    expect(createURL).not.toHaveBeenCalled()
  })

  it('shows a retryable failure as an alert when the media request fails', async () => {
    mocks.getMedia.mockRejectedValueOnce(new Error('404')).mockResolvedValueOnce(mediaResponse())
    const view = await mountLoaded()
    expect(view.get('[role="alert"]').text()).toContain('chat.mediaLoadFailed')
    expect(view.find('img').exists()).toBe(false)

    await view.get('button').trigger('click')
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(2)
    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
  })

  it('treats an empty body as a failure', async () => {
    mocks.getMedia.mockResolvedValueOnce({ data: new Blob([], { type: 'image/jpeg' }) })
    const view = await mountLoaded()
    expect(view.text()).toContain('chat.mediaLoadFailed')
    expect(createURL).not.toHaveBeenCalled()
  })

  it('offers a retry for an image that fails to decode instead of hiding it', async () => {
    const view = await mountLoaded()
    await view.get('img').trigger('error')
    expect(view.text()).toContain('chat.mediaLoadFailed')
    expect(view.find('img').exists()).toBe(false)
    expect(revokeURL).toHaveBeenCalledWith('blob:protected-photo')
  })

  it('discards media when its stored source is removed', async () => {
    const view = await mountLoaded()
    await view.setProps({ message: { ...imageMessage, media_url: '' } })
    expect(revokeURL).toHaveBeenCalledWith('blob:protected-photo')
    await flushPromises()
    expect(view.find('img').exists()).toBe(false)
    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
  })

  it('shows download progress while a large attachment loads', async () => {
    const pending = deferred<ReturnType<typeof mediaResponse>>()
    mocks.getMedia.mockReturnValueOnce(pending.promise)
    const view = mountMedia({ message: { ...imageMessage, message_type: 'video', media_mime_type: 'video/mp4' } })
    await view.get('button').trigger('click')
    await flushPromises()
    expect(view.find('[role="progressbar"]').exists()).toBe(false)

    requestOptions(0).onProgress?.(4_000_000, 16_000_000)
    await flushPromises()
    expect(view.get('[role="progressbar"]').attributes('aria-valuenow')).toBe('25')

    pending.resolve(mediaResponse('video/mp4'))
    await flushPromises()
    expect(view.find('[role="progressbar"]').exists()).toBe(false)
  })

  it('loads a document only when it is opened, then downloads it from the protected bytes', async () => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse('application/pdf'))
    const clicked = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined)
    const view = mountMedia({
      message: { ...imageMessage, message_type: 'document', media_mime_type: 'application/pdf', media_filename: 'lab-report.pdf' },
    })
    await flushPromises()
    expect(mocks.getMedia).not.toHaveBeenCalled()
    const button = view.get('button')
    expect(button.text()).toContain('lab-report.pdf')
    // Logical alignment keeps the filename on the reading side in RTL locales.
    expect(button.classes()).toContain('text-start')

    await button.trigger('click')
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledWith('message-1', 'workspace-b', expect.objectContaining({
      signal: expect.any(AbortSignal),
    }))
    const link = view.get('a[download]')
    expect(link.attributes('href')).toBe('blob:protected-photo')
    expect(link.attributes('download')).toBe('lab-report.pdf')
    expect(clicked).toHaveBeenCalledTimes(1)
  })

  it('keeps the document name in view when its download fails, and retries it', async () => {
    mocks.getMedia.mockRejectedValueOnce(new Error('network')).mockResolvedValueOnce(mediaResponse('application/pdf'))
    const clicked = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined)
    const view = mountMedia({
      message: { ...imageMessage, message_type: 'document', media_mime_type: 'application/pdf', media_filename: 'lab-report.pdf' },
    })
    await view.get('button').trigger('click')
    await flushPromises()

    const alert = view.get('[role="alert"]')
    expect(alert.text()).toContain('lab-report.pdf')
    expect(alert.text()).toContain('chat.mediaLoadFailed')
    expect(alert.get('[title="lab-report.pdf"]').text()).toBe('lab-report.pdf')

    await alert.get('button').trigger('click')
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(2)
    expect(view.get('a[download]').attributes('download')).toBe('lab-report.pdf')
    expect(clicked).toHaveBeenCalledTimes(1)
  })

  it('does not label a failed image with a filename', async () => {
    mocks.getMedia.mockRejectedValueOnce(new Error('404'))
    const view = await mountLoaded({ message: { ...imageMessage, media_filename: 'IMG_0001.jpg' } })
    expect(view.get('[role="alert"]').text()).not.toContain('IMG_0001.jpg')
  })

  it('names a document without a filename with the translated fallback', async () => {
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined)
    const view = mountMedia({
      message: { ...imageMessage, message_type: 'document', media_mime_type: 'application/pdf', media_filename: undefined },
    })
    expect(view.get('button').text()).toContain('chat.document')
    await view.get('button').trigger('click')
    await flushPromises()
    expect(view.get('a[download]').attributes('download')).toBe('chat.document')
  })

  it.each([
    ['video', 'video/mp4', 'video', 'chat.playVideo'],
    ['audio', 'audio/ogg; codecs=opus', 'audio', 'chat.playAudio'],
    ['template', 'video/mp4', 'video', 'chat.playVideo'],
  ])('loads %s (%s) only when the agent presses play, then plays it', async (messageType, mimeType, selector, label) => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse(mimeType))
    const play = vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined)
    const view = mountMedia({ message: { ...imageMessage, message_type: messageType, media_mime_type: mimeType } })
    await flushPromises()
    expect(mocks.getMedia).not.toHaveBeenCalled()
    expect(view.find(selector).exists()).toBe(false)
    expect(view.get('button').text()).toBe(label)

    await view.get('button').trigger('click')
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
    expect(view.get(selector).attributes('src')).toBe('blob:protected-photo')
    expect(createdBlob().type).toBe(mimeType.split(';')[0])
    expect(play).toHaveBeenCalledTimes(1)
  })

  it('keeps the player when the browser refuses to start playback', async () => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse('video/mp4'))
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockRejectedValue(new DOMException('blocked', 'NotAllowedError'))
    const view = mountMedia({ message: { ...imageMessage, message_type: 'video', media_mime_type: 'video/mp4' } })
    await view.get('button').trigger('click')
    await flushPromises()
    expect(view.get('video').attributes('controls')).toBeDefined()
    expect(view.attributes('data-media-status')).toBe('ready')
  })

  it.each([
    ['sticker', 'image/webp', 'chat.sticker'],
    ['template', 'image/jpeg', 'chat.headerImage'],
  ])('renders %s (%s) images with a translated description', async (messageType, mimeType, alt) => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse(mimeType))
    const view = await mountLoaded({ message: { ...imageMessage, message_type: messageType, media_mime_type: mimeType } })
    expect(mocks.getMedia).toHaveBeenCalledWith('message-1', 'workspace-b', expect.objectContaining({
      signal: expect.any(AbortSignal),
    }))
    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
    expect(view.get('img').attributes('alt')).toBe(alt)
    expect(view.html()).not.toContain('/api/media/')
  })

  it('treats a template header that is neither image nor video as an on-demand document', async () => {
    const view = mountMedia({
      message: { ...imageMessage, message_type: 'template', media_mime_type: 'application/pdf', media_filename: 'brochure.pdf' },
    })
    await flushPromises()
    expect(mocks.getMedia).not.toHaveBeenCalled()
    expect(view.get('button').text()).toContain('brochure.pdf')
  })

  describe('a forwarded image whose media is still being copied', () => {
    const notFound = () => Object.assign(new Error('Request failed with status code 404'), { response: { status: 404 } })
    // As the live socket delivers a forward: outgoing and pending, with no
    // forward marker (the socket payload carries none).
    const pendingImage = { ...imageMessage, direction: 'outgoing' as const, status: 'pending' }

    beforeEach(() => {
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    })

    afterEach(() => {
      vi.useRealTimers()
      for (const send of Object.values(mocks.sending)) expect(send).not.toHaveBeenCalled()
    })

    async function wait(ms: number) {
      await vi.advanceTimersByTimeAsync(ms)
      await flushPromises()
    }

    it('loads again by itself once the message leaves pending, and shows the image', async () => {
      mocks.getMedia.mockRejectedValueOnce(notFound()).mockResolvedValueOnce(mediaResponse())
      const view = await mountLoaded({ message: pendingImage })
      expect(view.attributes('data-media-status')).toBe('error')

      // Nothing changes while the message is still pending.
      await wait(30_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(1)

      await view.setProps({ message: { ...pendingImage, status: 'sent' } })
      await wait(1_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(2)
      expect(mocks.getMedia).toHaveBeenLastCalledWith('message-1', 'workspace-b', expect.objectContaining({
        signal: expect.any(AbortSignal),
      }))
      expect(view.attributes('data-media-status')).toBe('ready')
      expect(view.get('img').attributes('src')).toBe('blob:protected-photo')

      // Later status changes leave the loaded image alone.
      await view.setProps({ message: { ...pendingImage, status: 'delivered' } })
      await wait(30_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(2)
    })

    it('tries at most three times, with growing waits, then leaves Retry to the agent', async () => {
      mocks.getMedia.mockRejectedValue(notFound())
      const view = await mountLoaded({ message: pendingImage })
      await view.setProps({ message: { ...pendingImage, status: 'sent' } })

      await wait(999)
      expect(mocks.getMedia).toHaveBeenCalledTimes(1)
      await wait(1)
      expect(mocks.getMedia).toHaveBeenCalledTimes(2)
      await wait(5_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(3)
      await wait(15_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(4)

      // The budget is spent: neither time nor further status changes load again.
      await view.setProps({ message: { ...pendingImage, status: 'delivered' } })
      await view.setProps({ message: { ...pendingImage, status: 'read' } })
      await wait(120_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(4)
      expect(view.get('[role="alert"]').text()).toContain('chat.mediaLoadFailed')

      // Retry still works.
      mocks.getMedia.mockResolvedValueOnce(mediaResponse())
      await view.get('[role="alert"] button').trigger('click')
      await flushPromises()
      expect(mocks.getMedia).toHaveBeenCalledTimes(5)
      expect(view.attributes('data-media-status')).toBe('ready')
    })

    it("keeps the failure, its alert and Retry's focus in place while it tries again", async () => {
      const quietTry = deferred<ReturnType<typeof mediaResponse>>()
      mocks.getMedia
        .mockRejectedValueOnce(notFound())
        .mockReturnValueOnce(quietTry.promise)
        .mockResolvedValueOnce(mediaResponse())
      const view = mount(ProtectedMessageMedia, {
        props: { organizationId: 'workspace-b', message: pendingImage },
        attachTo: document.body,
      })
      wrappers.push(view)
      await flushPromises()
      const alert = view.get('[role="alert"]').element
      const retryButton = view.get('[role="alert"] button').element as HTMLButtonElement
      retryButton.focus()
      expect(document.activeElement).toBe(retryButton)

      await view.setProps({ message: { ...pendingImage, status: 'sent' } })
      await wait(1_000)
      // The try is running, yet the bubble shows no loading box: the alert is
      // the same element (announced once) and Retry still has focus.
      expect(mocks.getMedia).toHaveBeenCalledTimes(2)
      expect(view.attributes('data-media-status')).toBe('error')
      expect(view.attributes('aria-busy')).toBeUndefined()
      expect(view.get('[role="alert"]').element).toBe(alert)
      expect(document.activeElement).toBe(retryButton)

      quietTry.reject(notFound())
      await flushPromises()
      expect(view.get('[role="alert"]').element).toBe(alert)
      expect(document.activeElement).toBe(retryButton)

      // The next try succeeds; focus moves from Retry to the image that replaced it.
      await wait(5_000)
      expect(view.attributes('data-media-status')).toBe('ready')
      expect(document.activeElement).toBe(view.get('button').element)
    })

    it('does not try again after a later decode error once the media has loaded', async () => {
      mocks.getMedia.mockRejectedValueOnce(notFound()).mockResolvedValueOnce(mediaResponse())
      const view = await mountLoaded({ message: pendingImage })
      await view.setProps({ message: { ...pendingImage, status: 'sent' } })
      await wait(1_000)
      expect(view.attributes('data-media-status')).toBe('ready')

      await view.get('img').trigger('error')
      await view.setProps({ message: { ...pendingImage, status: 'delivered' } })
      await wait(60_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(2)
      expect(view.attributes('data-media-status')).toBe('error')
    })

    it('notices the message leaving pending while the first load is still failing', async () => {
      const first = deferred<ReturnType<typeof mediaResponse>>()
      mocks.getMedia.mockReturnValueOnce(first.promise).mockResolvedValueOnce(mediaResponse())
      const view = mountMedia({ message: pendingImage })
      await flushPromises()
      await view.setProps({ message: { ...pendingImage, status: 'sent' } })
      first.reject(notFound())
      await flushPromises()
      expect(view.attributes('data-media-status')).toBe('error')

      await wait(1_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(2)
      expect(view.attributes('data-media-status')).toBe('ready')
    })

    it('does not wait for the timer after Retry, and does not load twice', async () => {
      mocks.getMedia.mockRejectedValueOnce(notFound()).mockResolvedValueOnce(mediaResponse())
      const view = await mountLoaded({ message: pendingImage })
      await view.setProps({ message: { ...pendingImage, status: 'sent' } })
      await view.get('[role="alert"] button').trigger('click')
      await flushPromises()
      expect(mocks.getMedia).toHaveBeenCalledTimes(2)

      await wait(30_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(2)
      expect(view.attributes('data-media-status')).toBe('ready')
    })

    it('stops a scheduled reload when the bubble unmounts', async () => {
      mocks.getMedia.mockRejectedValueOnce(notFound())
      const view = await mountLoaded({ message: pendingImage })
      await view.setProps({ message: { ...pendingImage, status: 'sent' } })
      view.unmount()
      wrappers = []
      await wait(30_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(1)
    })

    it('gives new media a fresh budget', async () => {
      mocks.getMedia.mockRejectedValue(notFound())
      const view = await mountLoaded({ message: pendingImage })
      await view.setProps({ message: { ...pendingImage, status: 'sent' } })
      await wait(21_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(4)

      const copy = { ...pendingImage, media_url: 'organizations/workspace-b/messages/images/copy.jpg' }
      await view.setProps({ message: copy })
      await flushPromises()
      expect(mocks.getMedia).toHaveBeenCalledTimes(5)
      await view.setProps({ message: { ...copy, status: 'sent' } })
      await wait(21_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(8)
    })

    it.each([
      ['an incoming image', { ...imageMessage, direction: 'incoming' as const, status: 'received' }, 'read'],
      ['an outgoing image that was already sent', { ...pendingImage, status: 'sent' }, 'delivered'],
      ['a template header image', { ...pendingImage, message_type: 'template' }, 'sent'],
    ])('leaves %s to Retry', async (_, message, nextStatus) => {
      mocks.getMedia.mockRejectedValue(notFound())
      const view = await mountLoaded({ message })
      expect(view.attributes('data-media-status')).toBe('error')
      await view.setProps({ message: { ...message, status: nextStatus } })
      await wait(60_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(1)
    })

    it('leaves documents, video and audio to Retry, since they load only when asked', async () => {
      mocks.getMedia.mockRejectedValue(notFound())
      vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined)
      const kinds = [
        { ...pendingImage, message_type: 'document', media_mime_type: 'application/pdf', media_filename: 'lab.pdf' },
        { ...pendingImage, message_type: 'video', media_mime_type: 'video/mp4' },
        { ...pendingImage, message_type: 'audio', media_mime_type: 'audio/ogg' },
      ]
      for (const message of kinds) {
        const view = mountMedia({ message })
        await view.get('button').trigger('click')
        await flushPromises()
        expect(view.attributes('data-media-status')).toBe('error')
        await view.setProps({ message: { ...message, status: 'sent' } })
        await wait(30_000)
        expect(view.attributes('data-media-status')).toBe('error')
      }
      expect(mocks.getMedia).toHaveBeenCalledTimes(3)
    })

    it('does not reload an image that failed to decode, whatever the status does', async () => {
      const view = await mountLoaded({ message: pendingImage })
      await view.get('img').trigger('error')
      await view.setProps({ message: { ...pendingImage, status: 'sent' } })
      await wait(30_000)
      expect(mocks.getMedia).toHaveBeenCalledTimes(1)
      expect(view.attributes('data-media-status')).toBe('error')
    })
  })
})
