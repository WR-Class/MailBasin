package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
)

// randomID 生成一次性的登录会话标识。
func randomID(prefix string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(buf), nil
}

// microsoftLoginTTL 是设备码登录流程在前端可轮询的最长时间。
const microsoftLoginTTL = 15 * time.Minute

// microsoftClientConfig 解析当前生效的 Microsoft 客户端配置：
// 界面里保存的值优先，config.json 作为部署默认值兜底。
func (s *Server) microsoftClientConfig() (clientID, tenant string) {
	settings := s.store.Settings()
	clientID = firstNonEmptyText(settings.MicrosoftClientID, s.cfg.MicrosoftClientID)
	tenant = firstNonEmptyText(settings.MicrosoftTenant, s.cfg.MicrosoftTenant)
	if tenant == "" {
		tenant = protocol.DefaultMicrosoftTenant
	}
	return clientID, tenant
}

// microsoftModuleStatus 是 Microsoft 模块的对外状态。
type microsoftModuleStatus struct {
	Enabled            bool   `json:"enabled"`
	Configured         bool   `json:"configured"`
	ClientID           string `json:"client_id"`
	Tenant             string `json:"tenant"`
	UsingDefaultClient bool   `json:"using_default_client"`
	WatcherEnabled     bool   `json:"watcher_enabled"`
	AccountCount       int    `json:"account_count"`
	AliasLimitNote     string `json:"alias_limit_note"`
}

func (s *Server) microsoftStatus() microsoftModuleStatus {
	clientID, tenant := s.microsoftClientConfig()
	settings := s.store.Settings()
	usingDefault := strings.TrimSpace(clientID) == ""
	return microsoftModuleStatus{
		Enabled:            s.cfg.MicrosoftEnabled,
		Configured:         s.cfg.MicrosoftEnabled,
		ClientID:           firstNonEmptyText(clientID, protocol.DefaultMicrosoftClientID),
		Tenant:             tenant,
		UsingDefaultClient: usingDefault,
		WatcherEnabled:     settings.EnableMicrosoftMailWatcher,
		AccountCount:       len(s.store.MicrosoftAccounts()),
		AliasLimitNote:     "Microsoft Graph 不提供创建别名的接口，别名需要在 account.microsoft.com 手工创建后再导入；单账号上限 10 个。",
	}
}

func (s *Server) handleMicrosoftStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": s.microsoftStatus()})
}

func (s *Server) handleMicrosoftAccounts(w http.ResponseWriter, _ *http.Request) {
	items := s.store.MicrosoftAccounts()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"items":        items,
			"module":       s.microsoftStatus(),
			"module_ready": s.cfg.MicrosoftEnabled,
		},
	})
}

func (s *Server) handleMicrosoftAccount(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok || account.ProviderOrDefault() != domain.ProviderMicrosoft {
		writeError(w, http.StatusNotFound, "microsoft_account_not_found", "Microsoft 账号不存在")
		return
	}
	session, hasSession := s.store.MicrosoftSessionByAccountID(accountID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"account": account,
			"session": microsoftSessionView(session, hasSession),
			"mailboxes": func() []domain.Mailbox {
				items := []domain.Mailbox{}
				for _, mailbox := range s.store.AllMailboxes() {
					if strings.TrimSpace(mailbox.AccountID) == accountID {
						items = append(items, mailbox)
					}
				}
				return items
			}(),
		},
	})
}

// microsoftSessionView 只暴露会话的非敏感字段，token 一律不出站。
func microsoftSessionView(session domain.MicrosoftSession, found bool) map[string]any {
	if !found {
		return map[string]any{"exists": false}
	}
	return map[string]any{
		"exists":              true,
		"account_id":          session.AccountID,
		"email":               session.Email,
		"display_name":        session.DisplayName,
		"tenant":              session.Tenant,
		"scope":               session.Scope,
		"status":              session.Status,
		"saved_at":            session.SavedAt,
		"token_expires_at":    session.TokenExpiresAt,
		"last_checked_at":     session.LastCheckedAt,
		"last_check_ok":       session.LastCheckOK,
		"last_status_message": session.LastStatusMessage,
		"has_refresh_token":   strings.TrimSpace(session.RefreshToken) != "",
		"has_access_token":    strings.TrimSpace(session.AccessToken) != "",
	}
}

func (s *Server) handleMicrosoftAccountDelete(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok || account.ProviderOrDefault() != domain.ProviderMicrosoft {
		writeError(w, http.StatusNotFound, "microsoft_account_not_found", "Microsoft 账号不存在")
		return
	}
	deleted, err := s.store.DeleteAppleAccount(accountID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"deleted": deleted}})
}

// handleMicrosoftLoginStart 发起设备码登录，返回给用户去浏览器输入的验证码。
func (s *Server) handleMicrosoftLoginStart(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.MicrosoftEnabled {
		writeError(w, http.StatusConflict, "microsoft_disabled", "Microsoft 模块已在配置中关闭")
		return
	}
	clientID, tenant := s.microsoftClientConfig()
	login, err := s.mailbox.StartMicrosoftDeviceLogin(r.Context(), clientID, tenant)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	loginID, err := randomID("mslogin")
	if err != nil {
		writeServiceError(w, err)
		return
	}
	login.AccountID = ""
	s.microsoftLoginMu.Lock()
	s.pruneMicrosoftLoginsLocked()
	s.microsoftLogins[loginID] = login
	s.microsoftLoginMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"login_id":         loginID,
			"user_code":        login.UserCode,
			"verification_uri": login.VerificationURI,
			"message":          login.Message,
			"expires_at":       login.ExpiresAt,
			"interval_seconds": login.IntervalSeconds,
			"client_id":        login.ClientID,
			"tenant":           login.Tenant,
		},
	})
}

// handleMicrosoftLoginPoll 轮询设备码登录结果；成功后立即落库并返回账号。
func (s *Server) handleMicrosoftLoginPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LoginID string `json:"login_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	loginID := strings.TrimSpace(body.LoginID)
	if loginID == "" {
		writeError(w, http.StatusBadRequest, "microsoft_login_id_missing", "缺少 login_id")
		return
	}
	s.microsoftLoginMu.Lock()
	login, ok := s.microsoftLogins[loginID]
	s.microsoftLoginMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "microsoft_login_not_found", "登录会话不存在或已过期，请重新发起")
		return
	}
	if time.Now().After(login.ExpiresAt) {
		s.forgetMicrosoftLogin(loginID)
		writeError(w, http.StatusGone, "microsoft_devicecode_expired", "设备码已过期，请重新发起登录")
		return
	}

	session, err := s.mailbox.PollMicrosoftDeviceLogin(r.Context(), login, "")
	if err != nil {
		if errors.Is(err, protocol.ErrMicrosoftAuthorizationPending) {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": true,
				"data":    map[string]any{"status": "pending", "message": "等待浏览器中完成登录"},
			})
			return
		}
		if errors.Is(err, protocol.ErrMicrosoftSlowDown) {
			writeJSON(w, http.StatusOK, map[string]any{
				"success": true,
				"data":    map[string]any{"status": "slow_down", "message": "轮询过快，请放慢频率"},
			})
			return
		}
		s.forgetMicrosoftLogin(loginID)
		writeServiceError(w, err)
		return
	}
	s.forgetMicrosoftLogin(loginID)
	account, _ := s.store.FindAppleAccount(session.AccountID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"status":  "completed",
			"account": account,
			"session": microsoftSessionView(session, true),
		},
	})
}

func (s *Server) handleMicrosoftAccountCheck(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	session, err := s.mailbox.CheckMicrosoftAccount(r.Context(), accountID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"session": microsoftSessionView(session, true)},
	})
}

// handleMicrosoftAliases 读取账号上的全部邮件地址（Graph 只提供发现，不提供创建）。
func (s *Server) handleMicrosoftAliases(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	aliases, err := s.mailbox.MicrosoftAliases(r.Context(), accountID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"aliases": aliases, "count": len(aliases)},
	})
}

func (s *Server) handleMicrosoftAliasImport(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	result, err := s.mailbox.ImportMicrosoftAliases(r.Context(), accountID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

// handleMicrosoftPrimaryImport 只导入账号的主地址，对应谷歌那边的「导入主地址」。
func (s *Server) handleMicrosoftPrimaryImport(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	result, err := s.mailbox.ImportMicrosoftPrimary(r.Context(), accountID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

// handleMicrosoftAliasBatch 批量生成 Outlook + 标签地址。prefix 留空即纯随机；跨批次不会重复。
func (s *Server) handleMicrosoftAliasBatch(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	var body struct {
		Prefix string `json:"prefix"`
		Count  int    `json:"count"`
		Label  string `json:"label"`
		Note   string `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	result, err := s.mailbox.CreateMicrosoftAliases(accountID, body.Prefix, body.Count, body.Label, body.Note)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

// handleMicrosoftMailboxCreate 手工添加单个别名地址。
func (s *Server) handleMicrosoftMailboxCreate(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	var body struct {
		Email string `json:"email"`
		Label string `json:"label"`
		Note  string `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	mailbox, created, err := s.mailbox.ImportMicrosoftAlias(accountID, body.Email, body.Label, body.Note)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	mailbox.APIToken = ""
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"mailbox": mailbox, "created": created},
	})
}

// handleMicrosoftSettings 保存界面上的 Microsoft 配置（客户端 ID、租户、监听开关）。
func (s *Server) handleMicrosoftSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID       string `json:"client_id"`
		Tenant         string `json:"tenant"`
		WatcherEnabled *bool  `json:"watcher_enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	settings := s.store.Settings()
	settings.MicrosoftClientID = strings.TrimSpace(body.ClientID)
	settings.MicrosoftTenant = strings.TrimSpace(body.Tenant)
	if body.WatcherEnabled != nil {
		settings.EnableMicrosoftMailWatcher = *body.WatcherEnabled
	}
	if _, err := s.store.SaveSettings(settings); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": s.microsoftStatus()})
}

// forgetMicrosoftLogin 丢弃一次已完成的设备码登录会话。
func (s *Server) forgetMicrosoftLogin(loginID string) {
	s.microsoftLoginMu.Lock()
	delete(s.microsoftLogins, loginID)
	s.microsoftLoginMu.Unlock()
}

// pruneMicrosoftLoginsLocked 清掉过期的登录会话，调用方必须持有锁。
func (s *Server) pruneMicrosoftLoginsLocked() {
	now := time.Now()
	for id, login := range s.microsoftLogins {
		if now.After(login.ExpiresAt) {
			delete(s.microsoftLogins, id)
		}
	}
}
