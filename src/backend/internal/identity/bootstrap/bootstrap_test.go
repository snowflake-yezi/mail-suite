package bootstrap

import (
	"context"
	"testing"
)

func TestRoutesDisabledModeRegistersNoAuthenticationEndpoints(t *testing.T) {
	t.Setenv("MAIL_SUITE_AUTH_MODE", "disabled")
	registrar, err := Routes(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("disabled 模式不应构造 OIDC 或数据库 adapter：%v", err)
	}
	if registrar != nil {
		t.Fatal("disabled 模式不得注册认证路由")
	}
}

func TestRoutesRejectsMissingExplicitMode(t *testing.T) {
	t.Setenv("MAIL_SUITE_AUTH_MODE", "")
	if _, err := Routes(context.Background(), nil, nil); err == nil {
		t.Fatal("API 缺少显式认证模式必须启动失败")
	}
}
