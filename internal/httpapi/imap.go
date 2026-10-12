package httpapi

import (
	"net/http"
	"strings"

	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
	"icloud-privacy-mail-v2/internal/protocol"
)

// imapAccountModuleStatus 是 IMAP 模块（QQ / 163）的对外状态。
// 两个界面共用它，只是 preset 不同。
type imapAccountModuleStatus struct {
	Preset      string `json:"preset"`
	Name        string `json:"name"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Hint        string `json:"hint"`
	Limit       string `json:"limit"`
	AccountHint string `json:"account_hint"`
}

// imapPresetHint 描述每个 preset 在界面上该怎么说。
// 这些话是给使用者照做的，所以只写已经确认过的事实，不写猜测。
func imapPresetHint(preset string) imapAccountModuleStatus {
	chosen := protocol.IMAPPresetByKey(preset)
	return imapAccountModuleStatus{
		Preset: chosen.Key,
		Name:   chosen.Name,
		Host:   chosen.Host,
		Port:   chosen.Port,
		Hint:   chosen.Hint,
		Limit:  chosen.Limit,
	}
}

// handleIMAPAccounts 列出一个 preset 下的全部 IMAP 母号及其地址。
// 母号名下的地址一律全部返回，包括使用者选择不进邮箱池的那些。
func (s *Server) handleIMAPAccounts(w http.ResponseWriter, r *http.Request) {
	preset := strings.TrimSpace(r.URL.Query().Get("preset"))
	items := s.mailbox.IMAPAccounts(preset)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"items":  items,
		"module": imapPresetHint(preset),
	}})
}

// handleIMAPAccount 取一个母号的详情，连同它名下的地址。
func (s *Server) handleIMAPAccount(w http.ResponseWriter, r *http.Request) {
	view, ok := s.mailbox.FindIMAPAccount(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "imap_account_not_found", "该 IMAP 母号不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": view})
}

// handleIMAPAccountCreate 添加一个 IMAP 母号，并把它的地址一起导入。
//
// 母号自己的地址由服务端补进地址列表的第一条：QQ 和 163 的收信都发生在主地址的
// 收件箱里，少了这一条母号页面连自己的信都看不到。
func (s *Server) handleIMAPAccountCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Preset   string                            `json:"preset"`
		Email    string                            `json:"email"`
		Secret   string                            `json:"secret"`
		Host     string                            `json:"host"`
		Port     int                               `json:"port"`
		Username string                            `json:"username"`
		Label    string                            `json:"label"`
		Note     string                            `json:"note"`
		Raw      string                            `json:"raw"`
		Items    []mailboxservice.IMAPMailboxInput `json:"items"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	items := body.Items
	skipped := 0
	if strings.TrimSpace(body.Raw) != "" {
		parsed, dropped := mailboxservice.ParseIMAPMailboxLines(body.Raw)
		items = append(items, parsed...)
		skipped += dropped
	}
	view, result, err := s.mailbox.CreateIMAPAccount(
		body.Preset, body.Email, body.Secret, body.Host, body.Port, body.Username, body.Label, body.Note, items)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"account": view, "result": result, "skipped_lines": skipped,
	}})
}

// handleIMAPAccountAddresses 往已有母号里追加地址。
// 凭据沿用母号自己保存的那一份，不需要使用者再填一次授权码。
func (s *Server) handleIMAPAccountAddresses(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Raw   string                            `json:"raw"`
		Items []mailboxservice.IMAPMailboxInput `json:"items"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	items := body.Items
	skipped := 0
	if strings.TrimSpace(body.Raw) != "" {
		parsed, dropped := mailboxservice.ParseIMAPMailboxLines(body.Raw)
		items = append(items, parsed...)
		skipped += dropped
	}
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "no_items", "没有解析出任何邮箱地址，请检查粘贴内容")
		return
	}
	result, err := s.mailbox.AddIMAPAccountAddresses(r.PathValue("id"), items)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"result": result, "skipped_lines": skipped,
	}})
}

// handleIMAPAccountSync 读一次该母号名下所有地址的收件箱。
func (s *Server) handleIMAPAccountSync(w http.ResponseWriter, r *http.Request) {
	stored, err := s.mailbox.SyncIMAPAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		// 全军覆没时仍把已取到的数字带回去，前端好如实显示而不是只说一句失败。
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "code": "imap_sync_failed",
			"message": err.Error(), "retryable": true, "data": map[string]any{"stored": stored}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"stored": stored}})
}

// handleIMAPAccountDelete 删除母号及其名下的地址、邮件和租约。
func (s *Server) handleIMAPAccountDelete(w http.ResponseWriter, r *http.Request) {
	result, err := s.mailbox.DeleteIMAPAccount(r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"mailboxes": result.Mailboxes, "messages": result.Messages,
	}})
}

// handleMailboxPoolVisibility 切换一个地址是否出现在邮箱池。
//
// 邮箱池默认只看进池的地址，所以这个开关是使用者把 QQ / 163 地址放进池、
// 或从池里收回母号页面的唯一入口。
func (s *Server) handleMailboxPoolVisibility(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InPool *bool `json:"in_pool"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if body.InPool == nil {
		writeError(w, http.StatusBadRequest, "missing_in_pool", "请说明要放进邮箱池还是移出")
		return
	}
	mailbox, err := s.mailbox.SetMailboxPoolHidden(r.PathValue("id"), !*body.InPool)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"id": mailbox.ID, "email": mailbox.Email, "in_pool": !mailbox.PoolHidden,
	}})
}
