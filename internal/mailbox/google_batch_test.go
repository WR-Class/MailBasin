package mailbox

import (
	"strings"
	"testing"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
)

func newGoogleBatchFixture(t *testing.T) (*Service, string) {
	t.Helper()
	state := openSyncTestStore(t)
	session, err := state.SaveGoogleSession(domain.GoogleSession{Email: "user@gmail.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("创建 Google 账号失败：%v", err)
	}
	return NewService(config.Config{}, state), session.AccountID
}

// 跨批次不重复是这个功能的核心承诺：这次建 100 个、下次建 1000 个，
// 都不能撞上之前建过的任何一个。
func TestCreateGoogleAliasesBatchNeverRepeatsAcrossRuns(t *testing.T) {
	service, accountID := newGoogleBatchFixture(t)

	first, err := service.CreateGoogleAliases(accountID, "shop", 100, "", "")
	if err != nil {
		t.Fatalf("第一批生成失败：%v", err)
	}
	if first.Created != 100 {
		t.Fatalf("第一批应创建 100 个，实际 %d（跳过 %d）", first.Created, first.Skipped)
	}

	second, err := service.CreateGoogleAliases(accountID, "", 1000, "", "")
	if err != nil {
		t.Fatalf("第二批生成失败：%v", err)
	}
	if second.Created != 1000 {
		t.Fatalf("第二批应创建 1000 个，实际 %d（跳过 %d）", second.Created, second.Skipped)
	}

	seen := map[string]struct{}{}
	for _, email := range first.Addresses {
		seen[email] = struct{}{}
	}
	for _, email := range second.Addresses {
		if _, ok := seen[email]; ok {
			t.Fatalf("第二批重复了第一批的地址：%s", email)
		}
	}

	// 库里也必须是 1100 个互不相同的地址——重复写入会让这个数字偏小。
	counts := map[string]int{}
	for _, mailbox := range service.store.AllMailboxes() {
		counts[strings.ToLower(mailbox.Email)]++
	}
	if len(counts) != 1100 {
		t.Fatalf("邮箱池应有 1100 个不同地址，实际 %d", len(counts))
	}
	for email, count := range counts {
		if count > 1 {
			t.Fatalf("地址在库里重复了 %d 次：%s", count, email)
		}
	}
}

// 已存在的主地址也要算进去：手工建过的地址不能被随机生成再撞一次。
func TestCreateGoogleAliasesBatchAvoidsManuallyAddedAddresses(t *testing.T) {
	service, accountID := newGoogleBatchFixture(t)

	manual := "user+taken@gmail.com"
	if _, _, err := service.store.UpsertMailboxFromRemote(accountID, domain.RemoteMailbox{
		Provider: domain.ProviderGoogle,
		Email:    manual,
		Label:    manual,
		IsActive: true,
		Origin:   "google_manual",
	}, ""); err != nil {
		t.Fatalf("写入手工地址失败：%v", err)
	}

	result, err := service.CreateGoogleAliases(accountID, "taken", 20, "", "")
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	for _, email := range result.Addresses {
		if email == manual {
			t.Fatalf("生成结果撞上了已存在的手工地址：%s", email)
		}
	}
}

func TestCreateGoogleAliasesBatchRejectsOversizedCount(t *testing.T) {
	service, accountID := newGoogleBatchFixture(t)
	if _, err := service.CreateGoogleAliases(accountID, "", GoogleAliasBatchMax+1, "", ""); err == nil {
		t.Fatal("超过单次上限时必须报错，而不是真的去建这么多地址")
	}
	if _, err := service.CreateGoogleAliases(accountID, "", 0, "", ""); err == nil {
		t.Fatal("数量为 0 时必须报错")
	}
}

// 前缀本身就把长度吃光时要报错，而不是生成一批永远收不到信的地址。
func TestCreateGoogleAliasesBatchRejectsOversizedPrefix(t *testing.T) {
	service, accountID := newGoogleBatchFixture(t)
	_, err := service.CreateGoogleAliases(accountID, strings.Repeat("p", 60), 1, "", "")
	if err == nil {
		t.Fatal("前缀过长时必须报错")
	}
	if !strings.Contains(err.Error(), "太长") {
		t.Fatalf("错误信息应说明前缀过长：%v", err)
	}
}
