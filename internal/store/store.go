package store

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

type Store struct {
	mu             sync.RWMutex
	path           string
	db             *sql.DB
	codec          *secretCodec
	changes        *changeHub
	changeLogLimit int
}

func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = filepath.Join("data", "app.db")
	}
	s := &Store{path: path, changes: newChangeHub(), changeLogLimit: defaultChangeLogLimit}
	if err := s.openDatabase(); err != nil {
		return nil, err
	}
	if err := s.initializeDatabase(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) initializeDatabase() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	now := time.Now()
	if _, err := tx.Exec(`UPDATE metadata SET value = ? WHERE key = 'created_at' AND value = ''`, now.Format(time.RFC3339Nano)); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO metadata(key, value) VALUES ('schema_version', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.Itoa(databaseSchemaVersion)); err != nil {
		_ = tx.Rollback()
		return err
	}
	settings := domain.DefaultSettings()
	if found, err := s.readEntityTx(tx, "settings", "system", &settings); err != nil {
		_ = tx.Rollback()
		return err
	} else if !found {
		settings = domain.DefaultSettings()
	}
	settings.AppleAccountModuleReady = true
	if _, _, err := s.upsertEntityTx(tx, "settings", "settings", "system", settings); err != nil {
		_ = tx.Rollback()
		return err
	}
	var createSettings domain.CreateSettings
	if found, err := s.readEntityTx(tx, "create_settings", "system", &createSettings); err != nil {
		_ = tx.Rollback()
		return err
	} else if !found {
		createSettings = domain.DefaultCreateSettings()
	}
	normalizeCreateSettings(&createSettings)
	if _, _, err := s.upsertEntityTx(tx, "create_settings", "create-settings", "system", createSettings); err != nil {
		_ = tx.Rollback()
		return err
	}
	return s.commitTx(tx, nil)
}

func normalizeCreateSettings(settings *domain.CreateSettings) {
	defaults := domain.DefaultCreateSettings()
	if settings.Mode != "once" && settings.Mode != "scheduled" {
		settings.Mode = defaults.Mode
	}
	if strings.TrimSpace(settings.Label) == "" {
		settings.Label = defaults.Label
	}
	if strings.TrimSpace(settings.CreateChannel) == "" {
		settings.CreateChannel = defaults.CreateChannel
	}
	if strings.TrimSpace(settings.SchedulerCreateChannel) == "" {
		settings.SchedulerCreateChannel = defaults.SchedulerCreateChannel
	}
	if strings.TrimSpace(settings.AppleAccountTwoFactorMethod) == "" {
		settings.AppleAccountTwoFactorMethod = defaults.AppleAccountTwoFactorMethod
	}
	if strings.TrimSpace(settings.ICloudWebTwoFactorMethod) == "" {
		settings.ICloudWebTwoFactorMethod = defaults.ICloudWebTwoFactorMethod
	}
	if settings.SchedulerIntervalMinMinutes <= 0 {
		settings.SchedulerIntervalMinMinutes = defaults.SchedulerIntervalMinMinutes
	}
	if settings.SchedulerIntervalMaxMinutes <= 0 {
		settings.SchedulerIntervalMaxMinutes = settings.SchedulerIntervalMinMinutes
	}
	if settings.SchedulerIntervalMinMinutes > settings.SchedulerIntervalMaxMinutes {
		settings.SchedulerIntervalMinMinutes, settings.SchedulerIntervalMaxMinutes = settings.SchedulerIntervalMaxMinutes, settings.SchedulerIntervalMinMinutes
	}
	if settings.SchedulerAccountIntervalMinSeconds <= 0 {
		settings.SchedulerAccountIntervalMinSeconds = defaults.SchedulerAccountIntervalMinSeconds
	}
	if settings.SchedulerAccountIntervalMaxSeconds <= 0 {
		settings.SchedulerAccountIntervalMaxSeconds = settings.SchedulerAccountIntervalMinSeconds
	}
	if settings.SchedulerAccountIntervalMinSeconds > settings.SchedulerAccountIntervalMaxSeconds {
		settings.SchedulerAccountIntervalMinSeconds, settings.SchedulerAccountIntervalMaxSeconds = settings.SchedulerAccountIntervalMaxSeconds, settings.SchedulerAccountIntervalMinSeconds
	}
}

func (s *Store) loadEntities(table, order string, target any) error {
	query := `SELECT data_json FROM ` + table
	if strings.TrimSpace(order) != "" {
		query += ` ORDER BY ` + order
	}
	rows, err := s.db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	items := make([]json.RawMessage, 0)
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return err
		}
		plain, err := s.unprotectJSON(table, data)
		if err != nil {
			return err
		}
		items = append(items, plain)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// Snapshot 从 SQLite 组装导出快照，不保留运行期全量状态缓存。
func (s *Store) Snapshot() domain.State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := domain.State{SchemaVersion: domain.SchemaVersion, Settings: domain.DefaultSettings(), CreateSettings: domain.DefaultCreateSettings()}
	_ = s.loadEntities("apple_accounts", `json_extract(data_json, '$.created_at')`, &state.AppleAccounts)
	_ = s.loadEntities("mailboxes", `json_extract(data_json, '$.created_at')`, &state.Mailboxes)
	_ = s.loadEntities("mailbox_leases", `json_extract(data_json, '$.created_at')`, &state.MailboxLeases)
	_ = s.loadEntities("messages", `json_extract(data_json, '$.received_at')`, &state.Messages)
	_ = s.loadEntities("events", `json_extract(data_json, '$.created_at')`, &state.Events)
	_ = s.loadEntities("web_sessions", `json_extract(data_json, '$.created_at')`, &state.Sessions)
	_ = s.loadEntities("icloud_sessions", `json_extract(data_json, '$.saved_at')`, &state.ICloudSessions)
	_ = s.loadEntities("microsoft_sessions", `json_extract(data_json, '$.saved_at')`, &state.MicrosoftSessions)
	_, _ = s.readEntity("settings", "system", &state.Settings)
	_, _ = s.readEntity("create_settings", "system", &state.CreateSettings)
	var admins []domain.Admin
	_ = s.loadEntities("admins", `json_extract(data_json, '$.created_at')`, &admins)
	if len(admins) > 0 {
		state.Admin = &admins[0]
	}
	var rawNext, createdAt, updatedAt string
	_ = s.db.QueryRow(`SELECT value FROM metadata WHERE key = 'next_id'`).Scan(&rawNext)
	_ = s.db.QueryRow(`SELECT value FROM metadata WHERE key = 'created_at'`).Scan(&createdAt)
	_ = s.db.QueryRow(`SELECT value FROM metadata WHERE key = 'updated_at'`).Scan(&updatedAt)
	state.NextID, _ = strconv.Atoi(rawNext)
	state.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	state.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return state
}

func (s *Store) SetupAdmin(username, passwordHash string) (domain.Admin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Admin{}, err
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM admins`).Scan(&count); err != nil {
		_ = tx.Rollback()
		return domain.Admin{}, err
	}
	if count > 0 {
		_ = tx.Rollback()
		return domain.Admin{}, errors.New("管理员已经初始化")
	}
	id, err := s.nextIDTx(tx, "admin")
	if err != nil {
		_ = tx.Rollback()
		return domain.Admin{}, err
	}
	now := time.Now()
	admin := domain.Admin{ID: id, Username: strings.ToLower(strings.TrimSpace(username)), PasswordHash: passwordHash, CreatedAt: now, UpdatedAt: now}
	change, _, err := s.upsertEntityTx(tx, "admins", "admin", admin.ID, admin)
	if err != nil {
		_ = tx.Rollback()
		return domain.Admin{}, err
	}
	eventChange, err := s.appendEventTx(tx, "info", "auth", "已完成单管理员初始化")
	if err != nil {
		_ = tx.Rollback()
		return domain.Admin{}, err
	}
	return admin, s.commitTx(tx, []Change{change, eventChange})
}

func (s *Store) Admin() (domain.Admin, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var admin domain.Admin
	var data []byte
	err := s.db.QueryRow(`SELECT data_json FROM admins ORDER BY json_extract(data_json, '$.created_at') LIMIT 1`).Scan(&data)
	if err != nil || s.decodeEntity("admins", data, &admin) != nil {
		return domain.Admin{}, false
	}
	return admin, true
}

// ResetAdminPassword 就地替换管理员密码并作废全部登录态。
//
// 记录 ID 必须保留：会话和账号的 owner_id 都指向它，删了重建会把这些引用变成悬空值。
// 改密码和清会话放在同一个事务里，避免出现"新密码已生效但旧会话仍然可用"的窗口。
func (s *Store) ResetAdminPassword(passwordHash string) (domain.Admin, int, error) {
	passwordHash = strings.TrimSpace(passwordHash)
	if passwordHash == "" {
		return domain.Admin{}, 0, errors.New("密码散列不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Admin{}, 0, err
	}
	var admin domain.Admin
	var data []byte
	if err := tx.QueryRow(`SELECT data_json FROM admins ORDER BY json_extract(data_json, '$.created_at') LIMIT 1`).Scan(&data); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Admin{}, 0, errors.New("管理员尚未初始化，请先在浏览器完成首次设置")
		}
		return domain.Admin{}, 0, err
	}
	if err := s.decodeEntity("admins", data, &admin); err != nil {
		_ = tx.Rollback()
		return domain.Admin{}, 0, err
	}
	admin.PasswordHash = passwordHash
	admin.UpdatedAt = time.Now()
	change, _, err := s.upsertEntityTx(tx, "admins", "admin", admin.ID, admin)
	if err != nil {
		_ = tx.Rollback()
		return domain.Admin{}, 0, err
	}
	// 会话表用 token 散列做主键，逐行删除才能把变更事件如实记下来。
	var tokenHashes []string
	rows, err := tx.Query(`SELECT id FROM web_sessions`)
	if err != nil {
		_ = tx.Rollback()
		return domain.Admin{}, 0, err
	}
	for rows.Next() {
		var tokenHash string
		if err := rows.Scan(&tokenHash); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return domain.Admin{}, 0, err
		}
		tokenHashes = append(tokenHashes, tokenHash)
	}
	if err := rows.Close(); err != nil {
		_ = tx.Rollback()
		return domain.Admin{}, 0, err
	}
	changes := []Change{change}
	for _, tokenHash := range tokenHashes {
		sessionChange, removed, err := s.deleteEntityTx(tx, "web_sessions", "web-session", tokenHash)
		if err != nil {
			_ = tx.Rollback()
			return domain.Admin{}, 0, err
		}
		if removed {
			changes = append(changes, sessionChange)
		}
	}
	eventChange, err := s.appendEventTx(tx, "info", "auth", "已重置管理员密码，全部登录态已作废")
	if err != nil {
		_ = tx.Rollback()
		return domain.Admin{}, 0, err
	}
	changes = append(changes, eventChange)
	return admin, len(tokenHashes), s.commitTx(tx, changes)
}

func (s *Store) MarkLogin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	var admin domain.Admin
	var data []byte
	if err := tx.QueryRow(`SELECT data_json FROM admins LIMIT 1`).Scan(&data); err != nil {
		_ = tx.Rollback()
		return errors.New("管理员尚未初始化")
	}
	if err := s.decodeEntity("admins", data, &admin); err != nil {
		_ = tx.Rollback()
		return err
	}
	now := time.Now()
	admin.LastLoginAt, admin.UpdatedAt = now, now
	adminChange, _, err := s.upsertEntityTx(tx, "admins", "admin", admin.ID, admin)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	eventChange, err := s.appendEventTx(tx, "info", "auth", "管理员已登录")
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return s.commitTx(tx, []Change{adminChange, eventChange})
}

func (s *Store) SaveSession(tokenHash string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	now := time.Now()
	if _, err := tx.Exec(`DELETE FROM web_sessions WHERE json_extract(data_json, '$.expires_at') <= ?`, now.Format(time.RFC3339Nano)); err != nil {
		_ = tx.Rollback()
		return err
	}
	session := domain.WebSession{TokenHash: tokenHash, CreatedAt: now, LastSeenAt: now, ExpiresAt: expiresAt}
	change, _, err := s.upsertEntityTx(tx, "web_sessions", "web-session", tokenHash, session)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return s.commitTx(tx, []Change{change})
}

func (s *Store) ValidateSession(tokenHash string) (domain.Admin, bool) {
	tokenHash = strings.TrimSpace(tokenHash)
	if tokenHash == "" {
		return domain.Admin{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var session domain.WebSession
	if found, err := s.readEntity("web_sessions", tokenHash, &session); err != nil || !found || !session.ExpiresAt.After(time.Now()) {
		return domain.Admin{}, false
	}
	var admin domain.Admin
	var data []byte
	if err := s.db.QueryRow(`SELECT data_json FROM admins LIMIT 1`).Scan(&data); err != nil || s.decodeEntity("admins", data, &admin) != nil {
		return domain.Admin{}, false
	}
	if !constantTimeEqual(session.TokenHash, tokenHash) {
		return domain.Admin{}, false
	}
	// 最近访问时间最多每十分钟持久化一次，并且不作为前端业务变更广播。
	if time.Since(session.LastSeenAt) >= 10*time.Minute {
		session.LastSeenAt = time.Now()
		if tx, err := s.db.Begin(); err == nil {
			if _, changed, saveErr := s.upsertEntityTx(tx, "web_sessions", "web-session", tokenHash, session); saveErr == nil && changed {
				_ = s.commitTx(tx, nil)
			} else {
				_ = tx.Rollback()
			}
		}
	}
	return admin, true
}

func (s *Store) DeleteSession(tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	change, changed, err := s.deleteEntityTx(tx, "web_sessions", "web-session", tokenHash)
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

func (s *Store) Dashboard() domain.Dashboard {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := domain.Dashboard{}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM apple_accounts WHERE COALESCE(json_extract(data_json, '$.provider'), 'icloud') = 'icloud'`).Scan(&out.AppleAccountCount)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM apple_accounts WHERE COALESCE(json_extract(data_json, '$.provider'), 'icloud') = 'microsoft'`).Scan(&out.MicrosoftAccountCount)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM apple_accounts WHERE COALESCE(json_extract(data_json, '$.provider'), 'icloud') = 'google'`).Scan(&out.GoogleAccountCount)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM apple_accounts WHERE lower(json_extract(data_json, '$.status')) = 'active' OR lower(json_extract(data_json, '$.icloud_status')) = 'active'`).Scan(&out.ActiveAccountCount)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM mailboxes`).Scan(&out.MailboxCount)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM mailboxes WHERE json_extract(data_json, '$.api_active') = 1 AND json_extract(data_json, '$.icloud_active') = 1 AND lower(json_extract(data_json, '$.status')) = 'available'`).Scan(&out.AvailableCount)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&out.MessageCount)
	rows, err := s.db.Query(`SELECT data_json FROM events ORDER BY json_extract(data_json, '$.created_at') DESC LIMIT 30`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var data []byte
			var event domain.Event
			if rows.Scan(&data) == nil && s.decodeEntity("events", data, &event) == nil {
				out.Events = append(out.Events, event)
			}
		}
	}
	return out
}

// AppleAccounts 返回 iCloud 账号列表。Microsoft 账号与 iCloud 账号共用同一张表，
// 由 provider 字段区分，因此这里显式过滤，保证既有 Apple 账号页行为不变。
func (s *Store) AppleAccounts() []domain.AppleAccount {
	return s.Accounts(domain.ProviderICloud)
}

// MicrosoftAccounts 返回 Microsoft 账号列表。
func (s *Store) MicrosoftAccounts() []domain.AppleAccount {
	return s.Accounts(domain.ProviderMicrosoft)
}

// GoogleAccounts 返回 Google 账号列表。
func (s *Store) GoogleAccounts() []domain.AppleAccount {
	return s.Accounts(domain.ProviderGoogle)
}

// Mailboxes 返回邮箱池的分页结果。poolOnly 为 true 时排除标记为「不进池」的邮箱
// （目前只有按母号导入的 QQ / 163 地址会这样标记）；母号自己的页面用 AllMailboxes
// 或按 account_id 过滤，看得到全部，不受这里影响。
func (s *Store) Mailboxes(query, status, accountID string, poolOnly bool, page, pageSize int) domain.MailboxPage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	query = strings.ToLower(strings.TrimSpace(query))
	status = strings.ToLower(strings.TrimSpace(status))
	accountID = strings.TrimSpace(accountID)
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 7
	}
	if pageSize > 200 {
		pageSize = 200
	}
	where := []string{"1 = 1"}
	args := []any{}
	if poolOnly {
		// 缺省值必须等于「可见」：旧数据没有这个字段，反过来写会让升级后
		// 所有已有邮箱凭空从邮箱池消失。
		where = append(where, `COALESCE(json_extract(data_json, '$.pool_hidden'), 0) = 0`)
	}
	if accountID != "" {
		where = append(where, `json_extract(data_json, '$.account_id') = ?`)
		args = append(args, accountID)
	}
	if status != "" {
		where = append(where, `lower(json_extract(data_json, '$.status')) = ?`)
		args = append(args, status)
	}
	if query != "" {
		where = append(where, `lower(COALESCE(json_extract(data_json, '$.email'), '') || ' ' || COALESCE(json_extract(data_json, '$.label'), '') || ' ' || COALESCE(json_extract(data_json, '$.note'), '')) LIKE ?`)
		args = append(args, "%"+query+"%")
	}
	clause := strings.Join(where, " AND ")
	var total int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM mailboxes WHERE `+clause, args...).Scan(&total)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	queryArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := s.db.Query(`SELECT data_json FROM mailboxes WHERE `+clause+` ORDER BY json_extract(data_json, '$.created_at') DESC LIMIT ? OFFSET ?`, queryArgs...)
	items := make([]domain.Mailbox, 0, pageSize)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var data []byte
			var mailbox domain.Mailbox
			if rows.Scan(&data) == nil && s.decodeEntity("mailboxes", data, &mailbox) == nil {
				mailbox.APIToken = ""
				items = append(items, mailbox)
			}
		}
	}
	return domain.MailboxPage{Items: items, Page: page, PageSize: pageSize, Total: total, TotalPages: totalPages}
}

func (s *Store) Settings() domain.Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	settings := domain.DefaultSettings()
	_, _ = s.readEntity("settings", "system", &settings)
	return settings
}

func (s *Store) CreateSettings() domain.CreateSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var settings domain.CreateSettings
	if found, _ := s.readEntity("create_settings", "system", &settings); !found {
		settings = domain.DefaultCreateSettings()
	}
	normalizeCreateSettings(&settings)
	settings.AccountIDs = append([]string(nil), settings.AccountIDs...)
	return settings
}

func (s *Store) SaveCreateSettings(settings domain.CreateSettings) (domain.CreateSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalizeCreateSettings(&settings)
	settings.UpdatedAt = time.Now()
	tx, err := s.db.Begin()
	if err != nil {
		return domain.CreateSettings{}, err
	}
	change, _, err := s.upsertEntityTx(tx, "create_settings", "create-settings", "system", settings)
	if err != nil {
		_ = tx.Rollback()
		return domain.CreateSettings{}, err
	}
	eventChange, err := s.appendEventTx(tx, "info", "settings", "已保存邮箱创建设置")
	if err != nil {
		_ = tx.Rollback()
		return domain.CreateSettings{}, err
	}
	return settings, s.commitTx(tx, []Change{change, eventChange})
}

func (s *Store) ICloudSessions() []domain.ICloudSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sessions []domain.ICloudSession
	_ = s.loadEntities("icloud_sessions", `json_extract(data_json, '$.saved_at') DESC`, &sessions)
	return sessions
}

func (s *Store) SaveSettings(settings domain.Settings) (domain.Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	settings.PublicAPIKey = strings.TrimSpace(settings.PublicAPIKey)
	settings.ServerChanSendKey = strings.TrimSpace(settings.ServerChanSendKey)
	settings.AppleAccountModuleReady = true
	tx, err := s.db.Begin()
	if err != nil {
		return domain.Settings{}, err
	}
	change, _, err := s.upsertEntityTx(tx, "settings", "settings", "system", settings)
	if err != nil {
		_ = tx.Rollback()
		return domain.Settings{}, err
	}
	eventChange, err := s.appendEventTx(tx, "info", "settings", "已保存系统设置")
	if err != nil {
		_ = tx.Rollback()
		return domain.Settings{}, err
	}
	return settings, s.commitTx(tx, []Change{change, eventChange})
}

func (s *Store) ClearEvents() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM events`); err != nil {
		_ = tx.Rollback()
		return err
	}
	change := makeEntityChange("events", "event", "all", "deleted", nil, time.Now())
	return s.commitTx(tx, []Change{change})
}

func (s *Store) RecordEvent(level, category, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return nil
	}
	if level = strings.TrimSpace(level); level == "" {
		level = "info"
	}
	if category = strings.TrimSpace(category); category == "" {
		category = "system"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	change, err := s.appendEventTx(tx, level, category, message)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return s.commitTx(tx, []Change{change})
}

func (s *Store) appendEventTx(tx *sql.Tx, level, category, message string) (Change, error) {
	id, err := s.nextIDTx(tx, "evt")
	if err != nil {
		return Change{}, err
	}
	event := domain.Event{ID: id, Level: level, Category: category, Message: message, CreatedAt: time.Now()}
	change, _, err := s.upsertEntityTx(tx, "events", "event", id, event)
	if err != nil {
		return Change{}, err
	}
	if _, err := tx.Exec(`DELETE FROM events WHERE id IN (
		SELECT id FROM events ORDER BY json_extract(data_json, '$.created_at') DESC LIMIT -1 OFFSET 500
	)`); err != nil {
		return Change{}, err
	}
	return change, nil
}

func (s *Store) NextMailboxLabel(prefix string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = domain.DefaultCreateSettings().Label
	}
	rows, err := s.db.Query(`SELECT json_extract(data_json, '$.label') FROM mailboxes WHERE json_extract(data_json, '$.label') LIKE ?`, prefix+"_%")
	if err != nil {
		return prefix + "_1"
	}
	defer rows.Close()
	maxNumber := 0
	marker := prefix + "_"
	for rows.Next() {
		var label string
		if rows.Scan(&label) != nil || !strings.HasPrefix(strings.ToLower(label), strings.ToLower(marker)) {
			continue
		}
		number, err := strconv.Atoi(label[len(marker):])
		if err == nil && number > maxNumber {
			maxNumber = number
		}
	}
	return fmt.Sprintf("%s_%d", prefix, maxNumber+1)
}

func constantTimeEqual(candidate, expected string) bool {
	if candidate == "" || expected == "" || len(candidate) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1
}
