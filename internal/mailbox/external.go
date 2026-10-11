package mailbox

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
	"icloud-privacy-mail-v2/internal/protocol"
)

// ExternalMailboxMaxBatch 是单次导入的条数上限。
const ExternalMailboxMaxBatch = 500

// externalProbeTimeout 是导入时探活单个接口的超时。
const externalProbeTimeout = 12 * time.Second

// ExternalMailboxInput 是导入一条外部取码邮箱所需的信息。
//
// 这类邮箱没有母号、没有登录密码，唯一的凭据就是取码地址里的令牌，
// 所以 CodeAPIURL 是必填项。
type ExternalMailboxInput struct {
	Email      string `json:"email"`
	CodeAPIURL string `json:"code_api_url"`
	Label      string `json:"label,omitempty"`
	Note       string `json:"note,omitempty"`
}

// ExternalMailboxImportItem 是单条导入的结果，包含探活结论。
type ExternalMailboxImportItem struct {
	Email   string `json:"email"`
	Created bool   `json:"created"`
	Updated bool   `json:"updated"`
	Probe   string `json:"probe,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// ExternalMailboxImportResult 是批量导入的结果统计。
type ExternalMailboxImportResult struct {
	Requested int                         `json:"requested"`
	Created   int                         `json:"created"`
	Updated   int                         `json:"updated"`
	Failed    int                         `json:"failed"`
	Items     []ExternalMailboxImportItem `json:"items,omitempty"`
}

// ImportExternalMailboxes 导入一批只有取码接口的裸邮箱。
//
// 每条都会先做语法校验，再向接口发一次探活请求。探活失败不会阻止导入——
// 网络抖动不该让人重来一遍——但结论会如实写进结果和备注，让人一眼看出哪条是死的。
func (s *Service) ImportExternalMailboxes(inputs []ExternalMailboxInput) (ExternalMailboxImportResult, error) {
	if len(inputs) == 0 {
		return ExternalMailboxImportResult{}, errors.New("请至少提供一条邮箱地址和取码地址")
	}
	if len(inputs) > ExternalMailboxMaxBatch {
		return ExternalMailboxImportResult{}, fmt.Errorf("单次最多导入 %d 条，当前 %d 条", ExternalMailboxMaxBatch, len(inputs))
	}
	result := ExternalMailboxImportResult{Requested: len(inputs)}
	for _, input := range inputs {
		item := ExternalMailboxImportItem{Email: strings.ToLower(strings.TrimSpace(input.Email))}
		email, urlErr := normalizeExternalEmail(input.Email)
		if urlErr != nil {
			item.Reason = urlErr.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		item.Email = email
		endpoint, urlErr := normalizeExternalCodeURL(input.CodeAPIURL)
		if urlErr != nil {
			item.Reason = urlErr.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		// 已经存在但不是外部邮箱时拒绝：不把一个正常收信的邮箱改成靠外部接口取码。
		if existing, found := s.store.FindMailboxByEmail(email); found {
			if existing.ProviderOrDefault() != domain.ProviderExternal {
				item.Reason = fmt.Sprintf("该地址已存在，且属于 %s 服务商，未覆盖", existing.ProviderOrDefault())
				result.Failed++
				result.Items = append(result.Items, item)
				continue
			}
		}
		item.Probe = s.probeExternalCodeURL(endpoint)
		note := strings.TrimSpace(input.Note)
		if note == "" {
			note = "外部取码邮箱，无母号无密码，凭取码地址令牌取信"
		}
		note = note + "｜导入探活：" + item.Probe
		mailbox, created, err := s.store.UpsertMailboxFromRemote("", domain.RemoteMailbox{
			Provider:   domain.ProviderExternal,
			Email:      email,
			Label:      firstNonEmpty(strings.TrimSpace(input.Label), email),
			IsActive:   true,
			Origin:     "external_code_api",
			Note:       note,
			CodeAPIURL: endpoint,
		}, note)
		if err != nil {
			item.Reason = err.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		_ = mailbox
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
			"导入外部取码邮箱：新增 %d 个，更新 %d 个，失败 %d 个",
			result.Created, result.Updated, result.Failed))
	}
	return result, nil
}

// probeExternalCodeURL 探活一次取码接口，返回一句人话结论。
// 「还没有验证码」是正常结果，只有连不上或令牌被拒才算异常。
func (s *Service) probeExternalCodeURL(endpoint string) string {
	ctx, cancel := context.WithTimeout(context.Background(), externalProbeTimeout)
	defer cancel()
	client := protocol.NewHTTPClient(s.cfg.GoogleProxy, externalProbeTimeout)
	probe, err := protocol.FetchExternalCode(ctx, client, endpoint)
	if err == nil {
		if probe.Found {
			return "接口可达，且已取到验证码"
		}
		return "接口可达，当前没有验证码"
	}
	code, message, _ := protocol.ErrorDetails(err)
	if code == "external_code_unauthorized" || code == "external_code_not_found" {
		return "令牌可能已失效：" + message
	}
	return "本次探活未成功（" + message + "），导入后请手动同步确认"
}

// SyncExternalMailbox 向外部取码接口要一次验证码，取到就落成一条消息。
//
// 落库走的是和 iCloud / Gmail / Outlook 完全相同的那条消息表，
// 所以取码、租约、导出这些下游路径不需要任何改动。
func (s *Service) SyncExternalMailbox(ctx context.Context, mailboxID string) (int, error) {
	mailbox, ok := s.store.FindMailboxByID(strings.TrimSpace(mailboxID))
	if !ok {
		return 0, errors.New("邮箱不存在")
	}
	if mailbox.ProviderOrDefault() != domain.ProviderExternal {
		return 0, errors.New("该邮箱不是外部取码邮箱")
	}
	if strings.TrimSpace(mailbox.CodeAPIURL) == "" {
		return 0, errors.New("该邮箱没有保存取码地址，请重新导入")
	}
	client := protocol.NewHTTPClient(s.cfg.GoogleProxy, 30*time.Second)
	result, err := protocol.FetchExternalCode(ctx, client, mailbox.CodeAPIURL)
	if err != nil {
		return 0, err
	}
	if !result.Found {
		// 没有新验证码不是错误：如实返回 0，让调用方区分「没码」和「取不到」。
		return 0, nil
	}
	remoteID := firstNonEmpty(strings.TrimSpace(result.MessageID), "external:"+result.Code)
	receivedAt := result.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	subject := firstNonEmpty(strings.TrimSpace(result.Subject), "外部取码接口")
	from := firstNonEmpty(strings.TrimSpace(result.From), "external-code-api")
	if _, added, err := s.store.UpsertMessage(mailbox.ID, remoteID, "external_code_api", subject, from, result.Code, receivedAt); err != nil {
		return 0, err
	} else if added {
		return 1, nil
	}
	return 0, nil
}

// SyncAllExternalMailboxes 依次同步全部外部取码邮箱，返回成功取到新码的邮箱数。
func (s *Service) SyncAllExternalMailboxes(ctx context.Context) (int, error) {
	synced := 0
	var failures []string
	for _, mailbox := range s.store.AllMailboxes() {
		if mailbox.ProviderOrDefault() != domain.ProviderExternal {
			continue
		}
		count, err := s.SyncExternalMailbox(ctx, mailbox.ID)
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
		_ = s.store.RecordEvent("warning", "mailbox", "部分外部取码邮箱同步失败："+strings.Join(failures, "，"))
	}
	return synced, nil
}

// normalizeExternalEmail 校验并归一邮箱地址。
func normalizeExternalEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	if email == "" {
		return "", errors.New("缺少邮箱地址")
	}
	address, err := mail.ParseAddress(email)
	if err != nil || !strings.EqualFold(strings.TrimSpace(address.Address), email) {
		return "", errors.New("邮箱地址格式不正确")
	}
	return email, nil
}

// normalizeExternalCodeURL 校验取码地址，并拒绝把令牌之外的敏感部分当参数。
func normalizeExternalCodeURL(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", errors.New("缺少取码地址")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("取码地址不是合法的 URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("取码地址必须以 http:// 或 https:// 开头")
	}
	if parsed.Host == "" {
		return "", errors.New("取码地址缺少主机名")
	}
	return parsed.String(), nil
}

// externalEmailPattern / externalURLPattern 用于从一段自由文本里捞出地址和取码链接。
// 允许的粘贴形态很多——纯文本两列、逗号分隔、Markdown 表格单元格——
// 所以按「找到什么」解析，而不是按「第几列」解析。
var (
	externalEmailPattern = regexp.MustCompile("`?([A-Za-z0-9._%+\\-]+@[A-Za-z0-9.\\-]+\\.[A-Za-z]{2,})`?")
	externalURLPattern   = regexp.MustCompile(`https?://[^\s)\]|<>"'` + "`" + `]+`)
)

// ParseExternalMailboxLines 把多行粘贴解析成导入项。
//
// 支持这些写法（可混用）：
//
//	tippet_crooked_7j@icloud.com https://host/api/v1/access/TOKEN/mailboxes/.../code
//	labels_rubies.7t@icloud.com,https://host/...
//	| 3 | `gorge-layman.0i@icloud.com` | [链接](https://host/...) |
//
// 解析不出邮箱或取码链接的行会被跳过，跳过数由调用方报告，不静默吞掉。
func ParseExternalMailboxLines(raw string) ([]ExternalMailboxInput, int) {
	inputs := make([]ExternalMailboxInput, 0, 8)
	skipped := 0
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		emailMatch := externalEmailPattern.FindStringSubmatch(line)
		urlMatch := externalURLPattern.FindString(line)
		if len(emailMatch) < 2 || urlMatch == "" {
			skipped++
			continue
		}
		inputs = append(inputs, ExternalMailboxInput{
			Email:      strings.ToLower(emailMatch[1]),
			CodeAPIURL: strings.TrimSpace(urlMatch),
		})
	}
	return inputs, skipped
}
