package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
)

// 回调端点刻意没有套 protected：请求来自 Google 的 302，带的是 state 而不是会话 cookie。
// 因此 state 是唯一的防线——这条用例是那个决定的守门人。
// 任何一个对不上号的 state 都必须被拒绝，并且不能被拿来换取令牌。
func TestGoogleCallbackFailsClosedOnUnusableState(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	cases := []struct {
		name  string
		query string
	}{
		{"缺少 state", "?code=abc"},
		{"伪造的 state", "?code=abc&state=forged"},
		{"空 state", "?code=abc&state="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.handleGoogleCallback(recorder, httptest.NewRequest(http.MethodGet, "/api/google/callback"+tc.query, nil))

			if recorder.Code != http.StatusFound {
				t.Fatalf("应跳回前端页面，实际 HTTP %d", recorder.Code)
			}
			location := recorder.Header().Get("Location")
			if !strings.HasPrefix(location, "/google-accounts?google=error") {
				t.Fatalf("应以错误状态跳回账号页，实际 %q", location)
			}
			// 最关键的一条：不能有任何账号或会话被创建出来。
			if accounts := state.GoogleAccounts(); len(accounts) != 0 {
				t.Fatalf("被拒绝的回调不应创建账号：%+v", accounts)
			}
			if sessions := state.GoogleSessions(); len(sessions) != 0 {
				t.Fatalf("被拒绝的回调不应创建会话：%+v", sessions)
			}
		})
	}
}

// 用户点了「拒绝」时 Google 带回的是 error 而不是 code，这条路径同样不能落库。
func TestGoogleCallbackReportsProviderRejection(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	recorder := httptest.NewRecorder()
	server.handleGoogleCallback(recorder, httptest.NewRequest(http.MethodGet,
		"/api/google/callback?error=access_denied&error_description=%E7%94%A8%E6%88%B7%E6%8B%92%E7%BB%9D", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("应跳回前端页面，实际 HTTP %d", recorder.Code)
	}
	location := recorder.Header().Get("Location")
	if !strings.Contains(location, "google=error") {
		t.Fatalf("应带上错误状态：%q", location)
	}
	if len(state.GoogleAccounts()) != 0 {
		t.Fatal("用户拒绝授权不应创建账号")
	}
}

// state 用过一次就必须失效，否则一个被记录的 state 可以被重放。
func TestGoogleCallbackConsumesStateExactlyOnce(t *testing.T) {
	server, _ := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	server.googleFlowMu.Lock()
	server.googleFlows["state-1"] = domain.GoogleAuthFlow{
		State:        "state-1",
		CodeVerifier: "verifier",
		RedirectURI:  "http://127.0.0.1:8788/api/google/callback",
		ClientID:     "client-1",
		ExpiresAt:    time.Now().Add(time.Minute),
	}
	server.googleFlowMu.Unlock()

	// 第一次会走到换码（必然失败，因为没有真实客户端），但 state 必须已被消费。
	recorder := httptest.NewRecorder()
	server.handleGoogleCallback(recorder, httptest.NewRequest(http.MethodGet, "/api/google/callback?code=abc&state=state-1", nil))

	server.googleFlowMu.Lock()
	_, stillThere := server.googleFlows["state-1"]
	server.googleFlowMu.Unlock()
	if stillThere {
		t.Fatal("state 用过之后必须从内存里删掉，否则可以被重放")
	}

	// 第二次必然报"会话不存在"，而不是再次尝试换码。
	recorder = httptest.NewRecorder()
	server.handleGoogleCallback(recorder, httptest.NewRequest(http.MethodGet, "/api/google/callback?code=abc&state=state-1", nil))
	if location := recorder.Header().Get("Location"); !strings.Contains(location, "%E8%AF%B7%E9%87%8D%E6%96%B0%E5%8F%91%E8%B5%B7") &&
		!strings.Contains(location, "google=error") {
		t.Fatalf("重放应被拒绝：%q", location)
	}
}

// 过期的流程同样不能换码。
func TestGoogleCallbackRejectsExpiredFlow(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	server.googleFlowMu.Lock()
	server.googleFlows["state-old"] = domain.GoogleAuthFlow{
		State:     "state-old",
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	server.googleFlowMu.Unlock()

	recorder := httptest.NewRecorder()
	server.handleGoogleCallback(recorder, httptest.NewRequest(http.MethodGet, "/api/google/callback?code=abc&state=state-old", nil))
	if location := recorder.Header().Get("Location"); !strings.Contains(location, "google=error") {
		t.Fatalf("过期流程应被拒绝：%q", location)
	}
	if len(state.GoogleAccounts()) != 0 {
		t.Fatal("过期流程不应创建账号")
	}
}

// Google 与 Microsoft 最大的差别之一：没有可以内置的公共客户端。
// 这条用例防止以后有人"顺手"加一个默认 client_id 进去。
func TestGoogleStatusHasNoBuiltInClient(t *testing.T) {
	server, _ := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	recorder := httptest.NewRecorder()
	server.handleGoogleStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/google/status", nil))
	data := decodeAPIData(t, recorder)

	if data["configured"] != false {
		t.Fatalf("未配置时 configured 应为 false：%+v", data)
	}
	if got := data["client_id"]; got != "" {
		t.Fatalf("未配置时不应有兜底 client_id，实际 %q", got)
	}
	if !strings.Contains(data["setup_note"].(string), "桌面应用") {
		t.Fatalf("提示必须点明客户端类型：%q", data["setup_note"])
	}
	if !strings.Contains(data["alias_limit_note"].(string), "+") {
		t.Fatalf("提示必须说明只有 + 标签可用：%q", data["alias_limit_note"])
	}
	// 回调地址按当前端口拼出来，用户要拿它去 Google Console 登记。
	if got := data["redirect_uri"]; got != "http://127.0.0.1:8788/api/google/callback" {
		t.Fatalf("回调地址不正确：%q", got)
	}
}

// 未配置客户端时发起授权必须明确失败，而不是给出一个必然报错的链接。
func TestGoogleLoginStartRequiresClientID(t *testing.T) {
	server, _ := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	recorder := httptest.NewRecorder()
	server.handleGoogleLoginStart(recorder, httptest.NewRequest(http.MethodPost, "/api/google-accounts/login/start", nil))
	if recorder.Code == http.StatusOK {
		t.Fatalf("没有客户端 ID 时不应返回授权链接：%s", recorder.Body.String())
	}
}

// 模块被配置关闭时，授权入口必须直接拒绝。
func TestGoogleLoginStartRespectsDisabledModule(t *testing.T) {
	server, _ := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: false})

	recorder := httptest.NewRecorder()
	server.handleGoogleLoginStart(recorder, httptest.NewRequest(http.MethodPost, "/api/google-accounts/login/start", nil))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("模块关闭时应返回 409，实际 %d", recorder.Code)
	}
}

func TestGoogleLoginStartReturnsUsableAuthURL(t *testing.T) {
	server, _ := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})
	settings := server.store.Settings()
	settings.GoogleClientID = "client-abc.apps.googleusercontent.com"
	if _, err := server.store.SaveSettings(settings); err != nil {
		t.Fatalf("保存设置失败：%v", err)
	}

	recorder := httptest.NewRecorder()
	server.handleGoogleLoginStart(recorder, httptest.NewRequest(http.MethodPost, "/api/google-accounts/login/start", nil))
	data := decodeAPIData(t, recorder)

	authURL, _ := data["auth_url"].(string)
	if !strings.Contains(authURL, "accounts.google.com") || !strings.Contains(authURL, "code_challenge_method=S256") {
		t.Fatalf("授权链接不正确：%q", authURL)
	}
	// 生成的流程必须真的被存进内存，否则回调时对不上 state。
	stateValue, _ := data["state"].(string)
	server.googleFlowMu.Lock()
	_, stored := server.googleFlows[stateValue]
	server.googleFlowMu.Unlock()
	if !stored {
		t.Fatal("授权流程未被记录，回调必然失败")
	}
}

func TestGoogleSettingsSaveAndReload(t *testing.T) {
	server, _ := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	body := `{"client_id":"cid.apps.googleusercontent.com","client_secret":"sec","proxy":"http://127.0.0.1:7897","watcher_enabled":false}`
	recorder := httptest.NewRecorder()
	server.handleGoogleSettings(recorder, httptest.NewRequest(http.MethodPut, "/api/google/settings", strings.NewReader(body)))
	data := decodeAPIData(t, recorder)

	if data["configured"] != true || data["client_id"] != "cid.apps.googleusercontent.com" {
		t.Fatalf("设置未生效：%+v", data)
	}
	if data["has_client_secret"] != true {
		t.Fatalf("密钥应被记录：%+v", data)
	}
	if data["watcher_enabled"] != false {
		t.Fatalf("监听开关未保存：%+v", data)
	}

	// 落库后重新读取状态，确认不是只改了内存。
	recorder = httptest.NewRecorder()
	server.handleGoogleStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/google/status", nil))
	reloaded := decodeAPIData(t, recorder)
	if reloaded["client_id"] != "cid.apps.googleusercontent.com" || reloaded["proxy"] != "http://127.0.0.1:7897" {
		t.Fatalf("重新读取后配置丢失：%+v", reloaded)
	}
}

// 与 Microsoft 会话视图同样的要求：token 绝不出站。
func TestGoogleSessionViewNeverExposesTokens(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	session, err := state.SaveGoogleSession(domain.GoogleSession{
		Email:        "user@gmail.com",
		ClientID:     "cid",
		AccessToken:  "super-secret-access",
		RefreshToken: "super-secret-refresh",
	})
	if err != nil {
		t.Fatalf("保存会话失败：%v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/google-accounts/"+session.AccountID, nil)
	request.SetPathValue("id", session.AccountID)
	recorder := httptest.NewRecorder()
	server.handleGoogleAccount(recorder, request)

	raw := recorder.Body.String()
	if strings.Contains(raw, "super-secret-access") || strings.Contains(raw, "super-secret-refresh") {
		t.Fatalf("会话视图泄露了令牌：%s", raw)
	}
	data := decodeAPIData(t, recorder)
	view, _ := data["session"].(map[string]any)
	if view == nil {
		t.Fatalf("缺少 session 字段：%+v", data)
	}
	if view["has_refresh_token"] != true || view["has_access_token"] != true {
		t.Fatalf("应只暴露「有没有」而不是令牌本身：%+v", view)
	}
}

func TestGoogleAccountDetailRejectsNonGoogleAccount(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	microsoft, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "m@outlook.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("保存 Microsoft 会话失败：%v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/google-accounts/"+microsoft.AccountID, nil)
	request.SetPathValue("id", microsoft.AccountID)
	recorder := httptest.NewRecorder()
	server.handleGoogleAccount(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("非 Google 账号应返回 404，实际 %d", recorder.Code)
	}
	if code := decodeAPIError(t, recorder); code != "google_account_not_found" {
		t.Fatalf("错误码不正确：%q", code)
	}
}

func TestGoogleAccountDeleteRemovesSession(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	session, err := state.SaveGoogleSession(domain.GoogleSession{Email: "user@gmail.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("保存会话失败：%v", err)
	}

	request := httptest.NewRequest(http.MethodDelete, "/api/google-accounts/"+session.AccountID, nil)
	request.SetPathValue("id", session.AccountID)
	recorder := httptest.NewRecorder()
	server.handleGoogleAccountDelete(recorder, request)
	decodeAPIData(t, recorder)

	if len(state.GoogleAccounts()) != 0 {
		t.Fatal("删除后账号仍存在")
	}
	if _, ok := state.GoogleSessionByAccountID(session.AccountID); ok {
		t.Fatal("删除后会话仍存在")
	}
}

// + 地址是免费 Gmail 唯一的多地址手段，这条路径必须真的产出可收信的地址。
func TestGoogleAliasGenerateBuildsPlusAddress(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	session, err := state.SaveGoogleSession(domain.GoogleSession{Email: "user@gmail.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("保存会话失败：%v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/google-accounts/"+session.AccountID+"/aliases/generate",
		strings.NewReader(`{"tag":"Shop","label":"购物","note":""}`))
	request.SetPathValue("id", session.AccountID)
	recorder := httptest.NewRecorder()
	server.handleGoogleAliasGenerate(recorder, request)
	data := decodeAPIData(t, recorder)

	mailbox, _ := data["mailbox"].(map[string]any)
	if mailbox == nil {
		t.Fatalf("缺少 mailbox 字段：%+v", data)
	}
	if mailbox["email"] != "user+shop@gmail.com" {
		t.Fatalf("应生成小写的 + 地址：%+v", mailbox)
	}
	if mailbox["provider"] != domain.ProviderGoogle {
		t.Fatalf("邮箱服务商应为 google：%+v", mailbox)
	}
	if mailbox["api_token"] != nil && mailbox["api_token"] != "" {
		t.Fatal("创建的邮箱不应带出 API 令牌")
	}
}

func TestGoogleAliasGenerateRejectsBadTag(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	session, err := state.SaveGoogleSession(domain.GoogleSession{Email: "user@gmail.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("保存会话失败：%v", err)
	}

	for _, body := range []string{`{"tag":""}`, `{"tag":"a+b"}`, `{"tag":"a@b"}`} {
		request := httptest.NewRequest(http.MethodPost, "/api/google-accounts/"+session.AccountID+"/aliases/generate", strings.NewReader(body))
		request.SetPathValue("id", session.AccountID)
		recorder := httptest.NewRecorder()
		server.handleGoogleAliasGenerate(recorder, request)
		if recorder.Code == http.StatusOK {
			t.Fatalf("非法标签 %s 应被拒绝", body)
		}
	}
}

func TestGoogleMailboxCreateAddsAddressForAccount(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	session, err := state.SaveGoogleSession(domain.GoogleSession{Email: "user@gmail.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("保存会话失败：%v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/google-accounts/"+session.AccountID+"/mailboxes",
		strings.NewReader(`{"email":" User+Extra@Gmail.com ","label":"测试","note":"备注"}`))
	request.SetPathValue("id", session.AccountID)
	recorder := httptest.NewRecorder()
	server.handleGoogleMailboxCreate(recorder, request)
	data := decodeAPIData(t, recorder)

	mailbox, _ := data["mailbox"].(map[string]any)
	if mailbox == nil {
		t.Fatalf("缺少 mailbox 字段：%+v", data)
	}
	if mailbox["email"] != "user+extra@gmail.com" {
		t.Fatalf("地址应归一为小写：%+v", mailbox)
	}
}

func TestGoogleMailboxCreateRejectsBadInput(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	session, err := state.SaveGoogleSession(domain.GoogleSession{Email: "user@gmail.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("保存会话失败：%v", err)
	}

	for _, body := range []string{`{"email":""}`, `{"email":"not-an-email"}`, `{"email":"a@b@c.com"}`} {
		request := httptest.NewRequest(http.MethodPost, "/api/google-accounts/"+session.AccountID+"/mailboxes", strings.NewReader(body))
		request.SetPathValue("id", session.AccountID)
		recorder := httptest.NewRecorder()
		server.handleGoogleMailboxCreate(recorder, request)
		if recorder.Code == http.StatusOK {
			t.Fatalf("非法地址 %s 应被拒绝", body)
		}
	}
}

// 三个服务商共用一张账号表，列表接口必须严格隔离，否则 Google 账号会漏进 Apple 页面。
func TestGoogleAccountListIsIsolatedFromOtherProviders(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Config{Port: 8788, GoogleEnabled: true})

	google, err := state.SaveGoogleSession(domain.GoogleSession{Email: "g@gmail.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("保存 Google 会话失败：%v", err)
	}
	if _, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "m@outlook.com", RefreshToken: "r"}); err != nil {
		t.Fatalf("保存 Microsoft 会话失败：%v", err)
	}

	recorder := httptest.NewRecorder()
	server.handleGoogleAccounts(recorder, httptest.NewRequest(http.MethodGet, "/api/google-accounts", nil))
	data := decodeAPIData(t, recorder)

	items, _ := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("Google 账号列表应只有 1 条：%+v", items)
	}
	first, _ := items[0].(map[string]any)
	if first["id"] != google.AccountID {
		t.Fatalf("列表内容不正确：%+v", first)
	}

	recorder = httptest.NewRecorder()
	server.handleAppleAccounts(recorder, httptest.NewRequest(http.MethodGet, "/api/apple-accounts", nil))
	appleData := decodeAPIData(t, recorder)
	if items, _ := appleData["items"].([]any); len(items) != 0 {
		t.Fatalf("Apple 列表不应包含 Google/Microsoft 账号：%+v", items)
	}
}
