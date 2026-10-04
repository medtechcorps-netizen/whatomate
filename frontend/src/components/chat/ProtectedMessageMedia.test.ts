/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ProtectedMessageMedia from './ProtectedMessageMedia.vue'
import { CHAT_MEDIA_VISIBILITY_DWELL_MS, chatMediaLoadLimiter } from '@/lib/chatMedia'

const mocks = vi.hoisted(() => ({ getMedia: vi.fn() }))
vi.mock('@/services/api', () => ({ messagesService: { getMedia: mocks.getMedia } }))
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

// Lets each test decide when a bubble nears the chat viewport.
class FakeIntersectionObserver {
  static instances: FakeIntersectionObserver[] = []
  readonly targets = new Set<Element>()

  constructor(
    readonly callback: IntersectionObserverCallback,
    readonly options: IntersectionObserverInit = {},
  ) {
    FakeIntersectionObserver.instances.push(this)
  }

  observe(target: Element) {
    this.targets.add(target)
  }

  unobserve(target: Element) {
    this.targets.delete(target)
  }

  disconnect() {
    this.targets.clear()
  }

  takeRecords() {
    return []
  }
}

function setVisibility(isIntersecting: boolean, target?: Element) {
  for (const observer of [...FakeIntersectionObserver.instances]) {
    for (const element of [...observer.targets]) {
      if (target && element !== target) continue
      observer.callback(
        [{ isIntersecting, target: element } as unknown as IntersectionObserverEntry],
        observer as unknown as IntersectionObserver,
      )
    }
  }
}

// Brings bubbles near the view and keeps them there past the dwell time.
async function show(target?: Element) {
  setVisibility(true, target)
  await vi.advanceTimersByTimeAsync(CHAT_MEDIA_VISIBILITY_DWELL_MS)
  await flushPromises()
}

const imageMessage = {
  id: 'message-1',
  message_type: 'image',
  media_url: 'organizations/workspace-b/messages/images/photo.jpg',
  media_mime_type: 'image/jpeg',
  content: { body: 'Customer photo' },
}
const mediaResponse = (type = 'image/jpeg', body = 'media bytes') => ({ data: new Blob([body], { type }) })
const requestOptions = (call = 0) => mocks.getMedia.mock.calls[call][2] as MediaOptions

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

  async function mountVisible(props: Partial<InstanceType<typeof ProtectedMessageMedia>['$props']> = {}) {
    const view = mountMedia(props)
    await show(view.element)
    return view
  }

  function createdBlob(call = 0) {
    return createURL.mock.calls[call][0] as Blob
  }

  beforeEach(() => {
    mocks.getMedia.mockReset().mockResolvedValue(mediaResponse())
    createURL = vi.fn<(value: Blob | MediaSource) => string>().mockReturnValue('blob:protected-photo')
    revokeURL = vi.fn<(url: string) => void>()
    vi.stubGlobal('URL', class extends URL {
      static createObjectURL = createURL
      static revokeObjectURL = revokeURL
    })
    FakeIntersectionObserver.instances = []
    vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
  })

  afterEach(() => {
    for (const wrapper of wrappers) wrapper.unmount()
    wrappers = []
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
    vi.useRealTimers()
    // Every download slot is returned, whatever state a bubble was left in.
    expect(chatMediaLoadLimiter.active).toBe(0)
    expect(chatMediaLoadLimiter.waiting).toBe(0)
  })

  it('waits until an image nears the viewport, then renders bytes pinned to the transcript workspace', async () => {
    const view = mountMedia()
    await flushPromises()
    expect(mocks.getMedia).not.toHaveBeenCalled()
    expect(view.attributes('aria-busy')).toBe('true')
    expect(view.text()).toBe('chat.mediaLoading')
    expect(FakeIntersectionObserver.instances.at(-1)?.options.rootMargin).toBe('600px 0px')

    setVisibility(true, view.element)
    await vi.advanceTimersByTimeAsync(CHAT_MEDIA_VISIBILITY_DWELL_MS - 1)
    expect(mocks.getMedia).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    await flushPromises()

    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
    expect(mocks.getMedia).toHaveBeenCalledWith('message-1', 'workspace-b', expect.objectContaining({
      signal: expect.any(AbortSignal),
    }))
    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
    expect(view.get('img').attributes('alt')).toBe('Customer photo')
    expect(view.attributes('aria-busy')).toBeUndefined()
    // No native element is pointed at the header-less API URL or the storage key.
    expect(view.html()).not.toContain('/api/media/')
    expect(view.html()).not.toContain(imageMessage.media_url)
  })

  it('measures nearness against the chat scroll viewport, not the window', async () => {
    const viewport = document.createElement('div')
    viewport.setAttribute('data-reka-scroll-area-viewport', '')
    document.body.appendChild(viewport)
    const wrapper = mount(ProtectedMessageMedia, {
      props: { organizationId: 'workspace-b', message: imageMessage },
      attachTo: viewport,
    })
    wrappers.push(wrapper)

    expect(FakeIntersectionObserver.instances.at(-1)?.options.root).toBe(viewport)
    wrapper.unmount()
    wrappers = []
    viewport.remove()
  })

  it('does not fetch an image that only passes by on the way to the newest message', async () => {
    const view = mountMedia()
    setVisibility(true, view.element)
    await vi.advanceTimersByTimeAsync(CHAT_MEDIA_VISIBILITY_DWELL_MS / 2)
    setVisibility(false, view.element)
    await vi.advanceTimersByTimeAsync(CHAT_MEDIA_VISIBILITY_DWELL_MS * 4)
    await flushPromises()
    expect(mocks.getMedia).not.toHaveBeenCalled()
    expect(view.attributes('data-media-status')).toBe('idle')
  })

  it('waits for the observer to catch up when the transcript scrolled after it reported', async () => {
    const view = mountMedia()
    const rect = (top: number) => ({ top, bottom: top + 300, left: 0, right: 280, width: 280, height: 300, x: 0, y: top })
    const position = vi.spyOn(view.element as HTMLElement, 'getBoundingClientRect')
      .mockReturnValue(rect(5000) as DOMRect)

    await show(view.element)
    await vi.advanceTimersByTimeAsync(CHAT_MEDIA_VISIBILITY_DWELL_MS * 2)
    expect(mocks.getMedia).not.toHaveBeenCalled()

    position.mockReturnValue(rect(100) as DOMRect)
    await vi.advanceTimersByTimeAsync(CHAT_MEDIA_VISIBILITY_DWELL_MS)
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
  })

  it('does not announce each loading placeholder to screen readers', () => {
    const view = mountMedia()
    expect(view.find('[role="status"]').exists()).toBe(false)
    expect(view.find('[role="alert"]').exists()).toBe(false)
  })

  it('loads right away where IntersectionObserver is unavailable', async () => {
    vi.stubGlobal('IntersectionObserver', undefined)
    const view = mountMedia()
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
  })

  it('downloads at most four media items at once and starts the next when one finishes', async () => {
    const responses = Array.from({ length: 6 }, () => deferred<ReturnType<typeof mediaResponse>>())
    responses.forEach(response => mocks.getMedia.mockReturnValueOnce(response.promise))
    for (let index = 0; index < 6; index++) {
      mountMedia({ message: { ...imageMessage, id: `message-${index}` } })
    }

    await show()
    expect(mocks.getMedia).toHaveBeenCalledTimes(4)
    expect(chatMediaLoadLimiter.waiting).toBe(2)

    responses[0].resolve(mediaResponse())
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(5)

    responses[1].reject(new Error('network'))
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(6)
  })

  it('gives up its place in the queue when the image scrolls away before its turn', async () => {
    const responses = Array.from({ length: 4 }, () => deferred<ReturnType<typeof mediaResponse>>())
    responses.forEach(response => mocks.getMedia.mockReturnValueOnce(response.promise))
    const views = Array.from({ length: 5 }, (_, index) =>
      mountMedia({ message: { ...imageMessage, id: `message-${index}` } }),
    )
    await show()
    expect(mocks.getMedia).toHaveBeenCalledTimes(4)
    expect(views[4].attributes('data-media-status')).toBe('loading')

    setVisibility(false, views[4].element)
    await flushPromises()
    expect(views[4].attributes('data-media-status')).toBe('idle')
    expect(chatMediaLoadLimiter.waiting).toBe(0)
    // A download already in flight is not abandoned.
    setVisibility(false, views[0].element)
    await flushPromises()
    expect(views[0].attributes('data-media-status')).toBe('loading')

    responses[0].resolve(mediaResponse())
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(4)

    await show(views[4].element)
    expect(mocks.getMedia).toHaveBeenCalledTimes(5)
    expect(mocks.getMedia.mock.calls[4][0]).toBe('message-4')
  })

  it('opens a loaded raster image in a new tab from its object URL', async () => {
    const open = vi.fn()
    vi.stubGlobal('open', open)
    const view = await mountVisible()

    expect(createdBlob().type).toBe('image/jpeg')
    await view.get('button').trigger('click')
    expect(open).toHaveBeenCalledWith('blob:protected-photo', '_blank')
  })

  it('never gives a scriptable image a renderable type or a new-tab preview', async () => {
    mocks.getMedia.mockResolvedValueOnce(
      mediaResponse('image/svg+xml', '<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>'),
    )
    const open = vi.fn()
    vi.stubGlobal('open', open)
    const view = await mountVisible()

    // "Open image in new tab" on this object URL must download, not render.
    expect(createdBlob().type).toBe('application/octet-stream')
    expect(view.find('button').exists()).toBe(false)
    await view.get('img').trigger('click')
    expect(open).not.toHaveBeenCalled()
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

  it('fails closed instead of falling back to the login workspace when no workspace is known', async () => {
    const view = await mountVisible({ organizationId: '  ' })
    expect(mocks.getMedia).not.toHaveBeenCalled()
    expect(view.get('[role="alert"]').text()).toContain('chat.mediaLoadFailed')
    expect(view.find('img').exists()).toBe(false)
  })

  it('aborts and ignores the previous workspace response after the workspace changes', async () => {
    const previous = deferred<ReturnType<typeof mediaResponse>>()
    const current = deferred<ReturnType<typeof mediaResponse>>()
    mocks.getMedia.mockReturnValueOnce(previous.promise).mockReturnValueOnce(current.promise)
    const view = await mountVisible()
    const previousSignal = requestOptions(0).signal

    await view.setProps({ organizationId: 'workspace-c' })
    expect(previousSignal.aborted).toBe(true)
    await show(view.element)
    expect(mocks.getMedia.mock.calls[1][1]).toBe('workspace-c')

    current.resolve(mediaResponse())
    await flushPromises()
    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
    previous.resolve(mediaResponse())
    await flushPromises()
    expect(createURL).toHaveBeenCalledTimes(1)
  })

  it('revokes the loaded object URL when the workspace changes and when unmounted', async () => {
    const view = await mountVisible()
    createURL.mockReturnValueOnce('blob:second-workspace')

    await view.setProps({ organizationId: 'workspace-c' })
    expect(revokeURL).toHaveBeenCalledWith('blob:protected-photo')
    await show(view.element)
    expect(view.get('img').attributes('src')).toBe('blob:second-workspace')

    view.unmount()
    wrappers = []
    expect(revokeURL).toHaveBeenCalledWith('blob:second-workspace')
  })

  it('does not reload when a transcript refresh replaces the message object unchanged', async () => {
    const view = await mountVisible()
    await view.setProps({ message: { ...imageMessage } })
    await show(view.element)
    expect(mocks.getMedia).toHaveBeenCalledTimes(1)
    expect(revokeURL).not.toHaveBeenCalled()
  })

  it('does not create an object URL for a response that arrives after unmount', async () => {
    const pending = deferred<ReturnType<typeof mediaResponse>>()
    mocks.getMedia.mockReturnValueOnce(pending.promise)
    const view = await mountVisible()
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
    const view = await mountVisible()
    expect(view.get('[role="alert"]').text()).toContain('chat.mediaLoadFailed')
    expect(view.find('img').exists()).toBe(false)

    await view.get('button').trigger('click')
    await flushPromises()
    expect(mocks.getMedia).toHaveBeenCalledTimes(2)
    expect(view.get('img').attributes('src')).toBe('blob:protected-photo')
  })

  it('treats an empty body as a failure', async () => {
    mocks.getMedia.mockResolvedValueOnce({ data: new Blob([], { type: 'image/jpeg' }) })
    const view = await mountVisible()
    expect(view.text()).toContain('chat.mediaLoadFailed')
    expect(createURL).not.toHaveBeenCalled()
  })

  it('offers a retry for an image that fails to decode instead of hiding it', async () => {
    const view = await mountVisible()
    await view.get('img').trigger('error')
    expect(view.text()).toContain('chat.mediaLoadFailed')
    expect(view.find('img').exists()).toBe(false)
    expect(revokeURL).toHaveBeenCalledWith('blob:protected-photo')
  })

  it('discards media when its stored source is removed', async () => {
    const view = await mountVisible()
    await view.setProps({ message: { ...imageMessage, media_url: '' } })
    expect(revokeURL).toHaveBeenCalledWith('blob:protected-photo')
    await show(view.element)
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
    await show(view.element)
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
    await show(view.element)
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
    const view = await mountVisible({ message: { ...imageMessage, message_type: messageType, media_mime_type: mimeType } })
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
    await show(view.element)
    expect(mocks.getMedia).not.toHaveBeenCalled()
    expect(view.get('button').text()).toContain('brochure.pdf')
  })
})
