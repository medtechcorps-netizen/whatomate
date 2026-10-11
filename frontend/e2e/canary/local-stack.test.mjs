import assert from 'node:assert/strict'
import { copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { tmpdir } from 'node:os'
import test from 'node:test'
import vm from 'node:vm'

const source = readFileSync(new URL('./local-stack.mjs', import.meta.url), 'utf8')
// Execute the actual helper with real synthetic files. Only process execution
// and the existing private-ACL helpers are injected; no Go process or DB runs.
const begin = source.indexOf('async function bootstrap() {')
const end = source.indexOf('\nasync function start() {', begin)
assert.ok(begin >= 0 && end > begin)
const helper = source.slice(begin, end) + '\nbootstrap()'

async function exercise({ platform = 'linux', missing, buildFails = false, invokeFails = false, privateFails = false } = {}) {
  const root = mkdtempSync(join(tmpdir(), 'canary-bootstrap-contract-'))
  const directory = join(root, 'private')
  mkdirSync(directory, { mode: 0o700 })
  writeFileSync(join(directory, 'migrate.toml'), 'synthetic private config', { mode: 0o600 })
  const files = [
    ['internal/dbcatalog/golden/production-v0.json', 'production-v0.json', '{"synthetic":"profile"}\n'],
    ['internal/dbcatalog/shape/production-v0.sql', 'production-v0.sql', '-- synthetic overlay\n'],
  ]
  for (const [path, name, bytes] of files) {
    mkdirSync(dirname(join(root, path)), { recursive: true })
    if (name !== missing) writeFileSync(join(root, path), bytes)
  }
  const calls = [], checks = [], env = { SYNTHETIC_ONLY: 'true' }
  const context = {
    root, directory, process: { platform }, join, copyFileSync, mkdirSync, mkdtempSync,
    assertPrivateLocation: (path, options) => { assert.equal(path, join(directory, 'migrate.toml')); assert.equal(options.mustExist, true); checks.push('location') },
    assertPrivatePermissions: path => { if (privateFails) throw new Error('private-refusal'); assert.ok(path.startsWith(directory)); checks.push('permissions') },
    secureWindowsPath: (path, isDirectory) => { assert.ok(path.startsWith(join(directory, 'bootstrap-'))); assert.equal(isDirectory, true); checks.push('secure') },
    childEnvironment: () => env,
    execFileSync: (command, args, options) => {
      calls.push({ command, args: [...args] })
      assert.equal(options.cwd, root); assert.equal(options.env, env)
      assert.equal(options.stdio, 'inherit'); assert.equal(options.windowsHide, true)
      const executable = calls[0].args[3]
      assert.equal(executable.endsWith(platform === 'win32' ? 'bootstrap.exe' : 'bootstrap'), true)
      assert.ok(dirname(executable).startsWith(join(directory, 'bootstrap-')))
      for (const [, name, bytes] of files) assert.equal(readFileSync(join(dirname(executable), 'production-shape', name), 'utf8'), bytes)
      if (command === 'go') {
        assert.deepEqual([...args], ['build', '-mod=readonly', '-o', executable, './release/staging/bootstrap'])
        if (buildFails) throw new Error('build-refusal')
        writeFileSync(executable, 'synthetic executable')
      } else {
        assert.equal(command, executable); assert.ok(existsSync(executable))
        assert.deepEqual([...args], ['-config', join(directory, 'migrate.toml')])
        if (invokeFails) throw new Error('bootstrap-refusal')
      }
    },
  }
  let error
  try { await vm.runInNewContext(helper, context, { timeout: 1000 }) } catch (caught) { error = caught }
  finally { rmSync(root, { recursive: true, force: true }) }
  return { calls, checks, error }
}

test('local and hosted bootstrap builds a stable binary with both exact adjacent sidecars', async () => {
  for (const platform of ['linux', 'win32']) {
    const result = await exercise({ platform })
    assert.equal(result.error, undefined)
    assert.equal(result.calls.length, 2)
    assert.ok(result.checks.includes('location') && result.checks.includes('secure'))
  }
})
test('missing either sidecar refuses before build or database invocation', async () => {
  for (const missing of ['production-v0.json', 'production-v0.sql']) {
    const result = await exercise({ missing })
    assert.equal(result.error.code, 'ENOENT'); assert.equal(result.calls.length, 0)
  }
})
test('private path refusal runs no process', async () => {
  const result = await exercise({ privateFails: true })
  assert.equal(result.error.message, 'private-refusal'); assert.equal(result.calls.length, 0)
})
test('build failure does not invoke an old or partial bootstrap executable', async () => {
  const result = await exercise({ buildFails: true })
  assert.equal(result.error.message, 'build-refusal'); assert.equal(result.calls.length, 1)
})
test('bootstrap refusal is propagated without retry', async () => {
  const result = await exercise({ invokeFails: true })
  assert.equal(result.error.message, 'bootstrap-refusal'); assert.equal(result.calls.length, 2)
})
test('both workflow and local command use the tested helper and hosted unit coverage includes it', () => {
  assert.match(source, /else if \(mode === 'bootstrap'\) await bootstrap\(\)/)
  assert.doesNotMatch(source, /\['run', '\.\/release\/staging\/bootstrap'/)
  const workflow = readFileSync(new URL('../../../.github/workflows/canary-local.yml', import.meta.url), 'utf8')
  assert.match(workflow, /run: node frontend\/e2e\/canary\/local-stack\.mjs bootstrap/)
  const scripts = JSON.parse(readFileSync(new URL('../../package.json', import.meta.url), 'utf8')).scripts
  assert.ok(scripts['test:canary:unit'].includes('e2e/canary/local-stack.test.mjs'))
})
