package protocol

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Gmail 与 Outlook.com 共用同一套 + 标签语义，因此边界必须完全一致：
// 同一条输入在两个服务商那里要么都通过，要么都拒绝。
func TestPlusAliasBoundaryIsIdenticalForBothProviders(t *testing.T) {
	const base = "user@gmail.com"
	builders := map[string]func(string, string) (string, error){
		"gmail":     BuildPlusAlias,
		"microsoft": BuildMicrosoftPlusAlias,
	}
	cases := []struct {
		name    string
		tag     string
		wantErr bool
	}{
		{"普通标签", "shop", false},
		{"正好到 59 字符上限", strings.Repeat("a", 59), false},
		{"超一个字符", strings.Repeat("a", 60), true},
		{"含 at 符号", "a@b", true},
		{"含加号", "a+b", true},
		{"含空格", "a b", true},
		{"空标签", "   ", true},
	}
	for name, build := range builders {
		for _, testCase := range cases {
			_, err := build(base, testCase.tag)
			if testCase.wantErr && err == nil {
				t.Fatalf("%s / %s：应当报错却通过了", name, testCase.name)
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("%s / %s：不应报错却失败了：%v", name, testCase.name, err)
			}
		}
	}
}

// 错误码必须带上服务商前缀，否则前端和日志分不清是哪一家的地址被拒了。
func TestPlusAliasErrorCodesCarryProviderPrefix(t *testing.T) {
	_, err := BuildMicrosoftPlusAlias("user@outlook.com", strings.Repeat("a", 60))
	if err == nil {
		t.Fatal("超长标签应当报错")
	}
	if code := ErrorDetailsCode(err); !strings.HasPrefix(code, "microsoft_alias_") {
		t.Fatalf("错误码应带 microsoft_alias_ 前缀：%q", code)
	}
	_, err = BuildPlusAlias("user@gmail.com", strings.Repeat("a", 60))
	if code := ErrorDetailsCode(err); !strings.HasPrefix(code, "google_alias_") {
		t.Fatalf("错误码应带 google_alias_ 前缀：%q", code)
	}
}

// 实测确认过：Outlook 会保留标签，所以微软的构造结果必须和谷歌同构。
func TestBuildMicrosoftPlusAliasProducesExpectedAddress(t *testing.T) {
	email, err := BuildMicrosoftPlusAlias("qiaobaochong823680@outlook.com", "MBSTest")
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if email != "qiaobaochong823680+mbstest@outlook.com" {
		t.Fatalf("地址不正确（标签应统一转小写）：%q", email)
	}
	// 主地址本身已带 + 时应替换而不是叠加。
	replaced, err := BuildMicrosoftPlusAlias("user+old@outlook.com", "new")
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if replaced != "user+new@outlook.com" {
		t.Fatalf("应替换已有标签而不是叠加：%q", replaced)
	}
}

func TestBuildMicrosoftPlusAliasRejectsBadInput(t *testing.T) {
	if _, err := BuildMicrosoftPlusAlias("", "shop"); err == nil {
		t.Fatal("缺少主地址时应报错")
	}
	if _, err := BuildMicrosoftPlusAlias("not-an-address", "shop"); err == nil {
		t.Fatal("主地址格式不正确时应报错")
	}
}

// 锁死线上那个真实现象：一封发到 + 标签地址的邮件，
// 只有那个 + 地址已经进了邮箱池才会被认领。
//
// 背景：实测确认 Outlook 会把 +mbstest 原样放进 Graph 的 toRecipients，
// 所以邮件确实被扫到了；但路由是精确匹配收件人——池里只有主地址时，
// 这封信会被计入 Unmatched，而不是退而投进主地址的邮箱。
func TestSyncMailboxesClaimsPlusAddressOnlyWhenItIsInThePool(t *testing.T) {
	const plusAddress = "qiaobaochong823680+mbstest@outlook.com"
	client := newTestGraphClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []any{
				graphMessageFixture("m1", "1111", "2026-01-01T10:00:00Z", "text", "123456", []string{plusAddress}, nil),
			},
		})
	})
	options := MicrosoftMailSyncOptions{Limit: 50}

	// 池里只有主地址：扫得到，但没人认领。
	withoutPlus, err := client.SyncMailboxes(context.Background(), "test-token", []Mailbox{
		{ID: "box-primary", Email: "qiaobaochong823680@outlook.com"},
	}, options)
	if err != nil {
		t.Fatalf("同步失败：%v", err)
	}
	if withoutPlus.Unmatched != 1 || withoutPlus.Matched != 0 {
		t.Fatalf("池里没有 + 地址时应计为未匹配：matched=%d unmatched=%d",
			withoutPlus.Matched, withoutPlus.Unmatched)
	}
	if got := len(withoutPlus.MessagesByMailbox["box-primary"]); got != 0 {
		t.Fatalf("邮件不应被投进主地址的邮箱，实际 %d 封", got)
	}

	// 把 + 地址加进池子之后，同一封信就能被认领。
	withPlus, err := client.SyncMailboxes(context.Background(), "test-token", []Mailbox{
		{ID: "box-primary", Email: "qiaobaochong823680@outlook.com"},
		{ID: "box-plus", Email: plusAddress},
	}, options)
	if err != nil {
		t.Fatalf("同步失败：%v", err)
	}
	if withPlus.Matched != 1 || withPlus.Unmatched != 0 {
		t.Fatalf("池里有 + 地址时应匹配：matched=%d unmatched=%d", withPlus.Matched, withPlus.Unmatched)
	}
	messages := withPlus.MessagesByMailbox["box-plus"]
	if len(messages) != 1 {
		t.Fatalf("+ 地址的邮箱应收 1 封，实际 %d 封", len(messages))
	}
	if messages[0].ToRecipients[0] != plusAddress {
		t.Fatalf("收件人应原样保留 + 标签：%q", messages[0].ToRecipients[0])
	}
	// 主地址的邮箱不应被牵连——一封邮件只归属第一个命中的邮箱。
	if got := len(withPlus.MessagesByMailbox["box-primary"]); got != 0 {
		t.Fatalf("主地址邮箱不应收到这封信，实际 %d 封", got)
	}
}
