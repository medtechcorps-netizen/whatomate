// Shared validation for chat attachments (composer paperclip, drag-and-drop
// and paste all go through validateChatAttachment).
//
// The WhatsApp Cloud API carries ONE media object per message and only
// accepts specific MIME types: our backend uploads the file to Meta with the
// browser-reported Content-Type, so anything outside Meta's list fails after
// the upload. The server also caps the whole multipart request at 15 MiB
// (cmd/whatomate/main.go MaxRequestBodySize), which is tighter than Meta's
// 16 MB video/audio limit.

export type ChatMediaKind = 'image' | 'video' | 'audio' | 'document'

export const MAX_CHAT_ATTACHMENTS = 10

const MiB = 1024 * 1024

/** fasthttp MaxRequestBodySize for the whole multipart request. */
export const SERVER_UPLOAD_CAP_BYTES = 15 * MiB
/** Room for multipart boundaries, contact_id, type, account and a caption. */
const MULTIPART_OVERHEAD_BYTES = 64 * 1024
/** Largest file the server can accept in one /api/messages/media request. */
export const MAX_UPLOAD_FILE_BYTES = SERVER_UPLOAD_CAP_BYTES - MULTIPART_OVERHEAD_BYTES

/**
 * Meta's per-type limits. Meta's "5 MB" for images is read as 5,000,000
 * bytes: if Meta means 5 MiB, the safe side only costs about 4%.
 */
export const META_MEDIA_LIMIT_BYTES: Record<ChatMediaKind, number> = {
  image: 5_000_000,
  video: 16 * MiB,
  audio: 16 * MiB,
  document: 100 * MiB,
}

/**
 * Largest photo we try to convert in the browser; decoding bigger files can
 * exhaust memory on phones and low-end PCs.
 */
export const MAX_CONVERTIBLE_IMAGE_BYTES = 40 * MiB

/** Meta accepts a caption on image, video and document messages, up to 1024 characters. */
export const MAX_CAPTION_LENGTH = 1024

export const IMAGE_MIME_TYPES = ['image/jpeg', 'image/png'] as const
export const VIDEO_MIME_TYPES = ['video/mp4', 'video/3gpp'] as const
export const DOCUMENT_MIME_TYPES = [
  'application/pdf',
  'application/msword',
  'application/vnd.ms-excel',
  'application/vnd.ms-powerpoint',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  'application/vnd.openxmlformats-officedocument.presentationml.presentation',
  'text/plain',
] as const

/** Value for the composer's file input `accept` attribute. */
export const CHAT_ATTACHMENT_ACCEPT =
  'image/*,video/*,audio/*,.pdf,.doc,.docx,.xls,.xlsx,.ppt,.pptx,.txt'

// Browsers leave File.type empty for some extensions (notably Office files on
// machines without Office installed), so fall back to the extension.
const MIME_BY_EXTENSION: Record<string, string> = {
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  png: 'image/png',
  heic: 'image/heic',
  heif: 'image/heif',
  webp: 'image/webp',
  gif: 'image/gif',
  mp4: 'video/mp4',
  '3gp': 'video/3gpp',
  mov: 'video/quicktime',
  mp3: 'audio/mpeg',
  m4a: 'audio/mp4',
  aac: 'audio/aac',
  amr: 'audio/amr',
  ogg: 'audio/ogg',
  opus: 'audio/ogg',
  pdf: 'application/pdf',
  doc: 'application/msword',
  docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  xls: 'application/vnd.ms-excel',
  xlsx: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  ppt: 'application/vnd.ms-powerpoint',
  pptx: 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
  txt: 'text/plain',
}

export type AttachmentProblem =
  | 'empty'
  | 'too-large'
  | 'unsupported-image'
  | 'unsupported-video'
  | 'unsupported-type'

export type AttachmentValidation =
  | { ok: true; kind: ChatMediaKind; mimeType: string; limitBytes: number }
  | {
      ok: false
      problem: AttachmentProblem
      kind: ChatMediaKind | null
      mimeType: string
      /** Effective size limit for `kind`, when known. */
      limitBytes: number | null
      /**
       * The same bytes would be accepted as a document. Only offered when
       * Meta accepts the MIME type for upload (JPEG/PNG over 5 MB): Meta
       * rejects HEIC, WebP, GIF etc. at upload even as documents.
       */
      canSendAsDocument: boolean
      /**
       * A photo the browser may be able to re-encode as a JPEG under 5 MB:
       * other image formats (HEIC, WebP, GIF…) or an oversized JPEG/PNG.
       */
      canConvertToJpeg: boolean
    }

export interface ValidateAttachmentOptions {
  /** Send the file as a WhatsApp document instead of inline media. */
  asDocument?: boolean
}

function isConvertibleImage(mimeType: string, size: number): boolean {
  return (
    mimeType.startsWith('image/') &&
    mimeType !== 'image/svg+xml' &&
    size > 0 &&
    size <= MAX_CONVERTIBLE_IMAGE_BYTES
  )
}

function extensionOf(name: string): string {
  const dot = name.lastIndexOf('.')
  return dot >= 0 ? name.slice(dot + 1).toLowerCase() : ''
}

/** The file's MIME type, falling back to its extension when the browser gave none. */
export function inferMimeType(file: Pick<File, 'name' | 'type'>): string {
  const reported = (file.type || '').trim().toLowerCase()
  if (reported) return reported
  return MIME_BY_EXTENSION[extensionOf(file.name)] ?? ''
}

/**
 * Returns a File whose `type` is set when the browser left it empty, so the
 * multipart part carries a real Content-Type (the server forwards it to Meta).
 */
export function withInferredMimeType(file: File): File {
  if (file.type) return file
  const mimeType = inferMimeType(file)
  if (!mimeType) return file
  return new File([file], file.name, { type: mimeType, lastModified: file.lastModified })
}

export function effectiveLimitBytes(kind: ChatMediaKind): number {
  return Math.min(META_MEDIA_LIMIT_BYTES[kind], MAX_UPLOAD_FILE_BYTES)
}

function classify(mimeType: string): { kind: ChatMediaKind | null; supported: boolean } {
  if (mimeType.startsWith('image/')) {
    return { kind: 'image', supported: (IMAGE_MIME_TYPES as readonly string[]).includes(mimeType) }
  }
  if (mimeType.startsWith('video/')) {
    return { kind: 'video', supported: (VIDEO_MIME_TYPES as readonly string[]).includes(mimeType) }
  }
  if (mimeType.startsWith('audio/')) {
    // The composer has always accepted any audio/* file; Meta transcodes
    // nothing, so unusual codecs still fail at Meta, as before.
    return { kind: 'audio', supported: true }
  }
  // Office files are matched by prefix like the previous composer did, so
  // macro-enabled variants (…sheet.macroEnabled) are not newly rejected.
  const isDocument =
    (DOCUMENT_MIME_TYPES as readonly string[]).includes(mimeType) ||
    mimeType.startsWith('application/vnd.openxmlformats-officedocument')
  return { kind: isDocument ? 'document' : null, supported: isDocument }
}

export function validateChatAttachment(
  file: Pick<File, 'name' | 'type' | 'size'>,
  options: ValidateAttachmentOptions = {},
): AttachmentValidation {
  const mimeType = inferMimeType(file)
  const { kind: detectedKind, supported } = classify(mimeType)

  if (options.asDocument) {
    // Only JPEG/PNG images are offered "send as document" (see
    // canSendAsDocument); anything else keeps its original verdict.
    const documentable =
      detectedKind === 'document' ||
      (detectedKind === 'image' && supported)
    if (documentable) {
      const limitBytes = effectiveLimitBytes('document')
      if (file.size <= 0) {
        return { ok: false, problem: 'empty', kind: 'document', mimeType, limitBytes, canSendAsDocument: false, canConvertToJpeg: false }
      }
      if (file.size > limitBytes) {
        return {
          ok: false,
          problem: 'too-large',
          kind: 'document',
          mimeType,
          limitBytes,
          canSendAsDocument: false,
          canConvertToJpeg: isConvertibleImage(mimeType, file.size),
        }
      }
      return { ok: true, kind: 'document', mimeType, limitBytes }
    }
  }

  if (!detectedKind || !supported) {
    const problem: AttachmentProblem =
      detectedKind === 'image'
        ? 'unsupported-image'
        : detectedKind === 'video'
          ? 'unsupported-video'
          : 'unsupported-type'
    return {
      ok: false,
      problem,
      kind: detectedKind,
      mimeType,
      limitBytes: null,
      canSendAsDocument: false,
      canConvertToJpeg: detectedKind === 'image' && isConvertibleImage(mimeType, file.size),
    }
  }

  const limitBytes = effectiveLimitBytes(detectedKind)
  if (file.size <= 0) {
    return { ok: false, problem: 'empty', kind: detectedKind, mimeType, limitBytes, canSendAsDocument: false, canConvertToJpeg: false }
  }
  if (file.size > limitBytes) {
    return {
      ok: false,
      problem: 'too-large',
      kind: detectedKind,
      mimeType,
      limitBytes,
      canSendAsDocument:
        detectedKind === 'image' && file.size <= effectiveLimitBytes('document'),
      canConvertToJpeg: detectedKind === 'image' && isConvertibleImage(mimeType, file.size),
    }
  }
  return { ok: true, kind: detectedKind, mimeType, limitBytes }
}

/** Audio messages carry no caption on WhatsApp. */
export function kindSupportsCaption(kind: ChatMediaKind | null): boolean {
  return kind === 'image' || kind === 'video' || kind === 'document'
}

/** Human-readable size, e.g. "4.2 MB" (binary units, matching the limits). */
export function formatFileSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return ''
  if (bytes < 1024) return `${bytes} B`
  if (bytes < MiB) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / MiB).toFixed(1)} MB`
}

/** True when a drag or clipboard payload carries files. */
export function dataTransferHasFiles(dataTransfer: DataTransfer | null | undefined): boolean {
  if (!dataTransfer) return false
  return Array.from(dataTransfer.types ?? []).includes('Files')
}

/**
 * Files to attach from a paste. Returns [] when the clipboard also holds
 * plain text: copying cells from Excel or Word puts both text and a rendered
 * image on the clipboard, and the agent expects the text.
 */
export function filesFromClipboard(clipboard: DataTransfer | null | undefined): File[] {
  if (!clipboard) return []
  let files = Array.from(clipboard.files ?? [])
  if (files.length === 0) {
    // Some browsers expose pasted screenshots only through items.
    for (const item of Array.from(clipboard.items ?? [])) {
      if (item.kind !== 'file') continue
      const file = item.getAsFile()
      if (file) files.push(file)
    }
  }
  if (files.length === 0) return []
  // Files win (macOS Finder puts the file name in text/plain next to the
  // file), except for a rich-text copy: Excel/Word put text, HTML and a
  // rendered image of the selection on the clipboard, and the agent wants
  // the text.
  const text = clipboard.getData?.('text/plain') ?? ''
  const html = clipboard.getData?.('text/html') ?? ''
  const richTextCopy = !!text.trim() && !!html.trim() && files.every(file => file.type.startsWith('image/'))
  files = richTextCopy ? [] : files
  return files
}
