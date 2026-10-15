import type { PDFDocumentLoadingTask, PDFDocumentProxy, RenderTask } from 'pdfjs-dist/legacy/build/pdf.mjs'
// The worker is a same-origin file emitted next to the app's own assets, so
// the CSP's worker-src 'self' admits it. It is only fetched when a PDF is
// first previewed. The legacy build carries polyfills: the modern build calls
// built-ins (Map.prototype.getOrInsertComputed and others) that agents' older
// browsers do not have.
import pdfWorkerURL from 'pdfjs-dist/legacy/build/pdf.worker.min.mjs?url'

// Attachments sent as documents are previewed only when their bytes say what
// they are; the type the sender declared is not trusted. A PDF is drawn onto
// canvases by pdf.js and never becomes a document of its own: no object URL
// with a PDF type is created, so neither the browser's PDF viewer nor "Open in
// new tab" ever renders it, and the download keeps its opaque type (see
// displaySafeMediaBlob). JPEG and PNG get an image type only for the preview.
export type DocumentPreviewType = 'application/pdf' | 'image/jpeg' | 'image/png'

const PDF_SIGNATURE = [0x25, 0x50, 0x44, 0x46, 0x2d] // %PDF-

const SIGNATURES: ReadonlyArray<readonly [DocumentPreviewType, readonly number[]]> = [
  ['application/pdf', PDF_SIGNATURE],
  ['image/png', [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]],
  ['image/jpeg', [0xff, 0xd8, 0xff]],
]

async function leadingBytes(blob: Blob, count: number): Promise<Uint8Array> {
  return new Uint8Array(await blob.slice(0, count).arrayBuffer())
}

function startsWith(bytes: Uint8Array, signature: readonly number[]): boolean {
  return bytes.length >= signature.length && signature.every((byte, index) => bytes[index] === byte)
}

export async function documentPreviewType(blob: Blob): Promise<DocumentPreviewType | null> {
  const head = await leadingBytes(blob, 8)
  return SIGNATURES.find(([, signature]) => startsWith(head, signature))?.[0] ?? null
}

// Long files show their first pages; the rest are a download away. Each page
// is drawn only when it scrolls into view.
export const PDF_PREVIEW_PAGE_LIMIT = 30

// A drawn page is capped at about 12 MB of canvas memory, whatever the page
// size or screen density.
const MAX_CANVAS_PIXELS = 3_000_000
const MAX_OUTPUT_SCALE = 2

export type PdfPreviewFailure = 'password' | 'invalid' | 'unavailable'

export class PdfPreviewError extends Error {
  constructor(readonly reason: PdfPreviewFailure) {
    super(`PDF preview failed: ${reason}`)
    this.name = 'PdfPreviewError'
  }
}

export interface PdfPageSize {
  width: number
  height: number
}

export interface PdfPageRender {
  promise: Promise<void>
  cancel(): void
}

export interface PdfPreviewDocument {
  // Pages in the file; only the first PDF_PREVIEW_PAGE_LIMIT are previewed.
  readonly pageCount: number
  // Size of each previewed page in PDF points; the preview keeps its proportions.
  readonly pageSizes: readonly PdfPageSize[]
  renderPage(pageNumber: number, canvas: HTMLCanvasElement, cssWidth: number): PdfPageRender
  destroy(): void
}

type PdfjsModule = typeof import('pdfjs-dist/legacy/build/pdf.mjs')

let pdfjsModule: Promise<PdfjsModule> | null = null

// pdf.js is a separate chunk loaded on the first preview. A failed load (for
// example a chunk removed by a deploy) is forgotten so the next preview retries.
function loadPdfjs(): Promise<PdfjsModule> {
  if (!pdfjsModule) {
    const loading = import('pdfjs-dist/legacy/build/pdf.mjs').then(pdfjs => {
      pdfjs.GlobalWorkerOptions.workerSrc = pdfWorkerURL
      return pdfjs
    })
    pdfjsModule = loading
    loading.catch(() => {
      if (pdfjsModule === loading) pdfjsModule = null
    })
  }
  return pdfjsModule
}

function abortError(): DOMException {
  return new DOMException('PDF preview closed', 'AbortError')
}

export function isPdfPreviewAbort(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}

function failureFor(error: unknown): PdfPreviewFailure {
  const name = (error as { name?: unknown } | null)?.name
  if (name === 'PasswordException') return 'password'
  if (name === 'InvalidPDFException' || name === 'FormatError') return 'invalid'
  return 'unavailable'
}

export async function openPdfPreview(blob: Blob, options: { signal?: AbortSignal } = {}): Promise<PdfPreviewDocument> {
  const { signal } = options
  // Checked before pdf.js loads: bytes that are not a PDF never reach it.
  if (!startsWith(await leadingBytes(blob, PDF_SIGNATURE.length), PDF_SIGNATURE)) throw new PdfPreviewError('invalid')
  if (signal?.aborted) throw abortError()

  let pdfjs: PdfjsModule
  let data: Uint8Array
  try {
    ;[pdfjs, data] = await Promise.all([loadPdfjs(), blob.arrayBuffer().then(buffer => new Uint8Array(buffer))])
  } catch {
    throw new PdfPreviewError('unavailable')
  }
  if (signal?.aborted) throw abortError()

  const params = {
    // Transferred to the worker; this copy is not used again.
    data,
    // pdf.js 6 has no eval-based code left, and dropped this option with it.
    // It is still set so a downgrade to a 5.x build stays eval-free: the CSP
    // has no 'unsafe-eval', and CVE-2024-4367 ran through pdf.js's eval path.
    isEvalSupported: false,
    // XFA forms would build HTML from the file; only canvas output is used.
    enableXfa: false,
    verbosity: pdfjs.VerbosityLevel.ERRORS,
  }
  // No onPassword handler: an encrypted file rejects with PasswordException.
  const loadingTask: PDFDocumentLoadingTask = pdfjs.getDocument(params)
  const destroy = () => void loadingTask.destroy().catch(() => undefined)
  signal?.addEventListener('abort', destroy, { once: true })

  let pdf: PDFDocumentProxy
  let pageSizes: PdfPageSize[]
  try {
    pdf = await loadingTask.promise
    const previewed = Math.min(pdf.numPages, PDF_PREVIEW_PAGE_LIMIT)
    const sizes = await Promise.allSettled(
      Array.from({ length: previewed }, (_, index) =>
        pdf.getPage(index + 1).then(page => {
          const { width, height } = page.getViewport({ scale: 1 })
          return { width, height }
        }),
      ),
    )
    // A page whose size cannot be read takes the size of one that can, and
    // shows its own failure when drawn.
    const fallback = sizes.find((size): size is PromiseFulfilledResult<PdfPageSize> => size.status === 'fulfilled')
    if (!fallback) throw new PdfPreviewError('invalid')
    pageSizes = sizes.map(size => (size.status === 'fulfilled' ? size.value : fallback.value))
  } catch (error) {
    signal?.removeEventListener('abort', destroy)
    destroy()
    if (signal?.aborted) throw abortError()
    if (error instanceof PdfPreviewError) throw error
    throw new PdfPreviewError(failureFor(error))
  }
  if (signal?.aborted) throw abortError()
  signal?.removeEventListener('abort', destroy)

  return {
    pageCount: pdf.numPages,
    pageSizes,
    renderPage(pageNumber, canvas, cssWidth) {
      let task: RenderTask | null = null
      let cancelled = false
      const promise = (async () => {
        const page = await pdf.getPage(pageNumber)
        if (cancelled) throw abortError()
        const size = page.getViewport({ scale: 1 })
        const density = Math.min(globalThis.devicePixelRatio || 1, MAX_OUTPUT_SCALE)
        const fitted = (cssWidth / size.width) * density
        const capped = Math.sqrt(MAX_CANVAS_PIXELS / (size.width * size.height))
        const viewport = page.getViewport({ scale: Math.min(fitted, capped) })
        canvas.width = Math.floor(viewport.width)
        canvas.height = Math.floor(viewport.height)
        task = page.render({ canvas, viewport })
        try {
          await task.promise
        } catch (error) {
          if (cancelled) throw abortError()
          throw error
        } finally {
          page.cleanup()
        }
      })()
      return {
        promise,
        cancel() {
          cancelled = true
          task?.cancel()
        },
      }
    },
    destroy,
  }
}
