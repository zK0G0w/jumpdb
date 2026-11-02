# jumpdb

将 Python 版堡垒机数据库登录工具重写为轻量 Go CLI：复用用户在网页取得的授权 Token 申请数据库运维会话，解密连接参数，并提供与客户端无关的本地 TCP 隧道。已实现 `token set`、`connect`、`ui`、`version`，MySQL 主路径已在真实环境用 DBX 跑通；平台为 macOS `darwin/arm64`、`darwin/amd64`。

## 硬约束（始终适用）

- **公开仓库**：本仓库在 GitHub 公开。门户地址、两把协议密钥、Token 和任何真实会话数据都不得出现在源码、测试、文档、提交历史或构建产物中；它们由用户在本机录入。测试一律使用合成密钥和合成数据。
- **凭据**：日志、终端输出、错误信息和控制页接口不包含 Token、密钥、会话 ID、代理地址或原始错误；参数错误不回显参数值。
- **TLS**：HTTPS 必须验证证书，不添加跳过校验或 HTTP 生产开关；合成服务只通过测试代码注入。隧道不是加密隧道，不能这样宣传。
- **监听**：隧道与控制页只监听 `127.0.0.1`。
- **范围**：首版不做桌面 GUI、DMG、自动更新、后台常驻服务（launchd、开机自启）、浏览器协议注册（含 `ssosecdb://`）、自动网页登录、自动获取或续期 Token、门户资源列表同步及其他数据库适配。`jumpdb ui` 是前台运行的本地控制页，不属于上述范围。
- **依赖**：运行时不依赖 Python、Windows 客户端或外部脚本。核心优先使用 Go 标准库，只为实际需求引入依赖；CLI 与协议逻辑分开，不预建多厂商插件框架，不调用旧 Python 进程。客户端不限品牌，不依赖 Navicat 或 DBX 的私有接口。
- **授权**：连接真实资源、推送标签（会触发发布）需维护者另行授权。
- **证据**：按实际运行结果报告。合成或回环通过不能写成真实登录成功，一种客户端通过不能写成所有客户端已验证，交叉编译成功不等于目标机运行通过。多客户端兼容不等于多数据库类型支持，驱动、认证及 TLS 兼容性以实测为准。
- **旧项目**：旧 Python 项目只读，不修改、不写入。
- **语言**：沟通、文档和提交说明使用中文；标识符、技术术语和命令保留英文。

## 按任务阅读

| 任务 | 先读 |
|---|---|
| 修改会话申请、解密、握手、激活、转发、TLS | [docs/protocol.md](docs/protocol.md)、[docs/legacy-reference.md](docs/legacy-reference.md) |
| 修改 CLI 参数、输出、配置、控制页 | [docs/behavior.md](docs/behavior.md) |
| 构建、测试、smoke、兼容性向量 | [docs/development.md](docs/development.md) |
| 修改工作流、分发、`install.sh`，或准备发布 | [docs/release.md](docs/release.md) |
| 判断某项是否已验证、排查真实环境问题、真实联调 | [docs/verification.md](docs/verification.md) |
| 面向用户的安装与使用说明 | [README.md](README.md) |

## 完成前

- 运行 `go test ./...`、`go test -race ./...`、`go vet ./...` 和 `go build ./...`；涉及 CLI 或 `install.sh` 时，再构建二进制并运行 `python3 -I -B scripts/smoke_cli.py <二进制>`。
- 针对消费者可见行为测试，不测试源码文本或简单转发。
- 按已实现的事实更新对应文档，尤其是 [docs/verification.md](docs/verification.md)；不把规划写成现成功能。
- 先完成 Go 版主路径及证据，再切换日常入口；不在同一仓库保留两套长期生产实现。
