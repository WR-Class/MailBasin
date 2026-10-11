package protocol

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

const (
	// GoogleAuthEndpoint 是浏览器授权页地址。
	GoogleAuthEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	// GoogleTokenEndpoint 同时用于换码和续期。
	GoogleTokenEndpoint = "https://oauth2.googleapis.com/token"
	// GmailBaseURL 是 Gmail API 的用户级前缀。
	GmailBaseURL = "https://gmail.googleapis.com/gmail/v1/users/me"
	// GoogleOAuthScope 只申请读取邮件。这是"受限范围"，第三方生产使用需要年度安全评估；
	// 个人自用可以走验证例外，但同意屏幕停在"测试"状态时 refresh token 只有 7 天。
	GoogleOAuthScope = "https://www.googleapis.com/auth/gmail.readonly"
)

// GoogleAuthClient 负责 Google 的 OAuth2 交互与账号信息读取。
//
// 这里走「本地回调 + PKCE」而不是设备码流程：Google 的设备码流程只对
// "TVs and Limited Input devices" 类型的客户端开放，用桌面应用类型去调
// 会直接返回 HTTP 401 invalid_client: Invalid client type——错误描述里点名的是
// 客户端**类型**，不是 ID，很容易被误读成"ID 写错了"。
//
// 客户端是轻量的，每次调用现建即可，这样设置页改了代理或客户端 ID 后无需重启。
type GoogleAuthClient struct {
	httpClient *http.Client
	proxy      string
}

// NewGoogleAuthClient 按当前生效的代理配置创建一个客户端。
func NewGoogleAuthClient(proxy string) *GoogleAuthClient {
	return &GoogleAuthClient{
		httpClient: NewHTTPClient(proxy, 30*time.Second),
		proxy:      ResolveProxy(proxy),
	}
}

// GoogleTokenResult 是一次换码或续期的结果。
//
// Google 续期时不会返回新的 refresh_token，因此调用方必须保留原有的那个；
// 这一点与 Microsoft 不同，那边每次续期都会轮换。
type GoogleTokenResult struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scope        string    `json:"scope"`
	TokenType    string    `json:"token_type"`
}

// GoogleProfile 是 Gmail 的 /profile 响应。
type GoogleProfile struct {
	EmailAddress  string `json:"emailAddress"`
	MessagesTotal int64  `json:"messagesTotal"`
	ThreadsTotal  int64  `json:"threadsTotal"`
	HistoryID     string `json:"historyId"`
}

// NewGoogleAuthFlow 生成一次授权流程：随机的 state、PKCE verifier，以及拼好的授权链接。
//
// state 用来拒绝伪造的回调，code_verifier 用来证明换取 token 的就是发起授权的那一方。
func NewGoogleAuthFlow(clientID, redirectURI string) (domain.GoogleAuthFlow, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return domain.GoogleAuthFlow{}, errCode("google_client_id_missing",
			"缺少 Google 客户端 ID，请先到「系统设置 → Google 账号」填写", false)
	}
	redirectURI = strings.TrimSpace(redirectURI)
	if redirectURI == "" {
		return domain.GoogleAuthFlow{}, errCode("google_redirect_uri_missing", "缺少回调地址", false)
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return domain.GoogleAuthFlow{}, errCode("google_state_generate_failed", "生成 state 失败："+err.Error(), true)
	}
	verifier, err := randomURLSafe(48)
	if err != nil {
		return domain.GoogleAuthFlow{}, errCode("google_verifier_generate_failed", "生成 PKCE verifier 失败："+err.Error(), true)
	}
	now := time.Now()
	return domain.GoogleAuthFlow{
		State:        state,
		CodeVerifier: verifier,
		RedirectURI:  redirectURI,
		ClientID:     clientID,
		AuthURL:      buildGoogleAuthURL(clientID, redirectURI, state, codeChallenge(verifier)),
		CreatedAt:    now,
		ExpiresAt:    now.Add(15 * time.Minute),
	}, nil
}

// buildGoogleAuthURL 拼授权链接。
//
// access_type=offline 是拿到 refresh token 的前提；prompt=consent 保证重复授权时
// Google 仍会回传 refresh token（否则第二次授权不会再给）。
func buildGoogleAuthURL(clientID, redirectURI, state, challenge string) string {
	query := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {GoogleOAuthScope},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
	}
	return GoogleAuthEndpoint + "?" + query.Encode()
}

// codeChallenge 是 PKCE 的 S256 变换：verifier 的 SHA-256 再做 URL 安全 base64（无填充）。
func codeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomURLSafe(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// ExchangeCode 用回调拿到的授权码换取 token。
func (c *GoogleAuthClient) ExchangeCode(ctx context.Context, clientID, clientSecret, code, verifier, redirectURI string) (GoogleTokenResult, error) {
	clientID = strings.TrimSpace(clientID)
	code = strings.TrimSpace(code)
	if clientID == "" || code == "" {
		return GoogleTokenResult{}, errCode("google_exchange_missing", "换取 token 缺少必要参数", false)
	}
	form := url.Values{
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	// Google 的文档把 client_secret 标为 Optional，但实测换码时必须提供，
	// 桌面应用类型也一样——不带就回 invalid_request: client_secret is missing。
	// 这里填了就带上；没填时由 googleOAuthError 给出可操作的提示。
	if secret := strings.TrimSpace(clientSecret); secret != "" {
		form.Set("client_secret", secret)
	}

	body, status, err := c.postForm(ctx, GoogleTokenEndpoint, form)
	if err != nil {
		return GoogleTokenResult{}, err
	}
	if status < 200 || status >= 300 {
		return GoogleTokenResult{}, googleOAuthError(status, body, "换取 Google token 失败")
	}
	return parseGoogleToken(body)
}

// RefreshAccessToken 用 refresh_token 换一份新的 access_token。
//
// Google 续期不会返回新的 refresh_token，因此调用方必须保留原有的那个。
// 这一点与 Microsoft 不同：那边每次续期都会轮换 refresh_token。
func (c *GoogleAuthClient) RefreshAccessToken(ctx context.Context, clientID, clientSecret, refreshToken string) (GoogleTokenResult, error) {
	clientID = strings.TrimSpace(clientID)
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return GoogleTokenResult{}, errCode("google_refresh_token_missing", "缺少 refresh_token，请重新登录 Google 账号", false)
	}
	form := url.Values{
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}
	if secret := strings.TrimSpace(clientSecret); secret != "" {
		form.Set("client_secret", secret)
	}

	body, status, err := c.postForm(ctx, GoogleTokenEndpoint, form)
	if err != nil {
		return GoogleTokenResult{}, err
	}
	if status < 200 || status >= 300 {
		// 续期失败最常见的原因是 refresh token 过期：同意屏幕停在"测试"状态时只有 7 天。
		// 这一点使用者从界面上看不出来，所以在错误里点明。
		return GoogleTokenResult{}, googleOAuthError(status, body, "刷新 Google token 失败")
	}
	return parseGoogleToken(body)
}

// FetchProfile 读取 Gmail 账号信息，用于确认授权身份与邮箱地址。
func (c *GoogleAuthClient) FetchProfile(ctx context.Context, accessToken string) (GoogleProfile, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return GoogleProfile{}, errCode("google_access_token_missing", "缺少 access_token", false)
	}
	body, status, err := c.getJSON(ctx, GmailBaseURL+"/profile", accessToken)
	if err != nil {
		return GoogleProfile{}, err
	}
	if status < 200 || status >= 300 {
		return GoogleProfile{}, gmailError(status, body, "读取 Google 账号信息失败")
	}
	var profile GoogleProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		return GoogleProfile{}, errCode("google_profile_parse_failed", "解析 Google 账号信息失败："+err.Error(), true)
	}
	if strings.TrimSpace(profile.EmailAddress) == "" {
		return GoogleProfile{}, errCode("google_profile_email_missing", "Google 账号未返回邮箱地址", true)
	}
	return profile, nil
}

func (c *GoogleAuthClient) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, errCode("google_request_build_failed", "构造 Google 请求失败："+err.Error(), false)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	return c.do(request)
}

func (c *GoogleAuthClient) getJSON(ctx context.Context, endpoint, accessToken string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, errCode("google_request_build_failed", "构造 Google 请求失败："+err.Error(), false)
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	request.Header.Set("Accept", "application/json")
	return c.do(request)
}

func (c *GoogleAuthClient) do(request *http.Request) ([]byte, int, error) {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, 0, googleUnreachableError(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, response.StatusCode, errCode("google_read_failed", "读取 Google 响应失败："+err.Error(), true)
	}
	return body, response.StatusCode, nil
}

// googleUnreachableError 把网络层错误翻译成可操作的提示。
//
// Go 不读 Windows 的系统代理设置：http.DefaultTransport 只认 HTTP_PROXY / HTTPS_PROXY
// 环境变量，从不读取 WinINET 注册表。所以"连不上 Google"最常见的原因就是没走代理。
func googleUnreachableError(err error) error {
	detail := "未知错误"
	if err != nil {
		detail = err.Error()
	}
	return errCode("google_unreachable",
		"连接 Google 失败："+detail+
			"。最常见的原因是代理没有生效——Go 不会自动读取 Windows 的系统代理，"+
			"请到「系统设置 → Google 账号 → 代理地址」填写，例如 http://127.0.0.1:7897", true)
}

// parseGoogleToken 解析换码/续期的响应。
//
// expires_in 会减去 5 分钟提前量：否则过期边界上会发出一批必然 401 的请求，
// 而每一条都要走一次刷新，白白浪费配额。
func parseGoogleToken(body []byte) (GoogleTokenResult, error) {
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return GoogleTokenResult{}, errCode("google_token_parse_failed", "解析 Google 令牌失败："+err.Error(), true)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return GoogleTokenResult{}, errCode("google_token_missing", "Google 未返回 access_token", true)
	}
	const skew = 5 * time.Minute
	expiresIn := time.Duration(payload.ExpiresIn) * time.Second
	if expiresIn > skew {
		expiresIn -= skew
	}
	return GoogleTokenResult{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		ExpiresAt:    time.Now().Add(expiresIn),
		Scope:        payload.Scope,
		TokenType:    payload.TokenType,
	}, nil
}

// googleErrorCode 读 OAuth 错误码。Google 在 token 端点用扁平形状 {"error":"invalid_client"}，
// 而 Gmail API 用嵌套形状 {"error":{"code":401,...}}，两种都要认。
func googleErrorCode(body []byte) string {
	var flat struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &flat); err == nil && strings.TrimSpace(flat.Error) != "" {
		return strings.TrimSpace(flat.Error)
	}
	var nested struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &nested); err == nil {
		return strings.TrimSpace(nested.Error.Status)
	}
	return ""
}

// googleErrorDescription 读 OAuth 错误描述。
func googleErrorDescription(body []byte) string {
	var payload struct {
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		return strings.TrimSpace(payload.ErrorDescription)
	}
	return ""
}

// gmailErrorMessage 读 Gmail API 的嵌套错误消息。
func gmailErrorMessage(body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		return strings.TrimSpace(payload.Error.Message)
	}
	return ""
}

// googleOAuthError 把 OAuth 错误翻译成使用者能照着做的提示。
//
// 这里覆盖的是 loopback + PKCE 流程真实的几类失败。把最可能的原因直接写进错误里，
// 省得使用者拿着一个完全正确的 client id 反复怀疑自己。
func googleOAuthError(status int, body []byte, prefix string) error {
	code := googleErrorCode(body)
	detail := googleErrorDescription(body)
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
	switch code {
	case "invalid_client":
		// 设备码流程只对 "TVs and Limited Input devices" 开放，用桌面应用类型去调就会走到这里，
		// 而 Google 的错误描述点名的是客户端**类型**而不是 ID。
		message += "。请确认这个客户端 ID 的应用类型是「桌面应用」——" +
			"Google 的设备码流程只对「电视和受限输入设备」类型开放，" +
			"用桌面应用类型去调设备码接口必然走到这里"
	case "redirect_uri_mismatch":
		message += "。回调地址与客户端登记的不一致。桌面应用类型会自动放行 127.0.0.1 的任意端口，" +
			"出现这个错误通常说明客户端类型不是「桌面应用」；" +
			"若确实要用「Web 应用」类型，必须在凭据里把回调地址原样登记"
	case "invalid_grant":
		message += "。授权码已被使用过、已过期，或本次换码的回调地址与发起授权时用的不一致。" +
			"重新点一次「添加 Google 账号」即可；同一个授权码只能用一次。" +
			"如果这是续期失败，则说明 refresh token 已失效——" +
			"同意屏幕停在「测试」状态时它只有 7 天有效期"
	case "unauthorized_client":
		message += "。该客户端未被允许使用此授权方式，请确认已启用 Gmail API 且客户端类型是「桌面应用」"
	case "invalid_request":
		// Google 的文档把 client_secret 标为 Optional，但实测并非如此。
		// 用 gcloud SDK 那个确定属于桌面/installed 类型的客户端做对照实验，
		// 不带密钥时回的同样是这一条——所以这跟客户端类型无关，
		// 也不该建议使用者去重建客户端。
		if strings.Contains(strings.ToLower(detail), "client_secret") {
			message += "。到 Google Cloud Console 的凭据页把「客户端密钥」复制出来，" +
				"填到「系统设置 → Google 账号 → 客户端密钥」再试一次。" +
				"注意这一项与客户端类型无关：Google 文档把它标为 Optional，" +
				"但换码时实际必须提供，桌面应用类型也一样（已实测对照）"
		}
	}
	return errCode("google_oauth_failed", message, status >= 500)
}

// gmailError 把 Gmail API 的 HTTP 状态映射成同步层能识别的信号。
//
// 401 必须映射成可重试的"令牌失效"，同步层据此刷新一次再重试。
func gmailError(status int, body []byte, prefix string) error {
	detail := gmailErrorMessage(body)
	if detail == "" {
		detail = trimForError(body)
	}
	message := fmt.Sprintf("%s（HTTP %d）", prefix, status)
	if detail != "" {
		message += "：" + detail
	}
	switch status {
	case http.StatusUnauthorized:
		return errCode("google_token_rejected", "Google 授权已被拒绝，请重新登录账号；"+message, false)
	case http.StatusForbidden:
		return errCode("google_permission_denied",
			"当前授权缺少读取邮件的权限，请重新登录并同意 gmail.readonly；"+message, false)
	case http.StatusTooManyRequests:
		return errCode("google_rate_limited", "Gmail API 已限流，请稍后再试；"+message, true)
	}
	return errCode("google_api_failed", message, status >= 500)
}
