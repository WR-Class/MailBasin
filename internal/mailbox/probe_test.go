package mailbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icloud-privacy-mail-v2/internal/config"
	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
)

// seedIMAPMailbox 放一个带完整 IMAP 连接信息的邮箱，作为体检的输入。
func seedIMAPMailbox(t *testing.T, service *Service, email, secret string) domain.Mailbox {
	t.Helper()
	mailbox, _, err := service.store.UpsertMailboxFromRemote("", domain.RemoteMailbox{
		Provider: domain.ProviderIMAP, Email: email, Label: email, IsActive: true, Origin: "imap",
		IMAPHost: "imap.example.com", IMAPPort: 993, IMAPUsername: email, IMAPSecret: secret,
	}, "")
	if err != nil {
		t.Fatalf("放置 IMAP 邮箱失败：%v", err)
	}
	return mailbox
}

// 体检必须把三种处境分开报：登录不了、登录得了但收件箱是空的、登录得了且确实有信。
// 混成一个「收信数 0」是这套判断最容易出错的地方——0 同时表示这三种完全不同的情况。
func TestProbeMailboxesSeparatesUnreadableFromEmpty(t *testing.T) {
	service := newIMAPFixture(t)
	service.imapInspect = func(_ context.Context, _ string, _ int, username, _ string) (protocol.GenericIMAPStat, error) {
		switch username {
		case "broken@example.com":
			return protocol.GenericIMAPStat{}, protocol.NewCodedError("imap_login_failed", "IMAP 登录失败，请确认密码填的是客户端授权码", false)
		case "empty@example.com":
			return protocol.GenericIMAPStat{Exists: 0, UIDNext: 11}, nil
		default:
			return protocol.GenericIMAPStat{Exists: 7, UIDNext: 99}, nil
		}
	}
	full := seedIMAPMailbox(t, service, "full@example.com", "secret")
	broken := seedIMAPMailbox(t, service, "broken@example.com", "stale-secret")
	empty := seedIMAPMailbox(t, service, "empty@example.com", "secret")

	// 重复的标识只该占一行。
	report := service.ProbeMailboxes(context.Background(), []string{full.ID, broken.ID, empty.ID, full.ID})
	if len(report.Items) != 3 {
		t.Fatalf("重复标识应去重成 3 行，实际 %d 行", len(report.Items))
	}
	if report.Checked != 3 || report.Unreadable != 1 || report.Readable != 2 {
		t.Fatalf("应为 3 个已检查（1 读不了 / 2 能读），实际 %+v", report)
	}
	// 读不了的排最前，那是唯一需要动手的部分；失败原因要原样带到界面，否则不知道改什么。
	if report.Items[0].ID != broken.ID || report.Items[0].Readable {
		t.Fatalf("读不了的应排最前且不该算成能读，实际 %+v", report.Items[0])
	}
	if report.Items[0].Code != "imap_login_failed" || !strings.Contains(report.Items[0].Reason, "授权码") {
		t.Fatalf("失败原因应如实带回，实际 %+v", report.Items[0])
	}
	byEmail := map[string]MailboxProbeItem{}
	for _, item := range report.Items {
		byEmail[item.Email] = item
	}
	if got := byEmail["empty@example.com"]; !got.Readable || got.RemoteExists != 0 {
		t.Fatalf("能登录但空收件箱应报「能读、0 封」，实际 %+v", got)
	}
	// 服务器上有 7 封就要报出 7 封；而从未同步过的收信数 0 是「不知道」，不是「没有邮件」。
	if got := byEmail["full@example.com"]; !got.Readable || got.RemoteExists != 7 || !got.NeverChecked {
		t.Fatalf("能登录且有 7 封应如实报出并标出从未检查过，实际 %+v", got)
	}
	// 判定之后要顺手摘掉：报告没人看的时候，「能不能用」得从池子的状态上就看得出来。
	if report.Demoted != 1 || !byEmail["broken@example.com"].Demoted {
		t.Fatalf("登录被拒的那个应被摘掉，实际 %+v", report)
	}
	if got := byEmail["broken@example.com"]; got.PreviousStatus != domain.StatusAvailable {
		t.Fatalf("应记下摘之前的状态，实际 %q", got.PreviousStatus)
	}
	// 摘掉要落到记录上，不只是写在报告里。
	if stored, _ := service.store.FindMailboxByID(broken.ID); stored.Status != domain.StatusFailed {
		t.Fatalf("记录里的状态应已改成失败，实际 %q", stored.Status)
	}
	// 没坏的一个都不许动。
	for _, id := range []string{full.ID, empty.ID} {
		if stored, _ := service.store.FindMailboxByID(id); stored.Status != domain.StatusAvailable {
			t.Fatalf("%s 不该被改动，实际 %q", id, stored.Status)
		}
	}
}

// 探不到的服务商要如实标成未覆盖，不能混进「正常」里，也不能被打上一个没有根据的
// 「读不了」——使用者会照着它去下架一批其实正常的邮箱。而「读不了却仍标着可用、仍在池里」
// 的必须被单独标出来：邮箱池只从可用且未隐藏的记录里挑人，这一条会被真的投放出去，
// 然后永远等不到验证码。
func TestProbeMailboxesStaysHonestAboutCoverageAndOfferedDeadMailboxes(t *testing.T) {
	service := newIMAPFixture(t)
	service.imapInspect = func(context.Context, string, int, string, string) (protocol.GenericIMAPStat, error) {
		return protocol.GenericIMAPStat{}, protocol.NewCodedError("imap_login_failed", "IMAP 登录失败", false)
	}
	broken := seedIMAPMailbox(t, service, "broken@example.com", "stale-secret")
	outlook := seedMailbox(t, service, "alias@outlook.com", domain.ProviderMicrosoft)
	google := seedMailbox(t, service, "alias@gmail.com", domain.ProviderGoogle)

	report := service.ProbeMailboxes(context.Background(), []string{broken.ID, "mbx_missing", outlook.ID, google.ID})
	if report.UnreadableButOffered != 1 {
		t.Fatalf("应恰好有 1 个「读不了却仍会被投放」，实际 %d", report.UnreadableButOffered)
	}
	if report.NotCovered != 2 || report.Missing != 1 || report.Checked != 1 {
		t.Fatalf("两个非 IMAP 邮箱该报未覆盖、一个标识该报本地无记录，实际 %+v", report)
	}
	if report.Demoted != 1 {
		t.Fatalf("登录被拒的那个应被摘掉，实际 %+v", report)
	}
	byID := map[string]MailboxProbeItem{}
	for _, item := range report.Items {
		byID[item.ID] = item
	}
	if !byID[broken.ID].OfferedWhileUnreadable {
		t.Fatal("读不了、标着可用、又在池里的邮箱应被标出来")
	}
	// 不存在的标识也要占一行并写明原因。悄悄丢掉会让「检查了 4 个只回来 3 行」这种账对不上，
	// 而账对不上就没人敢信这张表。
	if got := byID["mbx_missing"]; got.Code != "mailbox_not_found" {
		t.Fatalf("不存在的标识应如实占一行，实际 %+v", got)
	}
	for _, item := range []MailboxProbeItem{byID[outlook.ID], byID[google.ID]} {
		// 这两个没有关联母号，所以查不了。报的码从 provider_not_covered 换成了
		// account_missing：微软和谷歌现在是被覆盖的（走母号那份授权），这两条只是缺母号，
		// 那是一句更准的话。旧的码留着会把「我们查不了这条记录」说成「这类服务商我们不查」。
		if item.Covered || item.Readable || item.OfferedWhileUnreadable || item.Code != "account_missing" {
			t.Fatalf("没有母号的记录该报「缺母号、未覆盖」，实际 %+v", item)
		}
	}
}

// 外部取码邮箱的判定走取件接口：接口可达就是能读，有没有码是另一件事。
func TestProbeMailboxesCoversExternalCodeMailboxes(t *testing.T) {
	// 强制直连：本机代理会把打到回环地址的请求也劫走（表现为 502），那样验的是代理而不是
	// 这段分派逻辑。解析不出的代理地址会退回直连，见 NewHTTPClient。
	service := NewService(config.Config{GoogleProxy: "direct"}, openSyncTestStore(t))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"code":"no_code","data":{"code":"no_code"}}`))
	}))
	defer server.Close()
	mailbox, _, err := service.store.UpsertMailboxFromRemote("", domain.RemoteMailbox{
		Provider: domain.ProviderExternal, Email: "ext@icloud.com", Label: "ext@icloud.com",
		IsActive: true, Origin: "external_code_api", CodeAPIURL: server.URL + "/code",
	}, "")
	if err != nil {
		t.Fatalf("放置外部取码邮箱失败：%v", err)
	}

	report := service.ProbeMailboxes(context.Background(), []string{mailbox.ID})
	if len(report.Items) != 1 || !report.Items[0].Covered || !report.Items[0].Readable {
		t.Fatalf("取码接口可达时应报能读，实际 %+v", report.Items)
	}
	// 「接口可达但没有码」是正常状态。写成失败的话，一个失效的令牌会一直伪装成
	// 「还没有收到」，而使用者会去等一个永远不会来的码。
	if item := report.Items[0]; !strings.Contains(item.Reason, "没有验证码") || !item.NeverChecked {
		t.Fatalf("应如实写成「可达但没有验证码」，并标出从未检查过，实际 %+v", item)
	}
}

// 对照臂：网络类失败一律不下架。拿一次拨号失败或超时下架，等于用一次网络抖动
// 摘掉一个本来能用的邮箱——坏邮箱只是白等，被摘掉的好邮箱连等都等不到。
func TestProbeMailboxesDoesNotDemoteOnTransientNetworkFailures(t *testing.T) {
	service := newIMAPFixture(t)
	service.imapInspect = func(context.Context, string, int, string, string) (protocol.GenericIMAPStat, error) {
		return protocol.GenericIMAPStat{}, protocol.NewCodedError("imap_dial_failed", "连接邮件服务器失败", true)
	}
	mailbox := seedIMAPMailbox(t, service, "flaky@example.com", "secret")

	report := service.ProbeMailboxes(context.Background(), []string{mailbox.ID})
	if report.Demoted != 0 || report.Items[0].Demoted {
		t.Fatalf("网络类失败不该下架，实际 %+v", report.Items[0])
	}
	// 但仍然要如实说它读不了、而且还在池里会被投放——不下架不等于不报告。
	if report.Items[0].Readable || !report.Items[0].OfferedWhileUnreadable {
		t.Fatalf("应如实报出读不了且仍会被投放，实际 %+v", report.Items[0])
	}
	if stored, _ := service.store.FindMailboxByID(mailbox.ID); stored.Status != domain.StatusAvailable {
		t.Fatalf("状态不该被改动，实际 %q", stored.Status)
	}
}

// 手设过的状态不该被一次体检改写：那可能是有人特意停用的，也可能是正在出借的。
// 改状态会顺带释放租约，所以碰错了等于把邮箱从正在用它的任务手里拽走。
func TestProbeMailboxesLeavesHandSetStatusAlone(t *testing.T) {
	service := newIMAPFixture(t)
	service.imapInspect = func(context.Context, string, int, string, string) (protocol.GenericIMAPStat, error) {
		return protocol.GenericIMAPStat{}, protocol.NewCodedError("imap_login_failed", "IMAP 登录失败", false)
	}
	mailbox := seedIMAPMailbox(t, service, "paused@example.com", "stale-secret")
	if _, err := service.store.SetMailboxStatus(mailbox.ID, nil, nil, domain.StatusDisabled, nil); err != nil {
		t.Fatalf("先把它设成停用失败：%v", err)
	}

	report := service.ProbeMailboxes(context.Background(), []string{mailbox.ID})
	if report.Demoted != 0 {
		t.Fatalf("已经停用的邮箱不该被体检改写，实际 %+v", report.Items[0])
	}
	if stored, _ := service.store.FindMailboxByID(mailbox.ID); stored.Status != domain.StatusDisabled {
		t.Fatalf("状态应仍是停用，实际 %q", stored.Status)
	}
	if report.Items[0].Readable {
		t.Fatal("它确实读不了，这一点仍要如实报出")
	}
}

// 同一个错误码会因为「可重试」标记不同而意思完全相反，判定必须看那个标记。
//
// 这条测试补的是上一版漏掉的维度：上一版的对照臂用的是另一个码 imap_dial_failed，
// 而真正会出事的场合是同一个码 imap_login_failed——「登录请求失败」（网络）和
// 「登录被拒」（凭据错）共用它。只变错误码、不变标记的对照臂，恰好绕开了这个缺陷。
func TestProbeMailboxesJudgesByRetryableFlagNotOnlyByCode(t *testing.T) {
	cases := []struct {
		name      string
		code      string
		retryable bool
		wantOut   bool // 是否应被摘出池子
	}{
		{"登录被拒（不可重试）该摘", "imap_login_failed", false, true},
		{"登录请求失败（可重试）不该摘", "imap_login_failed", true, false},
		{"连接失败（可重试）不该摘", "imap_dial_failed", true, false},
		{"欢迎信息失败（可重试）不该摘", "imap_greeting_failed", true, false},
		{"打开收件箱失败（可重试）不该摘", "imap_select_failed", true, false},
		{"缺授权码（不可重试）该摘", "imap_secret_missing", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := newIMAPFixture(t)
			service.imapInspect = func(context.Context, string, int, string, string) (protocol.GenericIMAPStat, error) {
				return protocol.GenericIMAPStat{}, protocol.NewCodedError(tc.code, "模拟失败", tc.retryable)
			}
			mailbox := seedIMAPMailbox(t, service, "case@example.com", "secret")

			report := service.ProbeMailboxes(context.Background(), []string{mailbox.ID})
			item := report.Items[0]
			if item.Demoted != tc.wantOut {
				t.Fatalf("码=%s 可重试=%v：摘除应为 %v，实际 %+v", tc.code, tc.retryable, tc.wantOut, item)
			}
			// 标记本身也要如实报出来，否则界面无法解释「为什么这一次没有动它」。
			if item.Retryable != tc.retryable {
				t.Fatalf("报告里应带上可重试标记 %v，实际 %+v", tc.retryable, item)
			}
			want := domain.StatusAvailable
			if tc.wantOut {
				want = domain.StatusFailed
			}
			if stored, _ := service.store.FindMailboxByID(mailbox.ID); stored.Status != want {
				t.Fatalf("记录状态应为 %q，实际 %q", want, stored.Status)
			}
		})
	}
}

// seedAccountMailbox 放一个挂在某个母号名下的地址。微软、谷歌、苹果这三家的地址都是
// 这种形状：自己没有凭据，读信靠母号那一份授权。没有显式设过隐藏标记，所以它默认在池里
// ——这正是需要被筛掉的那一类。
func seedAccountMailbox(t *testing.T, service *Service, accountID, provider, email string) domain.Mailbox {
	t.Helper()
	mailbox, _, err := service.store.UpsertMailboxFromRemote(accountID, domain.RemoteMailbox{
		Provider: provider, Email: email, Label: email, IsActive: true, Origin: provider,
	}, "")
	if err != nil {
		t.Fatalf("放置 %s 邮箱失败：%v", provider, err)
	}
	return mailbox
}

// 母号的授权被明确拒绝时，名下每个还能投放的地址都要被摘出去；母号只是网络不通时，
// 一个都不许动。这两支必须各有对照，否则「不误摘」只是一句话。
func TestProbeMailboxesDemotesEveryAddressOfARejectedAccountOnly(t *testing.T) {
	cases := []struct {
		name    string
		verdict probeAccountVerdict
		wantOut int
	}{
		{
			name: "母号授权被拒：名下两个地址都该摘",
			verdict: probeAccountVerdict{
				covered: true, code: "microsoft_token_rejected",
				reason: "Microsoft 授权已被拒绝，请重新登录账号", retryable: false,
			},
			wantOut: 2,
		},
		{
			name: "母号只是网络不通：一个都不该摘",
			verdict: probeAccountVerdict{
				covered: true, code: "microsoft_channel_unreachable",
				reason: "读取账号资料失败（HTTP 503）", retryable: true,
			},
			wantOut: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := newIMAPFixture(t)
			service.accountChannelCheck = func(context.Context, string, string) probeAccountVerdict {
				return tc.verdict
			}
			first := seedAccountMailbox(t, service, "acct-1", domain.ProviderMicrosoft, "one@outlook.com")
			second := seedAccountMailbox(t, service, "acct-1", domain.ProviderMicrosoft, "two@outlook.com")

			report := service.ProbeMailboxes(context.Background(), []string{first.ID, second.ID})
			if report.Demoted != tc.wantOut {
				t.Fatalf("摘除数应为 %d，实际 %d：%+v", tc.wantOut, report.Demoted, report.Items)
			}
			for _, item := range report.Items {
				if item.Level != probeLevelAccount || item.AccountID != "acct-1" {
					t.Fatalf("结论应标明来自哪个母号，实际 %+v", item)
				}
				// 界面上要能看出这一条不是按地址单独测出来的。
				if !strings.Contains(item.Reason, "来自母号") {
					t.Fatalf("原因里应说清结论来自母号，实际 %q", item.Reason)
				}
			}
			want := domain.StatusAvailable
			if tc.wantOut > 0 {
				want = domain.StatusFailed
			}
			for _, mailbox := range []domain.Mailbox{first, second} {
				if stored, _ := service.store.FindMailboxByID(mailbox.ID); stored.Status != want {
					t.Fatalf("%s 的状态应为 %q，实际 %q", mailbox.Email, want, stored.Status)
				}
			}
		})
	}
}

// 同一个母号下的多个地址只该被查一次：五个别名查五遍，只是让对面多收四次同样的请求，
// 还可能给出互相矛盾的结论——同一份授权不可能既是好的又是坏的。
func TestProbeMailboxesChecksOneAccountOncePerBatch(t *testing.T) {
	service := newIMAPFixture(t)
	calls := 0
	service.accountChannelCheck = func(context.Context, string, string) probeAccountVerdict {
		calls++
		return probeAccountVerdict{covered: true, readable: true, code: "google_channel_ok", reason: "母号授权可用"}
	}
	ids := make([]string, 0, 3)
	for _, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		ids = append(ids, seedAccountMailbox(t, service, "acct-9", domain.ProviderGoogle, email).ID)
	}

	report := service.ProbeMailboxes(context.Background(), ids)
	if calls != 1 {
		t.Fatalf("同一个母号应只查一次，实际查了 %d 次", calls)
	}
	if report.Readable != 3 || report.Unreadable != 0 {
		t.Fatalf("三个地址都该算能读，实际 %+v", report)
	}
}

// 够不到的通道要报成「本次未覆盖」，不能报成「读不了」：只配了网页通道的苹果母号，
// 名下地址照样能取到验证码，按读不了处理会白白摘掉好地址。
func TestProbeMailboxesReportsAnUnreachableChannelAsNotCovered(t *testing.T) {
	service := newIMAPFixture(t)
	service.accountChannelCheck = func(context.Context, string, string) probeAccountVerdict {
		return probeAccountVerdict{
			covered: false, code: "icloud_imap_not_covered",
			reason: "这个母号没有配 IMAP 取码通道",
		}
	}
	mailbox := seedAccountMailbox(t, service, "acct-3", domain.ProviderICloud, "hidden@icloud.com")

	report := service.ProbeMailboxes(context.Background(), []string{mailbox.ID})
	if report.NotCovered != 1 || report.Unreadable != 0 || report.Demoted != 0 {
		t.Fatalf("应记成 1 个未覆盖、0 个读不了、0 个摘除，实际 %+v", report)
	}
	if report.Items[0].Readable {
		t.Fatal("够不到就不该说它能读")
	}
	if stored, _ := service.store.FindMailboxByID(mailbox.ID); stored.Status != domain.StatusAvailable {
		t.Fatalf("够不到不该改状态，实际 %q", stored.Status)
	}
}

// 三个服务商要各自走到自己那条通道上，不能都落到苹果那条。
func TestProbeMailboxesRoutesEachProviderToItsOwnChannel(t *testing.T) {
	service := newIMAPFixture(t)
	seen := map[string]int{}
	service.accountChannelCheck = func(_ context.Context, provider, accountID string) probeAccountVerdict {
		seen[provider+"|"+accountID]++
		return probeAccountVerdict{covered: true, readable: true, code: "channel_ok", reason: "正常"}
	}
	ids := []string{
		seedAccountMailbox(t, service, "ms-1", domain.ProviderMicrosoft, "m@outlook.com").ID,
		seedAccountMailbox(t, service, "gg-1", domain.ProviderGoogle, "g@gmail.com").ID,
		seedAccountMailbox(t, service, "ap-1", domain.ProviderICloud, "a@icloud.com").ID,
	}

	service.ProbeMailboxes(context.Background(), ids)
	for _, key := range []string{"microsoft|ms-1", "google|gg-1", "icloud|ap-1"} {
		if seen[key] != 1 {
			t.Fatalf("%s 应被查一次，实际 %d 次（全部：%v）", key, seen[key], seen)
		}
	}
}

// 记录没有关联母号时，如实说查不了，不替它下结论、也不去查通道、更不改状态。
func TestProbeMailboxesSaysWhenThereIsNoParentAccount(t *testing.T) {
	service := newIMAPFixture(t)
	calls := 0
	service.accountChannelCheck = func(context.Context, string, string) probeAccountVerdict {
		calls++
		return probeAccountVerdict{covered: true, readable: true, code: "channel_ok", reason: "正常"}
	}
	mailbox, _, err := service.store.UpsertMailboxFromRemote("", domain.RemoteMailbox{
		Provider: domain.ProviderMicrosoft, Email: "orphan@outlook.com", IsActive: true, Origin: "microsoft",
	}, "")
	if err != nil {
		t.Fatalf("放置邮箱失败：%v", err)
	}

	report := service.ProbeMailboxes(context.Background(), []string{mailbox.ID})
	if calls != 0 {
		t.Fatalf("没有母号就不该去查通道，实际查了 %d 次", calls)
	}
	if report.Items[0].Code != "account_missing" || report.Items[0].Covered {
		t.Fatalf("应报成「没有母号、本次未覆盖」，实际 %+v", report.Items[0])
	}
	if report.Demoted != 0 {
		t.Fatalf("没有母号不该改状态，实际摘了 %d 个", report.Demoted)
	}
}
