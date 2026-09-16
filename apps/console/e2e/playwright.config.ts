import { defineConfig, devices } from '@playwright/test'

// The built console (`bun run build` first). The tests serve it themselves,
// as Kuben does (same content security policy, SPA fallback), and answer its
// API calls: no server runs.
export default defineConfig({
  testDir: '.',
  testMatch: '*.pw.ts',
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['github']] : 'list',
  use: {
    baseURL: 'http://kuben.test',
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
