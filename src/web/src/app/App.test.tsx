import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { App } from './App'

// renderApp 为每个测试创建隔离 QueryClient，避免缓存跨用例泄漏。
function renderApp() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('运行概览', () => {
  it('renders the ready service state', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ status: 'ok' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )

    renderApp()

    expect(
      screen.getByRole('heading', { name: '运行概览' }),
    ).toBeInTheDocument()
    expect(await screen.findByText('服务正常')).toHaveAttribute(
      'role',
      'status',
    )
    expect(globalThis.fetch).toHaveBeenCalledWith(
      '/health/ready',
      expect.objectContaining({ method: 'GET' }),
    )
  })

  it('renders a generic unavailable state and supports refresh', async () => {
    const fetchMock = vi
      .spyOn(globalThis, 'fetch')
      .mockRejectedValue(new TypeError('connection refused'))
    renderApp()

    expect(await screen.findByText('服务不可用')).toHaveAttribute(
      'role',
      'status',
    )
    fireEvent.click(screen.getByRole('button', { name: '刷新服务状态' }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
    expect(document.body).not.toHaveTextContent('connection refused')
  })
})
