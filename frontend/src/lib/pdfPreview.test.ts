import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
  documentPreviewType,
  isPdfPreviewAbort,
  openPdfPreview,
  PDF_PREVIEW_MAX_IMAGE_PIXELS,
  PDF_PREVIEW_PAGE_LIMIT,
  PdfPreviewError,
  scanPdfImages,
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
    version: '6.3.289',
  }
})

const encoder = new TextEncoder()
const pdfBytes = () => new Blob([pdfWith()], { type: 'application/octet-stream' })

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

// A PDF image XObject with the given dictionary entries.
const imageObject = (entries: string, number = 4) =>
  `${number} 0 obj\n<< /Type /XObject /Subtype /Image ${entries} /BitsPerComponent 8 ${entries.includes('/ColorSpace') ? '' : '/ColorSpace /DeviceRGB'} /Length 3 >>\nstream\nabc\nendstream\nendobj\n`
function pdfWith(...objects: string[]) {
  return pdfObjects([
    '1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n',
    '2 0 obj\n<< /Type /Pages /Kids [] /Count 0 >>\nendobj\n',
    ...objects,
  ])
}
function pdfObjects(objects: string[]) {
  let body = '%PDF-1.7\n'
  let byteOffset = encoder.encode(body).length
  const entries = new Map<number, { offset: number; generation: number }>()
  for (const object of objects) {
    const header = /^(\s*)(\d+)\s+(\d+)\s*obj/.exec(object)
    if (!header) throw new Error('Fixture must supply one top-level object per argument')
    entries.set(Number(header[2]), { offset: byteOffset + encoder.encode(header[1]).length, generation: Number(header[3]) })
    body += object + '\n'
    byteOffset += encoder.encode(object + '\n').length
  }
  const size = Math.max(...entries.keys()) + 1
  const xrefOffset = byteOffset
  let xref = 'xref\n0 ' + size + '\n0000000000 65535 f \n'
  for (let number = 1; number < size; number++) {
    const entry = entries.get(number)
    xref += entry
      ? String(entry.offset).padStart(10, '0') + ' ' + String(entry.generation).padStart(5, '0') + ' n \n'
      : '0000000000 00000 f \n'
  }
  return encoder.encode(body + xref + 'trailer\n<< /Size ' + size + ' /Root 1 0 R >>\nstartxref\n' + xrefOffset + '\n%%EOF\n')
}
const scan = (...objects: string[]) => scanPdfImages(pdfWith(...objects))

const maskParent = imageObject('/Width 1 /Height 1 /SMask 9 0 R')
const largeMask = imageObject('/Width 12000 /Height 12000', 9)
function hiddenMaskPdf(pointIntoPayload: boolean) {
  const smallMask = imageObject('/Width 1 /Height 1', 9)
  const container = '20 0 obj << /Length ' + encoder.encode(largeMask).length + ' >>\nstream\n' + largeMask + '\nendstream\nendobj'
  const bytes = pdfWith(maskParent, smallMask, container)
  if (!pointIntoPayload) return bytes
  const text = new TextDecoder().decode(bytes)
  const xref = text.lastIndexOf('\nxref\n') + 1
  const lines = text.slice(xref).split('\n')
  // Fixed-width replacement preserves every offset and startxref. pdf.js
  // fetchUncompressed would otherwise parse this header inside stream20.
  lines[2 + 9] = String(text.indexOf(largeMask)).padStart(10, '0') + ' 00000 n '
  return encoder.encode(text.slice(0, xref) + lines.join('\n'))
}

function firstOneRepairPdf() {
  const header = '%PDF-1.7\n'
  const catalog = '2 0 obj << /Type /Catalog /Pages 3 0 R >> endobj\n'
  const pages = '3 0 obj << /Type /Pages /Kids [] /Count 0 >> endobj\n'
  const image = imageObject('/Width 1 /Height 1')
  const body = header + catalog + pages + image
  const row = (offset: number) => String(offset).padStart(10, '0') + ' 00000 n \n'
  // No repeated subsection IDs: without the explicit first=1/free refusal
  // our observed object2 would match, while pdf.js shifts that entry to1.
  const table = 'xref\n0 1\n0000000000 65535 f \n1 2\n0000000000 00000 f \n' +
    row(encoder.encode(header).length) + '3 2\n' + row(encoder.encode(header + catalog).length) + row(encoder.encode(header + catalog + pages).length)
  return encoder.encode(body + table + 'trailer\n<< /Size 5 /Root 2 0 R >>\nstartxref\n' + encoder.encode(body).length + '\n%%EOF\n')
}
const maskVariants = [
  ['short dimensions', largeMask.replace('/Width', '/W').replace('/Height', '/H')],
  ['number adjoining obj', largeMask.replace('9 0 obj', '9 0obj')],
  ['no subtype', largeMask.replace('/Subtype /Image', '')],
  ['escaped names', largeMask.replace('/Width', '/Wid#74h').replace('/Subtype', '/Sub#74ype')],
  ['long dictionary before dimensions', largeMask.replace('/Width', '/Note (' + 'x'.repeat(5000) + ') /Width')],
  ['long dictionary after dimensions', largeMask.replace('/Length', '/Note (' + 'x'.repeat(5000) + ') /Length')],
] as const

const unknownMasks = [
  ['absent reference', [maskParent]],
  ['different generation', [maskParent, largeMask.replace('9 0 obj', '9 1 obj')]],
  ['ambiguous revision', [maskParent, imageObject('/Width 1 /Height 1', 9), largeMask]],
  ['indirect dimension', [maskParent, largeMask.replace('/Width 12000', '/Width 12 0 R'), '12 0 obj 12000 endobj']],
  ['missing dimension', [maskParent, largeMask.replace('/Width 12000', '')]],
  ['duplicate dimension', [maskParent, largeMask.replace('/Width 12000', '/Width 1 /Width 12000')]],
  ['conflicting alias', [maskParent, largeMask.replace('/Width 12000', '/W 1 /Width 12000')]],
  ['indirect subtype', [maskParent, largeMask.replace('/Subtype /Image', '/Subtype 12 0 R'), '12 0 obj /Image endobj']],
  ['non-stream mask', [maskParent, '9 0 obj << /Width 1 /Height 1 >> endobj']],
  ['indirect stream length', [maskParent, largeMask.replace('/Length 3', '/Length 12 0 R'), '12 0 obj 3 endobj']],
  ['missing stream length', [maskParent, largeMask.replace('/Length 3', '')]],
  ['wrong stream length', [maskParent, largeMask.replace('/Length 3', '/Length 1')]],
  ['over-budget dictionary', [maskParent, largeMask.replace('/Width', '/Note (' + 'x'.repeat(70_000) + ') /Width')]],
  ['nested mask chain', [maskParent, imageObject('/Width 1 /Height 1 /SMask 10 0 R', 9), imageObject('/Width 1 /Height 1', 10)]],
  ['unsupported soft-mask array', [imageObject('/Width 1 /Height 1 /SMask [] /Mask 9 0 R'), largeMask]],
] as const

const invalidCatalogs = [
  ['scalar Root', ['1 0 obj null endobj']],
  ['stream Root', ['1 0 obj << /Length 3 >>\nstream\nabc\nendstream\nendobj']],
  ['absent Pages', ['1 0 obj << /Type /Catalog >> endobj']],
  ['missing Pages reference', ['1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj']],
  ['scalar Pages', ['1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj', '2 0 obj null endobj']],
  ['stream Pages', ['1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj', '2 0 obj << /Length 3 >>\nstream\nabc\nendstream\nendobj']],
  ['direct Pages outside supported proof', ['1 0 obj << /Type /Catalog /Pages << /Type /Pages /Count 0 >> >> endobj']],
] as const

describe('scanPdfImages', () => {
  it.each(invalidCatalogs)('refuses a valid-table file that can enter catalog repair: %s', (_, objects) => {
    expect(scanPdfImages(pdfObjects([...objects])).unverifiable).toBe(true)
  })
  it('requires xref mask identity to agree with the top-level header', () => {
    expect(scanPdfImages(hiddenMaskPdf(false))).toEqual({ oversized: false, oversizedMask: false, unverifiable: false })
    expect(scanPdfImages(hiddenMaskPdf(true)).unverifiable).toBe(true)
  })

  it('requires a proved table even when all image syntax is inside an opaque string', () => {
    const hidden = encoder.encode('%PDF-1.7\n(' + maskParent + largeMask + ')\n%%EOF\n')
    expect(scanPdfImages(hidden)).toMatchObject({ unverifiable: true })
    expect(scanPdfImages(encoder.encode('%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\n%%EOF\n')).unverifiable).toBe(true)
  })

  it('refuses the first-one/free-entry repair without relying on duplicate subsection rejection', () => {
    expect(scanPdfImages(firstOneRepairPdf()).unverifiable).toBe(true)
  })

  it('refuses xref streams instead of treating their opaque payload as a proved table', () => {
    const xrefStream = '20 0 obj << /Type /XRef /Length 3 >>\nstream\nabc\nendstream\nendobj'
    expect(scan(imageObject('/Width 1 /Height 1'), xrefStream).unverifiable).toBe(true)
  })

  it.each([
    ['absent table', (text: string) => text.slice(0, text.indexOf('\nxref\n')) + '\n%%EOF\n'],
    ['wrong startxref', (text: string) => text.replace(/startxref\n\d+/, 'startxref\n0')],
    ['wrong entry generation', (text: string) => text.replace(/(\d{10}) 00000 n /, '$1 00001 n ')],
    ['incremental predecessor', (text: string) => text.replace('/Root 1 0 R', '/Root 1 0 R /Prev 1')],
    ['hybrid xref stream', (text: string) => text.replace('/Root 1 0 R', '/Root 1 0 R /XRefStm 1')],
    ['encrypted trailer', (text: string) => text.replace('/Root 1 0 R', '/Root 1 0 R /Encrypt 8 0 R')],
    ['overlapping subsection', (text: string) => text.replace('trailer\n', '0 1\n0000000000 65535 f \ntrailer\n')],
    ['trailing revision', (text: string) => text + 'xref\n0 1\n0000000000 65535 f \n'],
  ])('refuses an unsupported or unproven xref: %s', (_, mutate) => {
    const safe = new TextDecoder().decode(pdfWith(imageObject('/Width 1 /Height 1')))
    expect(scanPdfImages(encoder.encode(mutate(safe))).unverifiable).toBe(true)
  })
  it.each(maskVariants)('bounds mask allocation with %s', (_, mask) => {
    expect(scan(maskParent, mask)).toMatchObject({ oversizedMask: true, unverifiable: false })
    expect(scan(mask, maskParent)).toMatchObject({ oversizedMask: true, unverifiable: false })
  })

  it.each(unknownMasks)('fails closed for %s', (_, objects) => {
    expect(scan(...objects).unverifiable).toBe(true)
  })

  it('bounds the combined surface, not each image area in isolation', () => {
    const image = imageObject('/W 16384 /H 1024 /SMask 9 0 R')
    const mask = imageObject('/W 1024 /H 16384', 9)
    expect(16384 * 1024).toBe(PDF_PREVIEW_MAX_IMAGE_PIXELS)
    // Only metadata is scanned; the hypothetical 1GiB RGBA is never allocated.
    expect(scan(image, mask)).toEqual({ oversized: false, oversizedMask: true, unverifiable: false })
    expect(scan(image, imageObject('/W 16384 /H 1024', 9))).toEqual({
      oversized: false, oversizedMask: false, unverifiable: false,
    })
  })

  it('does not take nested/comment/string dimensions instead of top-level dimensions', () => {
    const mask = largeMask.replace('/Width 12000', '/Note (9 0 obj /Width 1 /Height 1) /DecodeParms << /Width 1 /Height 1 >> % /Width 1\n/Width 12000')
    expect(scan(maskParent, mask)).toMatchObject({ oversizedMask: true, unverifiable: false })
  })

  it('does not resolve a mask using fake object headers inside a stream', () => {
    const decoy = imageObject('/Width 1 /Height 1', 9)
    const content = '20 0 obj << /Length ' + encoder.encode(decoy).length + ' >>\nstream\n' + decoy + '\nendstream\nendobj\n'
    expect(scan(maskParent, content).unverifiable).toBe(true)
    expect(scan(maskParent, content, largeMask)).toMatchObject({ oversizedMask: true, unverifiable: false })
  })

  it('bounds object bookkeeping and dictionary nesting without assuming safety', () => {
    const objects = Array.from({ length: 8193 }, (_, index) => (index + 20) + ' 0 obj null endobj\n')
    expect(scan(...objects).unverifiable).toBe(true)
    const nested = '/Note ' + '['.repeat(34) + '0' + ']'.repeat(34)
    expect(scan(imageObject('/Width 1 /Height 1 ' + nested)).unverifiable).toBe(true)
  })

  it('accepts a proven small mask with no subtype and an exact nonzero generation', () => {
    const parent = maskParent.replace('/SMask 9 0 R', '/SMask 9 2 R')
    const mask = imageObject('/W 800 /H 600', 9).replace('/Subtype /Image', '').replace('9 0 obj', '9 2obj')
    expect(scan(parent, mask)).toEqual({ oversized: false, oversizedMask: false, unverifiable: false })
  })
  it('limits images to 4096 x 4096 pixels', () => {
    expect(PDF_PREVIEW_MAX_IMAGE_PIXELS).toBe(16_777_216)
    expect(scan(imageObject('/Width 4096 /Height 4096')).oversized).toBe(false)
    expect(scan(imageObject('/Width 4097 /Height 4096')).oversized).toBe(true)
    expect(scan(imageObject('/Width 30000 /Height 30000')).oversized).toBe(true)
  })

  it.each([
    ['compact syntax', '4 0 obj<</Subtype/Image/Width 5000/Height 5000/Length 3>>stream\nabc\nendstream endobj'],
    ['height before width', imageObject('/Height 5000 /Width 5000')],
    ['a nested parameters dictionary', imageObject('/DecodeParms << /Columns 5000 /K -1 >> /Width 5000 /Height 5000')],
  ])('reads the size of an image in %s', (_, object) => {
    expect(scan(object).oversized).toBe(true)
  })

  it('ignores files without a large image', () => {
    expect(scan()).toEqual({ oversized: false, oversizedMask: false, unverifiable: false })
    expect(scan(imageObject('/Width 2480 /Height 3508'))).toEqual({ oversized: false, oversizedMask: false, unverifiable: false })
    // A form XObject is not an image, whatever its bounding box.
    expect(scan('5 0 obj\n<< /Subtype /Form /Width 9000 /Height 9000 /Length 3 >>\nstream\nq Q\nendstream\nendobj\n')).toEqual({
      oversized: false, oversizedMask: false, unverifiable: false,
    })
  })

  it('finds an image whose dictionary straddles the boundary between two reading windows', () => {
    const padding = ' '.repeat((1 << 20) - 40)
    expect(scanPdfImages(pdfWith(padding + imageObject('/Width 6000 /Height 6000'))).oversized).toBe(true)
  })

  it.each([
    ['an A3 page scanned at 300 dpi', '/Width 3508 /Height 4960'],
    ['a 24-megapixel phone photo', '/Width 6000 /Height 4000'],
  ])('still previews %s, leaving the image out', (_, size) => {
    expect(scan(imageObject(size))).toEqual({ oversized: true, oversizedMask: false, unverifiable: false })
  })

  it.each([
    ['/SMask', '/SMask 9 0 R'],
    ['/Mask', '/Mask 9 0 R'],
  ])('refuses a drawn image whose %s is an oversized image', (_, reference) => {
    const mask = imageObject('/Width 12000 /Height 12000 /ColorSpace /DeviceGray', 9)
    expect(scan(imageObject(`/Width 1 /Height 1 ${reference}`), mask)).toEqual({ oversized: true, oversizedMask: true, unverifiable: false })
    // The mask may come first in the file.
    expect(scan(mask, imageObject(`/Width 1 /Height 1 ${reference}`)).oversizedMask).toBe(true)
  })

  it('finds an oversized mask defined in a later reading window', () => {
    const padding = ' '.repeat(3 << 20)
    const bytes = pdfWith(imageObject('/Width 1 /Height 1 /SMask 9 0 R'), padding + imageObject('/Width 12000 /Height 12000', 9))
    expect(scanPdfImages(bytes).oversizedMask).toBe(true)
  })

  it('allows masks within the limit, colour-key masks, and the masks of images pdf.js leaves out', () => {
    expect(scan(imageObject('/Width 800 /Height 600 /SMask 9 0 R'), imageObject('/Width 800 /Height 600', 9)).oversizedMask).toBe(false)
    expect(scan(imageObject('/Width 800 /Height 600 /Mask [0 10 0 10 0 10]')).oversizedMask).toBe(false)
    expect(scan(imageObject('/Width 6000 /Height 4000 /SMask 999 0 R'))).toEqual({
      oversized: true, oversizedMask: false, unverifiable: false,
    })
    // A large transparent photo: pdf.js drops the photo before reading its mask.
    expect(scan(imageObject('/Width 6000 /Height 4000 /SMask 9 0 R'), imageObject('/Width 6000 /Height 4000', 9))).toEqual({
      oversized: true,
      oversizedMask: false,
      unverifiable: false,
    })
  })
})

describe('scanPdfImages on hostile input', () => {
  const sixteenMegabytes = (unit: string) => encoder.encode('%PDF-1.7\n' + unit.repeat(Math.ceil((16 << 20) / unit.length)))
  const timed = (bytes: Uint8Array) => {
    const started = performance.now()
    scanPdfImages(bytes)
    return performance.now() - started
  }

  it('walks a complete 16MB comment without decoding a file-sized string', () => {
    const bytes = pdfWith('20 0 obj\n%'+ 'x'.repeat(16 << 20) + '\nnull endobj', maskParent, largeMask)
    const started = performance.now()
    expect(scanPdfImages(bytes)).toMatchObject({ oversizedMask: true, unverifiable: false })
    const duration = performance.now() - started
    console.info('PDF preflight timing', { case: 'complete 16MB comment', bytes: bytes.length, milliseconds: duration, budgetMilliseconds: 2000 })
    expect(duration).toBeLessThan(2000)
  })

  // A scan that searched back and forth from every image marker took tens of
  // seconds per megabyte of these; a linear one takes milliseconds.
  it.each([
    ['repeated image markers', '/Subtype/Image s'],
    ['image markers after headers, never followed by a stream', '1 0 obj /Subtype/Image '],
    ['tiny image objects referencing masks', '7 0 obj<</Subtype/Image/Width 9/Height 9/SMask 7 0 R>>stream\n'],
    ['long digit runs', `8 0 obj /Subtype/Image /Width ${'9'.repeat(60)} /SMask ${'9'.repeat(60)} 0 R stream `],
  ])('scans 16 MB of %s within a generous bound', (_, unit) => {
    const bytes = sixteenMegabytes(unit)
    const duration = timed(bytes)
    console.info('PDF preflight timing', { case: _, bytes: bytes.length, milliseconds: duration, budgetMilliseconds: 2000 })
    expect(duration).toBeLessThan(2000)
  })

  it('takes time proportional to the file size within a reading window', () => {
    const sized = (length: number) => encoder.encode('%PDF-1.7\n' + '/Subtype/Image s'.repeat(length / 16))
    const samples = (bytes: Uint8Array) => Array.from({ length: 5 }, () => timed(bytes))
    const smallSamples = samples(sized(128 << 10))
    const largeSamples = samples(sized(1 << 20))
    const small = Math.min(...smallSamples)
    const large = Math.min(...largeSamples)
    console.info('PDF preflight scaling', { smallBytes: 128 << 10, largeBytes: 1 << 20, smallMilliseconds: smallSamples, largeMilliseconds: largeSamples })
    // 8 times the bytes: about 8 times the time; a quadratic scan takes 64.
    expect(large / Math.max(small, 0.05)).toBeLessThan(32)
  })
})

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

  it.each(invalidCatalogs)('never admits catalog repair to getDocument: %s', async (_, objects) => {
    await expect(openPdfPreview(new Blob([pdfObjects([...objects])]))).rejects.toMatchObject({ reason: 'unavailable' })
    expect(mocks.getDocument).not.toHaveBeenCalled()
  })

  it('refuses encrypted trailers before worker initialization', async () => {
    const text = new TextDecoder().decode(pdfWith()).replace('/Root 1 0 R', '/Root 1 0 R /Encrypt 8 0 R')
    await expect(openPdfPreview(new Blob([encoder.encode(text)]))).rejects.toMatchObject({ reason: 'unavailable' })
    expect(mocks.getDocument).not.toHaveBeenCalled()
  })

  it('refuses a hidden-object xref target before getDocument can resolve it', async () => {
    await expect(openPdfPreview(new Blob([hiddenMaskPdf(true)]))).rejects.toMatchObject({ reason: 'unavailable' })
    expect(mocks.getDocument).not.toHaveBeenCalled()
  })

  it('never invokes pdf.js repair on a file without a proved table', async () => {
    const bytes = encoder.encode('%PDF-1.7\n(' + maskParent + largeMask + ')\n%%EOF\n')
    await expect(openPdfPreview(new Blob([bytes]))).rejects.toMatchObject({ reason: 'unavailable' })
    expect(mocks.getDocument).not.toHaveBeenCalled()
  })

  it.each(maskVariants)('never hands the worker an oversized mask using %s', async (_, mask) => {
    await expect(openPdfPreview(new Blob([pdfWith(maskParent, mask)]))).rejects.toMatchObject({ reason: 'too-large' })
    expect(mocks.getDocument).not.toHaveBeenCalled()
  })

  it.each(unknownMasks)('never hands the worker an unverified mask: %s', async (_, objects) => {
    await expect(openPdfPreview(new Blob([pdfWith(...objects)]))).rejects.toMatchObject({ reason: 'unavailable' })
    expect(mocks.getDocument).not.toHaveBeenCalled()
  })

  it('refuses swapped-aspect allocation before creating a worker document', async () => {
    const bytes = pdfWith(imageObject('/Width 16384 /Height 1024 /Mask 9 0 R'), imageObject('/Width 1024 /Height 16384 /ImageMask true', 9))
    await expect(openPdfPreview(new Blob([bytes]))).rejects.toMatchObject({ reason: 'too-large' })
    expect(mocks.getDocument).not.toHaveBeenCalled()
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
    const source = pdfWith()
    const preview = await openPdfPreview(new Blob([source], { type: 'application/octet-stream' }))

    expect(mocks.imported).toBe(1)
    // The bundled worker file, served from the app's own origin.
    expect(mocks.workerOptions.workerSrc).toMatch(/pdf\.worker\.min\.mjs$/)
    expect(mocks.workerOptions.workerSrc).not.toMatch(/^(https?:)?\/\/(?!localhost)/)
    expect(mocks.getDocument).toHaveBeenCalledTimes(1)
    const params = mocks.getDocument.mock.calls[0][0]
    expect(params).toMatchObject({ isEvalSupported: false, enableXfa: false, maxImageSize: 16_777_216 })
    expect(params).not.toHaveProperty('url')
    expect(params).not.toHaveProperty('onPassword')
    expect(params.data).toEqual(source)
    expect(preview.pageCount).toBe(2)
    expect(preview.pageSizes).toEqual([{ width: 612, height: 792 }, { width: 612, height: 792 }])
    expect(preview.hasOversizedImages).toBe(false)
  })

  it('tells the preview when pdf.js will leave out an oversized image', async () => {
    mocks.getDocument.mockReturnValueOnce(fakeLoadingTask(Promise.resolve(fakeDocument(1))))
    const bytes = pdfWith(imageObject('/Width 8000 /Height 8000'))
    const preview = await openPdfPreview(new Blob([bytes]))
    expect(preview.hasOversizedImages).toBe(true)
    // The same file still goes to pdf.js, which skips the image itself.
    expect(mocks.getDocument.mock.calls[0][0].maxImageSize).toBe(PDF_PREVIEW_MAX_IMAGE_PIXELS)
  })

  it('refuses a file whose drawn image has an oversized mask before pdf.js sees it', async () => {
    const bytes = pdfWith(imageObject('/Width 1 /Height 1 /SMask 9 0 R'), imageObject('/Width 12000 /Height 12000', 9))
    const error = await openPdfPreview(new Blob([bytes])).catch(caught => caught)
    expect(error).toBeInstanceOf(PdfPreviewError)
    expect(error.reason).toBe('too-large')
    expect(mocks.getDocument).not.toHaveBeenCalled()
  })

  it('points pdf.js at same-origin CMaps, fonts and JavaScript image decoders, never WebAssembly', async () => {
    mocks.getDocument.mockReturnValueOnce(fakeLoadingTask(Promise.resolve(fakeDocument(1))))
    await openPdfPreview(pdfBytes())
    const params = mocks.getDocument.mock.calls[0][0]
    // The dev server serves the package itself; builds copy the same files
    // (pdfjsRuntimeFiles in vite.config.ts).
    expect(params).toMatchObject({
      cMapUrl: '/node_modules/pdfjs-dist/cmaps/',
      standardFontDataUrl: '/node_modules/pdfjs-dist/standard_fonts/',
      wasmUrl: '/node_modules/pdfjs-dist/wasm/',
      useWasm: false,
    })
    expect(params).not.toHaveProperty('cMapPacked', false)
    expect(params).not.toHaveProperty('useWorkerFetch', false)
  })

  it.each([
    'cmaps/UniGB-UTF16-H.bcmap',
    'standard_fonts/FoxitDingbats.pfb',
    'standard_fonts/FoxitSymbol.pfb',
    'wasm/jbig2_nowasm_fallback.js',
    'wasm/openjpeg_nowasm_fallback.js',
  ])('the pinned pdf.js package has the runtime file %s', file => {
    const root = createRequire(import.meta.url).resolve('pdfjs-dist/package.json').replace(/package\.json$/, '')
    expect(readFileSync(root + file).length).toBeGreaterThan(0)
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
