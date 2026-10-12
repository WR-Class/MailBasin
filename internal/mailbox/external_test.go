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
// 这是抽奖平台实际发的奖品文件格式，一个文件一个奖品。
func TestParseExternalMailboxLinesAcceptsLotteryPrizeJSON(t *testing.T) {
	raw := `{
  "value": "tippet_crooked_7j@icloud.com----https://ic-mail.example.com/api/v1/access/TOKEN/mailboxes/tippet_crooked_7j@icloud.com/code"
}`
	items, skipped := ParseExternalMailboxLines(raw)
	if len(items) != 1 || skipped != 0 {
		t.Fatalf("应解析出 1 条且无跳过，实际 %d 条 / %d 跳过", len(items), skipped)
	}
	if items[0].Email != "tippet_crooked_7j@icloud.com" {
		t.Fatalf("邮箱不正确：%q", items[0].Email)
	}
	// 关键：取码地址必须是干净的那一半，不能把邮箱或分隔符带进去。
	if !strings.HasPrefix(items[0].CodeAPIURL, "https://ic-mail.example.com/api/v1/access/TOKEN/") {
		t.Fatalf("取码地址不正确：%q", items[0].CodeAPIURL)
	}
	if strings.Contains(items[0].CodeAPIURL, "----") || strings.Contains(items[0].CodeAPIURL, "@icloud.com----") {
		t.Fatalf("取码地址里残留了分隔符或邮箱：%q", items[0].CodeAPIURL)
	}
}

// 多个奖品文件的内容拼在一起也要能一次导入。
func TestParseExternalMailboxLinesAcceptsJSONArray(t *testing.T) {
	raw := `[
  {"value": "a@icloud.com----https://host/access/T1/mailboxes/a@icloud.com/code"},
  {"value": "b@icloud.com----https://host/access/T2/mailboxes/b@icloud.com/code"},
  {"value": "c@icloud.com----https://host/access/T3/mailboxes/c@icloud.com/code"}
]`
	items, skipped := ParseExternalMailboxLines(raw)
	if len(items) != 3 || skipped != 0 {
		t.Fatalf("应解析出 3 条，实际 %d 条 / %d 跳过", len(items), skipped)
	}
	if items[0].Email != "a@icloud.com" || items[2].Email != "c@icloud.com" {
		t.Fatalf("顺序或内容不对：%+v", items)
	}
}

// 发奖平台换个字段名不该让导入失效。
func TestParseExternalMailboxLinesDoesNotHardcodeTheValueField(t *testing.T) {
	for _, raw := range []string{
		`{"prize": "x@icloud.com----https://host/access/T/mailboxes/x@icloud.com/code"}`,
		`{"data": {"code_url": "x@icloud.com----https://host/access/T/mailboxes/x@icloud.com/code"}}`,
		`{"a": ["x@icloud.com----https://host/access/T/mailboxes/x@icloud.com/code"]}`,
	} {
		items, _ := ParseExternalMailboxLines(raw)
		if len(items) != 1 || items[0].Email != "x@icloud.com" {
			t.Fatalf("字段名换掉后解析失败：%s -> %+v", raw, items)
		}
	}
}

// 以花括号开头但不是合法 JSON 时，必须退回按文本处理，而不是整段丢掉。
func TestParseExternalMailboxLinesFallsBackWhenBracesAreNotJSON(t *testing.T) {
	raw := "{备注} a@icloud.com https://host/access/T/mailboxes/a@icloud.com/code"
	items, _ := ParseExternalMailboxLines(raw)
	if len(items) != 1 || items[0].Email != "a@icloud.com" {
		t.Fatalf("非法 JSON 应退回文本解析：%+v", items)
	}
}

// 分隔符一旦出现就按机器格式严格判：左侧混进别的东西时整条丢掉，
// 而不是猜出里面的邮箱——猜错会导入一个错的地址，而且没人会注意到。
func TestParseExternalMailboxLinesRejectsJunkAroundTheSeparator(t *testing.T) {
	raw := `{"value": "{备注} a@icloud.com----https://host/access/T/mailboxes/a@icloud.com/code"}`
	items, skipped := ParseExternalMailboxLines(raw)
	if len(items) != 0 || skipped != 1 {
		t.Fatalf("分隔符两侧不干净时应整条跳过：%+v（跳过 %d）", items, skipped)
	}
}

// 分隔符写法不能把原来的 Markdown 表格和纯文本写法弄坏。
func TestParseExternalMailboxLinesSeparatorDoesNotBreakOtherForms(t *testing.T) {
	raw := "a@icloud.com https://host/access/T1/mailboxes/a@icloud.com/code\n" +
		"| 2 | `b@icloud.com` | [链接](https://host/access/T2/mailboxes/b@icloud.com/code) |\n" +
		"c@icloud.com----https://host/access/T3/mailboxes/c@icloud.com/code\n"
	items, skipped := ParseExternalMailboxLines(raw)
	if len(items) != 3 || skipped != 0 {
		t.Fatalf("三种写法应都认，实际 %d 条 / %d 跳过", len(items), skipped)
	}
	for _, item := range items {
		if !strings.HasPrefix(item.CodeAPIURL, "https://host/access/") {
			t.Fatalf("取码地址被弄脏了：%q", item.CodeAPIURL)
		}
	}
}

// 分隔符出现在但两侧不成对时，不能硬拆出一条垃圾记录。
func TestParseExternalMailboxLinesRejectsHalfPair(t *testing.T) {
	for _, raw := range []string{
		`{"value": "只有邮箱 a@icloud.com----"}`,
		`{"value": "----https://host/access/T/mailboxes/a@icloud.com/code"}`,
		`{"value": "not-an-email----https://host/code"}`,
	} {
		items, skipped := ParseExternalMailboxLines(raw)
		if len(items) != 0 || skipped != 1 {
			t.Fatalf("半条记录应被跳过：%s -> %d 条 / %d 跳过", raw, len(items), skipped)
		}
	}
}

// 选中多个奖品文件时，内容是首尾相接的多个 JSON 文档，而不是一个数组。
// 这是实际用起来最容易踩的形态：整段 Unmarshal 会失败并让导入全部报废。
func TestParseExternalMailboxLinesAcceptsConcatenatedPrizeDocuments(t *testing.T) {
	raw := `{
  "value": "tippet_crooked_7j@icloud.com----https://host/access/T1/mailboxes/tippet_crooked_7j@icloud.com/code"
}
{
  "value": "labels_rubies.7t@icloud.com----https://host/access/T2/mailboxes/labels_rubies.7t@icloud.com/code"
}
{
  "value": "gorge-layman.0i@icloud.com----https://host/access/T3/mailboxes/gorge-layman.0i@icloud.com/code"
}`
	items, skipped := ParseExternalMailboxLines(raw)
	if len(items) != 3 || skipped != 0 {
		t.Fatalf("拼接的奖品文档应解析出 3 条，实际 %d 条 / %d 跳过", len(items), skipped)
	}
	// 顺序按文件顺序，方便人对照。
	if items[0].Email != "tippet_crooked_7j@icloud.com" || items[2].Email != "gorge-layman.0i@icloud.com" {
		t.Fatalf("顺序或内容不对：%+v", items)
	}
}

// 尾部混进垃圾时，前面已经读出来的文档仍然要保住。
func TestParseExternalMailboxLinesKeepsValidDocumentsBeforeTrailingJunk(t *testing.T) {
	raw := `{"value": "a@icloud.com----https://host/access/T1/mailboxes/a@icloud.com/code"}
<<<这里不是 JSON>>>
{"value": "b@icloud.com----https://host/access/T2/mailboxes/b@icloud.com/code"}`
	items, _ := ParseExternalMailboxLines(raw)
	if len(items) != 1 || items[0].Email != "a@icloud.com" {
		t.Fatalf("应保住垃圾之前的那一条：%+v", items)
	}
}

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

// 回归测试：邮箱和链接直接粘在一起，既没有 ---- 也没有空格。
//
// 修之前这里会产出 "labels_rubies.7t@icloud.comhttps"：邮箱正则的顶级域部分
// `\.[A-Za-z]{2,}` 不会在 `com` 处停下——`comhttps` 全是字母，它一口气吞下去。
// 更糟的是跳过计数是 0，坏地址被静默存进库里，没人看得出来。
func TestParseExternalMailboxLinesHandlesAddressGluedToURL(t *testing.T) {
	const want = "labels_rubies.7t@icloud.com"
	const url = "https://ic-mail.example.com/api/v1/access/AAA/mailboxes/labels_rubies.7t@icloud.com/code"
	items, skipped := ParseExternalMailboxLines(want + url)
	if len(items) != 1 {
		t.Fatalf("应解析出 1 条，实际 %d（跳过 %d）", len(items), skipped)
	}
	if items[0].Email != want {
		t.Fatalf("邮箱被链接污染了：应为 %q，实际 %q", want, items[0].Email)
	}
	if strings.HasSuffix(items[0].Email, "https") {
		t.Fatalf("邮箱尾巴上挂着链接的开头：%q", items[0].Email)
	}
	if items[0].CodeAPIURL != url {
		t.Fatalf("取码地址不正确：%q", items[0].CodeAPIURL)
	}
	if skipped != 0 {
		t.Fatalf("这一行是能解析的，不该计入跳过，实际 %d", skipped)
	}
}

// 带连字符和下划线的地址最容易在粘连形状上踩到，单独钉一条。
func TestParseExternalMailboxLinesHandlesHyphenatedAddressGluedToURL(t *testing.T) {
	const want = "gorge-layman.0i@icloud.com"
	const url = "https://host/access/BBB/mailboxes/gorge-layman.0i@icloud.com/code"
	items, skipped := ParseExternalMailboxLines(want + url)
	if len(items) != 1 || skipped != 0 {
		t.Fatalf("应解析出 1 条且不跳过，实际 %d 条 / 跳过 %d", len(items), skipped)
	}
	if items[0].Email != want {
		t.Fatalf("邮箱不正确：应为 %q，实际 %q", want, items[0].Email)
	}
}

// 四种真实输入形状必须给出同一个邮箱。只测修好的那一种不算验证——
// 修一个形状而弄坏另一个，是这类改动最常见的失败方式。
func TestParseExternalMailboxLinesAllRealShapesAgreeOnTheAddress(t *testing.T) {
	const want = "labels_rubies.7t@icloud.com"
	const url = "https://ic-mail.example.com/api/v1/access/AAA/mailboxes/labels_rubies.7t@icloud.com/code"
	shapes := map[string]string{
		"分隔符":      want + "----" + url,
		"空格":       want + " " + url,
		"粘连":       want + url,
		"Markdown": "| " + want + " | " + url + " |",
	}
	for name, raw := range shapes {
		items, skipped := ParseExternalMailboxLines(raw)
		if len(items) != 1 {
			t.Fatalf("%s：应解析出 1 条，实际 %d（跳过 %d）", name, len(items), skipped)
		}
		if items[0].Email != want {
			t.Fatalf("%s：邮箱应为 %q，实际 %q", name, want, items[0].Email)
		}
	}
}

// 真的解析不出来的行必须计入跳过数，不能静默产出一个看着像但实际错的字段。
func TestParseExternalMailboxLinesNeverSilentlyAcceptsJunk(t *testing.T) {
	raw := "这是一行完全无关的文字，既没有地址也没有链接\n" +
		"只有链接没有邮箱 https://host/code\n" +
		"完整一条 ok@icloud.com https://host/access/T/mailboxes/ok@icloud.com/code\n"
	items, skipped := ParseExternalMailboxLines(raw)
	if len(items) != 1 {
		t.Fatalf("应只解析出 1 条，实际 %d", len(items))
	}
	if skipped != 2 {
		t.Fatalf("两行坏输入都应被计数，实际跳过 %d", skipped)
	}
	for _, item := range items {
		if strings.Contains(item.Email, "https") {
			t.Fatalf("坏字段被静默接受了：%q", item.Email)
		}
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
