package updatecheck

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"icloud-privacy-mail-v2/internal/buildinfo"
)

const (
	cacheTTL        = 10 * time.Minute
	requestTimeout  = 20 * time.Second
	responseMaxSize = 4 << 20
	githubRawURL    = "https://raw.githubusercontent.com"
	githubWebURL    = "https://github.com"
	// defaultBranch 是更新器会把工作树重置到的分支，以 scripts/update.ps1 的
	// -Target 默认值 origin/main 为准。改了那边必须改这里，否则界面会写出一个
	// 更新器根本不会去的地方。
	defaultBranch = "main"
)

//go:embed announcements.json
var announcementFiles embed.FS

type Announcement struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	Content     string `json:"content"`
	PublishedAt string `json:"published_at"`
	URL         string `json:"url,omitempty"`
}

type LatestInfo struct {
	Version     string `json:"version"`
	Name        string `json:"name"`
	Notes       string `json:"notes"`
	PublishedAt string `json:"published_at"`
	URL         string `json:"url"`
	Source      string `json:"source"`
}

// TargetInfo 是「这次更新会落到哪里」的预览。
//
// 它不是推断：更新器做的就是 fetch origin main + reset --hard origin/main，
// 所以这里的提交号就是它接下来会得到的那个。取不到时 Commit 留空并在 Error
// 里说明原因——绝不用分支名或上一次的值填成看起来确定的东西，那比不知道更坏。
type TargetInfo struct {
	Branch    string `json:"branch"`
	Commit    string `json:"commit"`
	FetchedAt string `json:"fetched_at"`
	Error     string `json:"error,omitempty"`
}

type Status struct {
	Enabled         bool           `json:"enabled"`
	Repository      string         `json:"repository"`
	RepositoryURL   string         `json:"repository_url"`
	Current         buildinfo.Info `json:"current"`
	Latest          *LatestInfo    `json:"latest,omitempty"`
	Target          *TargetInfo    `json:"target,omitempty"`
	UpdateAvailable bool           `json:"update_available"`
	CheckedAt       string         `json:"checked_at"`
	Error           string         `json:"error,omitempty"`
	Announcements   []Announcement `json:"announcements"`
}

type Service struct {
	enabled    bool
	repository string
	client     *http.Client
	rawBaseURL string
	webBaseURL string
	mu         sync.Mutex
	cachedAt   time.Time
	cached     Status
}

type httpStatusError struct {
	StatusCode int
	Body       string
}

func (e *httpStatusError) Error() string {
	if strings.TrimSpace(e.Body) == "" {
		return fmt.Sprintf("HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("HTTP %d：%s", e.StatusCode, strings.TrimSpace(e.Body))
}

type announcementDocument struct {
	SchemaVersion int             `json:"schema_version"`
	Latest        *latestDocument `json:"latest"`
	Announcements []Announcement  `json:"announcements"`
}

type latestDocument struct {
	Version     string `json:"version"`
	Name        string `json:"name"`
	Notes       string `json:"notes"`
	PublishedAt string `json:"published_at"`
	URL         string `json:"url"`
}

func New(enabled bool, repository string) *Service {
	return &Service{
		enabled:    enabled,
		repository: strings.Trim(strings.TrimSpace(repository), "/"),
		client:     &http.Client{Timeout: requestTimeout},
		rawBaseURL: githubRawURL,
		webBaseURL: githubWebURL,
	}
}

// SetHTTPClient 替换内部使用的 HTTP 客户端。
//
// 默认的 &http.Client{} 走 http.DefaultTransport，而 Go 的 DefaultTransport
// 只认 HTTP_PROXY / HTTPS_PROXY 环境变量，从不读取 Windows 的系统代理设置。
// 结果是：浏览器和 PowerShell 都能打开 raw.githubusercontent.com，本服务却报
// "wsarecv: An existing connection was forcibly closed by the remote host"——
// 看起来像代码问题，实际是没走代理。
//
// 所以在装配处注入一个走代理的客户端（见 protocol.NewHTTPClient），
// 让更新检查和其它联网模块用同一套代理解析。
func (s *Service) SetHTTPClient(client *http.Client) {
	if client == nil {
		return
	}
	s.client = client
}

// Check 通过仓库 Raw 公告清单检查项目版本和公告。
func (s *Service) Check(ctx context.Context, force bool) Status {
	now := time.Now()
	s.mu.Lock()
	if !force && !s.cachedAt.IsZero() && now.Sub(s.cachedAt) < cacheTTL {
		cached := s.cached
		s.mu.Unlock()
		return cached
	}
	s.mu.Unlock()

	status := s.check(ctx, now)
	s.mu.Lock()
	s.cached = status
	s.cachedAt = now
	s.mu.Unlock()
	return status
}

func (s *Service) check(ctx context.Context, checkedAt time.Time) Status {
	repositoryURL := ""
	if validRepository(s.repository) {
		repositoryURL = "https://github.com/" + s.repository
	}
	status := Status{
		Enabled:       s.enabled,
		Repository:    s.repository,
		RepositoryURL: repositoryURL,
		Current:       buildinfo.Current(),
		CheckedAt:     checkedAt.Format(time.RFC3339),
		Announcements: builtinAnnouncements(),
	}
	if !s.enabled {
		return status
	}
	if !validRepository(s.repository) {
		status.Error = "更新仓库格式不正确，应为 owner/repository"
		return status
	}

	// 先问目标再问公告：公告走 CDN，可能被缓存住最多五分钟，而「会落到哪里」
	// 不该跟着它一起晚到。
	status.Target = s.checkTarget(ctx)

	document, documentErr := s.fetchRepositoryDocument(ctx)
	if documentErr != nil {
		status.Error = "读取仓库公告配置失败：" + documentErr.Error()
		return status
	}
	status.Announcements = mergeAnnouncements(status.Announcements, document.Announcements)
	if document.SchemaVersion != 1 {
		status.Error = fmt.Sprintf("仓库公告配置版本不支持：schema_version 应为 1，当前为 %d", document.SchemaVersion)
		return status
	}
	if document.Latest == nil {
		status.Error = "仓库公告配置缺少 latest"
		return status
	}
	latest, updateAvailable, updateAnnouncement, err := configuredLatest(status.Current, *document.Latest)
	if err != nil {
		status.Error = "仓库公告配置无效：" + err.Error()
		return status
	}
	status.Latest = latest
	status.UpdateAvailable = updateAvailable
	if updateAnnouncement != nil {
		status.Announcements = mergeAnnouncements(status.Announcements, []Announcement{*updateAnnouncement})
	}
	return status
}

func configuredLatest(current buildinfo.Info, document latestDocument) (*LatestInfo, bool, *Announcement, error) {
	latest := LatestInfo{
		Version:     truncateText(strings.TrimSpace(document.Version), 120),
		Name:        truncateText(strings.TrimSpace(document.Name), 200),
		Notes:       truncateText(strings.TrimSpace(document.Notes), 8000),
		PublishedAt: strings.TrimSpace(document.PublishedAt),
		URL:         safeHTTPSURL(document.URL),
		Source:      "config",
	}
	if latest.Version == "" {
		return nil, false, nil, errors.New("latest.version 为空")
	}
	if _, valid := parseVersion(latest.Version); !valid {
		return nil, false, nil, errors.New("latest.version 应为语义化版本，例如 2.1.0")
	}
	if strings.TrimSpace(document.URL) != "" && latest.URL == "" {
		return nil, false, nil, errors.New("latest.url 应为有效的 HTTPS 地址")
	}
	latest.Name = firstNonEmpty(latest.Name, latest.Version)
	updateAvailable := versionIsNewer(current.Version, latest.Version)
	if !updateAvailable {
		return &latest, false, nil, nil
	}
	announcement := normalizeAnnouncement(Announcement{
		ID:          "release-" + normalizeID(latest.Version),
		Type:        "update",
		Title:       "版本更新 " + latest.Version,
		Summary:     firstNonEmpty(firstContentLine(latest.Notes), "项目更新清单已发布新版本。"),
		Content:     firstNonEmpty(latest.Notes, "项目更新清单已发布新版本。"),
		PublishedAt: latest.PublishedAt,
		URL:         latest.URL,
	})
	return &latest, updateAvailable, &announcement, nil
}

func (s *Service) fetchRepositoryDocument(ctx context.Context) (announcementDocument, error) {
	announcementsURL := strings.TrimRight(s.rawBaseURL, "/") + "/" + s.repository + "/HEAD/internal/updatecheck/announcements.json?t=" + strconv.FormatInt(time.Now().UnixNano(), 10)
	var document announcementDocument
	if err := s.getJSON(ctx, announcementsURL, &document); err != nil {
		return announcementDocument{}, err
	}
	document.Announcements = normalizeAnnouncements(document.Announcements)
	return document, nil
}

func (s *Service) getJSON(ctx context.Context, targetURL string, target any) error {
	return s.get(ctx, targetURL, "application/json", func(reader io.Reader) error {
		decoder := json.NewDecoder(reader)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			return fmt.Errorf("解析 JSON 失败：%w", err)
		}
		return nil
	})
}

func (s *Service) get(ctx context.Context, targetURL, accept string, decode func(io.Reader) error) error {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Pragma", "no-cache")
	request.Header.Set("User-Agent", "iCloud-Privacy-Mail-v2-Updater/"+buildinfo.Current().Version)
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return &httpStatusError{StatusCode: response.StatusCode, Body: string(body)}
	}
	return decode(io.LimitReader(response.Body, responseMaxSize))
}

// checkTarget 组装「这次更新会落到哪里」的预览。
//
// 取不到不当作整体失败：公告读得到而分支头读不到时，让界面退回原来的措辞，
// 比整块报错更有用。
func (s *Service) checkTarget(ctx context.Context) *TargetInfo {
	info := &TargetInfo{
		Branch:    defaultBranch,
		FetchedAt: time.Now().Format(time.RFC3339),
	}
	commit, err := s.fetchBranchHead(ctx, defaultBranch)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.Commit = commit
	return info
}

// fetchBranchHead 问远端：这个分支现在指向哪个提交。
//
// 走 git 的 smart HTTP 广告（info/refs），和 git ls-remote 是同一条路：不需要
// 令牌、没有 API 的速率限制，而且它给出的答案就是更新器接下来会 reset 到的那个。
// 不能用 raw.githubusercontent.com——那是文件 CDN，只回答「某个文件的内容」，
// 不回答「分支现在指向哪」。
func (s *Service) fetchBranchHead(ctx context.Context, branch string) (string, error) {
	target := strings.TrimRight(s.webBaseURL, "/") + "/" + s.repository + "/info/refs?service=git-upload-pack"
	commit := ""
	err := s.get(ctx, target, "application/x-git-upload-pack-advertisement", func(reader io.Reader) error {
		raw, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		commit = findRefCommit(raw, "refs/heads/"+branch)
		return nil
	})
	if err != nil {
		return "", err
	}
	if commit == "" {
		return "", fmt.Errorf("远端没有 refs/heads/%s", branch)
	}
	return commit, nil
}

// findRefCommit 从 git 的 pkt-line 广告里取出某个引用的提交号。
//
// 必须按长度前缀逐个包前进，不能按行切：pkt-line 之间没有换行，只有包自己的负载
// 里可能带换行——按行切会把 flush 包和下一个包粘成一行，读到的位置整体错开四字节。
// 这一点是测试逼出来的：假广告里 HEAD 行前面四项恰好也是十六进制，于是「按行切」
// 那一版安静地把提交号读空了（返回空字符串，而不是报错）。
//
// 每个包形如：<4 位十六进制总长度><负载>，负载为
// <40 位提交号><空格><引用名>[\0<能力列表>]。长度为 0 是 flush 包，跳过四个字节。
func findRefCommit(raw []byte, ref string) string {
	rest := raw
	for len(rest) >= 4 {
		size := hexSize(rest[:4])
		if size < 0 {
			return ""
		}
		if size == 0 {
			rest = rest[4:]
			continue
		}
		if size < 4 || size > len(rest) {
			return ""
		}
		text := strings.TrimRight(string(rest[4:size]), "\n")
		rest = rest[size:]
		if len(text) < 40 {
			continue
		}
		sha := text[:40]
		if !isHexSHA(sha) {
			continue
		}
		name := strings.TrimSpace(strings.SplitN(text[40:], "\x00", 2)[0])
		if name == ref {
			return sha
		}
	}
	return ""
}

// hexSize 解析 4 位十六进制的包长度，非十六进制返回 -1。
func hexSize(prefix []byte) int {
	value := 0
	for _, char := range prefix {
		switch {
		case char >= '0' && char <= '9':
			value = value*16 + int(char-'0')
		case char >= 'a' && char <= 'f':
			value = value*16 + int(char-'a'+10)
		case char >= 'A' && char <= 'F':
			value = value*16 + int(char-'A'+10)
		default:
			return -1
		}
	}
	return value
}

func isHexSHA(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func builtinAnnouncements() []Announcement {
	raw, err := announcementFiles.ReadFile("announcements.json")
	if err != nil {
		return []Announcement{}
	}
	var document announcementDocument
	if json.Unmarshal(raw, &document) != nil {
		return []Announcement{}
	}
	return normalizeAnnouncements(document.Announcements)
}

func mergeAnnouncements(groups ...[]Announcement) []Announcement {
	items := map[string]Announcement{}
	for _, group := range groups {
		for _, raw := range group {
			item := normalizeAnnouncement(raw)
			if item.ID == "" || item.Title == "" {
				continue
			}
			items[item.ID] = item
		}
	}
	result := make([]Announcement, 0, len(items))
	for _, item := range items {
		result = append(result, item)
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].PublishedAt > result[j].PublishedAt
	})
	return result
}

func normalizeAnnouncements(items []Announcement) []Announcement {
	return mergeAnnouncements(items)
}

func normalizeAnnouncement(item Announcement) Announcement {
	item.ID = truncateText(strings.TrimSpace(item.ID), 120)
	item.Type = strings.ToLower(strings.TrimSpace(item.Type))
	switch item.Type {
	case "update", "project", "system":
	default:
		item.Type = "project"
	}
	item.Title = truncateText(strings.TrimSpace(item.Title), 160)
	item.Summary = truncateText(strings.TrimSpace(item.Summary), 500)
	item.Content = truncateText(strings.TrimSpace(item.Content), 8000)
	item.PublishedAt = strings.TrimSpace(item.PublishedAt)
	item.URL = safeHTTPSURL(item.URL)
	return item
}

func validRepository(repository string) bool {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, part := range parts {
		for _, char := range part {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
				continue
			}
			return false
		}
	}
	return true
}

func safeHTTPSURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}
	return parsed.String()
}

func versionIsNewer(current, latest string) bool {
	latestVersion, latestValid := parseVersion(latest)
	if !latestValid {
		return false
	}
	currentVersion, currentValid := parseVersion(current)
	if !currentValid {
		return normalizeVersion(current) != normalizeVersion(latest)
	}
	maxParts := len(currentVersion.Parts)
	if len(latestVersion.Parts) > maxParts {
		maxParts = len(latestVersion.Parts)
	}
	for index := 0; index < maxParts; index++ {
		currentPart := 0
		latestPart := 0
		if index < len(currentVersion.Parts) {
			currentPart = currentVersion.Parts[index]
		}
		if index < len(latestVersion.Parts) {
			latestPart = latestVersion.Parts[index]
		}
		if latestPart != currentPart {
			return latestPart > currentPart
		}
	}
	return comparePrerelease(latestVersion.Prerelease, currentVersion.Prerelease) > 0
}

func normalizeVersion(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "version")
	value = strings.TrimSpace(value)
	return strings.TrimPrefix(value, "v")
}

type parsedVersion struct {
	Parts      []int
	Prerelease string
}

func parseVersion(value string) (parsedVersion, bool) {
	value = normalizeVersion(value)
	if buildIndex := strings.IndexByte(value, '+'); buildIndex >= 0 {
		value = value[:buildIndex]
	}
	prerelease := ""
	if prereleaseIndex := strings.IndexByte(value, '-'); prereleaseIndex >= 0 {
		prerelease = value[prereleaseIndex+1:]
		value = value[:prereleaseIndex]
		if prerelease == "" {
			return parsedVersion{}, false
		}
	}
	fields := strings.Split(value, ".")
	if len(fields) < 2 {
		return parsedVersion{}, false
	}
	parts := make([]int, 0, len(fields))
	for _, field := range fields {
		if field == "" {
			return parsedVersion{}, false
		}
		part, err := strconv.Atoi(field)
		if err != nil || part < 0 {
			return parsedVersion{}, false
		}
		parts = append(parts, part)
	}
	return parsedVersion{Parts: parts, Prerelease: prerelease}, true
}

func comparePrerelease(left, right string) int {
	if left == right {
		return 0
	}
	if left == "" {
		return 1
	}
	if right == "" {
		return -1
	}
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	maxParts := len(leftParts)
	if len(rightParts) > maxParts {
		maxParts = len(rightParts)
	}
	for index := 0; index < maxParts; index++ {
		if index >= len(leftParts) {
			return -1
		}
		if index >= len(rightParts) {
			return 1
		}
		leftNumber, leftErr := strconv.Atoi(leftParts[index])
		rightNumber, rightErr := strconv.Atoi(rightParts[index])
		switch {
		case leftErr == nil && rightErr == nil && leftNumber != rightNumber:
			if leftNumber > rightNumber {
				return 1
			}
			return -1
		case leftErr == nil && rightErr != nil:
			return -1
		case leftErr != nil && rightErr == nil:
			return 1
		case leftParts[index] > rightParts[index]:
			return 1
		case leftParts[index] < rightParts[index]:
			return -1
		}
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstContentLine(value string) string {
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#-* "))
		if line != "" {
			return truncateText(line, 500)
		}
	}
	return ""
}

func normalizeID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Map(func(char rune) rune {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			return char
		}
		return '-'
	}, value)
	return strings.Trim(value, "-")
}

func truncateText(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:limit])) + "…"
}
