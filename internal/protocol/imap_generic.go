package protocol

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"time"
)

// defaultIMAPPort 是标准 IMAPS 端口，QQ、163 和绝大多数自建邮箱都用它。
const defaultIMAPPort = 993

// genericIMAPTimeout 是一次连接 + 登录 + 打开收件箱的总预算。
// 探活要在用户点「导入」时同步返回，所以不能长；真正的收信走同步任务，另有预算。
const genericIMAPTimeout = 25 * time.Second

// IMAPPreset 是一个服务商的连接参数预设。
//
// 做成预设而不是在代码里按厂商分支，是因为 QQ 和 163 走的是同一条协议，
// 唯一的差别就是服务器地址；地址是数据，不该变成控制流。
type IMAPPreset struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
	// Hint 是开启协议、取授权码的操作路径，界面上直接展示给使用者。
	Hint string `json:"hint"`
	// Limit 如实说明该服务商能挂多少个地址，避免使用者以为可以像 Gmail 那样无限加号。
	Limit string `json:"limit"`
}

var imapPresets = []IMAPPreset{
	{
		Key: "qq", Name: "QQ邮箱", Host: "imap.qq.com", Port: 993,
		Hint:  "先在 QQ邮箱「设置 - 帐户」里开启 IMAP/SMTP 服务，按提示短信验证后会得到授权码，密码处填这个授权码（不是 QQ 密码）。账号名必须填完整邮箱地址。",
		Limit: "一个 QQ 邮箱最多 10 个别名地址，且别名要先去邮箱里绑定；不支持 + 标签，加号地址会被拒收。",
	},
	{
		Key: "163", Name: "163邮箱", Host: "imap.163.com", Port: 993,
		Hint:  "先在 163邮箱「设置 - POP/SMTP/IMAP」里开启 IMAP 服务，短信验证后会生成 16 位客户端授权码，密码处填这个授权码。",
		Limit: "163 免费邮箱一个账号只有一个地址，没有别名；不支持 + 标签，加号地址会被退信。",
	},
	{
		Key: "custom", Name: "自定义 / 自建", Host: "", Port: 993,
		Hint:  "填自建或其它服务商的 IMAP 地址。自建邮箱可以自由配置无限多的收件地址，是替代 + 标签最彻底的办法。",
		Limit: "能挂多少个地址取决于你自己的邮件服务器配置。",
	},
}

// IMAPPresets 返回全部预设，供界面渲染下拉框。
func IMAPPresets() []IMAPPreset {
	out := make([]IMAPPreset, len(imapPresets))
	copy(out, imapPresets)
	return out
}

// IMAPPresetByKey 按 key 找预设，找不到时返回自定义预设。
func IMAPPresetByKey(key string) IMAPPreset {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, preset := range imapPresets {
		if preset.Key == key {
			return preset
		}
	}
	return imapPresets[len(imapPresets)-1]
}

// dialGenericIMAPTLS 连接非 iCloud 的 IMAP 服务器。
//
// 这里刻意不走 iCloud 那套 DNS：iCloud 的解析器写死了 1.1.1.1 / 8.8.8.8 的
// DNS-over-TCP，还带一组 iCloud 专用 IP 兜底。QQ 和 163 都是国内服务，
// 用那套解析器会绕开系统解析器去打境外 DNS，在没有代理的网络里直接失败。
// 所以非 iCloud 主机一律用系统 DNS 和普通 TCP。
func dialGenericIMAPTLS(ctx context.Context, host string, port int) (net.Conn, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, errCode("imap_host_missing", "缺少 IMAP 服务器地址", false)
	}
	if port <= 0 {
		port = defaultIMAPPort
	}
	dialer := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 15 * time.Second},
		Config: &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		},
	}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, errCode("imap_dial_failed", "连接 IMAP 服务器失败："+err.Error(), true)
	}
	return conn, nil
}

// VerifyGenericIMAP 连一次服务器并完成登录 + 打开收件箱，用来在导入时判断凭据是否可用。
//
// 只做「能不能读信」这一件事：不发信、不改动任何邮件，连上后直接退出。
func VerifyGenericIMAP(ctx context.Context, host string, port int, username, secret string) error {
	username, secret = strings.TrimSpace(username), strings.TrimSpace(secret)
	if username == "" {
		return errCode("imap_username_missing", "缺少 IMAP 账号名", false)
	}
	if secret == "" {
		return errCode("imap_secret_missing", "缺少客户端授权码", false)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, genericIMAPTimeout)
	defer cancel()

	conn, err := dialGenericIMAPTLS(ctx, host, port)
	if err != nil {
		return err
	}
	defer conn.Close()
	// ctx 到点时主动断连，让阻塞中的读写立即返回，而不是干等 TCP 超时。
	stopClose := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopClose:
		}
	}()
	defer close(stopClose)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		return errCode("imap_greeting_failed", "读取 IMAP 欢迎信息失败："+err.Error(), true)
	}
	if !strings.Contains(strings.ToUpper(greeting), "OK") {
		return errCode("imap_greeting_failed", "IMAP 服务器未就绪："+imapResponseSummary([]string{greeting}), true)
	}
	loginLines, err := imapCommand(conn, reader, "A001", "LOGIN "+imapQuote(username)+" "+imapQuote(secret))
	if err != nil {
		return errCode("imap_login_failed", "IMAP 登录请求失败："+err.Error(), true)
	}
	if !imapTaggedOK(loginLines, "A001") {
		return errCode("imap_login_failed", "IMAP 登录失败，请确认账号名填的是完整邮箱地址、密码填的是客户端授权码："+imapResponseSummary(loginLines), false)
	}
	selectLines, err := imapCommand(conn, reader, "A002", "SELECT INBOX")
	if err != nil {
		return errCode("imap_select_failed", "打开收件箱失败："+err.Error(), true)
	}
	if !imapTaggedOK(selectLines, "A002") {
		return errCode("imap_select_failed", "打开收件箱失败："+imapResponseSummary(selectLines), true)
	}
	_, _ = imapCommand(conn, reader, "A003", "LOGOUT")
	return nil
}

// IMAPHostIsICloud 判断一个 IMAP 主机是不是 iCloud 自己的。
// 只有 iCloud 才该走那套专用 DNS 和 IP 兜底。
func IMAPHostIsICloud(host string) bool {
	return strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(host), "."), defaultICloudIMAPHost)
}
