import { useQuery } from '@tanstack/react-query'
import {
  Activity,
  CircleCheck,
  CircleX,
  Clock3,
  FileSearch,
  Gauge,
  Globe2,
  Inbox,
  RefreshCw,
  Search,
  Server,
  ShieldCheck,
  UsersRound,
  type LucideIcon,
} from 'lucide-react'
import { NavLink } from 'react-router-dom'

import { getServiceHealth } from '../../api/health'
import { PreviewAccount } from '../../components/PreviewAccount'
import { ThemeMenu } from '../../components/ThemeMenu'
import { AdminLayout } from '../../layouts/AdminLayout'

// AdminNavigationItem 描述管理侧栏入口及其当前可用状态。
interface AdminNavigationItem {
  label: string
  icon: LucideIcon
  path?: string
}

// adminNavigation 将控制台入口按管理任务分组，未接入的资源入口保持禁用。
const adminNavigation: readonly {
  label: string
  items: readonly AdminNavigationItem[]
}[] = [
  {
    label: '总览',
    items: [{ label: '运行概览', icon: Gauge, path: '/admin/overview' }],
  },
  {
    label: '资源',
    items: [
      { label: '服务与节点', icon: Server },
      { label: '域名', icon: Globe2 },
      { label: '邮箱账号', icon: UsersRound },
      { label: '邮件查询', icon: Inbox },
    ],
  },
  {
    label: '系统',
    items: [{ label: '审计日志', icon: ShieldCheck }],
  },
]

// AdminNavigation 渲染分组管理导航，不为尚无真实 API 的页面创建伪入口。
function AdminNavigation() {
  return (
    <nav className="admin-nav" aria-label="管理导航">
      {adminNavigation.map((group) => (
        <div className="admin-nav__group" key={group.label}>
          <p>{group.label}</p>
          {group.items.map((item) => {
            const ItemIcon = item.icon
            return item.path ? (
              <NavLink
                key={item.label}
                to={item.path}
                className={({ isActive }) =>
                  isActive ? 'admin-nav__item is-active' : 'admin-nav__item'
                }
              >
                <ItemIcon size={18} aria-hidden="true" />
                <span>{item.label}</span>
              </NavLink>
            ) : (
              <span
                className="admin-nav__item is-disabled"
                aria-disabled="true"
                key={item.label}
                title="业务 API 尚未接入"
              >
                <ItemIcon size={18} aria-hidden="true" />
                <span>{item.label}</span>
                <small>待接入</small>
              </span>
            )
          })}
        </div>
      ))}
    </nav>
  )
}

// UnavailableMetricProps 描述尚无业务 API 的管理指标占位状态。
interface UnavailableMetricProps {
  label: string
  icon: LucideIcon
}

// UnavailableMetric 用稳定尺寸展示不可用状态，不用硬编码数字伪造指标。
function UnavailableMetric({
  label,
  icon: MetricIcon,
}: UnavailableMetricProps) {
  return (
    <article className="metric-card is-unavailable">
      <span className="metric-card__icon" aria-hidden="true">
        <MetricIcon size={19} />
      </span>
      <div>
        <p>{label}</p>
        <strong>不可用</strong>
        <small>数据接口待接入</small>
      </div>
    </article>
  )
}

// AdminOverview 渲染管理账号专用概览，仅健康状态来自当前可用的真实探针。
export function AdminOverview() {
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
  const probeLabel = isPending
    ? '正在检查'
    : isOnline
      ? 'PostgreSQL 就绪'
      : '探针未确认'
  const checkedAt = healthQuery.dataUpdatedAt
    ? new Intl.DateTimeFormat('zh-CN', {
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
      }).format(healthQuery.dataUpdatedAt)
    : '尚未完成'
  const navigation = <AdminNavigation />
  const topbar = (
    <header className="admin-topbar">
      <div className="admin-breadcrumb">
        <strong>MAIL SUITE</strong>
        <span>/</span>
        <span>运行概览</span>
      </div>
      <label className="admin-search">
        <Search size={17} aria-hidden="true" />
        <input
          type="search"
          aria-label="搜索管理资源"
          placeholder="搜索域名、邮箱或 Message-ID"
          disabled
        />
      </label>
      <div className="topbar-actions">
        <ThemeMenu />
        <PreviewAccount />
      </div>
    </header>
  )

  return (
    <AdminLayout navigation={navigation} topbar={topbar}>
      <header className="admin-page-heading">
        <div>
          <p className="eyebrow">基础设施</p>
          <h1>运行概览</h1>
          <p>查看控制面可用性与管理资源接入状态。</p>
        </div>
        <button
          className="secondary-button refresh-button"
          type="button"
          aria-label="刷新服务状态"
          disabled={healthQuery.isFetching}
          onClick={() => void healthQuery.refetch()}
        >
          <RefreshCw size={17} aria-hidden="true" />
          <span>{healthQuery.isFetching ? '刷新中' : '刷新状态'}</span>
        </button>
      </header>

      <section className="metrics-grid" aria-label="运行指标">
        <article className="metric-card">
          <span
            className={`metric-card__icon ${isOnline ? 'is-online' : isPending ? 'is-pending' : 'is-offline'}`}
            aria-hidden="true"
          >
            <StatusIcon size={19} />
          </span>
          <div>
            <p>服务健康</p>
            <strong>{statusLabel}</strong>
            <small>控制面就绪探针</small>
          </div>
        </article>
        <UnavailableMetric label="管理域名" icon={Globe2} />
        <UnavailableMetric label="活动邮箱" icon={UsersRound} />
        <UnavailableMetric label="待处理异常" icon={FileSearch} />
      </section>

      <div className="overview-grid">
        <section className="service-panel" aria-labelledby="service-heading">
          <header className="panel-heading">
            <div className="panel-title">
              <span className="panel-title__icon" aria-hidden="true">
                <Server size={20} />
              </span>
              <div>
                <h2 id="service-heading">控制面 API</h2>
                <code>/health/ready</code>
              </div>
            </div>
            <span
              className={`status-label ${isPending ? 'status-label--warning' : isOnline ? 'status-label--success' : 'status-label--danger'}`}
              role="status"
            >
              <StatusIcon size={15} aria-hidden="true" />
              {statusLabel}
            </span>
          </header>
          <dl className="service-details">
            <div>
              <dt>探针</dt>
              <dd>{probeLabel}</dd>
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

        <aside
          className="connection-panel"
          aria-labelledby="connection-heading"
        >
          <div>
            <p className="eyebrow">资源数据</p>
            <h2 id="connection-heading">管理 API 待接入</h2>
          </div>
          <ul>
            <li>
              <span>服务与节点</span>
              <strong>未连接</strong>
            </li>
            <li>
              <span>域名与邮箱</span>
              <strong>未连接</strong>
            </li>
            <li>
              <span>邮件与审计</span>
              <strong>未连接</strong>
            </li>
          </ul>
        </aside>
      </div>
    </AdminLayout>
  )
}
