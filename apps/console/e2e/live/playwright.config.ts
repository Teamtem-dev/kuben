import { defineConfig, devices } from '@playwright/test'

// The console as Kuben ships it: embedded in the Go binary, served by a
// running `kuben serve` on PostgreSQL and without a Kubernetes cluster (CI job
// console-live starts it). Nothing is mocked. The tests share that server's
// state and build on each other, so they run in order, once: a retry would
// meet a server that is already set up.
export default defineConfig({
  testDir: '.',
  testMatch: '*.live.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: Boolean(process.env.CI),
  timeout: 120_000,
  expect: { timeout: 15_000 },
  reporter: process.env.CI ? [['list'], ['github']] : 'list',
  use: {
    baseURL: process.env.CONSOLE_URL ?? 'http://127.0.0.1:3100',
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
