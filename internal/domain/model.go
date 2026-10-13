package domain

import (
	"encoding/json"
	"strings"
	"time"
)

const SchemaVersion = 5

// Provider 标识邮箱账号所属的服务商。空值一律按 iCloud 处理，保证旧数据与旧调用点行为不变。
const (
	ProviderICloud    = "icloud"
	ProviderMicrosoft = "microsoft"
	ProviderGoogle    = "google"
	// ProviderExternal 表示没有母号、没有登录密码的裸邮箱：
	// 只有一个第三方取码接口，靠那个 URL 里的令牌取信。
	ProviderExternal = "external"
	// ProviderIMAP 表示用标准 IMAP + 客户端授权码收信的邮箱（QQ、163、自建等）。
	// 这类邮箱同样没有 OAuth 母号，凭主机、端口、账号和授权码直连收件箱；
	// 之所以不按厂商各建一个 provider，是因为它们走的是同一条协议，
	// 差异只在服务器地址，而地址是每个邮箱自己的字段。
	ProviderIMAP = "imap"
)

const (
	StatusActive    = "active"
	StatusAvailable = "available"
	StatusReserved  = "reserved"
	StatusUsed      = "used"
	StatusFailed    = "failed"
	StatusDisabled  = "disabled"

	MailboxLeaseClaimed   = "claimed"
	MailboxLeaseCommitted = "committed"
	MailboxLeaseReleased  = "released"
	MailboxLeaseExpired   = "expired"

	ICloudStatusActive       = "active"
	ICloudStatusNeedLogin    = "need_login"
	ICloudStatusNeed2FA      = "need_2fa"
	ICloudStatusNoICloudPlus = "no_icloud_plus"
	ICloudStatusRateLimited  = "rate_limited"
	ICloudStatusPartial      = "partial"
	ICloudStatusFailed       = "failed"

	LoginStateICloudWeb    = "icloud_web"
	LoginStateAppleAccount = "apple_account"
	LoginStateICloudIMAP   = "icloud_imap"

	// Microsoft 账号状态。沿用 iCloud 状态文案，前端可以复用同一套颜色映射。
	MicrosoftStatusActive      = "active"
	MicrosoftStatusNeedLogin   = "need_login"
	MicrosoftStatusNeedConsent = "need_consent"
	MicrosoftStatusExpired     = "expired"
	MicrosoftStatusRateLimited = "rate_limited"
	MicrosoftStatusFailed      = "failed"

	// Google 账号状态。同样沿用 iCloud 文案，前端复用同一套颜色映射。
	GoogleStatusActive      = "active"
	GoogleStatusNeedLogin   = "need_login"
	GoogleStatusNeedConsent = "need_consent"
	GoogleStatusExpired     = "expired"
	GoogleStatusRateLimited = "rate_limited"
	GoogleStatusFailed      = "failed"
)

type State struct {
	SchemaVersion       int                `json:"schema_version"`
	NextID              int                `json:"next_id"`
	Admin               *Admin             `json:"admin,omitempty"`
	Sessions            []WebSession       `json:"sessions,omitempty"`
	AppleAccounts       []AppleAccount     `json:"apple_accounts,omitempty"`
	Mailboxes           []Mailbox          `json:"mailboxes,omitempty"`
	MailboxLeases       []MailboxLease     `json:"mailbox_leases,omitempty"`
	Messages            []Message          `json:"messages,omitempty"`
	Events              []Event            `json:"events,omitempty"`
	Settings            Settings           `json:"settings"`
	CreateSettings      CreateSettings     `json:"create_settings"`
	ICloudSessions      []ICloudSession    `json:"icloud_sessions,omitempty"`
	MicrosoftSessions   []MicrosoftSession `json:"microsoft_sessions,omitempty"`
	GoogleSessions      []GoogleSession    `json:"google_sessions,omitempty"`
	LegacyICloudSession json.RawMessage    `json:"legacy_icloud_session,omitempty"`
	LegacyICloudStates  []json.RawMessage  `json:"legacy_icloud_sessions,omitempty"`
	CreatedAt           time.Time          `json:"created_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
}

type Admin struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	LastLoginAt  time.Time `json:"last_login_at,omitempty"`
}

type WebSession struct {
	TokenHash  string    `json:"token_hash"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type AppleAccount struct {
	ID           string `json:"id"`
	OwnerID      string `json:"owner_id,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Label        string `json:"label"`
	AppleID      string `json:"apple_id"`
	Password     string `json:"password,omitempty"`
	Status       string `json:"status"`
	ICloudStatus string `json:"icloud_status"`
	Note         string `json:"note"`
	// 以下四项只对 provider=imap 的账号有意义（QQ、163、自建邮箱）。
	// Preset 决定服务器地址和界面归属，但不改变同步路径——它们共用一条 IMAP 通路，
	// 所以页面是「按 preset 过滤同一个组件」，而不是每个服务商一套代码。
	// Password 在 IMAP 账号上就是客户端授权码，它已经在 apple_accounts 的加密清单里，
	// 因此不需要新增加密字段。
	Preset       string    `json:"preset,omitempty"`
	IMAPHost     string    `json:"imap_host,omitempty"`
	IMAPPort     int       `json:"imap_port,omitempty"`
	IMAPUsername string    `json:"imap_username,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ProviderOrDefault 返回账号的服务商标识，空值按 iCloud 处理。
func (a AppleAccount) ProviderOrDefault() string {
	return NormalizeProvider(a.Provider)
}

// NormalizeProvider 把空值或未知值归一为 iCloud，避免旧数据在分支判断中落到未定义路径。
func NormalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderMicrosoft:
		return ProviderMicrosoft
	case ProviderGoogle:
		return ProviderGoogle
	case ProviderExternal:
		return ProviderExternal
	case ProviderIMAP:
		return ProviderIMAP
	}
	return ProviderICloud
}

type Mailbox struct {
	ID             string `json:"id"`
	OwnerID        string `json:"owner_id,omitempty"`
	Provider       string `json:"provider,omitempty"`
	AccountID      string `json:"account_id,omitempty"`
	AnonymousID    string `json:"anonymous_id,omitempty"`
	RemoteOrigin   string `json:"remote_origin,omitempty"`
	Label          string `json:"label"`
	Email          string `json:"email"`
	ForwardToEmail string `json:"forward_to_email,omitempty"`
	APIToken       string `json:"api_token,omitempty"`
	APIActive      bool   `json:"api_active"`
	ICloudActive   bool   `json:"icloud_active"`
	// CodeAPIURL 是外部取码接口的完整地址，只对 ProviderExternal 有意义。
	// 它内嵌了访问令牌，所以落库时必须和 api_token 一样加密。
	CodeAPIURL string `json:"code_api_url,omitempty"`
	// 以下四项是 IMAP 取码邮箱的连接信息，只对 ProviderIMAP 有意义。
	// IMAPSecret 是客户端授权码（QQ / 163）或 App 专用密码，等同于密码，落库必须加密。
	IMAPHost     string `json:"imap_host,omitempty"`
	IMAPPort     int    `json:"imap_port,omitempty"`
	IMAPUsername string `json:"imap_username,omitempty"`
	IMAPSecret   string `json:"imap_secret,omitempty"`
	// PoolHidden 为 true 时该邮箱不出现在邮箱池页面，只在其母号页面里可见。
	// 刻意用「隐藏」而不是「在池里」：旧数据没有这个字段，缺省值必须等于可见，
	// 反过来写会让升级后所有已有邮箱凭空从邮箱池消失。
	PoolHidden        bool      `json:"pool_hidden,omitempty"`
	ReceiveCount      int       `json:"receive_count"`
	Status            string    `json:"status"`
	Note              string    `json:"note"`
	ActiveLeaseID     string    `json:"active_lease_id,omitempty"`
	LastSyncAt        time.Time `json:"last_sync_at,omitempty"`
	LastSyncUID       string    `json:"last_sync_uid,omitempty"`
	LastCodeMessageID string    `json:"last_code_message_id,omitempty"`
	LastCodeAt        time.Time `json:"last_code_at,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// ProviderOrDefault 返回邮箱所属服务商，空值按 iCloud 处理。
func (m Mailbox) ProviderOrDefault() string {
	return NormalizeProvider(m.Provider)
}

// MailboxLease 记录外部调用方对邮箱的临时占用，只有提交租约才会把邮箱标记为已使用。
type MailboxLease struct {
	ID          string    `json:"id"`
	MailboxID   string    `json:"mailbox_id"`
	Email       string    `json:"email"`
	Project     string    `json:"project"`
	Purpose     string    `json:"purpose,omitempty"`
	RequestID   string    `json:"request_id,omitempty"`
	State       string    `json:"state"`
	Note        string    `json:"note,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	CommittedAt time.Time `json:"committed_at,omitempty"`
	ReleasedAt  time.Time `json:"released_at,omitempty"`
	ExpiredAt   time.Time `json:"expired_at,omitempty"`
}

type Message struct {
	ID          string    `json:"id"`
	OwnerID     string    `json:"owner_id,omitempty"`
	MailboxID   string    `json:"mailbox_id"`
	RemoteID    string    `json:"remote_id,omitempty"`
	RemoteIDs   []string  `json:"remote_ids,omitempty"`
	CanonicalID string    `json:"canonical_id,omitempty"`
	Source      string    `json:"source,omitempty"`
	Subject     string    `json:"subject"`
	From        string    `json:"from"`
	Body        string    `json:"body"`
	HTMLBody    string    `json:"html_body,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
	ReceivedAt  time.Time `json:"received_at"`
	CreatedAt   time.Time `json:"created_at"`
}

type RemoteMailbox struct {
	AnonymousID    string `json:"anonymous_id,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Email          string `json:"email"`
	ForwardToEmail string `json:"forward_to_email,omitempty"`
	Label          string `json:"label,omitempty"`
	Note           string `json:"note,omitempty"`
	IsActive       bool   `json:"is_active"`
	Origin         string `json:"origin,omitempty"`
	// CodeAPIURL 只在导入外部取码邮箱时给出，落库前会加密。
	CodeAPIURL string `json:"code_api_url,omitempty"`
	// IMAP 连接信息只在导入 IMAP 取码邮箱时给出，其中 IMAPSecret 落库前会加密。
	IMAPHost     string `json:"imap_host,omitempty"`
	IMAPPort     int    `json:"imap_port,omitempty"`
	IMAPUsername string `json:"imap_username,omitempty"`
	IMAPSecret   string `json:"imap_secret,omitempty"`
	// PoolHidden 让新导入的地址默认只挂在母号下，不进邮箱池。
	PoolHidden bool `json:"pool_hidden,omitempty"`
}

type Event struct {
	ID        string    `json:"id"`
	Level     string    `json:"level"`
	Category  string    `json:"category"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

type Settings struct {
	EnableMailWatcher              bool   `json:"enable_mail_watcher"`
	EnableAppleKeepAlive           bool   `json:"enable_apple_keep_alive"`
	EnablePublicMailboxAPI         bool   `json:"enable_public_mailbox_api"`
	EnablePublicCodePage           bool   `json:"enable_public_code_page"`
	EnableWebCodeSync              bool   `json:"enable_web_code_sync"`
	EnableWebManualMailSync        bool   `json:"enable_web_manual_mail_sync"`
	EnableWebBackgroundMail        bool   `json:"enable_web_background_mail"`
	EnableWebRemoteMailCleanup     bool   `json:"enable_web_remote_mail_cleanup"`
	PublicAPIKey                   string `json:"public_api_key,omitempty"`
	AppleAccountModuleReady        bool   `json:"apple_account_module_ready"`
	ServerChanSendKey              string `json:"server_chan_send_key,omitempty"`
	ServerChanHideIP               bool   `json:"server_chan_hide_ip"`
	NotifyAdminLogin               bool   `json:"notify_admin_login"`
	NotifyAccountLoginStateOffline bool   `json:"notify_account_login_state_offline"`

	// Microsoft 模块。client_id / tenant 留空时回退到 config.json 中的值，
	// 这样既能在界面上直接改，也保留配置文件作为部署默认值。
	EnableMicrosoftMailWatcher bool   `json:"enable_microsoft_mail_watcher"`
	MicrosoftClientID          string `json:"microsoft_client_id,omitempty"`
	MicrosoftTenant            string `json:"microsoft_tenant,omitempty"`

	// Google 模块。client_id 留空时回退到 config.json 中的值。
	// Google 没有可以内置的公共客户端，必须由使用者自己在 Google Cloud Console 建一个
	// 「桌面应用」类型的 OAuth 客户端；这一点和 Microsoft 不同，界面上要写清楚。
	EnableGoogleMailWatcher bool   `json:"enable_google_mail_watcher"`
	GoogleClientID          string `json:"google_client_id,omitempty"`
	GoogleClientSecret      string `json:"google_client_secret,omitempty"`
	GoogleRedirectBase      string `json:"google_redirect_base,omitempty"`
	// GoogleProxy 留空时自动读取系统代理。
	// Go 不会自己读 Windows 的系统代理设置，而 Google 在部分网络里无法直连，
	// 因此这个字段是让 Gmail 能跑起来的关键，不是可选项。
	GoogleProxy string `json:"google_proxy,omitempty"`
}

type CreateSettings struct {
	OwnerID                            string    `json:"owner_id,omitempty"`
	Mode                               string    `json:"mode,omitempty"`
	Label                              string    `json:"label,omitempty"`
	Note                               string    `json:"note,omitempty"`
	AccountIDs                         []string  `json:"account_ids,omitempty"`
	CreateChannel                      string    `json:"create_channel,omitempty"`
	SchedulerCreateChannel             string    `json:"scheduler_create_channel,omitempty"`
	AppleAccountTwoFactorMethod        string    `json:"apple_account_two_factor_method,omitempty"`
	ICloudWebTwoFactorMethod           string    `json:"icloud_web_two_factor_method,omitempty"`
	SchedulerIntervalMinMinutes        int       `json:"scheduler_interval_min_minutes,omitempty"`
	SchedulerIntervalMaxMinutes        int       `json:"scheduler_interval_max_minutes,omitempty"`
	SchedulerAccountIntervalMinSeconds int       `json:"scheduler_account_interval_min_seconds,omitempty"`
	SchedulerAccountIntervalMaxSeconds int       `json:"scheduler_account_interval_max_seconds,omitempty"`
	UpdatedAt                          time.Time `json:"updated_at,omitempty"`
}

type ICloudSession struct {
	OwnerID            string          `json:"owner_id,omitempty"`
	AccountID          string          `json:"account_id,omitempty"`
	SavedAt            time.Time       `json:"saved_at"`
	AppleID            string          `json:"apple_id,omitempty"`
	DSID               string          `json:"dsid"`
	ClientID           string          `json:"client_id"`
	ClientBuildNumber  string          `json:"client_build_number"`
	MasteringNumber    string          `json:"client_mastering_number"`
	PremiumMailBaseURL string          `json:"premium_mail_base_url"`
	MailGatewayBaseURL string          `json:"mail_gateway_base_url,omitempty"`
	MailBaseURL        string          `json:"mail_base_url,omitempty"`
	Host               string          `json:"host"`
	IsICloudPlus       bool            `json:"is_icloud_plus"`
	CanCreateHME       bool            `json:"can_create_hme"`
	Cookies            []SessionCookie `json:"cookies,omitempty"`
	LoginStates        []LoginState    `json:"login_states,omitempty"`
	Note               string          `json:"note,omitempty"`
	LastCheckedAt      time.Time       `json:"last_checked_at,omitempty"`
	LastCheckOK        bool            `json:"last_check_ok,omitempty"`
	LastStatusMessage  string          `json:"last_status_message,omitempty"`
}

type LoginState struct {
	Kind              string          `json:"kind"`
	Host              string          `json:"host,omitempty"`
	Origin            string          `json:"origin,omitempty"`
	SavedAt           time.Time       `json:"saved_at,omitempty"`
	Cookies           []SessionCookie `json:"cookies,omitempty"`
	Scnt              string          `json:"scnt,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	APIKey            string          `json:"api_key,omitempty"`
	DataAccessToken   string          `json:"data_access_token,omitempty"`
	UserAgent         string          `json:"user_agent,omitempty"`
	Note              string          `json:"note,omitempty"`
	IMAPEmail         string          `json:"imap_email,omitempty"`
	IMAPUsername      string          `json:"imap_username,omitempty"`
	IMAPHost          string          `json:"imap_host,omitempty"`
	IMAPPort          int             `json:"imap_port,omitempty"`
	IMAPAppPassword   string          `json:"imap_app_password,omitempty"`
	IMAPLastSyncAt    time.Time       `json:"imap_last_sync_at,omitempty"`
	IMAPLastSyncUID   string          `json:"imap_last_sync_uid,omitempty"`
	IMAPUIDValidity   string          `json:"imap_uid_validity,omitempty"`
	ManageExpiresAt   time.Time       `json:"manage_expires_at,omitempty"`
	LastCheckedAt     time.Time       `json:"last_checked_at,omitempty"`
	LastCheckOK       bool            `json:"last_check_ok,omitempty"`
	LastStatusMessage string          `json:"last_status_message,omitempty"`
}

type SessionCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"http_only,omitempty"`
	SameSite string  `json:"same_site,omitempty"`
}

// MicrosoftSession 保存一个 Microsoft 账号的 OAuth2 授权结果。
// AccessToken / RefreshToken 由 store 层按字段名加密落库，与 iCloud 会话保持一致的处理方式。
type MicrosoftSession struct {
	OwnerID           string    `json:"owner_id,omitempty"`
	AccountID         string    `json:"account_id,omitempty"`
	SavedAt           time.Time `json:"saved_at"`
	Email             string    `json:"email,omitempty"`
	DisplayName       string    `json:"display_name,omitempty"`
	Tenant            string    `json:"tenant,omitempty"`
	ClientID          string    `json:"client_id,omitempty"`
	AccessToken       string    `json:"access_token,omitempty"`
	RefreshToken      string    `json:"refresh_token,omitempty"`
	TokenExpiresAt    time.Time `json:"token_expires_at,omitempty"`
	Scope             string    `json:"scope,omitempty"`
	Status            string    `json:"status,omitempty"`
	LastCheckedAt     time.Time `json:"last_checked_at,omitempty"`
	LastCheckOK       bool      `json:"last_check_ok,omitempty"`
	LastStatusMessage string    `json:"last_status_message,omitempty"`
	Note              string    `json:"note,omitempty"`
}

// MicrosoftDeviceLogin 是一次尚未完成的设备码登录流程，只保存在内存中。
type MicrosoftDeviceLogin struct {
	AccountID       string    `json:"account_id"`
	DeviceCode      string    `json:"device_code"`
	UserCode        string    `json:"user_code"`
	VerificationURI string    `json:"verification_uri"`
	Message         string    `json:"message,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
	IntervalSeconds int       `json:"interval_seconds"`
	ClientID        string    `json:"client_id"`
	Tenant          string    `json:"tenant"`
}

// MicrosoftMailMessage 是从 Microsoft Graph 取回的单封邮件。
type MicrosoftMailMessage struct {
	ID           string    `json:"id"`
	Subject      string    `json:"subject"`
	From         string    `json:"from"`
	ToRecipients []string  `json:"to_recipients,omitempty"`
	Body         string    `json:"body"`
	HTMLBody     string    `json:"html_body,omitempty"`
	ContentType  string    `json:"content_type,omitempty"`
	ReceivedAt   time.Time `json:"received_at"`
}

// GoogleSession 保存一个 Google 账号的 OAuth2 授权结果。
// AccessToken / RefreshToken 由 store 层按字段名加密落库，与 iCloud / Microsoft 会话一致。
type GoogleSession struct {
	OwnerID           string    `json:"owner_id,omitempty"`
	AccountID         string    `json:"account_id,omitempty"`
	SavedAt           time.Time `json:"saved_at"`
	Email             string    `json:"email,omitempty"`
	DisplayName       string    `json:"display_name,omitempty"`
	ClientID          string    `json:"client_id,omitempty"`
	AccessToken       string    `json:"access_token,omitempty"`
	RefreshToken      string    `json:"refresh_token,omitempty"`
	TokenExpiresAt    time.Time `json:"token_expires_at,omitempty"`
	Scope             string    `json:"scope,omitempty"`
	Status            string    `json:"status,omitempty"`
	LastCheckedAt     time.Time `json:"last_checked_at,omitempty"`
	LastCheckOK       bool      `json:"last_check_ok,omitempty"`
	LastStatusMessage string    `json:"last_status_message,omitempty"`
	Note              string    `json:"note,omitempty"`
}

// GoogleAuthFlow 是一次尚未完成的 loopback 授权流程，只保存在内存中。
//
// Google 的设备码流程只对 "TVs and Limited Input devices" 类型的客户端开放，
// 用桌面应用类型去调会直接返回 invalid_client: Invalid client type。
// 因此这里走「本地回调 + PKCE」：浏览器授权后由 Google 回调到本服务自身。
type GoogleAuthFlow struct {
	State        string    `json:"state"`
	CodeVerifier string    `json:"code_verifier"`
	RedirectURI  string    `json:"redirect_uri"`
	ClientID     string    `json:"client_id"`
	AuthURL      string    `json:"auth_url"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	AccountID    string    `json:"account_id,omitempty"`
}

// GoogleMailMessage 是从 Gmail API 取回的单封邮件。
// DeliveredTo 来自 Delivered-To 头，是 + 别名路由最可靠的依据。
type GoogleMailMessage struct {
	ID           string    `json:"id"`
	Subject      string    `json:"subject"`
	From         string    `json:"from"`
	ToRecipients []string  `json:"to_recipients,omitempty"`
	DeliveredTo  []string  `json:"delivered_to,omitempty"`
	Body         string    `json:"body"`
	HTMLBody     string    `json:"html_body,omitempty"`
	ContentType  string    `json:"content_type,omitempty"`
	ReceivedAt   time.Time `json:"received_at"`
}

type Dashboard struct {
	AppleAccountCount     int     `json:"apple_account_count"`
	MicrosoftAccountCount int     `json:"microsoft_account_count"`
	GoogleAccountCount    int     `json:"google_account_count"`
	ActiveAccountCount    int     `json:"active_account_count"`
	MailboxCount          int     `json:"mailbox_count"`
	AvailableCount        int     `json:"available_count"`
	MessageCount          int     `json:"message_count"`
	Events                []Event `json:"events"`
}

// Page 是任意列表接口的一页。
//
// 四个账号页和邮箱池共用这一个形状，前端因此只需要一套分页控件；
// 分页字段也只在一处定义，不会两边慢慢分叉。
type Page[T any] struct {
	Items      []T `json:"items"`
	Page       int `json:"page"`
	PageSize   int `json:"page_size"`
	Total      int `json:"total"`
	TotalPages int `json:"total_pages"`
}

// MailboxPage 是邮箱池的分页结果，与其它列表同形。
type MailboxPage = Page[Mailbox]

type Task struct {
	ID                       string     `json:"id"`
	Name                     string     `json:"name"`
	Description              string     `json:"description"`
	Status                   string     `json:"status"`
	Progress                 int        `json:"progress"`
	Module                   string     `json:"module"`
	NextRunAt                *time.Time `json:"next_run_at,omitempty"`
	ScheduledIntervalSeconds int        `json:"scheduled_interval_seconds,omitempty"`
	JitterPercent            int        `json:"jitter_percent,omitempty"`
}

func DefaultSettings() Settings {
	return Settings{
		EnableMailWatcher:          false,
		EnableAppleKeepAlive:       false,
		EnablePublicMailboxAPI:     false,
		EnablePublicCodePage:       false,
		EnableWebCodeSync:          false,
		EnableWebManualMailSync:    true,
		EnableWebBackgroundMail:    true,
		EnableWebRemoteMailCleanup: true,
		AppleAccountModuleReady:    true,
		ServerChanHideIP:           true,
		EnableMicrosoftMailWatcher: true,
		EnableGoogleMailWatcher:    true,
	}
}

func DefaultCreateSettings() CreateSettings {
	return CreateSettings{
		Mode:                               "once",
		Label:                              "x",
		CreateChannel:                      "auto",
		SchedulerCreateChannel:             "auto",
		AppleAccountTwoFactorMethod:        "trusted_device",
		ICloudWebTwoFactorMethod:           "trusted_device",
		SchedulerIntervalMinMinutes:        60,
		SchedulerIntervalMaxMinutes:        60,
		SchedulerAccountIntervalMinSeconds: 5,
		SchedulerAccountIntervalMaxSeconds: 5,
	}
}
