package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func withStdin(t *testing.T, input string) {
	t.Helper()
	original := stdinReader
	t.Cleanup(func() { stdinReader = original })
	stdinReader = bufio.NewReader(strings.NewReader(input))
}

// 回归：bufio.Reader 一次会读入多行，如果每次读取都新建实例，
// 第二次读取就会拿到 EOF——密码要读两次，这条路径必然踩到。
func TestReadLinePlainReadsConsecutiveLines(t *testing.T) {
	withStdin(t, "first-secret\nsecond-secret\n")
	first, err := readLinePlain(os.Stdin)
	if err != nil {
		t.Fatalf("第一次读取失败：%v", err)
	}
	second, err := readLinePlain(os.Stdin)
	if err != nil {
		t.Fatalf("第二次读取失败：%v", err)
	}
	if first != "first-secret" || second != "second-secret" {
		t.Fatalf("读取结果不正确：%q / %q", first, second)
	}
}

// Windows 下管道输入带 CRLF，行尾的 \r 不能被算进密码里。
func TestReadLinePlainStripsCRLF(t *testing.T) {
	withStdin(t, "secret-with-crlf\r\n")
	value, err := readLinePlain(os.Stdin)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if value != "secret-with-crlf" {
		t.Fatalf("行尾 \\r 未被去掉：%q", value)
	}
}

// 最后一行没有换行符时也要能读出来。
func TestReadLinePlainHandlesMissingTrailingNewline(t *testing.T) {
	withStdin(t, "no-trailing-newline")
	value, err := readLinePlain(os.Stdin)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if value != "no-trailing-newline" {
		t.Fatalf("结果不正确：%q", value)
	}
}

func TestPromptNewPasswordAcceptsMatchingInput(t *testing.T) {
	withStdin(t, "a-good-password\na-good-password\n")
	value, err := promptNewPassword()
	if err != nil {
		t.Fatalf("应接受一致的新密码：%v", err)
	}
	if value != "a-good-password" {
		t.Fatalf("密码不正确：%q", value)
	}
}

func TestPromptNewPasswordRejectsMismatch(t *testing.T) {
	withStdin(t, "a-good-password\na-different-one\n")
	if _, err := promptNewPassword(); err == nil {
		t.Fatal("两次输入不一致时应报错")
	}
}

func TestPromptNewPasswordRejectsTooShort(t *testing.T) {
	withStdin(t, "short\nshort\n")
	if _, err := promptNewPassword(); err == nil {
		t.Fatal("少于 8 位时应报错")
	}
}

// 管道读完就 EOF，必须报错而不是拿空密码去重置。
func TestPromptNewPasswordFailsOnEOF(t *testing.T) {
	withStdin(t, "")
	if _, err := promptNewPassword(); err == nil {
		t.Fatal("输入为空时应报错")
	}
}
