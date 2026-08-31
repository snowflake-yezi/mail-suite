package database

import (
	"strings"
	"testing"
)

func TestValidateRejectsInvalidURLWithoutEchoingSecret(t *testing.T) {
	const invalidURL = "postgres://name:very-secret@%"

	err := Validate(invalidURL)
	if err == nil {
		t.Fatal("期望无效连接串被拒绝")
	}
	if strings.Contains(err.Error(), "very-secret") {
		t.Fatal("数据库配置错误不得回显秘密")
	}
}
