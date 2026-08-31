import { createRequire } from "node:module";

// pluginRequire 从唯一 Web 包解析已锁定插件，避免在仓库根建立第二套 Node 依赖。
const pluginRequire = createRequire(
  new URL("./src/web/package.json", import.meta.url),
);
const eslint = pluginRequire("@eslint/js");
const globals = pluginRequire("globals");
const reactHooks = pluginRequire("eslint-plugin-react-hooks");
const reactRefreshModule = pluginRequire("eslint-plugin-react-refresh");
const reactRefresh = reactRefreshModule.default ?? reactRefreshModule;
const tseslint = pluginRequire("typescript-eslint");

// ESLint 配置覆盖手写 TypeScript、React 和 E2E 测试，忽略所有生成或构建输出。
export default tseslint.config(
  {
    ignores: [
      ".cache",
      ".tmp",
      "src/web/dist",
      "src/web/node_modules",
      "src/web/coverage",
      "generated",
    ],
  },
  eslint.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["src/web/scripts/**/*.mjs"],
    languageOptions: {
      globals: globals.node,
    },
  },
  {
    files: ["**/*.{ts,tsx}"],
    languageOptions: {
      ecmaVersion: 2024,
      globals: {
        ...globals.browser,
        ...globals.node,
      },
    },
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.flat.recommended.rules,
      "react-refresh/only-export-components": [
        "warn",
        { allowConstantExport: true },
      ],
    },
  },
);
