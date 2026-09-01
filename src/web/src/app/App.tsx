import { Navigate, Route, Routes } from 'react-router-dom'

import { AccessDeniedPage } from '../features/access/AccessDeniedPage'
import { AdminOverview } from '../features/admin-overview/AdminOverview'
import { MailWorkspace } from '../features/mail/MailWorkspace'
import { LoginLayout } from '../layouts/LoginLayout'

// App 声明同一 SPA 内用户端、管理端及各自的未知路径回退规则。
export function App() {
  return (
    <Routes>
      <Route path="/" element={<Navigate to="/login" replace />} />
      <Route path="/login" element={<LoginLayout />} />
      <Route path="/forbidden" element={<AccessDeniedPage />} />
      <Route path="/mail" element={<Navigate to="/mail/inbox" replace />} />
      <Route path="/mail/:folder" element={<MailWorkspace />} />
      <Route path="/mail/*" element={<Navigate to="/mail/inbox" replace />} />
      <Route
        path="/admin"
        element={<Navigate to="/admin/overview" replace />}
      />
      <Route path="/admin/overview" element={<AdminOverview />} />
      <Route
        path="/admin/*"
        element={<Navigate to="/admin/overview" replace />}
      />
      <Route path="*" element={<Navigate to="/login" replace />} />
    </Routes>
  )
}
