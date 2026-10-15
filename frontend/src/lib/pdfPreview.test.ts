import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  documentPreviewType,
  isPdfPreviewAbort,
  openPdfPreview,
  PDF_PREVIEW_PAGE_LIMIT,
  PdfPreviewError,
} from './pdfPreview'

const mocks = vi.hoisted(() => ({
  imported: 0,
  getDocument: vi.fn(),
  workerOptions: { workerSrc: '' },
}))

vi.mock('pdfjs-dist/legacy/build/pdf.mjs', () => {
  mocks.imported++
  return {
    getDocument: mocks.getDocument,
    GlobalWorkerOptions: mocks.workerOptions,
    VerbosityLevel: { ERRORS: 0 },
  }
})

const encoder = new TextEncoder()
const pdfBytes = (body = '%PDF-1.7\n1 0 obj\n') => new Blob([encoder.encode(body)], { type: 'application/octet-stream' })

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

function namedError(name: string) {
  const error = new Error(name)
  error.name = name
  return error
}

function fakePage(width = 612, height = 792) {
  const renderTask = { promise: Promise.resolve(), cancel: vi.fn() }
  return {
    getViewport: vi.fn(({ scale }: { scale: number }) => ({ width: width * scale, height: height * scale })),
    render: vi.fn(() => renderTask),
    cleanup: vi.fn(),
    renderTask,
  }
}

function fakeLoadingTask(document: Promise<unknown>) {
  return { promise: document, destroy: vi.fn().mockResolvedValue(undefined) }
}

function fakeDocument(numPages: number, page = fakePage()) {
  return { numPages, getPage: vi.fn().mockResolvedValue(page), page }
}

const canvas = () => ({ width: 0, height: 0 }) as HTMLCanvasElement

describe('documentPreviewType', () => {
  it.each([
    ['a PDF', [0x25, 0x50, 0x44, 0x46, 0x2d, 0x31, 0x2e, 0x37], 'application/pdf'],
    ['a PNG', [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00], 'image/png'],
    ['a JPEG', [0xff, 0xd8, 0xff, 0xe0, 0x00], 'image/jpeg'],
  ])('recognises %s by its leading bytes', async (_, bytes, expected) => {
    expect(await documentPreviewType(new Blob([new Uint8Array(bytes)]))).toBe(expected)
  })

  it('trusts the bytes, never the declared type', async () => {
    const html = '<!doctype html><script>alert(1)</script>'
    expect(await documentPreviewType(new Blob([html], { type: 'application/pdf' }))).toBeNull()
    expect(await documentPreviewType(new Blob([html], { type: 'image/png' }))).toBeNull()
    expect(await documentPreviewType(pdfBytes())).toBe('application/pdf')
    expect(await documentPreviewType(new Blob(['%PDF-1.4'], { type: 'text/html' }))).toBe('application/pdf')
  })

  it.each([
    ['an empty file', []],
    ['a truncated PNG signature', [0x89, 0x50, 0x4e, 0x47]],
    ['a PDF marker that is not at the start', [0x20, 0x25, 0x50, 0x44, 0x46, 0x2d]],
    ['a GIF', [0x47, 0x49, 0x46, 0x38, 0x39, 0x61]],
    ['an SVG', [...encoder.encode('<svg xmlns')]],
  ])('does not preview %s', async (_, bytes) => {
    expect(await documentPreviewType(new Blob([new Uint8Array(bytes)]))).toBeNull()
  })
})

describe('openPdfPreview', () => {
  beforeEach(() => {
    mocks.getDocument.mockReset()
    mocks.workerOptions.workerSrc = ''
  })

  it('refuses bytes that do not start with %PDF- before pdf.js loads', async () => {
    const importedBefore = mocks.imported
    const error = await openPdfPreview(new Blob(['<html>%PDF-1.7'])).catch(caught => caught)
    expect(error).toBeInstanceOf(PdfPreviewError)
    expect(error.reason).toBe('invalid')
    expect(mocks.getDocument).not.toHaveBeenCalled()
    expect(mocks.imported).toBe(importedBefore)
  })

  it('loads pdf.js on demand with a same-origin worker, no eval and no XFA', async () => {
    const document = fakeDocument(2)
    mocks.getDocument.mockReturnValueOnce(fakeLoadingTask(Promise.resolve(document)))
    const preview = await openPdfPreview(pdfBytes('%PDF-1.7 lab report'))

    expect(mocks.imported).toBe(1)
    // The bundled worker file, served from the app's own origin.
    expect(mocks.workerOptions.workerSrc).toMatch(/pdf\.worker\.min\.mjs$/)
    expect(mocks.workerOptions.workerSrc).not.toMatch(/^(https?:)?\/\/(?!localhost)/)
    expect(mocks.getDocument).toHaveBeenCalledTimes(1)
    const params = mocks.getDocument.mock.calls[0][0]
    expect(params).toMatchObject({ isEvalSupported: false, enableXfa: false })
    expect(params).not.toHaveProperty('url')
    expect(params).not.toHaveProperty('onPassword')
    expect(new TextDecoder().decode(params.data)).toBe('%PDF-1.7 lab report')
    expect(preview.pageCount).toBe(2)
    expect(preview.pageSizes).toEqual([{ width: 612, height: 792 }, { width: 612, height: 792 }])
  })

  it(`previews only the first ${PDF_PREVIEW_PAGE_LIMIT} pages of a long file`, async () => {
    const document = fakeDocument(45)
    mocks.getDocument.mockReturnValueOnce(fakeLoadingTask(Promise.resolve(document)))
    const preview = await openPdfPreview(pdfBytes())

    expect(preview.pageCount).toBe(45)
    expect(preview.pageSizes).toHaveLength(PDF_PREVIEW_PAGE_LIMIT)
    expect(document.getPage).toHaveBeenCalledTimes(PDF_PREVIEW_PAGE_LIMIT)
    expect(document.getPage).not.toHaveBeenCalledWith(PDF_PREVIEW_PAGE_LIMIT + 1)
  })

  it('gives a page whose size cannot be read the size of a readable one', async () => {
    const page = fakePage(595, 842)
    const document = { numPages: 3, getPage: vi.fn() }
    document.getPage
      .mockRejectedValueOnce(namedError('UnknownErrorException'))
      .mockResolvedValueOnce(page)
      .mockResolvedValueOnce(page)
    mocks.getDocument.mockReturnValueOnce(fakeLoadingTask(Promise.resolve(document)))
    const preview = await openPdfPreview(pdfBytes())
    expect(preview.pageSizes).toEqual(Array(3).fill({ width: 595, height: 842 }))
  })

  it.each([
    ['PasswordException', 'password'],
    ['InvalidPDFException', 'invalid'],
    ['UnknownErrorException', 'unavailable'],
  ])('reports %s as a %s failure and releases the worker', async (name, reason) => {
    const task = fakeLoadingTask(Promise.reject(namedError(name)))
    mocks.getDocument.mockReturnValueOnce(task)
    const error = await openPdfPreview(pdfBytes()).catch(caught => caught)
    expect(error).toBeInstanceOf(PdfPreviewError)
    expect(error.reason).toBe(reason)
    expect(task.destroy).toHaveBeenCalled()
  })

  it('treats a file with no readable page as invalid', async () => {
    const document = { numPages: 1, getPage: vi.fn().mockRejectedValue(namedError('FormatError')) }
    const task = fakeLoadingTask(Promise.resolve(document))
    mocks.getDocument.mockReturnValueOnce(task)
    const error = await openPdfPreview(pdfBytes()).catch(caught => caught)
    expect(error.reason).toBe('invalid')
    expect(task.destroy).toHaveBeenCalled()
  })

  it('stops and releases the worker when the preview closes while the file is parsed', async () => {
    const pending = deferred<unknown>()
    const task = fakeLoadingTask(pending.promise)
    mocks.getDocument.mockReturnValueOnce(task)
    const controller = new AbortController()
    const opening = openPdfPreview(pdfBytes(), { signal: controller.signal }).catch(caught => caught)
    await vi.waitFor(() => expect(mocks.getDocument).toHaveBeenCalled())

    controller.abort()
    expect(task.destroy).toHaveBeenCalled()
    pending.reject(namedError('AbortException'))
    expect(isPdfPreviewAbort(await opening)).toBe(true)
  })

  it('does not start parsing when the preview closed before pdf.js loaded', async () => {
    const controller = new AbortController()
    const opening = openPdfPreview(pdfBytes(), { signal: controller.signal })
    controller.abort()
    expect(isPdfPreviewAbort(await opening.catch(caught => caught))).toBe(true)
    expect(mocks.getDocument).not.toHaveBeenCalled()
  })

  it('draws a page fitted to the given width, within the canvas memory cap', async () => {
    const document = fakeDocument(1)
    mocks.getDocument.mockReturnValueOnce(fakeLoadingTask(Promise.resolve(document)))
    const preview = await openPdfPreview(pdfBytes())

    const target = canvas()
    await preview.renderPage(1, target, 306).promise
    // Half of a 612pt-wide page at a density of 1.
    expect(target.width).toBe(306)
    expect(target.height).toBe(396)
    expect(document.page.render).toHaveBeenCalledWith({ canvas: target, viewport: { width: 306, height: 396 } })
    expect(document.page.cleanup).toHaveBeenCalled()

    const huge = canvas()
    await preview.renderPage(1, huge, 20_000).promise
    expect(huge.width * huge.height).toBeLessThanOrEqual(3_000_000)
  })

  it('cancels a page that is still drawing', async () => {
    const page = fakePage()
    const drawing = deferred<void>()
    page.renderTask.promise = drawing.promise
    page.renderTask.cancel.mockImplementation(() => drawing.reject(namedError('RenderingCancelledException')))
    const document = fakeDocument(1, page)
    mocks.getDocument.mockReturnValueOnce(fakeLoadingTask(Promise.resolve(document)))
    const preview = await openPdfPreview(pdfBytes())

    const render = preview.renderPage(1, canvas(), 600)
    await vi.waitFor(() => expect(page.render).toHaveBeenCalled())
    render.cancel()
    expect(page.renderTask.cancel).toHaveBeenCalled()
    expect(isPdfPreviewAbort(await render.promise.catch(caught => caught))).toBe(true)
  })

  it('releases the worker when the preview is destroyed', async () => {
    const task = fakeLoadingTask(Promise.resolve(fakeDocument(1)))
    mocks.getDocument.mockReturnValueOnce(task)
    const preview = await openPdfPreview(pdfBytes())
    preview.destroy()
    expect(task.destroy).toHaveBeenCalledTimes(1)
  })
})

describe('bundled pdf.js', () => {
  // The CSP has no 'unsafe-eval'. pdf.js 6 dropped its eval-based code path;
  // if an upgrade brings one back, isEvalSupported: false must keep it off.
  it.each(['pdfjs-dist/legacy/build/pdf.mjs', 'pdfjs-dist/legacy/build/pdf.worker.min.mjs'])(
    '%s compiles no code at runtime',
    file => {
      const source = readFileSync(createRequire(import.meta.url).resolve(file), 'utf8')
      // core-js's global-object shim, unreachable where globalThis exists.
      const withoutShim = source.replace(/Function\((["'])return this\1\)\(\)/g, '')
      expect(withoutShim).not.toMatch(/\bnew Function\s*\(/)
      expect(withoutShim).not.toMatch(/(?<![.\w$])Function\s*\(\s*["'`]/)
      expect(withoutShim).not.toMatch(/(?<![.\w$])eval\s*\(/)
    },
  )
})
