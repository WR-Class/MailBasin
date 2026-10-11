package protocol

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

// Microsoft 的读信走 Microsoft Graph，而不是 IMAP。
// Graph 是官方接口，不需要逆向，也不会因为客户端协议变更而失效；
// 代价是它按"账号"返回整个收件箱，因此需要按收件人把邮件分发到各个别名邮箱。
const (
	// graphMaxPageSize 是单页请求上限。Graph 允许更大值，但 100 页大小在延迟与体积之间更稳。
	graphMaxPageSize = 100
	// graphMaxPages 限制一次同步最多翻多少页，避免超大收件箱把单次请求拖成无限循环。
	graphMaxPages = 10
)

type GraphClient struct {
	httpClient *http.Client
	// baseURL 是 Graph 的根地址。生产环境固定为 microsoftGraphBaseURL，
	// 留出字段是为了让收件人分发逻辑可以在测试里对着本地服务端跑完整链路。
	baseURL string
}

func NewGraphClient() *GraphClient {
	return &GraphClient{httpClient: &http.Client{Timeout: 60 * time.Second}, baseURL: microsoftGraphBaseURL}
}

// MicrosoftMailSyncOptions 描述一次 Microsoft 收信的范围。
type MicrosoftMailSyncOptions struct {
	Limit    int
	After    time.Time
	FullScan bool
	Keyword  string
}

// MicrosoftMailSyncResult 是按邮箱分发后的同步结果。
type MicrosoftMailSyncResult struct {
	MessagesByMailbox map[string][]domain.MicrosoftMailMessage
	Scanned           int
	Matched           int
	Unmatched         int
	HasMore           bool
}

type graphEmailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type graphRecipient struct {
	EmailAddress graphEmailAddress `json:"emailAddress"`
}

type graphMessage struct {
	ID               string           `json:"id"`
	Subject          string           `json:"subject"`
	From             *graphRecipient  `json:"from"`
	ToRecipients     []graphRecipient `json:"toRecipients"`
	CcRecipients     []graphRecipient `json:"ccRecipients"`
	BodyPreview      string           `json:"bodyPreview"`
	ReceivedDateTime string           `json:"receivedDateTime"`
	Body             struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
}

type graphMessageList struct {
	Value    []graphMessage `json:"value"`
	NextLink string         `json:"@odata.nextLink"`
}

// SyncMailboxes 拉取账号收件箱，并按收件人地址把邮件分发到对应邮箱。
// 没有匹配到任何本地邮箱的邮件会被计入 Unmatched 而不是丢弃，便于判断别名是否配置完整。
func (c *GraphClient) SyncMailboxes(ctx context.Context, accessToken string, mailboxes []Mailbox, options MicrosoftMailSyncOptions) (MicrosoftMailSyncResult, error) {
	result := MicrosoftMailSyncResult{MessagesByMailbox: map[string][]domain.MicrosoftMailMessage{}}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return result, errCode("microsoft_access_token_missing", "缺少 access_token，请先登录 Microsoft 账号", false)
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
	if limit > graphMaxPageSize*graphMaxPages {
		limit = graphMaxPageSize * graphMaxPages
	}

	nextLink := c.buildInboxQuery(limit, options)
	page := 0
	for nextLink != "" && page < graphMaxPages {
		page++
		list, err := c.getMessagesPage(ctx, accessToken, nextLink)
		if err != nil {
			if page == 1 {
				return result, err
			}
			// 已经取到的页面仍然可用，把失败降级成"还有更多"，由下一次同步补齐。
			result.HasMore = true
			break
		}
		for _, message := range list.Value {
			result.Scanned++
			converted := convertGraphMessage(message)
			if options.Keyword != "" && !graphMessageMatchesKeyword(converted, options.Keyword) {
				continue
			}
			matched := false
			for _, recipient := range graphRecipientsOf(message) {
				mailbox, ok := byEmail[recipient]
				if !ok {
					continue
				}
				result.MessagesByMailbox[mailbox.ID] = append(result.MessagesByMailbox[mailbox.ID], converted)
				result.Matched++
				matched = true
				// 一封邮件只归属第一个命中的邮箱，避免多别名同时收到同一封信时重复入库。
				break
			}
			if !matched {
				result.Unmatched++
			}
		}
		if list.NextLink == "" {
			nextLink = ""
			break
		}
		nextLink = list.NextLink
		result.HasMore = true
	}
	if result.Scanned >= limit {
		result.HasMore = true
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

func (c *GraphClient) buildInboxQuery(limit int, options MicrosoftMailSyncOptions) string {
	query := url.Values{}
	query.Set("$top", strconv.Itoa(limit))
	query.Set("$select", "id,subject,from,toRecipients,ccRecipients,bodyPreview,body,receivedDateTime")
	query.Set("$orderby", "receivedDateTime desc")
	if !options.After.IsZero() {
		// Graph 要求 $orderby 的字段同时出现在 $filter 中，这里正好满足。
		query.Set("$filter", "receivedDateTime ge "+options.After.UTC().Format("2006-01-02T15:04:05Z"))
	}
	return c.baseURL + "/me/mailFolders/inbox/messages?" + query.Encode()
}

func (c *GraphClient) getMessagesPage(ctx context.Context, accessToken, endpoint string) (graphMessageList, error) {
	var list graphMessageList
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return list, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return list, errCode("microsoft_graph_unreachable", "请求 Microsoft Graph 失败："+err.Error(), true)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return list, errCode("microsoft_graph_read_failed", "读取 Microsoft Graph 响应失败："+err.Error(), true)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return list, microsoftGraphError(resp.StatusCode, body, "读取 Microsoft 收件箱失败")
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return list, errCode("microsoft_graph_parse_failed", "解析 Microsoft 邮件响应失败："+err.Error(), true)
	}
	return list, nil
}

// ListAliases 读取账号上的全部代理地址，用于把用户手工创建的 Outlook 别名批量导入邮箱池。
// Graph 不提供创建别名的接口，因此这里只做发现，不做创建。
func (c *GraphClient) ListAliases(ctx context.Context, accessToken string) ([]string, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, errCode("microsoft_access_token_missing", "缺少 access_token，请先登录 Microsoft 账号", false)
	}
	endpoint := c.baseURL + "/me?" + url.Values{
		"$select": {"mail,userPrincipalName,proxyAddresses"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errCode("microsoft_graph_unreachable", "请求 Microsoft Graph 失败："+err.Error(), true)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errCode("microsoft_graph_read_failed", "读取 Microsoft Graph 响应失败："+err.Error(), true)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, microsoftGraphError(resp.StatusCode, body, "读取 Microsoft 别名失败")
	}
	var out struct {
		Mail              string   `json:"mail"`
		UserPrincipalName string   `json:"userPrincipalName"`
		ProxyAddresses    []string `json:"proxyAddresses"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errCode("microsoft_graph_parse_failed", "解析 Microsoft 别名响应失败："+err.Error(), true)
	}
	seen := map[string]struct{}{}
	aliases := make([]string, 0, len(out.ProxyAddresses)+2)
	appendAlias := func(raw string) {
		address := normalizeMailAddress(raw)
		if address == "" {
			return
		}
		if _, ok := seen[address]; ok {
			return
		}
		seen[address] = struct{}{}
		aliases = append(aliases, address)
	}
	for _, raw := range out.ProxyAddresses {
		// proxyAddresses 的条目形如 "SMTP:primary@outlook.com" 或 "smtp:alias@outlook.com"。
		address := raw
		if index := strings.Index(raw, ":"); index >= 0 {
			address = raw[index+1:]
		}
		appendAlias(address)
	}
	appendAlias(out.Mail)
	appendAlias(out.UserPrincipalName)
	sort.Strings(aliases)
	return aliases, nil
}

// graphRecipientsOf 汇总一封邮件的所有收件人地址，用于别名分发。
func graphRecipientsOf(message graphMessage) []string {
	out := make([]string, 0, len(message.ToRecipients)+len(message.CcRecipients))
	for _, recipient := range message.ToRecipients {
		if address := normalizeMailAddress(recipient.EmailAddress.Address); address != "" {
			out = append(out, address)
		}
	}
	for _, recipient := range message.CcRecipients {
		if address := normalizeMailAddress(recipient.EmailAddress.Address); address != "" {
			out = append(out, address)
		}
	}
	return out
}

func convertGraphMessage(message graphMessage) domain.MicrosoftMailMessage {
	from := ""
	if message.From != nil {
		from = strings.TrimSpace(firstNonEmpty(message.From.EmailAddress.Address, message.From.EmailAddress.Name))
	}
	contentType := strings.ToLower(strings.TrimSpace(message.Body.ContentType))
	htmlBody := ""
	plainBody := ""
	if contentType == "html" {
		htmlBody = message.Body.Content
		plainBody = normalizeMailBody(message.Body.Content)
	} else {
		plainBody = normalizeMailBody(message.Body.Content)
	}
	if strings.TrimSpace(plainBody) == "" {
		plainBody = normalizeMailBody(message.BodyPreview)
	}
	receivedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(message.ReceivedDateTime))
	if err != nil {
		receivedAt = time.Now()
	}
	return domain.MicrosoftMailMessage{
		ID:           strings.TrimSpace(message.ID),
		Subject:      strings.TrimSpace(message.Subject),
		From:         from,
		ToRecipients: graphRecipientsOf(message),
		Body:         plainBody,
		HTMLBody:     htmlBody,
		ContentType:  contentType,
		ReceivedAt:   receivedAt,
	}
}

func graphMessageMatchesKeyword(message domain.MicrosoftMailMessage, keyword string) bool {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return true
	}
	return containsFold(message.Subject, keyword) ||
		containsFold(message.Body, keyword) ||
		containsFold(message.From, keyword)
}

// normalizeMailAddress 统一邮箱地址大小写与空白，保证别名匹配不受大小写影响。
func normalizeMailAddress(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
