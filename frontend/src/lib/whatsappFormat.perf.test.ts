/** @vitest-environment happy-dom */

import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import WhatsAppText from '@/components/chat/WhatsAppText.vue'
import {
  MAX_FORMATTED_LINE_LENGTH,
  parseWhatsAppFormat,
  stripWhatsAppMarkers,
  type WhatsAppNode,
} from './whatsappFormat'

// ---------------------------------------------------------------------------
// Reference: the original (quadratic) parser, kept verbatim so a fuzz test can
// prove the linear parser formats ordinary text exactly the same way.
// ---------------------------------------------------------------------------
const REF_INLINE: Record<string, 'bold' | 'italic' | 'strike'> = { '*': 'bold', _: 'italic', '~': 'strike' }
const refWord = (ch: string | undefined) => ch !== undefined && /[\p{L}\p{N}]/u.test(ch)
const refSpace = (ch: string | undefined) => ch !== undefined && /\s/u.test(ch)
function refCanOpen(text: string, index: number) {
  const next = text[index + 1]
  if (next === undefined || refSpace(next) || next === text[index]) return false
  return index === 0 || !refWord(text[index - 1])
}
function refFindClose(text: string, open: number) {
  const marker = text[open]
  for (let j = open + 2; j < text.length; j++) {
    const ch = text[j]
    if (ch === '\n') return -1
    if (ch !== marker) continue
    if (refSpace(text[j - 1])) continue
    if (refWord(text[j + 1])) continue
    return j
  }
  return -1
}
function refPush(nodes: WhatsAppNode[], text: string) {
  if (!text) return
  const last = nodes[nodes.length - 1]
  if (last?.type === 'text') last.text += text
  else nodes.push({ type: 'text', text })
}
function refInline(text: string): WhatsAppNode[] {
  const nodes: WhatsAppNode[] = []
  let textStart = 0
  let i = 0
  while (i < text.length) {
    const ch = text[i]
    if ((ch === '`' || ch in REF_INLINE) && refCanOpen(text, i)) {
      const close = refFindClose(text, i)
      if (close > 0) {
        refPush(nodes, text.slice(textStart, i))
        const inner = text.slice(i + 1, close)
        if (ch === '`') nodes.push({ type: 'code', text: inner })
        else nodes.push({ type: REF_INLINE[ch], children: refInline(inner) })
        i = close + 1
        textStart = i
        continue
      }
    }
    i++
  }
  refPush(nodes, text.slice(textStart))
  return nodes
}
function referenceParse(text: string): WhatsAppNode[] {
  const nodes: WhatsAppNode[] = []
  let plainStart = 0
  let i = text.indexOf('```')
  while (i >= 0) {
    const close = text.indexOf('```', i + 3)
    if (close < 0) break
    const inner = text.slice(i + 3, close)
    if (inner.trim()) {
      for (const node of refInline(text.slice(plainStart, i))) {
        if (node.type === 'text') refPush(nodes, node.text)
        else nodes.push(node)
      }
      nodes.push({ type: 'mono', text: inner })
      plainStart = close + 3
      i = text.indexOf('```', plainStart)
    } else {
      i = close
    }
  }
  for (const node of refInline(text.slice(plainStart))) {
    if (node.type === 'text') refPush(nodes, node.text)
    else nodes.push(node)
  }
  return nodes
}

function seededRandom(seed: number) {
  let state = seed
  return () => {
    state = (state * 1103515245 + 12345) & 0x7fffffff
    return state / 0x7fffffff
  }
}

function time(fn: () => void): number {
  const started = performance.now()
  fn()
  return performance.now() - started
}

describe('linear parser', () => {
  it('formats ordinary text exactly like the original parser (fuzz)', () => {
    const alphabet = ['*', '_', '~', '`', 'a', 'b', 'é', '1', ' ', ' ', '\n', '.', '(', ')', '-']
    const random = seededRandom(42)
    for (let round = 0; round < 3000; round++) {
      const length = 1 + Math.floor(random() * 40)
      let text = ''
      for (let k = 0; k < length; k++) text += alphabet[Math.floor(random() * alphabet.length)]
      expect(parseWhatsAppFormat(text), JSON.stringify(text)).toEqual(referenceParse(text))
    }
  })

  const adversarial = {
    'space-star-a': ' *a'.repeat(21846).slice(0, 65536),
    'mixed openers': '*_~`a '.repeat(10923).slice(0, 65536),
    'underscores': '_a'.repeat(32768),
    'nested spans': '*a _b ~c `d` c~ b_ a* '.repeat(3000).slice(0, 65536),
    'dotted link': 'https://' + '.'.repeat(65520) + 'x',
    'many links': 'www.a_b_ '.repeat(7282).slice(0, 65536),
  }

  // Warm the JIT so the first timed case is not penalised.
  parseWhatsAppFormat(' *a'.repeat(1000), { maxFormattedLineLength: Infinity })

  for (const [name, text] of Object.entries(adversarial)) {
    it(`parses a 64 KB adversarial line (${name}) in under 50 ms, even with no line cap`, () => {
      expect(text.length).toBeGreaterThanOrEqual(65_000)
      expect(time(() => parseWhatsAppFormat(text, { maxFormattedLineLength: Infinity }))).toBeLessThan(50)
      expect(time(() => parseWhatsAppFormat(text))).toBeLessThan(50)
    })
  }

  it('renders a 65,536-character adversarial line in under 50 ms', () => {
    mount(WhatsAppText, { props: { text: '*warm* up' } }).unmount()
    const text = adversarial['space-star-a']
    let wrapper: ReturnType<typeof mount> | undefined
    const elapsed = time(() => {
      wrapper = mount(WhatsAppText, { props: { text } })
    })
    expect(elapsed).toBeLessThan(50)
    expect(wrapper!.element.textContent).toBe(text)
    wrapper!.unmount()
  })

  it('renders 64 KB of short adversarial lines in under 50 ms', () => {
    const text = Array.from({ length: 512 }, () => ' *a _b'.repeat(21)).join('\n')
    expect(text.length).toBeGreaterThan(64_000)
    expect(time(() => parseWhatsAppFormat(text))).toBeLessThan(50)
  })
})

describe('line cap', () => {
  it('leaves lines longer than the cap as plain text but formats the others', () => {
    const long = '*x* '.repeat(MAX_FORMATTED_LINE_LENGTH / 4 + 1)
    const nodes = parseWhatsAppFormat(`*short*\n${long}\n_also_`)
    expect(nodes[0]).toEqual({ type: 'bold', children: [{ type: 'text', text: 'short' }] })
    expect(nodes[1]).toEqual({ type: 'text', text: `\n${long}\n` })
    expect(nodes[2]).toEqual({ type: 'italic', children: [{ type: 'text', text: 'also' }] })
  })
})

describe('links', () => {
  it('keeps marker characters inside links and does not format them', () => {
    const text = 'see https://x.test/some_path_name_ and https://x.test/a*b*c'
    expect(parseWhatsAppFormat(text)).toEqual([{ type: 'text', text }])
    expect(stripWhatsAppMarkers('www.x.test/~a~/_b_')).toBe('www.x.test/~a~/_b_')
  })

  it('formats around a link and lets a span contain one', () => {
    expect(parseWhatsAppFormat('*https://x.test/a_b*')).toEqual([
      { type: 'bold', children: [{ type: 'text', text: 'https://x.test/a_b' }] },
    ])
    expect(parseWhatsAppFormat('_visit https://x.test today_ and *now*')).toEqual([
      { type: 'italic', children: [{ type: 'text', text: 'visit https://x.test today' }] },
      { type: 'text', text: ' and ' },
      { type: 'bold', children: [{ type: 'text', text: 'now' }] },
    ])
  })

  it('does not let a span close inside a link', () => {
    // Without the link, "b_/" would close the italic.
    expect(parseWhatsAppFormat('_a https://x.test/b_/c')).toEqual([{ type: 'text', text: '_a https://x.test/b_/c' }])
    // A marker right after the link belongs to the sentence.
    expect(parseWhatsAppFormat('_a https://x.test/b_ c')).toEqual([
      { type: 'italic', children: [{ type: 'text', text: 'a https://x.test/b' }] },
      { type: 'text', text: ' c' },
    ])
  })
})
