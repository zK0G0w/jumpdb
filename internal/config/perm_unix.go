//go:build unix

package config

import "os"

// ProtectionNote 描述配置文件当前平台的保护方式，用于终端提示。
const ProtectionNote = "0600"

// privateEnough 要求文件权限恰好为 0600，拒绝同组或其他用户可访问的凭据文件。
func privateEnough(info os.FileInfo) bool {
	return info.Mode().Perm() == 0600
}
