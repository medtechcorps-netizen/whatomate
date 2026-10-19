import { defineConfig, devices } from '@playwright/test'

// Deliberately separate from normal tests and the Vite dev-server config.
// The root runner must start a source-bound built app with SecurityHeaders.
const base = new URL(process.env.R2_PDF_BASE_URL || 'invalid:')
if (base.protocol !== 'http:' || base.hostname !== '127.0.0.1' || !base.port ||
    base.pathname !== '/' || base.search || base.hash || base.username || base.password) {
  throw new Error('R2_PDF_BASE_URL must identify the owned loopback built app')
}
export default defineConfig({
  testDir: './e2e/qualification',
  testMatch: 'pdf-preview-built.spec.ts',
  globalSetup: undefined,
  webServer: undefined,
  fullyParallel: false,
  workers: 1,
  retries: 0,
  repeatEach: 1,
  forbidOnly: true,
  timeout: 30_000,
  globalTimeout: 120_000,
  use: { ...devices['Desktop Chrome'], baseURL: base.origin, serviceWorkers: 'block',
    viewport: { width: 1440, height: 1000 }, trace: 'off', screenshot: 'only-on-failure' },
  projects: [{ name: 'chromium' }],
})
