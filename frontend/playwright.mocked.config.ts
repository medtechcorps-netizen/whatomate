import { defineConfig, devices } from '@playwright/test'
import baseConfig from './playwright.config'

const useSystemChrome = process.env.PLAYWRIGHT_SYSTEM_CHROME === '1'

// Focused UI regressions using route mocks do not need the live API/database
// setup. Keep this config separate so the full E2E suite retains its normal
// seeded integration environment.
export default defineConfig({
  ...baseConfig,
  globalSetup: undefined,
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        ...(useSystemChrome ? { channel: 'chrome' as const } : {}),
      },
    },
  ],
})
