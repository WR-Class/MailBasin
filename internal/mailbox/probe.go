package mailbox

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
)

const (
	// MailboxProbeMaxBatch 是一次请求最多体检几个邮箱。
	// 批量由前端分批循环驱动（和已有的批量同步同一个约定）：一个几分钟不响应的请求
	// 既会被浏览器或代理掐断，也会让界面在整段时间里没有任何进度。
	MailboxProbeMaxBatch = 20
	// mailboxProbeWorkers 是单个请求内的并发数。刻意定得不高：对面是邮件服务商的
	// 登录接口，一次同时开太多连接，会把「正常」也探成「失败」——那比探得慢糟得多。
	mailboxProbeWorkers = 4
	// mailboxProbeTimeout 是单个邮箱的体检预算。
	mailboxProbeTimeout = 30 * time.Second
)

// MailboxProbeItem 是一个邮箱的体检结论。
type MailboxProbeItem struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Label    string `json:"label,omitempty"`
	Provider string `json:"provider"`

	// Covered 表示这次真的探过它。未覆盖时 Readable 一律为 false，
	// 并由 Reason 说明为什么没探——不用一个猜出来的结论把表格填满。
	Covered  bool `json:"covered"`
	Readable bool `json:"readable"`

	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Retryable 是服务商自己对这次失败的定性：真表示「可以重试」，也就是网络类或对面
	// 临时故障，不代表凭据有问题。判定该不该摘出池子必须看它，不能只看错误码——
	// imap_login_failed 一个码同时表示「登录请求失败」和「登录被拒」，只靠码分不开。
	Retryable bool `json:"retryable,omitempty"`

	// RemoteExists 是服务器说收件箱里有几封；LocalReceived 是本地记录里读进来过几封。
	// 两个数对不上，说明问题在读信路径上，而不在投递上。
	RemoteExists  int `json:"remote_exists"`
	LocalReceived int `json:"local_received"`

	// NeverChecked 表示这个邮箱从来没有被检查过。
	// 这时 LocalReceived 的 0 是「不知道」，不是「没有邮件」——两者的区别必须留在数据里，
	// 混在一个 0 里正是最容易得出错误结论的地方。
	NeverChecked  bool   `json:"never_checked"`
	LastCheckedAt string `json:"last_checked_at,omitempty"`

	// Missing 表示本地根本没有这条记录，与「服务商这次没覆盖」不是一回事：
	// 前者是要清掉的残留，后者是这套检查够不到的地方。混成一个数会让账对不上。
	Missing bool `json:"missing,omitempty"`

	Status string `json:"status"`
	InPool bool   `json:"in_pool"`
	// OfferedWhileUnreadable：判读不了的那一刻，它仍标着可用、仍在池里。邮箱池只从
	// 「可用且未隐藏」的记录里挑人，所以这些会被真的投放出去，然后永远等不到验证码。
	OfferedWhileUnreadable bool `json:"offered_while_unreadable"`
	// Demoted：本次已把它标成失败、从池里摘掉。PreviousStatus 是摘之前的状态，
	// 需要人工恢复时照着改回去。
	Demoted        bool   `json:"demoted"`
	PreviousStatus string `json:"previous_status,omitempty"`
}

// MailboxProbeReport 汇总一次体检。
type MailboxProbeReport struct {
	Checked      int `json:"checked"`
	Readable     int `json:"readable"`
	Unreadable   int `json:"unreadable"`
	NotCovered   int `json:"not_covered"`
	Missing      int `json:"missing"`
	NeverChecked int `json:"never_checked"`
	// UnreadableButOffered 是「读不了却仍会被投放」的个数，是可操作的那部分。
	UnreadableButOffered int `json:"unreadable_but_offered"`
	// Demoted 是本次已经摘掉的个数。它和上一行的差，就是「该摘却没摘成」的。
	Demoted int                `json:"demoted"`
	Items   []MailboxProbeItem `json:"items"`
}

// ProbeMailboxes 体检一批邮箱：能不能登录读信、服务器上到底有没有信。
//
// 这是只读诊断：不发信、不取正文、不改任何状态。所以它可以随时跑、反复跑，
// 而不会让被诊断的东西发生变化——诊断不得修改它所诊断的对象。
func (s *Service) ProbeMailboxes(ctx context.Context, ids []string) MailboxProbeReport {
	requested := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		requested = append(requested, id)
	}

	// 先按请求顺序占好槽位，再并发填内容：并发不改变输出顺序，
	// 使用者看到的行序才和它传进来的顺序对得上。
	results := make([]MailboxProbeItem, len(requested))
	slots := make(chan struct{}, mailboxProbeWorkers)
	var pending sync.WaitGroup
	for index, id := range requested {
		mailbox, found := s.store.FindMailboxByID(id)
		if !found {
			results[index] = MailboxProbeItem{
				ID: id, Covered: false, Readable: false, Missing: true,
				Code: "mailbox_not_found", Reason: "本地没有这个邮箱的记录，可能已经被删除",
			}
			continue
		}
		pending.Add(1)
		go func(index int, mailbox domain.Mailbox) {
			defer pending.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[index] = s.probeOneMailbox(ctx, mailbox)
		}(index, mailbox)
	}
	pending.Wait()

	// 读不了的排最前：那是唯一需要动手的部分。排序稳定，所以同组内保持传入顺序。
	sort.SliceStable(results, func(i, j int) bool {
		return mailboxProbeRank(results[i]) < mailboxProbeRank(results[j])
	})

	report := MailboxProbeReport{Items: results}
	for _, item := range results {
		switch {
		case item.Missing:
			report.Missing++
		case !item.Covered:
			report.NotCovered++
		case item.Readable:
			report.Checked++
			report.Readable++
		default:
			report.Checked++
			report.Unreadable++
		}
		if item.NeverChecked {
			report.NeverChecked++
		}
		if item.OfferedWhileUnreadable {
			report.UnreadableButOffered++
		}
		if item.Demoted {
			report.Demoted++
		}
	}
	return report
}

// mailboxProbeRank 决定体检结果在界面上的先后：越该被处理的越靠前。
func mailboxProbeRank(item MailboxProbeItem) int {
	switch {
	case item.Covered && !item.Readable:
		return 0 // 读不了——唯一需要动手的一类
	case !item.Covered:
		return 1 // 没探到，别让它混进「正常」里去
	default:
		return 2 // 能读
	}
}

// probeOneMailbox 按服务商分派到对应的读原语。
//
// 分派而不是写一条通用路径，是因为「读一封信」在不同服务商那里是三件不同的事：
// IMAP 是登录加打开收件箱，外部取码邮箱是请求一个取件地址，其余走各自的授权接口。
// 后一类这次够不到，就如实说没探到，不给一个看起来完整的结论。
func (s *Service) probeOneMailbox(ctx context.Context, mailbox domain.Mailbox) MailboxProbeItem {
	item := probeBaseItem(mailbox)
	switch mailbox.ProviderOrDefault() {
	case domain.ProviderIMAP:
		s.probeIMAPMailbox(ctx, mailbox, &item)
	case domain.ProviderExternal:
		s.probeExternalMailbox(ctx, mailbox, &item)
	default:
		item.Covered = false
		item.Readable = false
		item.Code = "provider_not_covered"
		item.Reason = "该服务商走授权接口读信，本次体检只覆盖 IMAP 与外部取码地址，没有检查它"
	}
	item.OfferedWhileUnreadable = item.Covered && !item.Readable &&
		mailbox.Status == domain.StatusAvailable && !mailbox.PoolHidden
	// 体检不能只出一份报告：报告没人看的时候，一个邮箱到底能不能用就没人知道。
	// 所以判定之后顺手把读不了的从池里摘掉。
	//
	// 两条限制都必要：只有凭据被明确拒绝才算（见 probeCredentialRejected），而且只动
	// 仍然标着可用的那些——手设过的状态和出借中的邮箱都不该被一次体检改写（改状态会
	// 顺带释放租约，那等于把邮箱从正在用它的任务手里拽走）。
	if mailbox.Status == domain.StatusAvailable && probeCredentialRejected(item) {
		note := "体检判定读不了：" + firstNonEmpty(item.Reason, item.Code)
		if updated, err := s.store.SetMailboxStatus(mailbox.ID, nil, nil, domain.StatusFailed, &note); err != nil {
			item.Reason += "；但把它标为失败没有成功：" + err.Error()
		} else {
			item.Demoted = true
			item.PreviousStatus = mailbox.Status
			item.Status = updated.Status
		}
	}
	return item
}

// probeCredentialRejected 判断这次失败是不是「凭据或连接配置被明确拒绝或缺失」，
// 也就是唯一足以把邮箱摘出池子的那一类失败。
//
// 判定要两个条件同时成立，缺一不可：
//
//  1. 错误码在下面这张表里。只有这些码代表「凭据或配置本身不对」。
//  2. 这次失败不可重试。同名的码会出现在两种意思相反的场合：imap_login_failed
//     既表示「登录请求本身失败」（网络断了，可重试），也表示「登录被拒」（凭据真错，
//     不可重试），两者只靠这个标记区分。只按码判断会把一次网络断线当成凭据失效，
//     把本来能用的邮箱摘出池子。
//
// 摘错的代价远大于漏报：坏邮箱只是白等一个验证码，被摘掉的好邮箱连等都等不到。
func probeCredentialRejected(item MailboxProbeItem) bool {
	if item.Retryable {
		return false
	}
	switch item.Code {
	case "imap_login_failed", "imap_username_missing", "imap_secret_missing", "imap_host_missing",
		// 本地缺主机或授权码时探针自己填的码，协议层没有这一个；
		// 它没有可重试标记，落在不可重试一侧是刻意的。
		"imap_credentials_missing",
		"external_code_unauthorized", "external_code_url_missing", "external_code_url_invalid":
		return true
	}
	return false
}

// probeBaseItem 填本地记录里已有的那部分，不涉及任何网络。
func probeBaseItem(mailbox domain.Mailbox) MailboxProbeItem {
	item := MailboxProbeItem{
		ID:            mailbox.ID,
		Email:         mailbox.Email,
		Label:         mailbox.Label,
		Provider:      mailbox.ProviderOrDefault(),
		LocalReceived: mailbox.ReceiveCount,
		Status:        mailbox.Status,
		InPool:        !mailbox.PoolHidden,
	}
	if mailbox.LastSyncAt.IsZero() {
		item.NeverChecked = true
	} else {
		item.LastCheckedAt = mailbox.LastSyncAt.Format(time.RFC3339)
	}
	return item
}

// probeIMAPMailbox 用邮箱自己保存的凭据连一次，只走到「打开收件箱」为止。
func (s *Service) probeIMAPMailbox(ctx context.Context, mailbox domain.Mailbox, item *MailboxProbeItem) {
	host := strings.TrimSpace(mailbox.IMAPHost)
	secret := strings.TrimSpace(mailbox.IMAPSecret)
	username := firstNonEmpty(strings.TrimSpace(mailbox.IMAPUsername), mailbox.Email)
	item.Covered = true
	if host == "" || secret == "" {
		item.Readable = false
		item.Code = "imap_credentials_missing"
		item.Reason = "本地没有存这个邮箱的服务器地址或授权码，请重新导入"
		return
	}
	inspect := s.imapInspect
	if inspect == nil {
		inspect = protocol.InspectGenericIMAP
	}
	probeCtx, cancel := context.WithTimeout(ctx, mailboxProbeTimeout)
	defer cancel()
	stat, err := inspect(probeCtx, host, mailbox.IMAPPort, username, secret)
	if err != nil {
		code, message, retryable := protocol.ErrorDetails(err)
		item.Readable = false
		item.Code = firstNonEmpty(code, "imap_probe_failed")
		item.Reason = message
		item.Retryable = retryable
		return
	}
	item.Readable = true
	item.RemoteExists = stat.Exists
	item.Code = "imap_ok"
	item.Reason = fmt.Sprintf("登录成功，收件箱里有 %d 封", stat.Exists)
	if !item.NeverChecked && stat.Exists > 0 && mailbox.ReceiveCount == 0 {
		// 服务器上有信、本地一封都没记进来：毛病在读信路径上，不在投递上。
		item.Reason += "；但本地一封都没读进来过，读信路径本身有问题"
	}
}

// probeExternalMailbox 向外部取码接口要一次信。取的只是「接口是否可达」，
// 有没有验证码是另一件事。
func (s *Service) probeExternalMailbox(ctx context.Context, mailbox domain.Mailbox, item *MailboxProbeItem) {
	endpoint := strings.TrimSpace(mailbox.CodeAPIURL)
	item.Covered = true
	if endpoint == "" {
		item.Readable = false
		item.Code = "external_code_url_missing"
		item.Reason = "本地没有存这个邮箱的取码地址，请重新导入"
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, mailboxProbeTimeout)
	defer cancel()
	client := protocol.NewHTTPClient(s.cfg.GoogleProxy, mailboxProbeTimeout)
	result, err := protocol.FetchExternalCode(probeCtx, client, endpoint)
	if err != nil {
		code, message, retryable := protocol.ErrorDetails(err)
		item.Readable = false
		item.Code = firstNonEmpty(code, "external_probe_failed")
		item.Reason = message
		item.Retryable = retryable
		return
	}
	item.Readable = true
	item.Code = "external_ok"
	// 「接口可达但没有码」是正常状态，不是故障。这两件事必须分开说，
	// 否则一个失效的令牌会一直伪装成「还没有收到」。
	if result.Found {
		item.Reason = "取码接口可达，并且现在就能取到验证码"
	} else {
		item.Reason = "取码接口可达，但当前没有验证码可取"
	}
}
