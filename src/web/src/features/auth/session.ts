import { createContext } from 'react'

import type { CurrentSession } from '../../api/session'

export const sessionQueryKey = ['current-session'] as const

// SessionContextValue 向路由和账号控件暴露唯一的浏览器会话状态。
export interface SessionContextValue {
  session: CurrentSession | undefined
  isPending: boolean
  isError: boolean
  retry: () => Promise<void>
  logout: () => Promise<void>
}

// SessionContext 保存内存会话，默认空值用于阻止组件绕过 provider。
export const SessionContext = createContext<SessionContextValue | undefined>(
  undefined,
)
