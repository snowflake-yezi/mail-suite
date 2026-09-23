// Package bootstrap 负责按 API 认证模式装配 OIDC、持久化和 HTTP adapter。
package bootstrap

import (
	"context"
	"log/slog"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	authorizationhttp "github.com/snowflake-yezi/mail-suite/src/backend/internal/authorization/httpapi"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/data/postgres"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity/httpapi"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity/oidcclient"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity/runtimeconfig"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/platform/probe"
)

// apiRoutes 共享一次身份服务装配；后续业务模块在此注册受 Protect 保护的路由。
type apiRoutes struct {
	authentication *httpapi.Handler
	authorization  *authorizationhttp.Boundary
}

// RegisterRoutes 只注册当前已批准的认证路由，不提前暴露业务端点。
func (routes *apiRoutes) RegisterRoutes(router *gin.Engine) {
	routes.authentication.RegisterRoutes(router)
}

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
	authentication, err := httpapi.New(authService, config.TrustedOrigin)
	if err != nil {
		return nil, err
	}
	authorization, err := authorizationhttp.New(authService, config.TrustedOrigin)
	if err != nil {
		return nil, err
	}
	return &apiRoutes{authentication: authentication, authorization: authorization}, nil
}
