import { ESLint } from 'eslint'
import { fileURLToPath } from 'node:url'

// repositoryRoot 将 ESLint 的基准目录固定为仓库根，使源码与独立 E2E 测试共享配置。
const repositoryRoot = fileURLToPath(new URL('../../..', import.meta.url))
const eslint = new ESLint({
  cwd: repositoryRoot,
  overrideConfigFile: 'eslint.config.js',
})
const results = await eslint.lintFiles(['src/web', 'tests/e2e'])
const formatter = await eslint.loadFormatter('stylish')
const report = formatter.format(results)

if (report) {
  process.stdout.write(report)
}
if (
  results.some((result) => result.errorCount > 0 || result.warningCount > 0)
) {
  process.exitCode = 1
}
