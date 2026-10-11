package auth

import (
	"strings"
	"testing"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	encoded, err := HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatalf("生成密码散列失败：%v", err)
	}
	if !VerifyPassword("correct-horse-battery", encoded) {
		t.Fatal("正确密码应校验通过")
	}
	if VerifyPassword("wrong-password-here", encoded) {
		t.Fatal("错误密码不应校验通过")
	}
	if VerifyPassword("", encoded) {
		t.Fatal("空密码不应校验通过")
	}
}

// 格式必须与登录校验一致，否则离线重置写进去的散列会永远登不进去。
func TestHashPasswordUsesExpectedFormatAndSalt(t *testing.T) {
	first, err := HashPassword("same-password-twice")
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword("same-password-twice")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("相同密码两次散列应因随机盐而不同")
	}
	parts := strings.Split(first, "$")
	if len(parts) != 4 {
		t.Fatalf("散列格式不正确：%q", first)
	}
	if parts[0] != passwordHashVersion {
		t.Fatalf("散列版本应为 %s，实际 %q", passwordHashVersion, parts[0])
	}
	if parts[1] != "120000" {
		t.Fatalf("迭代次数应为 120000，实际 %q", parts[1])
	}
	// 两份散列都必须能校验同一个密码。
	if !VerifyPassword("same-password-twice", first) || !VerifyPassword("same-password-twice", second) {
		t.Fatal("带不同盐的散列都应校验通过")
	}
}

func TestVerifyPasswordRejectsMalformedEncoded(t *testing.T) {
	cases := []struct {
		name    string
		encoded string
	}{
		{"空字符串", ""},
		{"字段数不足", "pbkdf2_sha256$120000$salt"},
		{"字段数过多", "pbkdf2_sha256$120000$salt$key$extra"},
		{"版本不匹配", "bcrypt$120000$c2FsdA$a2V5"},
		{"迭代次数非法", "pbkdf2_sha256$abc$c2FsdA$a2V5"},
		{"盐不是合法 base64", "pbkdf2_sha256$120000$!!!$a2V5"},
		{"密钥不是合法 base64", "pbkdf2_sha256$120000$c2FsdA$!!!"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if VerifyPassword("anything", item.encoded) {
				t.Fatalf("畸形散列不应校验通过：%q", item.encoded)
			}
		})
	}
}

// 命令行必须套用和网页端相同的规则，否则会出现"这里能设、那里登不进"的密码。
func TestValidateCredentialsMatchesLoginRules(t *testing.T) {
	valid := []struct {
		username string
		password string
	}{
		{"admin", "12345678"},
		{"a.b_c-d@example.com", "a-very-long-password"},
		{"Admin", "八位以上密码也可以"},
	}
	for _, item := range valid {
		if err := ValidateCredentials(item.username, item.password); err != nil {
			t.Fatalf("应通过校验：%q / %q -> %v", item.username, item.password, err)
		}
	}
	invalid := []struct {
		name     string
		username string
		password string
	}{
		{"密码过短", "admin", "1234567"},
		{"密码为空", "admin", ""},
		{"账号过短", "ab", "12345678"},
		{"账号含非法字符", "ad min", "12345678"},
		{"账号以非法字符开头", "-admin", "12345678"},
	}
	for _, item := range invalid {
		t.Run(item.name, func(t *testing.T) {
			if err := ValidateCredentials(item.username, item.password); err == nil {
				t.Fatalf("应拒绝：%q / %q", item.username, item.password)
			}
		})
	}
}

// 账号大小写不敏感是登录侧的行为，校验侧必须保持一致。
func TestValidateCredentialsNormalizesUsername(t *testing.T) {
	if err := ValidateCredentials("  Admin  ", "12345678"); err != nil {
		t.Fatalf("大小写与空白应被归一化后通过：%v", err)
	}
}
