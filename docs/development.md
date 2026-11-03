# 开发与测试

## 环境

- Go 1.26 或更高版本。
- Python 3 仅用于 smoke 和兼容性向量脚本，CLI 运行本身不依赖 Python。

## 代码组织

| 目录 | 职责 |
|---|---|
| `cmd/jumpdb` | 入口：信号处理与退出码 |
| `internal/cli` | `token set`、`connect`、`ui`、`version` 及帮助；不处理数据库协议 |
| `internal/config` | 权限受限的本地配置、资源列表、资源 ID 解析 |
| `internal/session` | 门户时间探测、会话申请、会话激活、HTTPS 信任链 |
| `internal/secdb` | 连接参数解码（AES-128-CBC） |
| `internal/tunnel` | 本机 TCP 隧道、代理握手与转发 |
| `internal/ui` | 本地 Web 控制页（`go:embed` 原生前端） |
| `scripts/install.sh` | 用户安装脚本，作为 Release 资产分发 |

CLI 与协议逻辑分开；核心优先使用 Go 标准库，只为实际需求引入依赖。

## 常用命令

```bash
go build ./...
go test ./...
go test -race ./...
go vet ./...

# 两种 macOS 架构；交叉编译成功不等于目标机运行通过
GOOS=darwin GOARCH=arm64 go build -o /tmp/jumpdb-darwin-arm64 ./cmd/jumpdb
GOOS=darwin GOARCH=amd64 go build -o /tmp/jumpdb-darwin-amd64 ./cmd/jumpdb

# 实际二进制的合成 smoke；只能在本机架构上运行
python3 -I -B scripts/smoke_cli.py /tmp/jumpdb-darwin-arm64
```

本地构建的 `jumpdb version` 显示 `dev`。需要模拟发布版本时，与 CI 一样用 ldflags 注入：

```bash
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-X github.com/zK0G0w/jumpdb/internal/cli.version=v0.1.0" \
  -o /tmp/jumpdb ./cmd/jumpdb
```

本地按 CI 参数打包的方法见 [release.md](release.md#本地打包)。

## 测试原则

- 测试一律使用合成密钥和合成数据，不读取用户的真实配置；门户地址、密钥、Token 及真实会话数据不得进入源码、测试或提交历史。
- 针对消费者可见的行为测试，不测试源码文本或简单转发。
- 合成 HTTP 门户与 HTTP 地址只通过测试代码注入；生产代码不提供跳过证书校验或接受 HTTP 的开关。`install.sh` 同理，smoke 用 `PATH` 中的假 `curl` 提供发布资产，脚本本身不提供可改下载来源的开关。
- 加解密回环不能单独证明兼容性；与旧实现的兼容性以向量为准（见下文）。

## smoke_cli.py

`scripts/smoke_cli.py` 用临时 HOME、伪终端和回环服务运行实际二进制，覆盖：

- 隐藏录入、更新、重新录入，取消录入时不保存及终端状态恢复；
- 参数错误、非终端输入拒绝、端口占用、门户探测失败、`SIGINT` 与端口重绑；
- `ui --no-open` 的令牌与 `Host` 校验及 `SIGINT` 退出；
- `install.sh`：以 UTF-8 locale 运行（macOS `/bin/sh` 在该 locale 下会把 `$var` 后紧跟的全角字符并入变量名，C locale 不暴露），语法检查，首次安装、覆盖不支持 `version` 的旧版、指定版本、自定义目录，校验和不一致或新文件无法运行时不安装、保留原文件且不留临时文件，安装不支持 `version` 的旧版时标为版本未知，`PATH` 缺失提示（含尾斜杠与符号链接条目不误判）与冲突警告，非法参数与不支持的架构。

脚本中的合成门户只触发 TLS 失败或等待，不证明 HTTPS 会话成功；完整的会话申请与代理转发由 Go 集成测试覆盖。

`install.sh` 以 `/bin/sh` 运行；修改后可另用 `dash -n` 或把 smoke 中的解释器换成 `/bin/dash` 检查 POSIX 兼容性。本机有 `shellcheck` 时也应运行一遍。

## 兼容性向量

`internal/secdb` 与旧版 Python 实现的兼容性向量由 `scripts/gen_secdb_vectors.py` 生成，必须先经旧 Python 实现校验：

- venv 建在 `/tmp`，用 `python -I -B` 运行，不在旧项目目录写入任何文件；
- 与旧版的有意差异写在 `DecodeSSOSecDB` 文档注释及该脚本中。

旧项目的位置与阅读方式见 [legacy-reference.md](legacy-reference.md)。
