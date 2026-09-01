package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/snowflake-yezi/mail-suite/src/backend/internal/identity"
)

func TestNewIdentityRepositoryRejectsUnsafeConfiguration(t *testing.T) {
	if _, err := NewIdentityRepository(nil, 30*time.Minute, 5*time.Minute); err == nil {
		t.Fatal("缺少连接池时不得创建身份 repository")
	}
}

func TestIdentityPersistenceErrorPreservesCancellationAndHidesDatabaseDetails(t *testing.T) {
	if !errors.Is(identityPersistenceError(context.Canceled), context.Canceled) {
		t.Fatal("身份持久化必须保留调用取消语义")
	}
	databaseError := &pgconn.PgError{Message: "secret identity database detail"}
	mapped := identityPersistenceError(databaseError)
	if !identity.HasErrorCode(mapped, identity.ErrorCodePersistenceUnavailable) {
		t.Fatalf("数据库错误应映射为认证服务不可用，实际为 %v", mapped)
	}
	if errors.Is(mapped, databaseError) || mapped.Error() == databaseError.Error() {
		t.Fatal("认证持久化错误不得泄露数据库详情")
	}
}
