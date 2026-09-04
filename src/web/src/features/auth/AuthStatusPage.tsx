import { LoaderCircle, RefreshCw } from 'lucide-react'

import { AppLogo } from '../../components/AppLogo'
import { ThemeMenu } from '../../components/ThemeMenu'

// AuthStatusPageProps 描述会话加载或可恢复故障页面的操作状态。
interface AuthStatusPageProps {
  mode: 'loading' | 'unavailable'
  onRetry?: () => void
}

// AuthStatusPage 确保会话确认前不闪现任何受保护业务内容。
export function AuthStatusPage({ mode, onRetry }: AuthStatusPageProps) {
  const isLoading = mode === 'loading'
  return (
    <main className="access-page auth-status-page">
      <header className="access-page__header">
        <AppLogo subtitle="安全访问" />
        <ThemeMenu />
      </header>
      <section className="access-panel" aria-live="polite">
        <span
          className={`access-panel__icon ${isLoading ? 'is-loading' : ''}`}
          aria-hidden="true"
        >
          {isLoading ? <LoaderCircle size={26} /> : <RefreshCw size={25} />}
        </span>
        <p className="eyebrow">账号会话</p>
        <h1>{isLoading ? '正在确认登录状态' : '暂时无法连接登录服务'}</h1>
        <p>
          {isLoading
            ? '请稍候。'
            : '当前没有建立或恢复会话，请在服务恢复后重试。'}
        </p>
        {!isLoading && onRetry ? (
          <button className="primary-button" type="button" onClick={onRetry}>
            <RefreshCw size={17} aria-hidden="true" />
            重新连接
          </button>
        ) : null}
      </section>
    </main>
  )
}
