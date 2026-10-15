// WhatsApp text formatting: a tokenizer for rendering (WhatsAppText.vue),
// a marker stripper for one-line previews, and selection-aware edit helpers
// for the composer.
//
// WhatsApp's rules (Help Center, "How to format your messages"):
//   *bold*  _italic_  ~strike~  ```monospace```  `inline code`
// A marker only opens at the start of the text or after a non-word
// character, must be followed by a non-space character, and only closes
// after a non-space character and before the end or a non-word character.
// So "2*3*4", "snake_case_name" and "* list item" stay plain text.
// Inline markers do not span lines; ``` blocks may. Lists ("* ", "- ",
// "1. ") and quotes ("> ") are left as plain text.

export type WhatsAppFormatKind = 'bold' | 'italic' | 'strike' | 'mono'

export type WhatsAppNode =
  | { type: 'text'; text: string }
  | { type: 'bold' | 'italic' | 'strike'; children: WhatsAppNode[] }
  /** ```monospace``` block */
  | { type: 'mono'; text: string }
  /** `inline code` */
  | { type: 'code'; text: string }

export const WHATSAPP_MARKERS: Record<WhatsAppFormatKind, string> = {
  bold: '*',
  italic: '_',
  strike: '~',
  mono: '```',
}

const INLINE_MARKERS: Record<string, 'bold' | 'italic' | 'strike'> = {
  '*': 'bold',
  _: 'italic',
  '~': 'strike',
}

const MONO_FENCE = '```'

const WORD_CHAR = /[\p{L}\p{N}]/u
const SPACE_CHAR = /\s/u

function isWordChar(ch: string | undefined): boolean {
  return ch !== undefined && WORD_CHAR.test(ch)
}

function isSpace(ch: string | undefined): boolean {
  return ch !== undefined && SPACE_CHAR.test(ch)
}

function canOpen(text: string, index: number): boolean {
  const marker = text[index]
  const next = text[index + 1]
  if (next === undefined || isSpace(next) || next === marker) return false
  return index === 0 || !isWordChar(text[index - 1])
}

function findClose(text: string, open: number): number {
  const marker = text[open]
  for (let j = open + 2; j < text.length; j++) {
    const ch = text[j]
    if (ch === '\n') return -1
    if (ch !== marker) continue
    if (isSpace(text[j - 1])) continue
    if (isWordChar(text[j + 1])) continue
    return j
  }
  return -1
}

function pushText(nodes: WhatsAppNode[], text: string) {
  if (!text) return
  const last = nodes[nodes.length - 1]
  if (last?.type === 'text') last.text += text
  else nodes.push({ type: 'text', text })
}

function parseInline(text: string): WhatsAppNode[] {
  const nodes: WhatsAppNode[] = []
  let textStart = 0
  let i = 0
  while (i < text.length) {
    const ch = text[i]
    const isMarker = ch === '`' || ch in INLINE_MARKERS
    if (isMarker && canOpen(text, i)) {
      const close = findClose(text, i)
      if (close > 0) {
        pushText(nodes, text.slice(textStart, i))
        const inner = text.slice(i + 1, close)
        if (ch === '`') nodes.push({ type: 'code', text: inner })
        else nodes.push({ type: INLINE_MARKERS[ch], children: parseInline(inner) })
        i = close + 1
        textStart = i
        continue
      }
    }
    i++
  }
  pushText(nodes, text.slice(textStart))
  return nodes
}

/** Tokenizes WhatsApp-formatted text into a tree that renders without HTML injection. */
export function parseWhatsAppFormat(text: string | null | undefined): WhatsAppNode[] {
  if (!text) return []
  const nodes: WhatsAppNode[] = []
  let plainStart = 0
  let i = text.indexOf(MONO_FENCE)
  while (i >= 0) {
    const close = text.indexOf(MONO_FENCE, i + MONO_FENCE.length)
    if (close < 0) break
    const inner = text.slice(i + MONO_FENCE.length, close)
    if (inner.trim()) {
      for (const node of parseInline(text.slice(plainStart, i))) {
        if (node.type === 'text') pushText(nodes, node.text)
        else nodes.push(node)
      }
      nodes.push({ type: 'mono', text: inner })
      plainStart = close + MONO_FENCE.length
      i = text.indexOf(MONO_FENCE, plainStart)
    } else {
      // "``````" or a whitespace-only block: not monospace; keep scanning
      // from the closing fence.
      i = close
    }
  }
  for (const node of parseInline(text.slice(plainStart))) {
    if (node.type === 'text') pushText(nodes, node.text)
    else nodes.push(node)
  }
  return nodes
}

function flatten(nodes: WhatsAppNode[]): string {
  let out = ''
  for (const node of nodes) {
    if (node.type === 'text' || node.type === 'mono' || node.type === 'code') out += node.text
    else out += flatten(node.children)
  }
  return out
}

/**
 * The text as WhatsApp would display it, without formatting markers. For
 * one-line previews (conversation list, reply bar, notifications).
 */
export function stripWhatsAppMarkers(text: string | null | undefined): string {
  return flatten(parseWhatsAppFormat(text))
}

// ---------------------------------------------------------------------------
// Composer edit helpers
// ---------------------------------------------------------------------------

export interface TextEdit {
  value: string
  selectionStart: number
  selectionEnd: number
}

function clampSelection(value: string, start: number, end: number): [number, number] {
  const a = Math.max(0, Math.min(Number.isFinite(start) ? start : value.length, value.length))
  const b = Math.max(0, Math.min(Number.isFinite(end) ? end : a, value.length))
  return a <= b ? [a, b] : [b, a]
}

/** Replaces the selection (or inserts at the caret) and puts the caret after the inserted text. */
export function insertTextAtSelection(
  value: string,
  selectionStart: number,
  selectionEnd: number,
  text: string,
): TextEdit {
  const [start, end] = clampSelection(value, selectionStart, selectionEnd)
  const caret = start + text.length
  return {
    value: value.slice(0, start) + text + value.slice(end),
    selectionStart: caret,
    selectionEnd: caret,
  }
}

function splitWhitespace(segment: string): { lead: string; core: string; trail: string } {
  const lead = segment.match(/^\s*/)?.[0] ?? ''
  const rest = segment.slice(lead.length)
  const trail = rest.match(/\s*$/)?.[0] ?? ''
  return { lead, core: rest.slice(0, rest.length - trail.length), trail }
}

function isWrapped(core: string, marker: string): boolean {
  return (
    core.length > marker.length * 2 &&
    core.startsWith(marker) &&
    core.endsWith(marker)
  )
}

/**
 * Toggles a WhatsApp marker around the selection.
 *
 * - Caret only: inserts an empty marker pair with the caret inside it (or
 *   removes an empty pair the caret sits in).
 * - Selection: wraps it, keeping surrounding spaces outside the markers
 *   because WhatsApp markers must hug non-space characters. Inline markers
 *   wrap each selected line separately (they cannot span lines); ``` wraps
 *   the whole selection as one block.
 * - Already formatted (markers inside or just outside the selection):
 *   removes the markers instead.
 */
export function toggleWhatsAppFormat(
  value: string,
  selectionStart: number,
  selectionEnd: number,
  kind: WhatsAppFormatKind,
): TextEdit {
  const marker = WHATSAPP_MARKERS[kind]
  const m = marker.length
  const [start, end] = clampSelection(value, selectionStart, selectionEnd)

  if (start === end) {
    if (value.slice(start - m, start) === marker && value.slice(start, start + m) === marker) {
      return {
        value: value.slice(0, start - m) + value.slice(start + m),
        selectionStart: start - m,
        selectionEnd: start - m,
      }
    }
    return {
      value: value.slice(0, start) + marker + marker + value.slice(start),
      selectionStart: start + m,
      selectionEnd: start + m,
    }
  }

  const selected = value.slice(start, end)

  // Markers just outside the selection, e.g. the user selected "hi" in "*hi*".
  // Inline markers only count when they could format (not "snake_case_name").
  const outsideIsFormatting =
    kind === 'mono' ||
    (!selected.includes('\n') && !isWordChar(value[start - m - 1]) && !isWordChar(value[end + m]))
  if (outsideIsFormatting) {
    if (value.slice(start - m, start) === marker && value.slice(end, end + m) === marker) {
      return {
        value: value.slice(0, start - m) + selected + value.slice(end + m),
        selectionStart: start - m,
        selectionEnd: end - m,
      }
    }
  }

  if (kind === 'mono') {
    const { lead, core, trail } = splitWhitespace(selected)
    if (!core) return { value, selectionStart: start, selectionEnd: end }
    if (isWrapped(core, marker)) {
      const inner = core.slice(m, core.length - m)
      const replaced = lead + inner + trail
      return {
        value: value.slice(0, start) + replaced + value.slice(end),
        selectionStart: start + lead.length,
        selectionEnd: start + lead.length + inner.length,
      }
    }
    const replaced = lead + marker + core + marker + trail
    return {
      value: value.slice(0, start) + replaced + value.slice(end),
      selectionStart: start + lead.length + m,
      selectionEnd: start + lead.length + m + core.length,
    }
  }

  const lines = selected.split('\n').map(splitWhitespace)
  const formattable = lines.filter(line => line.core)
  if (formattable.length === 0) return { value, selectionStart: start, selectionEnd: end }
  const unwrap = formattable.every(line => isWrapped(line.core, marker))
  const rebuilt = lines
    .map(({ lead, core, trail }) => {
      if (!core) return lead + trail
      const next = unwrap ? core.slice(m, core.length - m) : marker + core + marker
      return lead + next + trail
    })
    .join('\n')

  if (lines.length === 1) {
    // Keep the selection on the text itself so a second shortcut nests
    // (e.g. Ctrl+B then Ctrl+I gives *_text_*).
    const { lead, core } = lines[0]
    const innerLength = unwrap ? core.length - 2 * m : core.length
    const innerStart = start + lead.length + (unwrap ? 0 : m)
    return {
      value: value.slice(0, start) + rebuilt + value.slice(end),
      selectionStart: innerStart,
      selectionEnd: innerStart + innerLength,
    }
  }
  return {
    value: value.slice(0, start) + rebuilt + value.slice(end),
    selectionStart: start,
    selectionEnd: start + rebuilt.length,
  }
}

export interface ShortcutKeyEvent {
  key: string
  ctrlKey: boolean
  metaKey: boolean
  shiftKey: boolean
  altKey: boolean
}

/** Ctrl/⌘+B bold, Ctrl/⌘+I italic, Ctrl/⌘+Shift+X strikethrough. */
export function formatKindForShortcut(event: ShortcutKeyEvent): WhatsAppFormatKind | null {
  // Alt is excluded so AltGr (Ctrl+Alt on Windows) keeps typing characters.
  if (event.altKey || !(event.ctrlKey || event.metaKey)) return null
  const key = event.key.toLowerCase()
  if (!event.shiftKey && key === 'b') return 'bold'
  if (!event.shiftKey && key === 'i') return 'italic'
  if (event.shiftKey && key === 'x') return 'strike'
  return null
}
