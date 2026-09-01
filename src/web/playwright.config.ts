import { defineConfig, devices } from '@playwright/test'

// useExternalE2EServer 让标准脚本接管 Vite 生命周期，规避 Windows shell teardown 卡死。
const useExternalE2EServer = process.env.MAIL_SUITE_E2E_EXTERNAL_SERVER === '1'

// Playwright 配置固定桌面、主流移动设备和 320px 窄屏的首屏冒烟范围。
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
  webServer: useExternalE2EServer
    ? undefined
    : {
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
    {
      name: 'narrow-chromium',
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 320, height: 640 },
        isMobile: true,
        hasTouch: true,
      },
    },
  ],
})
