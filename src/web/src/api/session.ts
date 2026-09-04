// AccountType 是服务端会话允许进入产品路由的账号类型。
export type AccountType = 'mailbox' | 'administrator'

// PrincipalSummary 是当前本地主体的最小展示信息。
export interface PrincipalSummary {
  id: string
  displayName: string
}

// MailboxSummary 是邮箱账号会话绑定的唯一邮箱。
export interface MailboxSummary {
  id: string
  address: string
}

// AnonymousSession 表示浏览器当前没有可用的服务端会话。
export interface AnonymousSession {
  authenticated: false
}

// MailboxSession 表示只允许进入邮箱端的已认证会话。
export interface MailboxSession {
  authenticated: true
  accountType: 'mailbox'
  principal: PrincipalSummary
  tenantId: string
  mailbox: MailboxSummary
  permissions: string[]
  expiresAt: string
  csrfToken: string
}

// AdministratorSession 表示只允许进入管理端的已认证会话。
export interface AdministratorSession {
  authenticated: true
  accountType: 'administrator'
  principal: PrincipalSummary
  tenantId: string
  permissions: string[]
  expiresAt: string
  csrfToken: string
}

// UnsupportedSession 将服务端未知账号类型收敛到拒绝页，不暴露未校验字段。
export interface UnsupportedSession {
  authenticated: true
  accountType: 'unsupported'
  csrfToken: string
}

// CurrentSession 是匿名、受支持账号和防御性未知账号的会话判别联合。
export type CurrentSession =
  AnonymousSession | MailboxSession | AdministratorSession | UnsupportedSession

const anonymousSession: AnonymousSession = { authenticated: false }
const uuidPattern =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i
const permissionPattern = /^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)+$/
const dateTimePattern =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/

// isRecord 判断未知 JSON 是否可按普通对象继续校验。
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

// hasExactKeys 拒绝遗漏字段和服务端意外扩展，保持浏览器契约最小化。
function hasExactKeys(
  value: Record<string, unknown>,
  expectedKeys: readonly string[],
): boolean {
  const actualKeys = Object.keys(value).sort()
  const sortedExpectedKeys = [...expectedKeys].sort()
  return (
    actualKeys.length === sortedExpectedKeys.length &&
    actualKeys.every((key, index) => key === sortedExpectedKeys[index])
  )
}

// readPrincipal 校验主体字段并转换为前端命名。
function readPrincipal(value: unknown): PrincipalSummary | undefined {
  if (
    !isRecord(value) ||
    !hasExactKeys(value, ['id', 'display_name']) ||
    typeof value.id !== 'string' ||
    !uuidPattern.test(value.id) ||
    typeof value.display_name !== 'string' ||
    value.display_name.length < 1 ||
    value.display_name.length > 128
  ) {
    return undefined
  }
  return { id: value.id, displayName: value.display_name }
}

// readMailbox 校验邮箱绑定摘要，不接受空地址或超长值。
function readMailbox(value: unknown): MailboxSummary | undefined {
  if (
    !isRecord(value) ||
    !hasExactKeys(value, ['id', 'address']) ||
    typeof value.id !== 'string' ||
    !uuidPattern.test(value.id) ||
    typeof value.address !== 'string' ||
    value.address.length < 3 ||
    value.address.length > 320 ||
    !value.address.includes('@')
  ) {
    return undefined
  }
  return { id: value.id, address: value.address }
}

// readPermissions 校验、复制权限集合，避免响应数组被后续代码修改。
function readPermissions(value: unknown): string[] | undefined {
  if (
    !Array.isArray(value) ||
    value.some(
      (permission) =>
        typeof permission !== 'string' || !permissionPattern.test(permission),
    )
  ) {
    return undefined
  }
  const permissions = [...value]
  return new Set(permissions).size === permissions.length
    ? permissions
    : undefined
}

// readAuthenticatedFields 校验两类账号共享的服务端会话字段。
function readAuthenticatedFields(value: Record<string, unknown>) {
  const principal = readPrincipal(value.principal)
  const permissions = readPermissions(value.permissions)
  if (
    !principal ||
    !permissions ||
    typeof value.tenant_id !== 'string' ||
    !uuidPattern.test(value.tenant_id) ||
    typeof value.expires_at !== 'string' ||
    !dateTimePattern.test(value.expires_at) ||
    Number.isNaN(Date.parse(value.expires_at)) ||
    typeof value.csrf_token !== 'string' ||
    value.csrf_token.length < 32 ||
    value.csrf_token.length > 256
  ) {
    return undefined
  }
  return {
    principal,
    permissions,
    tenantId: value.tenant_id,
    expiresAt: value.expires_at,
    csrfToken: value.csrf_token,
  }
}

// readSessionResponse 对照 OpenAPI 判别联合解析响应，未知账号类型只保留拒绝语义。
export function readSessionResponse(payload: unknown): CurrentSession {
  if (!isRecord(payload) || typeof payload.authenticated !== 'boolean') {
    throw new Error('会话接口响应无效')
  }
  if (!payload.authenticated) {
    if (!hasExactKeys(payload, ['authenticated'])) {
      throw new Error('匿名会话响应无效')
    }
    return anonymousSession
  }
  if (
    payload.account_type !== 'mailbox' &&
    payload.account_type !== 'administrator'
  ) {
    if (
      typeof payload.csrf_token !== 'string' ||
      payload.csrf_token.length < 32 ||
      payload.csrf_token.length > 256
    ) {
      throw new Error('未知账号会话响应无效')
    }
    return {
      authenticated: true,
      accountType: 'unsupported',
      csrfToken: payload.csrf_token,
    }
  }

  const expectedKeys = [
    'authenticated',
    'principal',
    'account_type',
    'tenant_id',
    'permissions',
    'expires_at',
    'csrf_token',
  ]
  if (payload.account_type === 'mailbox') {
    expectedKeys.push('mailbox')
  }
  if (!hasExactKeys(payload, expectedKeys)) {
    throw new Error('已认证会话响应无效')
  }
  const shared = readAuthenticatedFields(payload)
  if (!shared) {
    throw new Error('已认证会话响应无效')
  }
  if (payload.account_type === 'mailbox') {
    const mailbox = readMailbox(payload.mailbox)
    if (!mailbox) {
      throw new Error('邮箱会话响应无效')
    }
    return {
      authenticated: true,
      accountType: 'mailbox',
      mailbox,
      ...shared,
    }
  }
  return {
    authenticated: true,
    accountType: 'administrator',
    ...shared,
  }
}

// getCurrentSession 查询当前同源服务端会话；401 按过期匿名态收敛。
export async function getCurrentSession(
  signal?: AbortSignal,
): Promise<CurrentSession> {
  const response = await fetch('/api/v1/session', {
    method: 'GET',
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal,
  })
  if (response.status === 401) {
    return anonymousSession
  }
  if (response.status !== 200) {
    throw new Error('会话服务暂时不可用')
  }
  return readSessionResponse(await response.json())
}

// logoutCurrentSession 使用当前会话 CSRF nonce 撤销服务端 Cookie 会话。
export async function logoutCurrentSession(csrfToken: string): Promise<void> {
  const response = await fetch('/api/v1/auth/logout', {
    method: 'POST',
    credentials: 'same-origin',
    headers: {
      Accept: 'application/json',
      'X-CSRF-Token': csrfToken,
    },
  })
  if (response.status !== 204) {
    throw new Error('退出失败')
  }
}
