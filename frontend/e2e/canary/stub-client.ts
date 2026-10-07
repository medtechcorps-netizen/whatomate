// @ts-nocheck
import { createHash, createHmac, randomBytes } from 'node:crypto'

export const sha256 = value => createHash('sha256').update(value).digest('hex')
export function controlHeaders(key, method, path, body, timestamp = String(Math.floor(Date.now() / 1000)), nonce = randomBytes(24).toString('hex')) {
  if (typeof key !== 'string' || key.length < 32) throw new Error('A private stub control key is required')
  return { 'Content-Type': 'application/json', 'X-Stub-Timestamp': timestamp, 'X-Stub-Nonce': nonce,
    'X-Stub-Mac': createHmac('sha256', key).update([method, path, timestamp, nonce, sha256(body)].join('|')).digest('hex') }
}

export class StubClient {
  constructor(origin, key) { this.origin = origin; this.key = key }
  async request(method, path, data) {
    if (!path.startsWith('/_control/')) throw new Error('Only authenticated stub controls are allowed')
    const body = data === undefined ? '' : JSON.stringify(data)
    const response = await fetch(this.origin + path, { method, redirect: 'error',
      headers: controlHeaders(this.key, method, path, body), body: body || undefined, signal: AbortSignal.timeout(45000) })
    if (!response.ok) throw new Error(`Stub control failed with HTTP ${response.status}`)
    const value = await response.json()
    if (value.callback_status && (value.callback_status < 200 || value.callback_status >= 300)) throw new Error('Synthetic webhook was rejected')
    return value
  }
  async journal(after = 0) {
    const entries = []
    let cursor = after
    for (let page = 0; page < 20; page++) {
      const result = await this.request('GET', `/_control/journal?after=${cursor}&limit=1000`)
      if (!Array.isArray(result.entries) || !Number.isSafeInteger(result.latest) || result.latest < cursor) throw new Error('Invalid or restarted stub journal')
      if (!result.entries.length && result.latest > cursor) throw new Error('Stub journal entries are missing')
      let previous = cursor
      for (const entry of result.entries) {
        if (!Number.isSafeInteger(entry.seq) || entry.seq <= previous || entry.seq > result.latest || (previous !== 0 && entry.seq !== previous + 1)) throw new Error('Stub journal was truncated or reordered')
        previous = entry.seq
      }
      entries.push(...result.entries)
      cursor = result.entries.at(-1)?.seq || cursor
      if (cursor === result.latest) return { entries, latest: result.latest }
    }
    throw new Error('Stub journal pagination did not settle')
  }
  async assertSend(after, { phone, to, body }) {
    const { entries } = await this.journal(after)
    const sends = entries.filter(e => e.kind === 'graph' && e.route === 'messages' && e.method === 'POST')
    if (sends.length !== 1 || sends[0].message_type !== 'text' || sends[0].status !== 200 || sends[0].phone_number_id !== phone || sends[0].to !== to || sends[0].text_sha256 !== sha256(body)) {
      throw new Error('Outbound stub journal must contain exactly one matching accepted text send')
    }
  }
  async assertNoSend(after) {
    const { entries } = await this.journal(after)
    if (entries.some(e => e.kind === 'graph' && e.route === 'messages' && e.method === 'POST')) throw new Error('Denied request reached the Graph send endpoint')
  }
}
