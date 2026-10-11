package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

// Microsoft 使用 OAuth2 设备码流完成授权。相比 iCloud 的 SRP，
// 这里没有密码交换、也没有逆向接口：用户在浏览器里完成登录，本地只保存 token。
const (
	microsoftLoginBaseURL = "https://login.microsoftonline.com"
	microsoftGraphBaseURL = "https://graph.microsoft.com/v1.0"

	// MicrosoftOAuthScope 是读取邮件与识别账号所需的最小授权集合。
	// offline_access 是拿到 refresh_token 的前提，缺它会导致 token 过期后无法续期。
	MicrosoftOAuthScope = "offline_access Mail.Read User.Read"

	// DefaultMicrosoftClientID 是 Microsoft 为公共客户端发布的设备码应用标识。
	// 默认值只为让功能开箱可用；生产使用建议在 Azure 门户注册自己的应用并覆盖它。
	DefaultMicrosoftClientID = "d3590ed6-52b3-4102-aeff-aad2292ab01c"
	DefaultMicrosoftTenant   = "common"
)

type MicrosoftAuthClient struct {
	httpClient *http.Client
}

func NewMicrosoftAuthClient() *MicrosoftAuthClient {
	return &MicrosoftAuthClient{httpClient: &http.Client{Timeout: 30 * time.Second}}
}

// MicrosoftTokenResult 是一次成功的 token 交换或续期结果。
type MicrosoftTokenResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Scope        string
	TokenType    string
}

// MicrosoftProfile 是 Graph /me 返回的账号基本信息。
type MicrosoftProfile struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
}

// Email 返回可用于展示和匹配的邮箱地址。
func (p MicrosoftProfile) Email() string {
	return strings.ToLower(strings.TrimSpace(firstNonEmpty(p.Mail, p.UserPrincipalName)))
}

func microsoftTenantOrDefault(tenant string) string {
	tenant = strings.TrimSpace(tenant)
	if tenant == "" {
		return DefaultMicrosoftTenant
	}
	return tenant
}

func microsoftClientIDOrDefault(clientID string) string {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return DefaultMicrosoftClientID
	}
	return clientID
}

// StartDeviceCodeLogin 申请一个设备码，返回给用户去浏览器输入。
func (c *MicrosoftAuthClient) StartDeviceCodeLogin(ctx context.Context, clientID, tenant string) (domain.MicrosoftDeviceLogin, error) {
	clientID = microsoftClientIDOrDefault(clientID)
	tenant = microsoftTenantOrDefault(tenant)
	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/devicecode", microsoftLoginBaseURL, url.PathEscape(tenant))

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("scope", MicrosoftOAuthScope)

	body, status, err := c.postForm(ctx, endpoint, form)
	if err != nil {
		return domain.MicrosoftDeviceLogin{}, err
	}
	if status < 200 || status >= 300 {
		return domain.MicrosoftDeviceLogin{}, microsoftOAuthError(status, body, "申请设备码失败")
	}

	var out struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		Message         string `json:"message"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return domain.MicrosoftDeviceLogin{}, errCode("microsoft_devicecode_parse_failed", "解析设备码响应失败："+err.Error(), true)
	}
	if strings.TrimSpace(out.DeviceCode) == "" || strings.TrimSpace(out.UserCode) == "" {
		return domain.MicrosoftDeviceLogin{}, errCode("microsoft_devicecode_empty", "Microsoft 未返回有效的设备码", true)
	}
	expiresIn := out.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 900
	}
	interval := out.Interval
	if interval <= 0 {
		interval = 5
	}
	return domain.MicrosoftDeviceLogin{
		DeviceCode:      out.DeviceCode,
		UserCode:        out.UserCode,
		VerificationURI: firstNonEmpty(out.VerificationURI, "https://microsoft.com/devicelogin"),
		Message:         out.Message,
		ExpiresAt:       time.Now().Add(time.Duration(expiresIn) * time.Second),
		IntervalSeconds: interval,
		ClientID:        clientID,
		Tenant:          tenant,
	}, nil
}

// ErrMicrosoftAuthorizationPending 表示用户尚未在浏览器中完成登录，调用方应继续轮询。
var ErrMicrosoftAuthorizationPending = errCode("microsoft_authorization_pending", "等待用户在浏览器中完成登录", true)

// ErrMicrosoftSlowDown 表示轮询过快，调用方应拉长间隔后重试。
var ErrMicrosoftSlowDown = errCode("microsoft_slow_down", "轮询过快，请降低频率", true)

// PollDeviceCodeToken 用设备码换取 token。用户还没完成登录时返回 ErrMicrosoftAuthorizationPending。
func (c *MicrosoftAuthClient) PollDeviceCodeToken(ctx context.Context, clientID, tenant, deviceCode string) (MicrosoftTokenResult, error) {
	clientID = microsoftClientIDOrDefault(clientID)
	tenant = microsoftTenantOrDefault(tenant)
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return MicrosoftTokenResult{}, errCode("microsoft_devicecode_missing", "设备码为空", false)
	}
	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token", microsoftLoginBaseURL, url.PathEscape(tenant))

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", deviceCode)

	body, status, err := c.postForm(ctx, endpoint, form)
	if err != nil {
		return MicrosoftTokenResult{}, err
	}
	if status >= 200 && status < 300 {
		return parseMicrosoftToken(body)
	}
	// 设备码流的中间态用 HTTP 400 + error 字段表达，必须与真正的失败区分开。
	if code := microsoftOAuthErrorCode(body); code != "" {
		switch code {
		case "authorization_pending":
			return MicrosoftTokenResult{}, ErrMicrosoftAuthorizationPending
		case "slow_down":
			return MicrosoftTokenResult{}, ErrMicrosoftSlowDown
		case "authorization_declined":
			return MicrosoftTokenResult{}, errCode("microsoft_authorization_declined", "用户拒绝了授权请求", false)
		case "expired_token":
			return MicrosoftTokenResult{}, errCode("microsoft_devicecode_expired", "设备码已过期，请重新发起登录", false)
		case "bad_verification_code":
			return MicrosoftTokenResult{}, errCode("microsoft_devicecode_invalid", "设备码无效，请重新发起登录", false)
		}
	}
	return MicrosoftTokenResult{}, microsoftOAuthError(status, body, "设备码换取 token 失败")
}

// RefreshAccessToken 用 refresh_token 换一份新的 access_token。
// Microsoft 会在续期时轮换 refresh_token，因此返回值必须整体覆盖旧会话。
func (c *MicrosoftAuthClient) RefreshAccessToken(ctx context.Context, clientID, tenant, refreshToken string) (MicrosoftTokenResult, error) {
	clientID = microsoftClientIDOrDefault(clientID)
	tenant = microsoftTenantOrDefault(tenant)
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return MicrosoftTokenResult{}, errCode("microsoft_refresh_token_missing", "缺少 refresh_token，请重新登录 Microsoft 账号", false)
	}
	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token", microsoftLoginBaseURL, url.PathEscape(tenant))

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("scope", MicrosoftOAuthScope)

	body, status, err := c.postForm(ctx, endpoint, form)
	if err != nil {
		return MicrosoftTokenResult{}, err
	}
	if status < 200 || status >= 300 {
		if code := microsoftOAuthErrorCode(body); code == "invalid_grant" {
			return MicrosoftTokenResult{}, errCode("microsoft_refresh_token_invalid", "Microsoft 授权已失效，请重新登录账号", false)
		}
		return MicrosoftTokenResult{}, microsoftOAuthError(status, body, "续期 Microsoft token 失败")
	}
	return parseMicrosoftToken(body)
}

// FetchProfile 读取 /me，用于确认授权身份与账号邮箱。
func (c *MicrosoftAuthClient) FetchProfile(ctx context.Context, accessToken string) (MicrosoftProfile, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return MicrosoftProfile{}, errCode("microsoft_access_token_missing", "缺少 access_token", false)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, microsoftGraphBaseURL+"/me", nil)
	if err != nil {
		return MicrosoftProfile{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return MicrosoftProfile{}, errCode("microsoft_graph_unreachable", "请求 Microsoft Graph 失败："+err.Error(), true)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return MicrosoftProfile{}, microsoftGraphError(resp.StatusCode, body, "读取 Microsoft 账号信息失败")
	}
	var profile MicrosoftProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		return MicrosoftProfile{}, errCode("microsoft_profile_parse_failed", "解析 Microsoft 账号信息失败："+err.Error(), true)
	}
	if profile.Email() == "" {
		return MicrosoftProfile{}, errCode("microsoft_profile_email_missing", "Microsoft 账号未返回邮箱地址", true)
	}
	return profile, nil
}

func (c *MicrosoftAuthClient) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, errCode("microsoft_login_unreachable", "请求 Microsoft 登录服务失败："+err.Error(), true)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, errCode("microsoft_login_read_failed", "读取 Microsoft 登录响应失败："+err.Error(), true)
	}
	return body, resp.StatusCode, nil
}

func parseMicrosoftToken(body []byte) (MicrosoftTokenResult, error) {
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return MicrosoftTokenResult{}, errCode("microsoft_token_parse_failed", "解析 Microsoft token 响应失败："+err.Error(), true)
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return MicrosoftTokenResult{}, errCode("microsoft_token_empty", "Microsoft 未返回 access_token", true)
	}
	expiresIn := out.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return MicrosoftTokenResult{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		// 提前 5 分钟视为过期，避免边界时刻带着即将失效的 token 发请求。
		ExpiresAt: time.Now().Add(time.Duration(expiresIn)*time.Second - 5*time.Minute),
		Scope:     out.Scope,
		TokenType: firstNonEmpty(out.TokenType, "Bearer"),
	}, nil
}

func microsoftOAuthErrorCode(body []byte) string {
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(out.Error))
}

func microsoftOAuthError(status int, body []byte, prefix string) error {
	code := microsoftOAuthErrorCode(body)
	detail := microsoftErrorDescription(body)
	if detail == "" {
		detail = trimForError(body)
	}
	message := fmt.Sprintf("%s（HTTP %d）", prefix, status)
	if code != "" {
		message += "：" + code
	}
	if detail != "" {
		message += "；" + detail
	}
	return errCode("microsoft_oauth_failed", message, status >= 500)
}

func microsoftGraphError(status int, body []byte, prefix string) error {
	detail := microsoftErrorDescription(body)
	if detail == "" {
		detail = trimForError(body)
	}
	message := fmt.Sprintf("%s（HTTP %d）", prefix, status)
	if detail != "" {
		message += "：" + detail
	}
	switch status {
	case http.StatusUnauthorized:
		return errCode("microsoft_token_rejected", "Microsoft 授权已被拒绝，请重新登录账号；"+message, false)
	case http.StatusForbidden:
		return errCode("microsoft_permission_denied", "当前授权缺少读取邮件的权限，请重新登录并同意 Mail.Read；"+message, false)
	case http.StatusTooManyRequests:
		return errCode("microsoft_rate_limited", "Microsoft Graph 已限流，请稍后再试；"+message, true)
	}
	return errCode("microsoft_graph_failed", message, status >= 500)
}

func microsoftErrorDescription(body []byte) string {
	var out struct {
		Error            json.RawMessage `json:"error"`
		ErrorDescription string          `json:"error_description"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	if strings.TrimSpace(out.ErrorDescription) != "" {
		return truncateMessage(out.ErrorDescription)
	}
	if len(out.Error) == 0 {
		return ""
	}
	var nested struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(out.Error, &nested); err != nil {
		return ""
	}
	return truncateMessage(firstNonEmpty(nested.Message, nested.Code))
}

func truncateMessage(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= 200 {
		return value
	}
	return string(runes[:200]) + "…"
}
