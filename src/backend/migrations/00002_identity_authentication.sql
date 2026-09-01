-- +goose Up
-- +goose StatementBegin

ALTER TABLE mailboxes
    ADD CONSTRAINT mailboxes_id_tenant_unique UNIQUE (id, tenant_id);

CREATE TABLE identity_principals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants (id),
    oidc_issuer text NOT NULL,
    oidc_subject text NOT NULL,
    account_type text NOT NULL,
    mailbox_id uuid,
    status text NOT NULL DEFAULT 'active',
    display_name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT identity_principals_mailbox_tenant_fk
        FOREIGN KEY (mailbox_id, tenant_id) REFERENCES mailboxes (id, tenant_id),
    CONSTRAINT identity_principals_issuer_valid CHECK (
        oidc_issuer = btrim(oidc_issuer)
        AND char_length(oidc_issuer) BETWEEN 1 AND 2048
        AND oidc_issuer !~ '[[:cntrl:]]'
    ),
    CONSTRAINT identity_principals_subject_valid CHECK (
        char_length(oidc_subject) BETWEEN 1 AND 255
        AND oidc_subject !~ '[[:cntrl:]]'
    ),
    CONSTRAINT identity_principals_account_type_valid CHECK (
        account_type IN ('mailbox', 'administrator')
    ),
    CONSTRAINT identity_principals_mailbox_binding_valid CHECK (
        (account_type = 'mailbox' AND mailbox_id IS NOT NULL)
        OR (account_type = 'administrator' AND mailbox_id IS NULL)
    ),
    CONSTRAINT identity_principals_status_valid CHECK (status IN ('active', 'suspended')),
    CONSTRAINT identity_principals_display_name_valid CHECK (
        display_name = btrim(display_name)
        AND char_length(display_name) BETWEEN 1 AND 128
    ),
    CONSTRAINT identity_principals_external_identity_unique UNIQUE (oidc_issuer, oidc_subject),
    CONSTRAINT identity_principals_id_external_identity_unique UNIQUE (id, oidc_issuer, oidc_subject),
    CONSTRAINT identity_principals_id_tenant_unique UNIQUE (id, tenant_id)
);

COMMENT ON TABLE identity_principals IS '外部 OIDC 身份到本地租户、账号类型和可选邮箱的唯一授权映射';
COMMENT ON COLUMN identity_principals.id IS '本地认证主体稳定标识，不等同于 OIDC subject 或邮箱标识';
COMMENT ON COLUMN identity_principals.tenant_id IS '主体所属租户标识，决定所有后续授权的一级边界';
COMMENT ON COLUMN identity_principals.oidc_issuer IS '按 OIDC exact-match 语义保存的签发方标识，不做大小写改写';
COMMENT ON COLUMN identity_principals.oidc_subject IS 'OIDC Provider 签发的不透明 subject，与 issuer 共同作为外部稳定键';
COMMENT ON COLUMN identity_principals.account_type IS '产品账号类型：mailbox 邮箱账号或 administrator 管理账号';
COMMENT ON COLUMN identity_principals.mailbox_id IS '邮箱账号唯一绑定的同租户邮箱；管理账号必须为空';
COMMENT ON COLUMN identity_principals.status IS '本地主体状态：active 可登录，suspended 拒绝并撤销既有会话';
COMMENT ON COLUMN identity_principals.display_name IS '用于界面展示的名称快照，不参与身份匹配或权限判断';
COMMENT ON COLUMN identity_principals.created_at IS '本地主体首次创建时间，使用数据库时区时间';
COMMENT ON COLUMN identity_principals.updated_at IS '主体映射、状态或显示属性最后更新时间';

CREATE TABLE principal_permissions (
    principal_id uuid NOT NULL,
    permission_name text NOT NULL,
    granted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, permission_name),
    CONSTRAINT principal_permissions_name_valid CHECK (
        char_length(permission_name) BETWEEN 3 AND 128
        AND permission_name ~ '^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)+$'
    )
);

COMMENT ON TABLE principal_permissions IS '本地认证主体拥有的稳定产品权限，OIDC claim 不得直接写入';
COMMENT ON COLUMN principal_permissions.principal_id IS '获得权限的本地认证主体标识';
COMMENT ON COLUMN principal_permissions.permission_name IS '由产品契约定义的稳定权限名称，如 portal.admin.access';
COMMENT ON COLUMN principal_permissions.granted_at IS '权限在控制面被授予的时间';

CREATE TABLE auth_flows (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    state_digest bytea NOT NULL,
    browser_cookie_digest bytea NOT NULL,
    nonce_digest bytea NOT NULL,
    pkce_verifier_ciphertext bytea NOT NULL,
    encryption_key_id text NOT NULL,
    return_to text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    CONSTRAINT auth_flows_state_digest_sha256 CHECK (octet_length(state_digest) = 32),
    CONSTRAINT auth_flows_browser_cookie_digest_sha256 CHECK (octet_length(browser_cookie_digest) = 32),
    CONSTRAINT auth_flows_nonce_digest_sha256 CHECK (octet_length(nonce_digest) = 32),
    CONSTRAINT auth_flows_verifier_ciphertext_valid CHECK (
        octet_length(pkce_verifier_ciphertext) BETWEEN 32 AND 4096
    ),
    CONSTRAINT auth_flows_encryption_key_id_valid CHECK (
        encryption_key_id = btrim(encryption_key_id)
        AND char_length(encryption_key_id) BETWEEN 1 AND 128
        AND encryption_key_id !~ '[[:cntrl:]]'
    ),
    CONSTRAINT auth_flows_return_to_valid CHECK (
        octet_length(return_to) BETWEEN 1 AND 2048
        AND left(return_to, 1) = '/'
        AND left(return_to, 2) <> '//'
        AND position(E'\\' IN return_to) = 0
        AND return_to !~ '[[:cntrl:]]'
    ),
    CONSTRAINT auth_flows_expiry_valid CHECK (
        expires_at > created_at AND expires_at <= created_at + interval '10 minutes'
    ),
    CONSTRAINT auth_flows_consumed_at_valid CHECK (
        consumed_at IS NULL OR consumed_at >= created_at
    ),
    CONSTRAINT auth_flows_state_digest_unique UNIQUE (state_digest)
);

COMMENT ON TABLE auth_flows IS 'OIDC 登录发起到 callback 之间最长十分钟且只能消费一次的临时流程';
COMMENT ON COLUMN auth_flows.id IS '登录流程内部稳定标识，不暴露给浏览器或 OIDC Provider';
COMMENT ON COLUMN auth_flows.state_digest IS '带服务端 pepper 的 state 摘要，不保存 state 明文';
COMMENT ON COLUMN auth_flows.browser_cookie_digest IS '临时关联 Cookie 的带 pepper 摘要，用于绑定发起登录的浏览器';
COMMENT ON COLUMN auth_flows.nonce_digest IS '带服务端 pepper 的 OIDC nonce 摘要，用于 callback 校验';
COMMENT ON COLUMN auth_flows.pkce_verifier_ciphertext IS '经批准密钥加密的 PKCE verifier 密文，不得记录明文';
COMMENT ON COLUMN auth_flows.encryption_key_id IS '解密 PKCE verifier 所需的非秘密密钥版本标识';
COMMENT ON COLUMN auth_flows.return_to IS '经过规范化和白名单校验的站内绝对返回路径';
COMMENT ON COLUMN auth_flows.created_at IS '登录流程创建时间';
COMMENT ON COLUMN auth_flows.expires_at IS '登录流程最晚消费时间，不得超过创建后十分钟';
COMMENT ON COLUMN auth_flows.consumed_at IS 'callback 原子消费时间；非空后不得再次建立会话';

CREATE INDEX auth_flows_expires_at_idx ON auth_flows (expires_at, id)
    WHERE consumed_at IS NULL;

CREATE TABLE user_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_digest bytea NOT NULL,
    principal_id uuid NOT NULL REFERENCES identity_principals (id),
    oidc_issuer text NOT NULL,
    oidc_subject text NOT NULL,
    oidc_session_id text,
    csrf_digest bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    idle_expires_at timestamptz NOT NULL,
    absolute_expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    revocation_reason text,
    CONSTRAINT user_sessions_principal_external_identity_fk
        FOREIGN KEY (principal_id, oidc_issuer, oidc_subject)
        REFERENCES identity_principals (id, oidc_issuer, oidc_subject),
    CONSTRAINT user_sessions_session_digest_sha256 CHECK (octet_length(session_digest) = 32),
    CONSTRAINT user_sessions_csrf_digest_sha256 CHECK (octet_length(csrf_digest) = 32),
    CONSTRAINT user_sessions_oidc_issuer_valid CHECK (
        oidc_issuer = btrim(oidc_issuer)
        AND char_length(oidc_issuer) BETWEEN 1 AND 2048
        AND oidc_issuer !~ '[[:cntrl:]]'
    ),
    CONSTRAINT user_sessions_oidc_subject_valid CHECK (
        char_length(oidc_subject) BETWEEN 1 AND 255
        AND oidc_subject !~ '[[:cntrl:]]'
    ),
    CONSTRAINT user_sessions_oidc_session_id_valid CHECK (
        oidc_session_id IS NULL
        OR (char_length(oidc_session_id) BETWEEN 1 AND 255 AND oidc_session_id !~ '[[:cntrl:]]')
    ),
    CONSTRAINT user_sessions_lifetime_valid CHECK (
        last_seen_at >= created_at
        AND idle_expires_at > created_at
        AND idle_expires_at <= absolute_expires_at
        AND absolute_expires_at > created_at
        AND absolute_expires_at <= created_at + interval '8 hours'
    ),
    CONSTRAINT user_sessions_revocation_valid CHECK (
        (revoked_at IS NULL AND revocation_reason IS NULL)
        OR (
            revoked_at IS NOT NULL
            AND revoked_at >= created_at
            AND revocation_reason = btrim(revocation_reason)
            AND char_length(revocation_reason) BETWEEN 1 AND 128
        )
    ),
    CONSTRAINT user_sessions_session_digest_unique UNIQUE (session_digest)
);

COMMENT ON TABLE user_sessions IS '跨 API 实例共享且可撤销的浏览器不透明会话事实源';
COMMENT ON COLUMN user_sessions.id IS '服务端会话内部稳定标识，不作为浏览器 Cookie 值';
COMMENT ON COLUMN user_sessions.session_digest IS '带服务端 pepper 的会话 Cookie 摘要，不保存 Cookie 明文';
COMMENT ON COLUMN user_sessions.principal_id IS '会话绑定的本地认证主体标识，创建后不可切换账号';
COMMENT ON COLUMN user_sessions.oidc_issuer IS '创建会话时已验证的 OIDC issuer，用于 backchannel 撤销';
COMMENT ON COLUMN user_sessions.oidc_subject IS '创建会话时已验证的 OIDC subject，用于主体级 backchannel 撤销';
COMMENT ON COLUMN user_sessions.oidc_session_id IS '可选 OIDC sid，用于精确撤销同一 Provider 会话';
COMMENT ON COLUMN user_sessions.csrf_digest IS '与当前会话绑定的 CSRF nonce 摘要，不保存 nonce 明文';
COMMENT ON COLUMN user_sessions.created_at IS '本地会话创建时间，也是绝对生命周期起点';
COMMENT ON COLUMN user_sessions.last_seen_at IS '最近一次被限频记录的有效鉴权访问时间';
COMMENT ON COLUMN user_sessions.idle_expires_at IS '会话空闲过期时间，默认不超过最近访问后三十分钟';
COMMENT ON COLUMN user_sessions.absolute_expires_at IS '会话绝对过期时间，首期不得超过创建后八小时';
COMMENT ON COLUMN user_sessions.revoked_at IS '本地退出、停用或 backchannel logout 的撤销时间';
COMMENT ON COLUMN user_sessions.revocation_reason IS '不包含凭据或外部错误详情的稳定撤销原因';

CREATE INDEX user_sessions_principal_active_idx ON user_sessions (principal_id, absolute_expires_at, id)
    WHERE revoked_at IS NULL;
CREATE INDEX user_sessions_oidc_sid_active_idx
    ON user_sessions (oidc_issuer, oidc_session_id, id)
    WHERE revoked_at IS NULL AND oidc_session_id IS NOT NULL;
CREATE INDEX user_sessions_oidc_subject_active_idx
    ON user_sessions (oidc_issuer, oidc_subject, id)
    WHERE revoked_at IS NULL;

CREATE TABLE authentication_audit_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id uuid REFERENCES identity_principals (id),
    action text NOT NULL,
    result text NOT NULL,
    failure_category text,
    source_digest bytea NOT NULL,
    request_id text NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT authentication_audit_events_action_valid CHECK (
        action IN (
            'login',
            'logout',
            'backchannel_logout',
            'session_revoked',
            'principal_disabled',
            'cross_portal_denied'
        )
    ),
    CONSTRAINT authentication_audit_events_result_valid CHECK (result IN ('succeeded', 'failed')),
    CONSTRAINT authentication_audit_events_failure_category_valid CHECK (
        (result = 'succeeded' AND failure_category IS NULL)
        OR (
            result = 'failed'
            AND failure_category = btrim(failure_category)
            AND char_length(failure_category) BETWEEN 1 AND 128
            AND failure_category ~ '^[a-z][a-z0-9_]*$'
        )
    ),
    CONSTRAINT authentication_audit_events_source_digest_sha256 CHECK (octet_length(source_digest) = 32),
    CONSTRAINT authentication_audit_events_request_id_valid CHECK (
        char_length(request_id) BETWEEN 1 AND 64
        AND request_id ~ '^[A-Za-z0-9._-]+$'
    )
);

COMMENT ON TABLE authentication_audit_events IS '不含 token、Cookie、完整 claim 或密码的认证安全审计事件';
COMMENT ON COLUMN authentication_audit_events.id IS '认证审计事件稳定标识';
COMMENT ON COLUMN authentication_audit_events.principal_id IS '可选本地主体标识；身份映射失败时为空以避免账号枚举';
COMMENT ON COLUMN authentication_audit_events.action IS '认证动作：登录、退出、撤销、停用或跨端拒绝';
COMMENT ON COLUMN authentication_audit_events.result IS '动作结果：succeeded 成功或 failed 失败';
COMMENT ON COLUMN authentication_audit_events.failure_category IS '失败时的稳定脱敏类别，成功事件必须为空';
COMMENT ON COLUMN authentication_audit_events.source_digest IS '请求来源信息的不可逆摘要，不保存完整地址或 User-Agent';
COMMENT ON COLUMN authentication_audit_events.request_id IS '关联结构化日志和调用链的受控请求标识';
COMMENT ON COLUMN authentication_audit_events.occurred_at IS '认证动作在服务端确认的时间';

CREATE INDEX authentication_audit_events_principal_time_idx
    ON authentication_audit_events (principal_id, occurred_at DESC, id);
CREATE INDEX authentication_audit_events_request_id_idx
    ON authentication_audit_events (request_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE authentication_audit_events;
DROP TABLE user_sessions;
DROP TABLE auth_flows;
DROP TABLE principal_permissions;
DROP TABLE identity_principals;
ALTER TABLE mailboxes DROP CONSTRAINT mailboxes_id_tenant_unique;

-- +goose StatementEnd
