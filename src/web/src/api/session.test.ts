import { describe, expect, it, vi } from 'vitest'

import { getCurrentSession, readSessionResponse } from './session'

const activeSessionExpiry = new Date(Date.now() + 60 * 60 * 1000).toISOString()

const validMailboxPayload = {
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
}

describe('会话响应解析', () => {
  it('maps a valid mailbox session without exposing wire field names', () => {
    expect(readSessionResponse(validMailboxPayload)).toMatchObject({
      authenticated: true,
      accountType: 'mailbox',
      principal: { displayName: 'Mailbox Fixture' },
      mailbox: { address: 'mailbox@mail-suite.example.test' },
    })
  })

  it('rejects extra fields, duplicate permissions, and malformed CSRF values', () => {
    expect(() =>
      readSessionResponse({ authenticated: false, token: 'unexpected' }),
    ).toThrow('匿名会话响应无效')
    expect(() =>
      readSessionResponse({
        ...validMailboxPayload,
        permissions: ['mail.read', 'mail.read'],
      }),
    ).toThrow('已认证会话响应无效')
    expect(() =>
      readSessionResponse({ ...validMailboxPayload, csrf_token: 'short' }),
    ).toThrow('已认证会话响应无效')
  })

  it('maps an unknown authenticated account type only to rejection', () => {
    expect(
      readSessionResponse({
        authenticated: true,
        account_type: 'future-account',
        csrf_token: 'future-csrf-token-with-at-least-32-bytes',
        bearer_token: 'must-not-be-consumed',
      }),
    ).toEqual({
      authenticated: true,
      accountType: 'unsupported',
      csrfToken: 'future-csrf-token-with-at-least-32-bytes',
    })
  })

  it('normalizes a 401 response to an anonymous session', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(null, { status: 401 }),
    )

    await expect(getCurrentSession()).resolves.toEqual({
      authenticated: false,
    })
  })
})
