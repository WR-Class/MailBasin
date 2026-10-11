package protocol

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Go 的 http.DefaultTransport 只认 HTTP_PROXY / HTTPS_PROXY 环境变量，
// 完全不读 Windows 的 WinINET 系统代理设置。
//
// 这不是一个理论问题：在 Google 被直连阻断、只能走本地代理的网络里，
// 浏览器和 PowerShell 都能连上，而本服务连不上，报错只是一句裸的
// dial tcp <ipv6>:443: connectex 超时——看起来像代码 bug，实际是路由问题。
// 微软的端点通常可以直连，所以这个问题只在 Google 这条路上暴露出来。
//
// 因此这里按「显式配置 → 环境变量 → 系统代理」的顺序解析。
func ResolveProxy(explicit string) string {
	if value := strings.TrimSpace(explicit); value != "" {
		return value
	}
	if value := firstNonEmpty(os.Getenv("HTTPS_PROXY"), os.Getenv("https_proxy")); value != "" {
		return value
	}
	if value := firstNonEmpty(os.Getenv("HTTP_PROXY"), os.Getenv("http_proxy")); value != "" {
		return value
	}
	return systemProxy()
}

// systemProxy 读取操作系统的代理设置，结果缓存一次。
// 读注册表要起一个 reg 进程，不能每次发请求都做一遍。
func systemProxy() string {
	systemProxyOnce.Do(func() { systemProxyValue = readSystemProxy() })
	return systemProxyValue
}

var (
	systemProxyOnce  sync.Once
	systemProxyValue string
)

// readSystemProxy 读取操作系统的代理设置。目前只实现了 Windows（WinINET）。
func readSystemProxy() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	const key = `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	enabled, err := exec.Command("reg", "query", key, "/v", "ProxyEnable").Output()
	if err != nil || !strings.Contains(string(enabled), "0x1") {
		return ""
	}
	out, err := exec.Command("reg", "query", key, "/v", "ProxyServer").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return ""
	}
	value := fields[len(fields)-1]
	// 形如 "http=host:port;https=host:port"，优先取 https 那一项。
	if strings.Contains(value, "=") {
		for _, part := range strings.Split(value, ";") {
			if strings.HasPrefix(strings.ToLower(part), "https=") {
				return "http://" + part[len("https="):]
			}
		}
		return ""
	}
	return "http://" + value
}

// NewHTTPClient 构造一个走代理（若解析到）的 HTTP 客户端。
// 代理地址非法时退回直连，而不是让整个模块不可用。
func NewHTTPClient(explicitProxy string, timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	proxyURL := ResolveProxy(explicitProxy)
	if proxyURL == "" {
		return client
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" {
		return client
	}
	client.Transport = &http.Transport{Proxy: http.ProxyURL(parsed)}
	return client
}

// ProxyDescription 返回当前生效的代理，用于在界面上显示诊断信息。
func ProxyDescription(explicit string) string {
	if value := ResolveProxy(explicit); value != "" {
		return value
	}
	return ""
}
