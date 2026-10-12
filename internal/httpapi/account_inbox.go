package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
)

// handleAccountInbox 返回一个母号名下所有地址的邮件与验证码。
//
// 这个端点故意做成与 service provider 无关：iCloud、Microsoft、Google、
// QQ、163 五种母号共用它，因为要返回的结构完全相同。给每个服务商各开一个
// 端点只会多出五处要跟着改的代码，而它们的差别是零。
//
// 它只读本地库，不发网络请求。取新信是各页面的同步按钮负责的事。
func (s *Server) handleAccountInbox(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit 必须是正整数")
			return
		}
		limit = parsed
	}
	if limit == 0 {
		limit = mailboxservice.AccountInboxMaxMessages
	}
	inbox, err := s.mailbox.AccountInbox(r.PathValue("id"), limit)
	if err != nil {
		writeError(w, http.StatusNotFound, "account_not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": inbox})
}
