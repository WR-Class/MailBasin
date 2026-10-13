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

// 报告里「扫描」必须是扫描数、不是入库数，「匹配」也必须是匹配数、不是恒为 0。
//
// 这条锁死的是「拿入库数冒充扫描数」那种改法。它曾经把
// 「服务器返回了 446 个 UID」读成「服务器什么都没返回」——报告上的名字和值
// 不是一回事时，看一眼数字就会得出相反的结论。
func TestBackfillReportsScannedAndMatchedNotTheStoredCount(t *testing.T) {
	state := openSyncTestStore(t)
	service := NewService(config.Default(), state)
	mailbox := createIMAPBackfillTestMailbox(t, service)

	receivedAt := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	backend := &fakeMessageSyncBackend{imapResult: protocol.MailSyncBatchResult{
		MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{
			mailbox.ID: {{
				RemoteID: "imap:9", RemoteIDs: []string{"imap:9"}, CanonicalID: "message-id:report@example.invalid",
				Source: "imap", UID: "9", Subject: "Login verification code",
				Body: "Your verification code is 111111.", ContentType: "text/plain", ReceivedAt: receivedAt,
			}},
		},
		Scanned: 9,
		Matched: 7,
	}}
	service.messageBackend = backend

	result, err := service.BackfillMailboxMessages(context.Background(), mailbox.ID)
	if err != nil {
		t.Fatalf("回填失败：%v", err)
	}
	if result.SyncedMessages != 1 {
		t.Fatalf("应入库 1 封，实际 %d", result.SyncedMessages)
	}
	if result.Scanned != 9 {
		t.Fatalf("「扫描」应是后端返回的 9，实际 %d；拿入库数冒充时它会等于入库数 %d", result.Scanned, result.SyncedMessages)
	}
	if result.Matched != 7 {
		t.Fatalf("「匹配」应是后端返回的 7，实际 %d", result.Matched)
	}
}

// 外部取码邮箱没有收件箱，回填必须如实拒绝。
//
// 以前它会静默走一遍普通同步、把 FullScan 丢掉，而界面那个按钮写着
// 「取回服务器上已有的旧邮件」——参数被丢掉、标签照着念，正是这里要消掉的不一致。
func TestBackfillRejectsExternalCodeMailbox(t *testing.T) {
	state := openSyncTestStore(t)
	service := NewService(config.Default(), state)
	const address = "external-backfill@example.invalid"
	if _, err := service.ImportExternalMailboxes([]ExternalMailboxInput{{
		Email: address, CodeAPIURL: "https://host/access/token/mailboxes/" + address + "/code",
	}}); err != nil {
		t.Fatalf("导入外部取码邮箱失败：%v", err)
	}
	mailbox, ok := service.store.FindMailboxByEmail(address)
	if !ok {
		t.Fatal("外部取码邮箱没有导入")
	}

	if _, err := service.BackfillMailboxMessages(context.Background(), mailbox.ID); err == nil {
		t.Fatal("对外部取码邮箱回填应当明确报错，而不是静默成功")
	} else if code, _, _ := protocol.ErrorDetails(err); code != "external_has_no_mailbox" {
		t.Fatalf("错误码应是 external_has_no_mailbox，实际 %q（%v）", code, err)
	}
}

// 回填的下限必须跟着保留期走。
//
// 比保留期更早的邮件取回来也留不住——自动维护会按 received_at 删掉它——那次全量扫描的
// 墙钟和写库就白费。实测里正是这样：命中 446 封、入库 169 封，紧接着那轮维护删掉了 169 封。
func TestBackfillBoundsScansByTheRetentionWindow(t *testing.T) {
	state := openSyncTestStore(t)
	service := NewService(config.Default(), state)
	mailbox := createIMAPBackfillTestMailbox(t, service)

	backend := &fakeMessageSyncBackend{imapResult: protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}}}
	service.messageBackend = backend

	before := time.Now()
	if _, err := service.BackfillMailboxMessages(context.Background(), mailbox.ID); err != nil {
		t.Fatalf("回填失败：%v", err)
	}
	if len(backend.imapOptions) == 0 {
		t.Fatal("回填没有走到 IMAP 后端")
	}
	options := backend.imapOptions[len(backend.imapOptions)-1]

	days := service.cfg.DatabaseMessageRetentionDays
	if days <= 0 {
		t.Fatalf("这个测试假定配置里有保留期，实际 %d", days)
	}
	if options.After.IsZero() {
		t.Fatal("回填必须把保留期起点作为下限传下去，否则会去取注定被删掉的邮件")
	}
	want := before.AddDate(0, 0, -days)
	if diff := options.After.Sub(want); diff > time.Minute || diff < -time.Minute {
		t.Fatalf("下限应是「现在减去保留期 %d 天」即 %s 附近，实际 %s（差 %s）",
			days, want.Format(time.RFC3339), options.After.Format(time.RFC3339), diff)
	}
}

// 反向：常规增量同步不该被套上保留期下限——它本来就是从游标往后走的。
// 少了这条，把下限加到所有路径上也能让上面那条通过。
func TestIncrementalIMAPSyncKeepsNoRetentionFloor(t *testing.T) {
	state := openSyncTestStore(t)
	service := NewService(config.Default(), state)
	mailbox := createIMAPBackfillTestMailbox(t, service)

	backend := &fakeMessageSyncBackend{imapResult: protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}}}
	service.messageBackend = backend

	if _, err := service.SyncIMAPMailboxWithOptions(context.Background(), mailbox.ID, MessageSyncOptions{
		Mode: protocol.MailSyncModeVerification, Limit: 20, UseCursor: true,
	}); err != nil {
		t.Fatalf("增量同步失败：%v", err)
	}
	if len(backend.imapOptions) == 0 {
		t.Fatal("增量同步没有走到 IMAP 后端")
	}
	options := backend.imapOptions[len(backend.imapOptions)-1]
	if !options.After.IsZero() {
		t.Fatalf("增量同步不该带保留期下限，实际 %s", options.After.Format(time.RFC3339))
	}
}

// 批量满量同步必须和单个邮箱回填用同一个保留期下限。
//
// 两个入口都搜 UID SEARCH ALL 的时候，两边都会把注定被自动维护删掉的邮件取回来。
// 只锁一个入口是不够的：改好一个、漏掉另一个，正是这一条要防的形状。
func TestBulkExistingMailboxSyncSharesTheRetentionFloor(t *testing.T) {
	state := openSyncTestStore(t)
	saveSyncTestAccount(t, state, "bulk-floor@icloud.com", true)
	backend := &fakeMessageSyncBackend{
		imapResult: protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}},
		webResult:  protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}},
	}
	service := NewService(config.Default(), state)
	service.messageBackend = backend

	before := time.Now()
	if _, err := service.SyncExistingMailboxMessages(context.Background()); err != nil {
		t.Fatalf("批量同步失败：%v", err)
	}
	imapOptions, webOptions := backend.latestOptions()
	days := service.cfg.DatabaseMessageRetentionDays
	if days <= 0 {
		t.Fatalf("这个测试假定配置里有保留期，实际 %d", days)
	}
	want := before.AddDate(0, 0, -days)
	for _, item := range []struct {
		name    string
		options protocol.MailSyncOptions
	}{{"IMAP", imapOptions}, {"Web", webOptions}} {
		if !item.options.FullScan || item.options.UseCursor {
			t.Fatalf("%s 路径应当是无游标满量扫描：%+v", item.name, item.options)
		}
		if item.options.After.IsZero() {
			t.Fatalf("%s 路径必须带上保留期下限，否则它会去搜 UID SEARCH ALL", item.name)
		}
		if diff := item.options.After.Sub(want); diff > time.Minute || diff < -time.Minute {
			t.Fatalf("%s 路径的下限应是「现在减去保留期 %d 天」即 %s 附近，实际 %s（差 %s）",
				item.name, days, want.Format(time.RFC3339), item.options.After.Format(time.RFC3339), diff)
		}
	}
}

// 被打断的一轮必须能接着做，而不是从零重扫。
//
// 原来的收尾是「服务重启，未完成的邮件同步任务已停止，请重新执行」——重新执行就是从零重来。
// 一轮批量全量在几百个邮箱上要跑很久，白重扫的代价是真实存在的。
func TestInterruptedBulkSyncResumesFromCompletedAccounts(t *testing.T) {
	state := openSyncTestStore(t)
	finished := saveSyncTestAccount(t, state, "resume-finished@icloud.com", true)
	saveSyncTestAccount(t, state, "resume-pending@icloud.com", true)
	backend := &fakeMessageSyncBackend{
		imapResult: protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}},
		webResult:  protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}},
	}
	service := NewService(config.Default(), state)
	service.messageBackend = backend

	// 造出「上一轮做完一个账号就被打断」的状态：重启后从库里读回来的就是这个形状。
	service.messageSyncJob = ExistingMailboxMessageSyncJob{
		Status:              "interrupted",
		CompletedAccountIDs: []string{finished.AccountID},
		Failures:            []ExistingMailboxMessageSyncFailure{},
	}

	job, err := service.StartExistingMailboxMessageSync(context.Background())
	if err != nil {
		t.Fatalf("启动续传失败：%v", err)
	}
	if job.ResumedAccounts != 1 || job.TotalAccounts != 1 {
		t.Fatalf("这一轮应当只做没做完的那 1 个账号：Resumed=%d Total=%d", job.ResumedAccounts, job.TotalAccounts)
	}
	if len(job.CompletedAccountIDs) != 1 || job.CompletedAccountIDs[0] != finished.AccountID {
		t.Fatalf("已完成集合要接着带下去，否则再被打断一次就丢断点了：%+v", job.CompletedAccountIDs)
	}

	status := waitExistingMailboxMessageSyncJob(t, service)
	if status.SuccessfulAccounts != 1 || status.FailedAccounts != 0 {
		t.Fatalf("续传这一轮的结果不对：%+v", status)
	}
	if imap, web, _ := backend.counts(); imap != 1 || web != 1 {
		t.Fatalf("已经做完的账号不该再被访问一次：IMAP=%d，Web=%d", imap, web)
	}
}

// 反面：整轮跑完之后再点一次，必须是全新的一轮。
//
// 少了这条，续传就会变成「跑过一次以后再也做不了全量」——完成集合永远非空，
// 每次都被当成续传。
func TestCompletedBulkSyncStartsAFullPassAgain(t *testing.T) {
	state := openSyncTestStore(t)
	finished := saveSyncTestAccount(t, state, "full-again-a@icloud.com", true)
	saveSyncTestAccount(t, state, "full-again-b@icloud.com", true)
	backend := &fakeMessageSyncBackend{
		imapResult: protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}},
		webResult:  protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}},
	}
	service := NewService(config.Default(), state)
	service.messageBackend = backend

	// 上一轮是完整跑完的：完成集合里还留着 id，但它已经不该被认。
	service.messageSyncJob = ExistingMailboxMessageSyncJob{
		Status:              "completed",
		CompletedAccountIDs: []string{finished.AccountID},
		Failures:            []ExistingMailboxMessageSyncFailure{},
	}

	job, err := service.StartExistingMailboxMessageSync(context.Background())
	if err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	if job.ResumedAccounts != 0 || job.TotalAccounts != 2 {
		t.Fatalf("整轮跑完之后应当是全新的一轮，两个账号都要做：Resumed=%d Total=%d", job.ResumedAccounts, job.TotalAccounts)
	}
	_ = waitExistingMailboxMessageSyncJob(t, service)
	if imap, _, _ := backend.counts(); imap != 2 {
		t.Fatalf("两个账号都该被访问：IMAP=%d", imap)
	}
}

// 成功做完的账号要被记下来，而且要真的落库。
//
// 上面那条续传测试是手工把完成集合塞进去的，只覆盖了「读到集合之后怎么用」，
// 没覆盖「这个集合是怎么写出来的」。少了这一条，把记录那一步删掉也照样全绿。
func TestCompletedBulkSyncRecordsWhichAccountsItFinished(t *testing.T) {
	state := openSyncTestStore(t)
	first := saveSyncTestAccount(t, state, "record-a@icloud.com", true)
	second := saveSyncTestAccount(t, state, "record-b@icloud.com", true)
	backend := &fakeMessageSyncBackend{
		imapResult: protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}},
		webResult:  protocol.MailSyncBatchResult{MessagesByMailbox: map[string][]protocol.ICloudSyncedMessage{}},
	}
	service := NewService(config.Default(), state)
	service.messageBackend = backend

	if _, err := service.StartExistingMailboxMessageSync(context.Background()); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	status := waitExistingMailboxMessageSyncJob(t, service)
	if status.Status != "completed" {
		t.Fatalf("两个账号都该成功，任务应以 completed 收尾：%+v", status)
	}
	recorded := make(map[string]bool, len(status.CompletedAccountIDs))
	for _, id := range status.CompletedAccountIDs {
		recorded[id] = true
	}
	if !recorded[first.AccountID] || !recorded[second.AccountID] {
		t.Fatalf("做完的账号都要记进完成集合：期望 %s 和 %s，实际 %+v",
			first.AccountID, second.AccountID, status.CompletedAccountIDs)
	}

	// 落库才是关键：续传靠的是重启之后还能把它读回来。
	var persisted ExistingMailboxMessageSyncJob
	found, err := state.LoadRuntimeState(existingMailboxMessageSyncStateID, &persisted)
	if err != nil || !found {
		t.Fatalf("任务状态应当已落库：found=%v err=%v", found, err)
	}
	persistedIDs := make(map[string]bool, len(persisted.CompletedAccountIDs))
	for _, id := range persisted.CompletedAccountIDs {
		persistedIDs[id] = true
	}
	if !persistedIDs[first.AccountID] || !persistedIDs[second.AccountID] {
		t.Fatalf("落库的那份也要带完成集合，否则重启后断点就丢了：%+v", persisted.CompletedAccountIDs)
	}
}

func waitExistingMailboxMessageSyncJob(t *testing.T, service *Service) ExistingMailboxMessageSyncJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := service.ExistingMailboxMessageSyncStatus()
		if !status.Running {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("批量同步没在预期时间内结束：%+v", status)
		}
		time.Sleep(10 * time.Millisecond)
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
