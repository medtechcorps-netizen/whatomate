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
// before its ordinary decode path. Mask allocation is checked below. 4096x4096
// still covers an A4 page scanned at 400 dpi.
export const PDF_PREVIEW_MAX_IMAGE_PIXELS = 4096 * 4096

export interface PdfImageScan {
  oversized: boolean
  // The combined image/mask draw surface exceeds the pixel budget.
  oversizedMask: boolean
  // Safe dimensions, stream boundaries or reference identity were not proved.
  unverifiable: boolean
}

// Conservative preflight, not a PDF decoder or xref repairer. The byte cursor
// only moves forward. Strings and stream payloads never supply object IDs.
// Budget exhaustion falls back to download, never to an assumed safe preview.
const MAX_SCAN_OBJECTS = 8192
const MAX_DICTIONARY_BYTES = 64 << 10
const MAX_SCAN_TOKENS = 1_000_000
const MAX_VALUE_DEPTH = 32
const MAX_TOKEN_BYTES = 128

class PdfScanRefusal extends Error {}
function refuseScan(): never { throw new PdfScanRefusal() }
interface PdfToken { kind: string; text: string; start: number }
type PdfValue = number | string | boolean | null | PdfReference | PdfValue[] | Map<string, PdfValue>
interface PdfReference { object: number; generation: number }
interface ScannedImage { width: number; height: number; mask: PdfReference | null }
const command = (token: PdfToken, text: string) => token.kind === 'word' && token.text === text
const white = (byte: number) => byte === 0 || byte === 9 || byte === 10 || byte === 12 || byte === 13 || byte === 32
const delimiter = (byte: number) => white(byte) || byte === 40 || byte === 41 || byte === 60 || byte === 62 || byte === 91 || byte === 93 || byte === 47 || byte === 37
const digit = (byte: number) => byte >= 48 && byte <= 57
const refKey = (ref: PdfReference) => ref.object + ':' + ref.generation
const isReference = (value: PdfValue | undefined): value is PdfReference =>
  value !== undefined && value !== null && typeof value === 'object' && !Array.isArray(value) && !(value instanceof Map)

class PdfScanReader {
  position = 0
  private tokens = 0
  private ahead: PdfToken[] = []
  constructor(readonly bytes: Uint8Array) {}

  peek(index = 0): PdfToken {
    while (this.ahead.length <= index) this.ahead.push(this.token())
    return this.ahead[index]!
  }
  next(): PdfToken { return this.ahead.shift() ?? this.token() }

  private token(): PdfToken {
    const bytes = this.bytes
    while (this.position < bytes.length) {
      if (white(bytes[this.position]!)) { this.position++; continue }
      if (bytes[this.position] !== 37) break
      while (this.position < bytes.length && bytes[this.position] !== 10 && bytes[this.position] !== 13) this.position++
    }
    const start = this.position
    if (++this.tokens > MAX_SCAN_TOKENS) refuseScan()
    if (start === bytes.length) return { kind: 'eof', text: '', start }
    const first = bytes[this.position++]!
    const result = (kind: string, text = '') => ({ kind, text, start })
    if (first === 40) {
      let depth = 1
      while (this.position < bytes.length && depth) {
        const byte = bytes[this.position++]!
        if (byte === 92) { if (this.position < bytes.length) this.position++; continue }
        if (byte === 40 && ++depth > MAX_VALUE_DEPTH) refuseScan()
        if (byte === 41) depth--
      }
      if (depth) refuseScan()
      return result('opaque')
    }
    if (first === 60) {
      if (bytes[this.position] === 60) { this.position++; return result('<<') }
      while (this.position < bytes.length && bytes[this.position] !== 62) {
        if (!white(bytes[this.position]!) && !/[0-9a-f]/i.test(String.fromCharCode(bytes[this.position]!))) refuseScan()
        this.position++
      }
      if (bytes[this.position++] !== 62) refuseScan()
      return result('opaque')
    }
    if (first === 62) {
      if (bytes[this.position++] !== 62) refuseScan()
      return result('>>')
    }
    if (first === 91 || first === 93) return result(String.fromCharCode(first))
    if (first === 41) refuseScan()
    const numeric = digit(first) || first === 43 || first === 45 || first === 46
    let text = first === 47 ? '' : String.fromCharCode(first)
    while (this.position < bytes.length && !delimiter(bytes[this.position]!)) {
      const byte = bytes[this.position]!
      // pdf.js accepts "9 0obj": the integer ends before the command.
      if (numeric && !digit(byte) && byte !== 46) break
      if (this.position - start >= MAX_TOKEN_BYTES) refuseScan()
      text += String.fromCharCode(byte)
      this.position++
    }
    if (first === 47) {
      if (/#(?![0-9a-f]{2})/i.test(text)) refuseScan()
      text = text.replace(/#([0-9a-f]{2})/gi, (_, hex: string) => String.fromCharCode(parseInt(hex, 16)))
      return result('name', text)
    }
    return result(numeric ? 'number' : 'word', text)
  }

  value(depth = 0): PdfValue {
    if (depth > MAX_VALUE_DEPTH) refuseScan()
    const token = this.next()
    if (token.kind === 'number') {
      const value = Number(token.text)
      if (!Number.isFinite(value)) refuseScan()
      if (this.peek().kind === 'number' && command(this.peek(1), 'R')) {
        const generation = Number(this.next().text)
        this.next()
        if (!Number.isSafeInteger(value) || value < 0 || !Number.isSafeInteger(generation) || generation < 0) refuseScan()
        return { object: value, generation }
      }
      return value
    }
    if (token.kind === 'name') return token.text
    if (token.kind === 'opaque') return null
    if (token.kind === 'word' && token.text === 'null') return null
    if (token.kind === 'word' && (token.text === 'true' || token.text === 'false')) return token.text === 'true'
    if (token.kind === '[') {
      const values: PdfValue[] = []
      while (this.peek().kind !== ']') {
        if (this.position - token.start > MAX_DICTIONARY_BYTES) refuseScan()
        values.push(this.value(depth + 1))
      }
      this.next()
      return values
    }
    if (token.kind === '<<') {
      const entries = new Map<string, PdfValue>()
      while (this.peek().kind !== '>>') {
        if (this.position - token.start > MAX_DICTIONARY_BYTES) refuseScan()
        const key = this.next()
        if (key.kind !== 'name' || entries.has(key.text)) refuseScan()
        entries.set(key.text, this.value(depth + 1))
      }
      this.next()
      if (this.position - token.start > MAX_DICTIONARY_BYTES) refuseScan()
      return entries
    }
    return refuseScan()
  }

  skipStream(length: PdfValue | undefined) {
    // Indirect/missing Length needs xref interpretation. Do not search binary
    // data for apparent end markers that could hide later image objects.
    if (typeof length !== 'number' || !Number.isSafeInteger(length) || length < 0 || this.ahead.length) refuseScan()
    const bytes = this.bytes
    if (bytes[this.position] === 13) this.position++
    if (bytes[this.position] === 10) this.position++
    else if (bytes[this.position - 1] !== 13) refuseScan()
    if (length > bytes.length - this.position) refuseScan()
    this.position += length
    if (!command(this.next(), 'endstream')) refuseScan()
  }

  finishFile() {
    if (this.ahead.length) refuseScan()
    while (white(this.bytes[this.position]!)) this.position++
    for (const byte of [37, 37, 69, 79, 70]) {
      if (this.bytes[this.position++] !== byte) refuseScan() // %%EOF
    }
    while (white(this.bytes[this.position]!)) this.position++
    if (this.position !== this.bytes.length) refuseScan()
  }
}

function scanInteger(token: PdfToken): number {
  if (token.kind !== 'number' || !/^\d+$/.test(token.text)) return refuseScan()
  const value = Number(token.text)
  if (!Number.isSafeInteger(value)) return refuseScan()
  return value
}

// Validate one ordinary table, not incremental/hybrid xrefs or a repair path.
// pdf.js can resolve an xref into bytes inside a different stream: every active
// offset must agree with an actual top-level header seen by our byte cursor.
function readClassicXref(reader: PdfScanReader): { entries: Map<string, number>; root: PdfReference } {
  const entries = new Map<string, number>()
  const seen = new Set<number>()
  let freeZero = false
  while (!command(reader.peek(), 'trailer')) {
    const first = scanInteger(reader.next())
    const count = scanInteger(reader.next())
    if (!count || count > MAX_SCAN_OBJECTS + 1 - seen.size || !Number.isSafeInteger(first + count)) refuseScan()
    for (let index = 0; index < count; index++) {
      const object = first + index
      if (seen.has(object)) refuseScan()
      seen.add(object)
      const offset = scanInteger(reader.next())
      const generation = scanInteger(reader.next())
      const state = reader.next()
      if (generation > 65535) refuseScan()
      if (command(state, 'n')) {
        if (!object || offset >= reader.bytes.length) refuseScan()
        entries.set(refKey({ object, generation }), offset)
      } else if (command(state, 'f')) {
        // pdf.js repairs a subsection starting at1 with a free first entry
        // by shifting it to0. This validator never adopts that repair.
        if (first === 1 && index === 0) refuseScan()
        if (object === 0 && generation === 65535) freeZero = true
      } else refuseScan()
    }
  }
  reader.next()
  const trailer = reader.value()
  if (!(trailer instanceof Map) || trailer.has('Prev') || trailer.has('XRefStm') || trailer.has('Encrypt')) refuseScan()
  const size = trailer.get('Size')
  const root = trailer.get('Root')
  if (!freeZero || typeof size !== 'number' || !Number.isSafeInteger(size) || size <= 0 ||
      !isReference(root) || !entries.has(refKey(root))) refuseScan()
  for (const object of seen) if (object >= size) refuseScan()
  return { entries, root }
}

function imageDimension(dict: Map<string, PdfValue>, full: string, short: string): number {
  if (dict.has(full) && dict.has(short)) refuseScan()
  const value = dict.get(short) ?? dict.get(full)
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value <= 0) return refuseScan()
  return value
}

// Image streams cannot live in compressed object streams. Retain only bounded
// dimension/reference summaries, including masks without Subtype. Indirect
// lengths/dimensions, ambiguous revisions and unsupported xref forms refuse
// some valid PDFs. Every preview requires a complete matching classic xref,
// even when no stream was observed: pdf.js raw repair can discover hidden ones.
// Payload/dictionary size disagreement, inline images and decompression
// expansion remain outside this preflight's guarantee.
export function scanPdfImages(bytes: Uint8Array, maxPixels = PDF_PREVIEW_MAX_IMAGE_PIXELS): PdfImageScan {
  let oversized = false
  let oversizedMask = false
  try {
    const reader = new PdfScanReader(bytes)
    const objects = new Map<string, number>()
    const dictionaries = new Map<string, PdfReference | null>()
    const streams = new Map<string, Map<string, PdfValue>>()
    let xref: Map<string, number> | null = null
    let root: PdfReference | null = null
    let xrefOffset = -1
    let ended = false
    while (reader.peek().kind !== 'eof') {
      const first = reader.peek()
      if (xref && !command(first, 'startxref')) refuseScan()
      if (command(first, 'xref')) {
        if (xref) refuseScan()
        xrefOffset = reader.next().start
        const table = readClassicXref(reader)
        xref = table.entries
        root = table.root
        continue
      }
      if (command(first, 'startxref')) {
        reader.next()
        if (!xref || scanInteger(reader.next()) !== xrefOffset) refuseScan()
        reader.finishFile()
        ended = true
        break
      }
      if (first.kind !== 'number' || reader.peek(1).kind !== 'number' || !command(reader.peek(2), 'obj')) {
        // Trailer dictionaries/strings cannot supply object declarations.
        if (first.kind === '<<' || first.kind === '[') reader.value()
        else {
          if (['obj', 'stream', 'endstream', 'endobj'].includes(first.text)) refuseScan()
          reader.next()
        }
        continue
      }
      const object = Number(reader.next().text)
      const generation = Number(reader.next().text)
      reader.next()
      if (!Number.isSafeInteger(object) || object <= 0 || !Number.isSafeInteger(generation) || generation < 0) refuseScan()
      const key = refKey({ object, generation })
      if (objects.has(key) || objects.size >= MAX_SCAN_OBJECTS) refuseScan()
      objects.set(key, first.start)
      const value = reader.value()
      if (command(reader.peek(), 'stream')) {
        reader.next()
        if (!(value instanceof Map)) refuseScan()
        reader.skipStream(value.get('Length'))
        if (value.get('Type') === 'XRef') refuseScan()
        const subtype = value.get('Subtype')
        if (subtype !== undefined && typeof subtype !== 'string') refuseScan()
        const summary = new Map<string, PdfValue>()
        for (const name of ['Subtype', 'Width', 'W', 'Height', 'H', 'SMask', 'Mask']) {
          const field = value.get(name)
          if (field === undefined) continue
          // Complex values remain invalid without retaining large subtrees.
          summary.set(name, field instanceof Map || (Array.isArray(field) && (field.length > 32 || !field.every(item => typeof item === 'number'))) ? false : field)
        }
        streams.set(key, summary)
      } else if (value instanceof Map) {
        const pages = value.get('Pages')
        dictionaries.set(key, isReference(pages) ? pages : null)
      }
      if (!command(reader.next(), 'endobj')) refuseScan()
    }
    if (!xref || !ended) refuseScan()
    if (xref) {
      if (!ended || xref.size !== objects.size) refuseScan()
      for (const [key, offset] of objects) if (xref.get(key) !== offset) refuseScan()
    }
    // Pinned pdf.js re-enters raw-object repair when Root or Root.Pages is
    // not a Dict, even with a valid table. Prove the simple indirect form;
    // direct/unsupported catalogs fall back rather than reaching that repair.
    const pages = root ? dictionaries.get(refKey(root)) : null
    if (!pages || !dictionaries.has(refKey(pages))) refuseScan()
    const image = (dict: Map<string, PdfValue>): ScannedImage => {
      const width = imageDimension(dict, 'Width', 'W')
      const height = imageDimension(dict, 'Height', 'H')
      const soft = dict.get('SMask')
      if (soft !== undefined && soft !== null && soft !== 'None' && !isReference(soft)) refuseScan()
      const mask = soft !== undefined && soft !== null && soft !== 'None' ? soft : dict.get('Mask')
      if (mask !== undefined && mask !== null && mask !== 'None' && !isReference(mask) && !Array.isArray(mask)) refuseScan()
      return { width, height, mask: isReference(mask) ? mask : null }
    }
    for (const dict of streams.values()) {
      if (dict.get('Subtype') !== 'Image') continue
      const drawn = image(dict)
      if (drawn.width > maxPixels / drawn.height) { oversized = true; continue }
      if (!drawn.mask) continue
      const maskDict = streams.get(refKey(drawn.mask))
      if (!maskDict) refuseScan()
      const mask = image(maskDict)
      if (mask.mask) refuseScan() // Do not follow/assume harmless nested chains.
      if (Math.max(drawn.width, mask.width) > maxPixels / Math.max(drawn.height, mask.height)) oversizedMask = true
    }
    return { oversized, oversizedMask, unverifiable: false }
  } catch (error) {
    if (!(error instanceof PdfScanRefusal)) throw error
    return { oversized, oversizedMask, unverifiable: true }
  }
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
  if (images.unverifiable) throw new PdfPreviewError('unavailable')
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
