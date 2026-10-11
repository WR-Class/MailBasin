package store

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func openMicrosoftTestStore(t *testing.T) *Store {
	t.Helper()
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return state
}

func TestMigrationV6CreatesMicrosoftSessionTableAndProviderIndexes(t *testing.T) {
	state := openMicrosoftTestStore(t)
	var count int
	if err := state.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("读取迁移记录失败：%v", err)
	}
	if count != databaseSchemaVersion || databaseSchemaVersion < 6 {
		t.Fatalf("迁移版本不正确：count=%d version=%d", count, databaseSchemaVersion)
	}
	var name string
	if err := state.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'microsoft_sessions'`).Scan(&name); err != nil {
		t.Fatalf("microsoft_sessions 表未创建：%v", err)
	}
	for _, index := range []string{"idx_accounts_provider", "idx_mailboxes_provider"} {
		if err := state.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&name); err != nil {
			t.Fatalf("索引 %s 未创建：%v", index, err)
		}
	}
}

func TestSaveMicrosoftSessionCreatesProviderScopedAccount(t *testing.T) {
	state := openMicrosoftTestStore(t)
	expires := time.Now().Add(time.Hour)
	saved, err := state.SaveMicrosoftSession(domain.MicrosoftSession{
		Email:          "user@outlook.com",
		DisplayName:    "User",
		Tenant:         "common",
		ClientID:       "client-1",
		AccessToken:    "access-token-value",
		RefreshToken:   "refresh-token-value",
		TokenExpiresAt: expires,
		Scope:          "Mail.Read User.Read offline_access",
		LastCheckedAt:  time.Now(),
		LastCheckOK:    true,
	})
	if err != nil {
		t.Fatalf("保存 Microsoft 会话失败：%v", err)
	}
	if saved.AccountID == "" {
		t.Fatal("保存 Microsoft 会话后应生成账号 ID")
	}
	if saved.Status != domain.MicrosoftStatusActive {
		t.Fatalf("状态应为 active，实际 %q", saved.Status)
	}

	account, ok := state.FindAppleAccount(saved.AccountID)
	if !ok {
		t.Fatal("应创建对应的账号记录")
	}
	if account.ProviderOrDefault() != domain.ProviderMicrosoft {
		t.Fatalf("账号服务商应为 microsoft，实际 %q", account.ProviderOrDefault())
	}
	if account.AppleID != "user@outlook.com" || account.ICloudStatus != domain.MicrosoftStatusActive {
		t.Fatalf("账号展示字段不正确：%+v", account)
	}
	if account.Password != "" {
		t.Fatal("Microsoft 账号不应保存密码字段")
	}

	loaded, ok := state.MicrosoftSessionByAccountID(saved.AccountID)
	if !ok {
		t.Fatal("应能按账号 ID 读回 Microsoft 会话")
	}
	if loaded.AccessToken != "access-token-value" || loaded.RefreshToken != "refresh-token-value" || loaded.Email != "user@outlook.com" {
		t.Fatalf("会话读回内容不正确：%+v", loaded)
	}
	if delta := loaded.TokenExpiresAt.Sub(expires); delta > time.Second || delta < -time.Second {
		t.Fatalf("令牌过期时间未保留：%v", loaded.TokenExpiresAt)
	}
}

// 这条用例守住的是一个真实修过的漏洞：SSE 事件载荷对任何订阅者可见，
// 而会话表只承载凭据，一旦整行进入载荷就等于把 OAuth2 令牌广播出去。
func TestMicrosoftSessionEncryptsTokensAndKeepsThemOutOfChangeEvents(t *testing.T) {
	state := openMicrosoftTestStore(t)
	saved, err := state.SaveMicrosoftSession(domain.MicrosoftSession{
		Email:        "secret@outlook.com",
		AccessToken:  "plain-access-token",
		RefreshToken: "plain-refresh-token",
	})
	if err != nil {
		t.Fatalf("保存 Microsoft 会话失败：%v", err)
	}

	var raw []byte
	if err := state.db.QueryRow(`SELECT data_json FROM microsoft_sessions WHERE id = ?`, saved.AccountID).Scan(&raw); err != nil {
		t.Fatalf("读取会话原始数据失败：%v", err)
	}
	if bytes.Contains(raw, []byte("plain-access-token")) || bytes.Contains(raw, []byte("plain-refresh-token")) {
		t.Fatalf("Microsoft 令牌未加密落库：%s", raw)
	}
	if !bytes.Contains(raw, []byte(secretPrefix)) {
		t.Fatalf("Microsoft 令牌应使用加密前缀存储：%s", raw)
	}

	rows, err := state.db.Query(`SELECT event_type, payload_json FROM change_log WHERE event_type LIKE 'microsoft-session%'`)
	if err != nil {
		t.Fatalf("读取变更日志失败：%v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var eventType string
		var payload []byte
		if err := rows.Scan(&eventType, &payload); err != nil {
			t.Fatalf("扫描变更日志失败：%v", err)
		}
		seen++
		if bytes.Contains(payload, []byte("plain-access-token")) || bytes.Contains(payload, []byte("plain-refresh-token")) {
			t.Fatalf("变更事件泄露了 OAuth2 令牌：%s %s", eventType, payload)
		}
		var decoded map[string]any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("事件载荷不是合法 JSON：%v", err)
		}
		if _, ok := decoded["data"]; ok {
			t.Fatalf("会话事件不应携带行数据：%s", payload)
		}
	}
	if seen == 0 {
		t.Fatal("保存 Microsoft 会话应产生变更事件")
	}

	// 加密不能破坏可用性：明文必须仍能读回。
	loaded, ok := state.MicrosoftSessionByAccountID(saved.AccountID)
	if !ok || loaded.AccessToken != "plain-access-token" || loaded.RefreshToken != "plain-refresh-token" {
		t.Fatalf("加密后读回不正确：%+v", loaded)
	}
}

// redactSecrets 是第二道防线：即便将来有会话表漏进载荷，凭据字段也会被删掉。
func TestRedactSecretsStripsCredentialFields(t *testing.T) {
	payload := map[string]any{
		"email":         "user@outlook.com",
		"access_token":  "a",
		"refresh_token": "b",
		"id_token":      "c",
		"client_secret": "d",
		"nested":        map[string]any{"refresh_token": "e", "keep": "ok"},
		"list":          []any{map[string]any{"access_token": "f", "keep": "ok"}},
	}
	redactSecrets(payload)
	encoded, _ := json.Marshal(payload)
	for _, secret := range []string{`"a"`, `"b"`, `"c"`, `"d"`, `"e"`, `"f"`} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("凭据字段未被清除：%s", encoded)
		}
	}
	for _, kept := range []string{"user@outlook.com", "ok"} {
		if !bytes.Contains(encoded, []byte(kept)) {
			t.Fatalf("非敏感字段被误删：%s", encoded)
		}
	}
}

func TestAccountsSeparatesICloudAndMicrosoftProviders(t *testing.T) {
	state := openMicrosoftTestStore(t)
	icloud, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "me@icloud.com", DSID: "111"})
	if err != nil {
		t.Fatalf("保存 iCloud 会话失败：%v", err)
	}
	microsoft, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "me@outlook.com", RefreshToken: "refresh"})
	if err != nil {
		t.Fatalf("保存 Microsoft 会话失败：%v", err)
	}

	all := state.Accounts("")
	if len(all) != 2 {
		t.Fatalf("不带服务商筛选应返回全部账号，实际 %d 个", len(all))
	}
	onlyICloud := state.Accounts(domain.ProviderICloud)
	if len(onlyICloud) != 1 || onlyICloud[0].ID != icloud.AccountID {
		t.Fatalf("iCloud 筛选结果不正确：%+v", onlyICloud)
	}
	onlyMicrosoft := state.Accounts(domain.ProviderMicrosoft)
	if len(onlyMicrosoft) != 1 || onlyMicrosoft[0].ID != microsoft.AccountID {
		t.Fatalf("Microsoft 筛选结果不正确：%+v", onlyMicrosoft)
	}
	if list := state.AppleAccounts(); len(list) != 1 || list[0].ID != icloud.AccountID {
		t.Fatalf("AppleAccounts() 应只返回 iCloud 账号：%+v", list)
	}
	if list := state.MicrosoftAccounts(); len(list) != 1 || list[0].ID != microsoft.AccountID {
		t.Fatalf("MicrosoftAccounts() 结果不正确：%+v", list)
	}
	for _, account := range all {
		if account.Password != "" {
			t.Fatalf("账号列表不应返回密码：%+v", account)
		}
	}
}

// 空 provider 必须继续被解释为 icloud，否则升级会把已有账号全部丢出列表。
func TestLegacyAccountWithoutProviderStaysICloud(t *testing.T) {
	state := openMicrosoftTestStore(t)
	session, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "legacy@icloud.com", DSID: "999"})
	if err != nil {
		t.Fatalf("保存 iCloud 会话失败：%v", err)
	}
	if _, err := state.db.Exec(`UPDATE apple_accounts SET data_json = json_remove(data_json, '$.provider')`); err != nil {
		t.Fatalf("准备旧数据失败：%v", err)
	}
	account, ok := state.FindAppleAccount(session.AccountID)
	if !ok {
		t.Fatal("旧账号应仍可读取")
	}
	if account.ProviderOrDefault() != domain.ProviderICloud {
		t.Fatalf("空 provider 应归一化为 icloud，实际 %q", account.ProviderOrDefault())
	}
	if list := state.AppleAccounts(); len(list) != 1 || list[0].ID != session.AccountID {
		t.Fatalf("旧账号应仍出现在 AppleAccounts() 中：%+v", list)
	}
}

func TestMicrosoftSessionDeduplicatesByEmail(t *testing.T) {
	state := openMicrosoftTestStore(t)
	first, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "dup@outlook.com", RefreshToken: "first-refresh"})
	if err != nil {
		t.Fatalf("首次保存失败：%v", err)
	}
	second, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "DUP@outlook.com", RefreshToken: "second-refresh"})
	if err != nil {
		t.Fatalf("二次保存失败：%v", err)
	}
	if first.AccountID != second.AccountID {
		t.Fatalf("同一邮箱应复用账号记录：%s != %s", first.AccountID, second.AccountID)
	}
	if accounts := state.Accounts(domain.ProviderMicrosoft); len(accounts) != 1 {
		t.Fatalf("重复授权不应产生多个账号：%d", len(accounts))
	}
	loaded, _ := state.MicrosoftSessionByAccountID(first.AccountID)
	if loaded.RefreshToken != "second-refresh" {
		t.Fatalf("重复授权应覆盖令牌，实际 %q", loaded.RefreshToken)
	}
}

func TestDashboardCountsProvidersSeparately(t *testing.T) {
	state := openMicrosoftTestStore(t)
	if _, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "a@icloud.com", DSID: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "b@icloud.com", DSID: "2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "c@outlook.com", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	dashboard := state.Dashboard()
	if dashboard.AppleAccountCount != 2 {
		t.Fatalf("iCloud 账号计数应为 2，实际 %d", dashboard.AppleAccountCount)
	}
	if dashboard.MicrosoftAccountCount != 1 {
		t.Fatalf("Microsoft 账号计数应为 1，实际 %d", dashboard.MicrosoftAccountCount)
	}
}

func TestDeleteMicrosoftAccountRemovesSessionAndMailboxes(t *testing.T) {
	state := openMicrosoftTestStore(t)
	session, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "gone@outlook.com", RefreshToken: "r"})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{
		Email: "alias@outlook.com", IsActive: true, Provider: domain.ProviderMicrosoft,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if mailbox.ProviderOrDefault() != domain.ProviderMicrosoft {
		t.Fatalf("远程邮箱的服务商应随账号写入：%q", mailbox.ProviderOrDefault())
	}
	result, err := state.DeleteAppleAccount(session.AccountID)
	if err != nil {
		t.Fatalf("删除 Microsoft 账号失败：%v", err)
	}
	if result.Mailboxes == 0 {
		t.Fatalf("删除账号应同时清理别名：%+v", result)
	}
	if _, ok := state.MicrosoftSessionByAccountID(session.AccountID); ok {
		t.Fatal("删除账号后不应残留 Microsoft 会话")
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); ok {
		t.Fatal("删除账号后不应残留别名邮箱")
	}
}

func TestMicrosoftStatusFromSessionCoversFailureModes(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		session domain.MicrosoftSession
		want    string
	}{
		{"无凭据需要重新登录", domain.MicrosoftSession{}, domain.MicrosoftStatusNeedLogin},
		{"校验通过为正常", domain.MicrosoftSession{RefreshToken: "r", LastCheckedAt: now, LastCheckOK: true}, domain.MicrosoftStatusActive},
		{"校验失败为失败", domain.MicrosoftSession{RefreshToken: "r", LastCheckedAt: now, LastCheckOK: false}, domain.MicrosoftStatusFailed},
		{"令牌过期且未校验为过期", domain.MicrosoftSession{RefreshToken: "r", TokenExpiresAt: now.Add(-time.Hour)}, domain.MicrosoftStatusExpired},
		{"有刷新令牌未校验为正常", domain.MicrosoftSession{RefreshToken: "r"}, domain.MicrosoftStatusActive},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := MicrosoftStatusFromSession(item.session); got != item.want {
				t.Fatalf("状态推导错误：got=%q want=%q", got, item.want)
			}
		})
	}
}
