/** @vitest-environment happy-dom */

import { describe, expect, it } from 'vitest'
import {
  MAX_UPLOAD_FILE_BYTES,
  META_MEDIA_LIMIT_BYTES,
  SERVER_UPLOAD_CAP_BYTES,
  effectiveLimitBytes,
  filesFromClipboard,
  formatFileSize,
  inferMimeType,
  kindSupportsCaption,
  validateChatAttachment,
  withInferredMimeType,
} from './chatAttachments'

const MiB = 1024 * 1024

function fileLike(name: string, type: string, size: number) {
  return { name, type, size }
}

describe('validateChatAttachment', () => {
  it('accepts JPEG and PNG photos up to Meta’s 5 MB image limit', () => {
    expect(validateChatAttachment(fileLike('a.jpg', 'image/jpeg', 5 * MiB))).toEqual({
      ok: true,
      kind: 'image',
      mimeType: 'image/jpeg',
      limitBytes: 5 * MiB,
    })
    expect(validateChatAttachment(fileLike('a.png', 'image/png', 1024)).ok).toBe(true)
  })

  it('offers "send as document" for an oversized JPEG/PNG and accepts it that way', () => {
    const big = fileLike('scan.jpg', 'image/jpeg', 8 * MiB)
    const verdict = validateChatAttachment(big)
    expect(verdict).toMatchObject({ ok: false, problem: 'too-large', kind: 'image', canSendAsDocument: true })

    expect(validateChatAttachment(big, { asDocument: true })).toEqual({
      ok: true,
      kind: 'document',
      mimeType: 'image/jpeg',
      limitBytes: MAX_UPLOAD_FILE_BYTES,
    })
  })

  it('rejects HEIC, WebP and GIF photos with no document fallback (Meta refuses those MIME types)', () => {
    for (const [name, type] of [
      ['IMG_0001.HEIC', 'image/heic'],
      ['sticker.webp', 'image/webp'],
      ['anim.gif', 'image/gif'],
    ]) {
      const verdict = validateChatAttachment(fileLike(name, type, 1000))
      expect(verdict).toMatchObject({ ok: false, problem: 'unsupported-image', canSendAsDocument: false })
      // Asking for a document anyway does not bypass Meta's MIME list.
      expect(validateChatAttachment(fileLike(name, type, 1000), { asDocument: true }).ok).toBe(false)
    }
  })

  it('accepts MP4/3GPP video and rejects other containers', () => {
    expect(validateChatAttachment(fileLike('a.mp4', 'video/mp4', MiB)).ok).toBe(true)
    expect(validateChatAttachment(fileLike('a.3gp', 'video/3gpp', MiB)).ok).toBe(true)
    expect(validateChatAttachment(fileLike('a.mov', 'video/quicktime', MiB))).toMatchObject({
      ok: false,
      problem: 'unsupported-video',
    })
  })

  it('caps video and audio at the server’s multipart limit, which is below Meta’s 16 MB', () => {
    expect(META_MEDIA_LIMIT_BYTES.video).toBe(16 * MiB)
    expect(effectiveLimitBytes('video')).toBe(MAX_UPLOAD_FILE_BYTES)
    expect(effectiveLimitBytes('audio')).toBe(MAX_UPLOAD_FILE_BYTES)
    expect(MAX_UPLOAD_FILE_BYTES).toBeLessThan(SERVER_UPLOAD_CAP_BYTES)

    expect(validateChatAttachment(fileLike('v.mp4', 'video/mp4', MAX_UPLOAD_FILE_BYTES)).ok).toBe(true)
    expect(validateChatAttachment(fileLike('v.mp4', 'video/mp4', MAX_UPLOAD_FILE_BYTES + 1))).toMatchObject({
      ok: false,
      problem: 'too-large',
      canSendAsDocument: false,
    })
    expect(validateChatAttachment(fileLike('a.ogg', 'audio/ogg', 15 * MiB)).ok).toBe(false)
  })

  it('accepts documents up to just under the 15 MB server cap', () => {
    expect(validateChatAttachment(fileLike('r.pdf', 'application/pdf', MAX_UPLOAD_FILE_BYTES))).toMatchObject({
      ok: true,
      kind: 'document',
    })
    expect(validateChatAttachment(fileLike('r.pdf', 'application/pdf', SERVER_UPLOAD_CAP_BYTES)).ok).toBe(false)
    for (const type of [
      'application/msword',
      'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
      'application/vnd.ms-excel',
      'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
      'application/vnd.ms-powerpoint',
      'text/plain',
    ]) {
      expect(validateChatAttachment(fileLike('x', type, 10)).ok, type).toBe(true)
    }
  })

  it('rejects unknown types and empty files', () => {
    expect(validateChatAttachment(fileLike('a.zip', 'application/zip', 10))).toMatchObject({
      ok: false,
      problem: 'unsupported-type',
      kind: null,
    })
    expect(validateChatAttachment(fileLike('a.exe', '', 10))).toMatchObject({ ok: false, problem: 'unsupported-type' })
    expect(validateChatAttachment(fileLike('a.pdf', 'application/pdf', 0))).toMatchObject({ ok: false, problem: 'empty' })
  })

  it('falls back to the extension when the browser reports no MIME type', () => {
    expect(inferMimeType({ name: 'Report.DOCX', type: '' })).toBe(
      'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    )
    expect(validateChatAttachment(fileLike('Report.xlsx', '', 100))).toMatchObject({ ok: true, kind: 'document' })
  })
})

describe('withInferredMimeType', () => {
  it('re-types files the browser left untyped so the multipart part has a Content-Type', () => {
    const untyped = new File(['x'], 'notes.txt', { type: '' })
    const typed = withInferredMimeType(untyped)
    expect(typed.type).toBe('text/plain')
    expect(typed.name).toBe('notes.txt')

    const already = new File(['x'], 'a.png', { type: 'image/png' })
    expect(withInferredMimeType(already)).toBe(already)
  })
})

describe('helpers', () => {
  it('formats sizes and knows which kinds take captions', () => {
    expect(formatFileSize(512)).toBe('512 B')
    expect(formatFileSize(1536)).toBe('1.5 KB')
    expect(formatFileSize(5 * MiB)).toBe('5.0 MB')
    expect(kindSupportsCaption('image')).toBe(true)
    expect(kindSupportsCaption('document')).toBe(true)
    expect(kindSupportsCaption('audio')).toBe(false)
    expect(kindSupportsCaption(null)).toBe(false)
  })
})

describe('filesFromClipboard', () => {
  function clipboard(opts: { text?: string; files?: File[]; items?: Array<{ kind: string; getAsFile: () => File | null }> }) {
    return {
      getData: (type: string) => (type === 'text/plain' ? opts.text ?? '' : ''),
      files: opts.files ?? [],
      items: opts.items ?? [],
      types: [],
    } as unknown as DataTransfer
  }

  it('returns pasted files when there is no text', () => {
    const shot = new File(['png'], 'image.png', { type: 'image/png' })
    expect(filesFromClipboard(clipboard({ files: [shot] }))).toEqual([shot])
  })

  it('reads files from items when files is empty', () => {
    const shot = new File(['png'], 'image.png', { type: 'image/png' })
    expect(
      filesFromClipboard(clipboard({ items: [{ kind: 'string', getAsFile: () => null }, { kind: 'file', getAsFile: () => shot }] })),
    ).toEqual([shot])
  })

  it('prefers text when the clipboard holds both (e.g. cells copied from Excel)', () => {
    const render = new File(['png'], 'image.png', { type: 'image/png' })
    expect(filesFromClipboard(clipboard({ text: 'Qty\t2', files: [render] }))).toEqual([])
    expect(filesFromClipboard(null)).toEqual([])
  })
})
