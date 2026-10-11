package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

func newMicrosoftAPIFixture(t *testing.T, cfg config.Config) (*Server, *store.Store) {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建测试数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return New(cfg, state, slog.New(slog.NewTextHandler(io.Discard, nil))), state
}

func decodeAPIData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
		Code    string         `json:"code"`
		Message string         `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON：%s", recorder.Body.String())
	}
	if !payload.Success {
		t.Fatalf("接口返回失败：code=%s message=%s", payload.Code, payload.Message)
	}
	return payload.Data
}

func decodeAPIError(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Success bool   `json:"success"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON：%s", recorder.Body.String())
	}
	return payload.Code
}

// 两个服务商共用一张账号表，接口层必须各自只返回自己那一边。
func TestAccountEndpointsSeparateProviders(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Default())
	icloudSession, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "me@icloud.com", DSID: "111"})
	if err != nil {
		t.Fatalf("创建 iCloud 账号失败：%v", err)
	}
	microsoftSession, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "me@outlook.com", RefreshToken: "refresh"})
	if err != nil {
		t.Fatalf("创建 Microsoft 账号失败：%v", err)
	}

	recorder := httptest.NewRecorder()
	server.handleMicrosoftAccounts(recorder, httptest.NewRequest(http.MethodGet, "/api/microsoft-accounts", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("读取 Microsoft 账号失败：%d %s", recorder.Code, recorder.Body.String())
	}
	data := decodeAPIData(t, recorder)
	items, _ := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("Microsoft 接口应只返回 Microsoft 账号：%+v", items)
	}
	if id, _ := items[0].(map[string]any)["id"].(string); id != microsoftSession.AccountID {
		t.Fatalf("Microsoft 接口返回了错误的账号：%s", id)
	}

	recorder = httptest.NewRecorder()
	server.handleAppleAccounts(recorder, httptest.NewRequest(http.MethodGet, "/api/apple-accounts", nil))
	data = decodeAPIData(t, recorder)
	items, _ = data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("Apple 接口应只返回 iCloud 账号：%+v", items)
	}
	if id, _ := items[0].(map[string]any)["id"].(string); id != icloudSession.AccountID {
		t.Fatalf("Apple 接口返回了错误的账号：%s", id)
	}
}

func TestMicrosoftStatusReportsConfigAndDefaults(t *testing.T) {
	server, _ := newMicrosoftAPIFixture(t, config.Default())
	recorder := httptest.NewRecorder()
	server.handleMicrosoftStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/microsoft/status", nil))
	data := decodeAPIData(t, recorder)

	if data["enabled"] != true || data["watcher_enabled"] != true {
		t.Fatalf("默认配置下模块应开启：%+v", data)
	}
	// 未配置 client_id 时应回退到内置公共客户端，并如实标注。
	if data["using_default_client"] != true {
		t.Fatalf("未配置 client_id 时应标记为使用内置客户端：%+v", data)
	}
	if data["client_id"] != protocol.DefaultMicrosoftClientID {
		t.Fatalf("client_id 应回退到内置值：%v", data["client_id"])
	}
	if data["tenant"] != "common" {
		t.Fatalf("tenant 默认值不正确：%v", data["tenant"])
	}
	// Graph 不提供创建别名的接口，这个限制必须如实暴露给前端。
	if note, _ := data["alias_limit_note"].(string); !strings.Contains(note, "Graph") || !strings.Contains(note, "10") {
		t.Fatalf("应说明别名创建限制：%q", note)
	}
}

// 界面里保存的值优先于 config.json，否则用户改了却看不到效果。
func TestMicrosoftSettingsOverrideConfigFile(t *testing.T) {
	cfg := config.Default()
	cfg.MicrosoftClientID = "from-config-file"
	cfg.MicrosoftTenant = "config-tenant"
	server, state := newMicrosoftAPIFixture(t, cfg)

	recorder := httptest.NewRecorder()
	server.handleMicrosoftStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/microsoft/status", nil))
	if data := decodeAPIData(t, recorder); data["client_id"] != "from-config-file" || data["tenant"] != "config-tenant" {
		t.Fatalf("未保存界面设置时应回退到配置文件：%+v", data)
	}

	body := `{"client_id":" ui-client-id ","tenant":" ui-tenant ","watcher_enabled":false}`
	recorder = httptest.NewRecorder()
	server.handleMicrosoftSettings(recorder, httptest.NewRequest(http.MethodPut, "/api/microsoft/settings", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("保存 Microsoft 设置失败：%d %s", recorder.Code, recorder.Body.String())
	}
	data := decodeAPIData(t, recorder)
	if data["client_id"] != "ui-client-id" || data["tenant"] != "ui-tenant" || data["watcher_enabled"] != false {
		t.Fatalf("界面设置未生效：%+v", data)
	}
	if data["using_default_client"] != false {
		t.Fatalf("已配置 client_id 时不应再标记为内置客户端：%+v", data)
	}
	persisted := state.Settings()
	if persisted.MicrosoftClientID != "ui-client-id" || persisted.MicrosoftTenant != "ui-tenant" || persisted.EnableMicrosoftMailWatcher {
		t.Fatalf("设置未落库：%+v", persisted)
	}
}

// 会话视图是出站边界：token 绝不能出现在响应里。
func TestMicrosoftSessionViewNeverExposesTokens(t *testing.T) {
	view := microsoftSessionView(domain.MicrosoftSession{
		AccountID:    "acc-1",
		Email:        "user@outlook.com",
		AccessToken:  "super-secret-access",
		RefreshToken: "super-secret-refresh",
		Scope:        "Mail.Read",
	}, true)
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("序列化会话视图失败：%v", err)
	}
	if strings.Contains(string(encoded), "super-secret") {
		t.Fatalf("会话视图泄露了令牌：%s", encoded)
	}
	if view["has_access_token"] != true || view["has_refresh_token"] != true {
		t.Fatalf("应只暴露令牌是否存在：%+v", view)
	}
	if view["email"] != "user@outlook.com" {
		t.Fatalf("非敏感字段应保留：%+v", view)
	}
	if missing := microsoftSessionView(domain.MicrosoftSession{}, false); missing["exists"] != false {
		t.Fatalf("不存在的会话应标记 exists=false：%+v", missing)
	}
}

func TestMicrosoftAccountDetailRejectsNonMicrosoftAccount(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Default())
	icloudSession, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "me@icloud.com", DSID: "111"})
	if err != nil {
		t.Fatalf("创建 iCloud 账号失败：%v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/microsoft-accounts/"+icloudSession.AccountID, nil)
	request.SetPathValue("id", icloudSession.AccountID)
	recorder := httptest.NewRecorder()
	server.handleMicrosoftAccount(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("iCloud 账号不应能从 Microsoft 接口读到：%d %s", recorder.Code, recorder.Body.String())
	}
	if code := decodeAPIError(t, recorder); code != "microsoft_account_not_found" {
		t.Fatalf("错误码不正确：%s", code)
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/microsoft-accounts/"+icloudSession.AccountID, nil)
	request.SetPathValue("id", icloudSession.AccountID)
	recorder = httptest.NewRecorder()
	server.handleMicrosoftAccountDelete(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("iCloud 账号不应能从 Microsoft 接口删除：%d", recorder.Code)
	}
	if _, ok := state.FindAppleAccount(icloudSession.AccountID); !ok {
		t.Fatal("被拒绝的删除请求不应真的删掉 iCloud 账号")
	}
}

func TestMicrosoftAccountDeleteRemovesSessionAndAliases(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Default())
	session, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "gone@outlook.com", RefreshToken: "refresh"})
	if err != nil {
		t.Fatalf("创建 Microsoft 账号失败：%v", err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "alias@outlook.com", IsActive: true, Provider: domain.ProviderMicrosoft}, "")
	if err != nil {
		t.Fatalf("创建别名失败：%v", err)
	}

	request := httptest.NewRequest(http.MethodDelete, "/api/microsoft-accounts/"+session.AccountID, nil)
	request.SetPathValue("id", session.AccountID)
	recorder := httptest.NewRecorder()
	server.handleMicrosoftAccountDelete(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("删除 Microsoft 账号失败：%d %s", recorder.Code, recorder.Body.String())
	}
	data := decodeAPIData(t, recorder)
	deleted, _ := data["deleted"].(map[string]any)
	if deleted == nil {
		t.Fatalf("删除结果应包含清理统计：%+v", data)
	}
	if _, ok := state.MicrosoftSessionByAccountID(session.AccountID); ok {
		t.Fatal("删除后不应残留 Microsoft 会话")
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); ok {
		t.Fatal("删除后不应残留别名")
	}
}

// 手工添加别名是唯一可用的"创建"路径，因为 Graph 不提供创建接口。
func TestMicrosoftMailboxCreateAddsAliasForAccount(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Default())
	session, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "owner@outlook.com", RefreshToken: "refresh"})
	if err != nil {
		t.Fatalf("创建 Microsoft 账号失败：%v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/microsoft-accounts/"+session.AccountID+"/mailboxes", strings.NewReader(`{"email":" alias@outlook.com ","label":"测试","note":"备注"}`))
	request.SetPathValue("id", session.AccountID)
	recorder := httptest.NewRecorder()
	server.handleMicrosoftMailboxCreate(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("添加别名失败：%d %s", recorder.Code, recorder.Body.String())
	}
	data := decodeAPIData(t, recorder)
	mailbox, _ := data["mailbox"].(map[string]any)
	if mailbox == nil || mailbox["email"] != "alias@outlook.com" {
		t.Fatalf("别名未正确写入：%+v", data)
	}
	if mailbox["api_token"] != nil && mailbox["api_token"] != "" {
		t.Fatalf("响应不应回显邮箱 API Token：%+v", mailbox)
	}
	if data["created"] != true {
		t.Fatalf("首次添加应标记为新建：%+v", data)
	}
	stored, ok := state.FindMailboxByEmail("alias@outlook.com")
	if !ok || stored.ProviderOrDefault() != domain.ProviderMicrosoft || stored.AccountID != session.AccountID {
		t.Fatalf("别名未绑定到正确的 Microsoft 账号：%+v", stored)
	}
}

func TestMicrosoftMailboxCreateRejectsBadInput(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Default())
	session, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "owner@outlook.com", RefreshToken: "refresh"})
	if err != nil {
		t.Fatalf("创建 Microsoft 账号失败：%v", err)
	}
	cases := []struct {
		name string
		body string
	}{
		{"空邮箱", `{"email":""}`},
		{"非法邮箱", `{"email":"not-an-email"}`},
		{"非法 JSON", `{`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/microsoft-accounts/"+session.AccountID+"/mailboxes", strings.NewReader(item.body))
			request.SetPathValue("id", session.AccountID)
			recorder := httptest.NewRecorder()
			server.handleMicrosoftMailboxCreate(recorder, request)
			if recorder.Code == http.StatusOK {
				t.Fatalf("非法输入不应成功：%s", recorder.Body.String())
			}
		})
	}
	if mailboxes := state.AllMailboxes(); len(mailboxes) != 0 {
		t.Fatalf("失败的请求不应写入邮箱：%+v", mailboxes)
	}
}

// 未完成的设备码登录只存在于内存里，必须能被轮询、也能被过期清理。
func TestMicrosoftLoginPollHandlesUnknownAndExpiredSessions(t *testing.T) {
	server, _ := newMicrosoftAPIFixture(t, config.Default())

	recorder := httptest.NewRecorder()
	server.handleMicrosoftLoginPoll(recorder, httptest.NewRequest(http.MethodPost, "/api/microsoft-accounts/login/poll", strings.NewReader(`{"login_id":"missing"}`)))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未知登录会话应返回 404：%d %s", recorder.Code, recorder.Body.String())
	}
	if code := decodeAPIError(t, recorder); code != "microsoft_login_not_found" {
		t.Fatalf("错误码不正确：%s", code)
	}

	recorder = httptest.NewRecorder()
	server.handleMicrosoftLoginPoll(recorder, httptest.NewRequest(http.MethodPost, "/api/microsoft-accounts/login/poll", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("缺少 login_id 应返回 400：%d", recorder.Code)
	}

	server.microsoftLoginMu.Lock()
	server.microsoftLogins["expired"] = domain.MicrosoftDeviceLogin{
		DeviceCode: "device", UserCode: "CODE", ExpiresAt: time.Now().Add(-time.Minute),
	}
	server.microsoftLogins["live"] = domain.MicrosoftDeviceLogin{
		DeviceCode: "device", UserCode: "CODE", ExpiresAt: time.Now().Add(time.Minute),
	}
	server.microsoftLoginMu.Unlock()

	recorder = httptest.NewRecorder()
	server.handleMicrosoftLoginPoll(recorder, httptest.NewRequest(http.MethodPost, "/api/microsoft-accounts/login/poll", strings.NewReader(`{"login_id":"expired"}`)))
	if recorder.Code != http.StatusGone {
		t.Fatalf("过期设备码应返回 410：%d %s", recorder.Code, recorder.Body.String())
	}
	if code := decodeAPIError(t, recorder); code != "microsoft_devicecode_expired" {
		t.Fatalf("错误码不正确：%s", code)
	}

	// 过期会话应被顺手清掉，未过期的必须保留。
	server.microsoftLoginMu.Lock()
	server.pruneMicrosoftLoginsLocked()
	_, expiredStillThere := server.microsoftLogins["expired"]
	_, liveStillThere := server.microsoftLogins["live"]
	server.microsoftLoginMu.Unlock()
	if expiredStillThere {
		t.Fatal("过期登录会话应被清理")
	}
	if !liveStillThere {
		t.Fatal("未过期登录会话不应被清理")
	}
}

func TestMicrosoftLoginStartRespectsDisabledModule(t *testing.T) {
	cfg := config.Default()
	cfg.MicrosoftEnabled = false
	server, _ := newMicrosoftAPIFixture(t, cfg)

	recorder := httptest.NewRecorder()
	server.handleMicrosoftLoginStart(recorder, httptest.NewRequest(http.MethodPost, "/api/microsoft-accounts/login/start", nil))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("模块关闭时应拒绝发起登录：%d %s", recorder.Code, recorder.Body.String())
	}
	if code := decodeAPIError(t, recorder); code != "microsoft_disabled" {
		t.Fatalf("错误码不正确：%s", code)
	}
	server.microsoftLoginMu.Lock()
	pending := len(server.microsoftLogins)
	server.microsoftLoginMu.Unlock()
	if pending != 0 {
		t.Fatal("被拒绝的登录不应留下内存会话")
	}
}

// 仪表盘必须把两个服务商的账号数分开统计，否则界面上的数字会互相污染。
func TestDashboardEndpointCountsProvidersSeparately(t *testing.T) {
	server, state := newMicrosoftAPIFixture(t, config.Default())
	if _, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "a@icloud.com", DSID: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "b@outlook.com", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.handleDashboard(recorder, httptest.NewRequest(http.MethodGet, "/api/dashboard", nil))
	data := decodeAPIData(t, recorder)
	if data["apple_account_count"] != float64(1) || data["microsoft_account_count"] != float64(1) {
		t.Fatalf("仪表盘计数不正确：%+v", data)
	}
}
