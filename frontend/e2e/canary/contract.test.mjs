import assert from 'node:assert/strict'
import { createHash, createHmac } from 'node:crypto'
import { readFileSync, mkdtempSync, mkdirSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { tmpdir } from 'node:os'
import test from 'node:test'
import { UI_CHECKS, CHECK_EXECUTION_ORDER } from './checks.ts'
import { readPrivateFile, validateProfile } from './profiles.ts'
import { controlHeaders, StubClient } from './stub-client.ts'
import { LiveProductScenario } from './scenario.ts'
import { ProductAPI } from './provision.ts'
import PrivateReporter from './private-reporter.ts'
import { verifyReport } from './verify-report.mjs'
import { assertPrivateLocation, assertPrivateOutput, atomicWritePrivate, checkoutRoot, childEnvironment, refuseAmbient, secureWindowsPath, validateReportPath } from './private-files.mjs'

const file = path => readFileSync(new URL(path, import.meta.url), 'utf8')
// Captured from the immutable Git blob named in the manifest, never generated
// from the current port. This retains the original byte-level proof after the
// retired production service is removed, with no runtime/history dependency.
const provenance = JSON.parse(file('./source-provenance.json'))
const hash = value => createHash('sha256').update(value).digest('hex')
const local = { profile: 'local', origin: 'http://127.0.0.1:8080', namespace: 'rereply-local-unit', stubOrigin: 'http://127.0.0.1:8090' }
test('profile refuses production and noncanonical addresses before any I/O', () => {
  assert.doesNotThrow(() => validateProfile(local))
  for (const origin of ['https://app.rereply.app', 'http://app.rereply.app', 'https://rereply.app', 'https://foo.rereply.app', 'https://app.rereply.app.evil.test', 'http://127.0.0.1:8080/path', 'http://user:pass@127.0.0.1:8080', 'http://127.0.0.1:8080/', 'http://127.0.0.1:8080?x=1']) {
    assert.throws(() => validateProfile({ ...local, origin }))
  }
  for (const namespace of ['rereply-canary', 'rereply-staging-unit', '', '../../outside']) assert.throws(() => validateProfile({ ...local, namespace }))
  assert.throws(() => validateProfile({ ...local, stubOrigin: 'https://graph.facebook.com' }))
  assert.throws(() => validateProfile({ ...local, profile: 'production' }))
})
test('staging requires HTTPS and the exact setup origin hash and stub prefix', () => {
  const origin = 'https://synthetic-staging.example.test'
  const staging = { profile: 'staging', origin, namespace: 'rereply-staging-unit', stubOrigin: origin + '/_stub', originHash: createHash('sha256').update(origin).digest('hex') }
  assert.doesNotThrow(() => validateProfile(staging))
  for (const change of [{ originHash: undefined }, { originHash: '0'.repeat(64) }, { origin: 'https://another.example.test' }, { stubOrigin: 'https://another.example.test/_stub' }, { stubOrigin: origin + '/_control' }]) assert.throws(() => validateProfile({ ...staging, ...change }))
})
test('check names, execution order, timeouts and late-layout constants match historical provenance', () => {
  assert.equal(provenance.schema_version, 1)
  assert.equal(provenance.source.repository, 'medtechcorps-netizen/whatomate')
  assert.equal(provenance.source.commit, '313d7b8bcdfc5070bc2af7dbd72d839b49b1ec8e')
  assert.equal(provenance.source.git_blob_sha1, '0d2bf73f4ec9a41ea30a70680633e30fb68a4d71')
  assert.equal(provenance.source.sha256, '8193c95335e1f4a7b5df34d98bb3693211d815602b4b82a7e544f9975fc2ba7b')
  for (const [name, expected] of Object.entries({ UI_CHECKS, CHECK_EXECUTION_ORDER })) {
    assert.deepEqual(provenance.checks[name], expected)
  }
  const checks = file('./checks.ts')
  const constants = ['DEFAULT_TIMEOUT_MS', 'DRIVER_EXECUTION_TIMEOUT_MS', 'DEADLINE_CLEANUP_GRACE_MS', 'BOTTOM_TOLERANCE_PX', 'LATE_LAYOUT_VIEWPORT', 'LATE_LAYOUT_NARROW_VIEWPORT', 'LATE_LAYOUT_SETTLE_MS', 'LATE_LAYOUT_FRAME_SETTLE_MS', 'NATIVE_SELECTION_SETTLE_TIMEOUT_MS']
  assert.deepEqual(Object.keys(provenance.constants), constants)
  for (const name of constants) {
    const pattern = new RegExp(`const ${name} = [\\s\\S]*?;`)
    const match = checks.match(pattern)
    assert.ok(match, `missing original constant: ${name}`)
    assert.equal(match[0], provenance.constants[name], name)
  }
})
function verifyScenarioProvenance(source) {
  const port = source.replaceAll('await this.deliverWebhook(', 'await sendWebhook(')
  assert.deepEqual(Object.keys(provenance.methods_sha256), [...UI_CHECKS, 'requireDenied'])
  for (const name of [...UI_CHECKS, 'requireDenied']) {
    const pattern = new RegExp(`  async ${name}\\([^]*?(?=\\n  async |\\n}\\n)`)
    const match = port.match(pattern)
    assert.ok(match, `missing original method: ${name}`)
    assert.equal(hash(match[0]), provenance.methods_sha256[name], name)
  }
  // The large layout, identity and polling helpers are equally significant.
  const names = ['legacyChannelAccountName', 'validateConversation', 'metaTextMessage', 'pageFetch', 'validateLiveFixtureConversation', 'validateRefreshedServiceWindow', 'loadLiveFixtureConversation', 'login', 'documentCount', 'selectOmnichannelConversation', 'selectNativeContact', 'scrollMetrics', 'scrollToBottom', 'metricsAtBottom', 'requireAtBottom', 'requireNativeSelectionAtBottom', 'setScrollAnchoring', 'requireLateLayoutPreservesLatest', 'waitForApiMessage', 'waitForApiMessageBody', 'messageIDs', 'attentionCount', 'pollValue']
  assert.deepEqual(Object.keys(provenance.helpers_sha256), names)
  for (const name of names) {
    const pattern = new RegExp(`(?:export )?(?:async )?function ${name}\\([^]*?(?=\\n(?:export |async )?function |\\nexport class |$)`)
    const match = port.match(pattern)
    assert.ok(match, `missing original helper: ${name}`)
    assert.equal(hash(match[0]), provenance.helpers_sha256[name], name)
  }
}
test('all 13 scenario methods retain the original assertion bytes', () => {
  verifyScenarioProvenance(file('./scenario.ts'))
})
test('historical provenance refuses changed assertions and missing helpers', () => {
  const source = file('./scenario.ts')
  for (const changed of [
    source.replace('  async klinik_whatsapp_outbound(', '  async renamed_outbound('),
    source.replace('function scrollMetrics(', 'function missingScrollMetrics('),
    source.replace('  async requireDenied(', '  async requireDenied( /* altered assertion */ '),
  ]) {
    assert.notEqual(changed, source)
    assert.throws(() => verifyScenarioProvenance(changed))
  }
})
test('Playwright package, lock roots, installed package and container pins agree exactly', () => {
  const version = JSON.parse(file('../../package.json')).devDependencies['@playwright/test']
  const lock = JSON.parse(file('../../package-lock.json'))
  assert.equal(version, '1.57.0')
  assert.equal(lock.packages[''].devDependencies['@playwright/test'], version)
  assert.equal(lock.packages['node_modules/@playwright/test'].version, version)
  assert.equal(JSON.parse(file('../../node_modules/@playwright/test/package.json')).version, version)
  assert.equal(lock.packages['node_modules/playwright'].version, version)
  assert.equal(lock.packages['node_modules/playwright-core'].version, version)
  // PR9 adds e2e-staging. Its exact version and immutable digest must join this
  // contract when that branch lands; PR11 also works before that dependency.
  const ship = file('../../../.github/workflows/ship.yml')
  const staging = ship.match(/^  e2e-staging:\r?\n[\s\S]*?(?=^  [a-zA-Z0-9_-]+:|(?![\s\S]))/m)
  if (staging) {
    const images = staging[0].match(/^      image: .+$/gm) || []
    assert.deepEqual(images, [`      image: mcr.microsoft.com/playwright:v${version}-noble@sha256:8fb7af3bb488c51364d6554876a8eddf377736608327dbdf4177b4901faf7bc9`])
  }
})
function report() { return { suites: [{ specs: UI_CHECKS.map(title => ({ title, tests: [{ expectedStatus: 'passed', status: 'expected', results: [{ status: 'passed', retry: 0 }] }] })) }], errors: [] } }
test('report verifier requires exactly 13 named single-attempt passes', () => {
  assert.equal(verifyReport(report()), true)
  for (const change of [
    r => r.suites[0].specs.pop(), r => r.suites[0].specs.push(r.suites[0].specs[0]),
    r => { r.suites[0].specs[0].title = 'renamed' }, r => { r.suites[0].specs[0].tests[0].results[0].status = 'skipped' },
    r => { r.suites[0].specs[0].tests[0].results[0].retry = 1 }, r => { r.suites[0].specs[0].tests[0].expectedStatus = 'failed' },
    r => r.suites[0].specs[0].tests[0].results.push({ status: 'passed', retry: 1 }), r => r.errors.push('setup error'),
  ]) { const bad = report(); change(bad); assert.throws(() => verifyReport(bad)) }
})
test('stub HMAC signs the upstream control path, query and exact body', () => {
  const key = 'synthetic-control-key-for-unit-tests-only'
  const body = '{"text":"synthetic"}', path = '/_control/journal?after=23&limit=1000'
  const expected = createHmac('sha256', key).update(['GET', path, '1700000000', 'unit_nonce_12345678', createHash('sha256').update(body).digest('hex')].join('|')).digest('hex')
  assert.equal(controlHeaders(key, 'GET', path, body, '1700000000', 'unit_nonce_12345678')['X-Stub-Mac'], expected)
  assert.throws(() => controlHeaders('short', 'GET', path, ''))
})
test('denial assertion rejects a provider send even when Graph refused it', async () => {
  const client = new StubClient(local.stubOrigin, 'synthetic-control-key-for-unit-tests-only')
  client.journal = async () => ({ entries: [{ kind: 'graph', route: 'messages', method: 'POST', status: 400 }] })
  await assert.rejects(client.assertNoSend(0))
})

test('journal refuses restart, lost pages and noncontiguous evidence', async () => {
  for (const value of [
    { entries: [], latest: 0 }, { entries: [], latest: 11 },
    { entries: [{ seq: 12 }], latest: 12 },
    { entries: [{ seq: 11 }, { seq: 13 }], latest: 13 },
    { entries: [{ seq: 11 }, { seq: 11 }], latest: 11 },
  ]) {
    const client = new StubClient(local.stubOrigin, 'synthetic-control-key-for-unit-tests-only')
    client.request = async () => value
    await assert.rejects(client.assertNoSend(10))
  }
  const client = new StubClient(local.stubOrigin, 'synthetic-control-key-for-unit-tests-only')
  client.request = async () => ({ entries: [{ seq: 11 }, { seq: 12 }], latest: 12 })
  assert.deepEqual((await client.journal(10)).entries.map(entry => entry.seq), [11, 12])
})

test('scenario restores the saved synthetic account before browser launch', async () => {
  const calls = [], meta = { business_account_id: '999000000001', phone_number_id: '999000000002', display_phone_number: '999000000003' }
  const scenario = new LiveProductScenario({ descriptor: { fixture_namespace: local.namespace, klinik: { meta } },
    stub: { request: async (...args) => calls.push(args) }, chromium: { launch: async () => { throw new Error('intentional browser stop') } } })
  await assert.rejects(scenario.prepare(), /intentional browser stop/)
  assert.deepEqual(calls, [['POST', '/_control/accounts', { ...meta, verified_name: local.namespace }]])
})

test('failure progress and provisioning errors never serialize sensitive call logs', async t => {
  const secret = 'synthetic-password-and-session-cookie-marker', output = []
  t.mock.method(console, 'log', value => output.push(value))
  t.mock.method(console, 'error', value => output.push(value))
  const reporter = new PrivateReporter()
  reporter.onError(new Error(`locator.fill(\"${secret}\")`))
  reporter.onTestEnd({ title: UI_CHECKS[0], expectedStatus: 'passed', outcome: () => 'unexpected' },
    { status: 'failed', retry: 0, error: { message: secret, stack: secret }, stdout: [secret], attachments: [{ body: secret }] })
  assert.equal(output.some(value => value.includes(secret)), false)
  assert.equal(reporter.printsToStdio(), true)
  const api = new ProductAPI({ storageState: async () => ({ cookies: [] }), fetch: async () => { throw new Error(secret) } })
  await assert.rejects(api.call('POST', '/api/auth/login', { password: secret }), error => !error.message.includes(secret))
})
test('stress verifier counts every repeated check and late-layout iteration', () => {
  const normal = report().suites[0].specs
  const specs = Array.from({ length: 120 }, () => structuredClone(normal)).flat()
  for (let index = 1; index <= 80; index++) for (const name of ['omnichannel_late_layout_autoscroll', 'native_chat_late_layout_autoscroll']) {
    specs.push({ ...structuredClone(normal[0]), title: `${name} ${index}` })
  }
  const value = { specs }
  assert.equal(verifyReport(value, { stress: true }), true)
  assert.throws(() => verifyReport(value))
  specs.pop()
  assert.throws(() => verifyReport(value, { stress: true }))
})
test('ambient application and relay overrides are refused before local I/O and stripped from children', () => {
  for (const key of ['WHATOMATE_DATABASE__URL', 'WHATOMATE_CONFIG', 'STUB_CALLBACK_ORIGIN', 'META_RELAY_DATABASE_URL', 'GMAIL_RELAY_TOKEN', 'whatomate_database__url']) {
    assert.throws(() => refuseAmbient({ [key]: 'sensitive-value-never-print' }))
    assert.equal(childEnvironment({ [key]: 'sensitive-value-never-print', PATH: 'test-path' })[key], undefined)
  }
  assert.doesNotThrow(() => refuseAmbient({ CANARY_PROFILE: 'local' }))
  assert.equal(childEnvironment({ PATH: 'test-path' }, { STUB_ENVIRONMENT: 'local' }).STUB_ENVIRONMENT, 'local')
})
test('private output paths refuse checkout roots, other Git worktrees and junction/symlink escapes', () => {
  assert.throws(() => assertPrivateLocation(join(checkoutRoot, 'private.json')))
  assert.throws(() => assertPrivateLocation('relative.json'))
  const directory = mkdtempSync(join(tmpdir(), 'canary-contract-'))
  try {
    const repo = join(directory, 'repo'); mkdirSync(repo); writeFileSync(join(repo, '.git'), 'synthetic marker')
    assert.throws(() => assertPrivateLocation(join(repo, 'secret.json')))
    const link = join(directory, 'link')
    symlinkSync(repo, link, process.platform === 'win32' ? 'junction' : 'dir')
    assert.throws(() => assertPrivateLocation(join(link, 'secret.json')))
  } finally {
    assert.ok(resolve(directory).startsWith(resolve(tmpdir()) + (process.platform === 'win32' ? '\\' : '/') + 'canary-contract-'))
    rmSync(directory, { recursive: true })
  }
})
test('atomic private replacement leaves one complete document and no temporary files', () => {
  const directory = mkdtempSync(join(tmpdir(), 'canary-contract-'))
  secureWindowsPath(directory, true)
  try {
    const child = join(directory, 'directory'); mkdirSync(child, { mode: 0o700 })
    assert.throws(() => assertPrivateOutput(child))
    const path = join(directory, 'private.json')
    atomicWritePrivate(path, JSON.stringify({ from_setup: 'retained', canary: {} }))
    assert.throws(() => validateReportPath(path, directory + '/./private.json'))
    if (process.platform === 'win32') {
      assert.throws(() => validateReportPath(path, path.toUpperCase()))
      assert.throws(() => validateReportPath(path, path.replaceAll('\\', '/')))
    }
    assert.equal(validateReportPath(path, join(directory, 'report.json')), join(directory, 'report.json'))
    const original = JSON.parse(readFileSync(path, 'utf8'))
    atomicWritePrivate(path, JSON.stringify({ ...original, canary: { fixture: 'synthetic' } }))
    assert.deepEqual(JSON.parse(readFileSync(path, 'utf8')), { from_setup: 'retained', canary: { fixture: 'synthetic' } })
    atomicWritePrivate(path, '{"password":"synthetic-must-not-appear"')
    assert.throws(() => readPrivateFile(path), error => error.message === 'Invalid private canary JSON')
  } finally {
    assert.ok(resolve(directory).startsWith(resolve(tmpdir()) + (process.platform === 'win32' ? '\\' : '/') + 'canary-contract-'))
    rmSync(directory, { recursive: true })
  }
})
