//go:build windows

package cli

import "os/exec"

// openURL 用系统默认浏览器打开地址；不经过 cmd.exe，避免地址中的特殊字符被解释。
func openURL(address string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", address).Run()
}
