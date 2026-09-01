-- name: CreateAuthFlow :one
INSERT INTO auth_flows (
    id,
    state_digest,
    browser_cookie_digest,
    nonce_digest,
    pkce_verifier_ciphertext,
    encryption_key_id,
    return_to,
    created_at,
    expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, state_digest, browser_cookie_digest, nonce_digest, pkce_verifier_ciphertext,
    encryption_key_id, return_to, created_at, expires_at, consumed_at;

-- name: ConsumeAuthFlow :one
UPDATE auth_flows
SET consumed_at = $3
WHERE state_digest = $1
  AND browser_cookie_digest = $2
  AND consumed_at IS NULL
  AND expires_at > $3
RETURNING id, nonce_digest, pkce_verifier_ciphertext, encryption_key_id, return_to,
    created_at, expires_at, consumed_at;

-- name: GetActivePrincipalByExternalIdentity :one
SELECT
    identity_principals.id,
    identity_principals.tenant_id,
    identity_principals.account_type,
    identity_principals.mailbox_id,
    identity_principals.display_name,
    mailboxes.local_part AS mailbox_local_part,
    domains.name AS mailbox_domain
FROM identity_principals
JOIN tenants ON tenants.id = identity_principals.tenant_id
LEFT JOIN mailboxes ON mailboxes.id = identity_principals.mailbox_id
LEFT JOIN domains ON domains.id = mailboxes.domain_id
WHERE identity_principals.oidc_issuer = $1
  AND identity_principals.oidc_subject = $2
  AND identity_principals.status = 'active'
  AND tenants.status = 'active'
  AND (
      (
          identity_principals.account_type = 'mailbox'
          AND mailboxes.tenant_id = identity_principals.tenant_id
          AND mailboxes.desired_status = 'active'
      )
      OR (
          identity_principals.account_type = 'administrator'
          AND EXISTS (
              SELECT 1
              FROM principal_permissions
              WHERE principal_permissions.principal_id = identity_principals.id
                AND principal_permissions.permission_name = 'portal.admin.access'
          )
      )
  );

-- name: CreateUserSession :one
INSERT INTO user_sessions (
    id,
    session_digest,
    principal_id,
    oidc_issuer,
    oidc_subject,
    oidc_session_id,
    csrf_digest,
    created_at,
    last_seen_at,
    idle_expires_at,
    absolute_expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8, $9, $10)
RETURNING id, principal_id, created_at, last_seen_at, idle_expires_at, absolute_expires_at;

-- name: GetActiveSessionByDigest :one
SELECT
    user_sessions.id AS session_id,
    identity_principals.id AS principal_id,
    identity_principals.tenant_id,
    identity_principals.display_name,
    identity_principals.account_type,
    identity_principals.mailbox_id,
    mailboxes.local_part AS mailbox_local_part,
    domains.name AS mailbox_domain,
    user_sessions.created_at,
    user_sessions.last_seen_at,
    user_sessions.idle_expires_at,
    user_sessions.absolute_expires_at,
    user_sessions.csrf_digest
FROM user_sessions
JOIN identity_principals ON identity_principals.id = user_sessions.principal_id
JOIN tenants ON tenants.id = identity_principals.tenant_id
LEFT JOIN mailboxes ON mailboxes.id = identity_principals.mailbox_id
LEFT JOIN domains ON domains.id = mailboxes.domain_id
WHERE user_sessions.session_digest = sqlc.arg(session_digest)
  AND user_sessions.revoked_at IS NULL
  AND user_sessions.idle_expires_at > sqlc.arg(checked_at)
  AND user_sessions.absolute_expires_at > sqlc.arg(checked_at)
  AND identity_principals.status = 'active'
  AND tenants.status = 'active'
  AND (
      (
          identity_principals.account_type = 'mailbox'
          AND mailboxes.tenant_id = identity_principals.tenant_id
          AND mailboxes.desired_status = 'active'
      )
      OR (
          identity_principals.account_type = 'administrator'
          AND EXISTS (
              SELECT 1
              FROM principal_permissions
              WHERE principal_permissions.principal_id = identity_principals.id
                AND principal_permissions.permission_name = 'portal.admin.access'
          )
      )
  );

-- name: ListPrincipalPermissions :many
SELECT permission_name
FROM principal_permissions
WHERE principal_id = $1
ORDER BY permission_name;

-- name: TouchActiveSession :one
UPDATE user_sessions
SET
    last_seen_at = sqlc.arg(checked_at),
    idle_expires_at = LEAST(
        sqlc.arg(checked_at) + sqlc.arg(idle_timeout)::interval,
        absolute_expires_at
    )
WHERE id = sqlc.arg(session_id)
  AND revoked_at IS NULL
  AND idle_expires_at > sqlc.arg(checked_at)
  AND absolute_expires_at > sqlc.arg(checked_at)
RETURNING last_seen_at, idle_expires_at;

-- name: RevokeSessionByDigest :execrows
UPDATE user_sessions
SET revoked_at = $2, revocation_reason = $3
WHERE session_digest = $1 AND revoked_at IS NULL;

-- name: RevokeSessionsByOidcSessionID :execrows
UPDATE user_sessions
SET revoked_at = $3, revocation_reason = $4
WHERE oidc_issuer = $1
  AND oidc_session_id = $2
  AND revoked_at IS NULL;

-- name: RevokeSessionsByOidcSubject :execrows
UPDATE user_sessions
SET revoked_at = $3, revocation_reason = $4
WHERE oidc_issuer = $1
  AND oidc_subject = $2
  AND revoked_at IS NULL;

-- name: CreateAuthenticationAuditEvent :exec
INSERT INTO authentication_audit_events (
    id,
    principal_id,
    action,
    result,
    failure_category,
    source_digest,
    request_id,
    occurred_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);
