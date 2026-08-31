import { Component, type ErrorInfo, type ReactNode } from 'react'

// ErrorBoundaryProps 描述应用级异常边界需要保护的内容。
interface ErrorBoundaryProps {
  children: ReactNode
}

// ErrorBoundaryState 只记录当前渲染树是否已经发生异常。
interface ErrorBoundaryState {
  hasError: boolean
}

// ErrorBoundary 阻止未捕获渲染异常留下空白页面。
export class ErrorBoundary extends Component<
  ErrorBoundaryProps,
  ErrorBoundaryState
> {
  public state: ErrorBoundaryState = { hasError: false }

  // getDerivedStateFromError 将任意渲染异常转换为稳定降级界面。
  public static getDerivedStateFromError(): ErrorBoundaryState {
    return { hasError: true }
  }

  // componentDidCatch 保留浏览器控制台诊断，不渲染异常细节。
  public componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error('应用渲染失败', error, info.componentStack)
  }

  // render 根据异常状态返回工作区或安全降级界面。
  public render(): ReactNode {
    if (this.state.hasError) {
      return (
        <main className="fatal-state">
          <h1>界面暂时不可用</h1>
          <button type="button" onClick={() => window.location.reload()}>
            重新加载
          </button>
        </main>
      )
    }
    return this.props.children
  }
}
