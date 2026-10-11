package store

import (
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func openAdminTestStore(t *testing.T) *Store {
	t.Helper()
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 数据库失败：%v", err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return state
}

// 重置必须保留记录 ID：会话和账号的 owner_id 都指向它，删了重建会留下悬空引用。
func TestResetAdminPasswordPreservesIDAndInvalidatesSessions(t *testing.T) {
	state := openAdminTestStore(t)
	created, err := state.SetupAdmin("admin", "pbkdf2_sha256$120000$b2xk$b2xkaGFzaA")
	if err != nil {
		t.Fatalf("初始化管理员失败：%v", err)
	}
	if err := state.SaveSession("session-one", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("写入会话失败：%v", err)
	}
	if err := state.SaveSession("session-two", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("写入会话失败：%v", err)
	}
	if _, ok := state.ValidateSession("session-one"); !ok {
		t.Fatal("重置前会话应有效")
	}

	updated, sessions, err := state.ResetAdminPassword("pbkdf2_sha256$120000$bmV3$bmV3aGFzaA")
	if err != nil {
		t.Fatalf("重置密码失败：%v", err)
	}
	if updated.ID != created.ID {
		t.Fatalf("重置后记录 ID 必须保持不变：%s -> %s", created.ID, updated.ID)
	}
	if updated.Username != created.Username {
		t.Fatalf("重置不应改动账号名：%s -> %s", created.Username, updated.Username)
	}
	if updated.PasswordHash != "pbkdf2_sha256$120000$bmV3$bmV3aGFzaA" {
		t.Fatalf("新散列未写入：%q", updated.PasswordHash)
	}
	if sessions != 2 {
		t.Fatalf("应作废 2 个会话，实际 %d", sessions)
	}
	// 改密码却不作废旧会话，等于没改。
	if _, ok := state.ValidateSession("session-one"); ok {
		t.Fatal("重置后旧会话必须失效")
	}
	if _, ok := state.ValidateSession("session-two"); ok {
		t.Fatal("重置后旧会话必须失效")
	}
	reloaded, ok := state.Admin()
	if !ok || reloaded.ID != created.ID || reloaded.PasswordHash != updated.PasswordHash {
		t.Fatalf("重置结果未落库：%+v", reloaded)
	}
	if !reloaded.UpdatedAt.After(created.CreatedAt) && reloaded.UpdatedAt.IsZero() {
		t.Fatal("重置应刷新 UpdatedAt")
	}
}

func TestResetAdminPasswordRequiresExistingAdmin(t *testing.T) {
	state := openAdminTestStore(t)
	if _, _, err := state.ResetAdminPassword("pbkdf2_sha256$120000$c2FsdA$a2V5"); err == nil {
		t.Fatal("尚未初始化管理员时应报错，而不是凭空创建一条记录")
	}
	if _, ok := state.Admin(); ok {
		t.Fatal("失败的调用不应创建管理员记录")
	}
}

func TestResetAdminPasswordRejectsEmptyHash(t *testing.T) {
	state := openAdminTestStore(t)
	if _, err := state.SetupAdmin("admin", "pbkdf2_sha256$120000$b2xk$b2xkaGFzaA"); err != nil {
		t.Fatalf("初始化管理员失败：%v", err)
	}
	if _, _, err := state.ResetAdminPassword("   "); err == nil {
		t.Fatal("空散列应被拒绝")
	}
	// 失败的调用不能把原密码清掉，否则账号会彻底登不进去。
	admin, ok := state.Admin()
	if !ok || admin.PasswordHash != "pbkdf2_sha256$120000$b2xk$b2xkaGFzaA" {
		t.Fatalf("失败的调用不应改动原凭据：%+v", admin)
	}
}

// 重置不能牵连业务数据：账号、邮箱和 owner_id 引用都要原样保留。
func TestResetAdminPasswordLeavesAccountsAndOwnershipIntact(t *testing.T) {
	state := openAdminTestStore(t)
	created, err := state.SetupAdmin("admin", "pbkdf2_sha256$120000$b2xk$b2xkaGFzaA")
	if err != nil {
		t.Fatalf("初始化管理员失败：%v", err)
	}
	session, err := state.SaveICloudSession(domain.ICloudSession{AppleID: "keep@icloud.com", DSID: "keep-dsid"})
	if err != nil {
		t.Fatalf("创建账号失败：%v", err)
	}
	mailbox, _, err := state.UpsertMailboxFromRemote(session.AccountID, domain.RemoteMailbox{Email: "keep-alias@icloud.com", IsActive: true}, "")
	if err != nil {
		t.Fatalf("创建邮箱失败：%v", err)
	}

	if _, _, err := state.ResetAdminPassword("pbkdf2_sha256$120000$bmV3$bmV3aGFzaA"); err != nil {
		t.Fatalf("重置密码失败：%v", err)
	}

	if _, ok := state.FindAppleAccount(session.AccountID); !ok {
		t.Fatal("重置密码不应删除账号")
	}
	if _, ok := state.FindMailboxByID(mailbox.ID); !ok {
		t.Fatal("重置密码不应删除邮箱")
	}
	account, _ := state.FindAppleAccount(session.AccountID)
	if account.OwnerID != created.ID {
		t.Fatalf("账号的 owner_id 应仍指向原管理员：%q != %q", account.OwnerID, created.ID)
	}
}

// 重置要留下可审计的事件记录，事后能查清凭据是什么时候被换掉的。
func TestResetAdminPasswordRecordsEvent(t *testing.T) {
	state := openAdminTestStore(t)
	if _, err := state.SetupAdmin("admin", "pbkdf2_sha256$120000$b2xk$b2xkaGFzaA"); err != nil {
		t.Fatalf("初始化管理员失败：%v", err)
	}
	if _, _, err := state.ResetAdminPassword("pbkdf2_sha256$120000$bmV3$bmV3aGFzaA"); err != nil {
		t.Fatalf("重置密码失败：%v", err)
	}
	events := state.Dashboard().Events
	found := false
	for _, event := range events {
		if event.Category == "auth" && event.Message == "已重置管理员密码，全部登录态已作废" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("应记录一条 auth 事件，实际：%+v", events)
	}
}
