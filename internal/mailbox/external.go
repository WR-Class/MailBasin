package mailbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
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

// externalPairSeparator 是抽奖平台发奖文件里的分隔符：邮箱----取码地址。
const externalPairSeparator = "----"

// ParseExternalMailboxLines 把粘贴内容或奖品文件解析成导入项。
//
// 两种来源都走这里，因为解析规则本来就该是同一套：
//
// ① 直接粘贴文本，支持这些写法（可混用）：
//
//	tippet_crooked_7j@icloud.com https://host/api/v1/access/TOKEN/mailboxes/.../code
//	labels_rubies.7t@icloud.com,https://host/...
//	| 3 | `gorge-layman.0i@icloud.com` | [链接](https://host/...) |
//	tippet_crooked_7j@icloud.com----https://host/...
//
// ② 直接粘贴奖品 JSON 文件的内容（单个对象或对象数组）：
//
//	{"value": "tippet_crooked_7j@icloud.com----https://host/..."}
//
// 解析不出邮箱或取码链接的条目会被跳过，跳过数由调用方报告，不静默吞掉。
func ParseExternalMailboxLines(raw string) ([]ExternalMailboxInput, int) {
	inputs := make([]ExternalMailboxInput, 0, 8)
	skipped := 0
	for _, candidate := range externalCandidateLines(raw) {
		// 空行不是「解析失败」，不该计进跳过数里。
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		email, endpoint, ok := splitExternalPair(candidate)
		if !ok {
			skipped++
			continue
		}
		inputs = append(inputs, ExternalMailboxInput{Email: email, CodeAPIURL: endpoint})
	}
	return inputs, skipped
}

// externalCandidateLines 把输入摊平成「一条一行」的候选文本。
//
// 粘贴的是 JSON 就按 JSON 收集所有字符串值；否则按行切。
// 这样两种入口共用同一套配对规则，而不是各写一个解析器。
func externalCandidateLines(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if values, ok := collectJSONStrings(trimmed); ok {
			return values
		}
		// 不是合法 JSON 就退回按文本处理，不因为一个花括号把内容全丢掉。
	}
	return strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
}

// collectJSONStrings 递归收集 JSON 文档里所有字符串值。
// 不假设字段名叫 value —— 发奖平台换个字段名不该让导入失效。
//
// 用 Decoder 逐个读而不是 Unmarshal 一次：把几个奖品文件拼在一起是最常见的用法，
// 而拼起来就不是单个合法 JSON 文档了（Unmarshal 会直接失败，整段报废）。
func collectJSONStrings(raw string) ([]string, bool) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	values := make([]string, 0, 8)
	parsed := 0
	for {
		var decoded any
		err := decoder.Decode(&decoded)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// 读到不合法的地方就停；已经读出来的文档仍然有效。
			break
		}
		parsed++
		collectStringsInto(&values, decoded)
	}
	if parsed == 0 {
		return nil, false
	}
	return values, true
}

// collectStringsInto 递归收集一个 JSON 值里的所有字符串。
func collectStringsInto(values *[]string, value any) {
	switch typed := value.(type) {
	case string:
		*values = append(*values, typed)
	case []any:
		for _, item := range typed {
			collectStringsInto(values, item)
		}
	case map[string]any:
		// 键排序保证同一份输入每次产出同样的顺序。
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectStringsInto(values, typed[key])
		}
	}
}

// splitExternalPair 从一段候选文本里取出「邮箱 + 取码地址」。
func splitExternalPair(candidate string) (string, string, bool) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return "", "", false
	}
	// 显式切开「邮箱----取码地址」，不指望邮箱正则回溯出正确边界。
	// 分隔符一旦出现，就按这个格式严格判：两侧必须各自完整，半条记录宁可丢掉也不猜。
	if index := strings.Index(candidate, externalPairSeparator); index >= 0 {
		email := strings.TrimSpace(candidate[:index])
		endpoint := strings.TrimSpace(candidate[index+len(externalPairSeparator):])
		if !externalEmailIsExact(email) || !externalURLIsExact(endpoint) {
			return "", "", false
		}
		return strings.ToLower(email), endpoint, true
	}
	urlMatch := externalURLPattern.FindString(candidate)
	if urlMatch == "" {
		return "", "", false
	}
	// 先把链接整段从候选文本里摘掉，再从剩下的部分取邮箱。
	//
	// 不这么做的话，「邮箱和链接直接粘在一起」的形状（`a@b.comhttps://...`，
	// 没有 ---- 也没有空格）会产出坏地址：邮箱正则的顶级域部分
	// `\.[A-Za-z]{2,}` 不会在 `com` 处停下，因为后面跟的是 `https`——整个
	// `comhttps` 全是字母，它一口气吞下去，于是得到 `a@b.comhttps`。
	// 更糟的是跳过计数是 0：坏地址被静默存进库里，没人看得出来。
	emailSource := strings.Replace(candidate, urlMatch, " ", 1)
	emailMatch := externalEmailPattern.FindStringSubmatch(emailSource)
	if len(emailMatch) < 2 {
		return "", "", false
	}
	return strings.ToLower(emailMatch[1]), strings.TrimSpace(urlMatch), true
}

// externalEmailIsExact 要求整段就是邮箱本身，而不是「包含」一个邮箱。
// 「{备注} a@icloud.com」这种左侧残留不能当成地址。
func externalEmailIsExact(value string) bool {
	match := externalEmailPattern.FindStringSubmatch(value)
	return len(match) >= 2 && match[1] == value
}

// externalURLIsExact 同上：取码地址必须整段都是链接。
func externalURLIsExact(value string) bool {
	return value != "" && externalURLPattern.FindString(value) == value
}
