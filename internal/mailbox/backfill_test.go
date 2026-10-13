package mailbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

// 断言「全量」这个意图真的到达了最底层：既没有被分发分支丢掉，
// 也没有被硬编码的增量参数覆盖。只检查上层调用点写对了是不够的。
func TestBackfillMailboxMessagesSendsFullScanWithoutCursorToIMAPBackend(t *testing.T) {
	state := openSyncTestStore(t)
	service := NewService(config.Default(), state)
	mailbox := createIMAPBackfillTestMailbox(t, service)

	backend := &fakeMessageSyncBackend{imapResult: protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}}}
	service.messageBackend = backend

	if _, err := service.BackfillMailboxMessages(context.Background(), mailbox.ID); err != nil {
		t.Fatalf("回填失败：%v", err)
	}
	options, _ := backend.latestOptions()
	if !options.FullScan {
		t.Fatalf("回填必须以全量扫描下发，实际 %+v", options)
	}
	if options.UseCursor {
		t.Fatalf("回填不能带游标，否则只搜游标之后那一段，旧邮件拿不到：%+v", options)
	}
	if options.CursorUID != "" {
		t.Fatalf("回填不应携带游标值，实际 %q", options.CursorUID)
	}
	if options.Mode != protocol.MailSyncModeAllRecent {
		t.Fatalf("回填模式不正确：%+v", options)
	}
}

// 增量那条路不能被这次改动带跑：它仍然要带游标、仍然是取码模式。
func TestIncrementalIMAPMailboxSyncStillUsesCursor(t *testing.T) {
	state := openSyncTestStore(t)
	service := NewService(config.Default(), state)
	mailbox := createIMAPBackfillTestMailbox(t, service)
	if _, err := state.ApplyMailboxSyncBatch([]store.MailboxSyncUpdate{{MailboxID: mailbox.ID, LastUID: "573", SyncedAt: time.Now()}}); err != nil {
		t.Fatalf("设置测试游标失败：%v", err)
	}

	backend := &fakeMessageSyncBackend{imapResult: protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}}}
	service.messageBackend = backend

	if _, err := service.SyncIMAPMailbox(context.Background(), mailbox.ID); err != nil {
		t.Fatalf("增量同步失败：%v", err)
	}
	options, _ := backend.latestOptions()
	if !options.UseCursor || options.FullScan {
		t.Fatalf("增量同步应带游标且非全量：%+v", options)
	}
	if options.CursorUID != "573" {
		t.Fatalf("增量同步应从邮箱自己的游标往后搜，实际 %q", options.CursorUID)
	}
	if options.Mode != protocol.MailSyncModeVerification {
		t.Fatalf("增量同步应保持取码模式：%+v", options)
	}
}

// 这是本轮最危险的一处：全量扫描只完成一部分时若把游标推到最新 UID，
// 没扫到的那一段就永远够不到了——正是要修的那个毛病，只是换了个触发方式。
//
// 直接打 SyncIMAPMailboxWithOptions，不经分发分支：这条规则属于它，
// 混在一起会让「分发丢了参数」和「游标推进规则写错」两种故障分不清。
func TestPartialFullScanDoesNotAdvanceCursor(t *testing.T) {
	state := openSyncTestStore(t)
	service := NewService(config.Default(), state)
	mailbox := createIMAPBackfillTestMailbox(t, service)
	if _, err := state.ApplyMailboxSyncBatch([]store.MailboxSyncUpdate{{MailboxID: mailbox.ID, LastUID: "573", SyncedAt: time.Now()}}); err != nil {
		t.Fatalf("设置测试游标失败：%v", err)
	}

	backend := &fakeMessageSyncBackend{imapResult: protocol.MailSyncBatchResult{
		MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{},
		LastUID:           "900",
		FailedBatches:     1,
	}}
	service.messageBackend = backend

	if _, err := service.SyncIMAPMailboxWithOptions(context.Background(), mailbox.ID, MessageSyncOptions{
		Mode: protocol.MailSyncModeAllRecent, FullScan: true, UseCursor: false,
	}); err == nil {
		t.Fatalf("部分完成应当如实报错，而不是报成功")
	}
	after, ok := state.FindMailboxByID(mailbox.ID)
	if !ok {
		t.Fatalf("回填后邮箱应仍然存在")
	}
	if after.LastSyncUID != "573" {
		t.Fatalf("部分完成时游标不得推进，实际 %q；推进了就再也扫不到没取到的那一段", after.LastSyncUID)
	}
}

// 反过来：完整跑完才允许推进游标，否则每次回填都会把已经取过的再取一遍。
func TestCompletedFullScanAdvancesCursor(t *testing.T) {
	state := openSyncTestStore(t)
	service := NewService(config.Default(), state)
	mailbox := createIMAPBackfillTestMailbox(t, service)

	backend := &fakeMessageSyncBackend{imapResult: protocol.MailSyncBatchResult{
		MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{},
		LastUID:           "900",
	}}
	service.messageBackend = backend

	if _, err := service.SyncIMAPMailboxWithOptions(context.Background(), mailbox.ID, MessageSyncOptions{
		Mode: protocol.MailSyncModeAllRecent, FullScan: true, UseCursor: false,
	}); err != nil {
		t.Fatalf("完整回填不应报错：%v", err)
	}
	after, ok := state.FindMailboxByID(mailbox.ID)
	if !ok {
		t.Fatalf("回填后邮箱应仍然存在")
	}
	if after.LastSyncUID != "900" {
		t.Fatalf("完整回填后游标应推进到最新，实际 %q", after.LastSyncUID)
	}
}

// 全量与「用游标」同时成立时，全量必须赢：带游标会让搜索从中间开始，那就不是全量了。
// 现在没有调用方同时传这两个值，所以这层守卫只靠这条测试守着——
// 少了它，守卫可以被人删掉而没有任何测试变红。
func TestFullScanOverridesUseCursor(t *testing.T) {
	state := openSyncTestStore(t)
	service := NewService(config.Default(), state)
	mailbox := createIMAPBackfillTestMailbox(t, service)
	if _, err := state.ApplyMailboxSyncBatch([]store.MailboxSyncUpdate{{MailboxID: mailbox.ID, LastUID: "573", SyncedAt: time.Now()}}); err != nil {
		t.Fatalf("设置测试游标失败：%v", err)
	}

	backend := &fakeMessageSyncBackend{imapResult: protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}}}
	service.messageBackend = backend

	if _, err := service.SyncIMAPMailboxWithOptions(context.Background(), mailbox.ID, MessageSyncOptions{
		Mode: protocol.MailSyncModeAllRecent, FullScan: true, UseCursor: true,
	}); err != nil {
		t.Fatalf("全量同步失败：%v", err)
	}
	options, _ := backend.latestOptions()
	if options.UseCursor {
		t.Fatalf("全量扫描不得带游标：%+v", options)
	}
	if options.CursorUID != "" {
		t.Fatalf("全量扫描不得携带游标值，实际 %q", options.CursorUID)
	}
}

// 回填把邮件真的写进消息表才算数：这是「取不到已经存在的邮件」的最终验收点。
func TestBackfillStoresReturnedMessages(t *testing.T) {
	state := openSyncTestStore(t)
	service := NewService(config.Default(), state)
	mailbox := createIMAPBackfillTestMailbox(t, service)

	receivedAt := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	backend := &fakeMessageSyncBackend{imapResult: protocol.MailSyncBatchResult{
		MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{
			mailbox.ID: {{
				RemoteID: "imap:12", RemoteIDs: []string{"imap:12"}, CanonicalID: "message-id:backfill@example.invalid",
				Source: "imap", UID: "12", Subject: "Login verification code",
				Body: "Your verification code is 386812.", ContentType: "text/plain", ReceivedAt: receivedAt,
			}},
		},
		LastUID: "12",
	}}
	service.messageBackend = backend

	created, err := service.BackfillMailboxMessages(context.Background(), mailbox.ID)
	if err != nil {
		t.Fatalf("回填失败：%v", err)
	}
	if created.SyncedMessages != 1 {
		t.Fatalf("回填应写入 1 封邮件，实际 %d", created.SyncedMessages)
	}
	messages := state.MessagesForMailbox(mailbox.ID)
	if len(messages) != 1 {
		t.Fatalf("消息表里应有 1 封，实际 %d", len(messages))
	}
	if !strings.Contains(messages[0].Body, "386812") {
		t.Fatalf("写进去的邮件正文不对：%q", messages[0].Body)
	}
}

func createIMAPBackfillTestMailbox(t *testing.T, service *Service) domain.Mailbox {
	t.Helper()
	account, _, err := service.CreateIMAPAccount(
		"qq", "443836027@example.invalid", "test-app-password", "imap.qq.com", 993, "", "回填测试", "", nil,
	)
	if err != nil {
		t.Fatalf("创建测试 IMAP 母号失败：%v", err)
	}
	if len(account.Mailboxes) != 1 {
		t.Fatalf("测试母号地址数不正确：%d", len(account.Mailboxes))
	}
	return account.Mailboxes[0]
}
