import { test as base, expect, type BrowserContext, type Page } from '@playwright/test'
import { digest, multipartReceiver } from './multipart-receiver'

const org = 'fc222222-2222-4222-8222-222222222222'
const otherOrg = 'fc999999-9999-4999-8999-999999999999'
const contact = 'fc333333-3333-4333-8333-333333333333'
const otherContact = 'fc444444-4444-4444-8444-444444444444'
const user = { id: 'fc555555-5555-4555-8555-555555555555', email: 'multipart@example.test',
  full_name: 'Synthetic Multipart Agent', organization_id: org, organization_name: 'Synthetic Upload Workspace',
  is_super_admin: true, is_reseller_admin: false }
const first = { name: 'synthetic-first.txt', mimeType: 'text/plain', buffer: Buffer.from('First synthetic attachment.\n') }
const second = { name: 'synthetic-second.txt', mimeType: 'text/plain', buffer: Buffer.from('Second synthetic attachment.\n') }
const replyIds = ['fc666666-6666-4666-8666-666666666661', 'fc666666-6666-4666-8666-666666666662']
type NativeUpload = { name: string; sent: boolean; uploadLoaded: number; uploadTotal: number;
  uploadLoad: boolean; aborted: boolean; error: boolean; ended: boolean; status: number }
declare global {
  interface Window { __r2Uploads: NativeUpload[]; __r2Overflow: boolean; __r2StorageKeys: string[] }
}
type Receiver = Awaited<ReturnType<typeof multipartReceiver>>
type App = { page: Page; context: BrowserContext; receiver: Receiver }

async function syntheticApi(page: Page, receiver: Receiver, withReplies = false) {
  await page.addInitScript(({ user, org }) => {
    if (!localStorage.getItem('user')) localStorage.setItem('user', JSON.stringify(user))
    if (!localStorage.getItem('selected_organization_id')) localStorage.setItem('selected_organization_id', org)
    if (!localStorage.getItem('whm_session_id')) localStorage.setItem('whm_session_id', 'synthetic-session-one')
    localStorage.setItem('color-mode', 'light')
    window.__r2Uploads = []
    window.__r2Overflow = false
    window.__r2StorageKeys = []
    window.addEventListener('storage', event => {
      if (window.__r2StorageKeys.length >= 16) window.__r2Overflow = true
      else if (event.key) window.__r2StorageKeys.push(event.key)
    })
    // Observation only: original XHR, body, transport, progress and abort are
    // untouched. No synthetic progress dispatch, Axios adapter or store hook.
    const original = XMLHttpRequest.prototype.send
    XMLHttpRequest.prototype.send = function (body) {
      if (body instanceof FormData && body.has('contact_id')) {
        const file = body.get('file')
        if (!(file instanceof File) || window.__r2Uploads.length >= 8) {
          window.__r2Overflow = true
        } else {
          const row: NativeUpload = { name: file.name, sent: true, uploadLoaded: 0, uploadTotal: 0,
            uploadLoad: false, aborted: false, error: false, ended: false, status: 0 }
          window.__r2Uploads.push(row)
          this.upload.addEventListener('progress', event => {
            row.uploadLoaded = event.loaded; row.uploadTotal = event.total
          })
          this.upload.addEventListener('load', event => {
            row.uploadLoad = event.lengthComputable && event.total > 0 && event.loaded === event.total
          })
          this.addEventListener('abort', () => { row.aborted = true })
          this.addEventListener('error', () => { row.error = true })
          this.addEventListener('loadend', () => { row.ended = true; row.status = this.status })
        }
      }
      return original.call(this, body)
    }
  }, { user, org })
  await page.route('**/api/**', route => route.fulfill({ json: { data: {} } }))
  await page.route(/\/api\/messages\/media$/, route => {
    // Page routes precede context routes: enforce the receiver boundary here
    // too, including when a misbound build contains a different VITE_API_URL.
    if (route.request().url() === `${receiver.origin}/api/messages/media` && route.request().method() === 'POST') {
      return route.continue()
    }
    receiver.faults.push('non-fixture-media-request')
    return route.abort('blockedbyclient')
  })
  await page.route(/\/api\/me(?:\?.*)?$/, route => route.fulfill({ json: { data: user } }))
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, route =>
    route.fulfill({ json: { data: { mode: 'licensed', entitlements: {} } } }))
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, route => route.fulfill({ json: { data: { token: '' } } }))
  await page.route(/\/api\/organizations(?:\?.*)?$/, route => route.fulfill({ json: { data: { organizations: [
    { id: org, name: 'Synthetic Upload Workspace' }, { id: otherOrg, name: 'Synthetic Other Workspace' },
  ] } } }))
  const contacts = [contact, otherContact].map((id, index) => ({ id,
    name: index ? 'Synthetic Other Chat' : 'Synthetic Upload Recipient', profile_name: '',
    phone_number: `6000000000${index + 1}`, status: 'active', tags: [], metadata: {}, unread_count: 0,
    whatsapp_account: 'synthetic-upload', identity_review_ai_state: { known: true, ai_allowed: true, blocked: false },
    last_message_at: '2026-10-01T00:00:00Z', created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-01T00:00:00Z',
  }))
  await page.route(/\/api\/contacts(?:\?.*)?$/, route =>
    route.fulfill({ json: { data: { contacts, total: contacts.length } } }))
  for (const row of contacts) {
    await page.route(new RegExp(`/api/contacts/${row.id}(?:\\?.*)?$`), route => route.fulfill({ json: { data: row } }))
    await page.route(new RegExp(`/api/contacts/${row.id}/messages(?:\\?.*)?$`), route =>
      route.fulfill({ json: { data: { messages: withReplies && row.id === contact ? replyIds.map((id, index) => ({
        id, contact_id: contact, direction: 'incoming', message_type: 'text',
        content: { body: `Synthetic quote Q${index + 1}` }, status: 'delivered',
        created_at: `2026-10-01T00:0${index}:00Z`, updated_at: `2026-10-01T00:0${index}:00Z`,
      })) : [], has_more: false } } }))
  }
}

const test = base.extend<{ app: App; withReplies: boolean }>({
  withReplies: [false, { option: true }],
  app: async ({ browser, baseURL, withReplies }, use, testInfo) => {
    const receiver = await multipartReceiver(baseURL ?? '', withReplies ? replyIds : [])
    let context: BrowserContext | undefined
    let page: Page | undefined
    let cleanup: unknown = null
    let observations: { native: NativeUpload[]; overflow: boolean; storageKeys: string[] } | null = null
    const cleanupErrors: string[] = []
    try {
      context = await browser.newContext({ storageState: { cookies: [], origins: [] }, serviceWorkers: 'block',
        viewport: { width: 1600, height: 1000 } })
      await context.route('**/*', route => {
        if (new URL(route.request().url()).origin === receiver.origin) return route.continue()
        receiver.faults.push('non-fixture-browser-request')
        return route.abort('blockedbyclient')
      })
      page = await context.newPage()
      await syntheticApi(page, receiver, withReplies)
      const response = await page.goto(`${receiver.origin}/chat/${contact}`)
      expect(response?.status()).toBe(200)
      expect(response?.headers()['content-security-policy']).toContain("default-src 'self'")
      await expect(page.getByTestId('chat-header-name')).toHaveText('Synthetic Upload Recipient')
      await expect(page.locator('input[type="file"][multiple]')).toHaveCount(1)
      await use({ page, context, receiver })
    } finally {
      try {
        if (page && !page.isClosed()) observations = await page.evaluate(() => ({
          native: window.__r2Uploads, overflow: window.__r2Overflow, storageKeys: window.__r2StorageKeys,
        }))
      } catch { cleanupErrors.push('observation-read') }
      try { await context?.close() } catch { cleanupErrors.push('context-close') }
      try { cleanup = await receiver.close() } catch { cleanupErrors.push('receiver-close') }
      const evidence = Buffer.from(JSON.stringify({ schema: 1, syntheticAcceptanceOnly: true,
        realNativeXHR: true, observations, uploads: receiver.uploads, faults: receiver.faults, cleanup, cleanupErrors }))
      expect(evidence.length).toBeLessThanOrEqual(64 * 1024)
      await testInfo.attach('multipart-native-transport', { body: evidence, contentType: 'application/json' })
      expect(cleanupErrors).toEqual([])
      expect(receiver.faults).toEqual([])
      expect(observations).toMatchObject({ overflow: false })
      expect(observations?.native.every(row => row.ended)).toBe(true)
      expect(cleanup).toMatchObject({ closed: true, sockets: 0, proxies: 0 })
    }
  },
})

async function stage(app: App, files = [first, second], { expectActiveHold = true }: { expectActiveHold?: boolean } = {}) {
  await app.page.locator('input[type="file"][multiple]').setInputFiles(files)
  await expect(app.page.getByTestId('composer-media-item')).toHaveCount(files.length)
  await expect(app.page.getByTestId('composer-media-send')).toBeEnabled()
  await app.page.getByTestId('composer-media-send').click()
  if (expectActiveHold) await expect(app.page.getByTestId('organization-switcher')).toBeDisabled()
}

async function uploaded(app: App, ordinal = 1, file = first, detached = false) {
  await expect.poll(() => app.receiver.uploads[ordinal - 1]?.bodyComplete).toBe(true)
  expect(app.receiver.uploads[ordinal - 1]).toMatchObject({ organization: org, contact, kind: 'document',
    filename: file.name, fileBytes: file.buffer.length, fileSha256: digest(file.buffer) })
  expect(app.receiver.uploads[ordinal - 1].bodyBytes).toBeGreaterThan(file.buffer.length)
  await expect.poll(() => app.page.evaluate(i => window.__r2Uploads[i]?.uploadLoad, ordinal - 1)).toBe(true)
  expect(await app.page.evaluate(i => window.__r2Uploads[i].name, ordinal - 1)).toBe(file.name)
  await expect.poll(() => app.page.evaluate(i => {
    const row = window.__r2Uploads[i]; return row?.uploadTotal > 0 && row.uploadLoaded === row.uploadTotal
  }, ordinal - 1)).toBe(true)
  // Also wait for the app's own progress listener; the observer is not the
  // authority for the composable's completed-upload classification.
  const item = app.page.getByTestId('composer-media-item').filter({ hasText: file.name })
  if (detached) await expect(item).toHaveCount(0)
  else await expect(item.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '100')
}

function accepted(app: App, ordinal = 1, status = 'sent') {
  app.receiver.respond(ordinal, 200, { data: {
    id: `fc777777-7777-4777-8777-${String(ordinal).padStart(12, '0')}`, contact_id: contact,
    direction: 'outgoing', message_type: 'text', content: { body: `Synthetic accepted ${ordinal}` },
    status, created_at: new Date().toISOString(), updated_at: new Date().toISOString(),
  } })
}

async function nativeEnded(app: App, ordinal: number) {
  await expect.poll(() => app.page.evaluate(i => window.__r2Uploads[i]?.ended, ordinal - 1)).toBe(true)
}

async function identityWrite(app: App, key: string, value: string) {
  const writer = await app.context.newPage()
  try {
    await writer.goto(`${app.receiver.origin}/__multipart/empty`)
    await writer.evaluate(({ key, value }) => localStorage.setItem(key, value), { key, value })
    // This is a real cross-document StorageEvent, not event injection.
    await expect.poll(() => app.page.evaluate(k => window.__r2StorageKeys.includes(k), key)).toBe(true)
  } finally { await writer.close() }
}

async function noResend(app: App, expected = 1) {
  // A bounded absence observation after terminal UI/transport state, not the
  // trigger for cancellation. Followed by exact totals in the saved evidence.
  await app.page.waitForTimeout(750)
  expect(app.receiver.uploads).toHaveLength(expected)
  expect(await app.page.evaluate(() => window.__r2Uploads.length)).toBe(expected)
}

test('stops queued multipart work when another tab changes workspace during provider pacing', async ({ app }) => {
  await stage(app); await uploaded(app)
  accepted(app, 1, 'pending')
  await nativeEnded(app, 1)
  await expect(app.page.getByTestId('composer-media-item').nth(1)).toHaveAttribute('data-status', 'ready')
  await identityWrite(app, 'selected_organization_id', otherOrg)
  await expect(app.page.getByTestId('composer-media-item')).toHaveCount(0)
  await expect(app.page.getByTestId('organization-switcher')).toBeEnabled()
  await expect(app.page.locator('[data-sonner-toast]').filter({ hasText: 'Synthetic Upload Recipient' })).toHaveCount(0)
  await noResend(app)
})

for (const change of [
  { label: 'workspace', key: 'selected_organization_id', value: otherOrg },
  { label: 'logical session for the same user', key: 'whm_session_id', value: 'synthetic-session-two' },
]) {
  test(`aborts the native multipart request and never resends after a ${change.label} change`, async ({ app }) => {
    await stage(app); await uploaded(app)
    await identityWrite(app, change.key, change.value)
    await nativeEnded(app, 1)
    expect(await app.page.evaluate(() => window.__r2Uploads[0])).toMatchObject({ aborted: true, status: 0 })
    await expect.poll(() => app.receiver.uploads[0].responseClosed).toBe(true)
    expect(app.receiver.uploads[0]).toMatchObject({ closedBeforeResponseEnd: true, responseStatus: null })
    await expect(app.page.getByTestId('composer-media-item')).toHaveCount(0)
    await expect(app.page.getByTestId('organization-switcher')).toBeEnabled()
    await expect(app.page.locator('[data-sonner-toast]').filter({ hasText: 'Synthetic Upload Recipient' })).toHaveCount(0)
    await noResend(app)
    // Restoring the identity signal must not restart the retired batch.
    await identityWrite(app, change.key, change.key === 'whm_session_id' ? 'synthetic-session-one' : org)
    await noResend(app)
    expect(await app.page.getByText('Synthetic accepted 1', { exact: true }).count()).toBe(0)
  })
}

test('keeps both native uploads pinned to the original recipient across same-workspace chat navigation', async ({ app }) => {
  // Establish a real same-document browser-history entry before opening the
  // modal, which intentionally refuses Escape while sending.
  await app.page.getByTestId('chat-contact').filter({ hasText: 'Synthetic Other Chat' }).click()
  await expect(app.page.getByTestId('chat-header-name')).toHaveText('Synthetic Other Chat')
  await app.page.getByTestId('chat-contact').filter({ hasText: 'Synthetic Upload Recipient' }).click()
  await expect(app.page.getByTestId('chat-header-name')).toHaveText('Synthetic Upload Recipient')
  await stage(app); await uploaded(app)
  await app.page.goBack()
  await expect(app.page.getByTestId('chat-header-name')).toHaveText('Synthetic Other Chat')
  expect(await app.page.evaluate(() => window.__r2Uploads[0].aborted)).toBe(false)
  accepted(app)
  await uploaded(app, 2, second, true)
  accepted(app, 2)
  await nativeEnded(app, 2)
  expect(app.receiver.uploads.map(row => [row.contact, row.organization, row.filename])).toEqual([
    [contact, org, first.name], [contact, org, second.name],
  ])
  await expect(app.page.getByTestId('chat-header-name')).toHaveText('Synthetic Other Chat')
  await expect(app.page.getByTestId('chat-message').filter({ hasText: 'Synthetic accepted' })).toHaveCount(0)
  await noResend(app, 2)
})

test('Stop lets the completed upload settle and leaves the queued file unsent', async ({ app }) => {
  await stage(app); await uploaded(app)
  await app.page.getByTestId('composer-media-stop').click()
  await expect(app.page.getByTestId('composer-media-stop')).toBeDisabled()
  expect(await app.page.evaluate(() => window.__r2Uploads[0].aborted)).toBe(false)
  accepted(app)
  await expect(app.page.getByTestId('composer-media-item').nth(0)).toHaveAttribute('data-status', 'sent')
  await expect(app.page.getByTestId('composer-media-item').nth(1)).toHaveAttribute('data-status', 'ready')
  await expect(app.page.getByTestId('composer-media-send')).toBeEnabled()
  await nativeEnded(app, 1); await noResend(app)
})

for (const response of [
  { label: 'bare 500', status: 500, body: '', json: false },
  { label: 'enveloped 500', status: 500, body: { status: 'error', message: 'Synthetic send failure' }, json: true },
  { label: 'bare 520', status: 520, body: 'Synthetic unknown upstream result', json: false },
  { label: 'lost response', status: 0, body: '', json: false },
]) {
  test(`marks a completed native upload with ${response.label} unconfirmed without retry`, async ({ app }) => {
    await stage(app, [first]); await uploaded(app)
    if (response.status) app.receiver.respond(1, response.status, response.body, response.json)
    else {
      // A true zero-response loss on a proven unused connection, not an empty
      // response on a reused socket that the browser may transparently replay.
      expect(app.receiver.uploads[0].connectionId).toBeGreaterThan(0)
      expect(app.receiver.uploads[0].connectionRequestOrdinal).toBe(1)
      app.receiver.loseResponse(1)
      expect(app.receiver.uploads[0]).toMatchObject({ lossBeforeHeaders: true, responseBytesWrittenAtLoss: 0 })
    }
    await expect(app.page.getByTestId('composer-media-item')).toHaveAttribute('data-status', 'unconfirmed')
    await expect(app.page.getByTestId('composer-media-unconfirmed')).toBeVisible()
    await expect(app.page.getByTestId('composer-media-retry')).toHaveCount(0)
    await expect(app.page.getByTestId('composer-media-send')).toBeDisabled()
    await nativeEnded(app, 1)
    expect(await app.page.evaluate(() => window.__r2Uploads[0].status)).toBe(response.status)
    if (!response.status) expect(await app.page.evaluate(() => window.__r2Uploads[0].error)).toBe(true)
    await noResend(app)
  })
}

test('a completed-upload 422 remains failed and only an explicit Retry sends it again', async ({ app }) => {
  await stage(app, [first]); await uploaded(app)
  app.receiver.respond(1, 422, { message: 'Synthetic pre-dispatch refusal' })
  await expect(app.page.getByTestId('composer-media-item')).toHaveAttribute('data-status', 'failed')
  await expect(app.page.getByTestId('composer-media-retry')).toBeEnabled()
  await nativeEnded(app, 1); await noResend(app)
  await app.page.getByTestId('composer-media-retry').click()
  await uploaded(app, 2, first); accepted(app, 2)
  await nativeEnded(app, 2)
  await expect(app.page.getByTestId('composer-media-item')).toHaveCount(0)
  await expect(app.page.getByTestId('chat-message').filter({ hasText: 'Synthetic accepted 2' })).toHaveCount(1)
  expect(app.receiver.uploads).toHaveLength(2)
})

test('a network refusal before native upload completion stays failed rather than unconfirmed', async ({ app }) => {
  await app.page.route(/\/api\/messages\/media$/, route => route.abort('connectionrefused'))
  // A native refusal can settle before click resolves; assert the released hold
  // after the native terminal event instead of requiring a transient active hold.
  await stage(app, [first], { expectActiveHold: false })
  await nativeEnded(app, 1)
  await expect(app.page.getByTestId('organization-switcher')).toBeEnabled()
  expect(await app.page.evaluate(() => window.__r2Uploads[0])).toMatchObject({
    uploadLoad: false, uploadLoaded: 0, error: true, status: 0,
  })
  expect(app.receiver.uploads).toHaveLength(0)
  await expect(app.page.getByTestId('composer-media-item')).toHaveAttribute('data-status', 'failed')
  await expect(app.page.getByTestId('composer-media-unconfirmed')).toHaveCount(0)
  await expect(app.page.getByTestId('composer-media-retry')).toBeEnabled()
  await app.page.waitForTimeout(750)
  expect(await app.page.evaluate(() => window.__r2Uploads.length)).toBe(1)
  expect(app.receiver.uploads).toHaveLength(0)
})

// These two cases add R3 quote-field/wiring evidence to the same native
// transport fixture. Synthetic acceptance is not backend/provider proof.
test.describe('R3 native attachment quote ownership', () => {
  test.use({ withReplies: true })

  async function reply(app: App, index: number) {
    // The stable message wrapper carries the ID itself, not a private store.
    const row = app.page.locator(`[data-testid="chat-message"][data-message-id="${replyIds[index]}"]`)
    await expect(row).toBeVisible()
    await row.getByTestId('chat-message-reply').click()
    await expect(app.page.getByRole('button', { name: 'Cancel reply', exact: true })).toBeVisible()
  }

  test('only the first native attachment quotes the selected message', async ({ app }) => {
    await reply(app, 0)
    await stage(app)
    await uploaded(app)
    expect(app.receiver.uploads[0].replyTo).toBe(replyIds[0])
    await expect(app.page.getByTestId('composer-media-reply')).toContainText('Synthetic quote Q1')
    accepted(app)
    await uploaded(app, 2, second)
    expect(app.receiver.uploads[1].replyTo).toBeNull()
    await expect(app.page.getByTestId('composer-media-reply')).toHaveCount(0)
    accepted(app, 2)
    await nativeEnded(app, 2)
    await expect(app.page.getByTestId('composer-media-dialog')).toHaveCount(0)
    await expect(app.page.getByRole('button', { name: 'Cancel reply', exact: true })).toHaveCount(0)
    expect(app.receiver.uploads.map(row => row.replyTo)).toEqual([replyIds[0], null])
    await noResend(app, 2)
  })

  test('plain resend and another file retry never inherit the refused quote', async ({ app }) => {
    await reply(app, 0)
    await stage(app)
    await uploaded(app)
    expect(app.receiver.uploads[0].replyTo).toBe(replyIds[0])
    app.receiver.respond(1, 400, { status: 'error', message: 'Reply message not found for this contact' })
    await uploaded(app, 2, second)
    expect(app.receiver.uploads[1].replyTo).toBeNull()
    app.receiver.respond(2, 422, { message: 'Synthetic pre-dispatch refusal' })
    await nativeEnded(app, 2)
    await expect(app.page.getByTestId('composer-media-item')).toHaveCount(2)
    for (const item of await app.page.getByTestId('composer-media-item').all()) {
      await expect(item).toHaveAttribute('data-status', 'failed')
    }
    await expect(app.page.getByTestId('composer-media-reply')).toHaveCount(0)
    await expect(app.page.getByTestId('composer-media-send-without-quote')).toHaveCount(1)
    await noResend(app, 2)
    await app.page.getByTestId('composer-media-send-without-quote').click()
    await uploaded(app, 3, first)
    expect(app.receiver.uploads[2].replyTo).toBeNull()
    accepted(app, 3)
    await nativeEnded(app, 3)
    await expect(app.page.getByTestId('composer-media-item').nth(0)).toHaveAttribute('data-status', 'sent')
    await expect(app.page.getByTestId('composer-media-item').nth(1)).toHaveAttribute('data-status', 'failed')
    await noResend(app, 3)
    await app.page.getByTestId('composer-media-retry').click()
    await uploaded(app, 4, second)
    expect(app.receiver.uploads[3].replyTo).toBeNull()
    accepted(app, 4)
    await nativeEnded(app, 4)
    await expect(app.page.getByTestId('composer-media-dialog')).toHaveCount(0)
    // Closing a failed dialog discards its files, so a new Q2 is selected only
    // after the old batch completes through ordinary controls.
    await reply(app, 1)
    await stage(app, [first])
    await uploaded(app, 5, first)
    expect(app.receiver.uploads[4].replyTo).toBe(replyIds[1])
    accepted(app, 5)
    await nativeEnded(app, 5)
    await expect(app.page.getByTestId('composer-media-dialog')).toHaveCount(0)
    expect(app.receiver.uploads.map(row => row.replyTo)).toEqual([replyIds[0], null, null, null, replyIds[1]])
    await noResend(app, 5)
  })
})
