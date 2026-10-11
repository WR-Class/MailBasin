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

// microsoftTokenRefreshWindow 决定 access token 还剩多久就提前续期。
const microsoftTokenRefreshWindow = 2 * time.Minute

// MicrosoftAliasImportResult 是一次别名导入的结果统计。
type MicrosoftAliasImportResult struct {
	AccountID string   `json:"account_id"`
	Found     int      `json:"found"`
	Created   int      `json:"created"`
	Updated   int      `json:"updated"`
	Aliases   []string `json:"aliases,omitempty"`
}

// MicrosoftSession 读取账号的 OAuth2 会话，不做续期。
func (s *Service) MicrosoftSession(accountID string) (domain.MicrosoftSession, bool) {
	return s.store.MicrosoftSessionByAccountID(strings.TrimSpace(accountID))
}

// MicrosoftAccessToken 返回一个当前可用的 access token；过期时用 refresh_token 续期。
// Microsoft 会在续期时轮换 refresh_token，因此每次成功续期都必须整体回写会话。
func (s *Service) MicrosoftAccessToken(ctx context.Context, accountID string) (string, domain.MicrosoftSession, error) {
	accountID = strings.TrimSpace(accountID)
	session, ok := s.store.MicrosoftSessionByAccountID(accountID)
	if !ok {
		return "", session, errors.New("对应 Microsoft 账号登录态不存在，请先完成设备码登录")
	}
	if token := strings.TrimSpace(session.AccessToken); token != "" && session.TokenExpiresAt.After(time.Now().Add(microsoftTokenRefreshWindow)) {
		return token, session, nil
	}
	if strings.TrimSpace(session.RefreshToken) == "" {
		s.markMicrosoftSessionFailure(accountID, domain.MicrosoftStatusNeedLogin, "缺少 refresh_token，请重新登录 Microsoft 账号")
		return "", session, errors.New("缺少 refresh_token，请重新登录 Microsoft 账号")
	}
	refreshed, err := s.microsoftAuth.RefreshAccessToken(ctx, session.ClientID, session.Tenant, session.RefreshToken)
	if err != nil {
		s.markMicrosoftSessionFailure(accountID, domain.MicrosoftStatusExpired, err.Error())
		return "", session, err
	}
	session.AccessToken = refreshed.AccessToken
	if strings.TrimSpace(refreshed.RefreshToken) != "" {
		session.RefreshToken = refreshed.RefreshToken
	}
	session.TokenExpiresAt = refreshed.ExpiresAt
	session.Scope = firstNonEmpty(refreshed.Scope, session.Scope)
	session.Status = domain.MicrosoftStatusActive
	session.LastStatusMessage = ""
	saved, err := s.store.SaveMicrosoftSession(session)
	if err != nil {
		return "", session, err
	}
	return saved.AccessToken, saved, nil
}

// markMicrosoftSessionFailure 把失败原因写回会话与账号状态，供前端展示。
func (s *Service) markMicrosoftSessionFailure(accountID, status, message string) {
	session, ok := s.store.MicrosoftSessionByAccountID(accountID)
	if !ok {
		return
	}
	session.LastCheckedAt = time.Now()
	session.LastCheckOK = false
	session.Status = status
	session.LastStatusMessage = message
	_, _ = s.store.SaveMicrosoftSession(session)
	_ = s.store.SaveMicrosoftAccountStatus(accountID, status, "")
}

// markMicrosoftSessionSuccess 记录一次成功调用。
func (s *Service) markMicrosoftSessionSuccess(accountID string) {
	session, ok := s.store.MicrosoftSessionByAccountID(accountID)
	if !ok {
		return
	}
	session.LastCheckedAt = time.Now()
	session.LastCheckOK = true
	session.Status = domain.MicrosoftStatusActive
	session.LastStatusMessage = ""
	_, _ = s.store.SaveMicrosoftSession(session)
}

// CheckMicrosoftAccount 用当前授权读取账号信息，验证 token 是否仍然有效。
func (s *Service) CheckMicrosoftAccount(ctx context.Context, accountID string) (domain.MicrosoftSession, error) {
	accountID = strings.TrimSpace(accountID)
	token, session, err := s.MicrosoftAccessToken(ctx, accountID)
	if err != nil {
		return session, err
	}
	profile, err := s.microsoftAuth.FetchProfile(ctx, token)
	if err != nil {
		s.markMicrosoftSessionFailure(accountID, domain.MicrosoftStatusFailed, err.Error())
		return session, err
	}
	session.Email = firstNonEmpty(profile.Email(), session.Email)
	session.DisplayName = firstNonEmpty(profile.DisplayName, session.DisplayName)
	session.Status = domain.MicrosoftStatusActive
	session.LastCheckedAt = time.Now()
	session.LastCheckOK = true
	session.LastStatusMessage = ""
	saved, err := s.store.SaveMicrosoftSession(session)
	if err != nil {
		return session, err
	}
	_ = s.store.RecordEvent("info", "microsoft", "Microsoft 账号授权校验通过："+firstNonEmpty(saved.Email, accountID))
	return saved, nil
}

// MicrosoftAliases 读取账号上的全部邮件地址（主地址与用户手工创建的别名）。
// Graph 没有创建别名的接口，所以这里只做发现，导入后的别名由本项目负责收信取码。
func (s *Service) MicrosoftAliases(ctx context.Context, accountID string) ([]string, error) {
	accountID = strings.TrimSpace(accountID)
	token, session, err := s.MicrosoftAccessToken(ctx, accountID)
	if err != nil {
		return nil, err
	}
	aliases, err := s.graph.ListAliases(ctx, token)
	if err != nil {
		if protocol.ErrorDetailsCode(err) == "microsoft_token_rejected" {
			// token 被拒时清掉本地过期凭据，让下一次调用走完整续期。
			session.TokenExpiresAt = time.Time{}
			_, _ = s.store.SaveMicrosoftSession(session)
		}
		return nil, err
	}
	s.markMicrosoftSessionSuccess(accountID)
	return aliases, nil
}

const (
	microsoftPrimaryNote    = "Microsoft 主地址，导入自账号信息"
	microsoftAliasNote      = "Microsoft 别名，导入自账号代理地址"
	microsoftAliasOldOrigin = "microsoft_alias"
)

// importMicrosoftAddress 写入一个微软地址，按它是不是主地址选 origin 和备注。
// 主地址必须和别名区分开：以前一律按别名写入，邮箱池里就把主邮箱标成了「别名」。
func (s *Service) importMicrosoftAddress(accountID, address string, isPrimary bool, result *MicrosoftAliasImportResult) error {
	email := strings.ToLower(strings.TrimSpace(address))
	if _, err := mail.ParseAddress(email); err != nil {
		return nil
	}
	origin, note := microsoftAliasOldOrigin, microsoftAliasNote
	if isPrimary {
		origin, note = "microsoft_primary", microsoftPrimaryNote
	}
	mailbox, created, err := s.store.UpsertMailboxFromRemote(accountID, domain.RemoteMailbox{
		Provider: domain.ProviderMicrosoft,
		Email:    email,
		Label:    email,
		IsActive: true,
		Origin:   origin,
	}, note)
	if err != nil {
		return fmt.Errorf("导入地址 %s 失败：%w", email, err)
	}
	// UpsertMailboxFromRemote 只在备注为空时写默认值，所以旧版本按别名写入的主地址
	// 会一直留着「别名」这条错误备注。这里只在备注正好还是旧默认值时纠正它，
	// 不覆盖用户自己写过的备注。
	if isPrimary && !created && mailbox.Note == microsoftAliasNote {
		corrected := microsoftPrimaryNote
		if _, err := s.store.SetMailboxStatus(mailbox.ID, nil, nil, "", &corrected); err != nil {
			return fmt.Errorf("纠正 %s 的备注失败：%w", email, err)
		}
	}
	if created {
		result.Created++
	} else {
		result.Updated++
	}
	return nil
}

// MicrosoftPrimaryAddress 返回账号的主地址。
func (s *Service) MicrosoftPrimaryAddress(ctx context.Context, accountID string) (string, error) {
	accountID = strings.TrimSpace(accountID)
	token, _, err := s.MicrosoftAccessToken(ctx, accountID)
	if err != nil {
		return "", err
	}
	primary, _, err := s.graph.ListAddresses(ctx, token)
	if err != nil {
		return "", err
	}
	if primary == "" {
		return "", errors.New("Microsoft 未返回主地址，请稍后重试")
	}
	s.markMicrosoftSessionSuccess(accountID)
	return primary, nil
}

// ImportMicrosoftPrimary 只导入账号的主地址，对应谷歌那边「导入主地址」的用法。
// 用户手工在 account.microsoft.com 建别名之前，至少能先把主邮箱放进邮箱池。
func (s *Service) ImportMicrosoftPrimary(ctx context.Context, accountID string) (MicrosoftAliasImportResult, error) {
	accountID = strings.TrimSpace(accountID)
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok {
		return MicrosoftAliasImportResult{}, errors.New("Microsoft 账号不存在")
	}
	if account.ProviderOrDefault() != domain.ProviderMicrosoft {
		return MicrosoftAliasImportResult{}, errors.New("该账号不是 Microsoft 账号")
	}
	primary, err := s.MicrosoftPrimaryAddress(ctx, accountID)
	if err != nil {
		return MicrosoftAliasImportResult{}, err
	}
	result := MicrosoftAliasImportResult{AccountID: accountID, Found: 1, Aliases: []string{primary}}
	if err := s.importMicrosoftAddress(accountID, primary, true, &result); err != nil {
		return result, err
	}
	if result.Created > 0 || result.Updated > 0 {
		_ = s.store.RecordEvent("info", "microsoft", fmt.Sprintf(
			"Microsoft 主地址导入完成：新增 %d 个，更新 %d 个", result.Created, result.Updated))
	}
	return result, nil
}

// ImportMicrosoftAliases 把账号上的全部地址（主地址 + 别名）批量写入邮箱池，
// 已存在的地址只更新归属与状态。主地址标 microsoft_primary，别名标 microsoft_alias。
func (s *Service) ImportMicrosoftAliases(ctx context.Context, accountID string) (MicrosoftAliasImportResult, error) {
	accountID = strings.TrimSpace(accountID)
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok {
		return MicrosoftAliasImportResult{}, errors.New("Microsoft 账号不存在")
	}
	if account.ProviderOrDefault() != domain.ProviderMicrosoft {
		return MicrosoftAliasImportResult{}, errors.New("该账号不是 Microsoft 账号")
	}
	token, session, err := s.MicrosoftAccessToken(ctx, accountID)
	if err != nil {
		return MicrosoftAliasImportResult{}, err
	}
	primary, aliases, err := s.graph.ListAddresses(ctx, token)
	if err != nil {
		if protocol.ErrorDetailsCode(err) == "microsoft_token_rejected" {
			// token 被拒时清掉本地过期凭据，让下一次调用走完整续期。
			session.TokenExpiresAt = time.Time{}
			_, _ = s.store.SaveMicrosoftSession(session)
		}
		return MicrosoftAliasImportResult{}, err
	}
	s.markMicrosoftSessionSuccess(accountID)
	addresses := append([]string{}, aliases...)
	if primary != "" {
		addresses = append(addresses, primary)
	}
	result := MicrosoftAliasImportResult{AccountID: accountID, Found: len(addresses), Aliases: addresses}
	for _, address := range addresses {
		isPrimary := primary != "" && strings.EqualFold(address, primary)
		if err := s.importMicrosoftAddress(accountID, address, isPrimary, &result); err != nil {
			return result, err
		}
	}
	if result.Created > 0 || result.Updated > 0 {
		_ = s.store.RecordEvent("info", "microsoft", fmt.Sprintf(
			"Microsoft 别名导入完成：新增 %d 个，更新 %d 个", result.Created, result.Updated))
	}
	return result, nil
}

// ImportMicrosoftAlias 手工添加单个别名地址，用于 Graph 未返回代理地址的账号。
func (s *Service) ImportMicrosoftAlias(accountID, email, label, note string) (domain.Mailbox, bool, error) {
	accountID = strings.TrimSpace(accountID)
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok {
		return domain.Mailbox{}, false, errors.New("Microsoft 账号不存在")
	}
	if account.ProviderOrDefault() != domain.ProviderMicrosoft {
		return domain.Mailbox{}, false, errors.New("该账号不是 Microsoft 账号")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	address, err := mail.ParseAddress(email)
	if err != nil || !strings.EqualFold(strings.TrimSpace(address.Address), email) {
		return domain.Mailbox{}, false, errors.New("邮箱地址格式不正确")
	}
	return s.store.UpsertMailboxFromRemote(accountID, domain.RemoteMailbox{
		Provider: domain.ProviderMicrosoft,
		Email:    email,
		Label:    strings.TrimSpace(label),
		IsActive: true,
		Origin:   "microsoft_alias_manual",
	}, firstNonEmpty(note, "手动添加的 Microsoft 别名"))
}

// MicrosoftAliasBatchResult 是一次批量生成的结果统计。
type MicrosoftAliasBatchResult struct {
	AccountID string   `json:"account_id"`
	Requested int      `json:"requested"`
	Created   int      `json:"created"`
	Skipped   int      `json:"skipped"`
	Prefix    string   `json:"prefix,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
}

const (
	// MicrosoftAliasBatchMax 是单次批量生成的数量上限。
	MicrosoftAliasBatchMax = 1000
	// microsoftAliasBatchAttempts 是每个地址的最大重试次数；兜不住就如实记为 skipped。
	microsoftAliasBatchAttempts = 20
)

// CreateMicrosoftAliases 批量生成 + 标签地址。
//
// 免费 Outlook.com 同样支持 plus addressing——实测确认过：发到
// qiaobaochong823680+tag@outlook.com 的邮件能投递，且 Graph 的 toRecipients 原样保留标签。
// 所以这里和 Gmail 的做法完全一致。
//
// 跨批次不会重复：生成前先把已有的全部地址读进一个集合，每个候选同时比对「已有地址」和
// 「本批次已生成的地址」。凑不够时如实返回 skipped，而不是静默少建几个。
//
// 前提：+ 地址必须先在邮箱池里存在才能收到信——路由是按收件人精确匹配的。
func (s *Service) CreateMicrosoftAliases(accountID, prefix string, count int, label, note string) (MicrosoftAliasBatchResult, error) {
	accountID = strings.TrimSpace(accountID)
	account, ok := s.store.FindAppleAccount(accountID)
	if !ok {
		return MicrosoftAliasBatchResult{}, errors.New("Microsoft 账号不存在")
	}
	if account.ProviderOrDefault() != domain.ProviderMicrosoft {
		return MicrosoftAliasBatchResult{}, errors.New("该账号不是 Microsoft 账号")
	}
	session, ok := s.store.MicrosoftSessionByAccountID(accountID)
	if !ok {
		return MicrosoftAliasBatchResult{}, errors.New("对应 Microsoft 账号登录态不存在，请先完成设备码登录")
	}
	if count <= 0 {
		return MicrosoftAliasBatchResult{}, errors.New("生成数量必须大于 0")
	}
	if count > MicrosoftAliasBatchMax {
		return MicrosoftAliasBatchResult{}, fmt.Errorf("单次最多生成 %d 个地址，当前请求 %d 个", MicrosoftAliasBatchMax, count)
	}
	base := firstNonEmpty(session.Email, account.AppleID)
	if _, err := protocol.BuildMicrosoftPlusAlias(base, "probe"); err != nil {
		return MicrosoftAliasBatchResult{}, err
	}
	tagLimit := protocol.MaxPlusTagLength(base)
	if tagLimit < 1 {
		return MicrosoftAliasBatchResult{}, fmt.Errorf(
			"主地址 %s 的本地部分已占满 %d 个字符，无法再生成 + 别名", base, protocol.MaxPlusAliasLocalPart)
	}
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	randomLength := protocol.DefaultPlusTagLength
	if prefix != "" {
		// 前缀 + "-" + 随机串必须整体放得下，否则缩短随机部分而不是拒绝请求。
		randomLength = tagLimit - len(prefix) - 1
		if randomLength < 1 {
			return MicrosoftAliasBatchResult{}, fmt.Errorf(
				"前缀「%s」太长：这个主地址最多容纳 %d 个字符的标签，前缀已占 %d 个", prefix, tagLimit, len(prefix))
		}
	}
	if randomLength > protocol.DefaultPlusTagLength {
		randomLength = protocol.DefaultPlusTagLength
	}
	seen := map[string]struct{}{}
	for _, mailbox := range s.store.AllMailboxes() {
		seen[strings.ToLower(strings.TrimSpace(mailbox.Email))] = struct{}{}
	}
	result := MicrosoftAliasBatchResult{AccountID: accountID, Requested: count, Prefix: prefix}
	for index := 0; index < count; index++ {
		created := false
		for attempt := 0; attempt < microsoftAliasBatchAttempts; attempt++ {
			tag, err := protocol.RandomPlusTag(prefix, randomLength)
			if err != nil {
				return result, err
			}
			email, err := protocol.BuildMicrosoftPlusAlias(base, tag)
			if err != nil {
				return result, err
			}
			if _, exists := seen[email]; exists {
				continue
			}
			_, wasCreated, err := s.store.UpsertMailboxFromRemote(accountID, domain.RemoteMailbox{
				Provider: domain.ProviderMicrosoft,
				Email:    email,
				Label:    firstNonEmpty(strings.TrimSpace(label), email),
				IsActive: true,
				Origin:   "microsoft_plus_alias",
			}, firstNonEmpty(note, "Outlook + 标签地址，与主地址共用同一个收件箱"))
			if err != nil {
				return result, fmt.Errorf("生成地址 %s 失败：%w", email, err)
			}
			seen[email] = struct{}{}
			if !wasCreated {
				// 并发下被别人抢先建了同一个地址，换一个再来。
				continue
			}
			result.Created++
			result.Addresses = append(result.Addresses, email)
			created = true
			break
		}
		if !created {
			result.Skipped++
		}
	}
	if result.Created > 0 {
		_ = s.store.RecordEvent("info", "microsoft", fmt.Sprintf(
			"Outlook + 地址批量生成完成：新增 %d 个，未能生成 %d 个", result.Created, result.Skipped))
	}
	return result, nil
}

// StartMicrosoftDeviceLogin 发起设备码登录，返回给用户去浏览器输入的验证码。
func (s *Service) StartMicrosoftDeviceLogin(ctx context.Context, clientID, tenant string) (domain.MicrosoftDeviceLogin, error) {
	return s.microsoftAuth.StartDeviceCodeLogin(ctx, clientID, tenant)
}

// PollMicrosoftDeviceLogin 轮询设备码登录结果。用户完成登录后立即拉取账号信息并落库。
// 返回 protocol.ErrMicrosoftAuthorizationPending 表示用户还没在浏览器里完成操作。
func (s *Service) PollMicrosoftDeviceLogin(ctx context.Context, login domain.MicrosoftDeviceLogin, note string) (domain.MicrosoftSession, error) {
	token, err := s.microsoftAuth.PollDeviceCodeToken(ctx, login.ClientID, login.Tenant, login.DeviceCode)
	if err != nil {
		return domain.MicrosoftSession{}, err
	}
	profile, err := s.microsoftAuth.FetchProfile(ctx, token.AccessToken)
	if err != nil {
		return domain.MicrosoftSession{}, err
	}
	now := time.Now()
	session := domain.MicrosoftSession{
		Email:          profile.Email(),
		DisplayName:    profile.DisplayName,
		Tenant:         login.Tenant,
		ClientID:       login.ClientID,
		AccessToken:    token.AccessToken,
		RefreshToken:   token.RefreshToken,
		TokenExpiresAt: token.ExpiresAt,
		Scope:          token.Scope,
		Status:         domain.MicrosoftStatusActive,
		SavedAt:        now,
		LastCheckedAt:  now,
		LastCheckOK:    true,
		Note:           strings.TrimSpace(note),
	}
	if strings.TrimSpace(session.Email) == "" {
		return domain.MicrosoftSession{}, errors.New("Microsoft 账号未返回邮箱地址，无法建立账号记录")
	}
	saved, err := s.store.SaveMicrosoftSessionWithEvent(session, "info", "已添加 Microsoft 账号 "+session.Email)
	if err != nil {
		return domain.MicrosoftSession{}, err
	}
	return saved, nil
}

// syncMicrosoftGroupNow 完成一个 Microsoft 账号下全部邮箱的收信。
// 与 iCloud 路径的差别是：Graph 一次返回整个收件箱，本地再按收件人把邮件分发到各个别名。
func (s *Service) syncMicrosoftGroupNow(ctx context.Context, mailboxes []domain.Mailbox, options MessageSyncOptions, accountResult AccountMessageSyncResult) (AccountMessageSyncResult, error) {
	accountID := strings.TrimSpace(accountResult.AccountID)
	token, _, err := s.MicrosoftAccessToken(ctx, accountID)
	if err != nil {
		return accountResult, err
	}

	after := options.After
	if after.IsZero() && !options.FullScan {
		var cursor MailSyncState
		if found, _ := s.store.LoadRuntimeState(mailSyncStateID(accountID, "graph"), &cursor); found && !cursor.LastSyncAt.IsZero() {
			// 回退一分钟，避免上一轮写入与 Graph 索引之间的时间差漏掉边界邮件。
			after = cursor.LastSyncAt.Add(-time.Minute)
		}
	}

	fetchOptions := protocol.MicrosoftMailSyncOptions{
		Limit:    options.Limit,
		After:    after,
		FullScan: options.FullScan,
	}
	syncResult, syncErr := s.graph.SyncMailboxes(ctx, token, mailboxes, fetchOptions)
	if syncErr != nil {
		if protocol.ErrorDetailsCode(syncErr) == "microsoft_token_rejected" {
			// token 被服务端拒绝：清掉本地过期时间，下一轮强制续期后重试一次。
			if session, ok := s.store.MicrosoftSessionByAccountID(accountID); ok {
				session.TokenExpiresAt = time.Time{}
				_, _ = s.store.SaveMicrosoftSession(session)
			}
			retryToken, _, refreshErr := s.MicrosoftAccessToken(ctx, accountID)
			if refreshErr != nil {
				s.markMicrosoftSessionFailure(accountID, domain.MicrosoftStatusFailed, refreshErr.Error())
				return accountResult, refreshErr
			}
			syncResult, syncErr = s.graph.SyncMailboxes(ctx, retryToken, mailboxes, fetchOptions)
		}
		if syncErr != nil {
			s.markMicrosoftSessionFailure(accountID, domain.MicrosoftStatusFailed, syncErr.Error())
			return accountResult, syncErr
		}
	}
	s.markMicrosoftSessionSuccess(accountID)

	syncedAt := time.Now()
	updates := make([]store.MailboxSyncUpdate, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		update := store.MailboxSyncUpdate{MailboxID: mailbox.ID, SyncedAt: syncedAt}
		for _, message := range syncResult.MessagesByMailbox[mailbox.ID] {
			update.Messages = append(update.Messages, store.MailboxSyncMessage{
				RemoteID:    message.ID,
				CanonicalID: message.ID,
				Source:      "graph",
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
	_ = s.store.SaveRuntimeState(mailSyncStateID(accountID, "graph"), MailSyncState{
		AccountID:  accountID,
		Method:     "graph",
		Folder:     "inbox",
		LastSyncAt: syncedAt,
	}, false)

	accountResult.Method = "graph"
	accountResult.Scanned = syncResult.Scanned
	accountResult.Matched = syncResult.Matched
	accountResult.SyncedMessages = created
	accountResult.HasMore = syncResult.HasMore
	if syncResult.Unmatched > 0 {
		accountResult.FallbackReason = fmt.Sprintf("有 %d 封邮件未匹配到本地别名，可在账号页导入别名后重新同步", syncResult.Unmatched)
	}
	return accountResult, nil
}
