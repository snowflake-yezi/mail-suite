-- name: InsertOperation :one
INSERT INTO operations (
    id,
    tenant_id,
    kind,
    resource_type,
    resource_id,
    status,
    idempotency_key,
    request_hash,
    desired_revision,
    configuration_hash
) VALUES (
    $1,
    $2,
    'mailbox.provision',
    'mailbox',
    $3,
    'pending',
    $4,
    $5,
    1,
    $5
)
ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
RETURNING id, tenant_id, kind, resource_type, resource_id, status, idempotency_key,
    request_hash, created_at, updated_at, desired_revision, configuration_hash,
    attempt_count, next_attempt_at, lease_owner_id, lease_epoch, lease_expires_at,
    last_error_code, observed_status, observed_revision, observed_configuration_hash,
    observed_at;

-- name: GetOperationByIdempotencyKey :one
SELECT id, tenant_id, kind, resource_type, resource_id, status, idempotency_key,
    request_hash, created_at, updated_at, desired_revision, configuration_hash,
    attempt_count, next_attempt_at, lease_owner_id, lease_epoch, lease_expires_at,
    last_error_code, observed_status, observed_revision, observed_configuration_hash,
    observed_at
FROM operations
WHERE tenant_id = $1 AND idempotency_key = $2;

-- name: GetOperationForTenant :one
SELECT id, tenant_id, kind, resource_type, resource_id, status, idempotency_key,
    request_hash, created_at, updated_at, desired_revision, configuration_hash,
    attempt_count, next_attempt_at, lease_owner_id, lease_epoch, lease_expires_at,
    last_error_code, observed_status, observed_revision, observed_configuration_hash,
    observed_at
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

-- name: FinalizeMailboxOperationBeforeClaim :one
WITH candidate AS (
    SELECT
        operations.id,
        operations.tenant_id,
        operations.status AS prior_status,
        operations.attempt_count,
        operations.lease_epoch,
        CASE
            WHEN mailboxes.revision > operations.desired_revision THEN 'superseded'
            ELSE 'dead'
        END::text AS result_status,
        CASE
            WHEN mailboxes.revision > operations.desired_revision
                THEN 'OPERATION_REVISION_SUPERSEDED'
            ELSE 'WORKER_ATTEMPTS_EXHAUSTED'
        END::text AS error_code
    FROM operations
    JOIN mailboxes
      ON mailboxes.id = operations.resource_id
     AND mailboxes.tenant_id = operations.tenant_id
    WHERE operations.kind = 'mailbox.provision'
      AND operations.resource_type = 'mailbox'
      AND operations.status IN ('pending', 'running', 'retry_wait', 'unknown')
      AND (
          mailboxes.revision > operations.desired_revision
          OR (
              mailboxes.revision = operations.desired_revision
              AND operations.attempt_count >= sqlc.arg(max_attempts)::integer
              AND (
                  operations.status IN ('pending', 'retry_wait', 'unknown')
                  OR operations.lease_expires_at <= statement_timestamp()
              )
          )
      )
    ORDER BY
        CASE WHEN mailboxes.revision > operations.desired_revision THEN 0 ELSE 1 END,
        operations.created_at,
        operations.id
    FOR UPDATE OF operations, mailboxes SKIP LOCKED
    LIMIT 1
), completed_attempt AS (
    UPDATE operation_attempts
    SET completed_at = statement_timestamp(),
        result_status = candidate.result_status,
        error_code = candidate.error_code
    FROM candidate
    WHERE candidate.prior_status = 'running'
      AND operation_attempts.operation_id = candidate.id
      AND operation_attempts.attempt_number = candidate.attempt_count
      AND operation_attempts.lease_epoch = candidate.lease_epoch
      AND operation_attempts.completed_at IS NULL
    RETURNING operation_attempts.id
), finalized_operation AS (
    UPDATE operations
    SET status = candidate.result_status,
        next_attempt_at = statement_timestamp(),
        lease_owner_id = NULL,
        lease_expires_at = NULL,
        last_error_code = candidate.error_code,
        updated_at = statement_timestamp()
    FROM candidate
    WHERE operations.id = candidate.id
    RETURNING operations.id, operations.tenant_id
), delivered_wakeup AS (
    UPDATE outbox_events
    SET status = 'delivered',
        attempts = outbox_events.attempts + 1,
        delivered_at = statement_timestamp()
    FROM finalized_operation
    WHERE outbox_events.operation_id = finalized_operation.id
      AND outbox_events.tenant_id = finalized_operation.tenant_id
      AND outbox_events.status IN ('pending', 'processing')
    RETURNING outbox_events.operation_id
)
SELECT EXISTS (SELECT 1 FROM finalized_operation) AS applied;

-- name: ClaimMailboxOperation :one
WITH candidate AS (
    SELECT
        operations.id,
        operations.tenant_id,
        operations.resource_id AS mailbox_id,
        operations.status AS prior_status,
        operations.desired_revision,
        operations.configuration_hash,
        operations.attempt_count,
        operations.lease_epoch,
        mailboxes.domain_id,
        (mailboxes.local_part || '@' || domains.name)::text AS address
    FROM operations
    JOIN mailboxes
      ON mailboxes.id = operations.resource_id
     AND mailboxes.tenant_id = operations.tenant_id
    JOIN domains
      ON domains.id = mailboxes.domain_id
     AND domains.tenant_id = mailboxes.tenant_id
    WHERE operations.kind = 'mailbox.provision'
      AND operations.resource_type = 'mailbox'
      AND mailboxes.desired_status = 'active'
      AND mailboxes.revision = operations.desired_revision
      AND operations.attempt_count < sqlc.arg(max_attempts)::integer
      AND (
          (
              operations.status IN ('pending', 'retry_wait', 'unknown')
              AND operations.next_attempt_at <= statement_timestamp()
          )
          OR
          (
              operations.status = 'running'
              AND operations.lease_expires_at <= statement_timestamp()
          )
      )
    ORDER BY
        CASE
            WHEN operations.status = 'running' THEN operations.lease_expires_at
            ELSE operations.next_attempt_at
        END,
        operations.created_at,
        operations.id
    FOR UPDATE OF operations, mailboxes SKIP LOCKED
    LIMIT 1
), expired_attempt AS (
    UPDATE operation_attempts
    SET completed_at = statement_timestamp(),
        result_status = 'unknown',
        error_code = 'WORKER_LEASE_EXPIRED'
    FROM candidate
    WHERE candidate.prior_status = 'running'
      AND operation_attempts.operation_id = candidate.id
      AND operation_attempts.attempt_number = candidate.attempt_count
      AND operation_attempts.lease_epoch = candidate.lease_epoch
      AND operation_attempts.completed_at IS NULL
    RETURNING operation_attempts.id
), claimed AS (
    UPDATE operations
    SET status = 'running',
        attempt_count = operations.attempt_count + 1,
        lease_owner_id = sqlc.arg(owner_id)::uuid,
        lease_epoch = operations.lease_epoch + 1,
        lease_expires_at = statement_timestamp()
            + sqlc.arg(lease_duration_microseconds)::bigint * interval '1 microsecond',
        updated_at = statement_timestamp()
    FROM candidate
    WHERE operations.id = candidate.id
    RETURNING
        operations.id AS operation_id,
        operations.tenant_id,
        candidate.mailbox_id,
        candidate.domain_id,
        candidate.address,
        operations.desired_revision,
        operations.configuration_hash,
        operations.attempt_count AS attempt_number,
        candidate.prior_status,
        sqlc.arg(owner_id)::uuid AS lease_owner_id,
        operations.lease_epoch,
        operations.lease_expires_at
), inserted_attempt AS (
    INSERT INTO operation_attempts (
        operation_id,
        tenant_id,
        attempt_number,
        lease_owner_id,
        lease_epoch,
        prior_status,
        desired_revision,
        configuration_hash,
        started_at
    )
    SELECT
        claimed.operation_id,
        claimed.tenant_id,
        claimed.attempt_number,
        claimed.lease_owner_id,
        claimed.lease_epoch,
        claimed.prior_status,
        claimed.desired_revision,
        claimed.configuration_hash,
        statement_timestamp()
    FROM claimed
    RETURNING operation_id
), delivered_wakeup AS (
    UPDATE outbox_events
    SET status = 'delivered',
        attempts = outbox_events.attempts + 1,
        delivered_at = statement_timestamp()
    FROM claimed
    WHERE outbox_events.operation_id = claimed.operation_id
      AND outbox_events.tenant_id = claimed.tenant_id
      AND outbox_events.status IN ('pending', 'processing')
    RETURNING outbox_events.operation_id
)
SELECT
    claimed.operation_id,
    claimed.tenant_id,
    claimed.mailbox_id,
    claimed.domain_id,
    claimed.address,
    claimed.desired_revision,
    claimed.configuration_hash,
    claimed.attempt_number,
    claimed.prior_status,
    claimed.lease_owner_id,
    claimed.lease_epoch,
    claimed.lease_expires_at
FROM claimed;

-- name: ResolveMailboxOperation :one
WITH candidate AS (
    SELECT
        operations.id,
        operations.tenant_id,
        operations.resource_id,
        operations.desired_revision,
        operations.configuration_hash
    FROM operations
    JOIN mailboxes
      ON mailboxes.id = operations.resource_id
     AND mailboxes.tenant_id = operations.tenant_id
    WHERE operations.id = sqlc.arg(operation_id)::uuid
      AND operations.status = 'running'
      AND operations.lease_owner_id = sqlc.arg(lease_owner_id)::uuid
      AND operations.lease_epoch = sqlc.arg(lease_epoch)::bigint
      AND operations.attempt_count = sqlc.arg(attempt_number)::integer
      AND operations.desired_revision = sqlc.arg(desired_revision)::bigint
      AND operations.lease_expires_at > statement_timestamp()
      AND (
          NOT sqlc.arg(has_observation)::boolean
          OR sqlc.arg(observed_mailbox_id)::uuid = operations.resource_id
      )
      AND (
          sqlc.arg(result_status)::text <> 'succeeded'
          OR (
              sqlc.arg(has_observation)::boolean
              AND sqlc.arg(observed_status)::text = 'active'
              AND sqlc.arg(observed_revision)::bigint = operations.desired_revision
              AND sqlc.arg(observed_configuration_hash)::bytea = operations.configuration_hash
              AND mailboxes.revision = operations.desired_revision
              AND mailboxes.desired_status = 'active'
          )
      )
    FOR UPDATE OF operations, mailboxes
), updated_mailbox AS (
    UPDATE mailboxes
    SET observed_status = 'active',
        observed_revision = sqlc.arg(observed_revision)::bigint,
        observed_configuration_hash = sqlc.arg(observed_configuration_hash)::bytea,
        observed_at = sqlc.arg(observed_at)::timestamptz,
        updated_at = statement_timestamp()
    FROM candidate
    WHERE sqlc.arg(result_status)::text = 'succeeded'
      AND mailboxes.id = candidate.resource_id
      AND mailboxes.tenant_id = candidate.tenant_id
      AND mailboxes.revision = candidate.desired_revision
    RETURNING mailboxes.id
), updated_operation AS (
    UPDATE operations
    SET status = sqlc.arg(result_status)::text,
        next_attempt_at = CASE
            WHEN sqlc.arg(result_status)::text IN ('retry_wait', 'unknown')
                THEN statement_timestamp()
                    + sqlc.arg(next_attempt_delay_microseconds)::bigint * interval '1 microsecond'
            ELSE statement_timestamp()
        END,
        lease_owner_id = NULL,
        lease_expires_at = NULL,
        last_error_code = NULLIF(sqlc.arg(error_code)::text, ''),
        observed_status = CASE
            WHEN sqlc.arg(has_observation)::boolean THEN sqlc.arg(observed_status)::text
            ELSE operations.observed_status
        END,
        observed_revision = CASE
            WHEN sqlc.arg(has_observation)::boolean THEN sqlc.arg(observed_revision)::bigint
            ELSE operations.observed_revision
        END,
        observed_configuration_hash = CASE
            WHEN sqlc.arg(has_observation)::boolean
                THEN sqlc.arg(observed_configuration_hash)::bytea
            ELSE operations.observed_configuration_hash
        END,
        observed_at = CASE
            WHEN sqlc.arg(has_observation)::boolean THEN sqlc.arg(observed_at)::timestamptz
            ELSE operations.observed_at
        END,
        updated_at = statement_timestamp()
    FROM candidate
    WHERE operations.id = candidate.id
      AND (
          sqlc.arg(result_status)::text <> 'succeeded'
          OR EXISTS (SELECT 1 FROM updated_mailbox)
      )
    RETURNING operations.id
), updated_attempt AS (
    UPDATE operation_attempts
    SET completed_at = statement_timestamp(),
        result_status = sqlc.arg(result_status)::text,
        error_code = NULLIF(sqlc.arg(error_code)::text, ''),
        observed_status = CASE
            WHEN sqlc.arg(has_observation)::boolean THEN sqlc.arg(observed_status)::text
            ELSE NULL
        END,
        observed_revision = CASE
            WHEN sqlc.arg(has_observation)::boolean THEN sqlc.arg(observed_revision)::bigint
            ELSE NULL
        END,
        observed_configuration_hash = CASE
            WHEN sqlc.arg(has_observation)::boolean
                THEN sqlc.arg(observed_configuration_hash)::bytea
            ELSE NULL
        END,
        observed_at = CASE
            WHEN sqlc.arg(has_observation)::boolean THEN sqlc.arg(observed_at)::timestamptz
            ELSE NULL
        END
    FROM updated_operation
    WHERE operation_attempts.operation_id = updated_operation.id
      AND operation_attempts.attempt_number = sqlc.arg(attempt_number)::integer
      AND operation_attempts.lease_owner_id = sqlc.arg(lease_owner_id)::uuid
      AND operation_attempts.lease_epoch = sqlc.arg(lease_epoch)::bigint
      AND operation_attempts.completed_at IS NULL
    RETURNING operation_attempts.id
)
SELECT
    EXISTS (SELECT 1 FROM updated_operation)
    AND EXISTS (SELECT 1 FROM updated_attempt) AS applied;
