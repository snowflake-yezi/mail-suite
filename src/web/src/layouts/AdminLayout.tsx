import { ChevronLeft, ChevronRight, Menu, X } from 'lucide-react'
import { useState, type ReactNode } from 'react'

import { AppLogo } from '../components/AppLogo'

// AdminLayoutProps 描述管理控制台外壳中的固定导航、顶栏与页面内容区域。
interface AdminLayoutProps {
  navigation: ReactNode
  topbar: ReactNode
  children: ReactNode
}

// AdminLayout 提供可折叠桌面侧栏和移动抽屉，不暴露邮箱工作区入口。
export function AdminLayout({
  navigation,
  topbar,
  children,
}: AdminLayoutProps) {
  const [isCollapsed, setCollapsed] = useState(false)
  const [isNavigationOpen, setNavigationOpen] = useState(false)

  return (
    <div className={`admin-shell ${isCollapsed ? 'is-collapsed' : ''}`}>
      <button
        className="mobile-menu-button"
        type="button"
        aria-label={isNavigationOpen ? '关闭管理导航' : '打开管理导航'}
        aria-expanded={isNavigationOpen}
        onClick={() => setNavigationOpen((isOpen) => !isOpen)}
      >
        {isNavigationOpen ? <X size={20} /> : <Menu size={20} />}
      </button>

      {isNavigationOpen ? (
        <button
          className="navigation-scrim"
          type="button"
          aria-label="关闭管理导航"
          onClick={() => setNavigationOpen(false)}
        />
      ) : null}

      <aside className={`admin-sidebar ${isNavigationOpen ? 'is-open' : ''}`}>
        <AppLogo subtitle="邮件服务管理控制台" to="/admin/overview" />
        {navigation}
        <button
          className="sidebar-collapse"
          type="button"
          aria-label={isCollapsed ? '展开管理侧栏' : '收起管理侧栏'}
          onClick={() => setCollapsed((collapsed) => !collapsed)}
        >
          {isCollapsed ? (
            <ChevronRight size={18} aria-hidden="true" />
          ) : (
            <ChevronLeft size={18} aria-hidden="true" />
          )}
          <span>收起侧栏</span>
        </button>
      </aside>

      <div className="admin-product">
        {topbar}
        <main className="admin-content">{children}</main>
      </div>
    </div>
  )
}
