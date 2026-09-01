import { createContext } from 'react'

// ThemeMode 描述用户可选择并持久化的三种主题策略。
export type ThemeMode = 'system' | 'light' | 'dark'

// ThemeContextValue 暴露当前主题策略及唯一的更新入口。
export interface ThemeContextValue {
  mode: ThemeMode
  setMode: (mode: ThemeMode) => void
}

// ThemeContext 由应用根节点提供，未挂载时保持 undefined 以便尽早发现错误。
export const ThemeContext = createContext<ThemeContextValue | undefined>(
  undefined,
)
