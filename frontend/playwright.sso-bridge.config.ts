import { defineConfig, devices } from '@playwright/test'

// The real-app SSO bridge (R1). Run only by the Go test
// TestSSOBridgeRealBrowserTwoCallbacks (cmd/whatomate), which serves the real
// application over loopback TLS and sets BASE_URL and the SSO_BRIDGE_* values.
// Nothing from the default suite: no global setup, no web server, one worker,
// no retries, one spec outside the default test directory.
for (const name of ['BASE_URL', 'SSO_BRIDGE_HOST_RULES']) {
  if (!process.env[name]) throw new Error(`${name} is required: run the SSO bridge through its Go test`)
}

export default defineConfig({
  testDir: './e2e/sso-bridge',
  testMatch: 'sso-bridge.spec.ts',
  globalSetup: undefined,
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: true,
  timeout: 120_000,
  reporter: [
    ['list'],
    ['json', { outputFile: process.env.SSO_BRIDGE_REPORT || 'sso-bridge-report.json' }],
  ],
  use: {
    baseURL: process.env.BASE_URL,
    ignoreHTTPSErrors: true,
    trace: 'off',
    screenshot: 'off',
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        ignoreHTTPSErrors: true,
        // The fake identity provider's made-up hosts resolve to its local
        // server, so the real InitSSO redirect is followed as is.
        launchOptions: { args: [`--host-resolver-rules=${process.env.SSO_BRIDGE_HOST_RULES}`] },
      },
    },
  ],
})
