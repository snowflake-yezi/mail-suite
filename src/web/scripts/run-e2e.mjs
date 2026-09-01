import { spawn } from 'node:child_process'
import { delimiter, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { createServer } from 'vite'

// projectRoot 固定 Vite 与 Playwright 的共同工作目录，避免从调用位置推导路径。
const projectRoot = fileURLToPath(new URL('..', import.meta.url))
// playwrightCli 使用锁文件内的测试入口，不依赖全局命令或 shell 解析。
const playwrightCli = fileURLToPath(
  new URL('../node_modules/@playwright/test/cli.js', import.meta.url),
)

// runPlaywright 启动可显式关闭的 Vite 服务，并透传 Playwright 参数与退出码。
async function runPlaywright() {
  const server = await createServer({
    root: projectRoot,
    server: {
      host: '127.0.0.1',
      port: 4173,
      strictPort: true,
    },
  })

  await server.listen()
  try {
    const exitCode = await new Promise((resolve, reject) => {
      const child = spawn(
        process.execPath,
        [playwrightCli, 'test', ...process.argv.slice(2)],
        {
          cwd: projectRoot,
          env: {
            ...process.env,
            MAIL_SUITE_E2E_EXTERNAL_SERVER: '1',
            NODE_PATH: [
              join(projectRoot, 'node_modules'),
              process.env.NODE_PATH,
            ]
              .filter(Boolean)
              .join(delimiter),
          },
          stdio: 'inherit',
        },
      )
      child.once('error', reject)
      child.once('exit', (code) => resolve(code ?? 1))
    })
    process.exitCode = exitCode
  } finally {
    await server.close()
  }
}

await runPlaywright()
