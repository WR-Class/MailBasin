package mailbox

import (
	"context"
	"testing"

	"icloud-privacy-mail-v2/internal/domain"
)

// newIMAPFixture 返回一个探活被替换成「永远成功」的服务。
// 导入路径本身要测的是解析、去重、拒绝覆盖和落库，不是网络；
// 让单元测试真的去拨 imap.qq.com 既慢又不稳定。
func newIMAPFixture(t *testing.T) *Service {
	t.Helper()
	service := newExternalFixture(t)
	service.imapProbe = func(context.Context, string, int, string, string) error { return nil }
	return service
}

// seedMailbox 直接往邮箱池里放一个邮箱，用来构造「已存在」的前置条件。
func seedMailbox(t *testing.T, service *Service, email, provider string) domain.Mailbox {
	t.Helper()
	mailbox, _, err := service.store.UpsertMailboxFromRemote("", domain.RemoteMailbox{
		Provider: provider,
		Email:    email,
		Label:    email,
		IsActive: true,
	}, "")
	if err != nil {
		t.Fatalf("放置前置邮箱失败：%v", err)
	}
	return mailbox
}

// 最常见的形式：一行一个地址。
func TestParseIMAPMailboxLinesAcceptsPlainList(t *testing.T) {
	items, skipped := ParseIMAPMailboxLines("12345@qq.com\nsomeone@163.com\n")
	if len(items) != 2 || skipped != 0 {
		t.Fatalf("应解析出 2 条且无跳过，实际 %d 条 / %d 跳过", len(items), skipped)
	}
	if items[0].Email != "12345@qq.com" || items[1].Email != "someone@163.com" {
		t.Fatalf("地址不正确：%+v", items)
	}
}

// 空行和 # 注释行是排版，不是内容，不该被算成「跳过」。
// 把空行计成跳过会让界面报出「跳过 8 条」这种吓人的数字。
func TestParseIMAPMailboxLinesIgnoresBlanksAndComments(t *testing.T) {
	items, skipped := ParseIMAPMailboxLines("\n# QQ 主号\n12345@qq.com\n\n\n# 163\nsomeone@163.com\n\n")
	if len(items) != 2 {
		t.Fatalf("应解析出 2 条，实际 %d 条：%+v", len(items), items)
	}
	if skipped != 0 {
		t.Fatalf("空行和注释不该计入跳过，实际跳过 %d 条", skipped)
	}
}

// 从表格里复制出来的内容通常是逗号或分号分隔，一行多个地址也要认。
func TestParseIMAPMailboxLinesAcceptsCommaAndSemicolonForms(t *testing.T) {
	items, _ := ParseIMAPMailboxLines("a@qq.com, b@qq.com; c@163.com\td@163.com e@qq.com")
	if len(items) != 5 {
		t.Fatalf("应解析出 5 条，实际 %d 条：%+v", len(items), items)
	}
}

// 重复地址只留一条：同一个地址导入两次不该产生两个邮箱。
func TestParseIMAPMailboxLinesDeduplicates(t *testing.T) {
	items, _ := ParseIMAPMailboxLines("a@qq.com\nA@QQ.COM\na@qq.com")
	if len(items) != 1 {
		t.Fatalf("重复地址应只留一条，实际 %d 条：%+v", len(items), items)
	}
	if items[0].Email != "a@qq.com" {
		t.Fatalf("地址应归一为小写：%q", items[0].Email)
	}
}

// 真正不是地址的内容要如实计入跳过，不能悄悄丢掉。
func TestParseIMAPMailboxLinesCountsSkipped(t *testing.T) {
	items, skipped := ParseIMAPMailboxLines("a@qq.com\n这不是地址\nb@qq.com\n@@@\n")
	if len(items) != 2 {
		t.Fatalf("应解析出 2 条，实际 %d 条", len(items))
	}
	if skipped != 2 {
		t.Fatalf("应跳过 2 条，实际 %d 条", skipped)
	}
}

// 新增的 provider 必须能穿过归一化。
// 若 NormalizeProvider 不认识 imap，所有 IMAP 邮箱会被静默改判成 iCloud，
// 然后进入按账号分组的同步路径——那条路没有账号可以认证。
func TestIMAPProviderSurvivesNormalization(t *testing.T) {
	if got := domain.NormalizeProvider("imap"); got != domain.ProviderIMAP {
		t.Fatalf("imap 应归一为 %q，实际 %q", domain.ProviderIMAP, got)
	}
	if got := domain.NormalizeProvider("IMAP"); got != domain.ProviderIMAP {
		t.Fatalf("大小写不应影响归一化，实际 %q", got)
	}
	mailbox := domain.Mailbox{Provider: domain.ProviderIMAP}
	if got := mailbox.ProviderOrDefault(); got != domain.ProviderIMAP {
		t.Fatalf("ProviderOrDefault 应返回 %q，实际 %q", domain.ProviderIMAP, got)
	}
}

// 预设负责填地址，手填负责覆盖：两者都要成立，「自定义 / 自建」才能用同一套代码。
func TestResolveIMAPCredentialsPresetThenOverride(t *testing.T) {
	host, port, username, secret := ResolveIMAPCredentials("qq", "", 0, " 12345@qq.com ", " code ")
	if host != "imap.qq.com" || port != 993 {
		t.Fatalf("QQ 预设应给出 imap.qq.com:993，实际 %s:%d", host, port)
	}
	if username != "12345@qq.com" || secret != "code" {
		t.Fatalf("账号名和授权码应去掉空白，实际 %q / %q", username, secret)
	}
	host, port, _, _ = ResolveIMAPCredentials("qq", "imap.example.com", 143, "", "")
	if host != "imap.example.com" || port != 143 {
		t.Fatalf("手填的地址和端口应覆盖预设，实际 %s:%d", host, port)
	}
}

// 认不出的预设回落到自定义，而不是回落到某个具体服务商——
// 猜错服务商比明确要求使用者填地址更糟。
func TestResolveIMAPCredentialsUnknownPresetFallsBackToCustom(t *testing.T) {
	host, port, _, _ := ResolveIMAPCredentials("no-such-vendor", "", 0, "", "")
	if host != "" {
		t.Fatalf("未知预设不该给出服务器地址，实际 %q", host)
	}
	if port != 993 {
		t.Fatalf("未知预设应给出默认端口 993，实际 %d", port)
	}
}

// 服务器地址只能是主机名或 IP，带上协议和路径是常见的手误，要当场拒绝。
func TestImportIMAPMailboxesRejectsMalformedHost(t *testing.T) {
	service := newIMAPFixture(t)
	for _, bad := range []string{"https://imap.qq.com", "imap.qq.com/path", "imap qq com", "..", ""} {
		if _, err := service.ImportIMAPMailboxes("custom", bad, 993, "", "code", []IMAPMailboxInput{{Email: "a@qq.com"}}); err == nil {
			t.Fatalf("服务器地址 %q 应被拒绝", bad)
		}
	}
}

// 授权码是唯一的凭据，缺了就没什么可导入的，要在写库前拦住。
func TestImportIMAPMailboxesRejectsMissingSecret(t *testing.T) {
	service := newIMAPFixture(t)
	if _, err := service.ImportIMAPMailboxes("qq", "", 0, "", "   ", []IMAPMailboxInput{{Email: "a@qq.com"}}); err == nil {
		t.Fatal("缺少授权码时应报错")
	}
}

// 不把一个正常收信的邮箱改成走 IMAP 登录。
func TestImportIMAPMailboxesRefusesToHijackExistingMailbox(t *testing.T) {
	service := newIMAPFixture(t)
	seedMailbox(t, service, "taken@icloud.com", domain.ProviderICloud)
	result, err := service.ImportIMAPMailboxes("qq", "", 0, "", "code", []IMAPMailboxInput{{Email: "taken@icloud.com"}})
	if err != nil {
		t.Fatalf("导入不该整体失败：%v", err)
	}
	if result.Failed != 1 || result.Created != 0 {
		t.Fatalf("应拒绝覆盖 iCloud 邮箱，实际 %+v", result)
	}
	mailbox, ok := service.store.FindMailboxByEmail("taken@icloud.com")
	if !ok {
		t.Fatal("原邮箱不该消失")
	}
	if mailbox.ProviderOrDefault() != domain.ProviderICloud {
		t.Fatalf("原邮箱的 provider 不该被改写，实际 %q", mailbox.ProviderOrDefault())
	}
}

// 同一个地址重复导入是更新，不是新建。
func TestImportIMAPMailboxesIsIdempotent(t *testing.T) {
	service := newIMAPFixture(t)
	inputs := []IMAPMailboxInput{{Email: "12345@qq.com"}}
	first, err := service.ImportIMAPMailboxes("qq", "", 0, "12345@qq.com", "code-one", inputs)
	if err != nil {
		t.Fatalf("首次导入失败：%v", err)
	}
	if first.Created != 1 || first.Updated != 0 {
		t.Fatalf("首次导入应新建 1 个，实际 %+v", first)
	}
	second, err := service.ImportIMAPMailboxes("qq", "", 0, "12345@qq.com", "code-two", inputs)
	if err != nil {
		t.Fatalf("二次导入失败：%v", err)
	}
	if second.Created != 0 || second.Updated != 1 {
		t.Fatalf("二次导入应更新 1 个，实际 %+v", second)
	}
	mailbox, ok := service.store.FindMailboxByEmail("12345@qq.com")
	if !ok {
		t.Fatal("邮箱应存在")
	}
	if mailbox.IMAPSecret != "code-two" {
		t.Fatalf("授权码应被新值覆盖，实际 %q", mailbox.IMAPSecret)
	}
}

// 导入的连接信息必须真的落库，而且下游的取码路径能读到它。
// 缺了任何一项，同步时都会退化成「连不上」。
func TestImportIMAPMailboxesStoresConnectionInfo(t *testing.T) {
	service := newIMAPFixture(t)
	if _, err := service.ImportIMAPMailboxes("163", "", 0, "someone@163.com", "sixteen-chars-ok", []IMAPMailboxInput{{Email: "someone@163.com"}}); err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	mailbox, ok := service.store.FindMailboxByEmail("someone@163.com")
	if !ok {
		t.Fatal("邮箱应存在")
	}
	if mailbox.ProviderOrDefault() != domain.ProviderIMAP {
		t.Fatalf("provider 应为 imap，实际 %q", mailbox.ProviderOrDefault())
	}
	if mailbox.IMAPHost != "imap.163.com" || mailbox.IMAPPort != 993 {
		t.Fatalf("服务器信息不正确：%s:%d", mailbox.IMAPHost, mailbox.IMAPPort)
	}
	if mailbox.IMAPUsername != "someone@163.com" {
		t.Fatalf("账号名不正确：%q", mailbox.IMAPUsername)
	}
	if mailbox.IMAPSecret != "sixteen-chars-ok" {
		t.Fatalf("授权码不正确：%q", mailbox.IMAPSecret)
	}
}

// 账号名留空时按该地址本身补：QQ 的 IMAP 账号名必须是完整邮箱地址。
func TestImportIMAPMailboxesDefaultsUsernameToAddress(t *testing.T) {
	service := newIMAPFixture(t)
	if _, err := service.ImportIMAPMailboxes("qq", "", 0, "", "code", []IMAPMailboxInput{{Email: "12345@qq.com"}}); err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	mailbox, _ := service.store.FindMailboxByEmail("12345@qq.com")
	if mailbox.IMAPUsername != "12345@qq.com" {
		t.Fatalf("账号名应回落到地址本身，实际 %q", mailbox.IMAPUsername)
	}
}

// 非 IMAP 邮箱不能进 IMAP 同步路径，否则会拿空的服务器地址去连。
func TestSyncIMAPMailboxRejectsNonIMAPProvider(t *testing.T) {
	service := newIMAPFixture(t)
	seedMailbox(t, service, "other@icloud.com", domain.ProviderICloud)
	mailbox, _ := service.store.FindMailboxByEmail("other@icloud.com")
	if _, err := service.SyncIMAPMailbox(context.Background(), mailbox.ID); err == nil {
		t.Fatal("非 IMAP 邮箱应被拒绝")
	}
}

// 缺少连接信息时给一句能照做的错误，而不是拿空地址去拨号。
func TestSyncIMAPMailboxRejectsMailboxWithoutCredentials(t *testing.T) {
	service := newIMAPFixture(t)
	seedMailbox(t, service, "noimap@qq.com", domain.ProviderIMAP)
	mailbox, _ := service.store.FindMailboxByEmail("noimap@qq.com")
	if _, err := service.SyncIMAPMailbox(context.Background(), mailbox.ID); err == nil {
		t.Fatal("缺少连接信息时应报错")
	}
}
