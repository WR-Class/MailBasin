package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

// GoogleStatusFromSession 把一次 OAuth2 授权的结果映射成账号状态。
//
// 状态只描述"当前这份 token 还能不能用"。Gmail 和 Graph 一样不提供创建别名的接口，
// 免费 Gmail 连一个附加地址都加不了，所以这里不涉及邮箱创建能力。
func GoogleStatusFromSession(session domain.GoogleSession) string {
	if strings.TrimSpace(session.RefreshToken) == "" && strings.TrimSpace(session.AccessToken) == "" {
		return domain.GoogleStatusNeedLogin
	}
	if !session.LastCheckedAt.IsZero() {
		if session.LastCheckOK {
			return domain.GoogleStatusActive
		}
		return domain.GoogleStatusFailed
	}
	if !session.TokenExpiresAt.IsZero() && session.TokenExpiresAt.Before(time.Now()) {
		return domain.GoogleStatusExpired
	}
	return domain.GoogleStatusActive
}

func cloneGoogleSession(session domain.GoogleSession) domain.GoogleSession {
	out := session
	return out
}

func mergeGoogleSession(existing, incoming domain.GoogleSession) domain.GoogleSession {
	out := cloneGoogleSession(incoming)
	out.OwnerID = firstNonEmpty(incoming.OwnerID, existing.OwnerID)
	out.AccountID = firstNonEmpty(incoming.AccountID, existing.AccountID)
	if out.SavedAt.IsZero() {
		out.SavedAt = existing.SavedAt
	}
	out.Email = firstNonEmpty(incoming.Email, existing.Email)
	out.DisplayName = firstNonEmpty(incoming.DisplayName, existing.DisplayName)
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

func (s *Store) GoogleSessionByAccountID(accountID string) (domain.GoogleSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var session domain.GoogleSession
	found, err := s.readEntity("google_sessions", strings.TrimSpace(accountID), &session)
	return cloneGoogleSession(session), found && err == nil
}

func (s *Store) GoogleSessions() []domain.GoogleSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sessions []domain.GoogleSession
	_ = s.loadEntities("google_sessions", `json_extract(data_json, '$.saved_at') DESC`, &sessions)
	return sessions
}

// SaveGoogleSession 保存一次 Google 授权结果，并在需要时创建或更新对应的账号记录。
// 账号通过邮箱地址去重，重复授权同一账号会更新原记录而不是新建。
func (s *Store) SaveGoogleSession(session domain.GoogleSession) (domain.GoogleSession, error) {
	return s.saveGoogleSession(session, "info", "已更新 Google 登录态", "已添加 Google 账号")
}

func (s *Store) SaveGoogleSessionWithEvent(session domain.GoogleSession, level, message string) (domain.GoogleSession, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "已更新 Google 登录态"
	}
	return s.saveGoogleSession(session, level, message, message)
}

func (s *Store) saveGoogleSession(session domain.GoogleSession, level, updateMessage, createMessage string) (domain.GoogleSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.GoogleSession{}, err
	}
	level = strings.TrimSpace(level)
	if level == "" {
		level = "info"
	}
	accountID := strings.TrimSpace(session.AccountID)
	var account domain.AppleAccount
	accountCreated := false
	if accountID == "" {
		accountID, err = s.findGoogleAccountIDTx(tx, session)
		if err != nil {
			_ = tx.Rollback()
			return domain.GoogleSession{}, err
		}
	}
	if accountID != "" {
		if found, err := s.readEntityTx(tx, "apple_accounts", accountID, &account); err != nil || !found {
			_ = tx.Rollback()
			if err != nil {
				return domain.GoogleSession{}, err
			}
			return domain.GoogleSession{}, errors.New("Google 账号不存在")
		}
	} else {
		accountID, err = s.nextIDTx(tx, "acc")
		if err != nil {
			_ = tx.Rollback()
			return domain.GoogleSession{}, err
		}
		now := time.Now()
		account = domain.AppleAccount{
			ID:        accountID,
			Provider:  domain.ProviderGoogle,
			Label:     firstNonEmpty(session.Email, "Google 账号"),
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
	var existing domain.GoogleSession
	exists, err := s.readEntityTx(tx, "google_sessions", accountID, &existing)
	if err != nil {
		_ = tx.Rollback()
		return domain.GoogleSession{}, err
	}
	if exists {
		session = mergeGoogleSession(existing, session)
	}
	session.AccountID = accountID
	session.Status = GoogleStatusFromSession(session)

	account.OwnerID = firstNonEmpty(account.OwnerID, session.OwnerID)
	account.Provider = domain.ProviderGoogle
	account.AppleID = firstNonEmpty(session.Email, account.AppleID)
	if account.Label == "" {
		account.Label = firstNonEmpty(account.AppleID, "Google 账号")
	}
	account.Status = domain.StatusActive
	account.ICloudStatus = session.Status
	account.Note = firstNonEmpty(session.Note, account.Note)
	account.UpdatedAt = time.Now()

	changes := []Change{}
	accountChange, accountChanged, err := s.upsertEntityTx(tx, "apple_accounts", "apple-account", account.ID, account)
	if err != nil {
		_ = tx.Rollback()
		return domain.GoogleSession{}, err
	}
	if accountChanged {
		changes = append(changes, accountChange)
	}
	sessionChange, sessionChanged, err := s.upsertEntityTx(tx, "google_sessions", "google-session", accountID, session)
	if err != nil {
		_ = tx.Rollback()
		return domain.GoogleSession{}, err
	}
	if sessionChanged {
		changes = append(changes, sessionChange)
	}
	if !accountChanged && !sessionChanged {
		_ = tx.Rollback()
		return cloneGoogleSession(session), nil
	}
	message := updateMessage
	if accountCreated || !exists {
		message = createMessage
	}
	eventChange, err := s.appendEventTx(tx, level, "google", message)
	if err != nil {
		_ = tx.Rollback()
		return domain.GoogleSession{}, err
	}
	changes = append(changes, eventChange)
	return cloneGoogleSession(session), s.commitTx(tx, changes)
}

// findGoogleAccountIDTx 按邮箱地址查找已有的 Google 账号，保证重复授权更新原记录。
func (s *Store) findGoogleAccountIDTx(tx *sql.Tx, session domain.GoogleSession) (string, error) {
	email := strings.TrimSpace(session.Email)
	if email == "" {
		return "", nil
	}
	var accountID string
	err := tx.QueryRow(`SELECT id FROM apple_accounts
		WHERE lower(trim(COALESCE(json_extract(data_json, '$.apple_id'), ''))) = lower(?)
			AND COALESCE(json_extract(data_json, '$.provider'), 'icloud') = 'google'
		ORDER BY COALESCE(json_extract(data_json, '$.created_at'), updated_at), id
		LIMIT 1`, email).Scan(&accountID)
	if err == nil {
		return strings.TrimSpace(accountID), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	err = tx.QueryRow(`SELECT id FROM google_sessions
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

// UpdateGoogleSession 在事务外读取、修改并回写一份 Google 会话。
func (s *Store) UpdateGoogleSession(accountID string, update func(*domain.GoogleSession) error) (domain.GoogleSession, error) {
	accountID = strings.TrimSpace(accountID)
	session, ok := s.GoogleSessionByAccountID(accountID)
	if !ok {
		return domain.GoogleSession{}, errors.New("Google 账号登录态不存在")
	}
	if err := update(&session); err != nil {
		return domain.GoogleSession{}, err
	}
	return s.SaveGoogleSession(session)
}

// SaveGoogleAccountStatus 只更新账号上的展示状态，不动会话凭据。
func (s *Store) SaveGoogleAccountStatus(accountID, status, message string) error {
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
		return errors.New("Google 账号不存在")
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

func (s *Store) googleSessionsLocked() ([]domain.GoogleSession, error) {
	var sessions []domain.GoogleSession
	if err := s.loadEntities("google_sessions", "", &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

// applyGoogleStatusLocked 把会话状态回填到账号列表上，与 iCloud / Microsoft 的做法一致。
func (s *Store) applyGoogleStatusLocked(accounts []domain.AppleAccount) {
	sessions, err := s.googleSessionsLocked()
	if err != nil || len(sessions) == 0 {
		return
	}
	byAccount := make(map[string]domain.GoogleSession, len(sessions))
	for _, session := range sessions {
		byAccount[session.AccountID] = session
	}
	for index := range accounts {
		if accounts[index].ProviderOrDefault() != domain.ProviderGoogle {
			continue
		}
		if session, ok := byAccount[accounts[index].ID]; ok {
			accounts[index].ICloudStatus = GoogleStatusFromSession(session)
		}
	}
}
