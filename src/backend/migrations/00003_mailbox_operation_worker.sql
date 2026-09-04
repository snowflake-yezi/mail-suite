-- +goose Up
-- +goose StatementBegin

ALTER TABLE mailboxes
    ADD COLUMN observed_revision bigint,
    ADD COLUMN observed_configuration_hash bytea,
    ADD COLUMN observed_at timestamptz;

COMMENT ON COLUMN mailboxes.observed_revision IS '最近一次经 mail-core 实际核验的资源版本；尚无可靠观测时为空';
COMMENT ON COLUMN mailboxes.observed_configuration_hash IS '最近一次实际观测配置的 SHA-256 摘要；不保存可逆配置内容';
COMMENT ON COLUMN mailboxes.observed_at IS 'mail-core 完成最近一次可靠状态核验的时间；尚无观测时为空';

-- 旧 schema 没有 revision/hash 观测证据，不能保留未经核验的 active 结论。
UPDATE mailboxes
SET observed_status = 'unknown',
    updated_at = now()
WHERE observed_status = 'active';

ALTER TABLE mailboxes
    ADD CONSTRAINT mailboxes_observed_revision_nonnegative
        CHECK (observed_revision IS NULL OR observed_revision >= 0),
    ADD CONSTRAINT mailboxes_observed_configuration_hash_sha256
        CHECK (
            observed_configuration_hash IS NULL
            OR octet_length(observed_configuration_hash) = 32
        ),
    ADD CONSTRAINT mailboxes_observation_complete
        CHECK (
            (observed_revision IS NULL AND observed_configuration_hash IS NULL AND observed_at IS NULL)
            OR
            (observed_revision IS NOT NULL AND observed_configuration_hash IS NOT NULL AND observed_at IS NOT NULL)
        ),
    ADD CONSTRAINT mailboxes_active_observation_valid
        CHECK (
            observed_status <> 'active'
            OR (observed_revision = revision AND observed_configuration_hash IS NOT NULL AND observed_at IS NOT NULL)
        );

ALTER TABLE operations
    ADD COLUMN desired_revision bigint NOT NULL DEFAULT 1,
    ADD COLUMN configuration_hash bytea,
    ADD COLUMN attempt_count integer NOT NULL DEFAULT 0,
    ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN lease_owner_id uuid,
    ADD COLUMN lease_epoch bigint NOT NULL DEFAULT 0,
    ADD COLUMN lease_expires_at timestamptz,
    ADD COLUMN last_error_code text,
    ADD COLUMN observed_status text,
    ADD COLUMN observed_revision bigint,
    ADD COLUMN observed_configuration_hash bytea,
    ADD COLUMN observed_at timestamptz;

UPDATE operations
SET configuration_hash = request_hash;

ALTER TABLE operations
    ALTER COLUMN configuration_hash SET NOT NULL;

-- 旧 schema 没有 lease 身份，遗留 running 不具备继续提交回执的资格。
UPDATE operations
SET status = 'unknown',
    next_attempt_at = now(),
    last_error_code = 'WORKER_LEASE_STATE_MIGRATED',
    updated_at = now()
WHERE status = 'running';

-- 旧 schema 无法证明 succeeded 来自真实 inspect，迁移后必须重新对账。
UPDATE operations
SET status = 'unknown',
    next_attempt_at = now(),
    last_error_code = 'OPERATION_OBSERVATION_REQUIRED',
    updated_at = now()
WHERE status = 'succeeded';

UPDATE operations
SET last_error_code = 'OPERATION_OUTCOME_UNKNOWN'
WHERE status = 'unknown' AND last_error_code IS NULL;

ALTER TABLE operations DROP CONSTRAINT operations_status_valid;
ALTER TABLE operations
    ADD CONSTRAINT operations_status_valid
        CHECK (status IN (
            'pending', 'running', 'retry_wait', 'unknown', 'succeeded', 'failed', 'dead', 'superseded'
        )),
    ADD CONSTRAINT operations_desired_revision_positive CHECK (desired_revision > 0),
    ADD CONSTRAINT operations_configuration_hash_sha256 CHECK (octet_length(configuration_hash) = 32),
    ADD CONSTRAINT operations_attempt_count_nonnegative CHECK (attempt_count >= 0),
    ADD CONSTRAINT operations_lease_epoch_nonnegative CHECK (lease_epoch >= 0),
    ADD CONSTRAINT operations_lease_state_complete CHECK (
        (status = 'running' AND lease_owner_id IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR
        (status <> 'running' AND lease_owner_id IS NULL AND lease_expires_at IS NULL)
    ),
    ADD CONSTRAINT operations_error_code_valid CHECK (
        last_error_code IS NULL OR last_error_code ~ '^[A-Z][A-Z0-9_]{2,63}$'
    ),
    ADD CONSTRAINT operations_observed_status_valid CHECK (
        observed_status IS NULL OR observed_status IN ('absent', 'active', 'suspended')
    ),
    ADD CONSTRAINT operations_observed_revision_nonnegative
        CHECK (observed_revision IS NULL OR observed_revision >= 0),
    ADD CONSTRAINT operations_observed_configuration_hash_sha256 CHECK (
        observed_configuration_hash IS NULL OR octet_length(observed_configuration_hash) = 32
    ),
    ADD CONSTRAINT operations_observation_complete CHECK (
        (observed_status IS NULL AND observed_revision IS NULL
            AND observed_configuration_hash IS NULL AND observed_at IS NULL)
        OR
        (observed_status IS NOT NULL AND observed_revision IS NOT NULL
            AND observed_configuration_hash IS NOT NULL AND observed_at IS NOT NULL)
    ),
    ADD CONSTRAINT operations_success_observation_valid CHECK (
        status <> 'succeeded'
        OR (
            observed_status = 'active'
            AND observed_revision = desired_revision
            AND observed_configuration_hash = configuration_hash
            AND observed_at IS NOT NULL
        )
    );

COMMENT ON COLUMN operations.desired_revision IS 'operation 创建时固定的邮箱期望版本，后续资源变更必须创建新 operation';
COMMENT ON COLUMN operations.status IS 'operation 状态：pending、running、retry_wait、unknown、succeeded、failed、dead 或 superseded';
COMMENT ON COLUMN operations.configuration_hash IS '本次 operation 期望配置的 SHA-256 摘要，与调用方幂等键相互独立';
COMMENT ON COLUMN operations.attempt_count IS '已经原子创建的执行 attempt 数量，从 0 开始且只增不减';
COMMENT ON COLUMN operations.next_attempt_at IS 'pending、retry_wait 或 unknown 状态下一次允许被 worker 领取的时间';
COMMENT ON COLUMN operations.lease_owner_id IS '当前 running attempt 的 worker 进程标识；非 running 状态必须为空';
COMMENT ON COLUMN operations.lease_epoch IS '每次领取单调递增的 fencing token，旧 epoch 回执不得提交';
COMMENT ON COLUMN operations.lease_expires_at IS '当前 worker 租约的绝对失效时间；非 running 状态必须为空';
COMMENT ON COLUMN operations.last_error_code IS '最近一次执行结果的稳定脱敏错误码，不保存地址、凭据或底层响应';
COMMENT ON COLUMN operations.observed_status IS '最近一次 inspect 返回的 mail-core 邮箱状态；未取得证据时为空';
COMMENT ON COLUMN operations.observed_revision IS '最近一次 inspect 返回的资源版本；未取得证据时为空';
COMMENT ON COLUMN operations.observed_configuration_hash IS '最近一次 inspect 返回的配置 SHA-256 摘要；未取得证据时为空';
COMMENT ON COLUMN operations.observed_at IS '最近一次 inspect 完成时间；未取得实际证据时为空';

DROP INDEX operations_tenant_status_created_idx;
CREATE INDEX operations_tenant_status_created_idx
    ON operations (tenant_id, status, next_attempt_at, created_at, id);
CREATE INDEX operations_worker_claim_idx
    ON operations (status, next_attempt_at, lease_expires_at, created_at, id)
    WHERE status IN ('pending', 'running', 'retry_wait', 'unknown');

CREATE TABLE operation_attempts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    operation_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    attempt_number integer NOT NULL,
    lease_owner_id uuid NOT NULL,
    lease_epoch bigint NOT NULL,
    prior_status text NOT NULL,
    desired_revision bigint NOT NULL,
    configuration_hash bytea NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    result_status text,
    error_code text,
    observed_status text,
    observed_revision bigint,
    observed_configuration_hash bytea,
    observed_at timestamptz,
    CONSTRAINT operation_attempts_operation_tenant_fk
        FOREIGN KEY (operation_id, tenant_id) REFERENCES operations (id, tenant_id),
    CONSTRAINT operation_attempts_attempt_number_positive CHECK (attempt_number > 0),
    CONSTRAINT operation_attempts_lease_epoch_positive CHECK (lease_epoch > 0),
    CONSTRAINT operation_attempts_prior_status_valid
        CHECK (prior_status IN ('pending', 'running', 'retry_wait', 'unknown')),
    CONSTRAINT operation_attempts_desired_revision_positive CHECK (desired_revision > 0),
    CONSTRAINT operation_attempts_configuration_hash_sha256
        CHECK (octet_length(configuration_hash) = 32),
    CONSTRAINT operation_attempts_result_status_valid CHECK (
        result_status IS NULL
        OR result_status IN ('retry_wait', 'unknown', 'succeeded', 'failed', 'dead', 'superseded')
    ),
    CONSTRAINT operation_attempts_completion_complete CHECK (
        (completed_at IS NULL AND result_status IS NULL AND error_code IS NULL)
        OR
        (completed_at IS NOT NULL AND result_status IS NOT NULL)
    ),
    CONSTRAINT operation_attempts_completion_time_valid
        CHECK (completed_at IS NULL OR completed_at >= started_at),
    CONSTRAINT operation_attempts_error_code_valid CHECK (
        error_code IS NULL OR error_code ~ '^[A-Z][A-Z0-9_]{2,63}$'
    ),
    CONSTRAINT operation_attempts_success_error_empty CHECK (
        result_status IS DISTINCT FROM 'succeeded' OR error_code IS NULL
    ),
    CONSTRAINT operation_attempts_observed_status_valid CHECK (
        observed_status IS NULL OR observed_status IN ('absent', 'active', 'suspended')
    ),
    CONSTRAINT operation_attempts_observed_revision_nonnegative
        CHECK (observed_revision IS NULL OR observed_revision >= 0),
    CONSTRAINT operation_attempts_observed_configuration_hash_sha256 CHECK (
        observed_configuration_hash IS NULL OR octet_length(observed_configuration_hash) = 32
    ),
    CONSTRAINT operation_attempts_observation_complete CHECK (
        (observed_status IS NULL AND observed_revision IS NULL
            AND observed_configuration_hash IS NULL AND observed_at IS NULL)
        OR
        (observed_status IS NOT NULL AND observed_revision IS NOT NULL
            AND observed_configuration_hash IS NOT NULL AND observed_at IS NOT NULL)
    ),
    CONSTRAINT operation_attempts_operation_number_unique UNIQUE (operation_id, attempt_number),
    CONSTRAINT operation_attempts_operation_epoch_unique UNIQUE (operation_id, lease_epoch)
);

COMMENT ON TABLE operation_attempts IS 'operation 每次真实外部调用的追加式审计记录，领取身份创建后不得复用';
COMMENT ON COLUMN operation_attempts.id IS '单次执行 attempt 的稳定标识，由数据库生成 UUID';
COMMENT ON COLUMN operation_attempts.operation_id IS 'attempt 所属的不可变 operation 标识';
COMMENT ON COLUMN operation_attempts.tenant_id IS 'attempt 所属租户标识，必须与 operation 保持一致';
COMMENT ON COLUMN operation_attempts.attempt_number IS '同一 operation 内从 1 开始单调递增的执行序号';
COMMENT ON COLUMN operation_attempts.lease_owner_id IS '领取本次 attempt 的 worker 进程标识，创建后不可改变';
COMMENT ON COLUMN operation_attempts.lease_epoch IS '本次 attempt 持有的 fencing token，创建后不可改变';
COMMENT ON COLUMN operation_attempts.prior_status IS '领取前 operation 状态，running 或 unknown 接管时用于优先 inspect';
COMMENT ON COLUMN operation_attempts.desired_revision IS '本次 attempt 固定处理的邮箱期望版本';
COMMENT ON COLUMN operation_attempts.configuration_hash IS '本次 attempt 固定处理的期望配置 SHA-256 摘要';
COMMENT ON COLUMN operation_attempts.started_at IS '数据库原子领取并创建本次 attempt 的时间';
COMMENT ON COLUMN operation_attempts.completed_at IS '当前 fencing token 成功提交结果的时间；执行中为空';
COMMENT ON COLUMN operation_attempts.result_status IS 'attempt 的收敛结果：retry_wait、unknown、succeeded、failed、dead 或 superseded';
COMMENT ON COLUMN operation_attempts.error_code IS '本次失败或未知结果的稳定脱敏错误码；成功时为空';
COMMENT ON COLUMN operation_attempts.observed_status IS '本次 inspect 实际返回的邮箱状态；未取得证据时为空';
COMMENT ON COLUMN operation_attempts.observed_revision IS '本次 inspect 实际返回的资源版本；未取得证据时为空';
COMMENT ON COLUMN operation_attempts.observed_configuration_hash IS '本次 inspect 实际返回的配置 SHA-256 摘要；未取得证据时为空';
COMMENT ON COLUMN operation_attempts.observed_at IS '本次 inspect 完成时间；未取得证据时为空';

CREATE INDEX operation_attempts_tenant_operation_idx
    ON operation_attempts (tenant_id, operation_id, attempt_number);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE operation_attempts;
DROP INDEX operations_worker_claim_idx;
DROP INDEX operations_tenant_status_created_idx;

ALTER TABLE operations
    DROP CONSTRAINT operations_status_valid,
    DROP CONSTRAINT operations_desired_revision_positive,
    DROP CONSTRAINT operations_configuration_hash_sha256,
    DROP CONSTRAINT operations_attempt_count_nonnegative,
    DROP CONSTRAINT operations_lease_epoch_nonnegative,
    DROP CONSTRAINT operations_lease_state_complete,
    DROP CONSTRAINT operations_error_code_valid,
    DROP CONSTRAINT operations_observed_status_valid,
    DROP CONSTRAINT operations_observed_revision_nonnegative,
    DROP CONSTRAINT operations_observed_configuration_hash_sha256,
    DROP CONSTRAINT operations_observation_complete,
    DROP CONSTRAINT operations_success_observation_valid;

UPDATE operations SET status = 'pending' WHERE status = 'retry_wait';
UPDATE operations SET status = 'failed' WHERE status IN ('dead', 'superseded');
UPDATE operations SET status = 'unknown' WHERE status = 'running';

ALTER TABLE operations
    DROP COLUMN desired_revision,
    DROP COLUMN configuration_hash,
    DROP COLUMN attempt_count,
    DROP COLUMN next_attempt_at,
    DROP COLUMN lease_owner_id,
    DROP COLUMN lease_epoch,
    DROP COLUMN lease_expires_at,
    DROP COLUMN last_error_code,
    DROP COLUMN observed_status,
    DROP COLUMN observed_revision,
    DROP COLUMN observed_configuration_hash,
    DROP COLUMN observed_at,
    ADD CONSTRAINT operations_status_valid
        CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'unknown'));

COMMENT ON COLUMN operations.status IS 'operation 状态：pending、running、succeeded、failed 或 unknown';

CREATE INDEX operations_tenant_status_created_idx
    ON operations (tenant_id, status, created_at, id);

ALTER TABLE mailboxes
    DROP CONSTRAINT mailboxes_observed_revision_nonnegative,
    DROP CONSTRAINT mailboxes_observed_configuration_hash_sha256,
    DROP CONSTRAINT mailboxes_observation_complete,
    DROP CONSTRAINT mailboxes_active_observation_valid,
    DROP COLUMN observed_revision,
    DROP COLUMN observed_configuration_hash,
    DROP COLUMN observed_at;

-- +goose StatementEnd
