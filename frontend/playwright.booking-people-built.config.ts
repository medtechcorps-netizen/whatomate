import { defineConfig, devices } from '@playwright/test'

// The runner owns a built application and a disposable, seeded database.
// This profile does not start an app or seed or reset a database.
const base = new URL(process.env.BASE_URL || 'invalid:')
const database = new URL(process.env.TEST_DATABASE_URL || 'invalid:')
const workspaceID = process.env.BOOKING_QUALIFICATION_WORKSPACE_ID || ''
const workspaceName = process.env.BOOKING_QUALIFICATION_WORKSPACE_NAME || ''
if (base.protocol !== 'http:' || base.hostname !== '127.0.0.1' || !base.port ||
    base.pathname !== '/' || base.search || base.hash || base.username || base.password ||
    !['postgres:', 'postgresql:'].includes(database.protocol) || database.hostname !== '127.0.0.1' ||
    !database.port || database.pathname !== '/whatomate_test' || database.hash ||
    !['', '?sslmode=disable'].includes(database.search) ||
    !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(workspaceID) ||
    !/^E2E-booking-people-[0-9a-f]{24}$/.test(workspaceName)) {
  throw new Error('The booking qualification requires the owned loopback app, disposable database, and preseeded synthetic workspace')
}

export default defineConfig({
  testDir: './e2e/qualification',
  testMatch: 'booking-people-built.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: true,
  timeout: 60_000,
  globalTimeout: 120_000,
  use: {
    ...devices['Desktop Chrome'],
    baseURL: base.origin,
    storageState: { cookies: [], origins: [] },
    serviceWorkers: 'block',
    trace: 'off',
    screenshot: 'only-on-failure',
  },
  projects: [{ name: 'chromium' }],
})
