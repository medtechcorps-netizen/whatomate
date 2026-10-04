import { describe, expect, it } from 'vitest'
import { displaySafeMediaBlob, displaySafeMediaType, mediaDownloadName, OPAQUE_MEDIA_TYPE } from './chatMedia'

describe('displaySafeMediaType', () => {
  it.each([
    ['image/jpeg', 'image', 'image/jpeg'],
    ['image/png', 'sticker', 'image/png'],
    ['image/webp', 'sticker', 'image/webp'],
    ['IMAGE/PNG; name="photo.png"', 'image', 'image/png'],
    ['video/mp4', 'video', 'video/mp4'],
    ['video/3gpp', 'video', 'video/3gpp'],
    ['audio/ogg; codecs=opus', 'audio', 'audio/ogg'],
    ['audio/mpeg', 'audio', 'audio/mpeg'],
  ] as const)('keeps %s for %s media, which renders inertly', (type, kind, expected) => {
    expect(displaySafeMediaType(type, kind)).toBe(expected)
  })

  it.each([
    // Scriptable when opened as a document in this app's origin.
    ['text/html', 'document'],
    ['text/html', 'image'],
    ['image/svg+xml', 'image'],
    ['image/svg+xml', 'sticker'],
    ['application/xhtml+xml', 'document'],
    ['text/xml', 'document'],
    ['video/x-custom+xml', 'video'],
    ['audio/x-custom+xml', 'audio'],
    // Never rendered inline, so always downloaded.
    ['application/pdf', 'document'],
    ['image/jpeg', 'document'],
    // A type that does not match the bubble it is shown in.
    ['video/mp4', 'image'],
    ['image/png', 'video'],
    ['', 'image'],
  ] as const)('turns %s for %s media into an opaque download', (type, kind) => {
    expect(displaySafeMediaType(type, kind)).toBe(OPAQUE_MEDIA_TYPE)
  })
})

describe('displaySafeMediaBlob', () => {
  it('keeps the bytes and replaces only the type', async () => {
    const html = '<script>window.parent.compromised = true</script>'
    const blob = displaySafeMediaBlob(new Blob([html], { type: 'text/html' }), 'document')
    expect(blob.type).toBe(OPAQUE_MEDIA_TYPE)
    expect(await blob.text()).toBe(html)
  })
})

describe('mediaDownloadName', () => {
  it('keeps the name the file arrived with', () => {
    expect(mediaDownloadName(' lab-report.pdf ', 'application/pdf', 'media')).toBe('lab-report.pdf')
  })

  it.each([
    ['image/jpeg', 'media.jpg'],
    ['IMAGE/PNG; name="photo.png"', 'media.png'],
    ['audio/ogg; codecs=opus', 'media.ogg'],
    ['application/pdf', 'media.pdf'],
    // Scriptable or unknown types get no extension that would open them
    // in a browser.
    ['image/svg+xml', 'media'],
    ['text/html', 'media'],
    ['', 'media'],
  ])('names an unnamed %s file %s', (type, expected) => {
    expect(mediaDownloadName(undefined, type, 'media')).toBe(expected)
    expect(mediaDownloadName('  ', type, 'media')).toBe(expected)
  })
})
