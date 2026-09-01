-- name: InsertOperation :one
INSERT INTO operations (
    id,
    tenant_id,
    kind,
    resource_type,
    resource_id,
    status,
    idempotency_key,
    request_hash
) VALUES (
    $1,
    $2,
    'mailbox.provision',
    'mailbox',
    $3,
    'pending',
    $4,
    $5
)
ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
RETURNING id, tenant_id, kind, resource_type, resource_id, status, idempotency_key,
    request_hash, created_at, updated_at;

-- name: GetOperationByIdempotencyKey :one
SELECT id, tenant_id, kind, resource_type, resource_id, status, idempotency_key,
    request_hash, created_at, updated_at
FROM operations
WHERE tenant_id = $1 AND idempotency_key = $2;

-- name: GetOperationForTenant :one
SELECT id, tenant_id, kind, resource_type, resource_id, status, idempotency_key,
    request_hash, created_at, updated_at
FROM operations
WHERE tenant_id = $1 AND id = $2;

-- name: GetProvisionableDomain :one
SELECT domains.id
FROM domains
JOIN tenants ON tenants.id = domains.tenant_id
WHERE domains.id = $1
  AND domains.tenant_id = $2
  AND domains.status = 'active'
  AND tenants.status = 'active';

-- name: InsertMailbox :one
INSERT INTO mailboxes (
    id,
    tenant_id,
    domain_id,
    local_part,
    display_name,
    desired_status,
    observed_status,
    revision
) VALUES ($1, $2, $3, $4, $5, 'active', 'unknown', 1)
RETURNING id, tenant_id, domain_id, local_part, display_name, desired_status,
    observed_status, revision, created_at, updated_at;

-- name: InsertOutboxEvent :exec
INSERT INTO outbox_events (
    id,
    tenant_id,
    operation_id,
    event_type,
    payload,
    status,
    attempts
) VALUES ($1, $2, $3, 'mailbox.provision.requested', $4, 'pending', 0);
