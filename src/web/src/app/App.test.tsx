import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { App } from './App'
import { ThemeProvider } from './theme/ThemeProvider'

// renderApp 为每个测试创建隔离路由、主题和 QueryClient，避免状态跨用例泄漏。
function renderApp(initialPath = '/') {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  return render(
    <ThemeProvider>
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={[initialPath]}>
          <App />
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>,
  )
}

afterEach(() => {
  vi.restoreAllMocks()
  window.localStorage.clear()
  delete document.documentElement.dataset.theme
})

describe('品牌登录与共享主题', () => {
  it('redirects the root route to the truthful login shell', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch')

    renderApp()

    expect(
      await screen.findByRole('heading', { name: '账号登录' }),
    ).toBeInTheDocument()
    expect(screen.getByRole('img', { name: 'Mail Suite' })).toHaveAttribute(
      'src',
      expect.stringContaining('logo.jpg'),
    )
    expect(screen.getByText('认证服务尚未接入')).toHaveAttribute(
      'role',
      'status',
    )
    expect(screen.getByRole('button', { name: '使用账号登录' })).toBeDisabled()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('persists one theme preference across product shells', async () => {
    renderApp('/login')

    fireEvent.change(screen.getByRole('combobox', { name: '主题' }), {
      target: { value: 'dark' },
    })

    await waitFor(() =>
      expect(document.documentElement).toHaveAttribute('data-theme', 'dark'),
    )
    expect(window.localStorage.getItem('mail-suite.theme')).toBe('dark')
  })

  it('returns an unknown route to login', async () => {
    renderApp('/not-a-route')

    expect(
      await screen.findByRole('heading', { name: '账号登录' }),
    ).toBeInTheDocument()
  })
})

describe('邮箱账号独立工作区', () => {
  it('renders an honest disconnected inbox without an admin entry', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch')

    renderApp('/mail/inbox')

    expect(
      await screen.findByRole('heading', { name: '收件箱' }),
    ).toBeInTheDocument()
    expect(screen.getAllByText('邮箱尚未连接').length).toBeGreaterThan(0)
    expect(screen.getByRole('button', { name: '写邮件' })).toBeDisabled()
    expect(screen.queryByRole('link', { name: /管理/ })).not.toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('navigates between mailbox folders', async () => {
    renderApp('/mail/inbox')

    fireEvent.click(screen.getByRole('link', { name: '星标' }))
    expect(
      await screen.findByRole('heading', { name: '星标邮件' }),
    ).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '星标' })).toHaveAttribute(
      'aria-current',
      'page',
    )
  })

  it('returns an unknown mail folder to the inbox', async () => {
    renderApp('/mail/not-a-folder')

    expect(
      await screen.findByRole('heading', { name: '收件箱' }),
    ).toBeInTheDocument()
  })
})

describe('管理账号独立控制台', () => {
  it('renders the ready service state without a mailbox entry', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ status: 'ok' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )

    renderApp('/admin/overview')

    expect(
      screen.getByRole('heading', { name: '运行概览' }),
    ).toBeInTheDocument()
    expect(await screen.findAllByText('服务正常')).not.toHaveLength(0)
    expect(screen.queryByRole('link', { name: '邮箱' })).not.toBeInTheDocument()
    expect(globalThis.fetch).toHaveBeenCalledWith(
      '/health/ready',
      expect.objectContaining({ method: 'GET' }),
    )
  })

  it('renders a generic unavailable state and supports refresh', async () => {
    const fetchMock = vi
      .spyOn(globalThis, 'fetch')
      .mockRejectedValue(new TypeError('connection refused'))
    renderApp('/admin/overview')

    expect(await screen.findAllByText('服务不可用')).not.toHaveLength(0)
    expect(screen.getByText('探针未确认')).toBeInTheDocument()
    expect(screen.queryByText('PostgreSQL 就绪')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '刷新服务状态' }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
    expect(document.body).not.toHaveTextContent('connection refused')
  })
})
