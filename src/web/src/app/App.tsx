import { useQuery } from '@tanstack/react-query'
import {
  Activity,
  CircleCheck,
  CircleX,
  Clock3,
  RefreshCw,
  Server,
} from 'lucide-react'
import { Navigate, Route, Routes } from 'react-router-dom'

import { getServiceHealth } from '../api/health'

// Workspace 是脚手架阶段唯一获批的管理端工作区。
function Workspace() {
  const healthQuery = useQuery({
    queryKey: ['service-health'],
    queryFn: ({ signal }) => getServiceHealth(signal),
    retry: false,
    refetchInterval: 30_000,
  })

  const isOnline = healthQuery.data?.status === 'ok' && !healthQuery.isError
  const isPending = healthQuery.isPending
  const statusLabel = isPending
    ? '检查中'
    : isOnline
      ? '服务正常'
      : '服务不可用'
  const StatusIcon = isPending ? Activity : isOnline ? CircleCheck : CircleX
  const checkedAt = healthQuery.dataUpdatedAt
    ? new Intl.DateTimeFormat('zh-CN', {
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
      }).format(healthQuery.dataUpdatedAt)
    : '尚未完成'

  return (
    <div className="workspace-shell">
      <aside className="sidebar">
        <div className="brand-lockup" aria-label="mail-suite">
          <span className="brand-mark" aria-hidden="true">
            MS
          </span>
          <div>
            <strong>mail-suite</strong>
            <span>管理工作区</span>
          </div>
        </div>

        <nav aria-label="主导航">
          <a href="/" aria-current="page" className="nav-item">
            <Activity size={18} aria-hidden="true" />
            运行概览
          </a>
        </nav>

        <div className="sidebar-foot">
          <span className="environment-dot" aria-hidden="true" />
          本地环境
        </div>
      </aside>

      <main className="workspace-main">
        <header className="topbar">
          <div>
            <p className="section-label">基础设施</p>
            <h1>运行概览</h1>
          </div>
          <button
            className="icon-button"
            type="button"
            aria-label="刷新服务状态"
            title="刷新服务状态"
            disabled={healthQuery.isFetching}
            onClick={() => void healthQuery.refetch()}
          >
            <RefreshCw size={18} aria-hidden="true" />
          </button>
        </header>

        <section
          className="status-panel"
          aria-labelledby="service-status-heading"
        >
          <div className="panel-heading">
            <div className="panel-title">
              <span className="service-icon" aria-hidden="true">
                <Server size={20} />
              </span>
              <div>
                <h2 id="service-status-heading">控制面 API</h2>
                <code>/health/ready</code>
              </div>
            </div>
            <span
              className={`status-badge ${isPending ? 'is-pending' : isOnline ? 'is-online' : 'is-offline'}`}
              role="status"
            >
              <StatusIcon size={16} aria-hidden="true" />
              {statusLabel}
            </span>
          </div>

          <dl className="status-details">
            <div>
              <dt>探针</dt>
              <dd>PostgreSQL 就绪</dd>
            </div>
            <div>
              <dt>检查周期</dt>
              <dd>30 秒</dd>
            </div>
            <div>
              <dt>
                <Clock3 size={15} aria-hidden="true" />
                最近检查
              </dt>
              <dd className="tabular-value">{checkedAt}</dd>
            </div>
          </dl>
        </section>
      </main>
    </div>
  )
}

// App 声明管理端的最小路由边界，未知路径统一回到运行概览。
export function App() {
  return (
    <Routes>
      <Route path="/" element={<Workspace />} />
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
