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
	if code, _, _ := ErrorDetails(err); code != "external_code_unparsable" {
		t.Fatalf("错误码应为 external_code_unparsable，实际 %q", code)
	}
}
