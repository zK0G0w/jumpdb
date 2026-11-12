//go:build windows

package config

import "os"

// ProtectionNote 描述配置文件当前平台的保护方式，用于终端提示。
const ProtectionNote = "依赖用户目录的访问控制"

// privateEnough 在 Windows 上恒为 true：Go 在 Windows 只能报告 0666/0444，无法表达 ACL，
// 权限位检查不可用。保护依赖默认配置目录（用户主目录下）继承的访问控制，jumpdb 不修改 ACL。
func privateEnough(os.FileInfo) bool {
	return true
}
