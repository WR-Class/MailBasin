package protocol

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

// Google 的读信走 Gmail REST API，而不是 IMAP。
//
// 与 Graph 最大的差别：Gmail 的列表接口只返回 id，不返回内容，
// 因此同步是「一次列表 + 每封信一次 get」。这个代价必须付，因为路由要看的
// To / Delivered-To 头只有在单封 get 里才有。
//
// 另一个差别是别名模型：免费 Gmail 一个附加地址都创建不了，只有 + 标签寻址。
// you+tag@gmail.com 与 you@gmail.com 投递到同一个收件箱，且 To 头会保留 +tag。
// 所以这里不做"发现别名"（没有可发现的），只做"生成 + 地址"。
const (
	gmailMaxPageSize = 100
	gmailMaxPages    = 5
	// gmailMaxFetchPerSync 限制单次同步最多取多少封正文，避免大收件箱把一轮同步拖太久。
	gmailMaxFetchPerSync = 60

	// MaxPlusAliasLocalPart 来自 RFC 5321 §4.5.3.1.1：@ 之前的本地部分最多 64 个字符。
	// 超过这个长度 Gmail 会直接拒收，但地址本身照样能存进库、也能在列表里显示——
	// 变成一个看起来正常、却永远收不到信的地址。所以必须在构造时就拦住。
	MaxPlusAliasLocalPart = 64
	// plusTagAlphabet 是小写字母加数字的完整 36 字符集。地址都是复制粘贴用的，
	// 不需要为了好念而牺牲字符空间——去掉 i/l/o/0/1 会让碰撞概率涨到三倍多。
	plusTagAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	// DefaultPlusTagLength 是随机标签的默认长度，36^8 ≈ 2.8×10^12，碰撞可以忽略。
	DefaultPlusTagLength = 8
)

// MaxPlusTagLength 返回这个主地址还能容纳多长的 + 标签。返回值小于 1 时无法生成别名。
func MaxPlusTagLength(email string) int {
	email = normalizeMailAddress(email)
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return 0
	}
	local := email[:at]
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	return MaxPlusAliasLocalPart - len(local) - 1
}

// RandomPlusTag 生成一个随机 + 标签。prefix 非空时形如 "shop-a7k2m9p3"，为空时是纯随机串。
// 用 crypto/rand 而不是 math/rand：这些地址是长期身份，不该可预测。
func RandomPlusTag(prefix string, length int) (string, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if length <= 0 {
		length = DefaultPlusTagLength
	}
	suffix, err := randomFromAlphabet(length)
	if err != nil {
		return "", err
	}
	if prefix == "" {
		return suffix, nil
	}
	return prefix + "-" + suffix, nil
}

// randomFromAlphabet 从 plusTagAlphabet 里等概率取 length 个字符。
// 用拒绝采样而不是直接取模：31 不整除 256，取模会让前 8 个字符偏多。
func randomFromAlphabet(length int) (string, error) {
	if length <= 0 {
		return "", nil
	}
	limit := byte(256 - (256 % len(plusTagAlphabet)))
	out := make([]byte, 0, length)
	buf := make([]byte, length*2)
	for len(out) < length {
		if _, err := rand.Read(buf); err != nil {
			return "", errCode("google_alias_random_failed", "生成随机 + 标签失败："+err.Error(), true)
		}
		for _, value := range buf {
			if value >= limit {
				continue
			}
			out = append(out, plusTagAlphabet[int(value)%len(plusTagAlphabet)])
			if len(out) == length {
				break
			}
		}
	}
	return string(out), nil
}

type GmailClient struct {
	httpClient *http.Client
	baseURL    string
	proxy      string
}

// NewGmailClient 构造客户端。proxy 留空时自动解析系统代理。
func NewGmailClient(proxy string) *GmailClient {
	return &GmailClient{
		httpClient: NewHTTPClient(proxy, 60*time.Second),
		baseURL:    GmailBaseURL,
		proxy:      proxy,
	}
}

// GoogleMailSyncOptions 描述一次 Google 收信的范围。
type GoogleMailSyncOptions struct {
	Limit    int
	After    time.Time
	FullScan bool
	Keyword  string
}

// GoogleMailSyncResult 是按邮箱分发后的同步结果。
type GoogleMailSyncResult struct {
	MessagesByMailbox map[string][]domain.GoogleMailMessage
	Scanned           int
	Matched           int
	Unmatched         int
	HasMore           bool
}

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailPart struct {
	PartID   string        `json:"partId"`
	MimeType string        `json:"mimeType"`
	Filename string        `json:"filename"`
	Headers  []gmailHeader `json:"headers"`
	Body     struct {
		Size int    `json:"size"`
		Data string `json:"data"`
	} `json:"body"`
	Parts []gmailPart `json:"parts"`
}

type gmailMessage struct {
	ID           string    `json:"id"`
	ThreadID     string    `json:"threadId"`
	LabelIDs     []string  `json:"labelIds"`
	Snippet      string    `json:"snippet"`
	InternalDate string    `json:"internalDate"`
	Payload      gmailPart `json:"payload"`
}

type gmailMessageList struct {
	Messages []struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	} `json:"messages"`
	NextPageToken      string `json:"nextPageToken"`
	ResultSizeEstimate int    `json:"resultSizeEstimate"`
}

// SyncMailboxes 拉取账号收件箱，并按收件人地址把邮件分发到对应邮箱。
// 没有匹配到任何本地邮箱的邮件会被计入 Unmatched 而不是丢弃，便于判断 + 地址是否配全。
func (c *GmailClient) SyncMailboxes(ctx context.Context, accessToken string, mailboxes []Mailbox, options GoogleMailSyncOptions) (GoogleMailSyncResult, error) {
	result := GoogleMailSyncResult{MessagesByMailbox: map[string][]domain.GoogleMailMessage{}}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return result, errCode("google_access_token_missing", "缺少 access_token，请先登录 Google 账号", false)
	}
	if len(mailboxes) == 0 {
		return result, nil
	}

	byEmail := map[string]Mailbox{}
	for _, mailbox := range mailboxes {
		if email := normalizeMailAddress(mailbox.Email); email != "" {
			byEmail[email] = mailbox
		}
	}
	if len(byEmail) == 0 {
		return result, nil
	}

	limit := options.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > gmailMaxPageSize*gmailMaxPages {
		limit = gmailMaxPageSize * gmailMaxPages
	}
	if limit > gmailMaxFetchPerSync {
		limit = gmailMaxFetchPerSync
	}

	ids, hasMore, err := c.listMessageIDs(ctx, accessToken, limit, options)
	if err != nil {
		return result, err
	}
	result.HasMore = hasMore

	for _, id := range ids {
		message, err := c.getMessage(ctx, accessToken, id)
		if err != nil {
			// 单封取失败不终止整轮：已经取到的仍然有效，缺的下一轮补齐。
			result.HasMore = true
			continue
		}
		result.Scanned++
		converted := convertGmailMessage(message)
		if options.Keyword != "" && !googleMessageMatchesKeyword(converted, options.Keyword) {
			continue
		}
		matched := false
		for _, recipient := range googleRecipientsOf(converted) {
			mailbox, ok := byEmail[recipient]
			if !ok {
				continue
			}
			result.MessagesByMailbox[mailbox.ID] = append(result.MessagesByMailbox[mailbox.ID], converted)
			result.Matched++
			matched = true
			// 一封邮件只归属第一个命中的邮箱，避免多地址同时收到同一封信时重复入库。
			break
		}
		if !matched {
			result.Unmatched++
		}
	}
	// 每个邮箱内部按收信时间升序，保证写库顺序与真实时间线一致。
	for id, messages := range result.MessagesByMailbox {
		sort.SliceStable(messages, func(i, j int) bool {
			return messages[i].ReceivedAt.Before(messages[j].ReceivedAt)
		})
		result.MessagesByMailbox[id] = messages
	}
	return result, nil
}

func (c *GmailClient) listMessageIDs(ctx context.Context, accessToken string, limit int, options GoogleMailSyncOptions) ([]string, bool, error) {
	query := url.Values{}
	query.Set("maxResults", strconv.Itoa(limit))
	search := "in:inbox"
	if !options.After.IsZero() {
		// Gmail 的 after: 接受 Unix 秒，比日期粒度更精确。
		search += " after:" + strconv.FormatInt(options.After.Unix(), 10)
	}
	query.Set("q", search)

	endpoint := c.baseURL + "/messages?" + query.Encode()
	body, status, err := c.get(ctx, accessToken, endpoint)
	if err != nil {
		return nil, false, err
	}
	if status < 200 || status >= 300 {
		return nil, false, gmailError(status, body, "读取 Gmail 收件箱失败")
	}
	var list gmailMessageList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, false, errCode("google_list_parse_failed", "解析 Gmail 邮件列表失败："+err.Error(), true)
	}
	ids := make([]string, 0, len(list.Messages))
	for _, item := range list.Messages {
		if id := strings.TrimSpace(item.ID); id != "" {
			ids = append(ids, id)
		}
	}
	hasMore := strings.TrimSpace(list.NextPageToken) != "" || len(ids) >= limit
	return ids, hasMore, nil
}

func (c *GmailClient) getMessage(ctx context.Context, accessToken, id string) (gmailMessage, error) {
	var message gmailMessage
	endpoint := c.baseURL + "/messages/" + url.PathEscape(id) + "?format=full"
	body, status, err := c.get(ctx, accessToken, endpoint)
	if err != nil {
		return message, err
	}
	if status < 200 || status >= 300 {
		return message, gmailError(status, body, "读取 Gmail 邮件失败")
	}
	if err := json.Unmarshal(body, &message); err != nil {
		return message, errCode("google_message_parse_failed", "解析 Gmail 邮件失败："+err.Error(), true)
	}
	return message, nil
}

func (c *GmailClient) get(ctx context.Context, accessToken, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, googleUnreachableError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, errCode("google_read_failed", "读取 Gmail 响应失败："+err.Error(), true)
	}
	return body, resp.StatusCode, nil
}

// ListAliases 返回账号上可以收到的地址。
//
// 这里刻意只返回主地址：免费 Gmail 无法创建附加地址，也没有任何接口能枚举 + 标签
// ——它们不是真实存在的地址，只是主地址的一个变体。假装"发现"一批别名是误导。
// 真正的多地址能力由 BuildPlusAlias 生成，使用者自己决定标签。
func (c *GmailClient) ListAliases(ctx context.Context, accessToken string) ([]string, error) {
	// 走自己的 HTTP 通道而不是另起一个 auth client：这样 baseURL 与 proxy 的配置
	// 对这个调用同样生效，测试里也能指向桩服务器而不是真的打到 Google。
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, errCode("google_access_token_missing", "缺少 access_token，请先登录 Google 账号", false)
	}
	body, status, err := c.get(ctx, accessToken, c.baseURL+"/profile")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, gmailError(status, body, "读取 Google 账号信息失败")
	}
	var profile GoogleProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		return nil, errCode("google_profile_parse_failed", "解析 Google 账号信息失败："+err.Error(), true)
	}
	email := normalizeMailAddress(profile.EmailAddress)
	if email == "" {
		return nil, errCode("google_profile_email_missing", "Google 账号未返回邮箱地址", true)
	}
	return []string{email}, nil
}

// BuildPlusAlias 把一个 + 标签拼到 Gmail 主地址上。
//
// 免费 Gmail 只能靠这种方式扩展地址：you+shop@gmail.com 与 you@gmail.com
// 投递到同一个收件箱，而且 To / Delivered-To 头会原样保留 +shop。
// 局限也要说清楚：+ 标签暴露真实地址，且部分网站会拒绝或截断 + 之后的字符。
func BuildPlusAlias(email, tag string) (string, error) {
	return buildPlusAlias(email, tag, "google_alias", "Gmail")
}

// BuildMicrosoftPlusAlias 把 + 标签拼到 Microsoft 主地址上。
//
// 免费 Outlook.com 同样支持 plus addressing，这一点是实测确认的：一封发到
// qiaobaochong823680+tag@outlook.com 的邮件确实投递到了该账号，而且 Graph 的
// toRecipients 原样保留了标签（没有被归一化成主地址）。所以分发逻辑与 Gmail 完全一致，
// 唯一的前提是那个 + 地址必须已经在邮箱池里——路由是精确匹配收件人的。
func BuildMicrosoftPlusAlias(email, tag string) (string, error) {
	return buildPlusAlias(email, tag, "microsoft_alias", "Microsoft")
}

// buildPlusAlias 是 Gmail 与 Outlook.com 共用的 + 标签拼接逻辑：两家的语义一致，
// 都是「主地址 + 任意标签投递到同一个收件箱，且标签保留在收件人头里」。
// codePrefix 让两家的错误码保持可区分，便于前端和日志定位到具体服务商。
func buildPlusAlias(email, tag, codePrefix, providerLabel string) (string, error) {
	email = normalizeMailAddress(email)
	tag = strings.TrimSpace(tag)
	if email == "" {
		return "", errCode(codePrefix+"_email_missing", "缺少 "+providerLabel+" 主地址", false)
	}
	if tag == "" {
		return "", errCode(codePrefix+"_tag_missing", "请填写 + 标签，例如 shop", false)
	}
	// + 本身不能进标签：you+a+b@gmail.com 里整段 a+b 会被当成一个标签，
	// 但人看着像是两个标签，而且与"替换已有 + 标签"的语义冲突。
	if strings.Contains(tag, "@") || strings.Contains(tag, "+") || strings.ContainsAny(tag, " \t\r\n") {
		return "", errCode(codePrefix+"_tag_invalid", "+ 标签不能包含 @、+ 或空白字符", false)
	}
	// 两家都不区分大小写，+Shop 与 +shop 是同一个收件箱。
	// 统一转小写，避免同一个地址在库里存成两条。
	tag = strings.ToLower(tag)
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return "", errCode(codePrefix+"_email_invalid", providerLabel+" 主地址格式不正确", false)
	}
	local, domain := email[:at], email[at+1:]
	// 主地址本身已经带 + 时，替换而不是叠加，避免 you+a+b@gmail.com 这种歧义地址。
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	// RFC 5321 的长度上限：超了对方会静默拒收，而地址在库里一切正常，
	// 所以这里必须报错，不能让使用者拿到一个永远收不到信的地址。
	if length := len(local) + 1 + len(tag); length > MaxPlusAliasLocalPart {
		return "", errCode(codePrefix+"_too_long", fmt.Sprintf(
			"地址过长：@ 之前最多 %d 个字符，「%s+%s」是 %d 个，请缩短 + 标签（此主地址最多可容纳 %d 个字符的标签）",
			MaxPlusAliasLocalPart, local, tag, length, MaxPlusAliasLocalPart-len(local)-1), false)
	}
	return local + "+" + tag + "@" + domain, nil
}

// googleRecipientsOf 汇总一封邮件的所有收件地址，用于 + 地址分发。
// Delivered-To 排在最前：它是投递的最终地址，即使 To 头被改写也仍然准确。
func googleRecipientsOf(message domain.GoogleMailMessage) []string {
	out := make([]string, 0, len(message.DeliveredTo)+len(message.ToRecipients))
	seen := map[string]struct{}{}
	appendAddress := func(raw string) {
		address := normalizeMailAddress(raw)
		if address == "" {
			return
		}
		if _, ok := seen[address]; ok {
			return
		}
		seen[address] = struct{}{}
		out = append(out, address)
	}
	for _, value := range message.DeliveredTo {
		appendAddress(value)
	}
	for _, value := range message.ToRecipients {
		appendAddress(value)
	}
	return out
}

func convertGmailMessage(message gmailMessage) domain.GoogleMailMessage {
	headers := map[string][]string{}
	collectGmailHeaders(message.Payload, headers)

	htmlBody, plainBody := extractGmailBodies(message.Payload)
	if strings.TrimSpace(plainBody) == "" {
		plainBody = normalizeMailBody(message.Snippet)
	}
	contentType := "text/plain"
	if strings.TrimSpace(htmlBody) != "" {
		contentType = "text/html"
	}

	receivedAt := time.Now()
	if millis, err := strconv.ParseInt(strings.TrimSpace(message.InternalDate), 10, 64); err == nil && millis > 0 {
		receivedAt = time.UnixMilli(millis)
	}

	return domain.GoogleMailMessage{
		ID:           strings.TrimSpace(message.ID),
		Subject:      strings.TrimSpace(gmailHeaderValue(headers, "Subject")),
		From:         strings.TrimSpace(gmailHeaderValue(headers, "From")),
		ToRecipients: splitMailAddresses(gmailHeaderValue(headers, "To")),
		DeliveredTo:  splitMailAddresses(gmailHeaderValue(headers, "Delivered-To")),
		Body:         plainBody,
		HTMLBody:     htmlBody,
		ContentType:  contentType,
		ReceivedAt:   receivedAt,
	}
}

// collectGmailHeaders 递归收集 MIME 树里所有部分的头。
// 顶层通常只有一部分头，To / Delivered-To 可能出现在任意一层。
func collectGmailHeaders(part gmailPart, out map[string][]string) {
	for _, header := range part.Headers {
		name := strings.TrimSpace(header.Name)
		if name == "" {
			continue
		}
		out[name] = append(out[name], header.Value)
	}
	for _, child := range part.Parts {
		collectGmailHeaders(child, out)
	}
}

func gmailHeaderValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// extractGmailBodies 递归找出纯文本与 HTML 两份正文。
// 只取 text/plain 与 text/html，附件（有 filename 的部分）直接跳过。
func extractGmailBodies(part gmailPart) (html, plain string) {
	mimeType := strings.ToLower(strings.TrimSpace(part.MimeType))
	if strings.TrimSpace(part.Filename) == "" {
		switch mimeType {
		case "text/html":
			if decoded := decodeGmailBody(part.Body.Data); decoded != "" {
				html = decoded
			}
		case "text/plain":
			if decoded := decodeGmailBody(part.Body.Data); decoded != "" {
				plain = decoded
			}
		}
	}
	for _, child := range part.Parts {
		childHTML, childPlain := extractGmailBodies(child)
		if childHTML != "" {
			html = childHTML
		}
		if childPlain != "" {
			plain = childPlain
		}
	}
	return html, plain
}

// decodeGmailBody 解码 Gmail 的正文。正文是 base64url，可能不带填充。
func decodeGmailBody(data string) string {
	data = strings.TrimSpace(data)
	if data == "" {
		return ""
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(data, "=")); err == nil {
		return normalizeMailBody(string(decoded))
	}
	if decoded, err := base64.URLEncoding.DecodeString(data); err == nil {
		return normalizeMailBody(string(decoded))
	}
	if decoded, err := base64.StdEncoding.DecodeString(data); err == nil {
		return normalizeMailBody(string(decoded))
	}
	return ""
}

// splitMailAddresses 从邮件头里拆出地址列表，形如 `A <a@x.com>, b@y.com`。
func splitMailAddresses(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	// 优先走标准解析器：显示名里可以合法地包含逗号（`"Doe, John" <j@x.com>`），
	// 朴素的按逗号切分会把这种地址切出一个垃圾项。标准解析器也认分号，
	// 但要先把分号换成逗号——Gmail 的 To 头两种分隔符都出现过。
	if list, err := mail.ParseAddressList(strings.ReplaceAll(value, ";", ",")); err == nil {
		out := make([]string, 0, len(list))
		for _, address := range list {
			if normalized := normalizeMailAddress(address.Address); normalized != "" {
				out = append(out, normalized)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	// 解析器对格式不规范的头部会整体失败，此时退回朴素切分，
	// 至少把能认出来的地址捞回来，而不是因为一个坏地址丢掉整封信。
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' })
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		address := part
		if start := strings.LastIndex(part, "<"); start >= 0 {
			if end := strings.Index(part[start:], ">"); end > 0 {
				address = part[start+1 : start+end]
			}
		}
		if address = normalizeMailAddress(address); address != "" {
			out = append(out, address)
		}
	}
	return out
}

func googleMessageMatchesKeyword(message domain.GoogleMailMessage, keyword string) bool {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return true
	}
	return containsFold(message.Subject, keyword) ||
		containsFold(message.Body, keyword) ||
		containsFold(message.From, keyword)
}
