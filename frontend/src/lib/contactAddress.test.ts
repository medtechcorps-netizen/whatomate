import { describe, expect, it } from 'vitest'
import { contactAddressDisplay, contactDisplayName, isPlaceholderPhone } from './contactAddress'

const HIDDEN_HINT = "WhatsApp did not share this customer's number (they message with a WhatsApp username)."

describe('contactAddressDisplay', () => {
  it('shows a real phone number', () => {
    expect(contactAddressDisplay({ phone_number: '60000000000' })).toEqual({
      kind: 'phone',
      text: '60000000000',
      hint: '',
    })
  })

  it('shows a real phone number on other channels too', () => {
    expect(contactAddressDisplay({ phone_number: '60000000000' }, { channel: 'instagram' }).kind).toBe('phone')
  })

  it.each(['bsuid:abc', 'user:abc', 'event:abc', 'id:abc'])('hides the %s placeholder for WhatsApp', (phone) => {
    expect(contactAddressDisplay({ phone_number: phone }, { channel: 'whatsapp' })).toEqual({
      kind: 'whatsapp_hidden',
      text: 'WhatsApp number hidden',
      hint: HIDDEN_HINT,
    })
  })

  it('treats the coexistence metadata flags as hidden numbers when the number is masked', () => {
    expect(
      contactAddressDisplay({ phone_number: '6010****000', metadata: { coexistence_phone_placeholder: true } }).kind,
    ).toBe('whatsapp_hidden')
    expect(
      contactAddressDisplay({ phone_number: '6010****000', metadata: { coexistence_phone_unavailable: true } }).kind,
    ).toBe('whatsapp_hidden')
    expect(contactAddressDisplay({ phone_number: '', metadata: { coexistence_phone_unavailable: true } }).kind).toBe(
      'whatsapp_hidden',
    )
    expect(
      contactAddressDisplay({ phone_number: 'bsuid:abc', metadata: { coexistence_phone_placeholder: true } }).kind,
    ).toBe('whatsapp_hidden')
  })

  it('shows a plain phone number even when a stale coexistence flag is set', () => {
    expect(
      contactAddressDisplay({ phone_number: '60000000000', metadata: { coexistence_phone_placeholder: true } }),
    ).toEqual({ kind: 'phone', text: '60000000000', hint: '' })
    expect(
      contactAddressDisplay({ phone_number: '60000000000', metadata: { coexistence_phone_unavailable: true } }).kind,
    ).toBe('phone')
    expect(
      contactAddressDisplay({ phone_number: '60000000000', metadata: { coexistence_phone_unavailable: false } }).kind,
    ).toBe('phone')
  })

  it('keeps a masked number without a flag as a phone', () => {
    expect(contactAddressDisplay({ phone_number: '6010****000' }).kind).toBe('phone')
  })

  it('appends the WhatsApp username when known', () => {
    expect(
      contactAddressDisplay({ phone_number: 'bsuid:abc', metadata: { coexistence_username: 'test_user' } }).text,
    ).toBe('WhatsApp number hidden (@test_user)')
    expect(
      contactAddressDisplay({ phone_number: 'bsuid:abc', metadata: { coexistence_username: '   ' } }).text,
    ).toBe('WhatsApp number hidden')
    expect(contactAddressDisplay({ phone_number: 'bsuid:abc', metadata: { coexistence_username: 42 } }).text).toBe(
      'WhatsApp number hidden',
    )
  })

  it('assumes WhatsApp when the channel is missing', () => {
    expect(contactAddressDisplay({ phone_number: '' }).kind).toBe('whatsapp_hidden')
  })

  it('uses WhatsApp when the contact has a WhatsApp account even on another channel', () => {
    expect(
      contactAddressDisplay({ phone_number: 'user:abc', whatsapp_account: 'account-1' }, { channel: 'instagram' })
        .kind,
    ).toBe('whatsapp_hidden')
  })

  it('names the other channel when there is no phone number', () => {
    expect(contactAddressDisplay({ phone_number: 'id:123' }, { channel: 'instagram' })).toEqual({
      kind: 'channel_id',
      text: 'Instagram customer',
      hint: 'This customer contacted you on Instagram; no phone number is linked.',
    })
    expect(contactAddressDisplay({ phone_number: null }, { channel: 'Messenger' }).text).toBe('Messenger customer')
    expect(contactAddressDisplay({}, { channel: 'tiktok' }).text).toBe('TikTok customer')
    expect(contactAddressDisplay({}, { channel: 'webchat' }).text).toBe('Website chat customer')
    expect(contactAddressDisplay({}, { channel: 'line' }).text).toBe('Line customer')
  })

  it('returns none without a contact', () => {
    expect(contactAddressDisplay(null)).toEqual({ kind: 'none', text: 'No phone number', hint: '' })
    expect(contactAddressDisplay(undefined)).toEqual({ kind: 'none', text: 'No phone number', hint: '' })
  })
})

describe('isPlaceholderPhone', () => {
  it('detects placeholder prefixes', () => {
    expect(isPlaceholderPhone('bsuid:1')).toBe(true)
    expect(isPlaceholderPhone(' user:1')).toBe(true)
    expect(isPlaceholderPhone('60000000000')).toBe(false)
    expect(isPlaceholderPhone(null)).toBe(false)
  })
})

describe('contactDisplayName', () => {
  it('prefers the trimmed profile name, then the saved name', () => {
    expect(contactDisplayName({ profile_name: '  Test Profile ', name: 'Test Name' })).toBe('Test Profile')
    expect(contactDisplayName({ profile_name: '   ', name: ' Test Name ' })).toBe('Test Name')
    expect(contactDisplayName({ profile_name: null, name: 'Test Name', phone_number: '60000000000' })).toBe(
      'Test Name',
    )
  })

  it('falls back to the phone number', () => {
    expect(contactDisplayName({ name: '', phone_number: ' 60000000000 ' })).toBe('60000000000')
  })

  it('shows a WhatsApp user when the number is hidden', () => {
    expect(contactDisplayName({ phone_number: 'bsuid:abc' })).toBe('WhatsApp user')
    expect(contactDisplayName({ phone_number: 'user:abc' }, { channel: 'whatsapp' })).toBe('WhatsApp user')
    expect(
      contactDisplayName({ phone_number: '6010****000', metadata: { coexistence_phone_unavailable: true } }),
    ).toBe('WhatsApp user')
  })

  it('names the channel for other channels', () => {
    expect(contactDisplayName({ phone_number: 'id:123' }, { channel: 'instagram' })).toBe('Instagram customer')
    expect(contactDisplayName({}, { channel: 'webchat' })).toBe('Website chat customer')
  })

  it('never returns a raw placeholder, even when stored as the name', () => {
    for (const placeholder of ['bsuid:abc', 'user:abc', 'event:abc', 'id:abc', 'BSUID:abc']) {
      const result = contactDisplayName({ name: placeholder, profile_name: placeholder, phone_number: placeholder })
      expect(result).toBe('WhatsApp user')
      expect(result.toLowerCase()).not.toMatch(/^(bsuid|user|event|id):/)
    }
  })

  it('returns Customer without a contact', () => {
    expect(contactDisplayName(null)).toBe('Customer')
    expect(contactDisplayName(undefined)).toBe('Customer')
  })
})
