// Package bootstrap 负责按 API 认证模式装配 OIDC、持久化和 HTTP adapter。
package bootstrap

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/data/postgres"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity/httpapi"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity/oidcclient"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity/runtimeconfig"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/probe"
)

// Routes 按显式 disabled 或 oidc 模式返回 API 认证路由，配置或 discovery 失败时阻止启动。
func Routes(ctx context.Context, pool *pgxpool.Pool, _ *slog.Logger) (probe.RouteRegistrar, error) {
	config, err := runtimeconfig.Load()
	if err != nil {
		return nil, err
	}
	if config.Mode == runtimeconfig.ModeDisabled {
		return nil, nil
	}
	provider, err := oidcclient.New(ctx, config)
	if err != nil {
		return nil, err
	}
	hasher, err := identity.NewSecretHasher(config.SecretPepper)
	if err != nil {
		return nil, err
	}
	pkceCipher, err := identity.NewPKCECipher(config.FlowEncryptionKeyID, map[string][]byte{
		config.FlowEncryptionKeyID: config.FlowEncryptionKey,
	})
	if err != nil {
		return nil, err
	}
	repository, err := postgres.NewIdentityRepository(
		pool,
		identity.DefaultSessionIdleTimeout,
		identity.DefaultSessionTouchInterval,
	)
	if err != nil {
		return nil, err
	}
	authService, err := identity.NewService(repository, provider, hasher, pkceCipher, identity.MFAPolicy{
		AllowedACR:  config.AdminAllowedACR,
		RequiredAMR: config.AdminRequiredAMR,
	})
	if err != nil {
		return nil, err
	}
	return httpapi.New(authService, config.TrustedOrigin)
}
