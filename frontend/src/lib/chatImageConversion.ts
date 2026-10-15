// In-browser conversion of photos WhatsApp will not take (HEIC, WebP, GIF,
// BMP, AVIF, or JPEG/PNG over 5 MB) into a JPEG under Meta's image limit.
// Decoding uses the browser's own codecs, so HEIC only converts where the
// browser can display it (Safari); elsewhere conversion fails cleanly.

import { effectiveLimitBytes } from '@/lib/chatAttachments'

export interface DecodedImage {
  width: number
  height: number
  draw: (context: CanvasRenderingContext2D, width: number, height: number) => void
  close?: () => void
}

export interface ImageCodec {
  decode: (file: File) => Promise<DecodedImage>
  encode: (image: DecodedImage, width: number, height: number, quality: number) => Promise<Blob>
}

export type ImageConversionFailure = 'decode' | 'encode' | 'too-large'

export class ImageConversionError extends Error {
  constructor(readonly reason: ImageConversionFailure) {
    super(`Image conversion failed: ${reason}`)
    this.name = 'ImageConversionError'
  }
}

/** Longest edge kept; also stays inside iOS Safari's 16.7 MP canvas limit. */
export const MAX_CONVERTED_EDGE = 4096
const QUALITY_STEPS = [0.92, 0.85, 0.75, 0.65]
const SCALE_STEPS = [1, 0.75, 0.5, 0.35]
/**
 * Dropping quality from 0.92 to 0.65 roughly halves a JPEG; past this ratio
 * only a smaller size can fit, so skip the remaining quality steps.
 */
const QUALITY_CAN_FIT_RATIO = 2

function jpegFileName(name: string): string {
  const dot = name.lastIndexOf('.')
  const base = dot > 0 ? name.slice(0, dot) : name || 'image'
  return `${base}.jpg`
}

async function decodeWithImageElement(file: File): Promise<DecodedImage> {
  const url = URL.createObjectURL(file)
  try {
    const image = new Image()
    image.decoding = 'async'
    image.src = url
    await image.decode()
    if (!image.naturalWidth || !image.naturalHeight) throw new ImageConversionError('decode')
    return {
      width: image.naturalWidth,
      height: image.naturalHeight,
      draw: (context, width, height) => context.drawImage(image, 0, 0, width, height),
      close: () => URL.revokeObjectURL(url),
    }
  } catch {
    URL.revokeObjectURL(url)
    throw new ImageConversionError('decode')
  }
}

export const browserImageCodec: ImageCodec = {
  async decode(file) {
    if (typeof createImageBitmap === 'function') {
      try {
        // Applies EXIF orientation, so rotated phone photos stay upright.
        const bitmap = await createImageBitmap(file)
        return {
          width: bitmap.width,
          height: bitmap.height,
          draw: (context, width, height) => context.drawImage(bitmap, 0, 0, width, height),
          close: () => bitmap.close(),
        }
      } catch {
        // Safari decodes HEIC in <img> but not always in createImageBitmap.
      }
    }
    return decodeWithImageElement(file)
  },
  encode(image, width, height, quality) {
    return new Promise((resolve, reject) => {
      const canvas = document.createElement('canvas')
      canvas.width = width
      canvas.height = height
      const context = canvas.getContext('2d')
      if (!context) {
        reject(new ImageConversionError('encode'))
        return
      }
      // JPEG has no transparency: flatten onto white, not black.
      context.fillStyle = '#ffffff'
      context.fillRect(0, 0, width, height)
      image.draw(context, width, height)
      canvas.toBlob(
        blob => (blob ? resolve(blob) : reject(new ImageConversionError('encode'))),
        'image/jpeg',
        quality,
      )
    })
  },
}

export interface ConvertImageOptions {
  /** Defaults to Meta's image limit. */
  maxBytes?: number
  codec?: ImageCodec
}

/**
 * Re-encodes an image as JPEG, lowering quality and then size until it fits.
 * Animated images keep their first frame.
 */
export async function convertImageToJpeg(file: File, options: ConvertImageOptions = {}): Promise<File> {
  const codec = options.codec ?? browserImageCodec
  const maxBytes = options.maxBytes ?? effectiveLimitBytes('image')
  let image: DecodedImage
  try {
    image = await codec.decode(file)
  } catch (error) {
    throw error instanceof ImageConversionError ? error : new ImageConversionError('decode')
  }
  try {
    const fit = Math.min(1, MAX_CONVERTED_EDGE / Math.max(image.width, image.height, 1))
    for (const scale of SCALE_STEPS) {
      const width = Math.max(1, Math.round(image.width * fit * scale))
      const height = Math.max(1, Math.round(image.height * fit * scale))
      for (const quality of QUALITY_STEPS) {
        const blob = await codec.encode(image, width, height, quality)
        if (blob.size > 0 && blob.size <= maxBytes) {
          return new File([blob], jpegFileName(file.name), { type: 'image/jpeg', lastModified: Date.now() })
        }
        if (blob.size > maxBytes * QUALITY_CAN_FIT_RATIO) break
      }
    }
    throw new ImageConversionError('too-large')
  } finally {
    image.close?.()
  }
}
