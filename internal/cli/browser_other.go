//go:build !windows

package cli

import (
	"os/exec"
	"runtime"
)

// openURL 用系统默认浏览器打开地址：macOS 使用 open，其他类 Unix 系统使用 xdg-open。
func openURL(address string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", address).Run()
	}
	return exec.Command("xdg-open", address).Run()
}
