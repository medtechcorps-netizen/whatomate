/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import MediaPreviewDialog from './MediaPreviewDialog.vue'
import { PdfPreviewError, type PdfPreviewDocument } from '@/lib/pdfPreview'

const mocks = vi.hoisted(() => ({ openPdfPreview: vi.fn() }))
vi.mock('@/lib/pdfPreview', async importOriginal => ({
  ...(await importOriginal<typeof import('@/lib/pdfPreview')>()),
  openPdfPreview: mocks.openPdfPreview,
}))
vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, values?: Record<string, unknown>) => (values ? `${key} ${JSON.stringify(values)}` : key),
  }),
}))

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

// Reports pages entering and leaving view on demand.
class FakeIntersectionObserver {
  static instances: FakeIntersectionObserver[] = []
  readonly observed = new Set<HTMLElement>()
  disconnected = false

  constructor(
    readonly callback: IntersectionObserverCallback,
    readonly options: IntersectionObserverInit = {},
  ) {
    FakeIntersectionObserver.instances.push(this)
  }

  observe(element: HTMLElement) {
    this.observed.add(element)
  }

  unobserve(element: HTMLElement) {
    this.observed.delete(element)
  }

  disconnect() {
    this.disconnected = true
    this.observed.clear()
  }

  takeRecords() {
    return []
  }

  report(pageNumbers: number[], isIntersecting: boolean) {
    const entries = pageNumbers.map(number => ({
      target: [...this.observed].find(element => element.dataset.page === String(number)),
      isIntersecting,
    }))
    this.callback(entries as unknown as IntersectionObserverEntry[], this as unknown as IntersectionObserver)
  }
}

const observer = () => FakeIntersectionObserver.instances.at(-1)!

type FakeRender = { page: number; canvas: HTMLCanvasElement; width: number; done: ReturnType<typeof deferred<void>>; cancel: ReturnType<typeof vi.fn> }

function fakePdf(pageCount: number, previewed = Math.min(pageCount, 30)) {
  const renders: FakeRender[] = []
  const document = {
    pageCount,
    pageSizes: Array.from({ length: previewed }, () => ({ width: 612, height: 792 })),
    renderPage: vi.fn((page: number, canvas: HTMLCanvasElement, width: number) => {
      const done = deferred<void>()
      const cancel = vi.fn(() => done.reject(new DOMException('closed', 'AbortError')))
      canvas.width = 612
      canvas.height = 792
      renders.push({ page, canvas, width, done, cancel })
      return { promise: done.promise, cancel }
    }),
    destroy: vi.fn(),
  } satisfies PdfPreviewDocument
  return { document, renders }
}

const pdfSource = { type: 'pdf' as const, blob: new Blob(['%PDF-1.7']) }
const imageSource = { type: 'image' as const, url: 'blob:photo', alt: 'Customer photo' }

describe('MediaPreviewDialog', () => {
  let wrapper: VueWrapper | null = null

  function mountPreview(props: Record<string, unknown> = {}) {
    wrapper = mount(MediaPreviewDialog, {
      props: {
        open: true,
        source: imageSource,
        title: 'IMG_0001.jpg',
        downloadUrl: 'blob:photo',
        downloadName: 'IMG_0001.jpg',
        ...props,
      },
      attachTo: document.body,
    })
    return wrapper
  }

  const preview = () => document.body.querySelector<HTMLElement>('[data-testid="chat-media-preview"]')
  const pages = () => [...document.body.querySelectorAll<HTMLElement>('[data-testid="chat-media-preview-page"]')]
  const pageState = (number: number) => pages()[number - 1]?.dataset.pageState
  const downloadLinks = () => [...document.body.querySelectorAll<HTMLAnchorElement>('a[download]')]

  async function settle() {
    await flushPromises()
    await flushPromises()
  }

  async function openPdf(pdf: ReturnType<typeof fakePdf>, props: Record<string, unknown> = {}) {
    mocks.openPdfPreview.mockResolvedValueOnce(pdf.document)
    mountPreview({ source: pdfSource, title: 'lab-report.pdf', downloadUrl: 'blob:opaque', downloadName: 'lab-report.pdf', ...props })
    await settle()
  }

  beforeEach(() => {
    mocks.openPdfPreview.mockReset()
    FakeIntersectionObserver.instances = []
    vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver)
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    document.body.innerHTML = ''
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('shows an image fitted to the screen in a dialog with its filename and a download of the same bytes', async () => {
    mountPreview()
    await settle()

    const dialog = document.body.querySelector('[role="dialog"]')
    expect(dialog).not.toBeNull()
    expect(preview()?.dataset.previewKind).toBe('image')
    expect(dialog?.textContent).toContain('IMG_0001.jpg')
    const image = dialog!.querySelector('img')!
    expect(image.getAttribute('src')).toBe('blob:photo')
    expect(image.getAttribute('alt')).toBe('Customer photo')
    expect(image.className).toContain('object-contain')
    expect(image.className).toContain('max-w-full')
    const [download] = downloadLinks()
    expect(download.getAttribute('href')).toBe('blob:photo')
    expect(download.getAttribute('download')).toBe('IMG_0001.jpg')
    expect(download.textContent).toContain('chat.mediaPreview.download')
    expect(dialog?.getAttribute('aria-describedby')).toBeTruthy()
    expect(mocks.openPdfPreview).not.toHaveBeenCalled()
  })

  it('zooms an image to twice its fitted size and back', async () => {
    vi.spyOn(HTMLImageElement.prototype, 'getBoundingClientRect').mockReturnValue({ width: 300 } as DOMRect)
    mountPreview()
    await settle()
    const zoom = document.body.querySelector<HTMLButtonElement>('[data-testid="chat-media-preview-zoom"]')!
    const image = () => document.body.querySelector('[role="dialog"] img') as HTMLImageElement
    expect(zoom.textContent).toContain('chat.mediaPreview.zoomIn')

    zoom.click()
    await settle()
    expect(image().style.width).toBe('600px')
    expect(image().className).toContain('max-w-none')
    expect(zoom.textContent).toContain('chat.mediaPreview.fitToScreen')

    image().click()
    await settle()
    expect(image().style.width).toBe('')
    expect(image().className).toContain('object-contain')
  })

  it('closes on Escape', async () => {
    mountPreview()
    await settle()
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await settle()
    expect(wrapper!.emitted('update:open')).toEqual([[false]])
  })

  it('offers the download instead when the image cannot be shown', async () => {
    mountPreview()
    await settle()
    document.body.querySelector('[role="dialog"] img')!.dispatchEvent(new Event('error'))
    await settle()

    const alert = document.body.querySelector('[role="alert"]')!
    expect(alert.textContent).toContain('chat.mediaPreview.previewFailed')
    expect(alert.querySelector('a[download]')?.getAttribute('href')).toBe('blob:photo')
    expect(preview()?.dataset.previewStatus).toBe('error')
    expect(document.body.querySelector('[data-testid="chat-media-preview-zoom"]')).toBeNull()
  })

  it('opens a PDF only while the preview is open', async () => {
    const pdf = fakePdf(1)
    mocks.openPdfPreview.mockResolvedValueOnce(pdf.document)
    mountPreview({ open: false, source: pdfSource })
    await settle()
    expect(mocks.openPdfPreview).not.toHaveBeenCalled()

    await wrapper!.setProps({ open: true })
    expect(mocks.openPdfPreview).toHaveBeenCalledWith(pdfSource.blob, { signal: expect.any(AbortSignal) })
    expect(preview()?.dataset.previewStatus).toBe('loading')
    expect(document.body.textContent).toContain('chat.mediaPreview.loading')
    await settle()
    expect(preview()?.dataset.previewStatus).toBe('ready')
  })

  it('lays out every previewed page and draws them as they come into view, one at a time, top to bottom', async () => {
    const pdf = fakePdf(3)
    await openPdf(pdf)

    expect(preview()?.dataset.previewKind).toBe('pdf')
    expect(pages()).toHaveLength(3)
    expect(pages()[0].style.aspectRatio).toBe('612 / 792')
    expect(pages()[0].querySelector('canvas')?.getAttribute('aria-label')).toBe(
      'chat.mediaPreview.pageLabel {"page":1,"total":3}',
    )
    // The scrolling body is the observer's root, so off-screen pages wait.
    expect(observer().options.root).toBe(pages()[0].closest('.overflow-auto'))
    expect(observer().observed.size).toBe(3)
    expect(pdf.document.renderPage).not.toHaveBeenCalled()

    observer().report([2, 1], true)
    await settle()
    expect(pdf.document.renderPage).toHaveBeenCalledTimes(1)
    expect(pdf.renders[0].page).toBe(1)
    expect(pdf.renders[0].canvas).toBe(pages()[0].querySelector('canvas'))
    expect(pdf.renders[0].width).toBeGreaterThan(0)
    expect(pageState(1)).toBe('rendering')
    expect(pageState(2)).toBe('queued')

    pdf.renders[0].done.resolve()
    await settle()
    expect(pageState(1)).toBe('rendered')
    expect(pdf.renders[1].page).toBe(2)
    pdf.renders[1].done.resolve()
    await settle()
    expect(pdf.document.renderPage).toHaveBeenCalledTimes(2)
    expect(pageState(3)).toBe('idle')
  })

  it('skips a waiting page that scrolls away before its turn', async () => {
    const pdf = fakePdf(3)
    await openPdf(pdf)
    observer().report([1, 2], true)
    await settle()
    observer().report([2], false)
    pdf.renders[0].done.resolve()
    await settle()
    expect(pdf.document.renderPage).toHaveBeenCalledTimes(1)
    expect(pageState(2)).toBe('idle')
  })

  it('gives back the canvases of pages scrolled far away and redraws them on return', async () => {
    const pdf = fakePdf(10)
    await openPdf(pdf)
    const drawAll = async () => {
      while (pdf.renders.some(render => pageState(render.page) === 'rendering')) {
        pdf.renders.at(-1)!.done.resolve()
        await settle()
      }
    }

    observer().report([1, 2, 3, 4, 5, 6, 7], true)
    await settle()
    await drawAll()
    // All seven are near the view, so none is released.
    expect([1, 2, 3, 4, 5, 6, 7].map(pageState)).toEqual(Array(7).fill('rendered'))

    observer().report([1, 2], false)
    observer().report([8], true)
    await settle()
    await drawAll()
    expect(pageState(8)).toBe('rendered')
    expect(pageState(1)).toBe('idle')
    expect(pageState(2)).toBe('idle')
    expect(pages()[0].querySelector('canvas')!.width).toBe(0)
    expect(pageState(3)).toBe('rendered')

    observer().report([1], true)
    await settle()
    expect(pdf.renders.at(-1)!.page).toBe(1)
  })

  it('zooms PDF pages to twice their fitted width and redraws the pages in view', async () => {
    vi.spyOn(HTMLOListElement.prototype, 'getBoundingClientRect').mockReturnValue({ width: 800 } as DOMRect)
    const pdf = fakePdf(3)
    await openPdf(pdf)
    const zoom = () => document.body.querySelector<HTMLButtonElement>('[data-testid="chat-media-preview-zoom"]')!
    const list = () => document.body.querySelector<HTMLOListElement>('ol')!

    // Page 3 is drawn, then scrolled away; pages 1 and 2 stay in view.
    observer().report([3], true)
    await settle()
    pdf.renders[0].done.resolve()
    await settle()
    observer().report([3], false)
    observer().report([1, 2], true)
    await settle()
    pdf.renders[1].done.resolve()
    await settle()
    expect(pdf.renders.map(render => render.page)).toEqual([3, 1, 2])
    expect(zoom().textContent).toContain('chat.mediaPreview.zoomIn')

    zoom().click()
    await settle()
    expect(list().style.width).toBe('1600px')
    expect(list().className).toContain('max-w-none')
    expect(zoom().textContent).toContain('chat.mediaPreview.fitToScreen')
    // Page 2 was still drawing at the old width: it is stopped and drawn again.
    expect(pdf.renders[2].cancel).toHaveBeenCalled()
    expect(pdf.renders[3].page).toBe(1)
    expect(pageState(2)).toBe('queued')
    // Page 3 is out of view: its old drawing is given back, not redrawn.
    expect(pageState(3)).toBe('idle')
    expect(pages()[2].querySelector('canvas')!.width).toBe(0)

    pdf.renders[3].done.resolve()
    await settle()
    expect(pdf.renders[4].page).toBe(2)
    pdf.renders[4].done.resolve()
    await settle()
    expect(pdf.renders).toHaveLength(5)

    zoom().click()
    await settle()
    expect(list().style.width).toBe('')
    expect(list().className).toContain('max-w-4xl')
    expect(pdf.renders.slice(5).map(render => render.page)).toEqual([1])
  })

  it('offers zoom only once the PDF is ready', async () => {
    const parsing = deferred<PdfPreviewDocument>()
    mocks.openPdfPreview.mockReturnValueOnce(parsing.promise)
    mountPreview({ source: pdfSource })
    await settle()
    expect(document.body.querySelector('[data-testid="chat-media-preview-zoom"]')).toBeNull()
    parsing.resolve(fakePdf(1).document)
    await settle()
    expect(document.body.querySelector('[data-testid="chat-media-preview-zoom"]')).not.toBeNull()
  })

  it('previews the first pages of a long PDF and offers the download for the rest', async () => {
    await openPdf(fakePdf(45, 30))
    expect(pages()).toHaveLength(30)
    const limit = document.body.querySelector('[data-testid="chat-media-preview-page-limit"]')!
    expect(limit.textContent).toContain('chat.mediaPreview.pageLimit {"shown":30,"total":45}')
    expect(limit.querySelector('a[download]')?.getAttribute('href')).toBe('blob:opaque')
    expect(limit.querySelector('a[download]')?.getAttribute('download')).toBe('lab-report.pdf')
  })

  it('does not mention a page limit when every page is previewed', async () => {
    await openPdf(fakePdf(30))
    expect(pages()).toHaveLength(30)
    expect(document.body.querySelector('[data-testid="chat-media-preview-page-limit"]')).toBeNull()
  })

  it.each([
    [new PdfPreviewError('password'), 'chat.mediaPreview.passwordProtected'],
    [new PdfPreviewError('invalid'), 'chat.mediaPreview.previewFailed'],
    [new PdfPreviewError('unavailable'), 'chat.mediaPreview.previewFailed'],
    [new Error('worker crashed'), 'chat.mediaPreview.previewFailed'],
  ])('falls back to the download when the PDF cannot be previewed (%s)', async (error, message) => {
    mocks.openPdfPreview.mockRejectedValueOnce(error)
    mountPreview({ source: pdfSource, downloadUrl: 'blob:opaque', downloadName: 'lab-report.pdf' })
    await settle()

    expect(preview()?.dataset.previewStatus).toBe('error')
    const alert = document.body.querySelector('[role="alert"]')!
    expect(alert.textContent).toContain(message)
    const download = alert.querySelector('a[download]')!
    expect(download.getAttribute('href')).toBe('blob:opaque')
    expect(download.getAttribute('download')).toBe('lab-report.pdf')
    expect(pages()).toHaveLength(0)
    expect(document.body.querySelector('[data-testid="chat-media-preview-zoom"]')).toBeNull()
  })

  it('marks a page that fails to draw and goes on with the next', async () => {
    const pdf = fakePdf(2)
    await openPdf(pdf)
    observer().report([1, 2], true)
    await settle()
    pdf.renders[0].done.reject(new Error('broken content stream'))
    await settle()
    expect(pageState(1)).toBe('failed')
    expect(pages()[0].textContent).toContain('chat.mediaPreview.pageFailed')
    expect(pdf.renders[1].page).toBe(2)
  })

  it('stops drawing and releases the PDF when the preview closes', async () => {
    const pdf = fakePdf(2)
    await openPdf(pdf)
    observer().report([1], true)
    await settle()

    await wrapper!.setProps({ open: false })
    await settle()
    expect(pdf.renders[0].cancel).toHaveBeenCalled()
    expect(pdf.document.destroy).toHaveBeenCalledTimes(1)
    expect(observer().disconnected).toBe(true)

    // Opening again parses the file afresh.
    const reopened = fakePdf(2)
    mocks.openPdfPreview.mockResolvedValueOnce(reopened.document)
    await wrapper!.setProps({ open: true })
    await settle()
    expect(mocks.openPdfPreview).toHaveBeenCalledTimes(2)
    expect(pages()).toHaveLength(2)
  })

  it('releases a PDF that finishes parsing after the preview closed', async () => {
    const pdf = fakePdf(1)
    const parsing = deferred<PdfPreviewDocument>()
    mocks.openPdfPreview.mockReturnValueOnce(parsing.promise)
    mountPreview({ source: pdfSource })
    await settle()
    const { signal } = mocks.openPdfPreview.mock.calls[0][1] as { signal: AbortSignal }

    await wrapper!.setProps({ open: false })
    expect(signal.aborted).toBe(true)
    parsing.resolve(pdf.document)
    await settle()
    expect(pdf.document.destroy).toHaveBeenCalledTimes(1)
    expect(FakeIntersectionObserver.instances).toHaveLength(0)
  })

  it('releases the PDF when the bubble unmounts', async () => {
    const pdf = fakePdf(1)
    await openPdf(pdf)
    wrapper!.unmount()
    wrapper = null
    expect(pdf.document.destroy).toHaveBeenCalledTimes(1)
  })
})
