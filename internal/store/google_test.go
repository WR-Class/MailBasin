package store

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestMigrationV7CreatesGoogleSessionTable(t *testing.T) {
	state := openMicrosoftTestStore(t)

	var count int
	if err := state.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("读取迁移记录失败：%v", err)
	}
	if count != databaseSchemaVersion || databaseSchemaVersion < 7 {
		t.Fatalf("迁移版本不正确：count=%d version=%d", count, databaseSchemaVersion)
	}

	var name string
	if err := state.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'google_sessions'`).Scan(&name); err != nil {
		t.Fatalf("google_sessions 表未创建：%v", err)
	}
	if err := state.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_google_sessions_email'`).Scan(&name); err != nil {
		t.Fatalf("idx_google_sessions_email 索引未创建：%v", err)
	}

	// v6 建立的表不能被 v7 顺手破坏。
	if err := state.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'microsoft_sessions'`).Scan(&name); err != nil {
		t.Fatalf("迁移 v7 之后 microsoft_sessions 表消失了：%v", err)
	}
}

func TestSaveGoogleSessionCreatesProviderScopedAccount(t *testing.T) {
	state := openMicrosoftTestStore(t)
	expires := time.Now().Add(time.Hour)
	saved, err := state.SaveGoogleSession(domain.GoogleSession{
		Email:          "user@gmail.com",
		DisplayName:    "User",
		ClientID:       "client-1.apps.googleusercontent.com",
		AccessToken:    "access-token-value",
		RefreshToken:   "refresh-token-value",
		TokenExpiresAt: expires,
		Scope:          "https://www.googleapis.com/auth/gmail.readonly",
		LastCheckedAt:  time.Now(),
		LastCheckOK:    true,
	})
	if err != nil {
		t.Fatalf("保存 Google 会话失败：%v", err)
	}
	if saved.AccountID == "" {
		t.Fatal("保存 Google 会话后应生成账号 ID")
	}
	if saved.Status != domain.GoogleStatusActive {
		t.Fatalf("状态应为 active，实际 %q", saved.Status)
	}

	account, ok := state.FindAppleAccount(saved.AccountID)
	if !ok {
		t.Fatal("应创建对应的账号记录")
	}
	if account.ProviderOrDefault() != domain.ProviderGoogle {
		t.Fatalf("账号服务商应为 google，实际 %q", account.ProviderOrDefault())
	}
	if account.AppleID != "user@gmail.com" {
		t.Fatalf("账号展示字段不正确：%+v", account)
	}
	if account.Password != "" {
		t.Fatal("Google 账号不应保存密码字段")
	}

	loaded, ok := state.GoogleSessionByAccountID(saved.AccountID)
	if !ok {
		t.Fatal("应能按账号 ID 读回 Google 会话")
	}
	if loaded.AccessToken != "access-token-value" || loaded.RefreshToken != "refresh-token-value" || loaded.Email != "user@gmail.com" {
		t.Fatalf("会话读回内容不正确：%+v", loaded)
	}
	if delta := loaded.TokenExpiresAt.Sub(expires); delta > time.Second || delta < -time.Second {
		t.Fatalf("令牌过期时间未保留：%v", loaded.TokenExpiresAt)
	}
}

// 这条用例守住的是一个真实修过的漏洞：SSE 事件载荷对任何订阅者可见，
// 而会话表只承载凭据，一旦整行进入载荷就等于把 OAuth2 令牌广播出去。
// Google 的 refresh token 有效期更长，泄露代价更高，所以这条同样必须成立。
func TestGoogleSessionEncryptsTokensAndKeepsThemOutOfChangeEvents(t *testing.T) {
	state := openMicrosoftTestStore(t)

	saved, err := state.SaveGoogleSession(domain.GoogleSession{
		Email:        "secret@gmail.com",
		AccessToken:  "plain-access-token",
		RefreshToken: "plain-refresh-token",
	})
	if err != nil {
		t.Fatalf("保存 Google 会话失败：%v", err)
	}

	var raw []byte
	if err := state.db.QueryRow(`SELECT data_json FROM google_sessions WHERE id = ?`, saved.AccountID).Scan(&raw); err != nil {
		t.Fatalf("读取会话原始数据失败：%v", err)
	}
	if bytes.Contains(raw, []byte("plain-access-token")) || bytes.Contains(raw, []byte("plain-refresh-token")) {
		t.Fatalf("Google 令牌未加密落库：%s", raw)
	}
	if !bytes.Contains(raw, []byte(secretPrefix)) {
		t.Fatalf("Google 令牌应使用加密前缀存储：%s", raw)
	}

	rows, err := state.db.Query(`SELECT event_type, payload_json FROM change_log WHERE event_type LIKE 'google-session%'`)
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
		t.Fatal("保存 Google 会话应产生变更事件")
	}
}

// 三个服务商共用一张账号表，所以"某个 provider 的列表"必须严格隔离。
// 这条用例是防止 Google 账号漏进 Apple 或 Microsoft 页面的唯一保证。
func TestGoogleAccountsAreIsolatedFromOtherProviders(t *testing.T) {
	state := openMicrosoftTestStore(t)

	google, err := state.SaveGoogleSession(domain.GoogleSession{Email: "g@gmail.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("保存 Google 会话失败：%v", err)
	}
	microsoft, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "m@outlook.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("保存 Microsoft 会话失败：%v", err)
	}

	if items := state.GoogleAccounts(); len(items) != 1 || items[0].ID != google.AccountID {
		t.Fatalf("Google 账号列表不正确：%+v", items)
	}
	if items := state.MicrosoftAccounts(); len(items) != 1 || items[0].ID != microsoft.AccountID {
		t.Fatalf("Microsoft 账号列表不正确：%+v", items)
	}
	if items := state.AppleAccounts(); len(items) != 0 {
		t.Fatalf("Apple 账号列表不应包含 Google/Microsoft 账号：%+v", items)
	}

	sessions := state.GoogleSessions()
	if len(sessions) != 1 || sessions[0].AccountID != google.AccountID {
		t.Fatalf("Google 会话列表不正确：%+v", sessions)
	}
}

// Google 在刷新令牌时经常不回传新的 refresh_token；此时绝不能把已有的清空，
// 否则用户每刷新一次访问令牌就会永久失去账号，只能重新授权。
func TestMergeGoogleSessionKeepsExistingRefreshTokenWhenIncomingIsEmpty(t *testing.T) {
	existing := domain.GoogleSession{
		AccountID:    "acct-1",
		Email:        "user@gmail.com",
		AccessToken:  "old-access",
		RefreshToken: "keep-me",
		Status:       domain.GoogleStatusActive,
	}
	incoming := domain.GoogleSession{
		AccountID:   "acct-1",
		AccessToken: "new-access",
	}

	merged := mergeGoogleSession(existing, incoming)
	if merged.RefreshToken != "keep-me" {
		t.Fatalf("refresh token 被清空：%q", merged.RefreshToken)
	}
	if merged.AccessToken != "new-access" {
		t.Fatalf("access token 未更新：%q", merged.AccessToken)
	}
	if merged.Email != "user@gmail.com" {
		t.Fatalf("已有邮箱被清空：%q", merged.Email)
	}

	// 反过来，显式给了新的 refresh token 时必须采用它。
	incoming.RefreshToken = "new-refresh"
	if merged := mergeGoogleSession(existing, incoming); merged.RefreshToken != "new-refresh" {
		t.Fatalf("新的 refresh token 未被采用：%q", merged.RefreshToken)
	}
}

// 会话才是状态的真相源。Accounts() 会按会话重新推导账号上的展示状态，
// 所以只写账号行是留不住的——这条用例把这个契约钉住，避免以后有人
// 以为 SaveGoogleAccountStatus 单独就能改变界面上的状态。
func TestGoogleAccountDisplayStatusIsDerivedFromSession(t *testing.T) {
	state := openMicrosoftTestStore(t)

	saved, err := state.SaveGoogleSession(domain.GoogleSession{Email: "user@gmail.com", RefreshToken: "r", LastCheckOK: true})
	if err != nil {
		t.Fatalf("保存 Google 会话失败：%v", err)
	}
	if accounts := state.GoogleAccounts(); len(accounts) != 1 || accounts[0].ICloudStatus != domain.GoogleStatusActive {
		t.Fatalf("初始状态应为 active：%+v", accounts)
	}

	// 只改账号行：界面读到的仍然是会话推导出来的 active。
	if err := state.SaveGoogleAccountStatus(saved.AccountID, domain.GoogleStatusExpired, "仅账号行"); err != nil {
		t.Fatalf("保存 Google 账号状态失败：%v", err)
	}
	if accounts := state.GoogleAccounts(); accounts[0].ICloudStatus != domain.GoogleStatusActive {
		t.Fatalf("账号行不应盖过会话推导的状态：%q", accounts[0].ICloudStatus)
	}

	// 改会话：这才是真正驱动界面的那条路径。
	updated, err := state.UpdateGoogleSession(saved.AccountID, func(session *domain.GoogleSession) error {
		session.LastCheckedAt = time.Now()
		session.LastCheckOK = false
		session.LastStatusMessage = "刷新令牌已失效"
		return nil
	})
	if err != nil {
		t.Fatalf("更新 Google 会话失败：%v", err)
	}
	if updated.Status != domain.GoogleStatusFailed {
		t.Fatalf("会话状态未更新：%q", updated.Status)
	}
	if updated.LastStatusMessage != "刷新令牌已失效" {
		t.Fatalf("状态说明未保存：%q", updated.LastStatusMessage)
	}
	if updated.RefreshToken != "r" {
		t.Fatalf("更新状态不应动到凭据：%q", updated.RefreshToken)
	}

	accounts := state.GoogleAccounts()
	if accounts[0].ICloudStatus != domain.GoogleStatusFailed {
		t.Fatalf("账号展示状态未跟随会话：%q", accounts[0].ICloudStatus)
	}
}

// 这里刻意与 MicrosoftStatusFromSession 保持逐行同构：两边语义必须一致，
// 否则同一个"没登录"的账号在两个页面上会显示成不同的状态。
//
// 注意"只有 access_token"被判为 active 是刻意的——那枚令牌此刻确实还能用一小时，
// 真正失效时 CheckGoogleAccount 会把 LastCheckOK 置假，状态自然收敛到 failed。
func TestGoogleStatusFromSessionMatchesMicrosoftSemantics(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		session domain.GoogleSession
		want    string
	}{
		{"有刷新令牌且检查通过", domain.GoogleSession{RefreshToken: "r", LastCheckedAt: now, LastCheckOK: true}, domain.GoogleStatusActive},
		{"有刷新令牌但检查失败", domain.GoogleSession{RefreshToken: "r", LastCheckedAt: now, LastCheckOK: false}, domain.GoogleStatusFailed},
		{"只有访问令牌", domain.GoogleSession{AccessToken: "a"}, domain.GoogleStatusActive},
		{"完全没有令牌", domain.GoogleSession{}, domain.GoogleStatusNeedLogin},
		{"令牌已过期且未检查过", domain.GoogleSession{AccessToken: "a", TokenExpiresAt: now.Add(-time.Hour)}, domain.GoogleStatusExpired},
		{"令牌未过期且未检查过", domain.GoogleSession{AccessToken: "a", TokenExpiresAt: now.Add(time.Hour)}, domain.GoogleStatusActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := GoogleStatusFromSession(tc.session)
			if got != tc.want {
				t.Fatalf("状态应为 %q，实际 %q", tc.want, got)
			}
			// 同一份输入，Microsoft 的实现必须给出对应的答案。
			// 两边状态常量取值相同（active/failed/need_login/expired），可以直接比。
			peer := MicrosoftStatusFromSession(domain.MicrosoftSession{
				AccessToken:    tc.session.AccessToken,
				RefreshToken:   tc.session.RefreshToken,
				TokenExpiresAt: tc.session.TokenExpiresAt,
				LastCheckedAt:  tc.session.LastCheckedAt,
				LastCheckOK:    tc.session.LastCheckOK,
			})
			if peer != got {
				t.Fatalf("与 Microsoft 语义不一致：google=%q microsoft=%q", got, peer)
			}
		})
	}
}

func TestDashboardCountsGoogleAccountsSeparately(t *testing.T) {
	state := openMicrosoftTestStore(t)

	if _, err := state.SaveGoogleSession(domain.GoogleSession{Email: "g@gmail.com", RefreshToken: "r"}); err != nil {
		t.Fatalf("保存 Google 会话失败：%v", err)
	}
	if _, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "m@outlook.com", RefreshToken: "r"}); err != nil {
		t.Fatalf("保存 Microsoft 会话失败：%v", err)
	}

	dashboard := state.Dashboard()
	if dashboard.GoogleAccountCount != 1 {
		t.Fatalf("Google 账号计数应为 1，实际 %d", dashboard.GoogleAccountCount)
	}
	if dashboard.MicrosoftAccountCount != 1 {
		t.Fatalf("Microsoft 账号计数应为 1，实际 %d", dashboard.MicrosoftAccountCount)
	}
	if dashboard.AppleAccountCount != 0 {
		t.Fatalf("Apple 账号计数应为 0，实际 %d", dashboard.AppleAccountCount)
	}
}
