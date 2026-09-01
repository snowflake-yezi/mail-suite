import { FilePenLine, Menu, X } from 'lucide-react'
import { useState, type ReactNode } from 'react'

import { AppLogo } from '../components/AppLogo'

// MailLayoutProps 描述邮箱布局需要的文件夹导航、顶栏及主工作区内容。
interface MailLayoutProps {
  navigation: ReactNode
  topbar: ReactNode
  children: ReactNode
}

// MailLayout 提供标准 Webmail 外壳，并在窄屏将文件夹栏切换为可关闭抽屉。
export function MailLayout({ navigation, topbar, children }: MailLayoutProps) {
  const [isNavigationOpen, setNavigationOpen] = useState(false)

  return (
    <div className="mail-shell">
      <button
        className="mobile-menu-button"
        type="button"
        aria-label={isNavigationOpen ? '关闭邮箱导航' : '打开邮箱导航'}
        aria-expanded={isNavigationOpen}
        onClick={() => setNavigationOpen((isOpen) => !isOpen)}
      >
        {isNavigationOpen ? <X size={20} /> : <Menu size={20} />}
      </button>

      {isNavigationOpen ? (
        <button
          className="navigation-scrim"
          type="button"
          aria-label="关闭邮箱导航"
          onClick={() => setNavigationOpen(false)}
        />
      ) : null}

      <aside className={`mail-sidebar ${isNavigationOpen ? 'is-open' : ''}`}>
        <AppLogo subtitle="邮箱" to="/mail/inbox" />
        <button
          className="primary-button compose-button"
          type="button"
          disabled
          title="邮件服务尚未接入"
        >
          <FilePenLine size={18} aria-hidden="true" />
          写邮件
        </button>
        {navigation}
        <div className="mail-connection" role="status">
          <span className="status-dot status-dot--warning" aria-hidden="true" />
          <span>
            <strong>邮箱尚未连接</strong>
            <small>等待邮件服务接入</small>
          </span>
        </div>
      </aside>

      <div className="mail-product">
        {topbar}
        <main className="mail-content">{children}</main>
      </div>
    </div>
  )
}
