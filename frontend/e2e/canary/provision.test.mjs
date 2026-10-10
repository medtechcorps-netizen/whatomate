import assert from 'node:assert/strict'
import test from 'node:test'
import { ProductAPI, markFixtureConversationRead } from './provision.ts'

const conversationID = '10000000-0000-4000-8000-000000000001'
const organizationID = '20000000-0000-4000-8000-000000000001'
const firstID = '30000000-0000-4000-8000-000000000002'
const secondID = '30000000-0000-4000-8000-000000000001'
const envelope = (id, fields = {}) => ({ message: { id, ...fields }, parts: null })

function transport(messages) {
  const calls = []
  const api = new ProductAPI({
    storageState: async () => ({ cookies: [] }),
    fetch: async (path, options) => {
      calls.push({ path, method: options.method, data: options.data, organization: options.headers['X-Organization-ID'] })
      assert.ok(calls.length <= 2)
      return { ok: () => true, json: async () => ({ data: options.method === 'GET' ? { messages } : { unread_count: 0 } }) }
    },
  }, organizationID)
  return { api, calls }
}

test('fixture cursor uses the nested message UUID and preserves server ingestion/UUID order', async () => {
  for (const messages of [
    // Created/provider time differs from the authoritative ingestion order.
    [envelope(firstID, { ingested_at: '2026-10-08T02:00:00Z', created_at: '2026-10-07T00:00:00Z' }),
      envelope(secondID, { ingested_at: '2026-10-08T01:00:00Z', created_at: '2026-10-08T00:00:00Z' })],
    // Date.parse rounds away the precision used by the database cursor.
    [envelope(secondID, { ingested_at: '2026-10-08T00:00:00.000002Z' }),
      envelope(firstID, { ingested_at: '2026-10-08T00:00:00.000001Z' })],
    // Equal ingestion timestamps are already ordered by UUID descending.
    [envelope(firstID, { ingested_at: '2026-10-08T00:00:00Z' }),
      envelope(secondID, { ingested_at: '2026-10-08T00:00:00Z' })],
    // Legacy rows may have no ingestion timestamp; the API uses created_at.
    [envelope(firstID, { created_at: '2026-10-08T00:00:00Z' })],
  ]) {
    const { api, calls } = transport(messages)
    await markFixtureConversationRead(api, conversationID)
    assert.deepEqual(calls, [
      { method: 'GET', path: `/api/conversations/${conversationID}/messages?limit=100`, data: undefined, organization: organizationID },
      { method: 'POST', path: `/api/conversations/${conversationID}/read`, data: { last_visible_message_id: messages[0].message.id }, organization: organizationID },
    ])
  }
})

test('empty, flat and malformed message responses cannot issue a cursor mutation', async () => {
  for (const messages of [undefined, null, {}, [], [null], [[]], [{}],
    [{ id: firstID, created_at: '2026-10-08T00:00:00Z' }],
    [{ message: null }], [{ message: [] }], [{ message: {} }],
    [envelope(123)], [envelope('invalid')], [envelope('00000000-0000-0000-0000-000000000000')],
    [envelope(firstID), { message: {} }],
  ]) {
    const { api, calls } = transport(messages)
    await assert.rejects(markFixtureConversationRead(api, conversationID), /^Error: Synthetic transcript is empty or malformed$/)
    assert.deepEqual(calls.map(call => call.method), ['GET'])
  }
})
