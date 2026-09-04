import { LogOut, UserRound } from 'lucide-react'
import { useState } from 'react'

import { useSession } from './useSession'

// SessionAccount 展示本地会话主体，并提供经过 CSRF 防护的退出命令。
export function SessionAccount() {
  const { session, logout } = useSession()
  const [isLoggingOut, setLoggingOut] = useState(false)
  const [logoutFailed, setLogoutFailed] = useState(false)

  if (!session?.authenticated || session.accountType === 'unsupported') {
    return null
  }

  const accountLabel =
    session.accountType === 'mailbox' ? session.mailbox.address : '管理员账号'

  const handleLogout = async () => {
    setLoggingOut(true)
    setLogoutFailed(false)
    try {
      await logout()
    } catch {
      setLogoutFailed(true)
    } finally {
      setLoggingOut(false)
    }
  }

  return (
    <div className="session-account-group">
      <div
        className="preview-account"
        aria-label={`当前账号：${session.principal.displayName}`}
        title={`${session.principal.displayName} · ${accountLabel}`}
      >
        <span className="preview-account__icon" aria-hidden="true">
          <UserRound size={17} />
        </span>
        <span>
          <strong>{session.principal.displayName}</strong>
          <small>{accountLabel}</small>
        </span>
      </div>
      <button
        className="icon-button"
        type="button"
        aria-label="退出登录"
        title="退出登录"
        disabled={isLoggingOut}
        onClick={() => void handleLogout()}
      >
        <LogOut size={17} aria-hidden="true" />
      </button>
      {logoutFailed ? (
        <span className="session-account-error" role="alert">
          退出失败，请重试
        </span>
      ) : null}
    </div>
  )
}
