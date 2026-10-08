import { test } from '@playwright/test'
import { CHECK_EXECUTION_ORDER } from './checks'
import { createScenario } from './scenario'
import { applyCanaryDrill, DRILL_PREPARED, DRILL_TRIGGERED } from './stage-drill.mjs'

test.describe.configure({ mode: 'serial', retries: 0 })
let scenario: ReturnType<typeof createScenario>
test.beforeAll(async ({ playwright }, testInfo) => {
  scenario = createScenario(playwright.chromium)
  await scenario.prepare()
  testInfo.annotations.push({ type: DRILL_PREPARED, description: 'complete' })
})
test.afterAll(async () => { await scenario?.close() })
for (const name of CHECK_EXECUTION_ORDER) {
  test(name, async () => {
    applyCanaryDrill(name, process.env, () => {
      test.info().annotations.push({ type: DRILL_TRIGGERED, description: 'e2e-fail' })
    })
    await scenario.runCheck(name)
  })
}
