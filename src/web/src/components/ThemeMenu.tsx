import { Monitor, Moon, Sun } from 'lucide-react'

import { useTheme } from '../app/theme/useTheme'
import type { ThemeMode } from '../app/theme/theme'

// ThemeMenu 在所有产品外壳中提供相同的主题选项和可访问名称。
export function ThemeMenu() {
  const { mode, setMode } = useTheme()
  const ThemeIcon = mode === 'light' ? Sun : mode === 'dark' ? Moon : Monitor

  return (
    <label className="theme-menu">
      <ThemeIcon size={17} aria-hidden="true" />
      <span className="visually-hidden">主题</span>
      <select
        aria-label="主题"
        value={mode}
        onChange={(event) => setMode(event.target.value as ThemeMode)}
      >
        <option value="system">跟随系统</option>
        <option value="light">浅色</option>
        <option value="dark">深色</option>
      </select>
    </label>
  )
}
