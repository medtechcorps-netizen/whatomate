import { defineConfig, devices } from '@playwright/test'

// Real menu + ScrollArea with synthetic local messages; no API/database setup.
export default defineConfig({
  testDir: './e2e/qualification',
  testMatch: 'message-context-menu.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: true,
  timeout: 15_000,
  globalTimeout: 180_000,
  reporter: 'list',
  use: {
    ...(process.env.PLAYWRIGHT_SYSTEM_CHROME === '1' ? { channel: 'chrome' as const } : {}),
    baseURL: 'http://127.0.0.1:4317',
    storageState: { cookies: [], origins: [] },
    serviceWorkers: 'block',
    trace: 'off',
    screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'chromium-touch', use: { ...devices['Pixel 7'] } },
    { name: 'chromium-desktop', use: { ...devices['Desktop Chrome'] } },
  ],
  webServer: {
    command: 'node node_modules/vite/bin/vite.js --host 127.0.0.1 --port 4317 --strictPort',
    url: 'http://127.0.0.1:4317/e2e/fixtures/message-context-menu.html',
    reuseExistingServer: false,
    timeout: 30_000,
  },
})
