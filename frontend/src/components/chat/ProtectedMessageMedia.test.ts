/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ProtectedMessageMedia from './ProtectedMessageMedia.vue'

const mocks = vi.hoisted(() => ({ getMedia: vi.fn() }))
vi.mock('@/services/api', () => ({ messagesService: { getMedia: mocks.getMedia } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(res => { resolve = res })
  return { promise, resolve }
}

const imageMessage = {
  id: 'message-1',
  message_type: 'image',
  media_url: 'organizations/workspace-b/messages/images/photo.jpg',
  media_mime_type: 'image/jpeg',
  content: { body: 'Customer photo' },
}
const mediaResponse = (type = 'image/jpeg') => ({ data: new Blob(['media bytes'], { type }) })

describe('ProtectedMessageMedia', () => {
  let wrapper: VueWrapper | null = null
  let createURL: ReturnType<typeof vi.fn<(value: Blob | MediaSource) => string>>
  let revokeURL: ReturnType<typeof vi.fn<(url: string) => void>>

  function mountMedia(props: Partial<InstanceType<typeof ProtectedMessageMedia>['$props']> = {}) {
    wrapper = mount(ProtectedMessageMedia, {
      props: { organizationId: 'workspace-b', message: imageMessage, ...props },
    })
    return wrapper
  }

  beforeEach(() => {
    mocks.getMedia.mockReset().mockResolvedValue(mediaResponse())
    createURL = vi.fn<(value: Blob | MediaSource) => string>().mockReturnValue('blob:protected-photo')
    revokeURL = vi.fn<(url: string) => void>()
    vi.stubGlobal('URL', class extends URL {
      static createObjectURL = createURL
      static revokeObjectURL = revokeURL
    })
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    vi.unstubAllGlobals()
  })

  it('fetches media pinned to the transcript workspace and renders the protected bytes', async () => {
    const view = mountMedia()
    expect(view.get('[role="status"]').text()).toBe('chat.mediaLoading')
    await flushPromises()

    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
    expect(mocks.getMedia).toHaveBeenCalledWith('message-1', 'workspace-b', expect.any(AbortSignal))
    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
    expect(view.get('img').attributes('alt')).toBe('Customer photo')
    // No native element is pointed at the header-less API URL or the storage key.
    expect(view.html()).not.toContain('/api/media/')
    expect(view.html()).not.toContain(imageMessage.media_url)
  })

  it('opens a loaded raster image in a new tab from its object URL', async () => {
    const open = vi.fn()
    vi.stubGlobal('open', open)
    const view = mountMedia()
    await flushPromises()

    await view.get('button').trigger('click')
    expect(open).toHaveBeenCalledWith('blob:protected-photo', '_blank')
  })

  it('never opens a scriptable image type as a same-origin document', async () => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse('image/svg+xml'))
    const open = vi.fn()
    vi.stubGlobal('open', open)
    const view = mountMedia()
    await flushPromises()

    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
    expect(view.find('button').exists()).toBe(false)
    await view.get('img').trigger('click')
    expect(open).not.toHaveBeenCalled()
  })

  it('fails closed instead of falling back to the login workspace when no workspace is known', async () => {
    const view = mountMedia({ organizationId: '  ' })
    await flushPromises()
    expect(mocks.getMedia).not.toHaveBeenCalled()
    expect(view.text()).toContain('chat.mediaLoadFailed')
    expect(view.find('img').exists()).toBe(false)
  })

  it('aborts and ignores the previous workspace response after the workspace changes', async () => {
    const previous = deferred<ReturnType<typeof mediaResponse>>()
    const current = deferred<ReturnType<typeof mediaResponse>>()
    mocks.getMedia.mockReturnValueOnce(previous.promise).mockReturnValueOnce(current.promise)
    const view = mountMedia()
    const previousSignal = mocks.getMedia.mock.calls[0][2] as AbortSignal

    await view.setProps({ organizationId: 'workspace-c' })
    expect(previousSignal.aborted).toBe(true)
    expect(mocks.getMedia.mock.calls[1][1]).toBe('workspace-c')

    current.resolve(mediaResponse())
    await flushPromises()
    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
    previous.resolve(mediaResponse())
    await flushPromises()
    expect(createURL).toHaveBeenCalledTimes(1)
  })

  it('revokes the loaded object URL when the workspace changes and when unmounted', async () => {
    const view = mountMedia()
    await flushPromises()
    createURL.mockReturnValueOnce('blob:second-workspace')

    await view.setProps({ organizationId: 'workspace-c' })
    expect(revokeURL).toHaveBeenCalledWith('blob:protected-photo')
    await flushPromises()
    expect(view.get('img').attributes('src')).toBe('blob:second-workspace')

    view.unmount()
    wrapper = null
    expect(revokeURL).toHaveBeenCalledWith('blob:second-workspace')
  })

  it('does not reload when a transcript refresh replaces the message object unchanged', async () => {
    const view = mountMedia()
    await flushPromises()
    await view.setProps({ message: { ...imageMessage } })
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
    expect(revokeURL).not.toHaveBeenCalled()
  })

  it('does not create an object URL for a response that arrives after unmount', async () => {
    const pending = deferred<ReturnType<typeof mediaResponse>>()
    mocks.getMedia.mockReturnValueOnce(pending.promise)
    const view = mountMedia()
    const signal = mocks.getMedia.mock.calls[0][2] as AbortSignal

    view.unmount()
    wrapper = null
    expect(signal.aborted).toBe(true)
    pending.resolve(mediaResponse())
    await flushPromises()
    expect(createURL).not.toHaveBeenCalled()
  })

  it('shows a retryable failure when the media request fails', async () => {
    mocks.getMedia.mockRejectedValueOnce(new Error('404')).mockResolvedValueOnce(mediaResponse())
    const view = mountMedia()
    await flushPromises()
    expect(view.text()).toContain('chat.mediaLoadFailed')
    expect(view.find('img').exists()).toBe(false)

    await view.get('button').trigger('click')
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(2)
    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
  })

  it('treats an empty body as a failure', async () => {
    mocks.getMedia.mockResolvedValueOnce({ data: new Blob([], { type: 'image/jpeg' }) })
    const view = mountMedia()
    await flushPromises()
    expect(view.text()).toContain('chat.mediaLoadFailed')
    expect(createURL).not.toHaveBeenCalled()
  })

  it('offers a retry for an image that fails to decode instead of hiding it', async () => {
    const view = mountMedia()
    await flushPromises()
    await view.get('img').trigger('error')
    expect(view.text()).toContain('chat.mediaLoadFailed')
    expect(view.find('img').exists()).toBe(false)
    expect(revokeURL).toHaveBeenCalledWith('blob:protected-photo')
  })

  it('discards media when its stored source is removed', async () => {
    const view = mountMedia()
    await flushPromises()
    await view.setProps({ message: { ...imageMessage, media_url: '' } })
    expect(revokeURL).toHaveBeenCalledWith('blob:protected-photo')
    expect(view.find('img').exists()).toBe(false)
    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
  })

  it('loads a document only when it is opened, then downloads it from the protected bytes', async () => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse('application/pdf'))
    const clicked = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined)
    const view = mountMedia({
      message: { ...imageMessage, message_type: 'document', media_mime_type: 'application/pdf', media_filename: 'lab-report.pdf' },
    })
    expect(mocks.getMedia).not.toHaveBeenCalled()
    expect(view.get('button').text()).toContain('lab-report.pdf')

    await view.get('button').trigger('click')
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledWith('message-1', 'workspace-b', expect.any(AbortSignal))
    const link = view.get('a[download]')
    expect(link.attributes('href')).toBe('blob:protected-photo')
    expect(link.attributes('download')).toBe('lab-report.pdf')
    expect(clicked).toHaveBeenCalledTimes(1)
    clicked.mockRestore()
  })

  it.each([
    ['sticker', 'image/webp', 'img'],
    ['video', 'video/mp4', 'video'],
    ['audio', 'audio/ogg', 'audio'],
    ['template', 'image/jpeg', 'img'],
    ['template', 'video/mp4', 'video'],
  ])('renders %s (%s) media from the protected bytes', async (messageType, mimeType, selector) => {
    mocks.getMedia.mockResolvedValueOnce(mediaResponse(mimeType))
    const view = mountMedia({ message: { ...imageMessage, message_type: messageType, media_mime_type: mimeType } })
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledWith('message-1', 'workspace-b', expect.any(AbortSignal))
    expect(view.get(selector).attributes('src')).toBe('blob:protected-photo')
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
})
