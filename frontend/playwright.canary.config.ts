import { defineConfig } from '@playwright/test'
import { dirname, join } from 'node:path'
import { loadProfile } from './e2e/canary/profiles'
import { createPrivateResultsDirectory, validateReportPath } from './e2e/canary/private-files.mjs'

const profile = loadProfile() // Refuse before Playwright launches anything.
const report = process.env.CANARY_REPORT_FILE || join(dirname(profile.privateFile), 'canary-report.json')
process.env.CANARY_REPORT_FILE = validateReportPath(profile.privateFile, report)
process.env.PLAYWRIGHT_NO_COPY_PROMPT = '1' // Disable automatic failure page snapshots.
process.umask(0o077)
const outputDir = createPrivateResultsDirectory(profile.privateFile)
export default defineConfig({
  testDir: './e2e/canary',
  testMatch: process.env.CANARY_STRESS === '1' ? 'stress.spec.ts' : 'canary.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: true,
  timeout: 60000,
  globalTimeout: process.env.CANARY_STRESS === '1' ? 6 * 60 * 60 * 1000 : 300000,
  // Raw Playwright errors can contain password fills or authenticated request
  // headers. The reporter prints names/statuses only, including failure paths.
  reporter: [['./e2e/canary/private-reporter.ts']],
  outputDir,
  preserveOutput: 'never',
  use: { trace: 'off', screenshot: 'off', video: 'off' },
})
