import { useContext } from 'react'

import { ThemeContext } from './theme'

// useTheme 返回共享主题状态，并阻止组件脱离 ThemeProvider 静默运行。
export function useTheme() {
  const context = useContext(ThemeContext)
  if (!context) {
    throw new Error('useTheme 必须在 ThemeProvider 内使用')
  }
  return context
}
