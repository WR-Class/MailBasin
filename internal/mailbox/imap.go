package mailbox

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
	"icloud-privacy-mail-v2/internal/store"
)

const (
	// IMAPMailboxMaxBatch 是一次最多导入的地址数。
	// 定得比外部取码邮箱小，因为 QQ 一个账号最多也就 10 个别名，163 免费号只有 1 个，
	// 一次导入几百条多半是粘错了内容。
	IMAPMailboxMaxBatch = 200
	imapProbeTimeout    = 30 * time.Second
	imapDefaultPort     = 993
)

// IMAPMailboxInput 是一条待导入的 IMAP 取码邮箱地址。
type IMAPMailboxInput struct {
	Email string
	Label string
	Note  string
}

// IMAPMailboxImportItem 是单条地址的导入结果。
type IMAPMailboxImportItem struct {
	Email   string `json:"email"`
	Created bool   `json:"created,omitempty"`
	Updated bool   `json:"updated,omitempty"`
	Probe   string `json:"probe,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// IMAPMailboxImportResult 汇总一次导入。
type IMAPMailboxImportResult struct {
	Requested int                     `json:"requested"`
	Created   int                     `json:"created"`
	Updated   int                     `json:"updated"`
	Failed    int                     `json:"failed"`
	Host      string                  `json:"host,omitempty"`
	Items     []IMAPMailboxImportItem `json:"items"`
}

// ResolveIMAPCredentials 把界面上选的服务商预设和手填内容补全成实际连接参数。
// 手填的 host / port 优先于预设，这样「自定义 / 自建」也能用同一套代码。
func ResolveIMAPCredentials(preset, host string, port int, username, secret string) (string, int, string, string) {
	chosen := protocol.IMAPPresetByKey(preset)
	host = firstNonEmpty(strings.TrimSpace(host), chosen.Host)
	if port <= 0 {
		port = chosen.Port
	}
	if port <= 0 {
		port = imapDefaultPort
	}
	return host, port, strings.TrimSpace(username), strings.TrimSpace(secret)
}

// ImportIMAPMailboxes 导入一批共用同一组凭据的 IMAP 取码邮箱。
//
// 凭据是整批共用的，不是逐条填写：一个 QQ 邮箱的所有别名都走同一个授权码，
// 让使用者为 10 个别名重复粘贴 10 次授权码是没必要的折磨。
// 代价是授权码按地址各存一份（都加密），换码时重导入一次即可——导入是幂等的。
func (s *Service) ImportIMAPMailboxes(preset, host string, port int, username, secret string, inputs []IMAPMailboxInput) (IMAPMailboxImportResult, error) {
	if len(inputs) == 0 {
		return IMAPMailboxImportResult{}, errors.New("请至少提供一条邮箱地址")
	}
	if len(inputs) > IMAPMailboxMaxBatch {
		return IMAPMailboxImportResult{}, fmt.Errorf("单次最多导入 %d 条，当前 %d 条", IMAPMailboxMaxBatch, len(inputs))
	}
	host, port, username, secret = ResolveIMAPCredentials(preset, host, port, username, secret)
	host, hostErr := normalizeIMAPHost(host)
	if hostErr != nil {
		return IMAPMailboxImportResult{}, hostErr
	}
	if secret == "" {
		return IMAPMailboxImportResult{}, errors.New("缺少客户端授权码")
	}

	result := IMAPMailboxImportResult{Requested: len(inputs), Host: host}
	// 授权码是整批共用的，所以探活只做一次，结论写给每一条。
	// 用户名留空时按第一条地址补：QQ 的账号名必须是完整邮箱地址，这里默认就用它。
	probeUser := username
	if probeUser == "" {
		probeUser = strings.ToLower(strings.TrimSpace(inputs[0].Email))
	}
	probe := s.probeIMAPCredentials(host, port, probeUser, secret)

	for _, input := range inputs {
		item := IMAPMailboxImportItem{Email: strings.ToLower(strings.TrimSpace(input.Email))}
		email, emailErr := normalizeExternalEmail(input.Email)
		if emailErr != nil {
			item.Reason = emailErr.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		item.Email = email
		// 已经存在但不是 IMAP 邮箱时拒绝：不把一个正常收信的邮箱改成走 IMAP 登录。
		if existing, found := s.store.FindMailboxByEmail(email); found {
			if existing.ProviderOrDefault() != domain.ProviderIMAP {
				item.Reason = fmt.Sprintf("该地址已存在，且属于 %s 服务商，未覆盖", existing.ProviderOrDefault())
				result.Failed++
				result.Items = append(result.Items, item)
				continue
			}
		}
		item.Probe = probe
		note := strings.TrimSpace(input.Note)
		if note == "" {
			note = "IMAP 取码邮箱，凭服务器地址与客户端授权码读信"
		}
		note = note + "｜导入探活：" + probe
		// 账号名逐条回落到该地址本身：QQ 的 IMAP 账号名必须是完整邮箱地址，
		// 而别名地址登录时用的仍是主地址，所以显式给了用户名就一律用它。
		mailboxUser := firstNonEmpty(username, email)
		_, created, err := s.store.UpsertMailboxFromRemote("", domain.RemoteMailbox{
			Provider:     domain.ProviderIMAP,
			Email:        email,
			Label:        firstNonEmpty(strings.TrimSpace(input.Label), email),
			IsActive:     true,
			Origin:       "imap",
			Note:         note,
			IMAPHost:     host,
			IMAPPort:     port,
			IMAPUsername: mailboxUser,
			IMAPSecret:   secret,
		}, note)
		if err != nil {
			item.Reason = err.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		if created {
			result.Created++
			item.Created = true
		} else {
			result.Updated++
			item.Updated = true
		}
		result.Items = append(result.Items, item)
	}
	if result.Created > 0 || result.Updated > 0 {
		_ = s.store.RecordEvent("info", "mailbox", fmt.Sprintf(
			"导入 IMAP 取码邮箱：新增 %d 个，更新 %d 个，失败 %d 个",
			result.Created, result.Updated, result.Failed))
	}
	return result, nil
}

// probeIMAPCredentials 连一次服务器，返回一句人话结论。
// 探活失败不阻止导入——网络抖动不该让人重来一遍——但结论会如实写进备注。
func (s *Service) probeIMAPCredentials(host string, port int, username, secret string) string {
	ctx, cancel := context.WithTimeout(context.Background(), imapProbeTimeout)
	defer cancel()
	probe := s.imapProbe
	if probe == nil {
		probe = protocol.VerifyGenericIMAP
	}
	if err := probe(ctx, host, port, username, secret); err != nil {
		code, message, _ := protocol.ErrorDetails(err)
		if code == "imap_login_failed" || code == "imap_secret_missing" || code == "imap_username_missing" {
			return "授权码或账号名可能不对：" + message
		}
		return "本次探活未成功（" + message + "），导入后请手动同步确认"
	}
	return "服务器可达，且已用该授权码登录成功"
}

// SyncIMAPMailbox 用邮箱自己的 IMAP 凭据读一次收件箱，把新邮件落进消息表。
//
// 落库走的是和 iCloud / Gmail / Outlook 完全相同的那条消息表，
// 所以取码、租约、导出这些下游路径不需要任何改动。
func (s *Service) SyncIMAPMailbox(ctx context.Context, mailboxID string) (int, error) {
	mailbox, ok := s.store.FindMailboxByID(strings.TrimSpace(mailboxID))
	if !ok {
		return 0, errors.New("邮箱不存在")
	}
	if mailbox.ProviderOrDefault() != domain.ProviderIMAP {
		return 0, errors.New("该邮箱不是 IMAP 取码邮箱")
	}
	if strings.TrimSpace(mailbox.IMAPHost) == "" || strings.TrimSpace(mailbox.IMAPSecret) == "" {
		return 0, errors.New("该邮箱没有保存 IMAP 连接信息，请重新导入")
	}
	state := protocol.LoginState{
		Kind:            "imap",
		IMAPEmail:       mailbox.Email,
		IMAPUsername:    firstNonEmpty(strings.TrimSpace(mailbox.IMAPUsername), mailbox.Email),
		IMAPHost:        strings.TrimSpace(mailbox.IMAPHost),
		IMAPPort:        mailbox.IMAPPort,
		IMAPAppPassword: strings.TrimSpace(mailbox.IMAPSecret),
	}
	result, err := protocol.SyncICloudIMAPMessagesWithOptions(ctx, state, []domain.Mailbox{mailbox}, protocol.MailSyncOptions{
		Mode:      protocol.MailSyncModeVerification,
		Limit:     s.cfg.MailWatcherFetchLimit,
		UseCursor: true,
		CursorUID: mailbox.LastSyncUID,
	})
	if err != nil {
		return 0, err
	}
	update := store.MailboxSyncUpdate{MailboxID: mailbox.ID, SyncedAt: time.Now(), LastUID: result.LastUID}
	for _, message := range result.MessagesByMailbox[mailbox.ID] {
		update.Messages = append(update.Messages, store.MailboxSyncMessage{
			RemoteID:    message.RemoteID,
			RemoteIDs:   message.RemoteIDs,
			CanonicalID: message.CanonicalID,
			Source:      "imap",
			Subject:     message.Subject,
			From:        message.From,
			Body:        message.Body,
			HTMLBody:    message.HTMLBody,
			ContentType: message.ContentType,
			ReceivedAt:  message.ReceivedAt,
		})
	}
	return s.store.ApplyMailboxSyncBatch([]store.MailboxSyncUpdate{update})
}

// SyncAllIMAPMailboxes 依次同步全部 IMAP 取码邮箱，返回成功取到新邮件的邮箱数。
func (s *Service) SyncAllIMAPMailboxes(ctx context.Context) (int, error) {
	synced := 0
	var failures []string
	for _, mailbox := range s.store.AllMailboxes() {
		if mailbox.ProviderOrDefault() != domain.ProviderIMAP {
			continue
		}
		count, err := s.SyncIMAPMailbox(ctx, mailbox.ID)
		if err != nil {
			code, message, _ := protocol.ErrorDetails(err)
			failures = append(failures, mailbox.Email+"（"+firstNonEmpty(code, message)+"）")
			continue
		}
		if count > 0 {
			synced++
		}
	}
	if len(failures) > 0 {
		_ = s.store.RecordEvent("warning", "mailbox", "部分 IMAP 取码邮箱同步失败："+strings.Join(failures, "，"))
	}
	return synced, nil
}

// ParseIMAPMailboxLines 把粘贴进来的地址列表解析成导入项。
//
// 只认邮箱地址本身：IMAP 的凭据是整批共用的，所以粘贴内容里除了地址什么都没有。
// 空行和以 # 开头的注释行直接跳过，不计入「跳过行数」。
func ParseIMAPMailboxLines(raw string) ([]IMAPMailboxInput, int) {
	var out []IMAPMailboxInput
	skipped := 0
	seen := make(map[string]struct{})
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		candidate := strings.TrimSpace(line)
		if candidate == "" || strings.HasPrefix(candidate, "#") {
			continue
		}
		// 容忍逗号、分号、制表符和空格分隔的一行多个地址，也容忍「地址 备注」这种写法。
		for _, piece := range strings.FieldsFunc(candidate, func(r rune) bool {
			return r == ',' || r == ';' || r == '\t' || r == ' '
		}) {
			piece = strings.TrimSpace(piece)
			if piece == "" {
				continue
			}
			email, err := normalizeExternalEmail(piece)
			if err != nil {
				skipped++
				continue
			}
			if _, dup := seen[email]; dup {
				continue
			}
			seen[email] = struct{}{}
			out = append(out, IMAPMailboxInput{Email: email})
		}
	}
	return out, skipped
}

// normalizeIMAPHost 只做最基本的形状校验：主机名或 IP，不能带协议和路径。
// 真正的可达性由导入时的探活回答，这里不假装能判断。
func normalizeIMAPHost(value string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(value))
	if host == "" {
		return "", errors.New("缺少 IMAP 服务器地址")
	}
	if strings.Contains(host, "://") || strings.ContainsAny(host, "/ @") {
		return "", errors.New("IMAP 服务器地址只能是主机名或 IP，不要带协议和路径")
	}
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return "", errors.New("IMAP 服务器地址格式不正确")
	}
	if _, err := mail.ParseAddress("probe@" + host); err != nil {
		return "", errors.New("IMAP 服务器地址格式不正确")
	}
	return host, nil
}
