// Command jumpdb 申请数据库运维会话，并为本地数据库客户端提供 TCP 隧道。
//
// 目前处于开发阶段，命令尚未实现。
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "jumpdb: 开发中，命令尚未实现")
	os.Exit(1)
}
