import { LockKeyhole, LogIn, RefreshCw, ShieldCheck } from 'lucide-react'

import { AppLogo } from '../components/AppLogo'
import { ThemeMenu } from '../components/ThemeMenu'

// LoginLayoutProps 描述登录入口允许保留的站内返回路径和恢复动作。
interface LoginLayoutProps {
  returnTo?: string
  unavailable?: boolean
  onRetry?: () => void
}

// LoginLayout 渲染统一身份入口，浏览器只跳转后端而不接触 OIDC token。
export function LoginLayout({
  returnTo,
  unavailable = false,
  onRetry,
}: LoginLayoutProps) {
  const loginParameters = new URLSearchParams()
  if (returnTo) {
    loginParameters.set('return_to', returnTo)
  }
  const loginQuery = loginParameters.toString()
  const loginHref = `/api/v1/auth/login${loginQuery ? `?${loginQuery}` : ''}`

  return (
    <main className="login-shell">
      <section className="login-brand" aria-label="Mail Suite 品牌">
        <AppLogo subtitle="邮件服务" size="large" />
        <div className="login-brand__statement">
          <p className="eyebrow">安全邮件工作空间</p>
          <h1>一个入口，连接你的邮件与服务管理。</h1>
        </div>
        <p className="login-brand__security">
          <ShieldCheck size={18} aria-hidden="true" />
          身份验证由组织身份服务完成
        </p>
      </section>

      <section className="login-workspace" aria-labelledby="login-heading">
        <div className="login-theme">
          <ThemeMenu />
        </div>
        <div className="login-panel">
          <span className="login-panel__icon" aria-hidden="true">
            <LockKeyhole size={22} />
          </span>
          <p className="eyebrow">Mail Suite</p>
          <h2 id="login-heading">账号登录</h2>
          <p className="login-panel__copy">
            登录后将根据账号权限进入邮箱或管理控制台。
          </p>
          <div
            className={`inline-notice ${unavailable ? 'is-error' : 'is-ready'}`}
            role="status"
          >
            <span
              className={`status-dot ${unavailable ? 'status-dot--danger' : 'status-dot--success'}`}
              aria-hidden="true"
            />
            {unavailable ? '登录服务暂时不可用' : '组织身份服务已连接'}
          </div>
          {unavailable ? (
            <button
              className="primary-button login-button"
              type="button"
              onClick={onRetry}
            >
              <RefreshCw size={17} aria-hidden="true" />
              重新连接
            </button>
          ) : (
            <a className="primary-button login-button" href={loginHref}>
              <LogIn size={17} aria-hidden="true" />
              使用账号登录
            </a>
          )}
          <p className="login-panel__footnote">
            本页面不会收集或保存邮箱密码。
          </p>
        </div>
      </section>
    </main>
  )
}
