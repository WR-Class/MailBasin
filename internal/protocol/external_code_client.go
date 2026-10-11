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
	return parseExternalCodeBody(body)
}

// parseExternalCodeBody 解析取码响应，优先用规范形状，再退回顶层 code。
func parseExternalCodeBody(body []byte) (ExternalCodeResult, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ExternalCodeResult{}, errCode("external_code_empty_response", "取码接口返回了空响应", true)
	}
	var envelope externalCodeEnvelope
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
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

// looksLikeVerificationCode 判断一个字符串是否像验证码：纯数字或纯字母数字，长度 4 到 10。
func looksLikeVerificationCode(value string) bool {
	if len(value) < 4 || len(value) > 10 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') {
			return false
		}
	}
	return true
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
