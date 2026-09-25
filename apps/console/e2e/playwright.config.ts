import { defineConfig, devices } from '@playwright/test'

// The built console (`bun run build` first). The tests serve it themselves,
// as Kuben does (same content security policy, SPA fallback), and answer its
// API calls: no server runs.
//
// Visual snapshots (visual.pw.ts) live in __screenshots__/, one per page,
// locale, theme and platform. A missing one is written instead of compared:
// Playwright's own `updateSnapshots: 'missing'` writes it but still fails the
// test (a soft error), so visual.pw.ts looks for the file first and takes
// the screenshot itself when there is none — the first run passes. CI
// uploads the folder as the `console-screenshots` artifact to download and
// commit. After an intended visual change, refresh them on Linux (the
// CI platform) with `npx playwright test visual.pw.ts --update-snapshots`,
// or delete the changed files and commit what the next CI run writes.
export default defineConfig({
  testDir: '.',
  testMatch: '*.pw.ts',
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['github']] : 'list',
  snapshotPathTemplate: '{testDir}/__screenshots__/{arg}-{projectName}-{platform}{ext}',
  expect: {
    toHaveScreenshot: { animations: 'disabled', caret: 'hide', scale: 'css', maxDiffPixelRatio: 0.01 },
  },
  use: {
    baseURL: 'http://kuben.test',
    trace: 'retain-on-failure',
    // Dates render the same on every machine.
    timezoneId: 'UTC',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
