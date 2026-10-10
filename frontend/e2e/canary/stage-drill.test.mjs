import assert from 'node:assert/strict'
import process from 'node:process'
import test from 'node:test'
import { spawnSync } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'
import { CHECK_EXECUTION_ORDER } from './checks.ts'
import { canaryDrill, applyCanaryDrill, DRILL_PREPARED, DRILL_TRIGGERED, DRILL_ERROR, DRILL_EVIDENCE } from './stage-drill.mjs'
import PrivateReporter from './private-reporter.ts'
import { secureWindowsPath } from './private-files.mjs'
import { verifyReport } from './verify-report.mjs'

test('only staging normal-run e2e-fail is permitted', () => {
  assert.equal(canaryDrill({ CANARY_PROFILE: 'local' }), 'none')
  assert.equal(canaryDrill({ CANARY_PROFILE: 'staging', CANARY_DRILL: 'e2e-fail' }), 'e2e-fail')
  for (const env of [
    { CANARY_PROFILE: 'local', CANARY_DRILL: 'e2e-fail' },
    { CANARY_PROFILE: 'staging', CANARY_DRILL: 'health-fail' },
    { CANARY_PROFILE: 'staging', CANARY_DRILL: 'bad-image' },
    { CANARY_PROFILE: 'staging', CANARY_DRILL: 'e2e-fail', CANARY_STRESS: '1' },
    { CANARY_DRILL: 'e2e-fail' },
  ]) assert.throws(() => canaryDrill(env))
})

test('first check deliberately fails with a fixed credential-free error', () => {
  const env = { CANARY_PROFILE: 'staging', CANARY_DRILL: 'e2e-fail' }
  assert.throws(() => applyCanaryDrill(CHECK_EXECUTION_ORDER[0], env), /^Error: Intentional staging canary failure drill$/)
  for (const name of CHECK_EXECUTION_ORDER.slice(1)) assert.doesNotThrow(() => applyCanaryDrill(name, env))
  for (const name of CHECK_EXECUTION_ORDER) assert.doesNotThrow(() => applyCanaryDrill(name, { CANARY_PROFILE: 'staging' }))
})

const annotations = () => [
  { type: DRILL_PREPARED, description: 'complete' },
  { type: DRILL_TRIGGERED, description: 'e2e-fail' },
]
const results = () => CHECK_EXECUTION_ORDER.map((title, index) => ({
  test: { title, expectedStatus: 'passed', outcome: () => index ? 'skipped' : 'unexpected' },
  result: { status: index ? 'skipped' : 'failed', retry: 0, annotations: index ? [] : annotations(),
    errors: index ? [] : [{ message: `Error: ${DRILL_ERROR}` }] },
}))

test('reporter refuses missing, duplicate, spoofed, partial and additional-failure evidence', t => {
  const output = []
  const directory = mkdtempSync(join(tmpdir(), 'canary-drill-reporter-'))
  secureWindowsPath(directory, true)
  const report = join(directory, 'report.json')
  t.after(() => {
    assert.ok(resolve(directory).startsWith(resolve(tmpdir()) + sep + 'canary-drill-reporter-'))
    rmSync(directory, { recursive: true, force: true })
  })
  t.mock.method(console, 'log', value => output.push(value))
  t.mock.method(console, 'error', value => output.push(value))
  const keys = ['CANARY_PROFILE', 'CANARY_DRILL', 'CANARY_STRESS', 'CANARY_REPORT_FILE']
  const previous = keys.map(key => process.env[key])
  t.after(() => keys.forEach((key, index) => {
    if (previous[index] === undefined) delete process.env[key]
    else process.env[key] = previous[index]
  }))
  const run = (change = () => {}, options = {}) => {
    output.length = 0
    process.env.CANARY_PROFILE = options.profile ?? 'staging'
    process.env.CANARY_DRILL = options.drill ?? 'e2e-fail'
    delete process.env.CANARY_STRESS
    delete process.env.CANARY_REPORT_FILE
    const reportFile = Object.hasOwn(options, 'reportFile') ? options.reportFile : report
    if (reportFile !== undefined) process.env.CANARY_REPORT_FILE = reportFile
    const rows = results(), reporter = new PrivateReporter()
    change(rows)
    for (const row of rows) reporter.onTestEnd(row.test, row.result)
    if (options.globalError) reporter.onError(new Error('synthetic-secret-never-log'))
    if (reportFile === 'relative-refused') assert.throws(() => reporter.onEnd({ status: 'failed' }), /Private output path must be absolute/)
    else reporter.onEnd({ status: options.status ?? 'failed' })
    assert.equal(output.some(value => value.includes('synthetic-secret-never-log')), false)
    return output.filter(value => value === DRILL_EVIDENCE).length
  }
  assert.equal(run(), 1)
  assert.deepEqual(JSON.parse(readFileSync(report, 'utf8')).errors, ['Canary run failed'])
  for (const reportFile of [undefined, '']) {
    rmSync(report, { force: true })
    assert.equal(run(undefined, { reportFile }), 0)
    assert.equal(existsSync(report), false)
  }
  const mutations = [
    rows => { rows[0].result.annotations = [] }, // Matching error text is insufficient.
    rows => { rows[0].result.annotations.pop() },
    rows => { rows[0].result.annotations.shift() },
    rows => { rows[0].result.annotations.push(annotations()[0]) },
    rows => { rows[0].result.annotations.push(annotations()[1]) },
    rows => { rows[0].result.annotations[1].description = 'other' },
    rows => { rows[0].result.errors = [{ message: 'synthetic-secret-never-log' }] },
    rows => { rows[0].result.errors.push({ message: 'synthetic-secret-never-log' }) },
    rows => { rows[1].result.errors.push({ message: 'synthetic-secret-never-log' }) },
    rows => { rows[0].result.retry = 1 },
    rows => { rows[1].result.retry = 1 },
    rows => { rows[1].result.status = 'failed' },
    rows => { rows[1].test.title = rows[0].test.title },
    rows => { rows[0].test.expectedStatus = 'failed' },
    rows => { rows.pop() },
    rows => { rows.push(rows[0]) },
  ]
  for (const mutate of mutations) assert.equal(run(mutate), 0)
  for (const options of [{ profile: 'local' }, { drill: 'none' }, { globalError: true }, { status: 'passed' }, { status: 'interrupted' }, { reportFile: 'relative-refused' }]) {
    assert.equal(run(undefined, options), 0)
  }
})

test('real Playwright distinguishes the intentional throw from preparation, natural and teardown failures', { timeout: 120000 }, () => {
  const directory = mkdtempSync(join(tmpdir(), 'canary-drill-evidence-'))
  secureWindowsPath(directory, true)
  const local = path => fileURLToPath(new URL(path, import.meta.url)).replaceAll('\\', '/')
  try {
    // Use the actual canary wrapper and reporter. Only module locations and the
    // scenario implementation are replaced; the shim never opens a browser/network.
    let spec = readFileSync(new URL('./canary.spec.ts', import.meta.url), 'utf8')
    for (const [original, replacement] of [
      ["'@playwright/test'", JSON.stringify(local('../../node_modules/@playwright/test/index.mjs'))],
      ["'./checks'", JSON.stringify(local('./checks.ts'))],
      ["'./scenario'", "'./scenario.mjs'"],
      ["'./stage-drill.mjs'", JSON.stringify(local('./stage-drill.mjs'))],
    ]) {
      assert.equal(spec.split(original).length, 2)
      spec = spec.replace(original, replacement)
    }
    writeFileSync(join(directory, 'canary.spec.ts'), spec)
    writeFileSync(join(directory, 'scenario.mjs'), `
      export const createScenario = () => ({
        async prepare() {
          if (process.env.EVIDENCE_CASE === 'prepare') throw new Error('synthetic-secret-never-log');
          if (process.env.EVIDENCE_CASE === 'spoof') throw new Error(${JSON.stringify(DRILL_ERROR)});
        },
        async runCheck() {
          if (process.env.EVIDENCE_CASE === 'natural') throw new Error(${JSON.stringify(DRILL_ERROR)});
        },
        async close() {
          if (process.env.EVIDENCE_CASE === 'teardown') throw new Error('synthetic-secret-never-log');
        }
      });
    `)
    writeFileSync(join(directory, 'playwright.config.mjs'), `export default ${JSON.stringify({
      testDir: directory, testMatch: 'canary.spec.ts', workers: 1, retries: 0,
      globalTimeout: 20000, timeout: 5000, preserveOutput: 'never',
      outputDir: join(directory, 'results'), reporter: [[local('./private-reporter.ts')]],
      use: { trace: 'off', screenshot: 'off', video: 'off' },
    })}`)
    const env = Object.fromEntries(Object.entries(process.env).filter(([key]) =>
      /^(PATH|SYSTEMROOT|WINDIR|COMSPEC|TEMP|TMP|HOME|USERPROFILE)$/i.test(key)))
    for (const mode of ['intentional', 'prepare', 'spoof', 'teardown', 'natural', 'normal']) {
      const report = join(directory, mode + '.json')
      const child = spawnSync(process.execPath, [local('../../node_modules/playwright/cli.js'), 'test', '--config', join(directory, 'playwright.config.mjs')], {
        cwd: directory, encoding: 'utf8', windowsHide: true, timeout: 30000, maxBuffer: 1024 * 1024,
        env: { ...env, EVIDENCE_CASE: mode, CANARY_PROFILE: 'staging',
          CANARY_DRILL: ['natural', 'normal'].includes(mode) ? 'none' : 'e2e-fail',
          CANARY_REPORT_FILE: report, PLAYWRIGHT_NO_COPY_PROMPT: '1' },
      })
      const output = child.stdout + child.stderr
      assert.equal(child.error, undefined, mode)
      assert.equal(child.status, mode === 'normal' ? 0 : 1, `${mode}: ${output}`)
      assert.equal(output.includes('synthetic-secret-never-log'), false, mode)
      assert.equal(output.split(DRILL_EVIDENCE).length - 1, mode === 'intentional' ? 1 : 0, `${mode}: ${output}`)
      const value = JSON.parse(readFileSync(report, 'utf8'))
      assert.deepEqual(Object.keys(value).sort(), ['errors', 'specs'])
      assert.equal(value.specs.length, 13, mode)
      if (mode === 'normal') assert.equal(verifyReport(value), true)
      else {
        assert.deepEqual(value.errors, ['Canary run failed'])
        assert.throws(() => verifyReport(value), undefined, mode)
      }
    }
  } finally {
    assert.ok(resolve(directory).startsWith(resolve(tmpdir()) + sep + 'canary-drill-evidence-'))
    rmSync(directory, { recursive: true, force: true })
  }
})
