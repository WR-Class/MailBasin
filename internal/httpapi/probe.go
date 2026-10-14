package httpapi

import (
	"fmt"
	"net/http"

	mailboxservice "icloud-privacy-mail-v2/internal/mailbox"
)

// handleMailboxProbeBatch 体检一批邮箱：哪些还能登录读信，哪些读不了了。
//
// 这是只读诊断，不发信、不改任何状态。和批量同步走同一个约定：一次请求只处理
// 调用方给的那一小批，进度由前端分批循环显示——一个几分钟不响应的请求既会被
// 浏览器掐断，也会让界面在整段时间里什么都看不到。
func (s *Server) handleMailboxProbeBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "missing_ids", "请指定要体检的邮箱")
		return
	}
	if len(body.IDs) > mailboxservice.MailboxProbeMaxBatch {
		writeError(w, http.StatusBadRequest, "too_many_ids", fmt.Sprintf(
			"单次最多体检 %d 个邮箱，当前 %d 个，请分批调用",
			mailboxservice.MailboxProbeMaxBatch, len(body.IDs)))
		return
	}
	report := s.mailbox.ProbeMailboxes(r.Context(), body.IDs)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": report})
}
