package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func openBatchTestStore(t *testing.T) (*Store, domain.Mailbox) {
	t.Helper()
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { state.Close() })
	mailbox, _, err := state.UpsertMailboxFromRemote("account", domain.RemoteMailbox{Email: "batch@imap.test", IsActive: true}, "")
	if err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}
	return state, mailbox
}

// 同一批里出现两封远端标识相同的信，只能落一条。
// 这一条正是把逐封查库换成内存索引后最容易丢的性质：索引必须随新写入同步更新，
// 否则第二封查不到第一封，会当成新邮件再存一遍。
func TestApplyMailboxSyncBatchCollapsesDuplicateWithinOneBatch(t *testing.T) {
	state, mailbox := openBatchTestStore(t)
	now := time.Now()
	created, err := state.ApplyMailboxSyncBatch([]MailboxSyncUpdate{{
		MailboxID: mailbox.ID,
		Messages: []MailboxSyncMessage{
			{RemoteID: "imap:7", Source: "imap", Subject: "第一封", Body: "111111", ReceivedAt: now},
			{RemoteID: "imap:7", Source: "imap", Subject: "第一封", Body: "111111", ReceivedAt: now},
		},
	}})
	if err != nil {
		t.Fatalf("批量写入失败：%v", err)
	}
	if created != 1 {
		t.Fatalf("批内重复邮件没有被合并：created=%d，期望 1", created)
	}
	if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 1 {
		t.Fatalf("批内重复邮件落成了 %d 条，期望 1 条", len(messages))
	}
	stored, _ := state.FindMailboxByID(mailbox.ID)
	if stored.ReceiveCount != 1 {
		t.Fatalf("收件数被重复计算：%d", stored.ReceiveCount)
	}
}

// 别名（remote_ids 数组里的历史标识）仍然要能和后到的信对上。
// IMAP 里同一封信在不同文件夹下的 UID 不一样，早先存下的那一条的 remote_id
// 可能与这次拿到的不相等，只有别名能对上；换成内存索引之后这条必须保持。
func TestApplyMailboxSyncBatchMatchesMessageByRemoteAlias(t *testing.T) {
	state, mailbox := openBatchTestStore(t)
	now := time.Now()
	if _, err := state.ApplyMailboxSyncBatch([]MailboxSyncUpdate{{
		MailboxID: mailbox.ID,
		Messages: []MailboxSyncMessage{{
			RemoteID: "inbox:42", RemoteIDs: []string{"inbox:42", "all:9001"},
			Source: "imap", Subject: "验证码", Body: "旧正文", ReceivedAt: now,
		}},
	}}); err != nil {
		t.Fatalf("保存初始邮件失败：%v", err)
	}
	// 第二次拿到的是同一个 UIDValidity 下的另一个文件夹编号，只对得上别名。
	created, err := state.ApplyMailboxSyncBatch([]MailboxSyncUpdate{{
		MailboxID: mailbox.ID,
		Messages: []MailboxSyncMessage{{
			RemoteID: "all:9001", Source: "imap", Subject: "验证码", Body: "完整正文", ReceivedAt: now,
		}},
	}})
	if err != nil {
		t.Fatalf("按别名去重失败：%v", err)
	}
	if created != 0 {
		t.Fatalf("别名命中的邮件被当成新邮件：created=%d，期望 0", created)
	}
	messages := state.MessagesForMailbox(mailbox.ID)
	if len(messages) != 1 {
		t.Fatalf("别名去重后邮件数为 %d，期望 1", len(messages))
	}
	// 去重命中且没有其它字段变化时，不覆盖已经存下的正文：这条规则本来就存在，
	// 换成内存索引之后必须保持，不能因为「又见到一封信」就把旧正文冲掉。
	if messages[0].Body != "旧正文" {
		t.Fatalf("别名命中时覆盖了已有正文：%q", messages[0].Body)
	}
	if !containsString(messages[0].RemoteIDs, "all:9001") {
		t.Fatalf("别名没有并进这条邮件的 remote_ids：%v", messages[0].RemoteIDs)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// canonical_id 命中的那条也仍然要合并。
func TestApplyMailboxSyncBatchMatchesMessageByCanonicalID(t *testing.T) {
	state, mailbox := openBatchTestStore(t)
	now := time.Now()
	if _, err := state.ApplyMailboxSyncBatch([]MailboxSyncUpdate{{
		MailboxID: mailbox.ID,
		Messages: []MailboxSyncMessage{{
			RemoteID: "imap:1", CanonicalID: "mail-abc", Source: "imap",
			Subject: "验证码", Body: "旧正文", ReceivedAt: now,
		}},
	}}); err != nil {
		t.Fatalf("保存初始邮件失败：%v", err)
	}
	created, err := state.ApplyMailboxSyncBatch([]MailboxSyncUpdate{{
		MailboxID: mailbox.ID,
		Messages: []MailboxSyncMessage{{
			RemoteID: "imap:2", CanonicalID: "mail-abc", Source: "imap",
			Subject: "验证码", Body: "完整正文", ReceivedAt: now,
		}},
	}})
	if err != nil {
		t.Fatalf("按 canonical_id 去重失败：%v", err)
	}
	if created != 0 {
		t.Fatalf("canonical_id 命中的邮件被当成新邮件：created=%d，期望 0", created)
	}
	if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 1 || messages[0].Body != "完整正文" {
		t.Fatalf("canonical_id 去重结果不正确：%+v", messages)
	}
}

// 同一批邮件重放一次不能长出第二份，游标之外的部分也必须保持幂等。
func TestApplyMailboxSyncBatchReplayIsIdempotent(t *testing.T) {
	state, mailbox := openBatchTestStore(t)
	now := time.Now()
	batch := make([]MailboxSyncMessage, 0, 50)
	for index := 0; index < 50; index++ {
		batch = append(batch, MailboxSyncMessage{
			RemoteID: fmt.Sprintf("imap:%d", index), Source: "imap",
			Subject: fmt.Sprintf("第 %d 封", index), Body: "123456", ReceivedAt: now.Add(time.Duration(index) * time.Minute),
		})
	}
	created, err := state.ApplyMailboxSyncBatch([]MailboxSyncUpdate{{MailboxID: mailbox.ID, Messages: batch}})
	if err != nil || created != 50 {
		t.Fatalf("首轮写入失败：created=%d err=%v", created, err)
	}
	created, err = state.ApplyMailboxSyncBatch([]MailboxSyncUpdate{{MailboxID: mailbox.ID, Messages: batch}})
	if err != nil {
		t.Fatalf("重放批次失败：%v", err)
	}
	if created != 0 {
		t.Fatalf("重放批次又新增了 %d 封，期望 0", created)
	}
	if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 50 {
		t.Fatalf("重放后邮件数为 %d，期望 50", len(messages))
	}
}

// 批量变更载荷只带列表要用的字段：正文不能进 change_log，也不会推给浏览器。
// 一次几千封的回填曾经因此写出一条 42 MB 的记录。
func TestApplyMailboxSyncBatchPayloadOmitsBodiesAndIsBounded(t *testing.T) {
	state, mailbox := openBatchTestStore(t)
	now := time.Now()
	batch := make([]MailboxSyncMessage, 0, batchPayloadMessageLimit+40)
	for index := 0; index < batchPayloadMessageLimit+40; index++ {
		batch = append(batch, MailboxSyncMessage{
			RemoteID: fmt.Sprintf("imap:%d", index), Source: "imap",
			Subject: fmt.Sprintf("第 %d 封", index), Body: "验证码 123456，请勿泄露",
			HTMLBody: "<p>验证码 123456</p>", ReceivedAt: now.Add(time.Duration(index) * time.Second),
		})
	}
	if _, err := state.ApplyMailboxSyncBatch([]MailboxSyncUpdate{{MailboxID: mailbox.ID, Messages: batch}}); err != nil {
		t.Fatalf("批量写入失败：%v", err)
	}
	var raw []byte
	if err := state.db.QueryRow(`SELECT payload_json FROM change_log WHERE event_type = 'mailbox.batch-updated' ORDER BY sequence DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatalf("读取批量变更载荷失败：%v", err)
	}
	if len(raw) > 400*1024 {
		t.Fatalf("批量变更载荷为 %d 字节，正文显然还在里面", len(raw))
	}
	if bytes.Contains(raw, []byte("验证码 123456")) {
		t.Fatal("批量变更载荷里仍然带着邮件正文")
	}
	var payload struct {
		CreatedMessageCount int              `json:"created_message_count"`
		Messages            []domain.Message `json:"messages"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("解析批量变更载荷失败：%v", err)
	}
	if payload.CreatedMessageCount != len(batch) {
		t.Fatalf("载荷里的新增计数被截断了：%d，期望 %d", payload.CreatedMessageCount, len(batch))
	}
	if len(payload.Messages) != batchPayloadMessageLimit {
		t.Fatalf("载荷里的邮件条数为 %d，期望上限 %d", len(payload.Messages), batchPayloadMessageLimit)
	}
	// 裁剪之后列表仍要能显示：主题和时间必须留着。
	if payload.Messages[0].Subject == "" || payload.Messages[0].ReceivedAt.IsZero() {
		t.Fatalf("裁剪后的邮件缺少列表要用的字段：%+v", payload.Messages[0])
	}
}

// 真实规模下的对照：回填一个已有几百封邮件的邮箱。旧实现每封信都要把邮箱里
// 已有的全部邮件扫一遍，这里只用一条断言防止它退回去（耗时上限放得很宽，
// 因为这条测试要抓的是 O(n²)，不是几百毫秒的抖动）。
func TestApplyMailboxSyncBatchStaysLinearOnLargeBackfill(t *testing.T) {
	state, mailbox := openBatchTestStore(t)
	base := time.Now().Add(-24 * time.Hour)
	existing := make([]MailboxSyncMessage, 0, 400)
	for index := 0; index < 400; index++ {
		existing = append(existing, MailboxSyncMessage{
			RemoteID: fmt.Sprintf("imap:%d", index), Source: "imap",
			Subject: "旧邮件", Body: "old", ReceivedAt: base,
		})
	}
	if _, err := state.ApplyMailboxSyncBatch([]MailboxSyncUpdate{{MailboxID: mailbox.ID, Messages: existing}}); err != nil {
		t.Fatalf("准备旧邮件失败：%v", err)
	}
	backfill := make([]MailboxSyncMessage, 0, 400)
	for index := 400; index < 800; index++ {
		backfill = append(backfill, MailboxSyncMessage{
			RemoteID: fmt.Sprintf("imap:%d", index), Source: "imap",
			Subject: "历史邮件", Body: "123456", ReceivedAt: base,
		})
	}
	started := time.Now()
	created, err := state.ApplyMailboxSyncBatch([]MailboxSyncUpdate{{MailboxID: mailbox.ID, Messages: backfill}})
	elapsed := time.Since(started)
	if err != nil || created != 400 {
		t.Fatalf("回填失败：created=%d err=%v", created, err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("回填 400 封用了 %s，去重查询退回了逐封扫全表", elapsed)
	}
	if messages := state.MessagesForMailbox(mailbox.ID); len(messages) != 800 {
		t.Fatalf("回填后邮件数为 %d，期望 800", len(messages))
	}
}
