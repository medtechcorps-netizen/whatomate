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

// pdf.js leaves out any embedded image larger than this before decoding it,
// so a hostile or huge image cannot exhaust the worker's memory. 4096x4096
// still covers an A4 page scanned at 400 dpi.
export const PDF_PREVIEW_MAX_IMAGE_PIXELS = 4096 * 4096

export interface PdfImageScan {
  // Some image is over the limit; pdf.js leaves it out of the page.
  oversized: boolean
  // An image pdf.js will draw uses an image over the limit as its /SMask or
  // /Mask. pdf.js checks only the drawn image's own size but decodes into a
  // buffer as large as the image and its masks together, so a 1x1 image with
  // a 12000x12000 soft mask would allocate over half a gigabyte. Such a file
  // is not previewed at all.
  oversizedMask: boolean
}

// Each window is decoded on its own so a large file is never one huge
// string; latin1 keeps one character per byte. A dictionary is followed at
// most REACH characters, and windows overlap by that much on each side.
const SCAN_WINDOW = 1 << 20
const SCAN_REACH = 4096
// Bounded repeats keep every pattern linear, whatever the file contains.
const SCAN_TOKENS = /\bobj\b|\bstream\b|\/Subtype\s*\/Image\b/g
const OBJECT_NUMBER = /(\d{1,10})\s{1,8}\d{1,5}\s{1,8}$/
const IMAGE_WIDTH = /\/Width\s{1,8}(\d{1,10})/
const IMAGE_HEIGHT = /\/Height\s{1,8}(\d{1,10})/
const MASK_REFERENCE = /\/S?Mask\s{1,8}(\d{1,10})\s{1,8}\d{1,5}\s{1,8}R\b/g

// pdf.js says nothing when it leaves an image out, and a scanned page that is
// one large image would then look blank, so the sizes are read here first.
// Image XObjects are streams, which never sit inside compressed object
// streams, so their dictionaries are plain text in the file; encryption
// covers only strings and stream data. One pass over each window finds every
// object header, image subtype and stream keyword; only a dictionary of at
// most SCAN_REACH characters is ever examined, so hostile input costs linear
// time (16 MB of repeated image markers scans in well under a second).
//
// What this cannot see, and pdf.js still decodes: sizes given indirectly
// (/Width 12 0 R) or with escaped names (/Wid#74h); inline images in content
// streams; a JPEG whose own frame size differs from its dictionary; and
// compression bombs, since pdf.js puts no limit on how far a stream inflates.
export function scanPdfImages(bytes: Uint8Array, maxPixels = PDF_PREVIEW_MAX_IMAGE_PIXELS): PdfImageScan {
  const decoder = new TextDecoder('latin1')
  const oversizedObjects = new Set<number>()
  const drawnMasks = new Set<number>()
  let oversized = false

  for (let offset = 0; offset < bytes.length; offset += SCAN_WINDOW) {
    const from = Math.max(0, offset - SCAN_REACH)
    const text = decoder.decode(bytes.subarray(from, Math.min(bytes.length, offset + SCAN_WINDOW + SCAN_REACH)))
    // Images whose subtype lies in this window's own range are this window's;
    // the overlaps only supply their headers and stream keywords.
    const ownStart = offset - from
    const ownEnd = ownStart + SCAN_WINDOW
    let header = -1
    let image = false
    const tokens = new RegExp(SCAN_TOKENS)
    for (let match = tokens.exec(text); match; match = tokens.exec(text)) {
      const at = match.index
      if (match[0] === 'obj') {
        header = at
        image = false
      } else if (match[0] === 'stream') {
        if (image && at - header <= SCAN_REACH) {
          const dictionary = text.slice(header, at)
          const width = IMAGE_WIDTH.exec(dictionary)
          const height = IMAGE_HEIGHT.exec(dictionary)
          const tooLarge = !!width && !!height && Number(width[1]) * Number(height[1]) > maxPixels
          if (tooLarge) {
            oversized = true
            const number = OBJECT_NUMBER.exec(text.slice(Math.max(0, header - 32), header))
            if (number) oversizedObjects.add(Number(number[1]))
          } else {
            // pdf.js drops an oversized image before reading its masks, so
            // only the masks of images it will draw matter.
            for (const reference of dictionary.matchAll(MASK_REFERENCE)) drawnMasks.add(Number(reference[1]))
          }
        }
        header = -1
        image = false
      } else if (header >= 0 && at - header <= SCAN_REACH && at >= ownStart && at < ownEnd) {
        image = true
      }
    }
  }

  let oversizedMask = false
  for (const mask of drawnMasks) oversizedMask ||= oversizedObjects.has(mask)
  return { oversized, oversizedMask }
}

export type PdfPreviewFailure = 'password' | 'invalid' | 'too-large' | 'unavailable'

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
  // The file has images over PDF_PREVIEW_MAX_IMAGE_PIXELS, which the preview leaves out.
  readonly hasOversizedImages: boolean
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

// Where pdf.js finds the files it loads by name: copied next to the built
// assets by pdfjsRuntimeFiles in vite.config.ts, served from the package
// itself by the dev server. The URL is built from a variable so Vite does not
// treat it as an asset import of its own.
function runtimeFilesURL(version: string): string {
  if (import.meta.env.DEV) return '/node_modules/pdfjs-dist/'
  const moduleURL = import.meta.url
  return new URL(`pdfjs-${version}/`, moduleURL).href
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

  // Read before getDocument, which hands the bytes to the worker.
  const images = scanPdfImages(data)
  if (images.oversizedMask) throw new PdfPreviewError('too-large')
  const hasOversizedImages = images.oversized
  const files = runtimeFilesURL(pdfjs.version)
  const params = {
    // Transferred to the worker; this copy is not used again.
    data,
    maxImageSize: PDF_PREVIEW_MAX_IMAGE_PIXELS,
    // CJK text in fonts the file does not embed needs Adobe's CMaps.
    cMapUrl: `${files}cmaps/`,
    // Other standard fonts fall back to system fonts; Symbol and ZapfDingbats
    // (check marks on forms) have none.
    standardFontDataUrl: `${files}standard_fonts/`,
    // Scanned pages (JBIG2, CCITT fax) and JPEG 2000 images decode in pdf.js
    // 6 only through these modules. Their WebAssembly builds would need
    // 'wasm-unsafe-eval', which the CSP does not grant, so the JavaScript
    // builds are used and no WebAssembly is ever compiled.
    wasmUrl: `${files}wasm/`,
    useWasm: false,
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
    hasOversizedImages,
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
