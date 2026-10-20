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

const MONO_FENCE = '```'

/**
 * Lines longer than this render as plain text. WhatsApp caps messages at
 * 4096 characters, so only pasted or imported text ever reaches it; it
 * bounds the work and the number of nodes a hostile message can create.
 */
export const MAX_FORMATTED_LINE_LENGTH = 8192

const WORD_CHAR = /[\p{L}\p{N}]/u
const SPACE_CHAR = /\s/u

function isWordChar(ch: string | undefined): boolean {
  return ch !== undefined && WORD_CHAR.test(ch)
}

// Character classes; 0 = anything else.
const CLASS_WORD = 1
const CLASS_SPACE = 2

// Links are detected first and their characters never open or close a
// marker, as in WhatsApp: "https://x.test/a_b_c_" keeps its underscores.
const URL_PATTERN = /\b(?:https?:\/\/|www\.)[^\s<>"]+/giu
// Trailing punctuation and markers belong to the sentence, not the link
// ("*see https://x.test*"). Trimmed with a loop: a regex here backtracks
// quadratically on a link padded with thousands of dots.
const URL_TRAILING = new Set(['.', ',', ';', ':', '!', '?', "'", '"', ')', ']', '}', '*', '_', '~', '`'])

const MARKER_TYPES = ['bold', 'italic', 'strike', 'code'] as const
const CODE_MARKER = 3

function markerIndex(ch: string): number {
  switch (ch) {
    case '*':
      return 0
    case '_':
      return 1
    case '~':
      return 2
    case '`':
      return CODE_MARKER
    default:
      return -1
  }
}

/**
 * Everything the parser needs, precomputed in one pass so each marker is
 * checked in O(1): parsing is linear in the text length. (The previous
 * version rescanned the rest of the line for every unmatched opener, which
 * a message of " *a" repeated turned into seconds of work.)
 */
interface ParseContext {
  text: string
  charClass: Uint8Array
  inUrl: Uint8Array
  /** Index of the newline ending the line that contains i (or text.length). */
  lineEnd: Int32Array
  /** 1 when the line that contains i is too long to format. */
  longLine: Uint8Array
  /** nextCloser[marker][i]: first index >= i that can close that marker, or -1. */
  nextCloser: Int32Array[]
}

function classify(text: string): Uint8Array {
  const classes = new Uint8Array(text.length)
  for (let i = 0; i < text.length; i++) {
    const code = text.charCodeAt(i)
    if (code < 128) {
      if ((code >= 48 && code <= 57) || (code >= 65 && code <= 90) || (code >= 97 && code <= 122)) {
        classes[i] = CLASS_WORD
      } else if (code === 32 || (code >= 9 && code <= 13)) {
        classes[i] = CLASS_SPACE
      }
    } else if (WORD_CHAR.test(text[i])) {
      classes[i] = CLASS_WORD
    } else if (SPACE_CHAR.test(text[i])) {
      classes[i] = CLASS_SPACE
    }
  }
  return classes
}

function createContext(text: string, maxLineLength: number): ParseContext {
  const n = text.length
  const charClass = classify(text)

  const inUrl = new Uint8Array(n)
  URL_PATTERN.lastIndex = 0
  for (let match = URL_PATTERN.exec(text); match; match = URL_PATTERN.exec(text)) {
    let urlEnd = match.index + match[0].length
    while (urlEnd > match.index && URL_TRAILING.has(text[urlEnd - 1])) urlEnd--
    inUrl.fill(1, match.index, urlEnd)
  }

  const lineEnd = new Int32Array(n + 1)
  lineEnd[n] = n
  for (let i = n - 1; i >= 0; i--) lineEnd[i] = text.charCodeAt(i) === 10 ? i : lineEnd[i + 1]
  const longLine = new Uint8Array(n)
  for (let start = 0; start < n; ) {
    const end = lineEnd[start]
    if (end - start > maxLineLength) longLine.fill(1, start, end)
    start = end + 1
  }

  const nextCloser = MARKER_TYPES.map(() => new Int32Array(n + 2).fill(-1))
  for (let j = n - 1; j >= 0; j--) {
    for (const closers of nextCloser) closers[j] = closers[j + 1]
    const marker = markerIndex(text[j])
    if (
      marker >= 0 &&
      j > 0 &&
      !inUrl[j] &&
      charClass[j - 1] !== CLASS_SPACE &&
      (j + 1 >= n || charClass[j + 1] !== CLASS_WORD)
    ) {
      nextCloser[marker][j] = j
    }
  }

  return { text, charClass, inUrl, lineEnd, longLine, nextCloser }
}

function pushText(nodes: WhatsAppNode[], text: string) {
  if (!text) return
  const last = nodes[nodes.length - 1]
  if (last?.type === 'text') last.text += text
  else nodes.push({ type: 'text', text })
}

function canOpen(ctx: ParseContext, i: number, end: number): boolean {
  if (ctx.inUrl[i] || ctx.longLine[i] || i + 1 >= end) return false
  if (ctx.charClass[i + 1] === CLASS_SPACE || ctx.text[i + 1] === ctx.text[i]) return false
  return i === 0 || ctx.charClass[i - 1] !== CLASS_WORD
}

function findClose(ctx: ParseContext, open: number, marker: number, end: number): number {
  const close = open + 2 < ctx.nextCloser[marker].length ? ctx.nextCloser[marker][open + 2] : -1
  if (close < 0 || close >= end || close >= ctx.lineEnd[open]) return -1
  return close
}

/**
 * Parses text[start, end). A span closes at the first valid closer of its
 * marker, so a span never contains a complete span of the same marker and
 * nesting is at most four levels deep: each character is visited a bounded
 * number of times.
 */
function parseRange(ctx: ParseContext, start: number, end: number, nodes: WhatsAppNode[] = []): WhatsAppNode[] {
  const { text } = ctx
  let textStart = start
  let i = start
  while (i < end) {
    const marker = markerIndex(text[i])
    if (marker >= 0 && canOpen(ctx, i, end)) {
      const close = findClose(ctx, i, marker, end)
      if (close >= 0) {
        pushText(nodes, text.slice(textStart, i))
        if (marker === CODE_MARKER) nodes.push({ type: 'code', text: text.slice(i + 1, close) })
        else {
          nodes.push({
            type: MARKER_TYPES[marker] as 'bold' | 'italic' | 'strike',
            children: parseRange(ctx, i + 1, close),
          })
        }
        i = close + 1
        textStart = i
        continue
      }
    }
    i++
  }
  pushText(nodes, text.slice(textStart, end))
  return nodes
}

export interface ParseWhatsAppFormatOptions {
  /** Lines longer than this stay plain text. */
  maxFormattedLineLength?: number
}

/** Tokenizes WhatsApp-formatted text into a tree that renders without HTML injection. */
export function parseWhatsAppFormat(
  text: string | null | undefined,
  options: ParseWhatsAppFormatOptions = {},
): WhatsAppNode[] {
  if (!text) return []
  const ctx = createContext(text, options.maxFormattedLineLength ?? MAX_FORMATTED_LINE_LENGTH)
  const nodes: WhatsAppNode[] = []
  let plainStart = 0
  let i = text.indexOf(MONO_FENCE)
  while (i >= 0) {
    const close = text.indexOf(MONO_FENCE, i + MONO_FENCE.length)
    if (close < 0) break
    const inner = text.slice(i + MONO_FENCE.length, close)
    if (inner.trim() && !ctx.longLine[i]) {
      parseRange(ctx, plainStart, i, nodes)
      nodes.push({ type: 'mono', text: inner })
      plainStart = close + MONO_FENCE.length
      i = text.indexOf(MONO_FENCE, plainStart)
    } else {
      // "``````", a whitespace-only block, or a fence on an over-long line:
      // not monospace; keep scanning from the closing fence.
      i = close
    }
  }
  parseRange(ctx, plainStart, text.length, nodes)
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
