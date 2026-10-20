import { defineConfig, devices } from '@playwright/test'

// Root supplies a source-bound, loopback built app. No database/provider calls
// are made by this synthetic browser transport suite.
const base = new URL(process.env.R2_MULTIPART_BASE_URL || 'invalid:')
if (base.protocol !== 'http:' || base.hostname !== '127.0.0.1' || !base.port ||
    base.pathname !== '/' || base.search || base.hash || base.username || base.password) {
  throw new Error('R2_MULTIPART_BASE_URL must identify the owned loopback built app')
}
export default defineConfig({
  testDir: './e2e/qualification', testMatch: 'chat-multipart-built.spec.ts',
  globalSetup: undefined, webServer: undefined, fullyParallel: false,
  workers: 1, retries: 0, repeatEach: 1, forbidOnly: true,
  timeout: 30_000, globalTimeout: 360_000,
  use: { ...devices['Desktop Chrome'], baseURL: base.origin,
    storageState: { cookies: [], origins: [] }, serviceWorkers: 'block',
    viewport: { width: 1600, height: 1000 }, trace: 'off', screenshot: 'only-on-failure' },
  projects: [{ name: 'chromium' }],
})
