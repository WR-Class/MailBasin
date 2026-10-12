package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

// SaveIMAPAccount 按 (preset, 邮箱地址) 找已有的 IMAP 母号并更新，找不到就新建。
// 返回值第二项表示是否新建。
//
// IMAP 母号复用 apple_accounts 表：它本来就是通用的母号表，iCloud / Microsoft /
// Google 三家共用，靠 provider 字段区分，还有 provider 索引。QQ 和 163 只是
// 同一类母号的两种 preset，所以不需要新建一张表，也不需要新的加密字段——
// 客户端授权码存在 Password 上，它已经在这张表的加密清单里。
func (s *Store) SaveIMAPAccount(account domain.AppleAccount) (domain.AppleAccount, bool, error) {
	email := strings.ToLower(strings.TrimSpace(account.AppleID))
	if email == "" {
		return domain.AppleAccount{}, false, errors.New("母号邮箱地址不能为空")
	}
	preset := strings.ToLower(strings.TrimSpace(account.Preset))
	if preset == "" {
		preset = "custom"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.AppleAccount{}, false, err
	}
	var existing domain.AppleAccount
	var data []byte
	err = tx.QueryRow(`SELECT data_json FROM apple_accounts
		WHERE lower(json_extract(data_json, '$.apple_id')) = ?
		  AND COALESCE(json_extract(data_json, '$.provider'), 'icloud') = ?
		  AND COALESCE(json_extract(data_json, '$.preset'), 'custom') = ?
		LIMIT 1`, email, domain.ProviderIMAP, preset).Scan(&data)
	created := errors.Is(err, sql.ErrNoRows)
	if err != nil && !created {
		_ = tx.Rollback()
		return domain.AppleAccount{}, false, err
	}
	now := time.Now()
	if !created {
		if err := s.decodeEntity("apple_accounts", data, &existing); err != nil {
			_ = tx.Rollback()
			return domain.AppleAccount{}, false, err
		}
		account.ID = existing.ID
		account.CreatedAt = existing.CreatedAt
		account.OwnerID = firstNonEmpty(account.OwnerID, existing.OwnerID)
		// 授权码留空时保留原值：一次不带授权码的重导入不该把已存的凭据清空。
		if strings.TrimSpace(account.Password) == "" {
			account.Password = existing.Password
		}
		if strings.TrimSpace(account.IMAPHost) == "" {
			account.IMAPHost = existing.IMAPHost
		}
		if account.IMAPPort <= 0 {
			account.IMAPPort = existing.IMAPPort
		}
		if strings.TrimSpace(account.IMAPUsername) == "" {
			account.IMAPUsername = existing.IMAPUsername
		}
		if strings.TrimSpace(account.Label) == "" {
			account.Label = existing.Label
		}
	} else {
		id, err := s.nextIDTx(tx, "acc")
		if err != nil {
			_ = tx.Rollback()
			return domain.AppleAccount{}, false, err
		}
		account.ID = id
		account.CreatedAt = now
		if account.OwnerID == "" {
			var admin domain.Admin
			var adminData []byte
			if tx.QueryRow(`SELECT data_json FROM admins LIMIT 1`).Scan(&adminData) == nil &&
				s.decodeEntity("admins", adminData, &admin) == nil {
				account.OwnerID = admin.ID
			}
		}
	}
	account.Provider = domain.ProviderIMAP
	account.Preset = preset
	account.AppleID = email
	if strings.TrimSpace(account.Label) == "" {
		account.Label = email
	}
	if strings.TrimSpace(account.Status) == "" {
		account.Status = domain.StatusActive
	}
	account.UpdatedAt = now
	change, _, err := s.upsertEntityTx(tx, "apple_accounts", "apple-account", account.ID, account)
	if err != nil {
		_ = tx.Rollback()
		return domain.AppleAccount{}, false, err
	}
	return account, created, s.commitTx(tx, []Change{change})
}

// SetIMAPAccountStatus 更新母号的服务商侧状态与说明。
// ICloudStatus 是「服务商侧状态」的通用槽位，前端只渲染一个状态标签。
func (s *Store) SetIMAPAccountStatus(accountID, status, message string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return errors.New("母号标识不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	var account domain.AppleAccount
	if found, err := s.readEntityTx(tx, "apple_accounts", accountID, &account); err != nil || !found {
		_ = tx.Rollback()
		if err != nil {
			return err
		}
		return errors.New("IMAP 母号不存在")
	}
	account.ICloudStatus = status
	account.UpdatedAt = time.Now()
	if strings.TrimSpace(message) != "" {
		account.Note = strings.TrimSpace(message)
	}
	change, changed, err := s.upsertEntityTx(tx, "apple_accounts", "apple-account", account.ID, account)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if !changed {
		_ = tx.Rollback()
		return nil
	}
	return s.commitTx(tx, []Change{change})
}

// stripMailboxSecrets 清掉不出存储层的凭据字段。
//
// Mailbox 同时当落库格式和接口返回格式用，所以不能靠 json:"-" 把字段藏起来——
// 那会把落库一起藏掉。只能在这里清，而且每一个把邮箱交给上层的读路径都要调它：
// 漏一处就是把使用者的邮箱密码发给浏览器。
func stripMailboxSecrets(mailbox *domain.Mailbox) {
	mailbox.APIToken = ""
	mailbox.IMAPSecret = ""
}

// MailboxesByAccount 返回某个母号名下的全部邮箱，不受「是否进邮箱池」影响——
// 母号自己的页面要看得到全部，包括使用者选择不进池的那些。
//
// 带 WHERE 的定向查询，而不是「读全表再在 Go 里过滤」：后者每调用一次都要把整张表
// 解密并反序列化一遍。单个母号看不出差别，母号列表页一次几百个就是几十万次解码。
func (s *Store) MailboxesByAccount(accountID string) []domain.Mailbox {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.Query(`SELECT data_json FROM mailboxes
		WHERE json_extract(data_json, '$.account_id') = ?
		ORDER BY json_extract(data_json, '$.created_at'), id`, accountID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]domain.Mailbox, 0)
	for rows.Next() {
		var data []byte
		var mailbox domain.Mailbox
		if rows.Scan(&data) != nil || s.decodeEntity("mailboxes", data, &mailbox) != nil {
			continue
		}
		stripMailboxSecrets(&mailbox)
		out = append(out, mailbox)
	}
	return out
}

// MailboxesGroupedByAccount 一次取回全部邮箱并按母号分组。
//
// 母号列表页要一次渲染几百个母号，逐个母号调一次 MailboxesByAccount 就是 N+1。
// 这里只读一次、只解密一次，调用方自己按 account_id 取。
func (s *Store) MailboxesGroupedByAccount() map[string][]domain.Mailbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []domain.Mailbox
	if err := s.loadEntities("mailboxes", `json_extract(data_json, '$.created_at')`, &out); err != nil {
		return map[string][]domain.Mailbox{}
	}
	grouped := make(map[string][]domain.Mailbox, len(out))
	for index := range out {
		accountID := strings.TrimSpace(out[index].AccountID)
		if accountID == "" {
			continue
		}
		stripMailboxSecrets(&out[index])
		grouped[accountID] = append(grouped[accountID], out[index])
	}
	return grouped
}
