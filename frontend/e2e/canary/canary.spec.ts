import { test } from '@playwright/test'
import { CANARY_FAILURE_PHASES, CHECK_EXECUTION_ORDER, type RunCanaryPhase } from './checks'
import { createScenario } from './scenario'
import { applyCanaryDrill, DRILL_PREPARED, DRILL_TRIGGERED } from './stage-drill.mjs'

test.describe.configure({ mode: 'serial', retries: 0 })
let scenario: ReturnType<typeof createScenario>
const runPhase: RunCanaryPhase = (phase, run) => test.step(phase, run)
test.beforeAll(async ({ playwright }, testInfo) => {
  scenario = await runPhase(CANARY_FAILURE_PHASES.setupFixture, async () => createScenario(playwright.chromium))
  await scenario.prepare(runPhase)
  testInfo.annotations.push({ type: DRILL_PREPARED, description: 'complete' })
})
test.afterAll(async () => { await scenario?.close() })
for (const name of CHECK_EXECUTION_ORDER) {
  test(name, async () => {
    applyCanaryDrill(name, process.env, () => {
      test.info().annotations.push({ type: DRILL_TRIGGERED, description: 'e2e-fail' })
    })
    await scenario.runCheck(name, name === CHECK_EXECUTION_ORDER[0] ? runPhase : undefined)
  })
}
