import { describe, expect, it } from 'vitest'
import {
  formatKindForShortcut,
  insertTextAtSelection,
  parseWhatsAppFormat,
  stripWhatsAppMarkers,
  toggleWhatsAppFormat,
} from './whatsappFormat'

describe('parseWhatsAppFormat', () => {
  it('parses the four inline styles and monospace blocks', () => {
    expect(parseWhatsAppFormat('*bold* _it_ ~gone~ `x = 1`')).toEqual([
      { type: 'bold', children: [{ type: 'text', text: 'bold' }] },
      { type: 'text', text: ' ' },
      { type: 'italic', children: [{ type: 'text', text: 'it' }] },
      { type: 'text', text: ' ' },
      { type: 'strike', children: [{ type: 'text', text: 'gone' }] },
      { type: 'text', text: ' ' },
      { type: 'code', text: 'x = 1' },
    ])
    expect(parseWhatsAppFormat('Run ```npm ci\nnpm test``` now')).toEqual([
      { type: 'text', text: 'Run ' },
      { type: 'mono', text: 'npm ci\nnpm test' },
      { type: 'text', text: ' now' },
    ])
  })

  it('nests different markers', () => {
    expect(parseWhatsAppFormat('*_both_*')).toEqual([
      { type: 'bold', children: [{ type: 'italic', children: [{ type: 'text', text: 'both' }] }] },
    ])
  })

  it('requires markers to hug non-space text at word boundaries', () => {
    const plain = (text: string) => expect(parseWhatsAppFormat(text)).toEqual([{ type: 'text', text }])
    plain('2*3*4')
    plain('snake_case_name')
    plain('* not bold *')
    plain('a * b * c')
    plain('**')
    plain('price*2')
    plain('see https://x.test/a_b_c')
  })

  it('accepts punctuation around markers', () => {
    expect(parseWhatsAppFormat('(*yes*).')).toEqual([
      { type: 'text', text: '(' },
      { type: 'bold', children: [{ type: 'text', text: 'yes' }] },
      { type: 'text', text: ').' },
    ])
  })

  it('handles non-Latin scripts as word characters', () => {
    expect(parseWhatsAppFormat('*ubat* sebelum makan')).toEqual([
      { type: 'bold', children: [{ type: 'text', text: 'ubat' }] },
      { type: 'text', text: ' sebelum makan' },
    ])
    expect(parseWhatsAppFormat('药*物*')).toEqual([{ type: 'text', text: '药*物*' }])
  })

  it('does not let inline markers span lines', () => {
    expect(parseWhatsAppFormat('*one\ntwo*')).toEqual([{ type: 'text', text: '*one\ntwo*' }])
  })

  it('leaves lists and quotes as plain text', () => {
    const text = '* Panadol\n- Vitamin C\n1. Rest\n> quoted'
    expect(parseWhatsAppFormat(text)).toEqual([{ type: 'text', text }])
  })

  it('does not parse markers inside code', () => {
    expect(parseWhatsAppFormat('`*not bold*`')).toEqual([{ type: 'code', text: '*not bold*' }])
    expect(parseWhatsAppFormat('```*raw* _x_```')).toEqual([{ type: 'mono', text: '*raw* _x_' }])
  })

  it('treats empty or whitespace fences as text', () => {
    expect(parseWhatsAppFormat('``````')).toEqual([{ type: 'text', text: '``````' }])
    expect(parseWhatsAppFormat('``` ``` *b*')).toEqual([
      { type: 'text', text: '``` ``` ' },
      { type: 'bold', children: [{ type: 'text', text: 'b' }] },
    ])
  })

  it('returns no nodes for empty input', () => {
    expect(parseWhatsAppFormat('')).toEqual([])
    expect(parseWhatsAppFormat(null)).toEqual([])
  })
})

describe('stripWhatsAppMarkers', () => {
  it('removes formatting markers but keeps literal ones', () => {
    expect(stripWhatsAppMarkers('*Reminder*: take _2 tablets_ ~daily~ `now`')).toBe(
      'Reminder: take 2 tablets daily now',
    )
    expect(stripWhatsAppMarkers('```code```')).toBe('code')
    expect(stripWhatsAppMarkers('2*3*4 and snake_case')).toBe('2*3*4 and snake_case')
    expect(stripWhatsAppMarkers(undefined)).toBe('')
  })
})

describe('toggleWhatsAppFormat', () => {
  it('wraps the selection and keeps the selection on the text', () => {
    expect(toggleWhatsAppFormat('take this now', 5, 9, 'bold')).toEqual({
      value: 'take *this* now',
      selectionStart: 6,
      selectionEnd: 10,
    })
  })

  it('keeps surrounding spaces outside the markers', () => {
    expect(toggleWhatsAppFormat('take this now', 4, 10, 'italic').value).toBe('take _this_ now')
  })

  it('nests a second style around the same selection', () => {
    const bold = toggleWhatsAppFormat('hi', 0, 2, 'bold')
    const both = toggleWhatsAppFormat(bold.value, bold.selectionStart, bold.selectionEnd, 'italic')
    expect(both.value).toBe('*_hi_*')
    expect(parseWhatsAppFormat(both.value)[0]).toMatchObject({ type: 'bold' })
  })

  it('unwraps when the markers are just outside or inside the selection', () => {
    expect(toggleWhatsAppFormat('a *hi* b', 3, 5, 'bold')).toEqual({ value: 'a hi b', selectionStart: 2, selectionEnd: 4 })
    expect(toggleWhatsAppFormat('a *hi* b', 2, 6, 'bold')).toEqual({ value: 'a hi b', selectionStart: 2, selectionEnd: 4 })
    expect(toggleWhatsAppFormat('x ```y``` z', 5, 6, 'mono').value).toBe('x y z')
  })

  it('does not treat underscores inside words as formatting to remove', () => {
    expect(toggleWhatsAppFormat('snake_hi_case', 6, 8, 'italic').value).toBe('snake__hi__case')
  })

  it('inserts an empty pair at a caret and removes it again', () => {
    const inserted = toggleWhatsAppFormat('ab', 1, 1, 'strike')
    expect(inserted).toEqual({ value: 'a~~b', selectionStart: 2, selectionEnd: 2 })
    expect(toggleWhatsAppFormat(inserted.value, 2, 2, 'strike')).toEqual({ value: 'ab', selectionStart: 1, selectionEnd: 1 })
    expect(toggleWhatsAppFormat('', 0, 0, 'mono')).toEqual({ value: '``````', selectionStart: 3, selectionEnd: 3 })
  })

  it('wraps each selected line for inline styles and the whole block for monospace', () => {
    expect(toggleWhatsAppFormat('one\n\ntwo', 0, 8, 'bold').value).toBe('*one*\n\n*two*')
    expect(toggleWhatsAppFormat('*one*\n*two*', 0, 11, 'bold').value).toBe('one\ntwo')
    expect(toggleWhatsAppFormat('one\ntwo', 0, 7, 'mono').value).toBe('```one\ntwo```')
  })

  it('ignores whitespace-only selections and clamps out-of-range positions', () => {
    expect(toggleWhatsAppFormat('a   b', 1, 4, 'bold').value).toBe('a   b')
    expect(toggleWhatsAppFormat('abc', 5, -2, 'bold')).toEqual({ value: '*abc*', selectionStart: 1, selectionEnd: 4 })
  })
})

describe('insertTextAtSelection', () => {
  it('inserts at the caret or replaces the selection', () => {
    expect(insertTextAtSelection('Hi there', 2, 2, '👋')).toEqual({ value: 'Hi👋 there', selectionStart: 4, selectionEnd: 4 })
    expect(insertTextAtSelection('Hi there', 3, 8, '🙂')).toEqual({ value: 'Hi 🙂', selectionStart: 5, selectionEnd: 5 })
  })
})

describe('formatKindForShortcut', () => {
  const key = (k: string, mods: Partial<{ ctrlKey: boolean; metaKey: boolean; shiftKey: boolean; altKey: boolean }> = {}) => ({
    key: k,
    ctrlKey: false,
    metaKey: false,
    shiftKey: false,
    altKey: false,
    ...mods,
  })

  it('maps Ctrl/⌘+B, Ctrl/⌘+I and Ctrl/⌘+Shift+X', () => {
    expect(formatKindForShortcut(key('b', { ctrlKey: true }))).toBe('bold')
    expect(formatKindForShortcut(key('i', { metaKey: true }))).toBe('italic')
    expect(formatKindForShortcut(key('X', { ctrlKey: true, shiftKey: true }))).toBe('strike')
  })

  it('ignores plain keys, AltGr combos and other shortcuts', () => {
    expect(formatKindForShortcut(key('b'))).toBeNull()
    expect(formatKindForShortcut(key('b', { ctrlKey: true, altKey: true }))).toBeNull()
    expect(formatKindForShortcut(key('x', { ctrlKey: true }))).toBeNull()
    expect(formatKindForShortcut(key('B', { ctrlKey: true, shiftKey: true }))).toBeNull()
    expect(formatKindForShortcut(key('Enter', { ctrlKey: true }))).toBeNull()
  })
})
