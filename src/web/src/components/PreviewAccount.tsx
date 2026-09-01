import { UserRound } from 'lucide-react'

// PreviewAccount 明确标记当前页面尚未绑定真实登录主体，避免把 UI 预览误认为认证会话。
export function PreviewAccount() {
  return (
    <div className="preview-account" role="status">
      <span className="preview-account__icon" aria-hidden="true">
        <UserRound size={17} />
      </span>
      <span>
        <strong>未登录预览</strong>
        <small>会话服务未接入</small>
      </span>
    </div>
  )
}
