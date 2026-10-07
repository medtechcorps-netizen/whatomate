import { test } from '@playwright/test'
import { CHECK_EXECUTION_ORDER } from './checks'
import { createScenario } from './scenario'

test.describe.configure({ mode: 'serial', retries: 0 })
// Manual only: 120 full runs, then 80 late-layout probes for EACH transcript.
// Every iteration is independently visible; skipped/retried cases cannot pass.
for (let iteration = 1; iteration <= 120; iteration++) {
  test.describe(`pass ${iteration}`, () => {
    let scenario: ReturnType<typeof createScenario>
    test.beforeAll(async ({ playwright }) => { scenario = createScenario(playwright.chromium); await scenario.prepare() })
    test.afterAll(async () => { await scenario?.close() })
    for (const name of CHECK_EXECUTION_ORDER) test(name, async () => { await scenario.runCheck(name) })
  })
}
test.describe('late layout repetition', () => {
  let scenario: ReturnType<typeof createScenario>
  test.beforeAll(async ({ playwright }) => {
    scenario = createScenario(playwright.chromium)
    await scenario.prepare()
    for (const name of CHECK_EXECUTION_ORDER) await scenario.runCheck(name)
  })
  test.afterAll(async () => { await scenario?.close() })
  for (let iteration = 1; iteration <= 80; iteration++) {
    for (const name of ['omnichannel_late_layout_autoscroll', 'native_chat_late_layout_autoscroll']) {
      test(`${name} ${iteration}`, async () => { await scenario.runCheck(name) })
    }
  }
})
