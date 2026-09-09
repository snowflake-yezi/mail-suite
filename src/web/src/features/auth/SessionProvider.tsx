import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useLayoutEffect, useRef, type ReactNode } from 'react'

import {
  getCurrentSession,
  logoutCurrentSession,
  type CurrentSession,
} from '../../api/session'
import {
  SessionContext,
  sessionQueryKey,
  type SessionContextValue,
} from './session'
import { navigateToProviderLogout } from './providerLogoutNavigation'

// sessionIdentityKey 生成决定业务缓存归属的稳定会话身份键。
function sessionIdentityKey(
  session: CurrentSession | undefined,
): string | undefined {
  if (!session?.authenticated || session.accountType === 'unsupported') {
    return undefined
  }
  return [
    session.principal.id,
    session.tenantId,
    session.accountType,
    session.accountType === 'mailbox' ? session.mailbox.id : '',
  ].join('\u0000')
}

// SessionProvider 在内存中维护会话，并在失效或退出时清除其他服务端缓存。
export function SessionProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient()
  const previousIdentityKey = useRef<string | undefined>(undefined)
  const sessionQuery = useQuery({
    queryKey: sessionQueryKey,
    queryFn: ({ signal }) => getCurrentSession(signal),
    retry: false,
    staleTime: 0,
    refetchOnWindowFocus: true,
  })

  useLayoutEffect(() => {
    const session = sessionQuery.data
    if (!session) {
      return
    }
    const currentIdentityKey = sessionIdentityKey(session)
    const identityChanged =
      previousIdentityKey.current !== undefined &&
      currentIdentityKey !== undefined &&
      previousIdentityKey.current !== currentIdentityKey
    if (currentIdentityKey === undefined || identityChanged) {
      queryClient.removeQueries({
        predicate: (query) => query.queryKey[0] !== sessionQueryKey[0],
      })
    }
    previousIdentityKey.current = currentIdentityKey
  }, [queryClient, sessionQuery.data])

  useEffect(() => {
    const session = sessionQuery.data
    if (!session?.authenticated || session.accountType === 'unsupported') {
      return
    }
    const remainingMilliseconds = Date.parse(session.expiresAt) - Date.now()
    const refreshDelay = Math.max(
      0,
      Math.min(remainingMilliseconds, 2_147_483_647),
    )
    const timeout = window.setTimeout(() => {
      void queryClient.invalidateQueries({ queryKey: sessionQueryKey })
    }, refreshDelay)
    return () => window.clearTimeout(timeout)
  }, [queryClient, sessionQuery.data])

  const value: SessionContextValue = {
    session: sessionQuery.data,
    isPending: sessionQuery.isPending,
    isError: sessionQuery.isError,
    retry: async () => {
      await sessionQuery.refetch()
    },
    logout: async () => {
      const session = sessionQuery.data
      if (!session?.authenticated) {
        return
      }
      const { providerLogoutUrl } = await logoutCurrentSession(
        session.csrfToken,
      )
      await queryClient.cancelQueries()
      queryClient.removeQueries({
        predicate: (query) => query.queryKey[0] !== sessionQueryKey[0],
      })
      queryClient.setQueryData<CurrentSession>(sessionQueryKey, {
        authenticated: false,
      })
      navigateToProviderLogout(providerLogoutUrl)
    },
  }

  return (
    <SessionContext.Provider value={value}>{children}</SessionContext.Provider>
  )
}
