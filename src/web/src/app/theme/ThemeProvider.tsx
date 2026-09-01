import { useEffect, useMemo, useState, type ReactNode } from 'react'

import { ThemeContext, type ThemeMode } from './theme'

const themeStorageKey = 'mail-suite.theme'

// readStoredTheme 只读取允许的主题枚举，忽略损坏或过期的浏览器数据。
function readStoredTheme(): ThemeMode {
  const storedMode = window.localStorage.getItem(themeStorageKey)
  return storedMode === 'light' || storedMode === 'dark' ? storedMode : 'system'
}

// resolveTheme 将用户策略与系统偏好合并为实际渲染主题。
function resolveTheme(mode: ThemeMode, prefersDark: boolean): 'light' | 'dark' {
  return mode === 'system' ? (prefersDark ? 'dark' : 'light') : mode
}

// ThemeProvider 统一管理登录页、邮箱端和管理端的主题状态与持久化副作用。
export function ThemeProvider({ children }: { children: ReactNode }) {
  const [mode, setMode] = useState<ThemeMode>(readStoredTheme)

  useEffect(() => {
    const mediaQuery = window.matchMedia('(prefers-color-scheme: dark)')

    // applyResolvedTheme 同步根节点和浏览器主题色，避免工作区切换时出现皮肤闪烁。
    const applyResolvedTheme = () => {
      const resolvedTheme = resolveTheme(mode, mediaQuery.matches)
      document.documentElement.dataset.theme = resolvedTheme
      document.documentElement.style.colorScheme = resolvedTheme
      document
        .querySelector('meta[name="theme-color"]')
        ?.setAttribute(
          'content',
          resolvedTheme === 'dark' ? '#101722' : '#f6f8fb',
        )
    }

    window.localStorage.setItem(themeStorageKey, mode)
    applyResolvedTheme()
    mediaQuery.addEventListener('change', applyResolvedTheme)
    return () => mediaQuery.removeEventListener('change', applyResolvedTheme)
  }, [mode])

  const value = useMemo(() => ({ mode, setMode }), [mode])
  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>
}
