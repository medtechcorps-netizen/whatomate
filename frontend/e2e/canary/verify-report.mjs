import { readFileSync } from 'node:fs'
import { pathToFileURL } from 'node:url'
import { UI_CHECKS } from './checks.ts'

export function verifyReport(report, { stress = false } = {}) {
  const results = []
  function walk(suite) {
    for (const spec of suite.specs || []) {
      if (spec.tests?.length !== 1) throw new Error('Each canary must execute exactly once')
      const test = spec.tests[0]
      if (test.expectedStatus !== 'passed' || test.status !== 'expected' || test.results?.length !== 1 || test.results[0].status !== 'passed' || test.results[0].retry !== 0) {
        throw new Error('Canary report contains a failure, retry or skipped check')
      }
      results.push(spec.title)
    }
    for (const child of suite.suites || []) walk(child)
  }
  if (report.errors?.length) throw new Error('Canary report has global errors')
  walk(report)
  if (stress) {
    const expected = UI_CHECKS.flatMap(name => Array(120).fill(name))
    for (let index = 1; index <= 80; index++) for (const name of ['omnichannel_late_layout_autoscroll', 'native_chat_late_layout_autoscroll']) expected.push(`${name} ${index}`)
    if (JSON.stringify(results.sort()) !== JSON.stringify(expected.sort())) throw new Error('Stress report requires 120 passes per check and 80 per late-layout check')
  } else if (results.length !== UI_CHECKS.length || new Set(results).size !== UI_CHECKS.length || UI_CHECKS.some(name => !results.includes(name))) {
    throw new Error('Canary report must contain exactly the 13 named checks')
  }
  return true
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { verifyReport(JSON.parse(readFileSync(process.argv[2], 'utf8')), { stress: process.env.CANARY_STRESS === '1' }); console.log('canary: exact check inventory passed, no retries') }
  catch (error) { console.error(error.message); process.exitCode = 1 }
}
