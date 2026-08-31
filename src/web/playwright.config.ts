import { defineConfig, devices } from '@playwright/test'

// Playwright 配置固定桌面和移动视口的首屏冒烟范围。
export default defineConfig({
  testDir: '../../tests/e2e',
  outputDir: '../../.tmp/playwright-results',
  reporter: [
    ['list'],
    ['html', { outputFolder: '../../.tmp/playwright-report', open: 'never' }],
  ],
  use: {
    baseURL: 'http://127.0.0.1:4173',
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
  webServer: {
    command:
      'node ./node_modules/vite/bin/vite.js --host 127.0.0.1 --port 4173',
    url: 'http://127.0.0.1:4173',
    reuseExistingServer: false,
  },
  projects: [
    {
      name: 'desktop-chromium',
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1440, height: 900 },
      },
    },
    {
      name: 'mobile-chromium',
      use: { ...devices['Pixel 7'] },
    },
  ],
})
