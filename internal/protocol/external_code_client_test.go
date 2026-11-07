package protocol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fetchFromServer(t *testing.T, handler http.HandlerFunc) (ExternalCodeResult, error) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	return FetchExternalCode(context.Background(), server.Client(), server.URL+"/code")
}

// 规范形状来自本项目自己的公开取码 API：{"success":true,"data":{...CodeResult...}}。
// 这是同源第三方部署最可能回的形状，必须优先命中。
func TestFetchExternalCodeParsesCanonicalShape(t *testing.T) {
	result, err := fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"email":       "tippet_crooked_7j@icloud.com",
				"code":        "483920",
				"subject":     "你的验证码",
				"from":        "no-reply@example.com",
				"received_at": "2026-01-01T10:00:00Z",
				"message_id":  "msg-1",
			},
		})
	})
	if err != nil {
		t.Fatalf("解析规范形状失败：%v", err)
	}
	if !result.Found || result.Code != "483920" {
		t.Fatalf("应取到验证码 483920，实际 found=%t code=%q", result.Found, result.Code)
	}
	if result.Subject != "你的验证码" || result.MessageID != "msg-1" {
		t.Fatalf("主题和消息 ID 应一并解析：%+v", result)
	}
	if !result.ReceivedAt.Equal(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("接收时间解析不正确：%v", result.ReceivedAt)
	}
}

// 兜底形状：顶层 code 直接就是验证码。
func TestFetchExternalCodeFallsBackToTopLevelCode(t *testing.T) {
	result, err := fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "code": "112233"})
	})
	if err != nil {
		t.Fatalf("解析顶层 code 失败：%v", err)
	}
	if !result.Found || result.Code != "112233" {
		t.Fatalf("应取到验证码 112233，实际 found=%t code=%q", result.Found, result.Code)
	}
}

// 这是真实抓到的响应：接口可达，只是还没有新验证码。
// 必须是「没找到」而不是错误——否则一个正常状态会被报成故障。
func TestFetchExternalCodeTreatsNoCodeAsNotAnError(t *testing.T) {
	result, err := fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":   false,
			"code":      "no_code",
			"message":   "暂未收到验证码",
			"retryable": true,
		})
	})
	if err != nil {
		t.Fatalf("「暂未收到验证码」不应被当成错误：%v", err)
	}
	if result.Found || result.Code != "" {
		t.Fatalf("不应取到验证码：%+v", result)
	}
}

// 顶层 code 是错误标识时，绝不能把它当验证码交出去。
func TestFetchExternalCodeNeverReturnsAnErrorIdentifierAsCode(t *testing.T) {
	for _, identifier := range []string{"no_code", "unauthorized", "invalid_token", "mailbox_not_found"} {
		result, err := fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": identifier})
		})
		if err != nil {
			t.Fatalf("%s 应被当成「没有码」而不是错误：%v", identifier, err)
		}
		if result.Found {
			t.Fatalf("错误标识 %s 被当成了验证码", identifier)
		}
	}
}

// 令牌失效必须和「还没收到码」区分开，否则界面会一直显示「等待中」。
func TestFetchExternalCodeDistinguishesDeadTokenFromNoCode(t *testing.T) {
	_, err := fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"code":"unauthorized"}`))
	})
	if err == nil {
		t.Fatal("401 必须报错，不能当成「还没收到验证码」")
	}
	code, _, retryable := ErrorDetails(err)
	if code != "external_code_unauthorized" {
		t.Fatalf("错误码应为 external_code_unauthorized，实际 %q", code)
	}
	if retryable {
		t.Fatalf("令牌失效不应标成可重试：%q", code)
	}

	_, err = fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if code, _, _ := ErrorDetails(err); code != "external_code_not_found" {
		t.Fatalf("404 的错误码应为 external_code_not_found，实际 %q", code)
	}
}

// 端点直接回纯文本验证码也要能用。
func TestFetchExternalCodeAcceptsPlainTextBody(t *testing.T) {
	result, err := fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("你的验证码是 778899，请勿泄露"))
	})
	if err != nil {
		t.Fatalf("解析纯文本失败：%v", err)
	}
	if !result.Found || result.Code != "778899" {
		t.Fatalf("应从文本里取到 778899，实际 %+v", result)
	}
}

// 未识别的顶层 code 必须如实报错，而不是猜它是验证码。
func TestFetchExternalCodeRejectsUnknownTopLevelCode(t *testing.T) {
	_, err := fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": "some_unknown_failure", "message": "上游故障"})
	})
	if err == nil {
		t.Fatal("未识别的错误标识应报错")
	}
	if code, _, _ := ErrorDetails(err); code != "external_code_error_response" {
		t.Fatalf("错误码应为 external_code_error_response，实际 %q", code)
	}
}

func TestFetchExternalCodeRejectsMalformedURL(t *testing.T) {
	for _, endpoint := range []string{"", "   ", "not-a-url", "ftp://host/code", "https://"} {
		if _, err := FetchExternalCode(context.Background(), nil, endpoint); err == nil {
			t.Fatalf("非法地址应被拒绝：%q", endpoint)
		}
	}
}

func TestExternalCodeHelpers(t *testing.T) {
	if !looksLikeVerificationCode("483920") || !looksLikeVerificationCode("ab12cd") {
		t.Fatal("合法验证码应被接受")
	}
	for _, bad := range []string{"abc", "12345678901", "12-34", "no_code", ""} {
		if looksLikeVerificationCode(bad) {
			t.Fatalf("非法验证码应被拒绝：%q", bad)
		}
	}
	if got := firstVerificationCode("code 483920 end"); got != "483920" {
		t.Fatalf("应抓出 483920，实际 %q", got)
	}
	if got := firstVerificationCode("no digits here"); got != "" {
		t.Fatalf("没有数字时应返回空，实际 %q", got)
	}
}

// 用户实际粘贴的就是 Markdown 表格，必须能直接吃下去。
func TestFetchExternalCodeHandlesNonJSONGarbage(t *testing.T) {
	_, err := fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>Service Unavailable</body></html>"))
	})
	if err == nil {
		t.Fatal("既不是 JSON 也不含验证码时应报错")
	}
	if code, _, _ := ErrorDetails(err); code != "external_code_unparsable" && code != "external_code_html_response" {
		t.Fatalf("错误码应为 external_code_unparsable 或 external_code_html_response，实际 %q", code)
	}
}

// 状态词长得完全符合旧的「4-10 位字母数字」判定，于是被当成验证码交出去。
// 使用者拿 "success" 去注册会一直失败，而界面告诉他「取到码了」。
// 这类错误比取不到严重：它伪装成成功。
func TestFetchExternalCodeNeverReturnsStatusWordAsCode(t *testing.T) {
	for _, word := range []string{
		"success", "successful", "pending", "done", "failed", "failure",
		"error", "errors", "expired", "loading", "waiting", "unknown",
		"true", "false", "null", "none", "nil", "queued", "ready", "timeout", "retry",
	} {
		result, err := fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": word})
		})
		if err != nil {
			// 报错是可以接受的：它至少没有把状态词当码交出去。
			continue
		}
		if result.Found {
			t.Fatalf("状态词 %q 被当成了验证码 %q —— 界面会显示「取到码了」，而它根本不是一个码", word, result.Code)
		}
	}
}

// 纯字母串一律不算验证码：真实验证码几乎总含数字，而状态词几乎总是纯字母。
// 这一条用一个不可能是状态词的纯字母串来断言规则的形状，而不是靠词表兜。
func TestLooksLikeVerificationCodeRejectsAllLetterStrings(t *testing.T) {
	for _, candidate := range []string{"abcxyz", "QWERTY", "bear", "hello"} {
		if looksLikeVerificationCode(candidate) {
			t.Fatalf("纯字母串 %q 不应被当成验证码", candidate)
		}
	}
	// 反方向：含数字的必须仍然通过，否则收紧过头会把真实取码全废掉。
	for _, candidate := range []string{"483920", "778899", "ab12cd", "ABCD12", "9f2a"} {
		if !looksLikeVerificationCode(candidate) {
			t.Fatalf("含数字的 %q 应被接受为验证码", candidate)
		}
	}
}

// 年份、版本号这类纯数字状态值也在旧的误判范围内（旧判定会接受 "2026"）。
// 这一档必须诚实：4-10 位纯数字与真实验证码在形状上无法区分，所以长度窗口内的
// 纯数字只能接受，不能靠收紧判定去排除——真正的防线是「网页不参与抓数字」。
// 可判定的边界只有长度：超出判定窗口的字符串必须被拒。
func TestLooksLikeVerificationCodeRejectsOverlongNumericTokens(t *testing.T) {
	for _, candidate := range []string{"20261018012", "202610180123", "12345678901"} {
		if looksLikeVerificationCode(candidate) {
			t.Fatalf("超出长度窗口的数字串 %q 不应被当成验证码", candidate)
		}
	}
	// 反方向：窗口内的纯数字必须接受。这是取舍，不是遗漏——
	// 真实世界里 6 位纯数字验证码远比 8 位年份日期串常见。
	for _, candidate := range []string{"483920", "20261018"} {
		if !looksLikeVerificationCode(candidate) {
			t.Fatalf("窗口内的数字串 %q 应被接受", candidate)
		}
	}
}

// 这是本次最关键的回归：取码地址配错、或上游把错误页吐回来时，响应是网页。
// 网页里的轮询间隔、定时器常量、版本号全是数字陷阱，旧的纯文本兜底会把
// 第一个 4-8 位数字串当成验证码——表现得像一次成功，实际是假码。
func TestFetchExternalCodeRefusesToMineDigitsFromHTML(t *testing.T) {
	page := `<!DOCTYPE html>
<html><head><title>邮件取码</title>
<script>var POLL_INTERVAL = 5000; setTimeout(refresh, 5000);</script>
</head><body><div id="code">暂无验证码</div>
<p>页面每 5000 毫秒刷新一次</p></body></html>`
	result, err := fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	if err == nil {
		t.Fatalf("网页响应必须报错，不能把页面里的数字当成验证码；实际返回 found=%t code=%q", result.Found, result.Code)
	}
	if result.Code != "" {
		t.Fatalf("网页响应绝不应产出验证码，实际 %q", result.Code)
	}
	if code, _, _ := ErrorDetails(err); code != "external_code_html_response" {
		t.Fatalf("错误码应为 external_code_html_response，实际 %q", code)
	}
}

// Content-Type 说不是 HTML、但正文明显是网页时同样必须拦住。
// 只信 Content-Type 是不够的：配错的端点经常把网页当 text/plain 发。
func TestFetchExternalCodeDetectsHTMLWithoutContentType(t *testing.T) {
	for _, body := range []string{
		"<!DOCTYPE html><html><body>5000</body></html>",
		"<html><head></head><body><p>4294967296</p></body></html>",
		"  \n <body>123456</body>",
	} {
		result, err := fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(body))
		})
		if err == nil {
			t.Fatalf("没有 Content-Type 但正文是网页时也必须报错，否则 %q 会变成假验证码", body)
		}
		if result.Code != "" {
			t.Fatalf("网页正文不应产出验证码，实际 %q", result.Code)
		}
	}
}

// 收紧不能把真正的纯文本取码废掉——这是上一条的对偶断言。
func TestFetchExternalCodeStillAcceptsGenuinePlainTextCode(t *testing.T) {
	result, err := fetchFromServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("your code is 483920"))
	})
	if err != nil {
		t.Fatalf("纯文本取码不应被收紧误伤：%v", err)
	}
	if !result.Found || result.Code != "483920" {
		t.Fatalf("应从纯文本取到 483920，实际 %+v", result)
	}
}
