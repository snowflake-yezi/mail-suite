import { describe, expect, it, vi } from 'vitest'

import {
  getCurrentSession,
  logoutCurrentSession,
  readLogoutResponse,
  readSessionResponse,
} from './session'

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

describe('退出响应解析', () => {
  it('accepts the exact HTTPS provider logout response', async () => {
    const providerURL =
      'https://idp.example.test/logout?client_id=mail-suite-test&post_logout_redirect_uri=https%3A%2F%2Fmail.example.test%2F'
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(
        JSON.stringify({
          logged_out: true,
          provider_logout_url: providerURL,
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    )

    await expect(
      logoutCurrentSession('csrf-token-with-at-least-32-bytes'),
    ).resolves.toEqual({ providerLogoutUrl: providerURL })
    expect(globalThis.fetch).toHaveBeenCalledWith(
      '/api/v1/auth/logout',
      expect.objectContaining({
        method: 'POST',
        credentials: 'same-origin',
        headers: expect.objectContaining({
          'X-CSRF-Token': 'csrf-token-with-at-least-32-bytes',
        }),
      }),
    )
  })

  it.each([
    ['old 204 response', undefined, 204],
    [
      'wrong discriminator',
      {
        logged_out: false,
        provider_logout_url: 'https://idp.example.test/logout',
      },
      200,
    ],
    [
      'extra field',
      {
        logged_out: true,
        provider_logout_url: 'https://idp.example.test/logout',
        token: 'unexpected',
      },
      200,
    ],
    [
      'HTTP URL',
      {
        logged_out: true,
        provider_logout_url: 'http://idp.example.test/logout',
      },
      200,
    ],
    [
      'credential URL',
      {
        logged_out: true,
        provider_logout_url: 'https://user@idp.example.test/logout',
      },
      200,
    ],
    [
      'fragment URL',
      {
        logged_out: true,
        provider_logout_url: 'https://idp.example.test/logout#fragment',
      },
      200,
    ],
  ])('rejects %s', async (_name, payload, status) => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      payload === undefined
        ? new Response(null, { status })
        : new Response(JSON.stringify(payload), {
            status,
            headers: { 'Content-Type': 'application/json' },
          }),
    )

    await expect(
      logoutCurrentSession('csrf-token-with-at-least-32-bytes'),
    ).rejects.toThrow('退出')
  })

  it('rejects malformed values without invoking fetch', () => {
    expect(() => readLogoutResponse(null)).toThrow('退出接口响应无效')
    expect(() =>
      readLogoutResponse({
        logged_out: true,
        provider_logout_url: ' https://idp.example.test/logout',
      }),
    ).toThrow('退出接口响应无效')
  })
})
