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

// googleTokenRefreshWindow 决定 access token 还剩多久就提前续期。
const googleTokenRefreshWindow = 2 * time.Minute

// GoogleAliasImportResult 是一次地址导入的结果统计。
type GoogleAliasImportResult struct {
	AccountID string   `json:"account_id"`
	Found     int      `json:"found"`
	Created   int      `json:"created"`
	Updated   int      `json:"updated"`
	Aliases   []string `json:"aliases,omitempty"`
	// Notice 说明 Google 侧的能力边界，前端直接展示，避免使用者以为漏导入了。
	Notice string `json:"notice,omitempty"`
}

// googleClientID / googleClientSecret / googleProxy 都按「界面设置优先、配置文件兜底」解析，
// 这样使用者改完设置不必重启服务。
func (s *Service) googleClientID() string {
	return firstNonEmpty(s.store.Settings().GoogleClientID, s.cfg.GoogleClientID)
}

func (s *Service) googleClientSecret() string {
	return firstNonEmpty(s.store.Settings().GoogleClientSecret, s.cfg.GoogleClientSecret)
}

func (s *Service) googleProxy() string {
	return firstNonEmpty(s.store.Settings().GoogleProxy, s.cfg.GoogleProxy)
}

// googleAuthClient / gmailClient 每次现构造。
// 代理地址可以在界面上随时改，缓存客户端会让改动直到重启才生效。
func (s *Service) googleAuthClient() *protocol.GoogleAuthClient {
	return protocol.NewGoogleAuthClient(s.googleProxy())
}

func (s *Service) gmailClient() *protocol.GmailClient {
	return protocol.NewGmailClient(s.googleProxy())
}

// GoogleSession 读取账号的 OAuth2 会话，不做续期。
func (s *Service) GoogleSession(accountID string) (domain.GoogleSession, bool) {
	return s.store.GoogleSessionByAccountID(strings.TrimSpace(accountID))
}

// GoogleAccessToken 返回一个当前可用的 access token；过期时用 refresh_token 续期。
//
// 与 Microsoft 的关键差别：Google 续期不返回新的 refresh_token，
// 因此这里只在返回值非空时才覆盖，否则会把唯一的那份凭据抹掉。
func (s *Service) GoogleAccessToken(ctx context.Context, accountID string) (string, domain.GoogleSession, error) {
	accountID = strings.TrimSpace(accountID)
	session, ok := s.store.GoogleSessionByAccountID(accountID)
	if !ok {
		return "", session, errors.New("对应 Google 账号登录态不存在，请先完成授权")
	}
	if token := strings.TrimSpace(session.AccessToken); token != "" && session.TokenExpiresAt.After(time.Now().Add(googleTokenRefreshWindow)) {
		return token, session, nil
	}
	if strings.TrimSpace(session.RefreshToken) == "" {
		s.markGoogleSessionFailure(accountID, domain.GoogleStatusNeedLogin, "缺少 refresh_token，请重新授权 Google 账号")
		return "", session, errors.New("缺少 refresh_token，请重新授权 Google 账号")
	}
	refreshed, err := s.googleAuthClient().RefreshAccessToken(ctx, session.ClientID, s.googleClientSecret(), session.RefreshToken)
	if err != nil {
		s.markGoogleSessionFailure(accountID, domain.GoogleStatusExpired, err.Error())
		return "", session, err
	}
	session.AccessToken = refreshed.AccessToken
	if strings.TrimSpace(refreshed.RefreshToken) != "" {
		session.RefreshToken = refreshed.RefreshToken
	}
	session.TokenExpiresAt = refreshed.ExpiresAt
	session.Scope = firstNonEmpty(refreshed.Scope, session.Scope)
	session.Status = domain.GoogleStatusActive
	session.LastStatusMessage = ""
	saved, err := s.store.SaveGoogleSession(session)
	if err != nil {
		return "", session, err
	}
	return saved.AccessToken, saved, nil
}

// markGoogleSessionFailure 把失败原因写回会话与账号状态，供前端展示。
func (s *Service) markGoogleSessionFailure(accountID, status, message string) {
	session, ok := s.store.GoogleSessionByAccountID(accountID)
	if !ok {
		return
	}
	session.LastCheckedAt = time.Now()
	session.LastCheckOK = false
	session.Status = status
	session.LastStatusMessage = message
	_, _ = s.store.SaveGoogleSession(session)
	_ = s.store.SaveGoogleAccountStatus(accountID, status, "")
}

// markGoogleSessionSuccess 记录一次成功调用。
func (s *Service) markGoogleSessionSuccess(accountID string) {
	session, ok := s.store.GoogleSessionByAccountID(accountID)
	if !ok {
		return
	}
	session.LastCheckedAt = time.Now()
	session.LastCheckOK = true
	session.Status = domain.GoogleStatusActive
	session.LastStatusMessage = ""
	_, _ = s.store.SaveGoogleSession(session)
}

// CheckGoogleAccount 用当前授权读取账号信息，验证 token 是否仍然有效。
func (s *Service) CheckGoogleAccount(ctx context.Context, accountID string) (domain.GoogleSession, error) {
	accountID = strings.TrimSpace(accountID)
	token, session, err := s.GoogleAccessToken(ctx, accountID)
	if err != nil {
		return session, err
	}
	profile, err := s.googleAuthClient().FetchProfile(ctx, token)
	if err != nil {
		s.markGoogleSessionFailure(accountID, domain.GoogleStatusFailed, err.Error())
		return session, err
	}
	session.Email = firstNonEmpty(strings.ToLower(strings.TrimSpace(profile.EmailAddress)), session.Email)
	session.DisplayName = firstNonEmpty(session.DisplayName, session.Email)
	session.Status = domain.GoogleStatusActive
	session.LastCheckedAt = time.Now()
	session.LastCheckOK = true
	session.LastStatusMessage = ""
	saved, err := s.store.SaveGoogleSession(session)
	if err != nil {
		return session, err
	}
	_ = s.store.RecordEvent("info", "google", "Google 账号授权校验通过："+firstNonEmpty(saved.Email, accountID))
	return saved, nil
}

// GoogleAliases 返回账号上可以收到的地址。
//
// 免费 Gmail 无法创建附加地址，也没有接口能枚举 + 标签——它们不是真实存在的地址，
// 只是主地址的变体。所以这里只返回主地址，多地址能力由 CreateGoogleAlias 生成。
func (s *Service) GoogleAliases(ctx context.Context, accountID string) ([]string, error) {
	accountID = strings.TrimSpace(accountID)
	token, session, err := s.GoogleAccessToken(ctx, accountID)
	if err != nil {
		return nil, err
	}
	aliases, err := s.gmailClient().ListAliases(ctx, token)
	if err != nil {
		if protocol.ErrorDetailsCode(err) == "google_token_rejected" {
			// token 被拒时清掉本地过期时间，让下一次调用走完整续期。
			session.TokenExpiresAt = time.Time{}
			_, _ = s.store.SaveGoogleSession(session)
		}
		return nil, err
	}
	s.markGoogleSessionSuccess(accountID)
	return aliases, nil
}

// ImportGoogleAliases 把账号主地址写入邮箱池。
//
// 与 Microsoft 的 ImportMicrosoftAliases 不同：那边能发现用户手工建的一串别名，
// 这边只有主地址一个。多地址请用 CreateGoogleAlias 生成 + 地址。
func (s *Service) ImportGoogleAliases(ctx context.Context, accountID string) (GoogleAliasImportResult, error) {
	accountID = strings.TrimSpace(accountID)
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok {
		return GoogleAliasImportResult{}, errors.New("Google 账号不存在")
	}
	if account.ProviderOrDefault() != domain.ProviderGoogle {
		return GoogleAliasImportResult{}, errors.New("该账号不是 Google 账号")
	}
	aliases, err := s.GoogleAliases(ctx, accountID)
	if err != nil {
		return GoogleAliasImportResult{}, err
	}
	result := GoogleAliasImportResult{
		AccountID: accountID,
		Found:     len(aliases),
		Aliases:   aliases,
		Notice:    "免费 Gmail 无法创建附加地址，这里只能导入主地址；需要更多地址请用「生成 + 地址」。",
	}
	for _, alias := range aliases {
		email := strings.ToLower(strings.TrimSpace(alias))
		if _, err := mail.ParseAddress(email); err != nil {
			continue
		}
		_, created, err := s.store.UpsertMailboxFromRemote(accountID, domain.RemoteMailbox{
			Provider: domain.ProviderGoogle,
			Email:    email,
			Label:    email,
			IsActive: true,
			Origin:   "google_primary",
		}, "Gmail 主地址，导入自账号信息")
		if err != nil {
			return result, fmt.Errorf("导入地址 %s 失败：%w", email, err)
		}
		if created {
			result.Created++
		} else {
			result.Updated++
		}
	}
	if result.Created > 0 || result.Updated > 0 {
		_ = s.store.RecordEvent("info", "google", fmt.Sprintf(
			"Google 地址导入完成：新增 %d 个，更新 %d 个", result.Created, result.Updated))
	}
	return result, nil
}

// CreateGoogleAlias 用一个 + 标签生成新地址并写入邮箱池。
//
// 这是 Gmail 唯一的免费多地址手段：you+shop@gmail.com 与 you@gmail.com 投递到
// 同一个收件箱，而 To / Delivered-To 头会原样保留 +shop，因此本地仍能按地址分发。
// 局限必须让使用者知道：+ 标签暴露真实地址，且部分网站会拒绝或截断 + 之后的字符。
func (s *Service) CreateGoogleAlias(accountID, tag, label, note string) (domain.Mailbox, bool, error) {
	accountID = strings.TrimSpace(accountID)
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok {
		return domain.Mailbox{}, false, errors.New("Google 账号不存在")
	}
	if account.ProviderOrDefault() != domain.ProviderGoogle {
		return domain.Mailbox{}, false, errors.New("该账号不是 Google 账号")
	}
	session, ok := s.store.GoogleSessionByAccountID(accountID)
	if !ok {
		return domain.Mailbox{}, false, errors.New("对应 Google 账号登录态不存在，请先完成授权")
	}
	base := firstNonEmpty(session.Email, account.AppleID)
	email, err := protocol.BuildPlusAlias(base, tag)
	if err != nil {
		return domain.Mailbox{}, false, err
	}
	return s.store.UpsertMailboxFromRemote(accountID, domain.RemoteMailbox{
		Provider: domain.ProviderGoogle,
		Email:    email,
		Label:    firstNonEmpty(strings.TrimSpace(label), email),
		IsActive: true,
		Origin:   "google_plus_alias",
	}, firstNonEmpty(note, "Gmail + 标签地址，与主地址共用同一个收件箱"))
}

// GoogleResolvedProxy 返回当前实际生效的代理地址，供界面显示诊断信息。
// 使用者需要能区分「代理没配上」和「代理配了但连不上」。
func (s *Service) GoogleResolvedProxy() string {
	return protocol.ProxyDescription(s.googleProxy())
}

// ImportGoogleMailbox 手工添加一个完整地址，用于账号上已有其他可收信地址的场景。
func (s *Service) ImportGoogleMailbox(accountID, email, label, note string) (domain.Mailbox, bool, error) {
	accountID = strings.TrimSpace(accountID)
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok {
		return domain.Mailbox{}, false, errors.New("Google 账号不存在")
	}
	if account.ProviderOrDefault() != domain.ProviderGoogle {
		return domain.Mailbox{}, false, errors.New("该账号不是 Google 账号")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	address, err := mail.ParseAddress(email)
	if err != nil || !strings.EqualFold(strings.TrimSpace(address.Address), email) {
		return domain.Mailbox{}, false, errors.New("邮箱地址格式不正确")
	}
	return s.store.UpsertMailboxFromRemote(accountID, domain.RemoteMailbox{
		Provider: domain.ProviderGoogle,
		Email:    email,
		Label:    firstNonEmpty(strings.TrimSpace(label), email),
		IsActive: true,
		Origin:   "google_manual",
	}, firstNonEmpty(note, "手动添加的 Gmail 地址"))
}

// GoogleAuthRedirectURI 按当前服务端口拼出回调地址。
//
// 桌面应用类型的客户端由 Google 通配 loopback 端口，不需要预先登记具体端口，
// 所以这里直接用本服务正在监听的端口即可。
func (s *Service) GoogleAuthRedirectURI() string {
	if base := strings.TrimSpace(s.store.Settings().GoogleRedirectBase); base != "" {
		return strings.TrimRight(base, "/") + "/api/google/callback"
	}
	if base := strings.TrimSpace(s.cfg.GoogleRedirectBase); base != "" {
		return strings.TrimRight(base, "/") + "/api/google/callback"
	}
	port := s.cfg.Port
	if port <= 0 {
		port = 8788
	}
	return fmt.Sprintf("http://127.0.0.1:%d/api/google/callback", port)
}

// StartGoogleAuth 生成一次授权流程，返回授权链接给前端打开。
func (s *Service) StartGoogleAuth() (domain.GoogleAuthFlow, error) {
	clientID := s.googleClientID()
	if strings.TrimSpace(clientID) == "" {
		return domain.GoogleAuthFlow{}, errors.New(
			"尚未填写 Google 客户端 ID。请先在 Google Cloud Console 建一个「桌面应用」类型的 OAuth 客户端，再填入设置。")
	}
	return protocol.NewGoogleAuthFlow(clientID, s.GoogleAuthRedirectURI())
}

// CompleteGoogleAuth 用回调拿到的授权码换取 token，并落库成账号。
func (s *Service) CompleteGoogleAuth(ctx context.Context, flow domain.GoogleAuthFlow, code, note string) (domain.GoogleSession, error) {
	token, err := s.googleAuthClient().ExchangeCode(ctx, flow.ClientID, s.googleClientSecret(), code, flow.CodeVerifier, flow.RedirectURI)
	if err != nil {
		return domain.GoogleSession{}, err
	}
	profile, err := s.googleAuthClient().FetchProfile(ctx, token.AccessToken)
	if err != nil {
		return domain.GoogleSession{}, err
	}
	email := strings.ToLower(strings.TrimSpace(profile.EmailAddress))
	if email == "" {
		return domain.GoogleSession{}, errors.New("Google 账号未返回邮箱地址，无法建立账号记录")
	}
	now := time.Now()
	session := domain.GoogleSession{
		Email:          email,
		DisplayName:    email,
		ClientID:       flow.ClientID,
		AccessToken:    token.AccessToken,
		RefreshToken:   token.RefreshToken,
		TokenExpiresAt: token.ExpiresAt,
		Scope:          token.Scope,
		Status:         domain.GoogleStatusActive,
		SavedAt:        now,
		LastCheckedAt:  now,
		LastCheckOK:    true,
		Note:           strings.TrimSpace(note),
	}
	if strings.TrimSpace(session.RefreshToken) == "" {
		// 没有 refresh_token 就意味着一小时后必须重新授权。这几乎总是因为
		// 授权链接少了 access_type=offline 或 prompt=consent，属于配置问题，
		// 直接报出来比让使用者在一天后收到一个莫名其妙的失效更省事。
		return domain.GoogleSession{}, errors.New(
			"Google 未返回 refresh_token，授权无法长期维持。请确认授权链接带了 access_type=offline 与 prompt=consent，然后重新授权")
	}
	saved, err := s.store.SaveGoogleSessionWithEvent(session, "info", "已添加 Google 账号 "+email)
	if err != nil {
		return domain.GoogleSession{}, err
	}
	return saved, nil
}

// syncGoogleGroupNow 完成一个 Google 账号下全部邮箱的收信。
// 与 Microsoft 路径同构：Gmail 一次返回收件箱的 id 列表，本地再按收件人把邮件分发到各个地址。
func (s *Service) syncGoogleGroupNow(ctx context.Context, mailboxes []domain.Mailbox, options MessageSyncOptions, accountResult AccountMessageSyncResult) (AccountMessageSyncResult, error) {
	accountID := strings.TrimSpace(accountResult.AccountID)
	token, _, err := s.GoogleAccessToken(ctx, accountID)
	if err != nil {
		return accountResult, err
	}

	after := options.After
	if after.IsZero() && !options.FullScan {
		var cursor MailSyncState
		if found, _ := s.store.LoadRuntimeState(mailSyncStateID(accountID, "google"), &cursor); found && !cursor.LastSyncAt.IsZero() {
			// 回退一分钟，避免上一轮写入与 Gmail 索引之间的时间差漏掉边界邮件。
			after = cursor.LastSyncAt.Add(-time.Minute)
		}
	}

	fetchOptions := protocol.GoogleMailSyncOptions{
		Limit:    options.Limit,
		After:    after,
		FullScan: options.FullScan,
	}
	syncResult, syncErr := s.gmailClient().SyncMailboxes(ctx, token, mailboxes, fetchOptions)
	if syncErr != nil {
		if protocol.ErrorDetailsCode(syncErr) == "google_token_rejected" {
			// token 被服务端拒绝：清掉本地过期时间，下一轮强制续期后重试一次。
			if session, ok := s.store.GoogleSessionByAccountID(accountID); ok {
				session.TokenExpiresAt = time.Time{}
				_, _ = s.store.SaveGoogleSession(session)
			}
			retryToken, _, refreshErr := s.GoogleAccessToken(ctx, accountID)
			if refreshErr != nil {
				s.markGoogleSessionFailure(accountID, domain.GoogleStatusFailed, refreshErr.Error())
				return accountResult, refreshErr
			}
			syncResult, syncErr = s.gmailClient().SyncMailboxes(ctx, retryToken, mailboxes, fetchOptions)
		}
		if syncErr != nil {
			s.markGoogleSessionFailure(accountID, domain.GoogleStatusFailed, syncErr.Error())
			return accountResult, syncErr
		}
	}
	s.markGoogleSessionSuccess(accountID)

	syncedAt := time.Now()
	updates := make([]store.MailboxSyncUpdate, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		update := store.MailboxSyncUpdate{MailboxID: mailbox.ID, SyncedAt: syncedAt}
		for _, message := range syncResult.MessagesByMailbox[mailbox.ID] {
			update.Messages = append(update.Messages, store.MailboxSyncMessage{
				RemoteID:    message.ID,
				CanonicalID: message.ID,
				Source:      "gmail",
				Subject:     message.Subject,
				From:        message.From,
				Body:        message.Body,
				HTMLBody:    message.HTMLBody,
				ContentType: message.ContentType,
				ReceivedAt:  message.ReceivedAt,
			})
		}
		updates = append(updates, update)
	}
	created, applyErr := s.store.ApplyMailboxSyncBatch(updates)
	if applyErr != nil {
		return accountResult, applyErr
	}
	// 记录本轮完成时间，作为下一次增量同步的起点。
	_ = s.store.SaveRuntimeState(mailSyncStateID(accountID, "google"), MailSyncState{
		AccountID:  accountID,
		Method:     "google",
		Folder:     "inbox",
		LastSyncAt: syncedAt,
	}, false)

	accountResult.Method = "gmail"
	accountResult.Scanned = syncResult.Scanned
	accountResult.Matched = syncResult.Matched
	accountResult.SyncedMessages = created
	accountResult.HasMore = syncResult.HasMore
	if syncResult.Unmatched > 0 {
		accountResult.FallbackReason = fmt.Sprintf("有 %d 封邮件未匹配到本地地址，可用「生成 + 地址」补齐后重新同步", syncResult.Unmatched)
	}
	return accountResult, nil
}
