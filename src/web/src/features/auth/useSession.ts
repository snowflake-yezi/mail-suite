import { useContext } from 'react'

import { SessionContext, type SessionContextValue } from './session'

// useSession 返回当前 provider 状态，禁止组件绕过统一会话边界。
export function useSession(): SessionContextValue {
  const context = useContext(SessionContext)
  if (!context) {
    throw new Error('会话组件必须位于 SessionProvider 内')
  }
  return context
}
