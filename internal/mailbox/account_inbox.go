package mailbox

import (
	"errors"
	"sort"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/protocol"
)

// AccountInboxMaxMessages 是母号页面一次最多展示的邮件数。
const AccountInboxMaxMessages = 200

// AccountInboxMailbox 是母号下的一个地址，连同它最近一次验证码。
type AccountInboxMailbox struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Label        string    `json:"label"`
	Provider     string    `json:"provider"`
	PoolHidden   bool      `json:"pool_hidden"`
	ReceiveCount int       `json:"receive_count"`
	LastCode     string    `json:"last_code,omitempty"`
	LastCodeAt   time.Time `json:"last_code_at,omitempty"`
	LastSyncAt   time.Time `json:"last_sync_at,omitempty"`
}

// AccountInboxMessage 是母号页面里展示的一封邮件。
// Code 是当场从标题和正文里提取的，可能为空——不是每封信都带验证码。
type AccountInboxMessage struct {
	ID           string    `json:"id"`
	MailboxID    string    `json:"mailbox_id"`
	MailboxEmail string    `json:"mailbox_email"`
	Subject      string    `json:"subject"`
	From         string    `json:"from"`
	Code         string    `json:"code,omitempty"`
	ReceivedAt   time.Time `json:"received_at"`
}

// AccountInbox 是一个母号的收件箱视图。
//
// 它读的全是本地已经存下的数据，不发任何网络请求：
// 母号页面要能随时打开看信，不该每次打开都去连一遍服务商。
// 取新信是「同步」按钮的事，两件事分开。
type AccountInbox struct {
	AccountID string                `json:"account_id"`
	Provider  string                `json:"provider"`
	Label     string                `json:"label"`
	Email     string                `json:"email"`
	Mailboxes []AccountInboxMailbox `json:"mailboxes"`
	Messages  []AccountInboxMessage `json:"messages"`
	CodeCount int                   `json:"code_count"`
	Total     int                   `json:"total"`
}

// AccountInbox 汇总一个母号名下所有地址的邮件与验证码。
//
// 一个母号的多个地址常常共用一个收件箱（QQ 和 163 都是如此），
// 所以同一个收件箱里的信会按收件人归到不同地址下——这正是需要的：
// 在母号页面上要能一眼看出哪封是发给哪个地址的。
//
// 它复用了既有的两条查询（MailboxesByAccount、MessagesForMailbox）和既有的
// 验证码提取（protocol.ExtractOTP），没有另起一条读路径。后者尤其重要：
// 界面显示的验证码必须和接口取到的验证码出自同一段代码，否则两边会悄悄分叉。
func (s *Service) AccountInbox(accountID string, limit int) (AccountInbox, error) {
	account, ok := s.store.FindAppleAccount(strings.TrimSpace(accountID))
	if !ok {
		return AccountInbox{}, errors.New("母号不存在")
	}
	if limit <= 0 || limit > AccountInboxMaxMessages {
		limit = AccountInboxMaxMessages
	}
	mailboxes := s.store.MailboxesByAccount(account.ID)
	inbox := AccountInbox{
		AccountID: account.ID,
		Provider:  account.ProviderOrDefault(),
		Label:     firstNonEmpty(account.Label, account.AppleID),
		Email:     account.AppleID,
		Mailboxes: make([]AccountInboxMailbox, 0, len(mailboxes)),
		Messages:  make([]AccountInboxMessage, 0),
	}
	for _, mailbox := range mailboxes {
		summary := AccountInboxMailbox{
			ID: mailbox.ID, Email: mailbox.Email, Label: mailbox.Label,
			Provider: mailbox.ProviderOrDefault(), PoolHidden: mailbox.PoolHidden,
			ReceiveCount: mailbox.ReceiveCount, LastSyncAt: mailbox.LastSyncAt,
		}
		for _, message := range s.store.MessagesForMailbox(mailbox.ID) {
			receivedAt := message.ReceivedAt
			if receivedAt.IsZero() {
				receivedAt = message.CreatedAt
			}
			code := protocol.ExtractOTP(message.Subject + "\n" + message.Body)
			if code != "" {
				inbox.CodeCount++
				// 每个地址只记最近一次验证码：这是母号页面上最需要的一眼信息。
				if summary.LastCodeAt.Before(receivedAt) {
					summary.LastCode = code
					summary.LastCodeAt = receivedAt
				}
			}
			inbox.Messages = append(inbox.Messages, AccountInboxMessage{
				ID: message.ID, MailboxID: mailbox.ID, MailboxEmail: mailbox.Email,
				Subject: message.Subject, From: message.From, Code: code, ReceivedAt: receivedAt,
			})
		}
		inbox.Mailboxes = append(inbox.Mailboxes, summary)
	}
	// 最新的在最前：打开这一页就是为了看刚到的信。
	sort.SliceStable(inbox.Messages, func(i, j int) bool {
		return inbox.Messages[i].ReceivedAt.After(inbox.Messages[j].ReceivedAt)
	})
	inbox.Total = len(inbox.Messages)
	if len(inbox.Messages) > limit {
		inbox.Messages = inbox.Messages[:limit]
	}
	return inbox, nil
}
