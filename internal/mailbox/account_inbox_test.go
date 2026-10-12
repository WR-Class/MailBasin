package mailbox

import (
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

// seedMessage 往一个地址下放一封信。验证码提取只认六位数字，
// 所以测试里的验证码都用六位，否则测的就不是聚合而是提取规则。
func seedMessage(t *testing.T, service *Service, mailboxID, remoteID, subject, body string, receivedAt time.Time) {
	t.Helper()
	if _, _, err := service.store.UpsertMessage(mailboxID, remoteID, "test", subject, "sender@example.com", body, receivedAt); err != nil {
		t.Fatalf("放置邮件失败：%v", err)
	}
}

func TestAccountInboxAggregatesAddressesAndCodes(t *testing.T) {
	service := newIMAPFixture(t)
	accountID := createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	if _, err := service.AddIMAPAccountAddresses(accountID, []IMAPMailboxInput{{Email: "alias@qq.com"}}); err != nil {
		t.Fatalf("追加地址失败：%v", err)
	}
	primary, _ := service.store.FindMailboxByEmail("12345@qq.com")
	alias, _ := service.store.FindMailboxByEmail("alias@qq.com")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seedMessage(t, service, primary.ID, "m1", "登录验证码", "你的验证码是 123456", base)
	seedMessage(t, service, alias.ID, "m2", "注册验证码", "验证码 654321", base.Add(time.Minute))
	seedMessage(t, service, alias.ID, "m3", "本周推荐", "这封信没有验证码", base.Add(2*time.Minute))

	inbox, err := service.AccountInbox(accountID, 0)
	if err != nil {
		t.Fatalf("聚合失败：%v", err)
	}
	if len(inbox.Mailboxes) != 2 {
		t.Fatalf("应聚合出 2 个地址，实际 %d 个：%+v", len(inbox.Mailboxes), inbox.Mailboxes)
	}
	if inbox.Total != 3 || len(inbox.Messages) != 3 {
		t.Fatalf("应有 3 封信，实际 Total=%d 返回 %d 封", inbox.Total, len(inbox.Messages))
	}
	if inbox.CodeCount != 2 {
		t.Fatalf("应只数出 2 封带验证码的信，实际 %d", inbox.CodeCount)
	}
	// 最新的在最前——打开这一页就是为了看刚到的信。
	if inbox.Messages[0].Subject != "本周推荐" {
		t.Fatalf("第一封应是最新的那封，实际 %q", inbox.Messages[0].Subject)
	}
	if inbox.Messages[0].Code != "" {
		t.Fatalf("没有验证码的信不该凭空有验证码：%q", inbox.Messages[0].Code)
	}
	if inbox.Messages[1].Code != "654321" {
		t.Fatalf("验证码应被当场提取出来，实际 %q", inbox.Messages[1].Code)
	}
	if inbox.Messages[1].MailboxEmail != "alias@qq.com" {
		t.Fatalf("每封信应带上是发给哪个地址的，实际 %q", inbox.Messages[1].MailboxEmail)
	}
	// 每个地址各记最近一次验证码。
	for _, summary := range inbox.Mailboxes {
		switch summary.Email {
		case "12345@qq.com":
			if summary.LastCode != "123456" {
				t.Fatalf("母号地址的最近验证码应为 123456，实际 %q", summary.LastCode)
			}
		case "alias@qq.com":
			if summary.LastCode != "654321" {
				t.Fatalf("别名的最近验证码应为 654321，实际 %q", summary.LastCode)
			}
		default:
			t.Fatalf("出现了意料之外的地址：%q", summary.Email)
		}
	}
}

// 一个只收过广告信的地址不该有「最近验证码」。
func TestAccountInboxLeavesLastCodeEmptyWithoutCodes(t *testing.T) {
	service := newIMAPFixture(t)
	accountID := createIMAPAccount(t, service, "163", "someone@163.com", "code")
	mailbox, _ := service.store.FindMailboxByEmail("someone@163.com")
	seedMessage(t, service, mailbox.ID, "m1", "订阅确认", "感谢订阅", time.Now().UTC())

	inbox, err := service.AccountInbox(accountID, 0)
	if err != nil {
		t.Fatalf("聚合失败：%v", err)
	}
	if inbox.CodeCount != 0 {
		t.Fatalf("没有验证码时应为 0，实际 %d", inbox.CodeCount)
	}
	if inbox.Mailboxes[0].LastCode != "" || !inbox.Mailboxes[0].LastCodeAt.IsZero() {
		t.Fatalf("不该有最近验证码：%q / %v", inbox.Mailboxes[0].LastCode, inbox.Mailboxes[0].LastCodeAt)
	}
}

// limit 只截断返回的列表，Total 仍要如实反映全量——
// 否则界面会以为「就这么多信」。
func TestAccountInboxLimitKeepsTotalHonest(t *testing.T) {
	service := newIMAPFixture(t)
	accountID := createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	mailbox, _ := service.store.FindMailboxByEmail("12345@qq.com")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		seedMessage(t, service, mailbox.ID, string(rune('a'+i)), "标题", "正文", base.Add(time.Duration(i)*time.Minute))
	}
	inbox, err := service.AccountInbox(accountID, 2)
	if err != nil {
		t.Fatalf("聚合失败：%v", err)
	}
	if len(inbox.Messages) != 2 {
		t.Fatalf("limit=2 应只返回 2 封，实际 %d 封", len(inbox.Messages))
	}
	if inbox.Total != 5 {
		t.Fatalf("Total 应如实反映全量 5，实际 %d", inbox.Total)
	}
}

// 超过上限的 limit 收敛到上限，而不是无限返回。
func TestAccountInboxClampsOversizedLimit(t *testing.T) {
	service := newIMAPFixture(t)
	accountID := createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	mailbox, _ := service.store.FindMailboxByEmail("12345@qq.com")
	seedMessage(t, service, mailbox.ID, "m1", "标题", "正文", time.Now().UTC())

	inbox, err := service.AccountInbox(accountID, AccountInboxMaxMessages+5000)
	if err != nil {
		t.Fatalf("聚合失败：%v", err)
	}
	if len(inbox.Messages) != 1 {
		t.Fatalf("超大 limit 不该改变结果，实际 %d 封", len(inbox.Messages))
	}
}

// 名下还没有地址的母号应返回空列表而不是报错：
// 刚建好的母号就是这个状态，界面要能正常显示。
func TestAccountInboxWithoutAddressesIsEmptyNotError(t *testing.T) {
	service := newIMAPFixture(t)
	account, _, err := service.store.SaveIMAPAccount(domain.AppleAccount{
		Provider: domain.ProviderIMAP, Preset: "qq", AppleID: "empty@qq.com",
		IMAPHost: "imap.qq.com", IMAPPort: 993, Password: "code",
	})
	if err != nil {
		t.Fatalf("建母号失败：%v", err)
	}
	inbox, err := service.AccountInbox(account.ID, 0)
	if err != nil {
		t.Fatalf("没有地址不该报错：%v", err)
	}
	if len(inbox.Mailboxes) != 0 || len(inbox.Messages) != 0 || inbox.Total != 0 {
		t.Fatalf("应返回空，实际 %+v", inbox)
	}
}

func TestAccountInboxRejectsUnknownAccount(t *testing.T) {
	service := newIMAPFixture(t)
	if _, err := service.AccountInbox("acc_not_exist", 0); err == nil {
		t.Fatal("不存在的母号应报错")
	}
	if _, err := service.AccountInbox("   ", 0); err == nil {
		t.Fatal("空白标识应报错")
	}
}

// 面板必须对五种服务商一视同仁——它靠的就是「母号名下有哪些地址」这个
// 与服务商无关的事实。这里用一个 Microsoft 母号来证明它不认服务商。
func TestAccountInboxWorksForNonIMAPAccounts(t *testing.T) {
	service := newIMAPFixture(t)
	accountID := seedMicrosoftAccount(t, service, "someone@outlook.com")
	mailbox, _, err := service.store.UpsertMailboxFromRemote(accountID, domain.RemoteMailbox{
		Provider: domain.ProviderMicrosoft, Email: "someone@outlook.com",
		Label: "someone@outlook.com", IsActive: true,
	}, "")
	if err != nil {
		t.Fatalf("放置邮箱失败：%v", err)
	}
	seedMessage(t, service, mailbox.ID, "m1", "登录验证码", "你的验证码是 246810", time.Now().UTC())

	inbox, err := service.AccountInbox(accountID, 0)
	if err != nil {
		t.Fatalf("聚合失败：%v", err)
	}
	if inbox.Provider != domain.ProviderMicrosoft {
		t.Fatalf("应如实报出服务商，实际 %q", inbox.Provider)
	}
	if inbox.CodeCount != 1 || inbox.Messages[0].Code != "246810" {
		t.Fatalf("非 IMAP 母号也该能聚合出验证码：%+v", inbox)
	}
}

// 母号的标签为空时用邮箱地址兜底，界面标题不该是空的。
func TestAccountInboxFallsBackToEmailForLabel(t *testing.T) {
	service := newIMAPFixture(t)
	accountID := createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	inbox, err := service.AccountInbox(accountID, 0)
	if err != nil {
		t.Fatalf("聚合失败：%v", err)
	}
	if inbox.Label != "12345@qq.com" || inbox.Email != "12345@qq.com" {
		t.Fatalf("标签应回落到地址，实际 %q / %q", inbox.Label, inbox.Email)
	}
	if inbox.Provider != domain.ProviderIMAP {
		t.Fatalf("服务商应为 imap，实际 %q", inbox.Provider)
	}
}

// 两个母号的邮件不能互相串台：聚合的边界是母号，不是整个库。
func TestAccountInboxDoesNotLeakAcrossAccounts(t *testing.T) {
	service := newIMAPFixture(t)
	first := createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	second := createIMAPAccount(t, service, "qq", "99999@qq.com", "code")
	other, _ := service.store.FindMailboxByEmail("99999@qq.com")
	seedMessage(t, service, other.ID, "m1", "别人的信", "验证码 111111", time.Now().UTC())

	inbox, err := service.AccountInbox(first, 0)
	if err != nil {
		t.Fatalf("聚合失败：%v", err)
	}
	if inbox.Total != 0 || inbox.CodeCount != 0 {
		t.Fatalf("不该看到另一个母号的邮件：%+v", inbox)
	}
	_ = second
}
