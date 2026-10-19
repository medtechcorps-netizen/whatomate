import { test, expect, type Page, type Request, type Response, type Worker } from '@playwright/test'
import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { syntheticPreviewPdf } from './synthetic-preview-pdf'

const org = 'fb222222-2222-4222-8222-222222222222'
const home = 'fb111111-1111-4111-8111-111111111111'
const contactId = 'fb333333-3333-4333-8333-333333333333'
const messageId = 'fb444444-4444-4444-8444-444444444444'
const pdf = syntheticPreviewPdf()
const hash = (bytes: Buffer) => createHash('sha256').update(bytes).digest('hex')
const nodeRequire = createRequire(import.meta.url)
const pdfRoot = dirname(nodeRequire.resolve('pdfjs-dist/package.json'))
const version = (JSON.parse(readFileSync(resolve(pdfRoot, 'package.json'), 'utf8')) as { version: string }).version
const middleware = readFileSync(fileURLToPath(new URL('../../../internal/middleware/middleware.go', import.meta.url)), 'utf8')
const policy = middleware.match(/h\.Set\("Content-Security-Policy", "([^"\r\n]+)"\)/)?.[1]

type Observations = {
  violations: Array<{ directive: string; blocked: string }>
  blobs: Array<{ url: string; type: string; size: number }>
  revoked: string[]
}

async function fixture(page: Page, hold = false) {
  const media: Array<{ method: string; organization: string | undefined }> = []
  const canceled = new Set<Request>()
  let settleMedia!: () => void
  const mediaSettled = new Promise<void>(resolveSettled => { settleMedia = resolveSettled })
  page.on('requestfailed', request => {
    if (new URL(request.url()).pathname === `/api/media/${messageId}`) canceled.add(request)
  })
  let release!: () => void
  const gate = new Promise<void>(resolveGate => { release = resolveGate })
  if (!hold) release()
  const user = { id: 'fb555555-5555-4555-8555-555555555555', email: 'pdf@example.test',
    full_name: 'Synthetic PDF Reviewer', organization_id: home, organization_name: 'PDF Home',
    is_super_admin: true, is_reseller_admin: false }
  await page.addInitScript(({ user, org }) => {
    if (!localStorage.getItem('user')) localStorage.setItem('user', JSON.stringify(user))
    if (!localStorage.getItem('selected_organization_id')) localStorage.setItem('selected_organization_id', org)
    localStorage.setItem('color-mode', 'light')
    const observations: Observations = { violations: [], blobs: [], revoked: [] }
    ;(window as unknown as { __pdfObservations: Observations }).__pdfObservations = observations
    window.addEventListener('securitypolicyviolation', event => observations.violations.push({
      directive: event.effectiveDirective, blocked: event.blockedURI,
    }))
    // Observers call the original functions; no renderer, canvas, Worker or
    // network asset is substituted. Only synthetic blob metadata is recorded.
    const create = URL.createObjectURL.bind(URL)
    const revoke = URL.revokeObjectURL.bind(URL)
    URL.createObjectURL = object => {
      const url = create(object)
      if (object instanceof Blob) observations.blobs.push({ url, type: object.type, size: object.size })
      return url
    }
    URL.revokeObjectURL = url => { observations.revoked.push(url); revoke(url) }
  }, { user, org })
  // API-only synthetic transport. Emitted scripts, workers, fonts, CMaps and
  // decoder modules always come from the actual built server, never a route.
  await page.route('**/api/**', route => route.fulfill({ json: { data: {} } }))
  await page.route(/\/api\/me(?:\?.*)?$/, route => route.fulfill({ json: { data: user } }))
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, route =>
    route.fulfill({ json: { data: { mode: 'licensed', entitlements: {} } } }))
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, route => route.fulfill({ json: { data: { token: '' } } }))
  await page.route(/\/api\/organizations(?:\?.*)?$/, route => route.fulfill({ json: { data: {
    organizations: [{ id: org, name: 'PDF Selected' }, { id: home, name: 'PDF Home' }],
  } } }))
  const contact = { id: contactId, phone_number: '60000000001', name: 'Synthetic PDF Contact',
    profile_name: 'Synthetic PDF Contact', status: 'active', tags: [], metadata: {}, unread_count: 0,
    whatsapp_account: 'synthetic-pdf', identity_review_ai_state: { known: true, ai_allowed: true, blocked: false },
    last_message_at: '2026-10-01T00:00:00Z', created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-01T00:00:00Z' }
  await page.route(/\/api\/contacts(?:\?.*)?$/, route => route.fulfill({ json: { data: { contacts: [contact], total: 1 } } }))
  await page.route(new RegExp(`/api/contacts/${contactId}(?:\\?.*)?$`), route => route.fulfill({ json: { data: contact } }))
  await page.route(new RegExp(`/api/contacts/${contactId}/messages(?:\\?.*)?$`), route => route.fulfill({ json: { data: {
    messages: route.request().headers()['x-organization-id'] === org ? [{ id: messageId, contact_id: contactId,
      direction: 'incoming', message_type: 'document', content: { body: '' },
      media_url: `organizations/${org}/messages/documents/synthetic-preview.pdf`,
      media_mime_type: 'application/octet-stream', media_filename: 'synthetic-preview.pdf',
      status: 'received', created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-01T00:00:00Z' }] : [], has_more: false,
  } } }))
  await page.route(new RegExp(`/api/media/${messageId}(?:\\?.*)?$`), async route => {
    const request = route.request()
    media.push({ method: request.method(), organization: request.headers()['x-organization-id'] })
    if (request.method() !== 'GET' || request.headers()['x-organization-id'] !== org) {
      await route.fulfill({ status: 404, json: { message: 'Wrong synthetic workspace' } })
      return
    }
    try {
      await gate
      await route.fulfill({ contentType: 'application/octet-stream', body: pdf })
    } catch (error) {
      // Suppress only the independently observed canceled request from the
      // intentional navigation case, not arbitrary synthetic transport errors.
      if (!hold || !canceled.has(request)) throw error
    } finally { settleMedia() }
  })
  return { media, release, mediaSettled }
}

async function openBuiltChat(page: Page) {
  expect(version).toBe('6.3.289')
  expect(policy).toBeTruthy()
  const response = await page.goto(`/chat/${contactId}`)
  expect(response?.status()).toBe(200)
  expect(response?.headers()['content-security-policy']).toBe(policy)
  expect(policy).not.toContain("'unsafe-eval'")
  expect(policy).not.toContain("'wasm-unsafe-eval'")
  const scripts = await page.locator('script[src]').evaluateAll(elements => elements.map(el => (el as HTMLScriptElement).src))
  expect(scripts.length).toBeGreaterThan(0)
  expect(scripts.every(url => new URL(url).origin === new URL(page.url()).origin && /\/assets\/.+\.js$/.test(new URL(url).pathname))).toBe(true)
  expect(scripts.some(url => /@vite|\/src\//.test(url))).toBe(false)
  return page.locator(`[data-message-id="${messageId}"] [data-testid="chat-message-media"]`)
}

async function observed(page: Page) {
  return page.evaluate(() => (window as unknown as { __pdfObservations: Observations }).__pdfObservations)
}

test('built PDF uses real worker, CMap, font and both JS decoders under production CSP', async ({ page, request }, testInfo) => {
  const state = await fixture(page)
  const responses: Response[] = []
  const workers: Worker[] = []
  const closed = new Set<Worker>()
  const failures: string[] = []
  const requestPaths: string[] = []
  let requestObservationOverflow = false
  page.context().on('request', request => {
    const path = new URL(request.url()).pathname
    if (requestPaths.length >= 256 || path.length > 512) requestObservationOverflow = true
    else requestPaths.push(path)
  })
  // BrowserContext also observes requests initiated by the real PDF worker.
  page.context().on('response', response => responses.push(response))
  page.on('worker', worker => { workers.push(worker); worker.on('close', () => closed.add(worker)) })
  page.on('pageerror', error => failures.push(error.message))
  const media = await openBuiltChat(page)
  const baselineViolations = (await observed(page)).violations
  await media.getByRole('button').click()
  const preview = page.getByTestId('chat-media-preview')
  await expect(preview).toHaveAttribute('data-preview-kind', 'pdf')
  const first = preview.locator('[data-testid="chat-media-preview-page"][data-page="1"]')
  await expect(first).toHaveAttribute('data-page-state', 'rendered')
  const pixels = await first.locator('canvas').evaluate(canvas => {
    const c = canvas as HTMLCanvasElement
    const context = c.getContext('2d')!
    const at = (x: number, y: number) => [...context.getImageData(Math.floor(c.width * x), Math.floor(c.height * y), 1, 1).data]
    const symbol = context.getImageData(Math.floor(c.width * .05), Math.floor(c.height * .10),
      Math.floor(c.width * .20), Math.floor(c.height * .20)).data
    let symbolInk = 0
    for (let i = 0; i < symbol.length; i += 4) {
      if (symbol[i] < 100 && symbol[i + 1] < 100 && symbol[i + 2] < 100 && symbol[i + 3] === 255) symbolInk++
    }
    return { red: at(.175, .825), whiteFax: at(.60, .70), blackFax: at(.80, .70), symbolInk, width: c.width, height: c.height }
  })
  expect(pixels.red).toEqual([255, 0, 0, 255])
  expect(pixels.whiteFax).toEqual([255, 255, 255, 255])
  expect(pixels.blackFax).toEqual([0, 0, 0, 255])
  expect(pixels.symbolInk).toBeGreaterThan(10)
  expect(pixels.width * pixels.height).toBeLessThanOrEqual(3_000_000)
  expect(workers).toHaveLength(1)
  const workerURL = workers[0].url()
  expect(new URL(workerURL).origin).toBe(new URL(page.url()).origin)
  expect(new URL(workerURL).pathname).toMatch(/^\/assets\/pdf\.worker\.min-[\w-]+\.mjs$/)
  const runtimeBase = `/assets/pdfjs-${version}/`
  const workerResponse = await request.get(workerURL)
  expect(workerResponse.ok()).toBe(true)
  expect(hash(await workerResponse.body())).toBe(hash(readFileSync(resolve(pdfRoot, 'legacy/build/pdf.worker.min.mjs'))))
  const second = preview.locator('[data-testid="chat-media-preview-page"][data-page="2"]')
  await second.scrollIntoViewIfNeeded()
  await expect(second).toHaveAttribute('data-page-state', 'rendered')
  const secondPixels = await second.locator('canvas').evaluate(canvas => {
    const c = canvas as HTMLCanvasElement
    const context = c.getContext('2d')!
    const at = (x: number, y: number) => [...context.getImageData(Math.floor(c.width * x), Math.floor(c.height * y), 1, 1).data]
    const dingbats = context.getImageData(Math.floor(c.width * .05), Math.floor(c.height * .10),
      Math.floor(c.width * .20), Math.floor(c.height * .20)).data
    let dingbatsInk = 0
    for (let i = 0; i < dingbats.length; i += 4) {
      if (dingbats[i] < 100 && dingbats[i + 1] < 100 && dingbats[i + 2] < 100 && dingbats[i + 3] === 255) dingbatsInk++
    }
    return { blue: at(.175, .825), greenJPX: at(.60, .70), magentaJPX: at(.80, .70), dingbatsInk }
  })
  expect(secondPixels.blue).toEqual([0, 0, 255, 255])
  expect(secondPixels.greenJPX).toEqual([0, 255, 0, 255])
  expect(secondPixels.magentaJPX).toEqual([255, 0, 255, 255])
  expect(secondPixels.dingbatsInk).toBeGreaterThan(10)
  const used = ['cmaps/UniJIS-UCS2-H.bcmap', 'standard_fonts/FoxitSymbol.pfb',
    'standard_fonts/FoxitDingbats.pfb', 'wasm/jbig2_nowasm_fallback.js', 'wasm/openjpeg_nowasm_fallback.js']
  for (const file of used) {
    // These must be actual browser/worker requests, not request.get probes.
    const response = responses.find(item => new URL(item.url()).pathname === runtimeBase + file)
    expect(response, `real renderer fetched ${file}`).toBeTruthy()
    expect(response!.status()).toBe(200)
    expect(hash(await response!.body())).toBe(hash(readFileSync(resolve(pdfRoot, file))))
  }
  const downloadLink = preview.getByTestId('chat-media-preview-download')
  const url = await downloadLink.getAttribute('href')
  expect((await observed(page)).blobs.find(blob => blob.url === url)).toMatchObject({ type: 'application/octet-stream', size: pdf.length })
  const downloadEvent = page.waitForEvent('download')
  await downloadLink.click()
  const download = await downloadEvent
  expect(download.suggestedFilename()).toBe('synthetic-preview.pdf')
  const stream = await download.createReadStream()
  expect(stream).toBeTruthy()
  const chunks: Buffer[] = []
  for await (const chunk of stream!) chunks.push(Buffer.from(chunk))
  expect(hash(Buffer.concat(chunks))).toBe(hash(pdf))
  expect(page.context().pages()).toHaveLength(1)
  await expect(page.locator('iframe,object,embed')).toHaveCount(0)
  await page.keyboard.press('Escape')
  await expect(preview).not.toBeVisible()
  await expect.poll(() => closed.has(workers[0])).toBe(true)
  // Close destroys the worker, while the authorized opaque media remains
  // reusable. Reopen must create a real new worker, without another API read.
  await media.getByRole('button').click()
  await expect(preview.locator('[data-page="1"]')).toHaveAttribute('data-page-state', 'rendered')
  expect(workers).toHaveLength(2)
  expect(state.media).toEqual([{ method: 'GET', organization: org }])
  await page.keyboard.press('Escape')
  await expect.poll(() => closed.size).toBe(2)
  // A refused/aborted fetch may never produce a Response. Observe requests
  // through both opens so fallback after a failed WASM fetch cannot pass.
  expect(requestObservationOverflow).toBe(false)
  expect(requestPaths.some(path => /\.wasm$/i.test(path))).toBe(false)
  // Keep pre-existing application bootstrap violations visible in evidence;
  // the real PDF path may not introduce any additional violation.
  expect((await observed(page)).violations).toEqual(baselineViolations)
  expect(failures).toEqual([])
  await testInfo.attach('pdf-built-evidence', { contentType: 'application/json', body: Buffer.from(JSON.stringify({
    fixtureSha256: hash(pdf), workerURL, runtimeFilesUsed: used, requestPaths, requestObservationOverflow,
    pixels, secondPixels, workerStarts: workers.length,
    workerCloses: closed.size, media: state.media, csp: policy, baselineViolations, violations: (await observed(page)).violations,
    decoderCoverage: 'CCITTFaxDecode and JPXDecode rendered; no JBIG2-bitstream, ICC/qcms or WASM execution claim',
  })) })
})

test('workspace reload cancels a held synthetic PDF and never displays its old bytes', async ({ page }) => {
  const state = await fixture(page, true)
  const media = await openBuiltChat(page)
  const baselineViolations = (await observed(page)).violations
  try {
    await media.getByRole('button').click()
    await expect.poll(() => state.media.length).toBe(1)
    const canceledMedia = page.waitForEvent('requestfailed', {
      predicate: request => new URL(request.url()).pathname === `/api/media/${messageId}`,
    })
    const homeTranscript = page.waitForResponse(response =>
      new URL(response.url()).pathname === `/api/contacts/${contactId}/messages` &&
      response.request().headers()['x-organization-id'] === home)
    await page.getByTestId('organization-switcher').click()
    const navigated = page.waitForEvent('domcontentloaded')
    await page.getByRole('option', { name: 'PDF Home', exact: true }).click()
    await navigated
    const canceledRequest = await canceledMedia
    expect(canceledRequest.failure()?.errorText).toBe('net::ERR_ABORTED')
    const homeResponse = await homeTranscript
    expect(homeResponse.status()).toBe(200)
    await homeResponse.finished()
    await expect.poll(() => page.evaluate(() => localStorage.getItem('selected_organization_id'))).toBe(home)
    state.release()
    await state.mediaSettled
    await expect(page.locator(`[data-message-id="${messageId}"]`)).toHaveCount(0)
    await expect(page.getByTestId('chat-media-preview')).toHaveCount(0)
    expect(state.media).toEqual([{ method: 'GET', organization: org }])
    expect((await observed(page)).blobs).toEqual([])
    expect((await observed(page)).violations).toEqual(baselineViolations)
  } finally { state.release() }
})
