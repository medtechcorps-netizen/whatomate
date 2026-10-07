import { test } from '@playwright/test'
import { CHECK_EXECUTION_ORDER } from './checks'
import { createScenario } from './scenario'

test.describe.configure({ mode: 'serial', retries: 0 })
let scenario: ReturnType<typeof createScenario>
test.beforeAll(async ({ playwright }) => {
  scenario = createScenario(playwright.chromium)
  await scenario.prepare()
})
test.afterAll(async () => { await scenario?.close() })
for (const name of CHECK_EXECUTION_ORDER) {
  test(name, async () => { await scenario.runCheck(name) })
}
