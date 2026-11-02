// Command jumpdb 申请数据库运维会话，并为本地数据库客户端提供 TCP 隧道。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/zK0G0w/jumpdb/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
