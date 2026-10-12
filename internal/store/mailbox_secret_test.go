package store

import (
	"path/filepath"
	"testing"

	"icloud-privacy-mail-v2/internal/domain"
)

// 展示路径不能把凭据发给上层，同步路径必须还能读到它。
//
// Mailbox 同时当落库格式和接口返回格式用，json 标签挡不住凭据：只要有一个读路径
// 忘了清，使用者的邮箱密码就会原样出现在浏览器的接口响应里。这条测试同时钉住
// 另一头——按 ID 重读的那条路必须还拿得到授权码，否则修完泄露就把收信弄坏了。
func TestMailboxReadPathsStripSecretsButSyncPathKeepsThem(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("创建临时状态失败：%v", err)
	}
	// 不关掉的话，Windows 上 t.TempDir 收尾会删不掉还被占用的 app.db，
	// 整条测试会因为收尾失败而报 FAIL——断言全过了也一样。
	t.Cleanup(func() { _ = state.Close() })
	const secret = "abcdefghijklmnop"
	created, _, err := state.UpsertMailboxFromRemote("acc_fixture", domain.RemoteMailbox{
		Provider:     domain.ProviderIMAP,
		Email:        "someone@gmail.com",
		Label:        "someone@gmail.com",
		IsActive:     true,
		IMAPHost:     "imap.gmail.com",
		IMAPPort:     993,
		IMAPUsername: "someone@gmail.com",
		IMAPSecret:   secret,
	}, "")
	if err != nil {
		t.Fatalf("创建测试邮箱失败：%v", err)
	}

	// 同步路径：SyncIMAPMailbox 就是按 ID 重读这个邮箱拿授权码的。
	loaded, ok := state.FindMailboxByID(created.ID)
	if !ok {
		t.Fatalf("同步路径读不到邮箱 %s", created.ID)
	}
	if loaded.IMAPSecret != secret {
		t.Fatalf("同步路径必须能读到授权码，实际 %q", loaded.IMAPSecret)
	}

	// 展示路径：三条都不能带凭据出去。
	paths := map[string][]domain.Mailbox{
		"MailboxesByAccount":        state.MailboxesByAccount("acc_fixture"),
		"MailboxesGroupedByAccount": state.MailboxesGroupedByAccount()["acc_fixture"],
		"Mailboxes":                 state.Mailboxes("", "", "", false, 1, 50).Items,
	}
	for name, mailboxes := range paths {
		if len(mailboxes) == 0 {
			t.Fatalf("%s 没返回任何邮箱，这条断言其实什么都没测", name)
		}
		for _, mailbox := range mailboxes {
			if mailbox.IMAPSecret != "" {
				t.Fatalf("%s 把授权码发给了上层：%q", name, mailbox.IMAPSecret)
			}
		}
	}
}
