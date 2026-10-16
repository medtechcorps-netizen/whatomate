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

  // Adversarial generators by length. Timing assertions compare equal total
  // work (one 64 KB parse vs sixteen 4 KB parses, interleaved, median of 5):
  // a linear parser scores about 1-3, the old quadratic one about 16, so the
  // limit of 8 leaves room on both sides. Absolute bounds are only a generous
  // ceiling (the old parser took 6 s), so a busy CI box stays green.
  const repeatTo = (pattern: string, length: number) => pattern.repeat(Math.ceil(length / pattern.length)).slice(0, length)
  const adversarial: Record<string, (length: number) => string> = {
    'space-star-a': length => repeatTo(' *a', length),
    'mixed openers': length => repeatTo('*_~`a ', length),
    'underscores': length => repeatTo('_a', length),
    'nested spans': length => repeatTo('*a _b ~c `d` c~ b_ a* ', length),
    'dotted link': length => 'https://' + '.'.repeat(length - 9) + 'x',
    'many links': length => repeatTo('www.a_b_ ', length),
  }
  const BIG = 65_536
  const SMALL = 4_096
  const SCALING_LIMIT = 8
  const CEILING_MS = 1_000

  function bestOf(runs: number, fn: () => void): number {
    let best = Infinity
    for (let run = 0; run < runs; run++) best = Math.min(best, time(fn))
    return best
  }

  function median(values: number[]): number {
    const sorted = [...values].sort((a, b) => a - b)
    return sorted[Math.floor(sorted.length / 2)]
  }

  // Warm the JIT so the first timed case is not penalised.
  parseWhatsAppFormat(' *a'.repeat(1000), { maxFormattedLineLength: Infinity })

  for (const [name, make] of Object.entries(adversarial)) {
    it(`parses adversarial input (${name}) in linear time, even with no line cap`, { retry: 2 }, () => {
      const big = make(BIG)
      const small = make(SMALL)
      const uncapped = { maxFormattedLineLength: Infinity }
      // Interleave the runs so a burst of machine load hits both sides.
      const bigRuns: number[] = []
      const smallRuns: number[] = []
      for (let run = 0; run < 5; run++) {
        bigRuns.push(time(() => parseWhatsAppFormat(big, uncapped)))
        smallRuns.push(time(() => {
          for (let k = 0; k < BIG / SMALL; k++) parseWhatsAppFormat(small, uncapped)
        }))
      }
      const bigMs = median(bigRuns)
      // Floor the denominator so timer granularity cannot fake a large ratio.
      expect(bigMs / Math.max(median(smallRuns), 0.5)).toBeLessThan(SCALING_LIMIT)
      expect(bigMs).toBeLessThan(CEILING_MS)
      expect(bestOf(3, () => parseWhatsAppFormat(big))).toBeLessThan(CEILING_MS)
    })
  }

  it('renders a 65,536-character adversarial line quickly', () => {
    mount(WhatsAppText, { props: { text: '*warm* up' } }).unmount()
    const text = adversarial['space-star-a'](BIG)
    let wrapper: ReturnType<typeof mount> | undefined
    const elapsed = time(() => {
      wrapper = mount(WhatsAppText, { props: { text } })
    })
    expect(elapsed).toBeLessThan(CEILING_MS)
    expect(wrapper!.element.textContent).toBe(text)
    wrapper!.unmount()
  })

  it('parses 64 KB of short adversarial lines quickly', () => {
    const text = Array.from({ length: 512 }, () => ' *a _b'.repeat(21)).join('\n')
    expect(text.length).toBeGreaterThan(64_000)
    expect(bestOf(3, () => parseWhatsAppFormat(text))).toBeLessThan(CEILING_MS)
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
