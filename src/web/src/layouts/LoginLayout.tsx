import { LockKeyhole, ShieldCheck } from 'lucide-react'

import { AppLogo } from '../components/AppLogo'
import { ThemeMenu } from '../components/ThemeMenu'

// LoginLayout 渲染统一身份入口；认证 API 未准入时保持按钮不可执行并说明真实状态。
export function LoginLayout() {
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
          <div className="inline-notice" role="status">
            <span
              className="status-dot status-dot--warning"
              aria-hidden="true"
            />
            认证服务尚未接入
          </div>
          <button
            className="primary-button login-button"
            type="button"
            disabled
            title="等待 OIDC 认证服务接入"
          >
            使用账号登录
          </button>
          <p className="login-panel__footnote">
            本页面不会收集或保存邮箱密码。
          </p>
        </div>
      </section>
    </main>
  )
}
