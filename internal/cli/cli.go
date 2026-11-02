// Package cli 提供配置录入和前台隧道命令，不处理数据库协议。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"

	"github.com/zK0G0w/jumpdb/internal/config"
	"github.com/zK0G0w/jumpdb/internal/secdb"
	"github.com/zK0G0w/jumpdb/internal/session"
	"github.com/zK0G0w/jumpdb/internal/tunnel"
	"golang.org/x/term"
)

const usage = `用法：
  jumpdb token set [--configure]
  jumpdb connect --id ID [--id-type auto|string|number] [--port PORT] [--show-password]

token set：首次隐藏录入门户、两把 16 字节原始密钥及 Token；以后仅更新 Token。
  --configure      显式重新录入全部配置；不迁移旧版配置
connect：固定监听 127.0.0.1，Ctrl+C 停止并释放连接。
  --id             必填；自动将无前导零的十进制非负整数作为数字，其余作为字符串
  --id-type        默认 auto；string 保留数字字符串，number 强制 int64 非负整数
  --port           默认 9011，范围 1..65535
  --show-password  显式在终端显示初始会话密码；不复制到剪贴板

监听、会话申请和代理握手分别报告；握手成功不代表数据库登录成功。
每个客户端连接重新申请会话；任一连接建立失败会停止整个隧道并退出 1。
退出码：0 正常结束或取消；1 运行失败；2 参数错误。
`

type sessionClient interface {
	Login(context.Context, session.ResourceID) (secdb.Params, error)
	Activate(context.Context, string) error
}

type application struct {
	out            io.Writer
	errOut         io.Writer
	path           func() (string, error)
	prompt         func(context.Context, string) (string, error)
	inputTerminal  bool
	outputTerminal bool
	proxyPort      int
	newSession     func(context.Context, session.Options) (sessionClient, error)
}

// Run 执行命令并返回退出码；ctx 取消时释放监听器和活跃连接。
func Run(ctx context.Context, args []string, input, output, errorOutput *os.File) int {
	app := application{
		out: output, errOut: errorOutput, path: config.DefaultPath,
		inputTerminal: term.IsTerminal(int(input.Fd())), outputTerminal: term.IsTerminal(int(output.Fd())),
		prompt: func(ctx context.Context, label string) (string, error) {
			return readSecret(ctx, input, errorOutput, label)
		},
		newSession: func(ctx context.Context, options session.Options) (sessionClient, error) {
			return session.New(ctx, options)
		},
	}
	return app.run(ctx, args)
}

func (app *application) run(ctx context.Context, args []string) int {
	if len(args) == 0 || len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Fprint(app.out, usage)
		return 0
	}
	var err error
	switch args[0] {
	case "token":
		if len(args) < 2 || args[1] != "set" {
			return app.argumentError()
		}
		flags := newFlags("token set")
		configure := flags.Bool("configure", false, "")
		if parseErr := flags.Parse(args[2:]); parseErr != nil {
			return app.flagError(parseErr)
		}
		if flags.NArg() != 0 {
			return app.argumentError()
		}
		err = app.setToken(ctx, *configure)
	case "connect":
		flags := newFlags("connect")
		id := flags.String("id", "", "")
		idType := flags.String("id-type", "auto", "")
		port := flags.Int("port", 9011, "")
		showPassword := flags.Bool("show-password", false, "")
		if parseErr := flags.Parse(args[1:]); parseErr != nil {
			return app.flagError(parseErr)
		}
		resource, parseErr := config.ParseResourceID(*id, *idType)
		if flags.NArg() != 0 || parseErr != nil || *port < 1 || *port > 65535 {
			return app.argumentError()
		}
		err = app.connect(ctx, resource, *port, *showPassword)
	default:
		return app.argumentError()
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		fmt.Fprintln(app.out, "已取消，监听器及活跃连接已关闭；未完成的录入不会保存。")
		return 0
	}
	if err != nil {
		fmt.Fprintln(app.errOut, "jumpdb:", err)
		return 1
	}
	return 0
}

func newFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func (app *application) flagError(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(app.out, usage)
		return 0
	}
	return app.argumentError()
}

func (app *application) argumentError() int {
	fmt.Fprintln(app.errOut, "jumpdb: 参数错误；请运行 jumpdb --help。参数值不会回显。")
	return 2
}

func (app *application) setToken(ctx context.Context, configure bool) error {
	if !app.inputTerminal {
		return errors.New("token set 需要交互终端，拒绝从管道或文件读取凭据")
	}
	path, err := app.path()
	if err != nil {
		return err
	}
	_, err = config.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		configure = true
	} else if err != nil {
		return err
	}
	var entered config.Data
	type promptField struct {
		label string
		value *string
	}
	var fields []promptField
	if configure {
		fields = append(fields,
			promptField{"门户 HTTPS 地址（隐藏输入）：", &entered.Portal},
			promptField{"传输密钥（原始 16 字节，隐藏输入）：", &entered.TransKey},
			promptField{"数据库密钥（原始 16 字节，隐藏输入）：", &entered.DBKey},
		)
	}
	fields = append(fields, promptField{"Token（仅值，隐藏输入）：", &entered.Token})
	for _, field := range fields {
		value, err := app.prompt(ctx, field.label)
		if err != nil {
			return err
		}
		*field.value = value
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// 录入可能持续较久，保存前重新读取磁盘，只改录入的字段，避免覆盖 Web 控制页写入的资源列表。
	if _, err := config.Update(path, func(data *config.Data) error {
		if configure {
			data.Portal, data.TransKey, data.DBKey = entered.Portal, entered.TransKey, entered.DBKey
		}
		data.Token = entered.Token
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintln(app.out, "本地配置已保存（0600）；未验证 Token 有效性。")
	return nil
}

func (app *application) connect(ctx context.Context, resource session.ResourceID, port int, showPassword bool) error {
	if showPassword && !app.outputTerminal {
		return errors.New("--show-password 仅允许输出到交互终端，拒绝重定向密码")
	}
	path, err := app.path()
	if err != nil {
		return err
	}
	data, err := config.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("尚未配置，请先运行 jumpdb token set")
	}
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return errors.New("无法监听本地端口，请检查端口是否已被占用")
	}
	defer listener.Close()
	fmt.Fprintf(app.out, "本地正在监听：127.0.0.1:%d（尚未申请会话或连接代理）\n", port)
	key, _ := secdb.ParseKey(data.DBKey)
	client, err := app.newSession(ctx, session.Options{Portal: data.Portal, Token: data.Token, DBKey: key})
	if err != nil {
		return fmt.Errorf("门户时间探测失败：%s", sessionError(err))
	}
	params, err := client.Login(ctx, resource)
	if err != nil {
		return fmt.Errorf("初始会话申请失败：%s", sessionError(err))
	}
	if params.Type != "MySQL" {
		return errors.New("初始会话的数据库类型不是 MySQL，首版不支持连接")
	}
	fmt.Fprintf(app.out, "初始会话申请成功；数据库类型：%q；账号：%q\n", params.Type, params.Username)
	if showPassword {
		fmt.Fprintf(app.out, "初始会话密码（引号内为 Go 转义表示）：%q\n", params.Password)
	}
	fmt.Fprintln(app.out, "等待客户端连接；每条连接另行申请会话，凭据可能变化。代理握手成功不代表数据库登录成功。Ctrl+C 停止。")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mutex sync.Mutex
	var failure error
	err = tunnel.Serve(ctx, listener, tunnel.Config{
		ProxyPort: app.proxyPort,
		Provider:  func(ctx context.Context) (secdb.Params, error) { return client.Login(ctx, resource) },
		// 每条连接的新会话都需激活后再握手；初始会话不建隧道，无需激活。
		Activate: func(ctx context.Context, params secdb.Params) error { return client.Activate(ctx, params.SessionID) },
		OnEvent: func(event tunnel.Event) {
			mutex.Lock()
			defer mutex.Unlock()
			if ctx.Err() != nil {
				return
			}
			// 终端只报告连接建立的各阶段；连接结束等观测事件留给 Web 控制页。
			stage, ok := map[tunnel.Stage]string{tunnel.StageSession: "客户端会话申请", tunnel.StageDial: "代理连接", tunnel.StageActivate: "会话激活", tunnel.StageHandshake: "代理握手"}[event.Stage]
			if !ok {
				return
			}
			if !event.Success {
				failure = fmt.Errorf("%s失败，已停止隧道并关闭活跃连接", stage)
				if event.Stage == tunnel.StageSession || event.Stage == tunnel.StageActivate {
					failure = fmt.Errorf("%w；请检查 Token、资源权限、门户连接及数据库密钥", failure)
				}
				cancel()
				return
			}
			fmt.Fprintln(app.out, stage+"成功")
		},
	})
	if failure != nil {
		return failure
	}
	if err != nil {
		return errors.New("本地隧道停止：监听失败")
	}
	return nil
}

func sessionError(err error) string {
	switch {
	case errors.Is(err, session.ErrTLS):
		return "TLS 证书校验失败，请检查证书链、主机名及有效期"
	case errors.Is(err, session.ErrBusiness):
		return "门户拒绝请求，请检查 Token、资源 ID 和权限；不会自动续期或重试"
	case errors.Is(err, session.ErrHTTP):
		return "HTTP 状态错误，请检查门户可用性及授权"
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时"
	case errors.Is(err, secdb.ErrDecrypt), errors.Is(err, secdb.ErrFormat), errors.Is(err, secdb.ErrPayload):
		return "会话解码失败，请检查数据库密钥及响应格式"
	case errors.Is(err, session.ErrResponse):
		return "门户响应格式无效"
	default:
		return "请求失败，请检查网络及本地配置"
	}
}
