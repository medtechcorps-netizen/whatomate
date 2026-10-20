/** @vitest-environment happy-dom */

import { describe, expect, it, vi } from 'vitest'
import {
  ImageConversionError,
  MAX_CONVERTED_EDGE,
  convertImageToJpeg,
  type DecodedImage,
  type ImageCodec,
} from './chatImageConversion'

const MiB = 1024 * 1024

/** Fake codec: encoded size = pixels * bytesPerPixel(quality). */
function fakeCodec(width: number, height: number, bytesPerPixel: (quality: number) => number) {
  const close = vi.fn()
  const encodes: Array<{ width: number; height: number; quality: number }> = []
  const codec: ImageCodec = {
    decode: vi.fn(async () => ({ width, height, draw: vi.fn(), close }) satisfies DecodedImage),
    encode: vi.fn(async (_image, w, h, quality) => {
      encodes.push({ width: w, height: h, quality })
      const size = Math.round(w * h * bytesPerPixel(quality))
      const blob = new Blob(['x'], { type: 'image/jpeg' })
      Object.defineProperty(blob, 'size', { value: size })
      return blob
    }),
  }
  return { codec, encodes, close }
}

const heic = () => new File(['heic'], 'IMG_4421.HEIC', { type: 'image/heic' })

describe('convertImageToJpeg', () => {
  it('returns a JPEG at the first quality that fits under 5 MB', async () => {
    const { codec, encodes, close } = fakeCodec(3000, 2000, () => 0.5)
    const result = await convertImageToJpeg(heic(), { codec })
    expect(result.type).toBe('image/jpeg')
    expect(result.name).toBe('IMG_4421.jpg')
    expect(encodes).toEqual([{ width: 3000, height: 2000, quality: 0.92 }])
    expect(close).toHaveBeenCalledTimes(1)
  })

  it('lowers quality, then size, until the photo fits', async () => {
    // 4000x3000 at 0.5 B/px is 6 MB; quality 0.65 brings it to ~3.9 MB.
    const { codec, encodes } = fakeCodec(4000, 3000, quality => (quality > 0.7 ? 0.5 : 0.33))
    await convertImageToJpeg(new File(['x'], 'scan.jpg', { type: 'image/jpeg' }), { codec })
    expect(encodes.map(e => e.quality)).toEqual([0.92, 0.85, 0.75, 0.65])
    expect(encodes.every(e => e.width === 4000)).toBe(true)

    const shrinking = fakeCodec(4000, 3000, () => 0.6)
    await convertImageToJpeg(heic(), { codec: shrinking.codec })
    const last = shrinking.encodes[shrinking.encodes.length - 1]
    expect(last.width).toBe(3000)
    expect(last.height).toBe(2250)
  })

  it('goes straight to a smaller size when lowering quality cannot fit', async () => {
    // 4000x3000 at 1 B/px is 12 MB, well over twice the limit.
    const { codec, encodes } = fakeCodec(4000, 3000, quality => (quality > 0.9 ? 1 : 0.8))
    const result = await convertImageToJpeg(heic(), { codec })
    expect(encodes[0]).toMatchObject({ width: 4000, quality: 0.92 })
    expect(encodes[1]).toMatchObject({ width: 3000, quality: 0.92 })
    expect(result.size).toBeLessThanOrEqual(5_000_000)
  })

  it('caps the longest edge at 4096 px', async () => {
    const { codec, encodes } = fakeCodec(8192, 4096, () => 0.01)
    await convertImageToJpeg(heic(), { codec })
    expect(encodes[0]).toMatchObject({ width: MAX_CONVERTED_EDGE, height: 2048 })
  })

  it('gives up with "too-large" when nothing fits and still releases the image', async () => {
    const { codec, close } = fakeCodec(2000, 2000, () => 50)
    await expect(convertImageToJpeg(heic(), { codec })).rejects.toMatchObject({ reason: 'too-large' })
    expect(close).toHaveBeenCalledTimes(1)
  })

  it('reports images the browser cannot decode', async () => {
    const codec: ImageCodec = {
      decode: async () => {
        throw new Error('unsupported')
      },
      encode: vi.fn(),
    }
    const error = await convertImageToJpeg(heic(), { codec }).catch(e => e)
    expect(error).toBeInstanceOf(ImageConversionError)
    expect(error.reason).toBe('decode')
    expect(codec.encode).not.toHaveBeenCalled()
  })

  it('respects a custom size limit', async () => {
    const { codec, encodes } = fakeCodec(1000, 1000, () => 1.5)
    await convertImageToJpeg(heic(), { codec, maxBytes: 2 * MiB })
    expect(encodes).toHaveLength(1)
  })
})
