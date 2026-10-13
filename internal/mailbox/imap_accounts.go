package mailbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

// IMAPAccountView 是一个 IMAP 母号连同它名下的地址。
// 地址一律返回全部，包括使用者选择不进邮箱池的那些——
// 母号页面是这些地址唯一的家，这里再过滤就再也看不到它们了。
type IMAPAccountView struct {
	domain.AppleAccount
	Mailboxes    []domain.Mailbox `json:"mailboxes"`
	MailboxCount int              `json:"mailbox_count"`
	PoolCount    int              `json:"pool_count"`
	MessageCount int              `json:"message_count"`
}

// buildIMAPAccountView 把母号和它名下的地址拼成视图，并算好三个计数。
// 列表和详情共用它，免得两处的计数口径慢慢分叉。
func buildIMAPAccountView(account domain.AppleAccount, mailboxes []domain.Mailbox) IMAPAccountView {
	if mailboxes == nil {
		// 空列表要序列化成 []，不是 null：前端直接 v-for，null 会让它炸。
		mailboxes = []domain.Mailbox{}
	}
	view := IMAPAccountView{AppleAccount: account, Mailboxes: mailboxes, MailboxCount: len(mailboxes)}
	for _, mailbox := range mailboxes {
		if !mailbox.PoolHidden {
			view.PoolCount++
		}
		view.MessageCount += mailbox.ReceiveCount
	}
	return view
}

// IMAPAccounts 按 preset 返回母号列表的一页；preset 为空时返回全部 IMAP 母号。
// 三个界面（QQ 邮箱 / 163 邮箱 / Gmail）用的是同一个函数，只是传入的 preset 不同。
func (s *Service) IMAPAccounts(preset, query string, page, pageSize int) domain.Page[IMAPAccountView] {
	accounts := s.store.AccountsPage(domain.ProviderIMAP, preset, query, page, pageSize, false)
	// 只给当前这一页的母号取地址：一页二十个母号就该只解二十份地址，
	// 而不是每翻一页都把整张 mailboxes 表读一遍。
	ids := make([]string, 0, len(accounts.Items))
	for _, account := range accounts.Items {
		ids = append(ids, account.ID)
	}
	grouped := s.store.MailboxesForAccounts(ids)
	views := make([]IMAPAccountView, 0, len(accounts.Items))
	for _, account := range accounts.Items {
		views = append(views, buildIMAPAccountView(account, grouped[account.ID]))
	}
	return domain.Page[IMAPAccountView]{
		Items: views, Page: accounts.Page, PageSize: accounts.PageSize,
		Total: accounts.Total, TotalPages: accounts.TotalPages,
	}
}

// FindIMAPAccount 按标识取一个母号，并确认它确实是 IMAP 母号。
func (s *Service) FindIMAPAccount(accountID string) (IMAPAccountView, bool) {
	account, ok := s.store.FindAppleAccount(strings.TrimSpace(accountID))
	if !ok || account.ProviderOrDefault() != domain.ProviderIMAP {
		return IMAPAccountView{}, false
	}
	return buildIMAPAccountView(account, s.store.MailboxesByAccount(account.ID)), true
}

// CreateIMAPAccount 建一个 IMAP 母号，并把它的地址一起导入。
//
// 母号自己的地址永远是第一个地址：QQ 和 163 的收信都发生在主地址的收件箱里，
// 别名只是同一个收件箱的另一个收件人写法。少了这一条，母号页面会连自己的信都看不到。
func (s *Service) CreateIMAPAccount(preset, email, secret, host string, port int, username, label, note string, addresses []IMAPMailboxInput) (IMAPAccountView, IMAPMailboxImportResult, error) {
	host, port, username, secret = ResolveIMAPCredentials(preset, host, port, username, secret)
	normalizedHost, hostErr := normalizeIMAPHost(host)
	if hostErr != nil {
		return IMAPAccountView{}, IMAPMailboxImportResult{}, hostErr
	}
	primary, emailErr := normalizeExternalEmail(email)
	if emailErr != nil {
		return IMAPAccountView{}, IMAPMailboxImportResult{}, fmt.Errorf("母号邮箱地址不正确：%w", emailErr)
	}
	if secret == "" {
		return IMAPAccountView{}, IMAPMailboxImportResult{}, errors.New("缺少客户端授权码")
	}
	// 母号地址排在最前并去重：使用者把主地址也粘进地址框时不该出现两条。
	inputs := []IMAPMailboxInput{{Email: primary, Label: label, Note: note}}
	seen := map[string]struct{}{primary: {}}
	for _, input := range addresses {
		candidate := strings.ToLower(strings.TrimSpace(input.Email))
		if candidate == "" {
			continue
		}
		if _, dup := seen[candidate]; dup {
			continue
		}
		seen[candidate] = struct{}{}
		inputs = append(inputs, input)
	}

	account, _, err := s.store.SaveIMAPAccount(domain.AppleAccount{
		Provider:     domain.ProviderIMAP,
		Preset:       strings.ToLower(strings.TrimSpace(preset)),
		AppleID:      primary,
		Label:        firstNonEmpty(strings.TrimSpace(label), primary),
		Password:     secret,
		IMAPHost:     normalizedHost,
		IMAPPort:     port,
		IMAPUsername: firstNonEmpty(username, primary),
		Status:       domain.StatusActive,
		Note:         strings.TrimSpace(note),
	})
	if err != nil {
		return IMAPAccountView{}, IMAPMailboxImportResult{}, err
	}
	result, err := s.ImportIMAPMailboxes(account.ID, account.Preset, normalizedHost, port, firstNonEmpty(username, primary), secret, inputs)
	if err != nil {
		return IMAPAccountView{}, result, err
	}
	// 探活结论写回母号本身：使用者看母号列表时就能知道这个授权码还能不能用。
	status := domain.StatusActive
	message := ""
	if result.Created == 0 && result.Updated == 0 && result.Failed > 0 {
		status = "failed"
		message = "地址导入全部失败，请检查授权码与服务器地址"
	}
	_ = s.store.SetIMAPAccountStatus(account.ID, status, message)
	view, _ := s.FindIMAPAccount(account.ID)
	_ = s.store.RecordEvent("info", "mailbox", "已添加 IMAP 母号 "+primary)
	return view, result, nil
}

// IMAPAccountBatchItem 是批量添加里一行的结果。
// 逐行报结果，而不是只回一个总数：批量里最需要知道的就是「哪一行错了」。
type IMAPAccountBatchItem struct {
	Email   string `json:"email"`
	OK      bool   `json:"ok"`
	Created int    `json:"created"`
	Failed  int    `json:"failed"`
	Reason  string `json:"reason,omitempty"`
}

// IMAPAccountBatchResult 汇总一次批量添加。
type IMAPAccountBatchResult struct {
	Accounts int                    `json:"accounts"`
	Failed   int                    `json:"failed"`
	Items    []IMAPAccountBatchItem `json:"items"`
}

// ImportIMAPAccounts 一次建多个彼此独立的 IMAP 母号。
//
// 逐行独立建，一行失败不影响其它行：批量里最不该发生的事，就是第 3 行的授权码
// 写错，把第 4 到第 6 行一起丢掉。每行都会真的探活一次，所以耗时就等于行数乘以
// 一次登录的时间——服务端的写超时是 30 分钟，放得下。
func (s *Service) ImportIMAPAccounts(preset, host string, port int, inputs []IMAPAccountInput) IMAPAccountBatchResult {
	result := IMAPAccountBatchResult{Items: make([]IMAPAccountBatchItem, 0, len(inputs))}
	for _, input := range inputs {
		item := IMAPAccountBatchItem{Email: input.Email}
		// 用户名、标签、备注一律留空：批量里每一行就是它自己的母号，
		// 标签由 CreateIMAPAccount 回落到邮箱地址本身。
		_, importResult, err := s.CreateIMAPAccount(preset, input.Email, input.Secret, host, port, "", "", "", nil)
		switch {
		case err != nil:
			item.Reason = err.Error()
		default:
			item.Created = importResult.Created
			item.Failed = importResult.Failed
			item.OK = importResult.Failed == 0
			if !item.OK {
				item.Reason = firstIMAPFailureReason(importResult)
			}
		}
		if item.OK {
			result.Accounts++
		} else {
			result.Failed++
		}
		result.Items = append(result.Items, item)
	}
	return result
}

// firstIMAPFailureReason 从导入结果里挑出第一条失败原因。
// 母号自己的地址永远排在地址列表第一条，所以这里拿到的通常就是
// 「这个授权码连不上」这一条，正是使用者需要看到的那句话。
func firstIMAPFailureReason(result IMAPMailboxImportResult) string {
	for _, item := range result.Items {
		if reason := strings.TrimSpace(item.Reason); reason != "" {
			return reason
		}
	}
	return "地址导入失败，请检查授权码与服务器地址"
}

// AddIMAPAccountAddresses 往已有母号里追加地址。
// 凭据沿用母号自己保存的那一份，不需要使用者再填一次授权码。
func (s *Service) AddIMAPAccountAddresses(accountID string, inputs []IMAPMailboxInput) (IMAPMailboxImportResult, error) {
	account, ok := s.store.FindAppleAccount(strings.TrimSpace(accountID))
	if !ok || account.ProviderOrDefault() != domain.ProviderIMAP {
		return IMAPMailboxImportResult{}, errors.New("IMAP 母号不存在")
	}
	if strings.TrimSpace(account.Password) == "" {
		return IMAPMailboxImportResult{}, errors.New("该母号没有保存客户端授权码，请删除后重新添加")
	}
	return s.ImportIMAPMailboxes(account.ID, account.Preset, account.IMAPHost, account.IMAPPort, account.IMAPUsername, account.Password, inputs)
}

// SyncIMAPAccount 读一次该母号名下所有地址的收件箱。
//
// 一个母号的多个地址共用一个收件箱，所以逐地址同步会重复读同一批信；
// 但路由是按收件人精确匹配的，必须每个地址都走一遍才能把它们各自归位。
// 失败按地址收集，不让一个地址的故障拖掉整批。
func (s *Service) SyncIMAPAccount(ctx context.Context, accountID string) (int, error) {
	account, ok := s.store.FindAppleAccount(strings.TrimSpace(accountID))
	if !ok || account.ProviderOrDefault() != domain.ProviderIMAP {
		return 0, errors.New("IMAP 母号不存在")
	}
	mailboxes := s.store.MailboxesByAccount(account.ID)
	if len(mailboxes) == 0 {
		return 0, errors.New("该母号下还没有地址，请先添加")
	}
	stored := 0
	var failures []string
	for _, mailbox := range mailboxes {
		count, err := s.SyncIMAPMailbox(ctx, mailbox.ID)
		if err != nil {
			code, message, _ := protocol.ErrorDetails(err)
			failures = append(failures, mailbox.Email+"（"+firstNonEmpty(code, message)+"）")
			continue
		}
		stored += count
	}
	if len(failures) == len(mailboxes) {
		// 全军覆没多半是授权码或服务器地址的问题，如实标出来而不是留个「正常」。
		_ = s.store.SetIMAPAccountStatus(account.ID, "failed", "同步失败："+strings.Join(failures, "，"))
		return stored, errors.New("全部地址同步失败：" + strings.Join(failures, "，"))
	}
	if len(failures) > 0 {
		_ = s.store.SetIMAPAccountStatus(account.ID, "partial", "部分地址同步失败："+strings.Join(failures, "，"))
		return stored, nil
	}
	_ = s.store.SetIMAPAccountStatus(account.ID, domain.StatusActive, "")
	return stored, nil
}

// DeleteIMAPAccount 删除母号及其名下的地址、邮件和租约。
// 复用 iCloud 那条通用删除路径：它按 account_id 删，与 provider 无关。
func (s *Service) DeleteIMAPAccount(accountID string) (store.DeleteAppleAccountResult, error) {
	account, ok := s.store.FindAppleAccount(strings.TrimSpace(accountID))
	if !ok || account.ProviderOrDefault() != domain.ProviderIMAP {
		return store.DeleteAppleAccountResult{}, errors.New("IMAP 母号不存在")
	}
	return s.store.DeleteAppleAccount(account.ID)
}

// SetMailboxPoolHidden 切换一个地址是否出现在邮箱池。
func (s *Service) SetMailboxPoolHidden(mailboxID string, hidden bool) (domain.Mailbox, error) {
	mailbox, err := s.store.SetMailboxPoolHidden(strings.TrimSpace(mailboxID), hidden)
	if err != nil {
		return domain.Mailbox{}, err
	}
	_ = s.store.RecordEvent("info", "mailbox", fmt.Sprintf("%s 已%s邮箱池",
		mailbox.Email, map[bool]string{true: "移出", false: "加入"}[hidden]))
	return mailbox, nil
}

// IMAPBatchSelection 是一批批量操作要作用到的母号。
//
// 只接受明确的标识列表，不做「空列表＝全部」这种兜底：那种语义一旦存在，
// 任何一次前端的小 bug 都会在几百个母号上跑一遍同步或改一遍可见性，
// 而使用者只会看到界面「自己动了」。「全部」这个范围由调用方先查出来，
// 再显式地传进来——那一刻它是个数字，使用者能看见也能确认。
type IMAPBatchSelection struct {
	AccountIDs []string `json:"account_ids"`
}

func (s IMAPBatchSelection) trimmed() []string {
	out := make([]string, 0, len(s.AccountIDs))
	for _, accountID := range s.AccountIDs {
		if trimmed := strings.TrimSpace(accountID); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// IMAPBatchSyncFailure 记下一个母号为什么没同步成功。
// 批量操作最需要知道的就是这个：只说「失败 3 个」等于没说，使用者无从下手。
type IMAPBatchSyncFailure struct {
	AccountID string `json:"account_id"`
	Account   string `json:"account,omitempty"`
	Reason    string `json:"reason"`
}

type IMAPBatchSyncResult struct {
	Total          int                    `json:"total"`
	Succeeded      int                    `json:"succeeded"`
	Failed         int                    `json:"failed"`
	SyncedMessages int                    `json:"synced_messages"`
	Failures       []IMAPBatchSyncFailure `json:"failures"`
}

// SyncIMAPAccountsBySelection 同步一批母号。
//
// 并发但限流：Gmail 对同一时间打开的 IMAP 连接有配额，几百个母号一起拨过去会被拒，
// 而被拒的那些会带着「同步失败」被记进使用者的账号状态里——一次批量操作把数据弄脏
// 比它慢几秒糟糕得多。前端按小批循环调用，进度因此是实时的，也可以中途停下。
func (s *Service) SyncIMAPAccountsBySelection(ctx context.Context, selection IMAPBatchSelection) IMAPBatchSyncResult {
	const workers = 4
	ids := selection.trimmed()
	result := IMAPBatchSyncResult{Total: len(ids), Failures: []IMAPBatchSyncFailure{}}
	if len(ids) == 0 {
		return result
	}

	// 带缓冲、一次投满再关闭：投递永远不会阻塞，也就不会出现「四个 worker 都提前
	// 退出之后发送方卡死」这种只在超时路径上才现形的死锁。
	queue := make(chan string, len(ids))
	for _, accountID := range ids {
		queue <- accountID
	}
	close(queue)

	var mu sync.Mutex
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for accountID := range queue {
				if ctx.Err() != nil {
					return
				}
				account, ok := s.store.FindAppleAccount(accountID)
				if !ok || account.ProviderOrDefault() != domain.ProviderIMAP {
					mu.Lock()
					result.Failed++
					result.Failures = append(result.Failures, IMAPBatchSyncFailure{AccountID: accountID, Reason: "IMAP 母号不存在"})
					mu.Unlock()
					continue
				}
				stored, err := s.SyncIMAPAccount(ctx, accountID)
				mu.Lock()
				if err != nil {
					result.Failed++
					result.Failures = append(result.Failures, IMAPBatchSyncFailure{
						AccountID: accountID,
						Account:   firstNonEmpty(account.Label, account.AppleID, accountID),
						Reason:    err.Error(),
					})
				} else {
					result.Succeeded++
					result.SyncedMessages += stored
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return result
}

type IMAPBatchPoolResult struct {
	TotalAccounts   int  `json:"total_accounts"`
	UpdatedMailboxes int `json:"updated_mailboxes"`
	InPool          bool `json:"in_pool"`
}

// SetIMAPAccountsPool 把一批母号名下的全部地址加入或移出邮箱池。
//
// 按母号而不是按地址：使用者勾的是母号，他脑子里的对象也是母号。让他先展开每个母号
// 再逐个勾地址，正是他现在抱怨的那件事。
//
// 已经处于目标状态的地址不计数、也不写库：重复点一次不该把「已更新 456 个」报出来。
func (s *Service) SetIMAPAccountsPool(selection IMAPBatchSelection, inPool bool) IMAPBatchPoolResult {
	ids := selection.trimmed()
	result := IMAPBatchPoolResult{TotalAccounts: len(ids), InPool: inPool}
	hidden := !inPool
	for _, accountID := range ids {
		for _, mailbox := range s.store.MailboxesByAccount(accountID) {
			if mailbox.PoolHidden == hidden {
				continue
			}
			if _, err := s.SetMailboxPoolHidden(mailbox.ID, hidden); err == nil {
				result.UpdatedMailboxes++
			}
		}
	}
	return result
}
