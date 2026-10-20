import { expect, test, type Page, type Route } from '@playwright/test'

// Built ChatView interactions with page-scoped, explicitly synthetic API
// responses. These prove frontend wiring, not backend authorization, provider
// delivery, or transport/handler at-most-once. Every mutation is intercepted;
// unrelated mutations and off-origin traffic are refused.
const id = (n: number) => `e3900000-0000-4000-8000-${String(n).padStart(12, '0')}`
const workspace = id(91)
const loginWorkspace = id(90)
const userId = id(92)
const sourceId = id(1)
const sourceMessageId = id(101)
const fixedTime = Date.parse('2026-10-20T12:00:00Z')

type FixtureContact = {
  id: string
  phone_number: string
  name: string
  profile_name: string
  status: string
  tags: string[]
  metadata: Record<string, unknown>
  unread_count: number
  manual_unread: boolean
  assigned_user_id: string | null
  whatsapp_account: string
  service_window_open: boolean
  identity_review_ai_state?: { known: boolean; ai_allowed: boolean; blocked: boolean }
  last_message_at: string
  created_at: string
  updated_at: string
}

function contact(n: number, name: string, overrides: Partial<FixtureContact> = {}): FixtureContact {
  return {
    id: id(n), phone_number: `6000000${String(n).padStart(4, '0')}`,
    name, profile_name: name, status: 'active', tags: [], metadata: {},
    unread_count: 0, manual_unread: false, assigned_user_id: null,
    whatsapp_account: 'r3-synthetic', service_window_open: true,
    identity_review_ai_state: { known: true, ai_allowed: true, blocked: false },
    last_message_at: new Date(fixedTime).toISOString(),
    created_at: new Date(fixedTime).toISOString(),
    updated_at: new Date(fixedTime).toISOString(),
    ...overrides,
  }
}

function message(n: number, contactId: string, body: string, extra: Record<string, unknown> = {}) {
  return {
    id: id(n), contact_id: contactId, direction: 'incoming',
    message_type: 'text', content: { body }, status: 'received',
    whatsapp_account: 'r3-synthetic',
    created_at: new Date(fixedTime).toISOString(),
    updated_at: new Date(fixedTime).toISOString(),
    ...extra,
  }
}

async function fixture(page: Page, baseURL: string | undefined, options: {
  forward?: boolean
  readOnlyOwn?: boolean
  aging?: boolean
} = {}) {
  if (!baseURL) throw new Error('The owned built-app baseURL is required')
  const origin = new URL(baseURL).origin
  if (!['127.0.0.1', 'localhost', '[::1]'].includes(new URL(origin).hostname)) {
    throw new Error('Synthetic UI fixture requires a loopback built app')
  }
  const user = {
    id: userId, email: 'r3-ui@example.test', full_name: 'Synthetic R3 Agent',
    organization_id: loginWorkspace, organization_name: 'Synthetic Login',
    is_super_admin: false, is_reseller_admin: false,
    role: {
      id: id(93), name: 'agent', is_system: false,
      permissions: [
        { id: 'chat-read', resource: 'chat', action: 'read' },
        ...(!options.readOnlyOwn ? [
          { id: 'chat-write', resource: 'chat', action: 'write' },
          { id: 'contacts-read', resource: 'contacts', action: 'read' },
        ] : []),
      ],
    },
  }
  const targets = [
    contact(2, 'Queued Recipient'), contact(3, 'Safe Retry Recipient'),
    contact(4, 'Ambiguous Recipient'), contact(5, 'Fourth Recipient'),
    contact(6, 'Fifth Recipient'), contact(7, 'Sixth Recipient'),
    contact(8, 'Closed Window', { service_window_open: false }),
    contact(9, 'Blocked Review', {
      identity_review_ai_state: { known: true, ai_allowed: false, blocked: true },
    }),
    contact(10, 'Unknown Review', { identity_review_ai_state: undefined }),
  ]
  const contacts = options.forward
    ? [contact(1, 'Source Conversation', { assigned_user_id: userId }), ...targets]
    : [
        contact(1, 'Mine Incoming', { assigned_user_id: userId, unread_count: 1 }),
        contact(2, 'Manual Unread', { assigned_user_id: id(94), manual_unread: true }),
        contact(3, 'Unassigned Contact'),
      ]
  const messages = new Map(contacts.map(c => [c.id, [
    message(c.id === sourceId ? 101 : 100 + Number(c.id.slice(-3)), c.id, `Synthetic message for ${c.name}`),
  ]]))
  if (options.forward) {
    messages.get(sourceId)!.push(
      message(111, sourceId, '[Message deleted from WhatsApp Business App]'),
      message(112, sourceId, 'Synthetic unsupported sticker', { message_type: 'sticker' }),
    )
  }
  if (options.aging) {
    // The browser clock starts paused at fixedTime+1s. Both messages start
    // 179s old, then cross the unchanged 3-minute threshold on the real
    // component's unchanged 30s interval. No component/store calls or events.
    const at = new Date(fixedTime + 1_000 - 179_000).toISOString()
    messages.get(sourceId)!.push(
      message(121, sourceId, 'Synthetic queued forward', {
        direction: 'outgoing', status: 'pending', is_forwarded: true,
        forward_dispatch: 'queued', created_at: at, updated_at: at,
      }),
      message(122, sourceId, 'Synthetic provider-phase forward', {
        direction: 'outgoing', status: 'pending', is_forwarded: true,
        forward_dispatch: 'provider', forward_dispatch_at: at, created_at: at, updated_at: at,
      }),
    )
  }
  const lists: Array<{ query: Record<string, string>; ids: string[] }> = []
  const reads: Array<{ contactId: string; body: Record<string, unknown> }> = []
  const sends: Array<{ contactId: string; body: Record<string, unknown> }> = []
  const faults: string[] = []
  const refuse = async (route: Route, reason: string) => {
    faults.push(reason)
    await route.abort('blockedbyclient')
  }
  await page.addInitScript(({ user, workspace }) => {
    localStorage.setItem('user', JSON.stringify(user))
    localStorage.setItem('selected_organization_id', workspace)
    localStorage.setItem('locale', 'en')
    localStorage.setItem('color-mode', 'light')
  }, { user, workspace })
  await page.routeWebSocket('**', socket => socket.close())
  await page.route('**/*', async route => {
    const request = route.request()
    const url = new URL(request.url())
    if (url.origin !== origin) return refuse(route, 'off-origin-request')
    const path = url.pathname
    const method = request.method()
    if (!path.startsWith('/api/')) {
      if (method !== 'GET') return refuse(route, 'non-api-mutation')
      return route.continue()
    }
    const ok = (data: unknown) => route.fulfill({ json: { status: 'success', data } })
    if (path === '/api/me' && method === 'GET') return ok(user)
    if (path === '/api/product/entitlements' && method === 'GET') return ok({ mode: 'licensed', entitlements: {} })
    if (path === '/api/auth/ws-token' && method === 'GET') return ok({ token: '' })
    if (path === '/api/me/organizations' && method === 'GET') {
      return ok({ organizations: [
        { organization_id: loginWorkspace, name: 'Synthetic Login', is_default: true, role_name: 'agent' },
        { organization_id: workspace, name: 'Synthetic R3 Workspace', is_default: false, role_name: 'agent' },
      ] })
    }
    if (path === '/api/contacts' || path.startsWith('/api/contacts/')) {
      if (await request.headerValue('x-organization-id') !== workspace) {
        return refuse(route, 'contact-workspace-mismatch')
      }
    }
    if (path === '/api/contacts' && method === 'GET') {
      const query = Object.fromEntries(url.searchParams)
      let rows = options.forward && query.limit === '25' ? targets : contacts
      if (options.readOnlyOwn) rows = rows.filter(c => c.assigned_user_id === userId)
      if (query.unread === 'true') rows = rows.filter(c => c.manual_unread || c.unread_count > 0)
      if (query.assigned === 'me') rows = rows.filter(c => c.assigned_user_id === userId)
      if (query.assigned === 'unassigned') rows = rows.filter(c => c.assigned_user_id === null)
      if (query.search) rows = rows.filter(c => c.name.toLowerCase().includes(query.search.toLowerCase()))
      lists.push({ query, ids: rows.map(c => c.id) })
      return ok({ contacts: rows, total: rows.length, page: 1, limit: Number(query.limit || 50) })
    }
    const match = path.match(/^\/api\/contacts\/([a-f0-9-]+)(?:\/(messages|mark-read))?$/)
    if (match) {
      const c = contacts.find(item => item.id === match[1])
      if (!c) return refuse(route, 'unknown-contact')
      if (!match[2] && method === 'GET') return ok(c)
      if (match[2] === 'messages' && method === 'GET') {
        const rows = messages.get(c.id)!
        return ok({ messages: rows, total: rows.length, has_more: false })
      }
      if (match[2] === 'mark-read' && method === 'POST') {
        const body = request.postDataJSON() as Record<string, unknown>
        reads.push({ contactId: c.id, body })
        const manual = Object.keys(body).length === 1 && ['read', 'unread'].includes(String(body.mark))
        const automatic = Object.keys(body).length === 1 &&
          messages.get(c.id)!.some(m => m.id === body.last_visible_message_id && m.direction === 'incoming')
        if ((!manual && !automatic) || (manual && options.readOnlyOwn)) return refuse(route, 'invalid-read-action')
        if (!options.readOnlyOwn) {
          c.manual_unread = body.mark === 'unread'
          if (!c.manual_unread) c.unread_count = 0
        }
        return ok({ status: 'success', cursor_synced: true, manual_unread: c.manual_unread })
      }
      if (match[2] === 'messages' && method === 'POST') {
        const body = request.postDataJSON() as Record<string, unknown>
        sends.push({ contactId: c.id, body })
        if (!options.forward || ![id(2), id(3), id(4)].includes(c.id) ||
            JSON.stringify(body) !== JSON.stringify({ type: 'text', content: {}, forward_of_message_id: sourceMessageId })) {
          return refuse(route, 'unexpected-send')
        }
        if (c.id === id(3) && sends.filter(s => s.contactId === c.id).length === 1) {
          return route.fulfill({ status: 503, json: {
            status: 'error', error_type: 'forward_temporarily_unavailable', message: 'Synthetic definite preacceptance refusal',
          } })
        }
        if (c.id === id(4)) return route.fulfill({ status: 500, json: {
          status: 'error', message: 'Synthetic ambiguous outcome',
        } })
        const sent = message(200 + sends.length, c.id, 'Synthetic queued forward', {
          direction: 'outgoing', status: 'pending', is_forwarded: true,
          forward_dispatch: 'queued', created_at: new Date().toISOString(),
        })
        messages.get(c.id)!.push(sent)
        return ok(sent)
      }
    }
    // Unrelated shell reads remain synthetic; never forward an unknown
    // mutation to the built server (and therefore never to a provider).
    if (method === 'GET') return ok({})
    return refuse(route, 'unexpected-api-mutation')
  })
  return { lists, reads, sends, faults, contacts }
}

const row = (page: Page, contactId: string) =>
  page.locator(`[data-testid="chat-contact"][data-contact-id="${contactId}"]`)
const bubble = (page: Page, messageId: string) =>
  page.locator(`[data-testid="chat-message"][data-message-id="${messageId}"]`)

async function rowIds(page: Page) {
  return page.getByTestId('chat-contact').evaluateAll(rows =>
    rows.map(r => r.getAttribute('data-contact-id')).sort(),
  )
}

test.describe('R3 built Chat inbox and forwarding — synthetic API outcomes', () => {
  test('filters scoped rows and persists manual read/unread before reopening acknowledges the visible message', async ({ page, baseURL }) => {
    const f = await fixture(page, baseURL)
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.goto('/chat')
    await expect.poll(() => rowIds(page)).toEqual([id(1), id(2), id(3)])
    for (const [filter, query, expected] of [
      ['unread', { unread: 'true' }, [id(1), id(2)]],
      ['mine', { assigned: 'me' }, [id(1)]],
      ['unassigned', { assigned: 'unassigned' }, [id(3)]],
      ['all', {}, [id(1), id(2), id(3)]],
    ] as const) {
      const before = f.lists.length
      await page.getByTestId(`chat-filter-${filter}`).click()
      await expect(page.getByTestId(`chat-filter-${filter}`)).toHaveAttribute('aria-pressed', 'true')
      await expect.poll(() => f.lists.length).toBeGreaterThan(before)
      await expect.poll(() => rowIds(page)).toEqual([...expected])
      const actual = f.lists.at(-1)!.query
      expect({ ...(actual.unread ? { unread: actual.unread } : {}), ...(actual.assigned ? { assigned: actual.assigned } : {}) }).toEqual(query)
    }
    await row(page, id(2)).getByTestId('chat-row-actions').click()
    await page.getByTestId('chat-row-mark-read').click()
    await expect.poll(() => f.reads.filter(r => r.body.mark)).toEqual([{ contactId: id(2), body: { mark: 'read' } }])
    await expect(page).toHaveURL(/\/chat$/)
    await expect(row(page, id(2)).getByTestId('chat-contact-manual-unread')).toHaveCount(0)
    // A fresh GET renders the fixture's acknowledged state, not just an
    // optimistic local dot. This is synthetic persistence, explicitly.
    const beforeReload = f.lists.length
    await page.reload()
    await expect.poll(() => f.lists.length).toBeGreaterThan(beforeReload)
    await expect(row(page, id(2))).toBeVisible()
    await expect(row(page, id(2)).getByTestId('chat-contact-manual-unread')).toHaveCount(0)
    await row(page, sourceId).click()
    await expect(bubble(page, sourceMessageId)).toBeVisible()
    await expect.poll(() => f.reads.filter(r => r.body.last_visible_message_id === sourceMessageId).length).toBe(1)
    await page.getByTestId('chat-more-options').click()
    await page.getByTestId('chat-menu-mark-unread').click()
    await expect(page).toHaveURL(/\/chat$/)
    await expect(row(page, sourceId).getByTestId('chat-contact-manual-unread')).toBeVisible()
    expect(f.reads.filter(r => r.body.mark)).toEqual([
      { contactId: id(2), body: { mark: 'read' } }, { contactId: sourceId, body: { mark: 'unread' } },
    ])
    await expect(page.getByTestId('chat-message-list')).toHaveCount(0)
    await row(page, sourceId).click()
    await expect(bubble(page, sourceMessageId)).toBeVisible()
    await expect.poll(() => f.reads.filter(r => r.body.last_visible_message_id === sourceMessageId).length).toBe(2)
    await expect.poll(() => f.contacts.find(c => c.id === sourceId)!.manual_unread).toBe(false)
    await expect(row(page, sourceId).getByTestId('chat-contact-manual-unread')).toHaveCount(0)
    expect(f.sends).toEqual([])
    expect(f.faults).toEqual([])
  })

  test('hides shared read-state controls and Unassigned for a synthetic read-only own-chat user', async ({ page, baseURL }) => {
    const f = await fixture(page, baseURL, { readOnlyOwn: true })
    await page.goto('/chat')
    await expect.poll(() => rowIds(page)).toEqual([sourceId])
    await expect(page.getByTestId('chat-filter-unassigned')).toHaveCount(0)
    await expect(page.getByTestId('chat-row-actions')).toHaveCount(0)
    await row(page, sourceId).click()
    await expect(bubble(page, sourceMessageId)).toBeVisible()
    await page.getByTestId('chat-more-options').click()
    await expect(page.getByTestId('chat-next-step-menu')).toBeVisible()
    await expect(page.getByTestId('chat-menu-mark-read')).toHaveCount(0)
    await expect(page.getByTestId('chat-menu-mark-unread')).toHaveCount(0)
    expect(f.reads.filter(r => r.body.mark)).toEqual([])
    expect(f.sends).toEqual([])
    expect(f.faults).toEqual([])
  })

  test('confirms named targets, blocks unsafe choices and only explicitly retries the synthetic typed 503 target', async ({ page, baseURL }) => {
    const f = await fixture(page, baseURL, { forward: true })
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.goto(`/chat/${sourceId}`)
    await expect(bubble(page, sourceMessageId)).toBeVisible()
    await expect(bubble(page, id(111)).getByTestId('chat-message-forward')).toHaveCount(0)
    await expect(bubble(page, id(112)).getByTestId('chat-message-forward')).toHaveCount(0)
    await bubble(page, sourceMessageId).getByTestId('chat-message-forward').click()
    const dialog = page.getByTestId('chat-forward-dialog')
    await expect(dialog).toBeVisible()
    const target = (n: number) => dialog.getByTestId(`chat-forward-target-${id(n)}`).getByRole('checkbox')
    for (const n of [8, 9, 10]) await expect(target(n)).toBeDisabled()
    for (const n of [2, 3, 4, 5, 6]) await target(n).check()
    await expect(target(7)).toBeDisabled()
    await target(5).uncheck()
    await target(6).uncheck()
    await expect(target(7)).toBeEnabled()
    await dialog.getByTestId('chat-forward-next').click()
    await expect(dialog.getByTestId('chat-forward-recipients').locator('li')).toHaveText([
      /Queued Recipient/, /Safe Retry Recipient/, /Ambiguous Recipient/,
    ])
    expect(f.sends).toEqual([])
    await dialog.getByTestId('chat-forward-confirm').click()
    const results = dialog.getByTestId('chat-forward-results')
    const result = (name: string) => results.locator('li').filter({ hasText: name })
    await expect(result('Queued Recipient')).toHaveAttribute('data-state', 'queued')
    await expect(result('Safe Retry Recipient')).toHaveAttribute('data-state', 'retryable')
    await expect(result('Ambiguous Recipient')).toHaveAttribute('data-state', 'uncertain')
    await expect(dialog.getByTestId('chat-forward-uncertain')).toBeVisible()
    await expect(result('Ambiguous Recipient').getByRole('button')).toHaveCount(0)
    // Bounded observation of no automatic resend, not an unlimited guarantee.
    await page.waitForTimeout(400)
    expect(f.sends.map(s => s.contactId)).toEqual([id(2), id(3), id(4)])
    await dialog.getByTestId(`chat-forward-retry-${id(3)}`).click()
    await expect(result('Safe Retry Recipient')).toHaveAttribute('data-state', 'queued')
    await expect(result('Ambiguous Recipient')).toHaveAttribute('data-state', 'uncertain')
    await page.waitForTimeout(400)
    expect(f.sends).toEqual([2, 3, 4, 3].map(n => ({
      contactId: id(n), body: { type: 'text', content: {}, forward_of_message_id: sourceMessageId },
    })))
    await dialog.getByTestId('chat-forward-done').click()
    await row(page, id(2)).click()
    await expect(bubble(page, id(201)).getByTestId('chat-message-forwarded')).toBeVisible()
    expect(f.faults).toEqual([])
  })

  test('ages synthetic queued and provider-marked forward chips in the open chat without reload or resend', async ({ page, baseURL }) => {
    await page.clock.install({ time: new Date(fixedTime) })
    await page.clock.pauseAt(new Date(fixedTime + 1_000))
    const f = await fixture(page, baseURL, { aging: true })
    await page.goto(`/chat/${sourceId}`)
    for (const n of [121, 122]) {
      await expect(bubble(page, id(n)).getByTestId('chat-message-forwarded')).toBeVisible()
      await expect(bubble(page, id(n)).locator('[data-forward-delivery-state]')).toHaveCount(0)
    }
    const marker = await page.evaluate(() => performance.timeOrigin)
    await page.clock.fastForward(30_000)
    await expect(bubble(page, id(121)).locator('[data-forward-delivery-state="delayed"]')).toBeVisible()
    await expect(bubble(page, id(122)).locator('[data-forward-delivery-state="uncertain"]')).toBeVisible()
    expect(await page.evaluate(() => performance.timeOrigin)).toBe(marker)
    expect(f.sends).toEqual([])
    expect(f.faults).toEqual([])
  })
})
