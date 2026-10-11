//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// readPasswordNoEcho 关闭控制台回显后读取一行，避免新密码留在终端回滚缓冲里。
// 第二个返回值表示回显是否真的被关掉了；调用方据此决定要不要提示用户。
func readPasswordNoEcho(file *os.File) (string, bool, error) {
	handle := windows.Handle(file.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		// 不是控制台（被重定向、被管道接管等），退回普通读取。
		value, err := readLinePlain(file)
		return value, false, err
	}
	if err := windows.SetConsoleMode(handle, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
		value, err := readLinePlain(file)
		return value, false, err
	}
	value, err := readLinePlain(file)
	// 无论读取是否成功都要把控制台恢复原状，否则后续输出会一直不可见。
	_ = windows.SetConsoleMode(handle, mode)
	return value, true, err
}
