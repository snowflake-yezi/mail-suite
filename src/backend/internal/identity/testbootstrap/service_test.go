package testbootstrap

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

func TestNewProvisionFixtureIsStableAndScopedToActiveMailbox(t *testing.T) {
	manifest := validTestManifest()
	first, err := newProvisionFixture(manifest)
	if err != nil {
		t.Fatalf("生成活动邮箱开通 fixture 失败：%v", err)
	}
	replayed, err := newProvisionFixture(manifest)
	if err != nil {
		t.Fatalf("重复生成活动邮箱开通 fixture 失败：%v", err)
	}
	if first.OperationID != replayed.OperationID || first.OutboxID != replayed.OutboxID ||
		first.IdempotencyKey != replayed.IdempotencyKey || first.RequestHash != replayed.RequestHash ||
		!bytes.Equal(first.Payload, replayed.Payload) {
		t.Fatal("相同活动邮箱必须生成完全稳定的 operation/outbox fixture")
	}
	if bytes.Contains(first.Payload, []byte("@")) {
		t.Fatal("outbox payload 不得包含可还原的邮箱地址")
	}

	changed := manifest
	changed.Mailbox.MailboxID = uuid.New()
	changed.Mailbox.LocalPart = "other-mailbox"
	second, err := newProvisionFixture(changed)
	if err != nil {
		t.Fatalf("生成另一活动邮箱开通 fixture 失败：%v", err)
	}
	if first.OperationID == second.OperationID || first.OutboxID == second.OutboxID ||
		first.IdempotencyKey == second.IdempotencyKey || first.RequestHash == second.RequestHash ||
		bytes.Equal(first.Payload, second.Payload) {
		t.Fatal("不同活动邮箱必须隔离 operation、outbox、幂等键、摘要和 payload")
	}
}
