package protocol

import (
	"context"
	"strings"
	"testing"
	"time"
)

// QQ 和 163 都必须在预设里，且地址正确。
// 这两个地址是使用者要照着填的东西，写错等于功能不可用。
func TestIMAPPresetsCoverQQAnd163(t *testing.T) {
	byKey := make(map[string]IMAPPreset)
	for _, preset := range IMAPPresets() {
		byKey[preset.Key] = preset
	}
	qq, ok := byKey["qq"]
	if !ok || qq.Host != "imap.qq.com" || qq.Port != 993 {
		t.Fatalf("QQ 预设不正确：%+v", qq)
	}
	mail163, ok := byKey["163"]
	if !ok || mail163.Host != "imap.163.com" || mail163.Port != 993 {
		t.Fatalf("163 预设不正确：%+v", mail163)
	}
	// 每个预设都要给出「怎么拿授权码」，否则使用者只能去猜。
	for _, preset := range IMAPPresets() {
		if strings.TrimSpace(preset.Hint) == "" {
			t.Fatalf("预设 %q 缺少获取授权码的说明", preset.Key)
		}
	}
}

// 两个服务商都不支持 + 标签，这一点必须写在预设里。
// 使用者若以为能像 Gmail 那样无限加号，会得到一批收不到信的地址。
func TestIMAPPresetsStateTheNoPlusTagLimit(t *testing.T) {
	for _, key := range []string{"qq", "163"} {
		preset := IMAPPresetByKey(key)
		if !strings.Contains(preset.Limit, "+") {
			t.Fatalf("预设 %q 应说明不支持 + 标签，实际：%q", key, preset.Limit)
		}
	}
}

func TestIMAPPresetByKeyIsCaseInsensitive(t *testing.T) {
	if got := IMAPPresetByKey("  QQ  "); got.Host != "imap.qq.com" {
		t.Fatalf("大小写和空白不该影响预设查找，实际 %+v", got)
	}
}

// 认不出的 key 回落到自定义：猜一个服务商比让使用者自己填地址更糟。
func TestIMAPPresetByKeyUnknownFallsBackToCustom(t *testing.T) {
	got := IMAPPresetByKey("no-such-vendor")
	if got.Key != "custom" || got.Host != "" {
		t.Fatalf("未知 key 应回落到自定义预设，实际 %+v", got)
	}
}

// iCloud 主机要能被认出来，带不带末尾的点都算——
// 这个判断决定了走哪条 DNS 路径，认错就会用错解析器。
func TestIMAPHostIsICloudRecognizesBothForms(t *testing.T) {
	for _, host := range []string{"imap.mail.me.com", "IMAP.MAIL.ME.COM", "imap.mail.me.com."} {
		if !IMAPHostIsICloud(host) {
			t.Fatalf("%q 应被认成 iCloud 主机", host)
		}
	}
	for _, host := range []string{"imap.qq.com", "imap.163.com", "imap.example.com", ""} {
		if IMAPHostIsICloud(host) {
			t.Fatalf("%q 不该被认成 iCloud 主机", host)
		}
	}
}

// 凭据不全时要在拨号前拦住，不能拿空字符串去连。
func TestVerifyGenericIMAPRejectsIncompleteCredentials(t *testing.T) {
	ctx := context.Background()
	if err := VerifyGenericIMAP(ctx, "imap.qq.com", 993, "", "code"); err == nil {
		t.Fatal("缺账号名时应报错")
	} else if code, _, _ := ErrorDetails(err); code != "imap_username_missing" {
		t.Fatalf("错误标识应为 imap_username_missing，实际 %q", code)
	}
	if err := VerifyGenericIMAP(ctx, "imap.qq.com", 993, "12345@qq.com", "  "); err == nil {
		t.Fatal("缺授权码时应报错")
	} else if code, _, _ := ErrorDetails(err); code != "imap_secret_missing" {
		t.Fatalf("错误标识应为 imap_secret_missing，实际 %q", code)
	}
}

// 连不上的主机要给出可识别的拨号错误，而不是伪装成登录失败——
// 这两件事对使用者的下一步动作完全不同。
func TestDialGenericIMAPReportsDialFailureSeparately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 本机 1 号端口不会有 IMAP 服务，连接会立刻被拒。
	_, err := dialGenericIMAPTLS(ctx, "127.0.0.1", 1)
	if err == nil {
		t.Fatal("连不上的主机应返回错误")
	}
	if code, _, _ := ErrorDetails(err); code != "imap_dial_failed" {
		t.Fatalf("错误标识应为 imap_dial_failed，实际 %q（%v）", code, err)
	}
}

// 空主机名要在拨号前就被拒绝，而不是拼出一个 ":993" 去连本机。
func TestDialGenericIMAPRejectsEmptyHost(t *testing.T) {
	_, err := dialGenericIMAPTLS(context.Background(), "   ", 993)
	if err == nil {
		t.Fatal("空主机名应被拒绝")
	}
	if code, _, _ := ErrorDetails(err); code != "imap_host_missing" {
		t.Fatalf("错误标识应为 imap_host_missing，实际 %q", code)
	}
}
