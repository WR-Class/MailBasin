package mailbox

import (
	"strings"
	"testing"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
)

func newMicrosoftBatchFixture(t *testing.T) (*Service, string) {
	t.Helper()
	state := openSyncTestStore(t)
	session, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "user@outlook.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("创建 Microsoft 账号失败：%v", err)
	}
	return NewService(config.Config{}, state), session.AccountID
}

// 跨批次不重复是核心承诺：这次建 100 个、下次建 1000 个都不能撞上之前建过的。
func TestCreateMicrosoftAliasesBatchNeverRepeatsAcrossRuns(t *testing.T) {
	service, accountID := newMicrosoftBatchFixture(t)

	first, err := service.CreateMicrosoftAliases(accountID, "shop", 100, "", "")
	if err != nil {
		t.Fatalf("第一批生成失败：%v", err)
	}
	if first.Created != 100 {
		t.Fatalf("第一批应创建 100 个，实际 %d（跳过 %d）", first.Created, first.Skipped)
	}

	second, err := service.CreateMicrosoftAliases(accountID, "", 1000, "", "")
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

// 生成的地址必须落在正确的域名上，且带上 + 标签。
func TestCreateMicrosoftAliasesBatchProducesPlusAddresses(t *testing.T) {
	service, accountID := newMicrosoftBatchFixture(t)
	result, err := service.CreateMicrosoftAliases(accountID, "shop", 5, "", "")
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	for _, email := range result.Addresses {
		if !strings.HasPrefix(email, "user+shop-") || !strings.HasSuffix(email, "@outlook.com") {
			t.Fatalf("地址格式不正确：%s", email)
		}
	}
	// 生成的邮箱要真的能在池里查到——否则收信时匹配不上。
	for _, email := range result.Addresses {
		if _, ok := service.store.FindMailboxByEmail(email); !ok {
			t.Fatalf("生成的地址没有进入邮箱池：%s", email)
		}
	}
}

func TestCreateMicrosoftAliasesBatchRejectsOversizedCount(t *testing.T) {
	service, accountID := newMicrosoftBatchFixture(t)
	if _, err := service.CreateMicrosoftAliases(accountID, "", MicrosoftAliasBatchMax+1, "", ""); err == nil {
		t.Fatal("超过单次上限时必须报错")
	}
	if _, err := service.CreateMicrosoftAliases(accountID, "", 0, "", ""); err == nil {
		t.Fatal("数量为 0 时必须报错")
	}
}

func TestCreateMicrosoftAliasesBatchRejectsOversizedPrefix(t *testing.T) {
	service, accountID := newMicrosoftBatchFixture(t)
	_, err := service.CreateMicrosoftAliases(accountID, strings.Repeat("p", 60), 1, "", "")
	if err == nil {
		t.Fatal("前缀过长时必须报错")
	}
	if !strings.Contains(err.Error(), "太长") {
		t.Fatalf("错误信息应说明前缀过长：%v", err)
	}
}

// 两个服务商的批量生成共用同一个去重空间：微软生成时也必须避开已存在的 Gmail 地址。
func TestCreateMicrosoftAliasesBatchSharesUniquenessSpace(t *testing.T) {
	state := openSyncTestStore(t)
	google, err := state.SaveGoogleSession(domain.GoogleSession{Email: "user@gmail.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("创建 Google 账号失败：%v", err)
	}
	microsoft, err := state.SaveMicrosoftSession(domain.MicrosoftSession{Email: "user@outlook.com", RefreshToken: "r"})
	if err != nil {
		t.Fatalf("创建 Microsoft 账号失败：%v", err)
	}
	service := NewService(config.Config{}, state)

	if _, err := service.CreateGoogleAliases(google.AccountID, "taken", 20, "", ""); err != nil {
		t.Fatalf("Google 批量生成失败：%v", err)
	}
	result, err := service.CreateMicrosoftAliases(microsoft.AccountID, "taken", 20, "", "")
	if err != nil {
		t.Fatalf("Microsoft 批量生成失败：%v", err)
	}
	// 域名不同，所以两边不该有交集；但都必须在同一个池子里各占一条。
	for _, email := range result.Addresses {
		if strings.HasSuffix(email, "@gmail.com") {
			t.Fatalf("微软生成结果里出现了 Gmail 地址：%s", email)
		}
	}
	all := map[string]int{}
	for _, mailbox := range service.store.AllMailboxes() {
		all[strings.ToLower(mailbox.Email)]++
	}
	if len(all) != 40 {
		t.Fatalf("两个账号各 20 个地址，池里应有 40 个不同地址，实际 %d", len(all))
	}
}
