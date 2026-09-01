import {
  Archive,
  FilePenLine,
  Inbox,
  MailOpen,
  MoreHorizontal,
  RefreshCw,
  Search,
  Send,
  Settings,
  Star,
  Trash2,
  type LucideIcon,
} from 'lucide-react'
import { Navigate, NavLink, useParams } from 'react-router-dom'

import { PreviewAccount } from '../../components/PreviewAccount'
import { ThemeMenu } from '../../components/ThemeMenu'
import { MailLayout } from '../../layouts/MailLayout'

// MailFolder 描述用户邮箱文件夹的稳定路由、标题和未连接状态。
interface MailFolder {
  path: string
  label: string
  title: string
  emptyTitle: string
  emptyDescription: string
  icon: LucideIcon
}

// mailFolders 是当前 Webmail 壳允许直接预览的系统文件夹清单。
const mailFolders: readonly MailFolder[] = [
  {
    path: 'inbox',
    label: '收件箱',
    title: '收件箱',
    emptyTitle: '邮箱尚未连接',
    emptyDescription: '邮件服务接通后，新收到的邮件会显示在这里。',
    icon: Inbox,
  },
  {
    path: 'starred',
    label: '星标',
    title: '星标邮件',
    emptyTitle: '暂无星标邮件',
    emptyDescription: '邮箱连接后，你标记的邮件会显示在这里。',
    icon: Star,
  },
  {
    path: 'sent',
    label: '已发送',
    title: '已发送',
    emptyTitle: '暂无已发送邮件',
    emptyDescription: '发送能力接通后，发送记录会显示在这里。',
    icon: Send,
  },
  {
    path: 'drafts',
    label: '草稿箱',
    title: '草稿箱',
    emptyTitle: '暂无草稿',
    emptyDescription: '草稿 API 接通后，未完成的邮件会显示在这里。',
    icon: FilePenLine,
  },
  {
    path: 'trash',
    label: '已删除',
    title: '已删除',
    emptyTitle: '暂无已删除邮件',
    emptyDescription: '邮箱连接后，已删除的邮件会显示在这里。',
    icon: Trash2,
  },
]

// MailWorkspace 渲染邮箱账号专用工作区；当前只展示真实的邮件 API 未连接状态。
export function MailWorkspace() {
  const { folder = 'inbox' } = useParams()
  const currentFolder = mailFolders.find((item) => item.path === folder)

  if (!currentFolder) {
    return <Navigate to="/mail/inbox" replace />
  }

  const EmptyIcon = currentFolder.icon
  const navigation = (
    <nav className="folder-nav" aria-label="邮箱文件夹">
      {mailFolders.map((item) => {
        const FolderIcon = item.icon
        return (
          <NavLink
            key={item.path}
            to={`/mail/${item.path}`}
            className={({ isActive }) => (isActive ? 'is-active' : undefined)}
          >
            <FolderIcon size={19} aria-hidden="true" />
            <span>{item.label}</span>
          </NavLink>
        )
      })}
    </nav>
  )
  const topbar = (
    <header className="mail-topbar">
      <label className="global-search">
        <Search size={18} aria-hidden="true" />
        <input
          type="search"
          aria-label="搜索邮件"
          placeholder="搜索主题、发件人或正文"
          disabled
        />
      </label>
      <div className="topbar-actions">
        <ThemeMenu />
        <button
          className="icon-button"
          type="button"
          aria-label="邮箱设置"
          title="邮箱服务尚未接入"
          disabled
        >
          <Settings size={18} aria-hidden="true" />
        </button>
        <PreviewAccount />
      </div>
    </header>
  )

  return (
    <MailLayout navigation={navigation} topbar={topbar}>
      <section
        className="message-list-pane"
        aria-labelledby="mail-folder-heading"
      >
        <header className="mail-section-heading">
          <div>
            <p className="eyebrow">我的邮箱</p>
            <h1 id="mail-folder-heading">{currentFolder.title}</h1>
          </div>
          <span className="status-label status-label--warning">未连接</span>
        </header>

        <div className="message-toolbar" aria-label="邮件列表工具栏">
          <input type="checkbox" aria-label="选择全部邮件" disabled />
          <span className="toolbar-divider" aria-hidden="true" />
          <button
            className="icon-button icon-button--quiet"
            type="button"
            aria-label="刷新邮件"
            title="邮箱尚未连接"
            disabled
          >
            <RefreshCw size={17} aria-hidden="true" />
          </button>
          <button
            className="icon-button icon-button--quiet"
            type="button"
            aria-label="归档邮件"
            title="邮箱尚未连接"
            disabled
          >
            <Archive size={17} aria-hidden="true" />
          </button>
          <button
            className="icon-button icon-button--quiet"
            type="button"
            aria-label="更多邮件操作"
            title="邮箱尚未连接"
            disabled
          >
            <MoreHorizontal size={18} aria-hidden="true" />
          </button>
        </div>

        <div className="mail-empty-state" role="status">
          <span className="empty-icon" aria-hidden="true">
            <EmptyIcon size={25} />
          </span>
          <h2>{currentFolder.emptyTitle}</h2>
          <p>{currentFolder.emptyDescription}</p>
        </div>
      </section>

      <section className="message-reader-pane" aria-label="邮件阅读面板">
        <div className="reader-empty-state">
          <span className="reader-empty-state__icon" aria-hidden="true">
            <MailOpen size={27} />
          </span>
          <h2>选择一封邮件</h2>
          <p>邮件连接后，可在这里安全阅读正文与附件。</p>
        </div>
      </section>
    </MailLayout>
  )
}
