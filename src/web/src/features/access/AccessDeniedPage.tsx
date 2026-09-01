import { ShieldX } from 'lucide-react'
import { Link } from 'react-router-dom'

import { AppLogo } from '../../components/AppLogo'
import { ThemeMenu } from '../../components/ThemeMenu'

// AccessDeniedPage 为未知账号类型和跨端访问提供稳定的无权限落点。
export function AccessDeniedPage() {
  return (
    <main className="access-page">
      <header className="access-page__header">
        <AppLogo subtitle="安全访问" to="/login" />
        <ThemeMenu />
      </header>
      <section className="access-panel" aria-labelledby="access-heading">
        <span className="access-panel__icon" aria-hidden="true">
          <ShieldX size={26} />
        </span>
        <p className="eyebrow">访问受限</p>
        <h1 id="access-heading">当前账号无权访问此区域</h1>
        <p>请使用获授权的独立账号重新登录，或联系管理员核对账号类型。</p>
        <Link className="primary-button" to="/login">
          返回登录
        </Link>
      </section>
    </main>
  )
}
