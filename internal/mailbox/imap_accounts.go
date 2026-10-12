package mailbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// IMAPAccounts 按 preset 返回母号列表；preset 为空时返回全部 IMAP 母号。
// 两个界面（QQ 邮箱 / 163 邮箱）用的是同一个函数，只是传入的 preset 不同。
func (s *Service) IMAPAccounts(preset string) []IMAPAccountView {
	preset = strings.ToLower(strings.TrimSpace(preset))
	accounts := s.store.Accounts(domain.ProviderIMAP)
	views := make([]IMAPAccountView, 0, len(accounts))
	for _, account := range accounts {
		if preset != "" && strings.ToLower(strings.TrimSpace(account.Preset)) != preset {
			continue
		}
		mailboxes := s.store.MailboxesByAccount(account.ID)
		view := IMAPAccountView{AppleAccount: account, Mailboxes: mailboxes, MailboxCount: len(mailboxes)}
		for _, mailbox := range mailboxes {
			if !mailbox.PoolHidden {
				view.PoolCount++
			}
			view.MessageCount += mailbox.ReceiveCount
		}
		views = append(views, view)
	}
	return views
}

// FindIMAPAccount 按标识取一个母号，并确认它确实是 IMAP 母号。
func (s *Service) FindIMAPAccount(accountID string) (IMAPAccountView, bool) {
	account, ok := s.store.FindAppleAccount(strings.TrimSpace(accountID))
	if !ok || account.ProviderOrDefault() != domain.ProviderIMAP {
		return IMAPAccountView{}, false
	}
	mailboxes := s.store.MailboxesByAccount(account.ID)
	view := IMAPAccountView{AppleAccount: account, Mailboxes: mailboxes, MailboxCount: len(mailboxes)}
	for _, mailbox := range mailboxes {
		if !mailbox.PoolHidden {
			view.PoolCount++
		}
		view.MessageCount += mailbox.ReceiveCount
	}
	return view, true
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
