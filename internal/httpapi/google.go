package httpapi

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

// Google 的授权是「浏览器跳出去 + Google 回调跳回来」，和 Microsoft 的设备码轮询不同。
//
// 两个直接后果：
//  1. 回调端点必须免鉴权——请求来自 Google 的 302，带的是 state 而不是本服务的会话 cookie。
//     安全性由 state 保证：只有本次发起授权时生成的那个 state 才能对上内存里的流程记录。
//  2. 回调成功后直接把浏览器 302 回账号页，前端刷新即看到新账号，不需要轮询。

// googleFlowTTL 是一次授权流程在内存里保留的最长时间。
const googleFlowTTL = 15 * time.Minute

// googleModuleStatus 是 Google 模块的对外状态。
type googleModuleStatus struct {
	Enabled         bool   `json:"enabled"`
	Configured      bool   `json:"configured"`
	ClientID        string `json:"client_id"`
	HasClientSecret bool   `json:"has_client_secret"`
	RedirectURI     string `json:"redirect_uri"`
	Proxy           string `json:"proxy"`
	ProxyDetected   bool   `json:"proxy_detected"`
	WatcherEnabled  bool   `json:"watcher_enabled"`
	AccountCount    int    `json:"account_count"`
	SetupNote       string `json:"setup_note"`
	AliasLimitNote  string `json:"alias_limit_note"`
}

func (s *Server) googleStatus() googleModuleStatus {
	settings := s.store.Settings()
	clientID := firstNonEmptyText(settings.GoogleClientID, s.cfg.GoogleClientID)
	proxy := firstNonEmptyText(settings.GoogleProxy, s.cfg.GoogleProxy)
	resolved := s.mailbox.GoogleResolvedProxy()
	return googleModuleStatus{
		Enabled:         s.cfg.GoogleEnabled,
		Configured:      strings.TrimSpace(clientID) != "",
		ClientID:        clientID,
		HasClientSecret: strings.TrimSpace(firstNonEmptyText(settings.GoogleClientSecret, s.cfg.GoogleClientSecret)) != "",
		RedirectURI:     s.mailbox.GoogleAuthRedirectURI(),
		Proxy:           proxy,
		ProxyDetected:   resolved != "",
		WatcherEnabled:  settings.EnableGoogleMailWatcher,
		AccountCount:    len(s.store.GoogleAccounts()),
		SetupNote: "Google 没有可以内置的公共客户端，必须自己在 Google Cloud Console 建一个「桌面应用」" +
			"类型的 OAuth 客户端，启用 Gmail API，并把自己的 Gmail 加为测试用户。" +
			"客户端密钥必须一并填上：Google 的文档把它标为可选，但换取令牌时实际必须提供，桌面应用类型也一样。",
		AliasLimitNote: "免费 Gmail 无法创建附加地址，只能用 + 标签（you+tag@gmail.com）。" +
			"+ 地址与主地址共用一个收件箱，且会暴露真实地址；部分网站会拒绝或截断 + 之后的字符。",
	}
}

func (s *Server) handleGoogleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": s.googleStatus()})
}

func (s *Server) handleGoogleAccounts(w http.ResponseWriter, r *http.Request) {
	page, pageSize, query := accountListPage(r)
	result := s.store.AccountsPage(domain.ProviderGoogle, "", query, page, pageSize, false)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": withPageFields(map[string]any{
			"items":        result.Items,
			"module":       s.googleStatus(),
			"module_ready": s.cfg.GoogleEnabled,
		}, result.Page, result.PageSize, result.Total, result.TotalPages),
	})
}

func (s *Server) handleGoogleAccount(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok || account.ProviderOrDefault() != domain.ProviderGoogle {
		writeError(w, http.StatusNotFound, "google_account_not_found", "Google 账号不存在")
		return
	}
	session, hasSession := s.store.GoogleSessionByAccountID(accountID)
	mailboxes := []domain.Mailbox{}
	for _, mailbox := range s.store.AllMailboxes() {
		if strings.TrimSpace(mailbox.AccountID) == accountID {
			mailboxes = append(mailboxes, mailbox)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"account":   account,
			"session":   googleSessionView(session, hasSession),
			"mailboxes": mailboxes,
		},
	})
}

// googleSessionView 只暴露会话的非敏感字段，token 一律不出站。
func googleSessionView(session domain.GoogleSession, found bool) map[string]any {
	if !found {
		return map[string]any{"exists": false}
	}
	return map[string]any{
		"exists":              true,
		"account_id":          session.AccountID,
		"email":               session.Email,
		"display_name":        session.DisplayName,
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

func (s *Server) handleGoogleAccountDelete(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok || account.ProviderOrDefault() != domain.ProviderGoogle {
		writeError(w, http.StatusNotFound, "google_account_not_found", "Google 账号不存在")
		return
	}
	deleted, err := s.store.DeleteAppleAccount(accountID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"deleted": deleted}})
}

// handleGoogleLoginStart 生成授权链接，前端拿到后把浏览器导过去。
func (s *Server) handleGoogleLoginStart(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.GoogleEnabled {
		writeError(w, http.StatusConflict, "google_disabled", "Google 模块已在配置中关闭")
		return
	}
	flow, err := s.mailbox.StartGoogleAuth()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	s.googleFlowMu.Lock()
	s.pruneGoogleFlowsLocked()
	s.googleFlows[flow.State] = flow
	s.googleFlowMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"auth_url":     flow.AuthURL,
			"state":        flow.State,
			"redirect_uri": flow.RedirectURI,
			"expires_at":   flow.ExpiresAt,
			"client_id":    flow.ClientID,
		},
	})
}

// handleGoogleCallback 接收 Google 的回调。这个端点刻意不套 protected：
// 请求来自 Google 的 302 重定向，带的是 state 而不是本服务的会话 cookie。
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	state := strings.TrimSpace(query.Get("state"))

	// Google 在用户拒绝授权时会带 error 回来，此时没有 code。
	if providerError := strings.TrimSpace(query.Get("error")); providerError != "" {
		detail := firstNonEmptyText(strings.TrimSpace(query.Get("error_description")), providerError)
		s.redirectGoogleResult(w, r, "error", detail)
		return
	}
	if state == "" {
		s.redirectGoogleResult(w, r, "error", "回调缺少 state，无法确认这次授权来自本机发起的请求")
		return
	}
	s.googleFlowMu.Lock()
	flow, ok := s.googleFlows[state]
	if ok {
		delete(s.googleFlows, state)
	}
	s.googleFlowMu.Unlock()
	if !ok {
		s.redirectGoogleResult(w, r, "error", "授权会话不存在或已过期，请重新发起")
		return
	}
	if time.Now().After(flow.ExpiresAt) {
		s.redirectGoogleResult(w, r, "error", "授权已超时，请重新发起")
		return
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		s.redirectGoogleResult(w, r, "error", "回调缺少授权码")
		return
	}
	session, err := s.mailbox.CompleteGoogleAuth(r.Context(), flow, code, "")
	if err != nil {
		// 换码失败的原因（客户端类型不对、回调地址没登记、代理不通、code 过期）
		// 只有 Google 自己知道，而错误只随 302 进浏览器、日志里不留痕迹就没法排查。
		// 这里记下 Google 的原始错误，加上真正生效的非敏感配置。
		// 绝不记录：授权码、client_secret、access/refresh token、code_verifier。
		s.log.Error("Google 授权回调换取 token 失败",
			"error", err.Error(),
			"client_id", flow.ClientID,
			"redirect_uri", flow.RedirectURI,
			"proxy", s.mailbox.GoogleResolvedProxy(),
		)
		s.redirectGoogleResult(w, r, "error", err.Error())
		return
	}
	s.log.Info("Google 账号授权成功", "email", session.Email, "account_id", session.AccountID)
	s.redirectGoogleResult(w, r, "connected", firstNonEmptyText(session.Email, "Google 账号已连接"))
}

// redirectGoogleResult 把浏览器送回账号页，并带上结果。
// 授权页是用户自己的页面，用 query 传状态比渲染一页纯文本更顺手。
func (s *Server) redirectGoogleResult(w http.ResponseWriter, r *http.Request, status, message string) {
	target := "/google-accounts?google=" + url.QueryEscape(status) + "&message=" + url.QueryEscape(message)
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleGoogleAccountCheck(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	session, err := s.mailbox.CheckGoogleAccount(r.Context(), accountID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"session": googleSessionView(session, true)},
	})
}

func (s *Server) handleGoogleAliases(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	aliases, err := s.mailbox.GoogleAliases(r.Context(), accountID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data":    map[string]any{"aliases": aliases, "count": len(aliases)},
	})
}

func (s *Server) handleGoogleAliasImport(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	result, err := s.mailbox.ImportGoogleAliases(r.Context(), accountID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

// handleGoogleAliasGenerate 用一个 + 标签生成新地址——Gmail 唯一的免费多地址手段。
func (s *Server) handleGoogleAliasGenerate(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	var body struct {
		Tag   string `json:"tag"`
		Label string `json:"label"`
		Note  string `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	mailbox, created, err := s.mailbox.CreateGoogleAlias(accountID, body.Tag, body.Label, body.Note)
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

// handleGoogleAliasBatch 批量生成 + 标签地址。prefix 留空即纯随机；跨批次不会重复。
func (s *Server) handleGoogleAliasBatch(w http.ResponseWriter, r *http.Request) {
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
	result, err := s.mailbox.CreateGoogleAliases(accountID, body.Prefix, body.Count, body.Label, body.Note)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

// handleGoogleMailboxCreate 手工添加一个完整地址，用于已有其他 Gmail 地址的场景。
func (s *Server) handleGoogleMailboxCreate(w http.ResponseWriter, r *http.Request) {
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
	mailbox, created, err := s.mailbox.ImportGoogleMailbox(accountID, body.Email, body.Label, body.Note)
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

// handleGoogleSettings 保存界面上的 Google 配置（客户端 ID、代理、监听开关）。
func (s *Server) handleGoogleSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID       string `json:"client_id"`
		ClientSecret   string `json:"client_secret"`
		Proxy          string `json:"proxy"`
		RedirectBase   string `json:"redirect_base"`
		WatcherEnabled *bool  `json:"watcher_enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	settings := s.store.Settings()
	settings.GoogleClientID = strings.TrimSpace(body.ClientID)
	settings.GoogleClientSecret = strings.TrimSpace(body.ClientSecret)
	settings.GoogleProxy = strings.TrimSpace(body.Proxy)
	settings.GoogleRedirectBase = strings.TrimSpace(body.RedirectBase)
	if body.WatcherEnabled != nil {
		settings.EnableGoogleMailWatcher = *body.WatcherEnabled
	}
	if _, err := s.store.SaveSettings(settings); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": s.googleStatus()})
}

// pruneGoogleFlowsLocked 清掉过期的授权流程，调用方必须持有锁。
func (s *Server) pruneGoogleFlowsLocked() {
	now := time.Now()
	for state, flow := range s.googleFlows {
		if now.After(flow.ExpiresAt) {
			delete(s.googleFlows, state)
		}
	}
}
