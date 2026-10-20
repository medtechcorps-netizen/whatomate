import { expect, test as base, type Page, type WebSocketRoute } from '@playwright/test'
import { deflateSync } from 'node:zlib'

// The built app, DOM, image decoding and focus are real. API responses and the
// status-update socket are page-scoped synthetic fixtures, not provider proof.
const org = 'fa222222-2222-4222-8222-222222222222'
const contactId = 'fa333333-3333-4333-8333-333333333333'
const imageId = 'fa444444-4444-4444-8444-444444444444'
const user = {
  id: 'fa555555-5555-4555-8555-555555555555',
  email: 'reload@example.test', full_name: 'Synthetic Reload Agent',
  organization_id: org, organization_name: 'Synthetic Reload Workspace',
  is_super_admin: true, is_reseller_admin: false,
}
const crcTable = Array.from({ length: 256 }, (_, n) => {
  let c = n
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
  return c >>> 0
})
function chunk(type: string, data: Buffer) {
  const size = Buffer.alloc(4); size.writeUInt32BE(data.length)
  const body = Buffer.concat([Buffer.from(type), data])
  let crc = 0xffffffff
  for (const byte of body) crc = crcTable[(crc ^ byte) & 255] ^ (crc >>> 8)
  const checksum = Buffer.alloc(4); checksum.writeUInt32BE((crc ^ 0xffffffff) >>> 0)
  return Buffer.concat([size, body, checksum])
}
function png() {
  const header = Buffer.alloc(13)
  header.writeUInt32BE(2, 0); header.writeUInt32BE(2, 4)
  header[8] = 8; header[9] = 2
  return Buffer.concat([
    Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]),
    chunk('IHDR', header), chunk('IDAT', deflateSync(Buffer.from([
      0, 46, 125, 50, 46, 125, 50, 0, 46, 125, 50, 46, 125, 50,
    ]))), chunk('IEND', Buffer.alloc(0)),
  ])
}
type MediaRequest = { method: string; messageId: string; organizationId: string | null }
type ReloadApp = {
  page: Page
  requests: MediaRequest[]
  status: (value: 'sent' | 'delivered' | 'read') => void
  release: () => void
  maxInFlight: () => number
}
declare global {
  interface Window { __r3ImageErrors: number; __r3ImageErrorOverflow: boolean }
}

const test = base.extend<{ app: ReloadApp; corruptSecond: boolean }>({
  corruptSecond: [false, { option: true }],
  app: async ({ browser, baseURL, corruptSecond }, use, testInfo) => {
    if (!baseURL) throw new Error('owned-base-url-required')
    const origin = new URL(baseURL).origin
    if (!['127.0.0.1', 'localhost', '[::1]'].includes(new URL(origin).hostname)) {
      throw new Error('synthetic-media-fixture-requires-loopback')
    }
    const context = await browser.newContext({
      storageState: { cookies: [], origins: [] }, serviceWorkers: 'block',
      viewport: { width: 1440, height: 1000 },
    })
    const page = await context.newPage()
    const requests: MediaRequest[] = []
    const faults: string[] = []
    const sentStatuses: string[] = []
    const selectedContacts: string[] = []
    const sockets: WebSocketRoute[] = []
    let inFlight = 0
    let maxInFlight = 0
    let authMessages = 0
    let socketMessages = 0
    let release!: () => void
    const gate = new Promise<void>(resolve => { release = resolve })
    let decodeErrors: number | null = null
    let decodeOverflow: boolean | null = null
    const cleanupErrors: string[] = []
    try {
      // Install before app code can arm timers, so the later negative oracle
      // also observes a counterfactual retry armed during native decode.
      if (corruptSecond) await page.clock.install()
      await page.addInitScript(({ user, org }) => {
        localStorage.setItem('user', JSON.stringify(user))
        localStorage.setItem('selected_organization_id', org)
        localStorage.setItem('whm_session_id', 'synthetic-r3-reload')
        localStorage.setItem('color-mode', 'light')
        localStorage.setItem('language', 'en')
        window.__r3ImageErrors = 0
        window.__r3ImageErrorOverflow = false
        // Observe actual browser decode failures; never synthesize an event.
        document.addEventListener('error', event => {
          if (event.isTrusted && event.target instanceof HTMLImageElement &&
              event.target.src.startsWith('blob:')) {
            if (window.__r3ImageErrors >= 8) window.__r3ImageErrorOverflow = true
            else window.__r3ImageErrors++
          }
        }, true)
      }, { user, org })
      await page.routeWebSocket('**/*', socket => {
        const url = new URL(socket.url())
        const expected = new URL(origin); expected.protocol = expected.protocol === 'https:' ? 'wss:' : 'ws:'
        if (url.origin !== expected.origin || url.pathname !== '/ws' || url.search || sockets.length >= 2) {
          faults.push('unexpected-websocket')
          socket.close()
          return
        }
        sockets.push(socket)
        socket.onMessage(raw => {
          if (++socketMessages > 32) { faults.push('socket-message-cap'); socket.close(); return }
          const message = JSON.parse(String(raw))
          if (message.type === 'auth' && message.payload?.token === 'synthetic-r3-media') authMessages++
          else if (message.type === 'ping') socket.send(JSON.stringify({ type: 'pong' }))
          else if (message.type === 'set_contact' && message.payload?.contact_id === contactId &&
                   Object.keys(message.payload).length === 1) selectedContacts.push(contactId)
          else faults.push('unexpected-socket-message')
        })
      })
      const at = '2026-10-01T00:00:00Z'
      const contact = {
        id: contactId, name: 'Synthetic Reload Recipient', profile_name: '',
        phone_number: '60000000001', status: 'active', tags: [], metadata: {}, unread_count: 0,
        whatsapp_account: 'synthetic-reload',
        identity_review_ai_state: { known: true, ai_allowed: true, blocked: false },
        last_message_at: at, created_at: at, updated_at: at,
      }
      const image = {
        id: imageId, contact_id: contactId, direction: 'outgoing', message_type: 'image',
        content: { body: 'Synthetic pending image' }, status: 'pending',
        media_url: `organizations/${org}/messages/images/${imageId}.png`,
        media_mime_type: 'image/png', created_at: at, updated_at: at,
      }
      await page.route('**/*', async route => {
        const request = route.request()
        const url = new URL(request.url())
        const method = request.method()
        if (url.origin !== origin) {
          faults.push('external-request'); await route.abort('blockedbyclient'); return
        }
        if (!url.pathname.startsWith('/api/')) {
          if (method === 'GET' || method === 'HEAD') await route.continue()
          else { faults.push('non-api-mutation'); await route.abort('blockedbyclient') }
          return
        }
        if (url.pathname === `/api/media/${imageId}` && method === 'GET') {
          if (requests.length >= 6) {
            faults.push('media-request-cap'); await route.abort('blockedbyclient'); return
          }
          requests.push({ method, messageId: imageId, organizationId: await request.headerValue('x-organization-id') })
          const ordinal = requests.length
          inFlight++; maxInFlight = Math.max(maxInFlight, inFlight)
          try {
            if (ordinal === 1) await route.fulfill({ status: 404, json: { message: 'Synthetic media not ready' } })
            else {
              if (ordinal === 2) await gate
              await route.fulfill({ contentType: 'image/png',
                body: ordinal === 2 && corruptSecond ? Buffer.from('Synthetic invalid PNG bytes') : png() })
            }
          } finally { inFlight-- }
          return
        }
        if (method === 'POST' && url.pathname === `/api/contacts/${contactId}/mark-read`) {
          const body = request.postDataJSON()
          if (Object.keys(body).length !== 1 || body.last_visible_message_id !== imageId ||
              await request.headerValue('x-organization-id') !== org) faults.push('unexpected-read-ack')
          await route.fulfill({ json: { data: { unread_count: 0, manual_unread: false } } })
          return
        }
        if (method !== 'GET') {
          faults.push('unexpected-api-mutation'); await route.abort('blockedbyclient'); return
        }
        const data = url.pathname === '/api/me' ? user
          : url.pathname === '/api/product/entitlements' ? { mode: 'licensed', entitlements: {} }
          : url.pathname === '/api/auth/ws-token' ? { token: 'synthetic-r3-media' }
          : url.pathname === '/api/organizations' ? { organizations: [{ id: org, name: user.organization_name }] }
          : url.pathname === '/api/contacts' ? { contacts: [contact], total: 1 }
          : url.pathname === `/api/contacts/${contactId}` ? contact
          : url.pathname === `/api/contacts/${contactId}/messages` ? { messages: [image], has_more: false }
          : {}
        await route.fulfill({ json: { data } })
      })
      const response = await page.goto(`${origin}/chat/${contactId}`)
      expect(response?.status()).toBe(200)
      expect(response?.headers()['content-security-policy']).toContain("default-src 'self'")
      await expect(page.getByTestId('chat-header-name')).toHaveText(contact.name)
      await expect(page.getByTestId('chat-message-media-error')).toBeVisible()
      await expect.poll(() => authMessages).toBe(1)
      expect(requests).toEqual([{ method: 'GET', messageId: imageId, organizationId: org }])
      await use({
        page, requests, release, maxInFlight: () => maxInFlight,
        status: value => {
          if (sentStatuses.length >= 8 || sockets.length !== 1) throw new Error('status-fixture-bound')
          sentStatuses.push(value)
          sockets[0].send(JSON.stringify({ type: 'status_update', payload: { message_id: imageId, status: value } }))
        },
      })
    } finally {
      release()
      try {
        if (!page.isClosed()) {
          const observed = await page.evaluate(() => ({
            errors: window.__r3ImageErrors, overflow: window.__r3ImageErrorOverflow,
          }))
          decodeErrors = observed.errors; decodeOverflow = observed.overflow
          await page.unrouteAll({ behavior: 'wait' })
        }
      } catch { cleanupErrors.push('route-or-observation-cleanup') }
      try { await context.close() } catch { cleanupErrors.push('context-close') }
      const evidence = Buffer.from(JSON.stringify({
        schema: 1, syntheticApiAndSocket: true, nativeImageDecode: true,
        requests, maxInFlight, inFlight, sentStatuses, selectedContacts, authMessages, socketMessages,
        decodeErrors, decodeOverflow, faults, cleanupErrors,
      }))
      expect(evidence.length).toBeLessThanOrEqual(16 * 1024)
      await testInfo.attach('r3-media-reload', { body: evidence, contentType: 'application/json' })
      expect(faults).toEqual([])
      expect(cleanupErrors).toEqual([])
      expect(inFlight).toBe(0)
      expect(decodeOverflow).toBe(false)
    }
  },
})

for (const moveToComposer of [false, true]) {
  test(`a pending image reloads once and ${moveToComposer ? 'keeps focus moved to the composer' : 'moves Retry focus to the decoded image'}`, async ({ app }) => {
    const retry = app.page.getByTestId('chat-message-media-error').getByRole('button', { name: 'Retry', exact: true })
    await retry.focus()
    await expect(retry).toBeFocused()
    app.status('sent')
    await expect.poll(() => app.requests.length).toBe(2)
    await expect(retry).toBeVisible()
    const composer = app.page.locator('textarea').first()
    if (moveToComposer) { await composer.focus(); await expect(composer).toBeFocused() }
    app.status('delivered'); app.status('read')
    // Observe beyond the first real retry interval while the second GET is
    // held; repeated socket changes must not start a competing request.
    await app.page.waitForTimeout(1100)
    expect(app.requests).toHaveLength(2)
    expect(app.maxInFlight()).toBe(1)
    app.release()
    const media = app.page.getByTestId('chat-message-media')
    await expect(media).toHaveAttribute('data-media-status', 'ready')
    await expect.poll(() => media.locator('img').evaluate((element: HTMLImageElement) => element.naturalWidth)).toBe(2)
    await expect(moveToComposer ? composer : media.getByRole('button', { name: 'Synthetic pending image', exact: true })).toBeFocused()
    expect(app.requests.every(request => request.method === 'GET' && request.organizationId === org)).toBe(true)
  })
}

test.describe('native image decode failure', () => {
  test.use({ corruptSecond: true })
  test('corrupt reloaded bytes stop automatic GETs until ordinary Retry', async ({ app }) => {
    app.status('sent')
    await expect.poll(() => app.requests.length).toBe(2)
    app.release()
    await expect.poll(() => app.page.evaluate(() => window.__r3ImageErrors)).toBe(1)
    const retry = app.page.getByTestId('chat-message-media-error').getByRole('button', { name: 'Retry', exact: true })
    await expect(retry).toBeVisible()
    // Explicit browser-clock observation of the unchanged production 5s/15s
    // remaining budget. This does not inject progress, decode or socket events.
    app.status('delivered'); app.status('read')
    await expect(app.page.locator(`[data-testid="chat-message"][data-message-id="${imageId}"] .status-icon`))
      .toHaveClass(/text-blue-400/)
    await app.page.clock.runFor(1)
    await app.page.clock.fastForward(30_000)
    await app.page.waitForTimeout(200)
    expect(app.requests).toHaveLength(2)
    await retry.click()
    await expect.poll(() => app.requests.length).toBe(3)
    const image = app.page.getByTestId('chat-message-media').locator('img')
    await expect.poll(() => image.evaluate((element: HTMLImageElement) => element.naturalWidth)).toBe(2)
    expect(app.maxInFlight()).toBe(1)
  })
})
