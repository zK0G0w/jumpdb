//go:build windows

package cli

import "os"

// waitReadable 在 Windows 上直接进入读取：控制台处于 raw 模式时 Ctrl+C 作为 0x03 字符交给
// term.Terminal 处理并返回 EOF，由 readSecret 转为取消并恢复控制台模式，不需要轮询。
func waitReadable(*os.File) (bool, error) {
	return true, nil
}
