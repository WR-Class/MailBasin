package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

// MicrosoftStatusFromSession 把一次 OAuth2 授权的结果映射成账号状态。
// 状态只描述"当前这份 token 还能不能用"，不涉及邮箱创建能力，因为 Graph 本身不提供创建别名的接口。
func MicrosoftStatusFromSession(session domain.MicrosoftSession) string {
	if strings.TrimSpace(session.RefreshToken) == "" && strings.TrimSpace(session.AccessToken) == "" {
		return domain.MicrosoftStatusNeedLogin
	}
	if !session.LastCheckedAt.IsZero() {
		if session.LastCheckOK {
			return domain.MicrosoftStatusActive
		}
		return domain.MicrosoftStatusFailed
	}
	if !session.TokenExpiresAt.IsZero() && session.TokenExpiresAt.Before(time.Now()) {
		return domain.MicrosoftStatusExpired
	}
	return domain.MicrosoftStatusActive
}

func cloneMicrosoftSession(session domain.MicrosoftSession) domain.MicrosoftSession {
	out := session
	return out
}

func mergeMicrosoftSession(existing, incoming domain.MicrosoftSession) domain.MicrosoftSession {
	out := cloneMicrosoftSession(incoming)
	out.OwnerID = firstNonEmpty(incoming.OwnerID, existing.OwnerID)
	out.AccountID = firstNonEmpty(incoming.AccountID, existing.AccountID)
	if out.SavedAt.IsZero() {
		out.SavedAt = existing.SavedAt
	}
	out.Email = firstNonEmpty(incoming.Email, existing.Email)
	out.DisplayName = firstNonEmpty(incoming.DisplayName, existing.DisplayName)
	out.Tenant = firstNonEmpty(incoming.Tenant, existing.Tenant)
	out.ClientID = firstNonEmpty(incoming.ClientID, existing.ClientID)
	out.AccessToken = firstNonEmpty(incoming.AccessToken, existing.AccessToken)
	out.RefreshToken = firstNonEmpty(incoming.RefreshToken, existing.RefreshToken)
	out.Scope = firstNonEmpty(incoming.Scope, existing.Scope)
	out.Status = firstNonEmpty(incoming.Status, existing.Status)
	out.LastStatusMessage = firstNonEmpty(incoming.LastStatusMessage, existing.LastStatusMessage)
	out.Note = firstNonEmpty(incoming.Note, existing.Note)
	if out.TokenExpiresAt.IsZero() {
		out.TokenExpiresAt = existing.TokenExpiresAt
	}
	if out.LastCheckedAt.IsZero() {
		out.LastCheckedAt = existing.LastCheckedAt
		out.LastCheckOK = existing.LastCheckOK
	}
	return out
}

func (s *Store) MicrosoftSessionByAccountID(accountID string) (domain.MicrosoftSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var session domain.MicrosoftSession
	found, err := s.readEntity("microsoft_sessions", strings.TrimSpace(accountID), &session)
	return cloneMicrosoftSession(session), found && err == nil
}

func (s *Store) MicrosoftSessions() []domain.MicrosoftSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sessions []domain.MicrosoftSession
	_ = s.loadEntities("microsoft_sessions", `json_extract(data_json, '$.saved_at') DESC`, &sessions)
	return sessions
}

// SaveMicrosoftSession 保存一次 Microsoft 授权结果，并在需要时创建或更新对应的账号记录。
// 账号通过邮箱地址去重，重复授权同一账号会更新原记录而不是新建。
func (s *Store) SaveMicrosoftSession(session domain.MicrosoftSession) (domain.MicrosoftSession, error) {
	return s.saveMicrosoftSession(session, "info", "已更新 Microsoft 登录态", "已添加 Microsoft 账号")
}

func (s *Store) SaveMicrosoftSessionWithEvent(session domain.MicrosoftSession, level, message string) (domain.MicrosoftSession, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "已更新 Microsoft 登录态"
	}
	return s.saveMicrosoftSession(session, level, message, message)
}

func (s *Store) saveMicrosoftSession(session domain.MicrosoftSession, level, updateMessage, createMessage string) (domain.MicrosoftSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.MicrosoftSession{}, err
	}
	level = strings.TrimSpace(level)
	if level == "" {
		level = "info"
	}
	accountID := strings.TrimSpace(session.AccountID)
	var account domain.AppleAccount
	accountCreated := false
	if accountID == "" {
		accountID, err = s.findMicrosoftAccountIDTx(tx, session)
		if err != nil {
			_ = tx.Rollback()
			return domain.MicrosoftSession{}, err
		}
	}
	if accountID != "" {
		if found, err := s.readEntityTx(tx, "apple_accounts", accountID, &account); err != nil || !found {
			_ = tx.Rollback()
			if err != nil {
				return domain.MicrosoftSession{}, err
			}
			return domain.MicrosoftSession{}, errors.New("Microsoft 账号不存在")
		}
	} else {
		accountID, err = s.nextIDTx(tx, "acc")
		if err != nil {
			_ = tx.Rollback()
			return domain.MicrosoftSession{}, err
		}
		now := time.Now()
		account = domain.AppleAccount{
			ID:        accountID,
			Provider:  domain.ProviderMicrosoft,
			Label:     firstNonEmpty(session.Email, "Microsoft 账号"),
			AppleID:   strings.TrimSpace(session.Email),
			Status:    domain.StatusActive,
			CreatedAt: now,
			UpdatedAt: now,
		}
		session.AccountID = accountID
		accountCreated = true
	}
	if session.SavedAt.IsZero() {
		session.SavedAt = time.Now()
	}
	if session.OwnerID == "" {
		var adminData []byte
		var admin domain.Admin
		if tx.QueryRow(`SELECT data_json FROM admins LIMIT 1`).Scan(&adminData) == nil && s.decodeEntity("admins", adminData, &admin) == nil {
			session.OwnerID = admin.ID
		}
	}
	var existing domain.MicrosoftSession
	exists, err := s.readEntityTx(tx, "microsoft_sessions", accountID, &existing)
	if err != nil {
		_ = tx.Rollback()
		return domain.MicrosoftSession{}, err
	}
	if exists {
		session = mergeMicrosoftSession(existing, session)
	}
	session.AccountID = accountID
	session.Status = MicrosoftStatusFromSession(session)

	account.OwnerID = firstNonEmpty(account.OwnerID, session.OwnerID)
	account.Provider = domain.ProviderMicrosoft
	account.AppleID = firstNonEmpty(session.Email, account.AppleID)
	if account.Label == "" {
		account.Label = firstNonEmpty(account.AppleID, "Microsoft 账号")
	}
	account.Status = domain.StatusActive
	account.ICloudStatus = session.Status
	account.Note = firstNonEmpty(session.Note, account.Note)
	account.UpdatedAt = time.Now()

	changes := []Change{}
	accountChange, accountChanged, err := s.upsertEntityTx(tx, "apple_accounts", "apple-account", account.ID, account)
	if err != nil {
		_ = tx.Rollback()
		return domain.MicrosoftSession{}, err
	}
	if accountChanged {
		changes = append(changes, accountChange)
	}
	sessionChange, sessionChanged, err := s.upsertEntityTx(tx, "microsoft_sessions", "microsoft-session", accountID, session)
	if err != nil {
		_ = tx.Rollback()
		return domain.MicrosoftSession{}, err
	}
	if sessionChanged {
		changes = append(changes, sessionChange)
	}
	if !accountChanged && !sessionChanged {
		_ = tx.Rollback()
		return cloneMicrosoftSession(session), nil
	}
	message := updateMessage
	if accountCreated || !exists {
		message = createMessage
	}
	eventChange, err := s.appendEventTx(tx, level, "microsoft", message)
	if err != nil {
		_ = tx.Rollback()
		return domain.MicrosoftSession{}, err
	}
	changes = append(changes, eventChange)
	return cloneMicrosoftSession(session), s.commitTx(tx, changes)
}

// findMicrosoftAccountIDTx 按邮箱地址查找已有的 Microsoft 账号，保证重复授权更新原记录。
func (s *Store) findMicrosoftAccountIDTx(tx *sql.Tx, session domain.MicrosoftSession) (string, error) {
	email := strings.TrimSpace(session.Email)
	if email == "" {
		return "", nil
	}
	var accountID string
	err := tx.QueryRow(`SELECT id FROM apple_accounts
		WHERE lower(trim(COALESCE(json_extract(data_json, '$.apple_id'), ''))) = lower(?)
			AND COALESCE(json_extract(data_json, '$.provider'), 'icloud') = 'microsoft'
		ORDER BY COALESCE(json_extract(data_json, '$.created_at'), updated_at), id
		LIMIT 1`, email).Scan(&accountID)
	if err == nil {
		return strings.TrimSpace(accountID), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	err = tx.QueryRow(`SELECT id FROM microsoft_sessions
		WHERE lower(trim(COALESCE(json_extract(data_json, '$.email'), ''))) = lower(?)
		ORDER BY COALESCE(json_extract(data_json, '$.saved_at'), updated_at), id
		LIMIT 1`, email).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(accountID), nil
}

// UpdateMicrosoftSession 在事务外读取、修改并回写一份 Microsoft 会话。
func (s *Store) UpdateMicrosoftSession(accountID string, update func(*domain.MicrosoftSession) error) (domain.MicrosoftSession, error) {
	accountID = strings.TrimSpace(accountID)
	session, ok := s.MicrosoftSessionByAccountID(accountID)
	if !ok {
		return domain.MicrosoftSession{}, errors.New("Microsoft 账号登录态不存在")
	}
	if err := update(&session); err != nil {
		return domain.MicrosoftSession{}, err
	}
	return s.SaveMicrosoftSession(session)
}

// SaveMicrosoftAccountStatus 只更新账号上的展示状态，不动会话凭据。
func (s *Store) SaveMicrosoftAccountStatus(accountID, status, message string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
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
		return errors.New("Microsoft 账号不存在")
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

// Accounts 按服务商返回账号列表，provider 为空时返回全部。
// 这是 AppleAccounts() 的泛化版本：Microsoft 账号与 iCloud 账号共用同一张表，由 provider 字段区分。
// 注意 ICloudStatus 字段是"服务商侧状态"的通用槽位，前端只渲染一个状态标签。
func (s *Store) Accounts(provider string) []domain.AppleAccount {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []domain.AppleAccount
	_ = s.loadEntities("apple_accounts", `json_extract(data_json, '$.created_at') DESC`, &out)
	wanted := strings.TrimSpace(provider)
	filtered := make([]domain.AppleAccount, 0, len(out))
	for index := range out {
		out[index].Password = ""
		if wanted != "" && out[index].ProviderOrDefault() != wanted {
			continue
		}
		filtered = append(filtered, out[index])
	}
	if wanted == "" || wanted == domain.ProviderICloud {
		if sessions, err := s.iCloudSessionsLocked(); err == nil && len(sessions) > 0 {
			byAccount := make(map[string]domain.ICloudSession, len(sessions))
			for _, session := range sessions {
				byAccount[session.AccountID] = session
			}
			for index := range filtered {
				if filtered[index].ProviderOrDefault() != domain.ProviderICloud {
					continue
				}
				if session, ok := byAccount[filtered[index].ID]; ok {
					filtered[index].ICloudStatus = iCloudStatusFromSession(session)
				}
			}
		}
	}
	if wanted == "" || wanted == domain.ProviderMicrosoft {
		if sessions, err := s.microsoftSessionsLocked(); err == nil && len(sessions) > 0 {
			byAccount := make(map[string]domain.MicrosoftSession, len(sessions))
			for _, session := range sessions {
				byAccount[session.AccountID] = session
			}
			for index := range filtered {
				if filtered[index].ProviderOrDefault() != domain.ProviderMicrosoft {
					continue
				}
				if session, ok := byAccount[filtered[index].ID]; ok {
					filtered[index].ICloudStatus = MicrosoftStatusFromSession(session)
				}
			}
		}
	}
	if wanted == "" || wanted == domain.ProviderGoogle {
		s.applyGoogleStatusLocked(filtered)
	}
	return filtered
}

func (s *Store) microsoftSessionsLocked() ([]domain.MicrosoftSession, error) {
	var sessions []domain.MicrosoftSession
	if err := s.loadEntities("microsoft_sessions", "", &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

func (s *Store) iCloudSessionsLocked() ([]domain.ICloudSession, error) {
	var sessions []domain.ICloudSession
	if err := s.loadEntities("icloud_sessions", "", &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}
