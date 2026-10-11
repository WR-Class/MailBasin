package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestGraphClient(t *testing.T, handler http.HandlerFunc) *GraphClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewGraphClient()
	client.baseURL = server.URL
	return client
}

func graphMessageFixture(id, subject, receivedAt string, contentType, content string, to, cc []string) map[string]any {
	recipients := func(addresses []string) []map[string]any {
		out := make([]map[string]any, 0, len(addresses))
		for _, address := range addresses {
			out = append(out, map[string]any{"emailAddress": map[string]any{"address": address}})
		}
		return out
	}
	return map[string]any{
		"id":               id,
		"subject":          subject,
		"from":             map[string]any{"emailAddress": map[string]any{"address": "sender@example.com", "name": "Sender"}},
		"toRecipients":     recipients(to),
		"ccRecipients":     recipients(cc),
		"bodyPreview":      "preview text",
		"receivedDateTime": receivedAt,
		"body":             map[string]any{"contentType": contentType, "content": content},
	}
}

// 一封邮件只归属第一个命中的别名，且没有命中的邮件必须被计数而不是丢弃。
func TestSyncMailboxesRoutesMessagesByRecipient(t *testing.T) {
	var requested []string
	client := newTestGraphClient(t, func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.RawQuery)
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization 头不正确：%q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": []any{
				graphMessageFixture("m1", "验证码", "2026-01-01T10:00:00Z", "text", "您的验证码是 111111", []string{"alias-one@outlook.com"}, nil),
				graphMessageFixture("m2", "抄送命中", "2026-01-01T09:00:00Z", "text", "验证码 222222", []string{"someone@example.com"}, []string{"alias-two@outlook.com"}),
				graphMessageFixture("m3", "无人认领", "2026-01-01T08:00:00Z", "text", "验证码 333333", []string{"nobody@example.com"}, nil),
				// 同时命中两个别名时必须只投递一次，否则同一封信会重复入库。
				graphMessageFixture("m4", "多别名", "2026-01-01T07:00:00Z", "text", "验证码 444444", []string{"alias-one@outlook.com", "alias-two@outlook.com"}, nil),
				// 大小写不同仍应命中同一个别名。
				graphMessageFixture("m5", "大小写", "2026-01-01T06:00:00Z", "text", "验证码 555555", []string{"ALIAS-ONE@Outlook.com"}, nil),
			},
		})
	})

	mailboxes := []Mailbox{
		{ID: "box-one", Email: "alias-one@outlook.com"},
		{ID: "box-two", Email: "alias-two@outlook.com"},
	}
	result, err := client.SyncMailboxes(context.Background(), "test-token", mailboxes, MicrosoftMailSyncOptions{Limit: 50})
	if err != nil {
		t.Fatalf("同步失败：%v", err)
	}
	if result.Scanned != 5 {
		t.Fatalf("扫描数量应为 5，实际 %d", result.Scanned)
	}
	if result.Unmatched != 1 {
		t.Fatalf("未匹配数量应为 1（m3），实际 %d", result.Unmatched)
	}
	if result.Matched != 4 {
		t.Fatalf("匹配数量应为 4，实际 %d", result.Matched)
	}
	if got := len(result.MessagesByMailbox["box-one"]); got != 3 {
		t.Fatalf("box-one 应收 3 封（m1/m4/m5），实际 %d", got)
	}
	if got := len(result.MessagesByMailbox["box-two"]); got != 1 {
		t.Fatalf("box-two 应收 1 封（m2，m4 已被 box-one 认领），实际 %d", got)
	}
	// 每个邮箱内部按收信时间升序，写库顺序才与真实时间线一致。
	boxOne := result.MessagesByMailbox["box-one"]
	for index := 1; index < len(boxOne); index++ {
		if boxOne[index-1].ReceivedAt.After(boxOne[index].ReceivedAt) {
			t.Fatalf("邮箱内邮件未按时间升序：%v", boxOne)
		}
	}
	if len(requested) != 1 {
		t.Fatalf("无 nextLink 时不应翻页，实际请求 %d 次", len(requested))
	}
	if !strings.Contains(requested[0], "%24top=50") && !strings.Contains(requested[0], "$top=50") {
		t.Fatalf("请求未带上 $top：%s", requested[0])
	}
	if !strings.Contains(requested[0], "%24orderby") && !strings.Contains(requested[0], "$orderby") {
		t.Fatalf("请求未带上 $orderby：%s", requested[0])
	}
}

func TestSyncMailboxesFollowsNextLink(t *testing.T) {
	client := newTestGraphClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "page=2") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []any{graphMessageFixture("second", "第二页", "2026-01-01T05:00:00Z", "text", "验证码 2", []string{"alias@outlook.com"}, nil)},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value":           []any{graphMessageFixture("first", "第一页", "2026-01-01T06:00:00Z", "text", "验证码 1", []string{"alias@outlook.com"}, nil)},
			"@odata.nextLink": "http://" + r.Host + "/next?page=2",
		})
	})
	result, err := client.SyncMailboxes(context.Background(), "test-token", []Mailbox{{ID: "box", Email: "alias@outlook.com"}}, MicrosoftMailSyncOptions{Limit: 50})
	if err != nil {
		t.Fatalf("同步失败：%v", err)
	}
	if result.Scanned != 2 {
		t.Fatalf("应翻页取到 2 封，实际 %d", result.Scanned)
	}
	if len(result.MessagesByMailbox["box"]) != 2 {
		t.Fatalf("两页邮件都应归属该邮箱：%+v", result.MessagesByMailbox)
	}
}

func TestSyncMailboxesRejectsMissingTokenAndEmptyMailboxes(t *testing.T) {
	client := NewGraphClient()
	if _, err := client.SyncMailboxes(context.Background(), "  ", []Mailbox{{ID: "box", Email: "a@outlook.com"}}, MicrosoftMailSyncOptions{}); ErrorDetailsCode(err) != "microsoft_access_token_missing" {
		t.Fatalf("缺少 token 时应报 microsoft_access_token_missing，实际 %v", err)
	}
	result, err := client.SyncMailboxes(context.Background(), "token", nil, MicrosoftMailSyncOptions{})
	if err != nil || result.Scanned != 0 || len(result.MessagesByMailbox) != 0 {
		t.Fatalf("没有邮箱时应直接返回空结果：%+v err=%v", result, err)
	}
}

func TestSyncMailboxesMapsGraphFailureCodes(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "microsoft_token_rejected"},
		{http.StatusForbidden, "microsoft_permission_denied"},
		{http.StatusTooManyRequests, "microsoft_rate_limited"},
		{http.StatusInternalServerError, "microsoft_graph_failed"},
	}
	for _, item := range cases {
		t.Run(item.want, func(t *testing.T) {
			client := newTestGraphClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(item.status)
				_, _ = w.Write([]byte(`{"error":{"code":"X","message":"boom"}}`))
			})
			_, err := client.SyncMailboxes(context.Background(), "token", []Mailbox{{ID: "box", Email: "a@outlook.com"}}, MicrosoftMailSyncOptions{})
			if got := ErrorDetailsCode(err); got != item.want {
				t.Fatalf("HTTP %d 应映射为 %s，实际 %s（%v）", item.status, item.want, got, err)
			}
		})
	}
}

// 第一页就失败必须整体报错；后续页失败则保留已取到的内容。
func TestSyncMailboxesKeepsEarlierPagesWhenLaterPageFails(t *testing.T) {
	client := newTestGraphClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "page=2") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value":           []any{graphMessageFixture("first", "第一页", "2026-01-01T06:00:00Z", "text", "验证码 1", []string{"alias@outlook.com"}, nil)},
			"@odata.nextLink": "http://" + r.Host + "/next?page=2",
		})
	})
	result, err := client.SyncMailboxes(context.Background(), "token", []Mailbox{{ID: "box", Email: "alias@outlook.com"}}, MicrosoftMailSyncOptions{})
	if err != nil {
		t.Fatalf("后续页失败不应让整次同步失败：%v", err)
	}
	if !result.HasMore || len(result.MessagesByMailbox["box"]) != 1 {
		t.Fatalf("应保留第一页并标记还有更多：%+v", result)
	}
}

func TestListAliasesParsesProxyAddresses(t *testing.T) {
	client := newTestGraphClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mail":              "Primary@Outlook.com",
			"userPrincipalName": "primary@outlook.com",
			"proxyAddresses": []string{
				"SMTP:Primary@Outlook.com",
				"smtp:alias-one@outlook.com",
				"smtp:ALIAS-TWO@outlook.com",
				"smtp:alias-one@outlook.com",
				"malformed-without-colon@outlook.com",
				"smtp:",
			},
		})
	})
	aliases, err := client.ListAliases(context.Background(), "token")
	if err != nil {
		t.Fatalf("读取别名失败：%v", err)
	}
	want := []string{"alias-one@outlook.com", "alias-two@outlook.com", "malformed-without-colon@outlook.com", "primary@outlook.com"}
	if len(aliases) != len(want) {
		t.Fatalf("别名数量不正确：got=%v want=%v", aliases, want)
	}
	for index := range want {
		if aliases[index] != want[index] {
			t.Fatalf("别名解析不正确：got=%v want=%v", aliases, want)
		}
	}
}

// 主地址由 proxyAddresses 前缀的大小写标记：大写 SMTP 是主地址，小写 smtp 是别名。
// 这正是之前被丢掉的信息——丢掉之后主邮箱就被当成了别名。
func TestListAddressesSeparatesPrimaryFromAliases(t *testing.T) {
	client := newTestGraphClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mail":              "Primary@Outlook.com",
			"userPrincipalName": "primary@outlook.com",
			"proxyAddresses": []string{
				"SMTP:Primary@Outlook.com",
				"smtp:alias-one@outlook.com",
				"smtp:ALIAS-TWO@outlook.com",
				"smtp:alias-one@outlook.com",
			},
		})
	})
	primary, aliases, err := client.ListAddresses(context.Background(), "token")
	if err != nil {
		t.Fatalf("拆分地址失败：%v", err)
	}
	if primary != "primary@outlook.com" {
		t.Fatalf("主地址不正确：got=%q want=%q", primary, "primary@outlook.com")
	}
	want := []string{"alias-one@outlook.com", "alias-two@outlook.com"}
	if len(aliases) != len(want) {
		t.Fatalf("别名数量不正确：got=%v want=%v", aliases, want)
	}
	for index := range want {
		if aliases[index] != want[index] {
			t.Fatalf("别名解析不正确：got=%v want=%v", aliases, want)
		}
	}
}

// mail 字段缺失时，大写 SMTP 那条就是主地址。
func TestListAddressesUsesUppercaseMarkerWhenMailMissing(t *testing.T) {
	client := newTestGraphClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"proxyAddresses": []string{
				"smtp:alias@outlook.com",
				"SMTP:Primary@Outlook.com",
			},
		})
	})
	primary, aliases, err := client.ListAddresses(context.Background(), "token")
	if err != nil {
		t.Fatalf("拆分地址失败：%v", err)
	}
	if primary != "primary@outlook.com" {
		t.Fatalf("无 mail 字段时应取大写 SMTP 标记：got=%q", primary)
	}
	if len(aliases) != 1 || aliases[0] != "alias@outlook.com" {
		t.Fatalf("别名不正确：%v", aliases)
	}
}

// 完全没有标记时退回旧行为：全部地址并入别名，主地址留空，不猜。
func TestListAddressesWithoutMarkerKeepsUnionBehaviour(t *testing.T) {
	client := newTestGraphClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"proxyAddresses": []string{"one@outlook.com", "two@outlook.com"},
		})
	})
	primary, aliases, err := client.ListAddresses(context.Background(), "token")
	if err != nil {
		t.Fatalf("拆分地址失败：%v", err)
	}
	if primary != "" {
		t.Fatalf("没有标记时不应猜主地址：got=%q", primary)
	}
	want := []string{"one@outlook.com", "two@outlook.com"}
	if len(aliases) != len(want) || aliases[0] != want[0] || aliases[1] != want[1] {
		t.Fatalf("无标记时应全部按别名返回：got=%v want=%v", aliases, want)
	}
}

func TestListAliasesRejectsMissingToken(t *testing.T) {
	client := NewGraphClient()
	if _, err := client.ListAliases(context.Background(), ""); ErrorDetailsCode(err) != "microsoft_access_token_missing" {
		t.Fatalf("缺少 token 时应报 microsoft_access_token_missing，实际 %v", err)
	}
}

func TestConvertGraphMessageHandlesHTMLAndTextBodies(t *testing.T) {
	html := graphMessage{
		ID:               "html-1",
		Subject:          " 验证码 ",
		ReceivedDateTime: "2026-01-01T10:00:00Z",
		Body: struct {
			ContentType string `json:"contentType"`
			Content     string `json:"content"`
		}{ContentType: "HTML", Content: "<p>您的验证码是 <b>654321</b></p>"},
	}
	html.From = &graphRecipient{EmailAddress: graphEmailAddress{Address: "sender@example.com"}}
	converted := convertGraphMessage(html)
	if converted.ID != "html-1" || converted.Subject != "验证码" || converted.From != "sender@example.com" {
		t.Fatalf("HTML 邮件基础字段不正确：%+v", converted)
	}
	if converted.ContentType != "html" || converted.HTMLBody == "" {
		t.Fatalf("HTML 正文应保留：%+v", converted)
	}
	if !strings.Contains(converted.Body, "654321") || strings.Contains(converted.Body, "<b>") {
		t.Fatalf("HTML 应同时抽出纯文本正文：%q", converted.Body)
	}

	// 纯文本邮件没有 HTML 正文，且正文为空时要回退到 bodyPreview。
	text := graphMessage{
		ID:               "text-1",
		ReceivedDateTime: "not-a-time",
		BodyPreview:      "预览里的验证码 999999",
	}
	text.Body.ContentType = "text"
	text.Body.Content = "   "
	converted = convertGraphMessage(text)
	if converted.HTMLBody != "" {
		t.Fatalf("纯文本邮件不应产生 HTML 正文：%+v", converted)
	}
	if !strings.Contains(converted.Body, "999999") {
		t.Fatalf("正文为空时应回退到 bodyPreview：%q", converted.Body)
	}
	if converted.ReceivedAt.IsZero() {
		t.Fatal("时间解析失败时应回退到当前时间而不是零值")
	}
}

func TestGraphRecipientsOfNormalizesAndSkipsEmpty(t *testing.T) {
	message := graphMessage{
		ToRecipients: []graphRecipient{
			{EmailAddress: graphEmailAddress{Address: "  Alias@Outlook.com "}},
			{EmailAddress: graphEmailAddress{Address: "   "}},
		},
		CcRecipients: []graphRecipient{{EmailAddress: graphEmailAddress{Address: "CC@Outlook.com"}}},
	}
	got := graphRecipientsOf(message)
	if len(got) != 2 || got[0] != "alias@outlook.com" || got[1] != "cc@outlook.com" {
		t.Fatalf("收件人归一化不正确：%v", got)
	}
}

func TestParseMicrosoftTokenAppliesExpirySkew(t *testing.T) {
	before := time.Now()
	result, err := parseMicrosoftToken([]byte(`{"access_token":"a","refresh_token":"r","expires_in":3600,"scope":"Mail.Read","token_type":"Bearer"}`))
	if err != nil {
		t.Fatalf("解析 token 失败：%v", err)
	}
	if result.AccessToken != "a" || result.RefreshToken != "r" || result.Scope != "Mail.Read" || result.TokenType != "Bearer" {
		t.Fatalf("token 字段不正确：%+v", result)
	}
	// 提前 5 分钟视为过期，避免边界时刻带着即将失效的 token 发请求。
	want := before.Add(3600*time.Second - 5*time.Minute)
	if delta := result.ExpiresAt.Sub(want); delta > 2*time.Second || delta < -2*time.Second {
		t.Fatalf("过期时间未按 5 分钟提前：got=%v want≈%v", result.ExpiresAt, want)
	}

	if _, err := parseMicrosoftToken([]byte(`{"refresh_token":"r"}`)); ErrorDetailsCode(err) != "microsoft_token_empty" {
		t.Fatalf("缺少 access_token 应报错，实际 %v", err)
	}
	if _, err := parseMicrosoftToken([]byte(`not json`)); ErrorDetailsCode(err) != "microsoft_token_parse_failed" {
		t.Fatalf("非法 JSON 应报解析失败，实际 %v", err)
	}
}

func TestMicrosoftErrorDescriptionsAndRetryability(t *testing.T) {
	oauthBody := []byte(`{"error":"invalid_grant","error_description":"AADSTS70000: refresh token expired"}`)
	if code := microsoftOAuthErrorCode(oauthBody); code != "invalid_grant" {
		t.Fatalf("OAuth 错误码解析不正确：%q", code)
	}
	err := microsoftOAuthError(http.StatusBadRequest, oauthBody, "续期失败")
	if !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "AADSTS70000") {
		t.Fatalf("OAuth 错误信息应包含错误码与描述：%v", err)
	}
	_, _, retryable := ErrorDetails(err)
	if retryable {
		t.Fatal("HTTP 400 的 OAuth 错误不应标记为可重试")
	}

	// Graph 的嵌套错误形状：error 是对象而不是字符串。
	graphBody := []byte(`{"error":{"code":"ErrorAccessDenied","message":"Access is denied."}}`)
	if detail := microsoftErrorDescription(graphBody); detail != "Access is denied." {
		t.Fatalf("Graph 嵌套错误描述解析不正确：%q", detail)
	}
	_, _, retryable = ErrorDetails(microsoftGraphError(http.StatusTooManyRequests, graphBody, "读取失败"))
	if !retryable {
		t.Fatal("429 应标记为可重试")
	}
	if code := ErrorDetailsCode(microsoftGraphError(http.StatusUnauthorized, nil, "读取失败")); code != "microsoft_token_rejected" {
		t.Fatalf("401 应映射为 microsoft_token_rejected，实际 %s", code)
	}
}

func TestMicrosoftProfileEmailPrefersMail(t *testing.T) {
	cases := []struct {
		name    string
		profile MicrosoftProfile
		want    string
	}{
		{"优先 mail", MicrosoftProfile{Mail: "Mail@Outlook.com", UserPrincipalName: "upn@outlook.com"}, "mail@outlook.com"},
		{"回退 UPN", MicrosoftProfile{UserPrincipalName: "UPN@Outlook.com"}, "upn@outlook.com"},
		{"都没有则为空", MicrosoftProfile{}, ""},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := item.profile.Email(); got != item.want {
				t.Fatalf("邮箱推导错误：got=%q want=%q", got, item.want)
			}
		})
	}
}

func TestMicrosoftDefaultsFillBlankInputs(t *testing.T) {
	if got := microsoftClientIDOrDefault("  "); got != DefaultMicrosoftClientID {
		t.Fatalf("空 client_id 应回退到内置值，实际 %q", got)
	}
	if got := microsoftClientIDOrDefault(" custom "); got != "custom" {
		t.Fatalf("自定义 client_id 应被保留并去空白，实际 %q", got)
	}
	if got := microsoftTenantOrDefault(""); got != DefaultMicrosoftTenant {
		t.Fatalf("空 tenant 应回退到 %s，实际 %q", DefaultMicrosoftTenant, got)
	}
	if got := microsoftTenantOrDefault(" contoso.onmicrosoft.com "); got != "contoso.onmicrosoft.com" {
		t.Fatalf("自定义 tenant 应被保留并去空白，实际 %q", got)
	}
}

func TestBuildInboxQueryIncludesFilterOnlyWhenAfterIsSet(t *testing.T) {
	client := NewGraphClient()
	plain := client.buildInboxQuery(25, MicrosoftMailSyncOptions{})
	if !strings.Contains(plain, "%24top=25") && !strings.Contains(plain, "$top=25") {
		t.Fatalf("应带上 $top：%s", plain)
	}
	if strings.Contains(plain, "filter") {
		t.Fatalf("没有 After 时不应带 $filter：%s", plain)
	}
	after := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	filtered := client.buildInboxQuery(25, MicrosoftMailSyncOptions{After: after})
	decoded, err := url.QueryUnescape(filtered)
	if err != nil {
		t.Fatalf("解码查询串失败：%v", err)
	}
	if !strings.Contains(decoded, "$filter=receivedDateTime ge 2026-01-02T03:04:05Z") {
		t.Fatalf("$filter 格式不正确：%s", decoded)
	}
	if !strings.Contains(decoded, "$orderby=receivedDateTime desc") {
		t.Fatalf("Graph 要求 $filter 字段出现在 $orderby 中：%s", decoded)
	}
}

// 授权失败必须是可判定的哨兵错误，前端据此区分"继续轮询"和"真的失败"。
func TestMicrosoftDeviceCodeSentinelsAreDistinguishable(t *testing.T) {
	if !errors.Is(ErrMicrosoftAuthorizationPending, ErrMicrosoftAuthorizationPending) {
		t.Fatal("哨兵错误应可比较")
	}
	if ErrorDetailsCode(ErrMicrosoftAuthorizationPending) != "microsoft_authorization_pending" {
		t.Fatalf("pending 哨兵错误码不正确：%s", ErrorDetailsCode(ErrMicrosoftAuthorizationPending))
	}
	if ErrorDetailsCode(ErrMicrosoftSlowDown) != "microsoft_slow_down" {
		t.Fatalf("slow_down 哨兵错误码不正确：%s", ErrorDetailsCode(ErrMicrosoftSlowDown))
	}
	if errors.Is(ErrMicrosoftSlowDown, ErrMicrosoftAuthorizationPending) {
		t.Fatal("两个哨兵错误不应互相匹配")
	}
}
