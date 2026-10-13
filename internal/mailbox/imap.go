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

// ImportIMAPMailboxes 把一批地址挂到一个 IMAP 母号名下。
//
// 凭据不再逐条传入，而是从母号本身读：一个 QQ 邮箱的所有别名都走同一个授权码，
// 让使用者为 10 个别名重复粘贴 10 次授权码是没必要的折磨。
// 代价是授权码按地址各存一份（都加密），换码时重导入一次即可——导入是幂等的。
//
// 新地址默认 PoolHidden，只出现在母号页面；要不要进邮箱池由使用者逐个决定。
func (s *Service) ImportIMAPMailboxes(accountID, preset, host string, port int, username, secret string, inputs []IMAPMailboxInput) (IMAPMailboxImportResult, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return IMAPMailboxImportResult{}, errors.New("缺少母号标识")
	}
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
		_, created, err := s.store.UpsertMailboxFromRemote(accountID, domain.RemoteMailbox{
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
			// 默认不进邮箱池：这批地址的家是母号页面。
			// 只在新建时生效，之后由使用者在界面上自己决定。
			PoolHidden: true,
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
//
// 这里只做增量：只看游标之后的新邮件。全量回填走 SyncIMAPMailboxWithOptions。
func (s *Service) SyncIMAPMailbox(ctx context.Context, mailboxID string) (int, error) {
	return s.SyncIMAPMailboxWithOptions(ctx, mailboxID, MessageSyncOptions{
		Mode:      protocol.MailSyncModeVerification,
		Limit:     s.cfg.MailWatcherFetchLimit,
		UseCursor: true,
	})
}

// SyncIMAPMailboxWithOptions 按调用方给的选项同步一个 IMAP 取码邮箱。
//
// 这个函数存在的理由：原来的 SyncIMAPMailbox 把模式、上限、游标全部硬编码了，
// 于是「全量」这个意图到了这一层就被丢掉——上游明明把它传了下来，函数却不接收。
// 结果是回填按钮对 IMAP 邮箱永远只搜游标之后那一段，旧邮件拿不到，
// 而报告上写的是「成功 0」，看不出是被谁丢的。
//
// 全量模式下不推进游标：扫描可能因为超时或单批失败而只完成一部分，
// 此时把游标推到最新 UID 会让没扫到的那一段永远够不到——这正是要修的毛病。
// 不推进游标的代价只是下次增量同步会重扫一段，而落库按远端 ID 去重，是安全的。
func (s *Service) SyncIMAPMailboxWithOptions(ctx context.Context, mailboxID string, options MessageSyncOptions) (int, error) {
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
	useCursor := options.UseCursor && !options.FullScan
	cursorUID := ""
	if useCursor {
		cursorUID = mailbox.LastSyncUID
	}
	// 走和其它服务商同一条后端接口，而不是直接调协议层：
	// 这样这一层才能被替换，参数是否真的传到了最底层也才测得到。
	backend := s.messageBackend
	if backend == nil {
		backend = defaultMessageSyncBackend{client: s.client}
	}
	result, err := backend.SyncIMAP(ctx, state, []domain.Mailbox{mailbox}, protocol.MailSyncOptions{
		Mode:      options.Mode,
		After:     options.After,
		Limit:     options.Limit,
		FullScan:  options.FullScan,
		UseCursor: useCursor,
		CursorUID: cursorUID,
	})
	if err != nil {
		return 0, err
	}
	lastUID := result.LastUID
	if options.FullScan && result.FailedBatches > 0 {
		lastUID = ""
	}
	update := store.MailboxSyncUpdate{MailboxID: mailbox.ID, SyncedAt: time.Now(), LastUID: lastUID}
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
	created, err := s.store.ApplyMailboxSyncBatch([]store.MailboxSyncUpdate{update})
	if err != nil {
		return 0, err
	}
	if options.FullScan && result.FailedBatches > 0 {
		return created, protocol.NewCodedError("imap_partial_scan", fmt.Sprintf("回填只完成了一部分：有 %d 批邮件没取到，已保存取到的 %d 封，游标未推进，可以再点一次继续", result.FailedBatches, created), true)
	}
	return created, nil
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

// IMAPAccountInput 是批量添加里的一行：一个母号邮箱，和它自己的授权码。
//
// 与 IMAPMailboxInput 的区别在凭据：那里的多个地址共用一个授权码、属于同一个母号；
// 这里的每一行都是一个独立的母号。两种形状不能混用——把六个 Gmail 的授权码
// 当成一个母号的六个别名导入，只有第一个能收到信。
type IMAPAccountInput struct {
	Email  string `json:"email"`
	Secret string `json:"secret"`
}

// ParseIMAPCredentialLines 解析「邮箱:授权码」形式的批量输入。
//
// 只在第一个冒号或制表符处切开，后面的分隔符一律算授权码的一部分——
// 按所有分隔符切会把含冒号的密码截断，而截断后的密码看起来只是个登录失败。
// 空行和以 # 开头的注释行跳过，不计入跳过行数；同一邮箱重复出现只留第一条。
func ParseIMAPCredentialLines(raw string) ([]IMAPAccountInput, int) {
	var out []IMAPAccountInput
	skipped := 0
	seen := make(map[string]struct{})
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		candidate := strings.TrimSpace(line)
		if candidate == "" || strings.HasPrefix(candidate, "#") {
			continue
		}
		index := strings.IndexAny(candidate, ":\t")
		if index <= 0 {
			// 没有分隔符，或分隔符前面是空的（比如开头就是一个冒号）。
			skipped++
			continue
		}
		email, err := normalizeExternalEmail(candidate[:index])
		if err != nil {
			skipped++
			continue
		}
		secret := normalizeCredentialSecret(candidate[index+1:])
		if secret == "" {
			skipped++
			continue
		}
		if _, dup := seen[email]; dup {
			continue
		}
		seen[email] = struct{}{}
		out = append(out, IMAPAccountInput{Email: email, Secret: secret})
	}
	return out, skipped
}

// normalizeCredentialSecret 处理授权码里的空白。
//
// Google 把 16 位应用专用密码分成四组显示（xxxx xxxx xxxx xxxx），照着界面抄下来
// 就带空格，留着空格会让每一行都登录失败。但只在「去掉空白后正好是 16 位字母数字」
// 时才去掉——那正是应用专用密码的形状。换成别的形状就原样保留，免得把一个真的
// 含空格的自建邮箱密码改坏；改坏了使用者还没有别的办法绕过。
func normalizeCredentialSecret(secret string) string {
	trimmed := strings.TrimSpace(secret)
	compacted := strings.Join(strings.Fields(trimmed), "")
	if len(compacted) == 16 && isAlphanumeric(compacted) {
		return compacted
	}
	return trimmed
}

func isAlphanumeric(value string) bool {
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return true
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
