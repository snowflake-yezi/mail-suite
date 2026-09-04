import {
  Navigate,
  Route,
  Routes,
  useLocation,
  useSearchParams,
} from 'react-router-dom'
import type { ReactNode } from 'react'

import type { CurrentSession } from '../api/session'
import { AccessDeniedPage } from '../features/access/AccessDeniedPage'
import { AdminOverview } from '../features/admin-overview/AdminOverview'
import { AuthStatusPage } from '../features/auth/AuthStatusPage'
import { SessionProvider } from '../features/auth/SessionProvider'
import { useSession } from '../features/auth/useSession'
import { MailWorkspace } from '../features/mail/MailWorkspace'
import { LoginLayout } from '../layouts/LoginLayout'

// accountHome 返回已验证账号类型唯一允许的产品落点。
function accountHome(accountType: 'mailbox' | 'administrator'): string {
  return accountType === 'mailbox' ? '/mail/inbox' : '/admin/overview'
}

// isExpiredSession 阻止已到服务端声明期限的会话短暂渲染产品页面。
function isExpiredSession(session: CurrentSession): boolean {
  return (
    session.authenticated &&
    session.accountType !== 'unsupported' &&
    Date.parse(session.expiresAt) <= Date.now()
  )
}

// loadingOrUnavailable 在会话确认前保持稳定页面，并提供可恢复重试。
function LoadingOrUnavailable() {
  const { isError, retry } = useSession()
  return isError ? (
    <AuthStatusPage mode="unavailable" onRetry={() => void retry()} />
  ) : (
    <AuthStatusPage mode="loading" />
  )
}

// LandingRoute 根据服务端会话决定根路径唯一落点。
function LandingRoute() {
  const { session, isPending, isError } = useSession()
  if (isPending || isError || !session || isExpiredSession(session)) {
    return <LoadingOrUnavailable />
  }
  if (!session.authenticated) {
    return <Navigate to="/login" replace />
  }
  if (session.accountType === 'unsupported') {
    return <Navigate to="/forbidden" replace />
  }
  return <Navigate to={accountHome(session.accountType)} replace />
}

// safeReturnTo 只允许登录页保留已知产品分区的站内绝对路径。
function safeReturnTo(value: string | null): string | undefined {
  const isMailPath =
    value === '/mail' ||
    value?.startsWith('/mail/') ||
    value?.startsWith('/mail?')
  const isAdminPath =
    value === '/admin' ||
    value?.startsWith('/admin/') ||
    value?.startsWith('/admin?')
  if (
    !value ||
    value.startsWith('//') ||
    value.includes('\\') ||
    (!isMailPath && !isAdminPath)
  ) {
    return undefined
  }
  return value
}

// LoginRoute 为匿名会话启用真实后端 OIDC 登录入口。
function LoginRoute() {
  const { session, isPending, isError, retry } = useSession()
  const [searchParams] = useSearchParams()
  if (isPending) {
    return <LoadingOrUnavailable />
  }
  if (isError) {
    return <LoginLayout unavailable onRetry={() => void retry()} />
  }
  if (!session || isExpiredSession(session)) {
    return <LoadingOrUnavailable />
  }
  if (session.authenticated) {
    return session.accountType === 'unsupported' ? (
      <Navigate to="/forbidden" replace />
    ) : (
      <Navigate to={accountHome(session.accountType)} replace />
    )
  }
  return <LoginLayout returnTo={safeReturnTo(searchParams.get('return_to'))} />
}

// ProtectedRoute 以服务端账号类型保护邮箱端或管理端页面。
function ProtectedRoute({
  accountType,
  children,
}: {
  accountType: 'mailbox' | 'administrator'
  children: ReactNode
}) {
  const { session, isPending, isError } = useSession()
  const location = useLocation()
  if (isPending || isError || !session || isExpiredSession(session)) {
    return <LoadingOrUnavailable />
  }
  if (!session.authenticated) {
    const returnTo = `${location.pathname}${location.search}`
    return (
      <Navigate
        to={`/login?${new URLSearchParams({ return_to: returnTo })}`}
        replace
      />
    )
  }
  if (
    session.accountType === 'unsupported' ||
    session.accountType !== accountType
  ) {
    return <Navigate to="/forbidden" replace />
  }
  return children
}

// ForbiddenRoute 只向已认证但账号类型不匹配的主体显示拒绝页。
function ForbiddenRoute() {
  const { session, isPending, isError } = useSession()
  if (isPending || isError || !session || isExpiredSession(session)) {
    return <LoadingOrUnavailable />
  }
  if (!session.authenticated) {
    return <Navigate to="/login" replace />
  }
  return <AccessDeniedPage />
}

// AppRoutes 声明同一 SPA 内受服务端会话约束的所有路由。
function AppRoutes() {
  return (
    <Routes>
      <Route path="/" element={<LandingRoute />} />
      <Route path="/login" element={<LoginRoute />} />
      <Route path="/forbidden" element={<ForbiddenRoute />} />
      <Route
        path="/mail"
        element={
          <ProtectedRoute accountType="mailbox">
            <Navigate to="/mail/inbox" replace />
          </ProtectedRoute>
        }
      />
      <Route
        path="/mail/:folder"
        element={
          <ProtectedRoute accountType="mailbox">
            <MailWorkspace />
          </ProtectedRoute>
        }
      />
      <Route
        path="/mail/*"
        element={
          <ProtectedRoute accountType="mailbox">
            <Navigate to="/mail/inbox" replace />
          </ProtectedRoute>
        }
      />
      <Route
        path="/admin"
        element={
          <ProtectedRoute accountType="administrator">
            <Navigate to="/admin/overview" replace />
          </ProtectedRoute>
        }
      />
      <Route
        path="/admin/overview"
        element={
          <ProtectedRoute accountType="administrator">
            <AdminOverview />
          </ProtectedRoute>
        }
      />
      <Route
        path="/admin/*"
        element={
          <ProtectedRoute accountType="administrator">
            <Navigate to="/admin/overview" replace />
          </ProtectedRoute>
        }
      />
      <Route path="*" element={<LandingRoute />} />
    </Routes>
  )
}

// App 为全站建立单一内存会话来源，子路由不得自行推断账号类型。
export function App() {
  return (
    <SessionProvider>
      <AppRoutes />
    </SessionProvider>
  )
}
