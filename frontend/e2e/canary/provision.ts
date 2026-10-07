// @ts-nocheck
import { randomBytes } from 'node:crypto'
import { pathToFileURL } from 'node:url'
import { request } from '@playwright/test'
import { loadProfile, readPrivateFile } from './profiles.ts'
import { atomicWritePrivate } from './private-files.mjs'
import { StubClient } from './stub-client.ts'

const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))
const unwrap = body => body.data ?? body
const numericID = () => '999' + (BigInt('0x' + randomBytes(6).toString('hex')) % 1000000000000n).toString().padStart(12, '0')

export class ProductAPI {
  constructor(context, organizationID) { this.context = context; this.organizationID = organizationID }
  async call(method, path, data) {
    if (!path.startsWith('/api/')) throw new Error('Only product API paths are allowed')
    const state = await this.context.storageState()
    const csrf = state.cookies.find(cookie => cookie.name === 'whm_csrf')?.value
    let response
    try {
      response = await this.context.fetch(path, { method, data, maxRedirects: 0,
        headers: { ...(csrf ? { 'X-CSRF-Token': csrf } : {}), ...(this.organizationID ? { 'X-Organization-ID': this.organizationID } : {}) } })
    } catch { throw new Error('Synthetic provisioning request could not complete') }
    if (!response.ok()) throw new Error(`Synthetic provisioning ${method} ${path.split('?')[0]} failed: HTTP ${response.status()}`)
    return unwrap(await response.json())
  }
}

export async function provision(profile = loadProfile()) {
  const context = await request.newContext({ baseURL: profile.origin, timeout: 45000, ignoreHTTPSErrors: false })
  try {
    const api = new ProductAPI(context)
    await api.call('POST', '/api/auth/login', { email: process.env.CANARY_ADMIN_EMAIL || profile.values.admin_email,
      password: process.env.CANARY_ADMIN_PASSWORD || profile.values.admin_password })
    const me = await api.call('GET', '/api/me')
    if (!me.is_super_admin || !me.organization_id) throw new Error('Synthetic bootstrap administrator required')
    if (profile.klinikOrganizationID && profile.klinikOrganizationID !== me.organization_id) throw new Error('Bootstrap organization differs from the private file')
    api.organizationID = me.organization_id
    const stub = new StubClient(profile.stubOrigin, profile.stubControlKey)
    // A saved fixture is deliberately reusable for stress runs. Never infer IDs
    // from display names or touch an unrelated namespace after a partial run.
    if (profile.values.fixture) throw new Error('This private file already has a fixture; run its checks or provision a fresh namespace')
    const nonKlinik = await api.call('POST', '/api/organizations', { name: profile.namespace + '-non-klinik' })
    // A zero-priced, synthetic manual license exercises real entitlement
    // enforcement. It does not disable billing checks or contact a provider.
    const plan = await api.call('POST', '/api/admin/product/plans', {
      code: profile.namespace, name: profile.namespace, status: 'active', is_public: false,
      prices: [{ code: profile.namespace + '-monthly', currency: 'MYR', unit_amount_minor: 0, interval: 'month' }],
      entitlements: [{ key: 'omnichannel.enabled', value_type: 'boolean', value: true }],
    })
    for (const organizationID of [me.organization_id, nonKlinik.id]) {
      await api.call('PUT', `/api/admin/organizations/${organizationID}/subscription`, {
        plan_id: plan.id, price_code: profile.namespace + '-monthly', status: 'active', manual_reference: profile.namespace,
      })
    }
    const createUser = async (organizationID, suffix) => {
      api.organizationID = organizationID
      const { roles } = await api.call('GET', '/api/roles')
      const role = roles.find(role => role.name === 'admin')
      if (!role) throw new Error('Synthetic organization has no admin role')
      const login = { schema_version: 1, email: profile.namespace + '-' + suffix + '@example.test', password: randomBytes(24).toString('hex') }
      const user = await api.call('POST', '/api/users', { ...login, full_name: profile.namespace + '-' + suffix, role_id: role.id, is_active: true, is_super_admin: false })
      if (user.is_super_admin) throw new Error('Canary user must not bypass organization access control')
      return login
    }
    const nonKlinikLogin = await createUser(nonKlinik.id, 'other')
    const klinikLogin = await createUser(me.organization_id, 'klinik')
    api.organizationID = me.organization_id
    const settings = await api.call('GET', '/api/chatbot/settings')
    await api.call('PUT', '/api/chatbot/settings', { ...settings, enabled: false, ai_enabled: false })
    const appID = process.env.CANARY_STUB_APP_ID || profile.values.stub_app_id
    if (!/^\d{1,32}$/.test(appID)) throw new Error('Synthetic stub app ID required')
    await api.call('PUT', '/api/integrations/meta', { enabled: true, config: { app_id: appID, config_id: appID },
      credentials: { app_secret: profile.stubAppSecret, webhook_verify_token: randomBytes(24).toString('hex') } })
    const phoneID = numericID(), businessID = numericID(), displayPhone = numericID()
    await stub.request('POST', '/_control/accounts', { business_account_id: businessID, phone_number_id: phoneID,
      display_phone_number: displayPhone, verified_name: profile.namespace })
    const account = await api.call('POST', '/api/accounts', { name: profile.namespace + '-wa', phone_id: phoneID,
      business_id: businessID, access_token: profile.stubAccessToken, api_version: 'v25.0', auto_read_receipt: false,
      is_default_incoming: true, is_default_outgoing: true })
    if (account.status !== 'active') throw new Error('Synthetic WhatsApp account did not activate')
    const conversations = {}
    let channelAccountID
    for (const label of ['a', 'b']) {
      const phone = numericID(), name = profile.namespace + '-' + label
      await api.call('POST', '/api/contacts', { phone_number: phone, profile_name: name, whatsapp_account: account.name })
      // Actual signed inbound messages create the native/chat mirror and an
      // overflowing transcript. No SQL fixtures or manufactured mirror rows.
      for (let index = 0; index < 16; index++) {
        await stub.request('POST', '/_control/inbound', { phone_number_id: phoneID, from: phone, profile_name: name,
          type: 'text', text: (`Synthetic ${label} history ${index} `).repeat(28), message_id: 'wamid.local.' + randomBytes(20).toString('hex'),
          timestamp: Math.floor(Date.now() / 1000) - 120 + index })
      }
      let conversation
      for (let attempt = 0; attempt < 90; attempt++) {
        const result = await api.call('GET', '/api/conversations?limit=100&search=' + phone)
        conversation = result.conversations?.find(item => item.contact?.phone_number === phone)
        if (conversation?.last_message_at) break
        await sleep(500)
      }
      if (!conversation) throw new Error('Synthetic conversation did not appear')
      channelAccountID = conversation.channel_account_id
      conversations[label] = { conversation_id: conversation.id, contact_id: conversation.contact_id, display_name: name, sender_wa_id: phone }
    }
    // Read cursors belong to the real non-super canary user, not the bootstrap
    // administrator. Clear both fixtures after the final inbound job settles.
    const userContext = await request.newContext({ baseURL: profile.origin, timeout: 45000 })
    try {
      const userAPI = new ProductAPI(userContext, me.organization_id)
      await userAPI.call('POST', '/api/auth/login', klinikLogin)
      for (const fixture of Object.values(conversations)) {
        const { messages } = await userAPI.call('GET', `/api/conversations/${fixture.conversation_id}/messages?limit=100`)
        if (!messages?.length) throw new Error('Synthetic transcript is empty')
        const latest = [...messages].sort((a, b) => Date.parse(b.ingested_at || b.created_at) - Date.parse(a.ingested_at || a.created_at))[0]
        await userAPI.call('POST', `/api/conversations/${fixture.conversation_id}/read`, { last_visible_message_id: latest.id })
      }
    } finally { await userContext.dispose() }
    const fixture = { descriptor: { schema_version: 1, product_origin: profile.origin, fixture_namespace: profile.namespace,
      klinik: { organization_id: me.organization_id, conversations, meta: { business_account_id: businessID, phone_number_id: phoneID,
        display_phone_number: displayPhone, channel_account_id: channelAccountID, legacy_account_id: account.id, legacy_account_name: account.name } },
      non_klinik: { organization_id: nonKlinik.id } }, klinik_login: klinikLogin, non_klinik_login: nonKlinikLogin }
    // Preserve any setup/redeploy fields added while REST provisioning ran.
    const current = readPrivateFile(profile.privateFile)
    if (current.origin_sha256 !== profile.saved.origin_sha256 || current.canary?.origin !== profile.values.origin) throw new Error('Private setup identity changed during provisioning')
    const saved = { ...current, canary: { ...current.canary, origin: profile.origin, namespace: profile.namespace,
      klinik_organization_id: me.organization_id, fixture } }
    atomicWritePrivate(profile.privateFile, JSON.stringify(saved, null, 2) + '\n')
    return fixture
  } finally { await context.dispose() }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  provision().then(() => console.log('Synthetic canary fixture saved privately')).catch(() => { console.error('Synthetic canary provisioning failed; private fixture was not published'); process.exitCode = 1 })
}
