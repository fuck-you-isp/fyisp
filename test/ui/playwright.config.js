// Playwright config for the fyisp UI tests. The Dockerfile's uitest stage
// starts fyisp-webdev on 127.0.0.1:3000 and runs `npx playwright test` here.
// @ts-check
const { defineConfig, devices } = require('@playwright/test');

module.exports = defineConfig({
  testDir: './specs',
  outputDir: './out/results',
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: [['list'], ['html', { outputFolder: './out/report', open: 'never' }]],
  use: {
    baseURL: process.env.FYISP_URL || 'http://127.0.0.1:3000',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'], viewport: { width: 1400, height: 900 } } },
  ],
});
