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
		if item.Covered || item.Readable || item.OfferedWhileUnreadable || item.Code != "provider_not_covered" {
			t.Fatalf("未覆盖的邮箱不该有任何判定，实际 %+v", item)
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
