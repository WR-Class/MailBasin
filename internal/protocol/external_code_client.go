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
)

// ExternalCodeMaxBody 是取码接口响应的读取上限，避免一个失控的端点把内存吃满。
const ExternalCodeMaxBody = 256 * 1024

// ExternalCodeResult 是一次外部取码的结果。
//
// Found 为 false 表示接口可达、只是还没有新验证码——这是正常状态，不是错误。
// 把「没有码」和「取不到」分开，是为了让一个失效的令牌不会一直伪装成「还没收到」。
type ExternalCodeResult struct {
	Found      bool
	Code       string
	Email      string
	Subject    string
	From       string
	ReceivedAt time.Time
	MessageID  string
}

// externalCodeEnvelope 覆盖两种响应形状。
//
// 规范形状来自本项目自己的公开取码 API：{"success":true,"data":{...CodeResult...}}。
// 但只依赖一种形状太脆——同源的第三方部署可能只回一个顶层 code，
// 所以 Code 同时承载「成功时的验证码」和「失败时的错误标识」（如 no_code）。
type externalCodeEnvelope struct {
	Success   bool   `json:"success"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Data      *struct {
		Code       string `json:"code"`
		Email      string `json:"email"`
		Subject    string `json:"subject"`
		From       string `json:"from"`
		ReceivedAt string `json:"received_at"`
		MessageID  string `json:"message_id"`
	} `json:"data"`
}

// externalCodeErrorCodes 是已知的「非验证码」code 取值。
// 出现在这里说明接口没给出码，而不是给了一个长得像码的字符串。
var externalCodeErrorCodes = map[string]bool{
	"no_code":             true,
	"not_found":           true,
	"mailbox_not_found":   true,
	"unauthorized":        true,
	"invalid_token":       true,
	"public_api_disabled": true,
	"mail_sync_failed":    true,
	"rate_limited":        true,
	"invalid_key":         true,
	"forbidden":           true,
}

// FetchExternalCode 向外部取码接口要一次验证码。
func FetchExternalCode(ctx context.Context, client *http.Client, endpoint string) (ExternalCodeResult, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ExternalCodeResult{}, errCode("external_code_url_missing", "缺少取码接口地址", false)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ExternalCodeResult{}, errCode("external_code_url_invalid", "取码接口地址不是合法的 http(s) 地址", false)
	}
	if client == nil {
		client = NewHTTPClient("", 30*time.Second)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return ExternalCodeResult{}, errCode("external_code_request_failed", "构造取码请求失败："+err.Error(), true)
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return ExternalCodeResult{}, errCode("external_code_unreachable", "取码接口不可达："+err.Error(), true)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, ExternalCodeMaxBody))
	if err != nil {
		return ExternalCodeResult{}, errCode("external_code_read_failed", "读取取码响应失败："+err.Error(), true)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ExternalCodeResult{}, errCode("external_code_unauthorized",
			fmt.Sprintf("取码令牌被拒绝（HTTP %d），请重新获取该邮箱的取码地址", response.StatusCode), false)
	}
	if response.StatusCode == http.StatusNotFound {
		return ExternalCodeResult{}, errCode("external_code_not_found",
			"取码接口返回 404，邮箱地址或访问令牌可能已失效", false)
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return ExternalCodeResult{}, errCode("external_code_rate_limited", "取码接口限流，稍后重试", true)
	}
	if response.StatusCode >= 400 {
		return ExternalCodeResult{}, errCode("external_code_http_error",
			fmt.Sprintf("取码接口返回 HTTP %d", response.StatusCode), response.StatusCode >= 500)
	}
	return parseExternalCodeBodyWithType(body, response.Header.Get("Content-Type"))
}

// isHTMLContentType 判断 Content-Type 是不是网页。
func isHTMLContentType(contentType string) bool {
	lowered := strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(lowered, "text/html") || strings.HasPrefix(lowered, "application/xhtml")
}

// parseExternalCodeBody 解析取码响应，优先用规范形状，再退回顶层 code。
func parseExternalCodeBody(body []byte) (ExternalCodeResult, error) {
	return parseExternalCodeBodyWithType(body, "")
}

// parseExternalCodeBodyWithType 与 parseExternalCodeBody 相同，但知道响应的 Content-Type。
//
// contentType 只用来排除一种情况：响应其实是个网页。网页绝不参与「从自由文本里抓
// 第一个数字串」这条兜底——那条规则是为纯文本验证码写的，用在网页上只会把页面里的
// 定时器常量当验证码交出去，而且表现得像一次成功。
func parseExternalCodeBodyWithType(body []byte, contentType string) (ExternalCodeResult, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ExternalCodeResult{}, errCode("external_code_empty_response", "取码接口返回了空响应", true)
	}
	var envelope externalCodeEnvelope
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		// 走到这里说明不是 JSON。先分清「这是个网页」和「这是纯文本验证码」——
		// 两者的处理完全不同，而网页里的数字几乎总是陷阱。
		if isHTMLContentType(contentType) || looksLikeHTMLResponse(trimmed) {
			return ExternalCodeResult{}, errCode("external_code_html_response",
				"取码接口返回的是网页而不是验证码，请在设置里改成一个返回 JSON 或纯文本的取码地址（当前地址可能指向了页面而不是接口）", false)
		}
		// 有些端点直接回纯文本验证码，这也是可用的。
		if code := firstVerificationCode(trimmed); code != "" {
			return ExternalCodeResult{Found: true, Code: code}, nil
		}
		return ExternalCodeResult{}, errCode("external_code_unparsable",
			"取码接口返回的内容既不是 JSON 也不含验证码", true)
	}
	// 规范形状：success 为真且 data 里有码。
	if envelope.Data != nil {
		code := strings.TrimSpace(envelope.Data.Code)
		if code != "" && !externalCodeErrorCodes[strings.ToLower(code)] {
			result := ExternalCodeResult{
				Found:     true,
				Code:      code,
				Email:     strings.TrimSpace(envelope.Data.Email),
				Subject:   strings.TrimSpace(envelope.Data.Subject),
				From:      strings.TrimSpace(envelope.Data.From),
				MessageID: strings.TrimSpace(envelope.Data.MessageID),
			}
			if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(envelope.Data.ReceivedAt)); err == nil {
				result.ReceivedAt = parsed
			}
			return result, nil
		}
	}
	// 退回形状：顶层 code 直接就是验证码。
	code := strings.TrimSpace(envelope.Code)
	if code == "" || externalCodeErrorCodes[strings.ToLower(code)] {
		return ExternalCodeResult{Found: false}, nil
	}
	if !looksLikeVerificationCode(code) {
		// 顶层 code 是错误标识但我们不认识——宁可如实报告，
		// 也不要把它当成验证码交给使用者。
		return ExternalCodeResult{}, errCode("external_code_error_response",
			firstNonEmpty(envelope.Message, "取码接口返回了未识别的错误："+code), envelope.Retryable)
	}
	return ExternalCodeResult{Found: true, Code: code}, nil
}

// externalCodeStatusWords 是「长得像验证码、其实是状态词」的取值。
//
// 这些词全部是 4~10 位纯字母，正好落在 looksLikeVerificationCode 的长度与字符集
// 窗口里：success、pending、done、failed、expired……一个只看字符集的判定会把它们
// 当成验证码交给使用者，而界面会显示「取到码了」。这类错误比「取不到」严重得多：
// 使用者拿一个假码去注册，会一直卡在验证失败，根本想不到是取码工具骗了他。
var externalCodeStatusWords = map[string]bool{
	"success":    true,
	"successful": true,
	"pending":    true,
	"done":       true,
	"failed":     true,
	"failure":    true,
	"error":      true,
	"errors":     true,
	"true":       true,
	"false":      true,
	"null":       true,
	"none":       true,
	"nil":        true,
	"ok":         true,
	"expired":    true,
	"loading":    true,
	"waiting":    true,
	"unknown":    true,
	"empty":      true,
	"retry":      true,
	"timeout":    true,
	"queued":     true,
	"ready":      true,
}

// looksLikeVerificationCode 判断一个字符串是否像验证码。
//
// 除了长度 4 到 10、只含字母数字之外，还要求**至少含一个数字**：真实验证码几乎
// 总是数字或字母加数字，而状态词几乎总是纯字母。同时显式排除已知状态词，兜住
// 将来出现的、含数字的状态词（例如 http2xx 这类）。
//
// 这条收紧是必要的：原来的实现只看字符集，于是 "success"、"pending"、"2026"
// 全部通过——前两个是状态词，第三个是年份。宁可如实报告「这个响应里没有码」，
// 也不能把状态词交给使用者去注册。
func looksLikeVerificationCode(value string) bool {
	if len(value) < 4 || len(value) > 10 {
		return false
	}
	hasDigit := false
	for _, char := range value {
		switch {
		case char >= '0' && char <= '9':
			hasDigit = true
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		default:
			return false
		}
	}
	if !hasDigit {
		return false
	}
	return !externalCodeStatusWords[strings.ToLower(value)]
}

// looksLikeHTMLResponse 判断响应正文是不是一个网页。
//
// 取码接口应该回 JSON 或纯文本，但配置错了、地址写成了管理页、或者上游把错误页
// 直接吐回来时，会拿到 HTML。HTML 里到处是数字陷阱：轮询间隔、定时器常量、
// 版本号、内联脚本里的毫秒数。这些数字一旦落进 firstVerificationCode，就会变成
// 一个看起来完全合法、实际毫无意义的验证码。
func looksLikeHTMLResponse(body string) bool {
	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	lowered := strings.ToLower(strings.TrimSpace(head))
	return strings.HasPrefix(lowered, "<!doctype") ||
		strings.HasPrefix(lowered, "<html") ||
		strings.HasPrefix(lowered, "<?xml") ||
		strings.Contains(lowered, "<head") ||
		strings.Contains(lowered, "<body")
}

// firstVerificationCode 从一段自由文本里抓出第一个连续 4 到 8 位数字。
func firstVerificationCode(text string) string {
	current := make([]rune, 0, 8)
	for _, char := range text {
		if char >= '0' && char <= '9' {
			current = append(current, char)
			continue
		}
		if len(current) >= 4 && len(current) <= 8 {
			return string(current)
		}
		current = current[:0]
	}
	if len(current) >= 4 && len(current) <= 8 {
		return string(current)
	}
	return ""
}
