package mailbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
)

func newExternalFixture(t *testing.T) *Service {
	t.Helper()
	return NewService(config.Config{}, openSyncTestStore(t))
}

// 用户实际粘贴的就是 Markdown 表格，必须能直接解析。
func TestParseExternalMailboxLinesHandlesMarkdownTable(t *testing.T) {
	raw := "序号邮箱地址访问链接\n" +
		"1`tippet_crooked_7j@icloud.com`[https://ic-mail.example.com/api/v1/access/AAA/mailboxes/tippet_crooked_7j@icloud.com/code](https://ic-mail.example.com/api/v1/access/AAA/mailboxes/tippet_crooked_7j@icloud.com/code)\n" +
		"2`labels_rubies.7t@icloud.com`[https://ic-mail.example.com/api/v1/access/BBB/mailboxes/labels_rubies.7t@icloud.com/code](https://ic-mail.example.com/api/v1/access/BBB/mailboxes/labels_rubies.7t@icloud.com/code)\n"
	items, skipped := ParseExternalMailboxLines(raw)
	if len(items) != 2 {
		t.Fatalf("应解析出 2 条，实际 %d（跳过 %d）", len(items), skipped)
	}
	if items[0].Email != "tippet_crooked_7j@icloud.com" {
		t.Fatalf("第一个邮箱不正确：%q", items[0].Email)
	}
	if !strings.Contains(items[0].CodeAPIURL, "/access/AAA/") {
		t.Fatalf("取码地址不正确：%q", items[0].CodeAPIURL)
	}
	if items[1].Email != "labels_rubies.7t@icloud.com" {
		t.Fatalf("第二个邮箱不正确：%q", items[1].Email)
	}
}

func TestParseExternalMailboxLinesAcceptsPlainAndCommaForms(t *testing.T) {
	raw := "a@icloud.com https://host/access/T1/mailboxes/a@icloud.com/code\n" +
		"b@icloud.com,https://host/access/T2/mailboxes/b@icloud.com/code\n" +
		"\n" +
		"| 3 | `c@icloud.com` | https://host/access/T3/mailboxes/c@icloud.com/code |\n"
	items, skipped := ParseExternalMailboxLines(raw)
	if len(items) != 3 {
		t.Fatalf("应解析出 3 条，实际 %d", len(items))
	}
	if skipped != 0 {
		t.Fatalf("不该有跳过行，实际 %d", skipped)
	}
}

func TestParseExternalMailboxLinesCountsSkippedLines(t *testing.T) {
	raw := "只有邮箱没有链接 a@icloud.com\n" +
		"只有链接没有邮箱 https://host/code\n" +
		"完整一条 b@icloud.com https://host/code\n"
	items, skipped := ParseExternalMailboxLines(raw)
	if len(items) != 1 {
		t.Fatalf("应只解析出 1 条，实际 %d", len(items))
	}
	if skipped != 2 {
		t.Fatalf("应报告 2 行跳过，实际 %d", skipped)
	}
}

// 新服务商必须能穿过归一化，否则会被悄悄当成 iCloud 去走 Apple 登录。
func TestExternalProviderSurvivesNormalization(t *testing.T) {
	if got := domain.NormalizeProvider("external"); got != domain.ProviderExternal {
		t.Fatalf("external 应归一为自身，实际 %q", got)
	}
	if got := domain.NormalizeProvider("  EXTERNAL  "); got != domain.ProviderExternal {
		t.Fatalf("大小写和空格应被容忍，实际 %q", got)
	}
	// 未知值仍然落到 iCloud，这条旧行为不能被破坏。
	if got := domain.NormalizeProvider("something-else"); got != domain.ProviderICloud {
		t.Fatalf("未知服务商仍应归一为 iCloud，实际 %q", got)
	}
}

func TestImportExternalMailboxesRejectsMalformedInput(t *testing.T) {
	service := newExternalFixture(t)
	result, err := service.ImportExternalMailboxes([]ExternalMailboxInput{
		{Email: "not-an-email", CodeAPIURL: "https://host/code"},
		{Email: "ok@icloud.com", CodeAPIURL: "ftp://host/code"},
		{Email: "ok2@icloud.com", CodeAPIURL: ""},
	})
	if err != nil {
		t.Fatalf("导入本身不应失败：%v", err)
	}
	if result.Failed != 3 || result.Created != 0 {
		t.Fatalf("三条都应失败：failed=%d created=%d", result.Failed, result.Created)
	}
	for _, item := range result.Items {
		if item.Reason == "" {
			t.Fatalf("失败项必须给出原因：%+v", item)
		}
	}
}

// 重复导入必须幂等：第二次是更新，不是新建。
func TestImportExternalMailboxesIsIdempotent(t *testing.T) {
	service := newExternalFixture(t)
	input := []ExternalMailboxInput{{Email: "dup@icloud.com", CodeAPIURL: "https://host/access/T/mailboxes/dup@icloud.com/code"}}

	first, err := service.ImportExternalMailboxes(input)
	if err != nil {
		t.Fatalf("首次导入失败：%v", err)
	}
	if first.Created != 1 || first.Updated != 0 {
		t.Fatalf("首次应新建 1 个：%+v", first)
	}
	second, err := service.ImportExternalMailboxes(input)
	if err != nil {
		t.Fatalf("再次导入失败：%v", err)
	}
	if second.Created != 0 || second.Updated != 1 {
		t.Fatalf("再次应更新而不是新建：%+v", second)
	}
	count := 0
	for _, mailbox := range service.store.AllMailboxes() {
		if mailbox.Email == "dup@icloud.com" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("邮箱池里应只有 1 条，实际 %d", count)
	}
}

// 已存在的非外部邮箱不能被外部导入覆盖掉。
func TestImportExternalMailboxesRefusesToHijackExistingMailbox(t *testing.T) {
	state := openSyncTestStore(t)
	service := NewService(config.Config{}, state)
	if _, _, err := state.UpsertMailboxFromRemote("", domain.RemoteMailbox{
		Provider: domain.ProviderMicrosoft, Email: "taken@outlook.com", IsActive: true, Origin: "microsoft_alias",
	}, ""); err != nil {
		t.Fatalf("准备已有邮箱失败：%v", err)
	}
	result, err := service.ImportExternalMailboxes([]ExternalMailboxInput{
		{Email: "taken@outlook.com", CodeAPIURL: "https://host/access/T/mailboxes/taken@outlook.com/code"},
	})
	if err != nil {
		t.Fatalf("导入不应整体失败：%v", err)
	}
	if result.Failed != 1 {
		t.Fatalf("应拒绝覆盖：%+v", result)
	}
	mailbox, _ := state.FindMailboxByEmail("taken@outlook.com")
	if mailbox.ProviderOrDefault() != domain.ProviderMicrosoft {
		t.Fatalf("已有服务商被改掉了：%q", mailbox.ProviderOrDefault())
	}
}

// 取码地址必须能原样读回。真正的「落库是密文」由 store 包的
// TestSQLiteEncryptsExternalCodeAPIURL 用原始字节断言——那个包才够得着 db。
func TestExternalCodeURLRoundTripsThroughStore(t *testing.T) {
	service := newExternalFixture(t)
	const endpoint = "https://host/access/SUPERSECRETTOKEN/mailboxes/enc@icloud.com/code"
	if _, err := service.ImportExternalMailboxes([]ExternalMailboxInput{{Email: "enc@icloud.com", CodeAPIURL: endpoint}}); err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	mailbox, ok := service.store.FindMailboxByEmail("enc@icloud.com")
	if !ok {
		t.Fatal("邮箱没有导入")
	}
	// 通过 store 读出来必须是明文（自动解密），证明写入时确实加密了。
	if mailbox.CodeAPIURL != endpoint {
		t.Fatalf("读回的取码地址不正确：%q", mailbox.CodeAPIURL)
	}
}

// 端到端：取到码之后，必须能被现有的取码路径查到。
func TestSyncExternalMailboxStoresCodeWhereTheCodePathFindsIt(t *testing.T) {
	var served bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !served {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": "no_code", "message": "暂未收到验证码", "retryable": true})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
			"email": "sync@icloud.com", "code": "556677", "subject": "验证码", "from": "no-reply@example.com", "message_id": "m-1",
		}})
	}))
	defer server.Close()

	service := newExternalFixture(t)
	if _, err := service.ImportExternalMailboxes([]ExternalMailboxInput{
		{Email: "sync@icloud.com", CodeAPIURL: server.URL + "/code"},
	}); err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	mailbox, _ := service.store.FindMailboxByEmail("sync@icloud.com")

	// 还没有码：同步返回 0，且不报错。
	count, err := service.SyncExternalMailbox(context.Background(), mailbox.ID)
	if err != nil {
		t.Fatalf("没有码时不应报错：%v", err)
	}
	if count != 0 {
		t.Fatalf("没有码时应返回 0，实际 %d", count)
	}

	served = true
	count, err = service.SyncExternalMailbox(context.Background(), mailbox.ID)
	if err != nil {
		t.Fatalf("取码失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("应落库 1 条，实际 %d", count)
	}
	// 再同步一次不应重复落库（同一个 message_id）。
	if again, err := service.SyncExternalMailbox(context.Background(), mailbox.ID); err != nil || again != 0 {
		t.Fatalf("重复同步不应新增：count=%d err=%v", again, err)
	}

	result, found, err := service.CachedCodeWithQuery(mailbox.ID, CodeQuery{})
	if err != nil {
		t.Fatalf("查询验证码失败：%v", err)
	}
	if !found || result.Code != "556677" {
		t.Fatalf("现有取码路径应查到 556677：found=%t result=%+v", found, result)
	}
}

// 同步一个不是外部类型的邮箱必须被挡住，而不是悄悄走错分支。
func TestSyncExternalMailboxRejectsNonExternalProvider(t *testing.T) {
	state := openSyncTestStore(t)
	service := NewService(config.Config{}, state)
	if _, _, err := state.UpsertMailboxFromRemote("", domain.RemoteMailbox{
		Provider: domain.ProviderGoogle, Email: "g@gmail.com", IsActive: true, Origin: "google_primary",
	}, ""); err != nil {
		t.Fatalf("准备邮箱失败：%v", err)
	}
	mailbox, _ := state.FindMailboxByEmail("g@gmail.com")
	if _, err := service.SyncExternalMailbox(context.Background(), mailbox.ID); err == nil {
		t.Fatal("非外部邮箱应被拒绝")
	}
}
