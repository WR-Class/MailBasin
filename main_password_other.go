//go:build !windows

package main

import "os"

// readPasswordNoEcho 在非 Windows 平台上不关闭回显，如实返回 false，
// 由调用方提示用户输入可见。这里刻意不引入平台相关的 termios 调用：
// 该服务的实际部署目标是 Windows，为其余平台维护一份不常用的系统调用得不偿失。
func readPasswordNoEcho(file *os.File) (string, bool, error) {
	value, err := readLinePlain(file)
	return value, false, err
}
