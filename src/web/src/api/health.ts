// HealthStatus 是健康契约允许返回的稳定状态集合。
export type HealthStatus = 'ok' | 'unavailable'

// HealthResponse 描述脚手架内部就绪接口的最小响应。
export interface HealthResponse {
  status: HealthStatus
  code?: 'DEPENDENCY_UNAVAILABLE'
}

// readHealthResponse 校验服务响应，拒绝把任意 JSON 当成健康状态。
async function readHealthResponse(response: Response): Promise<HealthResponse> {
  const payload: unknown = await response.json()
  if (
    typeof payload !== 'object' ||
    payload === null ||
    !('status' in payload) ||
    (payload.status !== 'ok' && payload.status !== 'unavailable')
  ) {
    throw new Error('健康接口响应无效')
  }

  if (response.status === 200 && payload.status === 'ok') {
    return { status: 'ok' }
  }
  if (
    response.status === 503 &&
    payload.status === 'unavailable' &&
    'code' in payload &&
    payload.code === 'DEPENDENCY_UNAVAILABLE'
  ) {
    return { status: 'unavailable', code: 'DEPENDENCY_UNAVAILABLE' }
  }
  throw new Error('健康接口状态与响应不一致')
}

// getServiceHealth 查询 API 就绪状态，不执行重试或业务副作用。
export async function getServiceHealth(
  signal?: AbortSignal,
): Promise<HealthResponse> {
  const response = await fetch('/health/ready', {
    method: 'GET',
    headers: { Accept: 'application/json' },
    signal,
  })
  return readHealthResponse(response)
}
