package protocol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestPhoneSecurityCodeAccepts412AfterSMSWasSent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"trustedPhoneNumbers":[{"id":7,"nonFTEU":true,"pushMode":"sms","lastTwoDigits":"41"}]}`))
	}))
	defer server.Close()

	client := &AppleAuthClient{httpClient: server.Client()}
	session := &appleAuthSession{
		Endpoints: appleAuthEndpoints{Auth: server.URL},
		UserAgent: appleAuthUserAgent,
	}
	if err := client.requestPhoneSecurityCode(context.Background(), session, nil); err != nil {
		t.Fatalf("412 短信发送结果不应被判定为失败：%v", err)
	}
	if !strings.Contains(string(session.TwoFactorPhone), `"id":7`) {
		t.Fatalf("未保存 Apple 返回的受信任手机号：%s", session.TwoFactorPhone)
	}
}

func TestRequestPhoneSecurityCodeRejects412WithoutPhoneDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"error":"precondition_failed"}`))
	}))
	defer server.Close()

	client := &AppleAuthClient{httpClient: server.Client()}
	session := &appleAuthSession{Endpoints: appleAuthEndpoints{Auth: server.URL}}
	if err := client.requestPhoneSecurityCode(context.Background(), session, nil); err == nil {
		t.Fatal("缺少受信任手机号信息的 412 响应不应被当作短信已发送")
	}
}
