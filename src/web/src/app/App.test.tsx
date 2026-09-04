import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { sessionQueryKey } from '../features/auth/session'
import { App } from './App'
import { ThemeProvider } from './theme/ThemeProvider'

const activeSessionExpiry = new Date(Date.now() + 60 * 60 * 1000).toISOString()

const mailboxSession = {
  authenticated: true,
  principal: {
    id: '30000000-0000-4000-8000-000000000003',
    display_name: 'Mailbox Fixture',
  },
  account_type: 'mailbox',
  tenant_id: '30000000-0000-4000-8000-000000000001',
  mailbox: {
    id: '30000000-0000-4000-8000-000000000004',
    address: 'mailbox@mail-suite.example.test',
  },
  permissions: [],
  expires_at: activeSessionExpiry,
  csrf_token: 'csrf-token-with-at-least-32-bytes',
} as const

const administratorSession = {
  authenticated: true,
  principal: {
    id: '30000000-0000-4000-8000-000000000005',
    display_name: 'Administrator Fixture',
  },
  account_type: 'administrator',
  tenant_id: '30000000-0000-4000-8000-000000000001',
  permissions: ['portal.admin.access'],
  expires_at: activeSessionExpiry,
  csrf_token: 'admin-csrf-token-with-at-least-32-bytes',
} as const

// jsonResponse 创建与浏览器会话或健康接口一致的 JSON 响应。
function jsonResponse(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

// requestPath 从 fetch 的三种标准输入中提取相对或绝对 URL。
function requestPath(input: RequestInfo | URL): string {
  if (typeof input === 'string') {
    return input
  }
  return input instanceof URL ? input.toString() : input.url
}

// mockApplicationFetch 按端点返回会话、退出和健康响应。
function mockApplicationFetch(options?: {
  session?: unknown
  sessionStatus?: number
  health?: 'ok' | 'unavailable'
  logoutStatus?: number
}) {
  const {
    session = { authenticated: false },
    sessionStatus = 200,
    health = 'ok',
    logoutStatus = 204,
  } = options ?? {}
  return vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const path = requestPath(input)
    if (path.includes('/api/v1/session')) {
      return Promise.resolve(jsonResponse(session, sessionStatus))
    }
    if (path.includes('/api/v1/auth/logout')) {
      return Promise.resolve(new Response(null, { status: logoutStatus }))
    }
    if (path.includes('/health/ready')) {
      return Promise.resolve(
        health === 'ok'
          ? jsonResponse({ status: 'ok' })
          : jsonResponse(
              { status: 'unavailable', code: 'DEPENDENCY_UNAVAILABLE' },
              503,
            ),
      )
    }
    return Promise.reject(new Error('测试收到未声明请求'))
  })
}

// renderApp 为每个测试创建隔离路由、主题和 QueryClient。
function renderApp(initialPath = '/') {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const result = render(
    <ThemeProvider>
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={[initialPath]}>
          <App />
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>,
  )
  return { ...result, queryClient }
}

afterEach(() => {
  vi.restoreAllMocks()
  window.localStorage.clear()
  delete document.documentElement.dataset.theme
})

describe('浏览器会话入口', () => {
  it('shows an enabled backend OIDC login for an anonymous session', async () => {
    const fetchMock = mockApplicationFetch()

    renderApp()

    expect(
      await screen.findByRole('heading', { name: '账号登录' }),
    ).toBeInTheDocument()
    expect(screen.getByText('组织身份服务已连接')).toHaveAttribute(
      'role',
      'status',
    )
    expect(screen.getByRole('link', { name: '使用账号登录' })).toHaveAttribute(
      'href',
      '/api/v1/auth/login',
    )
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/session',
      expect.objectContaining({ method: 'GET', credentials: 'same-origin' }),
    )
  })

  it('preserves a same-section return path when redirecting anonymously', async () => {
    mockApplicationFetch()

    renderApp('/mail/inbox')

    expect(
      await screen.findByRole('link', { name: '使用账号登录' }),
    ).toHaveAttribute('href', '/api/v1/auth/login?return_to=%2Fmail%2Finbox')
    expect(
      screen.queryByRole('heading', { name: '收件箱' }),
    ).not.toBeInTheDocument()
  })

  it('does not flash protected content while the session is loading', () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation(
      () => new Promise<Response>(() => undefined),
    )

    renderApp('/admin/overview')

    expect(
      screen.getByRole('heading', { name: '正在确认登录状态' }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('heading', { name: '运行概览' }),
    ).not.toBeInTheDocument()
  })

  it('recovers from a session service failure without creating a session', async () => {
    const fetchMock = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(jsonResponse({}, 503))
      .mockResolvedValueOnce(jsonResponse({ authenticated: false }))

    renderApp('/login')

    fireEvent.click(await screen.findByRole('button', { name: '重新连接' }))
    expect(
      await screen.findByRole('link', { name: '使用账号登录' }),
    ).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('treats a 401 session response as an expired anonymous session', async () => {
    mockApplicationFetch({ sessionStatus: 401 })

    renderApp('/mail/inbox')

    expect(
      await screen.findByRole('heading', { name: '账号登录' }),
    ).toBeInTheDocument()
  })

  it('fails closed when the session endpoint rejects access with 403', async () => {
    mockApplicationFetch({ sessionStatus: 403 })

    renderApp('/admin/overview')

    expect(
      await screen.findByRole('heading', {
        name: '暂时无法连接登录服务',
      }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('heading', { name: '运行概览' }),
    ).not.toBeInTheDocument()
  })

  it('rejects an unknown authenticated account type', async () => {
    mockApplicationFetch({
      session: {
        authenticated: true,
        account_type: 'unexpected',
        csrf_token: 'unexpected-csrf-token-with-at-least-32-bytes',
      },
    })

    renderApp('/admin/overview')

    expect(
      await screen.findByRole('heading', {
        name: '当前账号无权访问此区域',
      }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('heading', { name: '运行概览' }),
    ).not.toBeInTheDocument()
  })

  it('persists only the shared theme preference', async () => {
    mockApplicationFetch()
    renderApp('/login')

    fireEvent.change(await screen.findByRole('combobox', { name: '主题' }), {
      target: { value: 'dark' },
    })

    await waitFor(() =>
      expect(document.documentElement).toHaveAttribute('data-theme', 'dark'),
    )
    expect(window.localStorage).toHaveLength(1)
    expect(window.localStorage.getItem('mail-suite.theme')).toBe('dark')
  })
})

describe('邮箱账号路由', () => {
  it('renders the bound mailbox identity and disconnected mailbox state', async () => {
    mockApplicationFetch({ session: mailboxSession })

    renderApp('/mail/inbox')

    expect(
      await screen.findByRole('heading', { name: '收件箱' }),
    ).toBeInTheDocument()
    expect(screen.getByText('Mailbox Fixture')).toBeInTheDocument()
    expect(
      screen.getByText('mailbox@mail-suite.example.test'),
    ).toBeInTheDocument()
    expect(screen.getAllByText('邮箱尚未连接').length).toBeGreaterThan(0)
    expect(screen.getByRole('button', { name: '写邮件' })).toBeDisabled()
  })

  it('navigates between allowed mailbox folders', async () => {
    mockApplicationFetch({ session: mailboxSession })
    renderApp('/mail/inbox')

    fireEvent.click(await screen.findByRole('link', { name: '星标' }))
    expect(
      await screen.findByRole('heading', { name: '星标邮件' }),
    ).toBeInTheDocument()
  })

  it('rejects mailbox access to the administrator section', async () => {
    mockApplicationFetch({ session: mailboxSession })

    renderApp('/admin/overview')

    expect(
      await screen.findByRole('heading', {
        name: '当前账号无权访问此区域',
      }),
    ).toBeInTheDocument()
  })
})

describe('管理账号路由', () => {
  it('renders the management shell and current administrator', async () => {
    const fetchMock = mockApplicationFetch({
      session: administratorSession,
    })

    renderApp('/admin/overview')

    expect(
      await screen.findByRole('heading', { name: '运行概览' }),
    ).toBeInTheDocument()
    expect(await screen.findAllByText('服务正常')).not.toHaveLength(0)
    expect(screen.getByText('Administrator Fixture')).toBeInTheDocument()
    expect(screen.getByText('管理员账号')).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledWith(
      '/health/ready',
      expect.objectContaining({ method: 'GET' }),
    )
  })

  it('rejects administrator access to the mailbox section', async () => {
    mockApplicationFetch({ session: administratorSession })

    renderApp('/mail/inbox')

    expect(
      await screen.findByRole('heading', {
        name: '当前账号无权访问此区域',
      }),
    ).toBeInTheDocument()
  })
})

describe('退出与会话失效', () => {
  it('sends the CSRF nonce, clears cached data, and returns to login', async () => {
    const fetchMock = mockApplicationFetch({ session: mailboxSession })
    const { queryClient } = renderApp('/mail/inbox')
    queryClient.setQueryData(['sensitive-mail'], { subject: 'private' })

    fireEvent.click(await screen.findByRole('button', { name: '退出登录' }))

    expect(
      await screen.findByRole('heading', { name: '账号登录' }),
    ).toBeInTheDocument()
    expect(queryClient.getQueryData(['sensitive-mail'])).toBeUndefined()
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/auth/logout',
      expect.objectContaining({
        method: 'POST',
        credentials: 'same-origin',
        headers: expect.objectContaining({
          'X-CSRF-Token': mailboxSession.csrf_token,
        }),
      }),
    )
  })

  it('keeps the current session visible when logout fails', async () => {
    mockApplicationFetch({ session: mailboxSession, logoutStatus: 503 })
    renderApp('/mail/inbox')

    fireEvent.click(await screen.findByRole('button', { name: '退出登录' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('退出失败')
    expect(screen.getByRole('heading', { name: '收件箱' })).toBeInTheDocument()
  })

  it('clears sensitive caches when a refreshed session expires', async () => {
    const fetchMock = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(jsonResponse(mailboxSession))
      .mockResolvedValueOnce(jsonResponse({ authenticated: false }))
    const { queryClient } = renderApp('/mail/inbox')

    expect(
      await screen.findByRole('heading', { name: '收件箱' }),
    ).toBeInTheDocument()
    queryClient.setQueryData(['sensitive-mail'], { subject: 'private' })
    await queryClient.invalidateQueries({ queryKey: sessionQueryKey })

    expect(
      await screen.findByRole('heading', { name: '账号登录' }),
    ).toBeInTheDocument()
    expect(queryClient.getQueryData(['sensitive-mail'])).toBeUndefined()
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  // identityBoundarySessions 分别覆盖决定业务缓存归属的四类身份字段。
  const identityBoundarySessions: Array<[string, unknown]> = [
    [
      'principal',
      {
        ...mailboxSession,
        principal: {
          id: '30000000-0000-4000-8000-000000000013',
          display_name: 'Replacement Mailbox',
        },
      },
    ],
    [
      'tenant',
      {
        ...mailboxSession,
        tenant_id: '30000000-0000-4000-8000-000000000011',
      },
    ],
    [
      'mailbox binding',
      {
        ...mailboxSession,
        mailbox: {
          id: '30000000-0000-4000-8000-000000000014',
          address: 'replacement@mail-suite.example.test',
        },
      },
    ],
    ['account type', administratorSession],
  ]

  it.each(identityBoundarySessions)(
    'clears sensitive caches when the authenticated %s changes',
    async (_boundary, replacementSession) => {
      let sessionRequests = 0
      vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
        if (!requestPath(input).includes('/api/v1/session')) {
          return Promise.reject(new Error('测试收到未声明请求'))
        }
        const session =
          sessionRequests++ === 0 ? mailboxSession : replacementSession
        return Promise.resolve(jsonResponse(session))
      })
      const { queryClient } = renderApp('/mail/inbox')

      expect(
        await screen.findByText('mailbox@mail-suite.example.test'),
      ).toBeInTheDocument()
      queryClient.setQueryData(['sensitive-mail'], { subject: 'private' })
      await queryClient.invalidateQueries({ queryKey: sessionQueryKey })

      await waitFor(() => expect(sessionRequests).toBe(2))
      await waitFor(() =>
        expect(queryClient.getQueryData(['sensitive-mail'])).toBeUndefined(),
      )
    },
  )

  it('rechecks an already expired session before rendering protected content', async () => {
    const expiredSession = {
      ...mailboxSession,
      expires_at: new Date(Date.now() - 60 * 1000).toISOString(),
    }
    const fetchMock = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(jsonResponse(expiredSession))
      .mockResolvedValueOnce(jsonResponse({ authenticated: false }))

    renderApp('/mail/inbox')

    expect(
      await screen.findByRole('heading', { name: '账号登录' }),
    ).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })
})
