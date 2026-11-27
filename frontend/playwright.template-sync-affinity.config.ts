import { defineConfig, devices } from "@playwright/test";

const base = new URL(process.env.REREPLY_TEMPLATE_SYNC_AFFINITY_BASE_URL || "invalid:");
if (
  base.protocol !== "http:" ||
  base.hostname !== "127.0.0.1" ||
  !base.port ||
  base.href !== base.origin + "/"
) {
  throw new Error("REREPLY_TEMPLATE_SYNC_AFFINITY_BASE_URL must identify the owned loopback frontend");
}

export default defineConfig({
  testDir: "./e2e/qualification",
  testMatch: /[\\/]e2e[\\/]qualification[\\/]template-sync-workspace-affinity-mocked\.spec\.ts$/,
  globalSetup: undefined,
  webServer: undefined,
  fullyParallel: false,
  workers: 1,
  retries: 0,
  repeatEach: 1,
  forbidOnly: true,
  timeout: 30_000,
  globalTimeout: 120_000,
  use: {
    ...devices["Desktop Chrome"],
    baseURL: base.origin,
    storageState: { cookies: [], origins: [] },
    serviceWorkers: "block",
    trace: "off",
  },
  projects: [{ name: "chromium", use: { browserName: "chromium" } }],
});
