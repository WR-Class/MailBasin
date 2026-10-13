package mailbox

import (
	"context"
	"fmt"
	"strings"
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

// createIMAPAccount 建一个母号并返回它的标识。
// 地址现在一律挂在母号下，所以每条导入相关的测试都要先有一个母号。
func createIMAPAccount(t *testing.T, service *Service, preset, email, secret string) string {
	t.Helper()
	view, _, err := service.CreateIMAPAccount(preset, email, secret, "", 0, "", "", "", nil)
	if err != nil {
		t.Fatalf("建母号失败：%v", err)
	}
	return view.ID
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

// 批量添加最常见的形态：一行一个「邮箱:授权码」，每行是一个独立的母号。
func TestParseIMAPCredentialLinesAcceptsEmailColonSecret(t *testing.T) {
	items, skipped := ParseIMAPCredentialLines(
		"first@example.com:aaaaaaaaaaaaaaaa\nsecond@example.com:bbbbbbbbbbbbbbbb\n")
	if len(items) != 2 || skipped != 0 {
		t.Fatalf("应解析出 2 条且无跳过，实际 %d 条 / %d 跳过", len(items), skipped)
	}
	if items[0].Email != "first@example.com" || items[0].Secret != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("第一条不正确：%+v", items[0])
	}
	if items[1].Secret != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("第二条的授权码不正确：%+v", items[1])
	}
}

// 只在第一个分隔符处切开。授权码自己含冒号时，按所有分隔符切会把密码截断，
// 而截断后的密码看起来只是一次登录失败，使用者根本查不出原因。
func TestParseIMAPCredentialLinesSplitsOnTheFirstSeparatorOnly(t *testing.T) {
	items, _ := ParseIMAPCredentialLines("someone@example.com:aa:bb:cc")
	if len(items) != 1 {
		t.Fatalf("应解析出 1 条，实际 %d 条", len(items))
	}
	if items[0].Secret != "aa:bb:cc" {
		t.Fatalf("授权码应保留后面的冒号，实际 %q", items[0].Secret)
	}
}

// 制表符也算分隔符：从表格里复制出来的就是这种形状。
func TestParseIMAPCredentialLinesAcceptsTabSeparator(t *testing.T) {
	items, _ := ParseIMAPCredentialLines("someone@example.com\tsupersecret")
	if len(items) != 1 || items[0].Secret != "supersecret" {
		t.Fatalf("制表符分隔应能解析，实际 %+v", items)
	}
}

// Google 把 16 位应用专用密码分成四组显示，照着界面抄下来就带空格。
// 留着空格会让每一行都登录失败，所以这种形状必须把空格去掉。
func TestParseIMAPCredentialLinesCompactsGoogleAppPasswordSpaces(t *testing.T) {
	items, _ := ParseIMAPCredentialLines("someone@gmail.com:abcd efgh ijkl mnop")
	if len(items) != 1 {
		t.Fatalf("应解析出 1 条，实际 %d 条", len(items))
	}
	if items[0].Secret != "abcdefghijklmnop" {
		t.Fatalf("应用专用密码里的空格应被去掉，实际 %q", items[0].Secret)
	}
}

// 但只有「去掉空白后正好是 16 位字母数字」才动。自建邮箱的密码真的可以含空格，
// 把它改坏了使用者没有任何办法绕过——这条路必须留着。
func TestParseIMAPCredentialLinesKeepsASpacedPasswordThatIsNotAppPasswordShaped(t *testing.T) {
	items, _ := ParseIMAPCredentialLines("someone@selfhosted.example:my pass word")
	if len(items) != 1 {
		t.Fatalf("应解析出 1 条，实际 %d 条", len(items))
	}
	if items[0].Secret != "my pass word" {
		t.Fatalf("不是应用专用密码的形状就不该去掉空格，实际 %q", items[0].Secret)
	}
}

// 空行和注释是排版，不计入跳过——把空行算进去会报出「跳过 8 行」这种吓人的数字。
func TestParseIMAPCredentialLinesIgnoresBlanksAndComments(t *testing.T) {
	items, skipped := ParseIMAPCredentialLines(
		"\n# 第一批\na@example.com:aaaaaaaaaaaaaaaa\n\n# 第二批\nb@example.com:bbbbbbbbbbbbbbbb\n\n")
	if len(items) != 2 {
		t.Fatalf("应解析出 2 条，实际 %d 条：%+v", len(items), items)
	}
	if skipped != 0 {
		t.Fatalf("空行和注释不该计入跳过，实际跳过 %d 行", skipped)
	}
}

// 真正没法用的行要如实计入跳过，不能悄悄丢掉：漏报会让使用者以为六行都进去了。
func TestParseIMAPCredentialLinesCountsSkipped(t *testing.T) {
	items, skipped := ParseIMAPCredentialLines(
		"a@example.com:aaaaaaaaaaaaaaaa\n" + // 正常
			"没有分隔符的一行\n" + // 没有分隔符
			":只有授权码\n" + // 分隔符前面是空的
			"不是地址:aaaaaaaaaaaaaaaa\n" + // 邮箱不合法
			"b@example.com:\n" + // 授权码是空的
			"c@example.com:cccccccccccccccc\n") // 正常
	if len(items) != 2 {
		t.Fatalf("应解析出 2 条，实际 %d 条：%+v", len(items), items)
	}
	if skipped != 4 {
		t.Fatalf("应跳过 4 行，实际跳过 %d 行", skipped)
	}
}

// 同一个邮箱出现两次只留第一条：重复导入同一个母号是更新不是新建，
// 但把两行都发下去会让界面显示两次探活，白白多连一次服务器。
func TestParseIMAPCredentialLinesDeduplicatesByEmail(t *testing.T) {
	items, _ := ParseIMAPCredentialLines(
		"A@Example.com:aaaaaaaaaaaaaaaa\na@example.com:bbbbbbbbbbbbbbbb")
	if len(items) != 1 {
		t.Fatalf("重复邮箱应只留一条，实际 %d 条：%+v", len(items), items)
	}
	if items[0].Email != "a@example.com" {
		t.Fatalf("邮箱应归一为小写，实际 %q", items[0].Email)
	}
	if items[0].Secret != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("应保留第一条的授权码，实际 %q", items[0].Secret)
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

// 建母号时母号自己的地址必须成为第一个地址。
// QQ 和 163 的收信都发生在主地址的收件箱里，少了这一条母号页面连自己的信都看不到。
func TestCreateIMAPAccountIncludesPrimaryAddress(t *testing.T) {
	service := newIMAPFixture(t)
	view, result, err := service.CreateIMAPAccount("qq", "12345@qq.com", "code", "", 0, "", "", "", nil)
	if err != nil {
		t.Fatalf("建母号失败：%v", err)
	}
	if result.Created != 1 {
		t.Fatalf("应导入母号自身 1 条，实际 %+v", result)
	}
	if view.MailboxCount != 1 || view.Mailboxes[0].Email != "12345@qq.com" {
		t.Fatalf("母号名下应有它自己的地址，实际 %+v", view.Mailboxes)
	}
	if view.ProviderOrDefault() != domain.ProviderIMAP || view.Preset != "qq" {
		t.Fatalf("母号的 provider / preset 不正确：%q / %q", view.ProviderOrDefault(), view.Preset)
	}
}

// 使用者把主地址也粘进地址框时不该出现两条。
func TestCreateIMAPAccountDeduplicatesPrimaryAddress(t *testing.T) {
	service := newIMAPFixture(t)
	view, _, err := service.CreateIMAPAccount("qq", "12345@qq.com", "code", "", 0, "", "", "",
		[]IMAPMailboxInput{{Email: "12345@qq.com"}, {Email: "alias@qq.com"}})
	if err != nil {
		t.Fatalf("建母号失败：%v", err)
	}
	if view.MailboxCount != 2 {
		t.Fatalf("主地址重复粘贴后应仍是 2 条，实际 %d 条：%+v", view.MailboxCount, view.Mailboxes)
	}
}

// 新导入的地址默认不进邮箱池：这批地址的家是母号页面。
func TestImportedIMAPAddressesStayOutOfPoolByDefault(t *testing.T) {
	service := newIMAPFixture(t)
	createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	mailbox, ok := service.store.FindMailboxByEmail("12345@qq.com")
	if !ok {
		t.Fatal("母号地址应存在")
	}
	if !mailbox.PoolHidden {
		t.Fatal("新导入的 IMAP 地址应默认不进邮箱池")
	}
	page := service.store.Mailboxes("", "", "", true, 1, 50)
	for _, item := range page.Items {
		if item.Email == "12345@qq.com" {
			t.Fatal("默认不进池的地址不该出现在邮箱池里")
		}
	}
}

// 但母号页面必须看得到全部，包括不进池的那些——
// 否则那些地址在界面上就再也找不到了。
func TestAccountViewShowsAddressesEvenWhenHiddenFromPool(t *testing.T) {
	service := newIMAPFixture(t)
	accountID := createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	view, ok := service.FindIMAPAccount(accountID)
	if !ok {
		t.Fatal("母号应能查到")
	}
	if view.MailboxCount != 1 || view.PoolCount != 0 {
		t.Fatalf("母号页面应看到 1 条地址、其中 0 条进池，实际 %d / %d", view.MailboxCount, view.PoolCount)
	}
}

// 进池开关是双向的，而且只影响可见性，不动凭据。
func TestSetMailboxPoolVisibilityRoundTrips(t *testing.T) {
	service := newIMAPFixture(t)
	createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	mailbox, _ := service.store.FindMailboxByEmail("12345@qq.com")

	if _, err := service.SetMailboxPoolHidden(mailbox.ID, false); err != nil {
		t.Fatalf("放进邮箱池失败：%v", err)
	}
	page := service.store.Mailboxes("", "", "", true, 1, 50)
	found := false
	for _, item := range page.Items {
		if item.Email == "12345@qq.com" {
			found = true
		}
	}
	if !found {
		t.Fatal("放进池后应能在邮箱池里看到")
	}
	if _, err := service.SetMailboxPoolHidden(mailbox.ID, true); err != nil {
		t.Fatalf("移出邮箱池失败：%v", err)
	}
	restored, _ := service.store.FindMailboxByID(mailbox.ID)
	if !restored.PoolHidden {
		t.Fatal("移出后应重新变为不进池")
	}
	if restored.IMAPSecret != "code" {
		t.Fatalf("改可见性不该动授权码，实际 %q", restored.IMAPSecret)
	}
}

// 重导入不该把使用者手动勾进池的地址又踢出去。
func TestReimportDoesNotResetPoolVisibility(t *testing.T) {
	service := newIMAPFixture(t)
	accountID := createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	mailbox, _ := service.store.FindMailboxByEmail("12345@qq.com")
	if _, err := service.SetMailboxPoolHidden(mailbox.ID, false); err != nil {
		t.Fatalf("放进邮箱池失败：%v", err)
	}
	if _, err := service.AddIMAPAccountAddresses(accountID, []IMAPMailboxInput{{Email: "12345@qq.com"}}); err != nil {
		t.Fatalf("重导入失败：%v", err)
	}
	restored, _ := service.store.FindMailboxByID(mailbox.ID)
	if restored.PoolHidden {
		t.Fatal("重导入不该把已进池的地址重新标成不进池")
	}
}

// 服务器地址只能是主机名或 IP，带上协议和路径是常见的手误，要当场拒绝。
func TestCreateIMAPAccountRejectsMalformedHost(t *testing.T) {
	service := newIMAPFixture(t)
	for _, bad := range []string{"https://imap.qq.com", "imap.qq.com/path", "imap qq com", "..", ""} {
		if _, _, err := service.CreateIMAPAccount("custom", "a@qq.com", "code", bad, 993, "", "", "", nil); err == nil {
			t.Fatalf("服务器地址 %q 应被拒绝", bad)
		}
	}
}

// 授权码是唯一的凭据，缺了就没什么可导入的，要在写库前拦住。
func TestCreateIMAPAccountRejectsMissingSecret(t *testing.T) {
	service := newIMAPFixture(t)
	if _, _, err := service.CreateIMAPAccount("qq", "a@qq.com", "   ", "", 0, "", "", "", nil); err == nil {
		t.Fatal("缺少授权码时应报错")
	}
}

// 不把一个正常收信的邮箱改成走 IMAP 登录。
func TestImportIMAPMailboxesRefusesToHijackExistingMailbox(t *testing.T) {
	service := newIMAPFixture(t)
	seedMailbox(t, service, "taken@icloud.com", domain.ProviderICloud)
	accountID := createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	result, err := service.ImportIMAPMailboxes(accountID, "qq", "imap.qq.com", 993, "12345@qq.com", "code",
		[]IMAPMailboxInput{{Email: "taken@icloud.com"}})
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
	accountID := createIMAPAccount(t, service, "qq", "12345@qq.com", "code-one")
	inputs := []IMAPMailboxInput{{Email: "12345@qq.com"}}
	first, err := service.ImportIMAPMailboxes(accountID, "qq", "imap.qq.com", 993, "12345@qq.com", "code-one", inputs)
	if err != nil {
		t.Fatalf("首次导入失败：%v", err)
	}
	if first.Created != 0 || first.Updated != 1 {
		t.Fatalf("母号自身已在导入时建好，这次应是更新 1 个，实际 %+v", first)
	}
	second, err := service.ImportIMAPMailboxes(accountID, "qq", "imap.qq.com", 993, "12345@qq.com", "code-two", inputs)
	if err != nil {
		t.Fatalf("二次导入失败：%v", err)
	}
	if second.Created != 0 || second.Updated != 1 {
		t.Fatalf("二次导入应仍是更新 1 个，实际 %+v", second)
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
	createIMAPAccount(t, service, "163", "someone@163.com", "sixteen-chars-ok")
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
	createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	mailbox, _ := service.store.FindMailboxByEmail("12345@qq.com")
	if mailbox.IMAPUsername != "12345@qq.com" {
		t.Fatalf("账号名应回落到地址本身，实际 %q", mailbox.IMAPUsername)
	}
}

// 地址必须挂在母号下，否则母号页面看不到它。
func TestImportedAddressesBelongToTheAccount(t *testing.T) {
	service := newIMAPFixture(t)
	accountID := createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	if _, err := service.AddIMAPAccountAddresses(accountID, []IMAPMailboxInput{{Email: "alias@qq.com"}}); err != nil {
		t.Fatalf("追加地址失败：%v", err)
	}
	mailbox, ok := service.store.FindMailboxByEmail("alias@qq.com")
	if !ok {
		t.Fatal("追加的地址应存在")
	}
	if mailbox.AccountID != accountID {
		t.Fatalf("追加的地址应挂在母号下，实际 %q", mailbox.AccountID)
	}
}

// 两个界面靠 preset 分开：QQ 页面不该看到 163 的母号。
func TestIMAPAccountsFilterByPreset(t *testing.T) {
	service := newIMAPFixture(t)
	createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	createIMAPAccount(t, service, "163", "someone@163.com", "code")

	qq := service.IMAPAccounts("qq", "", 1, 0)
	if len(qq.Items) != 1 || qq.Items[0].AppleID != "12345@qq.com" {
		t.Fatalf("QQ 页面应只看到 QQ 母号，实际 %+v", qq.Items)
	}
	if qq.Total != 1 || qq.TotalPages != 1 {
		t.Fatalf("QQ 页的分页计数应为 1/1，实际 total=%d pages=%d", qq.Total, qq.TotalPages)
	}
	mail163 := service.IMAPAccounts("163", "", 1, 0)
	if len(mail163.Items) != 1 || mail163.Items[0].AppleID != "someone@163.com" {
		t.Fatalf("163 页面应只看到 163 母号，实际 %+v", mail163.Items)
	}
	all := service.IMAPAccounts("", "", 1, 0)
	if len(all.Items) != 2 || all.Total != 2 {
		t.Fatalf("不带 preset 应看到全部 2 个，实际 %d 个（total=%d）", len(all.Items), all.Total)
	}
}

// 翻页必须既不重复也不遗漏。
//
// 只看第一页对不对是不够的：OFFSET 算错时第一页永远是对的，错误全落在后面的页上。
// 所以这里把所有页的母号并起来去重，必须一个不多一个不少。
func TestIMAPAccountsPagesCoverEveryAccountExactlyOnce(t *testing.T) {
	service := newIMAPFixture(t)
	const total = 7
	const pageSize = 3
	for index := 0; index < total; index++ {
		createIMAPAccount(t, service, "gmail", fmt.Sprintf("user%02d@gmail.com", index), "abcdefghijklmnop")
	}

	seen := map[string]int{}
	pages := 0
	for page := 1; ; page++ {
		result := service.IMAPAccounts("gmail", "", page, pageSize)
		if result.Total != total {
			t.Fatalf("第 %d 页的 total 应为 %d，实际 %d", page, total, result.Total)
		}
		if result.TotalPages != 3 {
			t.Fatalf("每页 %d 条、共 %d 条时应有 3 页，实际 %d", pageSize, total, result.TotalPages)
		}
		if page == 1 && len(result.Items) != pageSize {
			t.Fatalf("首页条数应为 min(page_size,total)=%d，实际 %d", pageSize, len(result.Items))
		}
		if page == 3 && len(result.Items) != 1 {
			t.Fatalf("末页条数应为 %d，实际 %d", total%pageSize, len(result.Items))
		}
		for _, item := range result.Items {
			seen[item.ID]++
		}
		pages++
		if page >= result.TotalPages {
			break
		}
		if pages > 20 {
			t.Fatalf("页数异常，可能有死循环")
		}
	}
	if len(seen) != total {
		t.Fatalf("逐页并集应有 %d 个不同母号，实际 %d 个", total, len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("母号 %s 在翻页中出现了 %d 次", id, count)
		}
	}
}

// 搜索要能同时命中邮箱和标签，并且和分页一起生效。
func TestIMAPAccountsSearchNarrowsTheTotal(t *testing.T) {
	service := newIMAPFixture(t)
	createIMAPAccount(t, service, "gmail", "alpha@gmail.com", "abcdefghijklmnop")
	createIMAPAccount(t, service, "gmail", "beta@gmail.com", "abcdefghijklmnop")

	if all := service.IMAPAccounts("gmail", "", 1, 0); all.Total != 2 {
		t.Fatalf("不搜索时应看到 2 个，实际 %d", all.Total)
	}
	hit := service.IMAPAccounts("gmail", "alpha", 1, 0)
	if hit.Total != 1 || len(hit.Items) != 1 || hit.Items[0].AppleID != "alpha@gmail.com" {
		t.Fatalf("搜索 alpha 应只命中 1 个，实际 total=%d items=%+v", hit.Total, hit.Items)
	}
	if miss := service.IMAPAccounts("gmail", "nobody", 1, 0); miss.Total != 0 || len(miss.Items) != 0 {
		t.Fatalf("搜不到时应为 0，实际 total=%d items=%d", miss.Total, len(miss.Items))
	}
}

// 邮箱池里点同步，IMAP 邮箱不该走到 iCloud 那条路上去。
//
// 少了 IMAP 分支时它会掉进 iCloud 路径，报「对应 Apple 账号登录态不存在」——而 IMAP
// 母号根本没有 Apple 账号，使用者照着这个提示查永远查不出问题。这里把服务器指向本机
// 一个没人监听的端口：连接立刻被拒绝，测试既不联网也不等超时；断言的是「走没走对路」，
// 不是「连没连上」。
func TestIMAPMailboxSyncDoesNotClaimAnAppleSession(t *testing.T) {
	service := newIMAPFixture(t)
	view, _, err := service.CreateIMAPAccount("gmail", "someone@gmail.com", "abcdefghijklmnop", "127.0.0.1", 1, "", "", "", nil)
	if err != nil {
		t.Fatalf("建母号失败：%v", err)
	}
	mailboxes := service.store.MailboxesByAccount(view.ID)
	if len(mailboxes) == 0 {
		t.Fatalf("母号下应至少有一个地址")
	}
	result, err := service.SyncMailboxMessages(context.Background(), mailboxes[0].ID)
	if err != nil && strings.Contains(err.Error(), "Apple 账号登录态不存在") {
		t.Fatalf("IMAP 邮箱同步不该报 Apple 登录态不存在：%v", err)
	}
	if len(result.Accounts) == 0 {
		t.Fatalf("同步结果里应有一个母号，实际 %+v（err=%v）", result.Accounts, err)
	}
	if result.Accounts[0].Method != "imap" {
		t.Fatalf("IMAP 邮箱应走 imap 这条同步路径，实际 method=%q", result.Accounts[0].Method)
	}
}

// 授权码留空的重导入不该把已存的凭据清空。
func TestSaveIMAPAccountKeepsSecretWhenOmitted(t *testing.T) {
	service := newIMAPFixture(t)
	accountID := createIMAPAccount(t, service, "qq", "12345@qq.com", "original-code")
	if _, _, err := service.store.SaveIMAPAccount(domain.AppleAccount{
		Provider: domain.ProviderIMAP, Preset: "qq", AppleID: "12345@qq.com",
		IMAPHost: "imap.qq.com", IMAPPort: 993,
	}); err != nil {
		t.Fatalf("重存失败：%v", err)
	}
	account, ok := service.store.FindAppleAccount(accountID)
	if !ok {
		t.Fatal("母号应还在")
	}
	if account.Password != "original-code" {
		t.Fatalf("授权码不该被空值清掉，实际 %q", account.Password)
	}
}

// 删除母号要连带清掉它的地址和邮件，不留孤儿。
func TestDeleteIMAPAccountRemovesAddresses(t *testing.T) {
	service := newIMAPFixture(t)
	accountID := createIMAPAccount(t, service, "qq", "12345@qq.com", "code")
	if _, err := service.DeleteIMAPAccount(accountID); err != nil {
		t.Fatalf("删除母号失败：%v", err)
	}
	if _, ok := service.store.FindMailboxByEmail("12345@qq.com"); ok {
		t.Fatal("母号删除后它的地址不该还在")
	}
	if _, ok := service.store.FindAppleAccount(accountID); ok {
		t.Fatal("母号本身应被删除")
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

// 没有地址的母号不能去同步，也不该把状态标成正常。
func TestSyncIMAPAccountRejectsAccountWithoutAddresses(t *testing.T) {
	service := newIMAPFixture(t)
	account, _, err := service.store.SaveIMAPAccount(domain.AppleAccount{
		Provider: domain.ProviderIMAP, Preset: "qq", AppleID: "empty@qq.com",
		IMAPHost: "imap.qq.com", IMAPPort: 993, Password: "code",
	})
	if err != nil {
		t.Fatalf("建母号失败：%v", err)
	}
	if _, err := service.SyncIMAPAccount(context.Background(), account.ID); err == nil {
		t.Fatal("没有地址的母号应拒绝同步")
	}
}

// 不属于 IMAP 的母号标识不能走 IMAP 母号的同步路径——
// 那会拿别的服务商的账号去连一个空的服务器地址。
func TestSyncIMAPAccountRejectsNonIMAPAccountID(t *testing.T) {
	service := newIMAPFixture(t)
	accountID := seedMicrosoftAccount(t, service, "x@outlook.com")
	if _, err := service.SyncIMAPAccount(context.Background(), accountID); err == nil {
		t.Fatal("Microsoft 母号不该能走 IMAP 母号的同步路径")
	}
	if _, err := service.SyncIMAPAccount(context.Background(), "acc_not_exist"); err == nil {
		t.Fatal("不存在的母号应拒绝同步")
	}
}

// seedMicrosoftAccount 放一个别的服务商的母号，用来验证 IMAP 路径不会误接它。
func seedMicrosoftAccount(t *testing.T, service *Service, email string) string {
	t.Helper()
	session, err := service.store.SaveMicrosoftSession(domain.MicrosoftSession{Email: email})
	if err != nil {
		t.Fatalf("放置 Microsoft 母号失败：%v", err)
	}
	return session.AccountID
}
