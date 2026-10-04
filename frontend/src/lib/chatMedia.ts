import { createLoadLimiter } from './loadLimiter'

export type ChatMediaKind = 'image' | 'sticker' | 'video' | 'audio' | 'document'

// Chat media is shown from object URLs. An object URL has this app's origin
// but none of the CSP or nosniff headers the server sends with /api/media, and
// the browser renders it by the blob's type when an agent uses "Open image in
// new tab" or "Open link in new tab". A customer's HTML, SVG or XML file would
// then run script in the CRM's origin. So only types that render inertly keep
// their type; anything else, and every document, becomes an opaque download.
// "+xml" types are excluded on purpose: browsers render them as XML documents.
export const INLINE_IMAGE_TYPES: ReadonlySet<string> = new Set([
  'image/jpeg',
  'image/png',
  'image/gif',
  'image/webp',
  'image/avif',
  'image/bmp',
])

const INLINE_VIDEO_TYPES: ReadonlySet<string> = new Set([
  'video/mp4',
  'video/3gpp',
  'video/3gpp2',
  'video/webm',
  'video/ogg',
  'video/quicktime',
  'video/mpeg',
  'video/x-m4v',
])

const INLINE_AUDIO_TYPES: ReadonlySet<string> = new Set([
  'audio/aac',
  'audio/mp4',
  'audio/mpeg',
  'audio/mp3',
  'audio/amr',
  'audio/ogg',
  'audio/opus',
  'audio/wav',
  'audio/x-wav',
  'audio/wave',
  'audio/webm',
  'audio/x-m4a',
  'audio/3gpp',
  'audio/flac',
])

export const OPAQUE_MEDIA_TYPE = 'application/octet-stream'

export function displaySafeMediaType(type: string, kind: ChatMediaKind): string {
  // Parameters are dropped too, so nothing after the essence can change how
  // the type is parsed when the object URL is opened.
  const essence = type.split(';', 1)[0].trim().toLowerCase()
  const inlineTypes =
    kind === 'image' || kind === 'sticker'
      ? INLINE_IMAGE_TYPES
      : kind === 'video'
        ? INLINE_VIDEO_TYPES
        : kind === 'audio'
          ? INLINE_AUDIO_TYPES
          : null
  return inlineTypes?.has(essence) ? essence : OPAQUE_MEDIA_TYPE
}

export function displaySafeMediaBlob(blob: Blob, kind: ChatMediaKind): Blob {
  return new Blob([blob], { type: displaySafeMediaType(blob.type, kind) })
}

// An opaque download carries no type the browser could name the saved file
// by, so a file without a name of its own gets its extension here. Scriptable
// types get none.
const DOWNLOAD_EXTENSIONS: Readonly<Record<string, string>> = {
  'image/jpeg': '.jpg',
  'image/png': '.png',
  'image/gif': '.gif',
  'image/webp': '.webp',
  'image/avif': '.avif',
  'image/bmp': '.bmp',
  'video/mp4': '.mp4',
  'video/3gpp': '.3gp',
  'video/webm': '.webm',
  'video/quicktime': '.mov',
  'audio/aac': '.aac',
  'audio/amr': '.amr',
  'audio/mp4': '.m4a',
  'audio/mpeg': '.mp3',
  'audio/ogg': '.ogg',
  'audio/opus': '.opus',
  'audio/wav': '.wav',
  'audio/webm': '.weba',
  'application/pdf': '.pdf',
}

export function mediaDownloadName(filename: string | undefined, sourceType: string, fallbackBase: string): string {
  const name = filename?.trim()
  if (name) return name
  const essence = sourceType.split(';', 1)[0].trim().toLowerCase()
  return fallbackBase + (DOWNLOAD_EXTENSIONS[essence] ?? '')
}

// Every chat media download shares these slots, so a long transcript cannot
// fill the connection that other API calls need. A transcript opens at its
// newest message, so the newest media loads first.
export const chatMediaLoadLimiter = createLoadLimiter(4, { newestFirst: true })
