package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func newTestGmailClient(t *testing.T, handler http.HandlerFunc) *GmailClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewGmailClient("")
	client.baseURL = server.URL
	return client
}

// gmailMessageFixture 造一封 Gmail API 形状的邮件。
// payload 的头是平铺的（真实 Gmail 就是平铺在顶层 payload 上），
// 正文放在 text/plain 与 text/html 两个子部件里，和真实的多部分邮件一致。
func gmailMessageFixture(id, subject, internalDate, deliveredTo string, to []string, html, plain string) map[string]any {
	parts := []map[string]any{}
	if plain != "" {
		parts = append(parts, map[string]any{
			"partId":   "0",
			"mimeType": "text/plain",
			"body":     map[string]any{"size": len(plain), "data": base64.RawURLEncoding.EncodeToString([]byte(plain))},
		})
	}
	if html != "" {
		parts = append(parts, map[string]any{
			"partId":   "1",
			"mimeType": "text/html",
			"body":     map[string]any{"size": len(html), "data": base64.RawURLEncoding.EncodeToString([]byte(html))},
		})
	}
	headers := []map[string]any{
		{"name": "Subject", "value": subject},
		{"name": "From", "value": "Sender <sender@example.com>"},
	}
	// 真实的 Gmail 把多个收件人放在同一个 To 头里，逗号分隔。
	if len(to) > 0 {
		headers = append(headers, map[string]any{"name": "To", "value": strings.Join(to, ", ")})
	}
	if deliveredTo != "" {
		headers = append(headers, map[string]any{"name": "Delivered-To", "value": deliveredTo})
	}
	return map[string]any{
		"id":           id,
		"threadId":     "thread-" + id,
		"labelIds":     []string{"INBOX"},
		"snippet":      "snippet text",
		"internalDate": internalDate,
		"payload": map[string]any{
			"partId":   "",
			"mimeType": "multipart/alternative",
			"headers":  headers,
			"parts":    parts,
		},
	}
}

// 授权链接是这套集成里唯一一个"用户直接看到"的产物，任何一项缺失都会让授权失败，
// 而且失败现场在浏览器里，排查成本很高。所以逐项钉死。
func TestGoogleAuthFlowBuildsPKCELoopbackURL(t *testing.T) {
	flow, err := NewGoogleAuthFlow("client-123.apps.googleusercontent.com", "http://127.0.0.1:8788/api/google/callback")
	if err != nil {
		t.Fatalf("创建授权流程失败：%v", err)
	}
	if flow.State == "" || flow.CodeVerifier == "" {
		t.Fatalf("state 与 verifier 都必须生成：%+v", flow)
	}
	if flow.ExpiresAt.Before(time.Now()) {
		t.Fatal("授权流程不应一创建就过期")
	}

	parsed, err := url.Parse(flow.AuthURL)
	if err != nil {
		t.Fatalf("授权链接不是合法 URL：%v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "accounts.google.com" {
		t.Fatalf("授权端点不正确：%s", flow.AuthURL)
	}
	query := parsed.Query()
	checks := map[string]string{
		"client_id":             "client-123.apps.googleusercontent.com",
		"redirect_uri":          "http://127.0.0.1:8788/api/google/callback",
		"response_type":         "code",
		"code_challenge_method": "S256",
		// access_type=offline 是拿到 refresh token 的前提；
		// prompt=consent 保证重复授权时 Google 仍会回传 refresh token。
		"access_type": "offline",
		"prompt":      "consent",
	}
	for key, want := range checks {
		if got := query.Get(key); got != want {
			t.Fatalf("授权链接参数 %s 应为 %q，实际 %q", key, want, got)
		}
	}
	if query.Get("state") != flow.State {
		t.Fatal("授权链接里的 state 与流程记录不一致，回调将无法校验")
	}
	if query.Get("scope") != GoogleOAuthScope {
		t.Fatalf("授权范围不正确：%q", query.Get("scope"))
	}
	if query.Get("code_challenge") != codeChallenge(flow.CodeVerifier) {
		t.Fatal("授权链接里的 code_challenge 与 verifier 不匹配，换码必然失败")
	}
}

func TestCodeChallengeIsS256OfVerifier(t *testing.T) {
	verifier := "abcdefghijklmnopqrstuvwxyz0123456789-._~ABCDEFG"
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := codeChallenge(verifier); got != want {
		t.Fatalf("S256 挑战值不正确：want=%s got=%s", want, got)
	}
	// 两个不同的 verifier 不能撞出同一个挑战值（防呆，不是密码学证明）。
	if codeChallenge(verifier) == codeChallenge(verifier+"x") {
		t.Fatal("不同 verifier 产生了相同的挑战值")
	}
}

func TestParseGoogleTokenAppliesExpirySkew(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"access_token":  "at",
		"refresh_token": "rt",
		"expires_in":    3600,
		"scope":         GoogleOAuthScope,
		"token_type":    "Bearer",
	})
	token, err := parseGoogleToken(body)
	if err != nil {
		t.Fatalf("解析令牌失败：%v", err)
	}
	if token.AccessToken != "at" || token.RefreshToken != "rt" {
		t.Fatalf("令牌字段不正确：%+v", token)
	}
	// expires_in=3600 减去 5 分钟提前量，应落在 3300 秒附近。
	// 如果这里读到 3600，说明提前量没生效，过期边界上会发出一批必然 401 的请求。
	remaining := time.Until(token.ExpiresAt)
	if remaining > 3350*time.Second || remaining < 3250*time.Second {
		t.Fatalf("过期时间未按 5 分钟提前量收敛：还有 %v", remaining)
	}
}

func TestParseGoogleTokenRejectsEmptyBody(t *testing.T) {
	if _, err := parseGoogleToken([]byte(`{}`)); err == nil {
		t.Fatal("没有 access_token 时应报错")
	}
	if _, err := parseGoogleToken([]byte(`not json`)); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// 这条用例来自一次真实的踩坑：用"桌面应用"以外的客户端类型去跑设备码流程时，
// Google 返回的是 invalid_client，错误描述里点名的却是**客户端类型**而不是 ID。
// 那条提示是用户唯一能看懂病因的线索，必须保留。
// 这条来自一次真实的线上失败，而且我第一次的诊断是错的，值得完整记下来：
//
// 最初我用一个假 code 探测 token 端点，拿到 invalid_request: client_secret is missing，
// 就断定"客户端被建成了 Web 应用类型"。使用者说他选的就是「桌面应用」，于是我做了对照实验：
// 拿 gcloud SDK 那个确定属于桌面/installed 类型的公开客户端 ID，用同样的方式探测，
// 不带密钥时回的是**完全相同**的这条错误。
//
// 结论：Google 文档把 client_secret 标为 Optional，但换码时实际必须提供，
// 与客户端类型无关。所以正确的提示是"去把密钥填上"，绝不能建议重建客户端——
// 那会让人白白丢掉一个完全正确的客户端。
// 这个测试就是防止那句错误建议再被写回来。
func TestGoogleOAuthErrorExplainsMissingClientSecret(t *testing.T) {
	body := []byte(`{"error":"invalid_request","error_description":"client_secret is missing."}`)
	err := googleOAuthError(http.StatusBadRequest, body, "换取 Google token 失败")
	if err == nil {
		t.Fatal("应产生错误")
	}
	message := err.Error()
	if !strings.Contains(message, "client_secret is missing") {
		t.Fatalf("应保留 Google 的原始描述：%s", message)
	}
	if !strings.Contains(message, "客户端密钥") {
		t.Fatalf("应告诉使用者去哪里填密钥：%s", message)
	}
	if !strings.Contains(message, "桌面应用类型也一样") {
		t.Fatalf("必须点明这与客户端类型无关，否则使用者会去重建一个没用的客户端：%s", message)
	}
	// 上一版错误提示说的是"你这个是 Web 应用类型，重建一个桌面应用就好了"。
	// 那句话是错的，不能回来。
	if strings.Contains(message, "Web 应用") || strings.Contains(message, "重新建一个") {
		t.Fatalf("不应再建议改客户端类型：%s", message)
	}
}

// 与上面相对：其他 invalid_request 不应被套上"缺密钥"的解释，否则会误导。
func TestGoogleOAuthErrorLeavesUnrelatedInvalidRequestAlone(t *testing.T) {
	body := []byte(`{"error":"invalid_request","error_description":"Missing required parameter: code"}`)
	err := googleOAuthError(http.StatusBadRequest, body, "换取 Google token 失败")
	if err == nil {
		t.Fatal("应产生错误")
	}
	if strings.Contains(err.Error(), "桌面应用") {
		t.Fatalf("无关的 invalid_request 不应提示客户端类型：%s", err.Error())
	}
}

func TestGoogleOAuthErrorNamesClientTypeOnInvalidClient(t *testing.T) {
	body := []byte(`{"error":"invalid_client","error_description":"Invalid client type."}`)
	err := googleOAuthError(http.StatusUnauthorized, body, "google_token_failed")
	if err == nil {
		t.Fatal("invalid_client 应产生错误")
	}
	message := err.Error()
	if !strings.Contains(message, "Invalid client type") {
		t.Fatalf("错误信息应保留 Google 的原始描述：%s", message)
	}
	if !strings.Contains(message, "桌面应用") {
		t.Fatalf("错误信息应提示客户端类型这一真实病因：%s", message)
	}
}

func TestGoogleUnreachableErrorMentionsProxy(t *testing.T) {
	// 连不上时最可能的原因是没走代理（Go 不读 Windows 系统代理），提示必须指向这一点。
	err := googleUnreachableError(context.DeadlineExceeded)
	if err == nil {
		t.Fatal("超时应产生错误")
	}
	if !strings.Contains(err.Error(), "代理") {
		t.Fatalf("连不上时应提示代理：%s", err.Error())
	}
}

// 免费 Gmail 只能靠 + 标签制造多地址，所以这个函数是整条"多地址"能力的地基。
func TestBuildPlusAlias(t *testing.T) {
	cases := []struct {
		name    string
		email   string
		tag     string
		want    string
		wantErr bool
	}{
		{"普通标签", "user@gmail.com", "shop", "user+shop@gmail.com", false},
		{"标签含空格被去掉", "user@gmail.com", "  shop  ", "user+shop@gmail.com", false},
		{"已有加号时替换", "user+old@gmail.com", "new", "user+new@gmail.com", false},
		{"大小写归一", "User@Gmail.com", "Shop", "user+shop@gmail.com", false},
		{"空标签报错", "user@gmail.com", "", "", true},
		{"标签含加号报错", "user@gmail.com", "a+b", "", true},
		{"标签含 at 报错", "user@gmail.com", "a@b", "", true},
		{"标签含空格报错", "user@gmail.com", "a b", "", true},
		{"没有 at 报错", "user", "shop", "", true},
		// Workspace 的自有域名同样支持 + 寻址，所以这里刻意不限制域名。
		{"Workspace 域名可用", "user@corp.example.com", "shop", "user+shop@corp.example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildPlusAlias(tc.email, tc.tag)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应报错，实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if got != tc.want {
				t.Fatalf("want=%q got=%q", tc.want, got)
			}
		})
	}
}

// Gmail 的正文只在单封 get 里才有，且必须递归进 parts。
// 同时验证路由优先级：Delivered-To 是投递事实，比 To 更可信，必须优先。
func TestSyncMailboxesRoutesGmailByDeliveredToBeforeTo(t *testing.T) {
	messages := map[string]map[string]any{
		"m1": gmailMessageFixture("m1", "验证码 111", "1750000000000",
			"user+shop@gmail.com", []string{"user+other@gmail.com"}, "<p>111</p>", "111"),
		"m2": gmailMessageFixture("m2", "验证码 222", "1750000100000",
			"", []string{"user+shop@gmail.com"}, "<p>222</p>", "222"),
		"m3": gmailMessageFixture("m3", "无关邮件", "1750000200000",
			"", []string{"someone-else@example.com"}, "<p>333</p>", "333"),
	}
	client := newTestGmailClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			ids := []map[string]any{}
			for id := range messages {
				ids = append(ids, map[string]any{"id": id, "threadId": "t-" + id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": ids, "resultSizeEstimate": len(ids)})
		default:
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			message, ok := messages[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(message)
		}
	})

	result, err := client.SyncMailboxes(context.Background(), "token", []Mailbox{
		{ID: "box-shop", Email: "user+shop@gmail.com"},
		{ID: "box-other", Email: "user+other@gmail.com"},
	}, GoogleMailSyncOptions{Limit: 10})
	if err != nil {
		t.Fatalf("同步失败：%v", err)
	}

	// m1 的 Delivered-To 是 shop，To 是 other：必须归 shop。
	shop := result.MessagesByMailbox["box-shop"]
	if len(shop) != 2 {
		t.Fatalf("shop 应收 2 封（m1 按 Delivered-To、m2 按 To），实际 %d", len(shop))
	}
	for _, message := range shop {
		if message.ID == "m1" && !strings.Contains(message.Subject, "111") {
			t.Fatalf("m1 归属正确但内容不对：%+v", message)
		}
	}
	if other := result.MessagesByMailbox["box-other"]; len(other) != 0 {
		t.Fatalf("other 不应收到 m1（Delivered-To 优先）：%+v", other)
	}
	if result.Unmatched != 1 {
		t.Fatalf("m3 应被计为未匹配，实际 unmatched=%d", result.Unmatched)
	}
	if result.Scanned != 3 {
		t.Fatalf("扫描数应为 3，实际 %d", result.Scanned)
	}
	if result.Matched != 2 {
		t.Fatalf("匹配数应为 2，实际 %d", result.Matched)
	}

	// 正文必须从嵌套的 parts 里取出来，两种格式都要有。
	if got := shop[0].Body; !strings.Contains(got, "111") && !strings.Contains(got, "222") {
		t.Fatalf("正文未解析：%q", got)
	}
}

func TestConvertGmailMessageReadsNestedPartsAndHeaders(t *testing.T) {
	raw := gmailMessageFixture("m1", "主题", "1750000000000",
		"user+shop@gmail.com", []string{"user+shop@gmail.com", "cc@example.com"}, "<p>富文本</p>", "纯文本")
	body, _ := json.Marshal(raw)
	var message gmailMessage
	if err := json.Unmarshal(body, &message); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}

	converted := convertGmailMessage(message)
	if converted.Subject != "主题" {
		t.Fatalf("主题不正确：%q", converted.Subject)
	}
	if !strings.Contains(converted.From, "sender@example.com") {
		t.Fatalf("发件人不正确：%q", converted.From)
	}
	if len(converted.DeliveredTo) != 1 || converted.DeliveredTo[0] != "user+shop@gmail.com" {
		t.Fatalf("Delivered-To 未保留：%+v", converted.DeliveredTo)
	}
	if len(converted.ToRecipients) != 2 {
		t.Fatalf("收件人未拆全：%+v", converted.ToRecipients)
	}
	if !strings.Contains(converted.HTMLBody, "富文本") {
		t.Fatalf("HTML 正文未取到：%q", converted.HTMLBody)
	}
	if !strings.Contains(converted.Body, "纯文本") {
		t.Fatalf("纯文本正文未取到：%q", converted.Body)
	}
	if converted.ReceivedAt.IsZero() {
		t.Fatal("internalDate 未被解析成收信时间")
	}
}

// Gmail 的 body.data 在实践中出现过三种 base64 变体，任何一种解不开都会让取码失败。
func TestDecodeGmailBodyHandlesBase64Variants(t *testing.T) {
	plain := "验证码 123456"
	raw := []byte(plain)
	cases := map[string]string{
		"RawURLEncoding": base64.RawURLEncoding.EncodeToString(raw),
		"URLEncoding":    base64.URLEncoding.EncodeToString(raw),
		"StdEncoding":    base64.StdEncoding.EncodeToString(raw),
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			if got := decodeGmailBody(encoded); got != plain {
				t.Fatalf("want=%q got=%q", plain, got)
			}
		})
	}
	if got := decodeGmailBody(""); got != "" {
		t.Fatalf("空输入应返回空：%q", got)
	}
}

func TestSplitMailAddressesHandlesNamesAndSeparators(t *testing.T) {
	got := splitMailAddresses(`"Doe, John" <john@example.com>, jane@example.com; , bob@example.com`)
	want := []string{"john@example.com", "jane@example.com", "bob@example.com"}
	if len(got) != len(want) {
		t.Fatalf("地址数量不正确：want=%v got=%v", want, got)
	}
	for index := range want {
		if !strings.EqualFold(got[index], want[index]) {
			t.Fatalf("第 %d 个地址不正确：want=%v got=%v", index, want, got)
		}
	}
	if addresses := splitMailAddresses("   "); len(addresses) != 0 {
		t.Fatalf("空白输入不应产生地址：%v", addresses)
	}
}

// 免费 Gmail 没有可枚举的附加地址，所以这里必须老老实实只回主地址，
// 而不是编造一堆 + 标签让界面看起来"发现了很多别名"。
// RFC 5321 规定 @ 之前的本地部分最多 64 个字符。超了 Gmail 会静默拒收，
// 而地址在库里看起来一切正常——所以必须在构造时就报错。
func TestBuildPlusAliasEnforcesLocalPartLimit(t *testing.T) {
	const base = "user@gmail.com" // 本地部分 4 个字符，最多容纳 59 个字符的标签
	if got := MaxPlusTagLength(base); got != 59 {
		t.Fatalf("MaxPlusTagLength 计算错误：got=%d want=59", got)
	}
	// 正好到上限：本地部分 4 + "+" + 59 = 64，必须通过。
	atLimit := strings.Repeat("a", 59)
	email, err := BuildPlusAlias(base, atLimit)
	if err != nil {
		t.Fatalf("正好到 64 个字符时不应报错：%v", err)
	}
	if local := email[:strings.Index(email, "@")]; len(local) != 64 {
		t.Fatalf("边界地址的本地部分应为 64，实际 %d：%s", len(local), email)
	}
	// 超一个字符：65，必须报错而不是静默通过。
	if _, err := BuildPlusAlias(base, strings.Repeat("a", 60)); err == nil {
		t.Fatal("超过 64 个字符时必须报错，否则会生成一个永远收不到信的地址")
	} else if ErrorDetailsCode(err) != "google_alias_too_long" {
		t.Fatalf("错误码不正确：%v", err)
	}
}

// 随机标签用的字符集不能含 i、l、o、0、1：这几个在抄写地址时最容易看错。
func TestPlusTagAlphabetExcludesConfusableCharacters(t *testing.T) {
	for _, bad := range []string{"i", "l", "o", "0", "1"} {
		if strings.Contains(plusTagAlphabet, bad) {
			t.Fatalf("字符集不应包含易混淆字符 %q：%s", bad, plusTagAlphabet)
		}
	}
	if len(plusTagAlphabet) < 30 {
		t.Fatalf("字符集太小，碰撞概率会明显上升：%d 个字符", len(plusTagAlphabet))
	}
}

func TestRandomPlusTagRespectsPrefixAndLength(t *testing.T) {
	tag, err := RandomPlusTag("Shop", 8)
	if err != nil {
		t.Fatalf("生成随机标签失败：%v", err)
	}
	if !strings.HasPrefix(tag, "shop-") {
		t.Fatalf("前缀应统一转小写并用 - 连接：%q", tag)
	}
	if len(tag) != len("shop-")+8 {
		t.Fatalf("标签长度不正确：%q", tag)
	}
	for _, char := range strings.TrimPrefix(tag, "shop-") {
		if !strings.ContainsRune(plusTagAlphabet, char) {
			t.Fatalf("标签含字符集之外的字符 %q：%q", char, tag)
		}
	}
	// 不带前缀时是纯随机串。
	plain, err := RandomPlusTag("", 8)
	if err != nil {
		t.Fatalf("生成随机标签失败：%v", err)
	}
	if len(plain) != 8 || strings.Contains(plain, "-") {
		t.Fatalf("无前缀时不应出现分隔符：%q", plain)
	}
}

// 两次生成不应相同——否则批量创建会一直在同一个地址上打转。
func TestRandomPlusTagDoesNotRepeat(t *testing.T) {
	seen := map[string]struct{}{}
	for index := 0; index < 200; index++ {
		tag, err := RandomPlusTag("", 8)
		if err != nil {
			t.Fatalf("生成随机标签失败：%v", err)
		}
		if _, ok := seen[tag]; ok {
			t.Fatalf("200 次生成出现重复：%q", tag)
		}
		seen[tag] = struct{}{}
	}
}

func TestMaxPlusTagLengthHandlesInvalidInput(t *testing.T) {
	if got := MaxPlusTagLength("没有 at 符号"); got != 0 {
		t.Fatalf("非法地址应返回 0：%d", got)
	}
	if got := MaxPlusTagLength("user+old@gmail.com"); got != 59 {
		t.Fatalf("已有 + 标签时应先剥掉再计算：%d", got)
	}
}

func TestListAliasesReturnsPrimaryAddressOnly(t *testing.T) {

	client := newTestGmailClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": "user@gmail.com", "messagesTotal": 12})
	})
	aliases, err := client.ListAliases(context.Background(), "token")
	if err != nil {
		t.Fatalf("读取地址失败：%v", err)
	}
	if len(aliases) != 1 || aliases[0] != "user@gmail.com" {
		t.Fatalf("应只返回主地址：%+v", aliases)
	}
}

func TestGmailSyncMailboxesRejectsMissingTokenAndEmptyMailboxes(t *testing.T) {
	client := newTestGmailClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("凭据不完整时不应发起请求")
	})
	if _, err := client.SyncMailboxes(context.Background(), "", []Mailbox{{ID: "b", Email: "user@gmail.com"}}, GoogleMailSyncOptions{}); err == nil {
		t.Fatal("缺少访问令牌时应报错")
	}
	// 没有邮箱是"无事可做"，不是失败——调用方据此刻意与 Microsoft 不同，
	// 免得一个还没配地址的账号每轮都往日志里写一条错误。
	result, err := client.SyncMailboxes(context.Background(), "token", nil, GoogleMailSyncOptions{})
	if err != nil {
		t.Fatalf("没有邮箱不应报错：%v", err)
	}
	if result.Scanned != 0 || len(result.MessagesByMailbox) != 0 {
		t.Fatalf("没有邮箱时应返回空结果：%+v", result)
	}
}

// 401 必须映射成可重试的"令牌失效"信号，同步层据此刷新一次再重试。
func TestGmailErrorMapsUnauthorizedToTokenRejected(t *testing.T) {
	body := []byte(`{"error":{"code":401,"message":"Invalid Credentials","status":"UNAUTHENTICATED"}}`)
	err := gmailError(http.StatusUnauthorized, body, "gmail_sync_failed")
	if err == nil {
		t.Fatal("401 应产生错误")
	}
	if !strings.Contains(err.Error(), "Invalid Credentials") && !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误信息应包含病因：%s", err.Error())
	}
}

func TestResolveProxyPrefersExplicitThenEnv(t *testing.T) {
	if got := ResolveProxy("http://explicit:1234"); got != "http://explicit:1234" {
		t.Fatalf("显式代理应最优先：%q", got)
	}
	t.Setenv("HTTPS_PROXY", "http://env:5678")
	if got := ResolveProxy(""); got != "http://env:5678" {
		t.Fatalf("环境变量应作为第二优先：%q", got)
	}
	// 环境变量全部清空时会落到系统代理；系统代理是否配置取决于机器，
	// 所以这里只断言"不会 panic 且返回值可读"，不对具体值做断言。
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	if got := ResolveProxy(""); strings.Contains(got, "env") {
		t.Fatalf("环境变量已清空，不应再返回它：%q", got)
	}
	if got := ProxyDescription(""); got == "" {
		t.Fatal("没有代理时也应给出可读说明")
	}
}

func TestProxyDescriptionIsHumanReadable(t *testing.T) {
	if got := ProxyDescription("http://127.0.0.1:7897"); got != "http://127.0.0.1:7897" {
		t.Fatalf("应原样回显：%q", got)
	}
	if got := ProxyDescription(""); got == "" {
		t.Fatal("没有代理时也应给出可读说明，而不是空串")
	}
}

// 远端邮箱列表用的是 domain.Mailbox，这里顺带钉住按地址匹配时大小写不敏感。
func TestSyncMailboxesMatchesMailboxCaseInsensitively(t *testing.T) {
	raw := gmailMessageFixture("m1", "验证码", "1750000000000",
		"User+Shop@Gmail.com", nil, "<p>9</p>", "9")
	client := newTestGmailClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"messages": []map[string]any{{"id": "m1", "threadId": "t1"}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(raw)
	})

	result, err := client.SyncMailboxes(context.Background(), "token", []Mailbox{{ID: "box-shop", Email: "user+shop@gmail.com"}}, GoogleMailSyncOptions{})
	if err != nil {
		t.Fatalf("同步失败：%v", err)
	}
	if len(result.MessagesByMailbox["box-shop"]) != 1 {
		t.Fatalf("大小写不同应仍能匹配：%+v", result.MessagesByMailbox)
	}
}

var _ = domain.ProviderGoogle
