package mailcore

import (
	"errors"
	"testing"
)

func TestClassifyErrorReturnsOnlyStableAdapterDetails(t *testing.T) {
	retryable := NewError(ErrorClassRetryable, "MAIL_CORE_TEMPORARY")
	class, code := ClassifyError(retryable)
	if class != ErrorClassRetryable || code != "MAIL_CORE_TEMPORARY" {
		t.Fatalf("稳定 adapter 错误分类不正确：class=%q code=%q", class, code)
	}

	class, code = ClassifyError(errors.New("endpoint secret response"))
	if class != ErrorClassUnknown || code != ErrorCodeUnknown {
		t.Fatalf("未知错误必须脱敏归一化：class=%q code=%q", class, code)
	}
}

func TestNewErrorRejectsUnsafeCodeAndClass(t *testing.T) {
	adapterError := NewError(ErrorClass("other"), "mailbox alice@example.test failed")
	if adapterError.Class != ErrorClassUnknown || adapterError.Code != ErrorCodeUnknown {
		t.Fatalf("无效错误元数据必须归一化：%+v", adapterError)
	}
}
