package mailwatcher

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
	"icloud-privacy-mail-v2/internal/store"
)

func newMicrosoftWatcherTestService(t *testing.T, cfg config.Config) (*Service, *store.Store) {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	service := NewService(cfg, database, mailboxservice.NewService(cfg, database), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return service, database
}

// Microsoft 组没有 IMAP 也没有 iCloud Web，必须靠 hasGraph 才能进入轮询调度。
func TestGroupsIncludeMicrosoftAccountsWithSession(t *testing.T) {
	service, database := newMicrosoftWatcherTestService(t, config.Default())
	session, err := database.SaveMicrosoftSession(domain.MicrosoftSession{Email: "ms@outlook.com", RefreshToken: "refresh"})
	if err != nil {
		t.Fatalf("创建 Microsoft 会话失败：%v", err)
	}
	first, _, _ := database.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "alias-one@outlook.com", IsActive: true, Provider: domain.ProviderMicrosoft}, "")
	second, _, _ := database.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "alias-two@outlook.com", IsActive: true, Provider: domain.ProviderMicrosoft}, "")
	if first.ProviderOrDefault() != domain.ProviderMicrosoft || second.ProviderOrDefault() != domain.ProviderMicrosoft {
		t.Fatalf("邮箱服务商未随账号写入：%q %q", first.ProviderOrDefault(), second.ProviderOrDefault())
	}

	groups := service.groups(true)
	if len(groups) != 1 {
		t.Fatalf("应只有一个 Microsoft 分组：%+v", groups)
	}
	group := groups[0]
	if group.provider != domain.ProviderMicrosoft {
		t.Fatalf("分组服务商不正确：%q", group.provider)
	}
	if group.key != "ms:"+session.AccountID {
		t.Fatalf("Microsoft 分组键应加 ms: 前缀以避免与 iCloud 账号 ID 冲突：%q", group.key)
	}
	if !group.hasGraph || group.hasIMAP || group.hasWeb {
		t.Fatalf("Microsoft 分组应只走 Graph：%+v", group)
	}
	if !group.polls() {
		t.Fatal("Microsoft 分组必须进入轮询调度，否则永远不会收信")
	}
	if len(group.mailboxes) != 2 {
		t.Fatalf("同账号的两个别名应归入同一分组：%d", len(group.mailboxes))
	}
	imapGroups, webPollingGroups, graphPollingGroups, gmailPollingGroups := groupModeCounts(groups)
	if imapGroups != 0 || webPollingGroups != 0 || graphPollingGroups != 1 || gmailPollingGroups != 0 {
		t.Fatalf("Microsoft 分组必须只计入 Graph 轮询，混进 Web 计数会让状态页显示成 0 轮询：IMAP=%d Web=%d Graph=%d Gmail=%d",
			imapGroups, webPollingGroups, graphPollingGroups, gmailPollingGroups)
	}
}

// 没有 OAuth2 会话时不应去轮询，否则每一轮都会拿空 token 打 Graph。
func TestGroupsSkipMicrosoftAccountsWithoutSession(t *testing.T) {
	service, database := newMicrosoftWatcherTestService(t, config.Default())
	account, err := database.SaveICloudSession(domain.ICloudSession{AppleID: "placeholder@icloud.com", DSID: "placeholder"})
	if err != nil {
		t.Fatalf("创建占位账号失败：%v", err)
	}
	// 直接把邮箱挂到一个没有 Microsoft 会话的账号上。
	if _, _, err := database.UpsertMailboxFromRemote(account.AccountID, domain.RemoteMailbox{Email: "orphan@outlook.com", IsActive: true, Provider: domain.ProviderMicrosoft}, ""); err != nil {
		t.Fatalf("写入孤立 Microsoft 邮箱失败：%v", err)
	}
	if groups := service.groups(true); len(groups) != 0 {
		t.Fatalf("缺少 Microsoft 会话时不应产生分组：%+v", groups)
	}
}

func TestGroupsSkipMicrosoftAccountsWhenModuleDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.MicrosoftEnabled = false
	service, database := newMicrosoftWatcherTestService(t, cfg)
	session, err := database.SaveMicrosoftSession(domain.MicrosoftSession{Email: "ms@outlook.com", RefreshToken: "refresh"})
	if err != nil {
		t.Fatalf("创建 Microsoft 会话失败：%v", err)
	}
	if _, _, err := database.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "alias@outlook.com", IsActive: true, Provider: domain.ProviderMicrosoft}, ""); err != nil {
		t.Fatalf("写入 Microsoft 邮箱失败：%v", err)
	}
	if groups := service.groups(true); len(groups) != 0 {
		t.Fatalf("Microsoft 模块关闭时不应产生分组：%+v", groups)
	}
}

// 两种服务商必须能并存于同一次调度，且互不干扰。
func TestGroupsKeepICloudAndMicrosoftSeparate(t *testing.T) {
	service, database := newMicrosoftWatcherTestService(t, config.Default())
	icloudSession, err := database.SaveICloudSession(domain.ICloudSession{
		AppleID: "imap@icloud.com", DSID: "imap-dsid", MailGatewayBaseURL: "https://mail.example.test",
		Cookies:     []domain.SessionCookie{{Name: "token", Value: "imap-token"}},
		LoginStates: []domain.LoginState{{Kind: domain.LoginStateICloudIMAP, IMAPEmail: "imap@icloud.com", IMAPUsername: "imap@icloud.com", IMAPAppPassword: "app-password"}},
	})
	if err != nil {
		t.Fatalf("创建 iCloud 账号失败：%v", err)
	}
	_, _, _ = database.UpsertMailboxFromRemote(icloudSession.AccountID, domain.RemoteMailbox{Email: "imap-alias@icloud.com", IsActive: true}, "")
	microsoftSession, err := database.SaveMicrosoftSession(domain.MicrosoftSession{Email: "ms@outlook.com", RefreshToken: "refresh"})
	if err != nil {
		t.Fatalf("创建 Microsoft 会话失败：%v", err)
	}
	_, _, _ = database.UpsertMailboxFromRemote(microsoftSession.AccountID, domain.RemoteMailbox{Email: "alias@outlook.com", IsActive: true, Provider: domain.ProviderMicrosoft}, "")

	groups := service.groups(true)
	if len(groups) != 2 {
		t.Fatalf("iCloud 与 Microsoft 应各有一个分组：%+v", groups)
	}
	imapGroups, webPollingGroups, graphPollingGroups, gmailPollingGroups := groupModeCounts(groups)
	if imapGroups != 1 || webPollingGroups != 0 || graphPollingGroups != 1 || gmailPollingGroups != 0 {
		t.Fatalf("混合分组应分别计入 IMAP 与 Graph：IMAP=%d Web=%d Graph=%d Gmail=%d",
			imapGroups, webPollingGroups, graphPollingGroups, gmailPollingGroups)
	}
	byProvider := map[string]watchGroup{}
	for _, group := range groups {
		byProvider[group.provider] = group
	}
	if byProvider[domain.ProviderICloud].key != icloudSession.AccountID {
		t.Fatalf("iCloud 分组键不应被改写：%q", byProvider[domain.ProviderICloud].key)
	}
	if byProvider[domain.ProviderMicrosoft].key != "ms:"+microsoftSession.AccountID {
		t.Fatalf("Microsoft 分组键不正确：%q", byProvider[domain.ProviderMicrosoft].key)
	}
}

// Microsoft 组只有 Graph 一条读信路径，不能拿到 iCloud 的 Web 轮询间隔。
func TestPollDelayUsesGraphIntervalForMicrosoftGroups(t *testing.T) {
	cfg := config.Default()
	cfg.MailWatcherWebPollMS = 60000
	cfg.MicrosoftMailWatcherPollMS = 15000
	service := &Service{cfg: cfg}

	graphBase := service.pollDelay(watchGroup{key: "ms:acc", hasGraph: true}, 0)
	if graphBase < 15*time.Second || graphBase > 18*time.Second {
		t.Fatalf("Graph 首次轮询间隔超出预期：%s", graphBase)
	}
	graphRetry := service.pollDelay(watchGroup{key: "ms:acc", hasGraph: true}, 2)
	if graphRetry < time.Minute || graphRetry > 72*time.Second {
		t.Fatalf("Graph 失败退避间隔超出预期：%s", graphRetry)
	}
	// 上限必须是 10 分钟，避免退避无限增长。
	graphCapped := service.pollDelay(watchGroup{key: "ms:acc", hasGraph: true}, 20)
	if graphCapped < 10*time.Minute || graphCapped > 12*time.Minute {
		t.Fatalf("Graph 退避上限超出预期：%s", graphCapped)
	}

	webBase := service.pollDelay(watchGroup{key: "acc", hasWeb: true}, 0)
	if webBase < time.Minute || webBase > 72*time.Second {
		t.Fatalf("iCloud Web 轮询间隔不应被 Microsoft 配置影响：%s", webBase)
	}
}

// 轮询下限保证配置写错也不会把 Graph 打爆。
func TestGraphPollIntervalHasFloor(t *testing.T) {
	service := &Service{cfg: config.Default()}
	service.cfg.MicrosoftMailWatcherPollMS = 100
	if got := service.graphPollInterval(); got != 15*time.Second {
		t.Fatalf("过小的配置应被抬到 15 秒下限，实际 %s", got)
	}
	service.cfg.MicrosoftMailWatcherPollMS = 45000
	if got := service.graphPollInterval(); got != 45*time.Second {
		t.Fatalf("合法配置应原样使用，实际 %s", got)
	}
}

func TestFindWatchGroupMatchesMicrosoftPrefixedKey(t *testing.T) {
	groups := []watchGroup{
		{key: "icloud-acc", provider: domain.ProviderICloud, session: domain.ICloudSession{AccountID: "icloud-acc"}},
		{key: "ms:ms-acc", provider: domain.ProviderMicrosoft, hasGraph: true},
	}
	if group, ok := findWatchGroup(groups, "icloud-acc"); !ok || group.provider != domain.ProviderICloud {
		t.Fatalf("应按 iCloud 账号 ID 命中：%+v ok=%t", group, ok)
	}
	if group, ok := findWatchGroup(groups, "ms-acc"); !ok || group.provider != domain.ProviderMicrosoft {
		t.Fatalf("应按裸账号 ID 命中 Microsoft 分组：%+v ok=%t", group, ok)
	}
	if group, ok := findWatchGroup(groups, "ms:ms-acc"); !ok || group.provider != domain.ProviderMicrosoft {
		t.Fatalf("应按带前缀的分组键命中：%+v ok=%t", group, ok)
	}
	if _, ok := findWatchGroup(groups, "missing"); ok {
		t.Fatal("不存在的账号不应命中")
	}
}

// Graph 轮询必须记进 GraphPolls。这条路径以前固定传 false，于是邮件同步成功、
// 游标也在推进，状态里却显示 0 次轮询——把"正在工作"误报成"从未工作"。
func TestRecordSyncResultCountsGraphPollsNotWebPolls(t *testing.T) {
	// 不建库：recordSyncResult 只在 store 非空时才落盘，用裸 Service 可避开 Windows 上的临时目录清理竞争。
	service := &Service{}

	service.recordSyncResult(mailboxservice.MailboxMessageSyncBatchResult{SyncedMessages: 3}, nil, pollGraph)

	status := service.Snapshot()
	if status.GraphPolls != 1 {
		t.Fatalf("Graph 轮询应计 1 次，实际 %d", status.GraphPolls)
	}
	if status.WebPolls != 0 {
		t.Fatalf("Graph 轮询不得计入 Web 计数，实际 %d", status.WebPolls)
	}
	if status.SyncedMessages != 3 {
		t.Fatalf("同步邮件数应累计，实际 %d", status.SyncedMessages)
	}
}

// 两条轮询路径各自计数、互不串台，IMAP IDLE 不算轮询。
func TestRecordSyncResultKeepsWebAndGraphPollsApart(t *testing.T) {
	service := &Service{}

	service.recordSyncResult(mailboxservice.MailboxMessageSyncBatchResult{}, nil, webPollKind(true))
	service.recordSyncResult(mailboxservice.MailboxMessageSyncBatchResult{}, nil, pollGraph)
	service.recordSyncResult(mailboxservice.MailboxMessageSyncBatchResult{}, nil, pollGraph)
	service.recordSyncResult(mailboxservice.MailboxMessageSyncBatchResult{}, nil, pollNone)

	status := service.Snapshot()
	if status.WebPolls != 1 || status.GraphPolls != 2 {
		t.Fatalf("两条轮询路径应各自计数：Web=%d Graph=%d", status.WebPolls, status.GraphPolls)
	}
}

// webPollKind 必须如实折算布尔量，否则 iCloud 路径会静默丢掉轮询计数。
func TestWebPollKindMapsBoolean(t *testing.T) {
	if webPollKind(true) != pollWeb {
		t.Fatal("true 应折算为 pollWeb")
	}
	if webPollKind(false) != pollNone {
		t.Fatal("false 应折算为 pollNone")
	}
}

// 失败的轮询同样要计数，否则持续报错的 Graph 组看起来像根本没在跑。
func TestRecordSyncResultCountsGraphPollOnError(t *testing.T) {
	service := &Service{}

	service.recordSyncResult(mailboxservice.MailboxMessageSyncBatchResult{}, errors.New("graph 429"), pollGraph)

	status := service.Snapshot()
	if status.GraphPolls != 1 {
		t.Fatalf("失败的轮询同样应计数，实际 %d", status.GraphPolls)
	}
	if status.LastError == "" {
		t.Fatal("失败应记录 LastError")
	}
}
