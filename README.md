# jumpdb

`jumpdb` 是一个 macOS 命令行工具：复用你在网页登录后取得的授权 Token 申请数据库运维会话，并在本机 `127.0.0.1` 开放一个 TCP 隧道。DBX、Navicat、DBeaver、DataGrip、`mysql` 命令行等客户端连接这个本地端口即可访问数据库。

> **状态**：MySQL 主路径已在真实环境用 DBX 跑通；其他客户端、长时间空闲、长查询等尚未验证，见[已知限制](#已知限制)。

## 声明

本项目仅用于研究和学习网络协议、AES-CBC 数据格式、TCP 转发及 Go CLI 实践。它不是专门针对阿里云堡垒机或任何特定厂商堡垒机产品的逆向工程项目，也不提供破解、绕过鉴权或越权访问的能力。仓库不包含真实门户地址、协议密钥、账号、Token 或会话数据。只能在自己已获授权的环境和账号上使用，并遵守所在组织的安全策略及相关法律法规；因使用不当造成的后果由使用者自行承担。

## 安装

支持 Apple Silicon（arm64）和 Intel（amd64）的 macOS，运行时不依赖 Python 或其他程序。

### 一键安装（推荐）

```bash
curl -fsSL https://github.com/zK0G0w/jumpdb/releases/latest/download/install.sh | sh
```

脚本会识别本机架构，下载最新稳定版并按 `SHA256SUMS` 校验，然后安装到 `~/.local/bin/jumpdb`。它不使用 `sudo`，不修改 `~/.zshrc` 等配置文件，也不读取或改动 jumpdb 的配置。可以用环境变量调整：

```bash
# 安装指定版本（包括预发布版）
curl -fsSL https://github.com/zK0G0w/jumpdb/releases/latest/download/install.sh | JUMPDB_VERSION=v0.1.0 sh

# 安装到其他目录（必须是绝对路径，当前用户需有写权限；脚本不会使用 sudo）
curl -fsSL https://github.com/zK0G0w/jumpdb/releases/latest/download/install.sh | JUMPDB_INSTALL_DIR="$HOME/bin" sh
```

如果提示安装目录不在 `PATH` 中，在 `~/.zshrc` 末尾加上下面一行，然后重新打开终端：

```bash
export PATH="$HOME/.local/bin:$PATH"
```

想先审阅脚本再执行，可以先下载：

```bash
curl -fsSLO https://github.com/zK0G0w/jumpdb/releases/latest/download/install.sh
less install.sh
sh install.sh
```

安装后确认：

```bash
jumpdb --version
jumpdb --help
```

### 手动安装

在 [Releases](https://github.com/zK0G0w/jumpdb/releases) 页面下载与本机架构对应的压缩包和 `SHA256SUMS`，放在同一目录（不确定架构时运行 `uname -m`：`arm64` 对应 arm64，`x86_64` 对应 amd64）：

```bash
shasum -a 256 -c --ignore-missing SHA256SUMS
tar -xzf jumpdb-darwin-arm64.tar.gz      # Intel 改为 jumpdb-darwin-amd64.tar.gz
mkdir -p ~/.local/bin
mv jumpdb ~/.local/bin/
```

然后按上一节把 `~/.local/bin` 加入 `PATH`。

### 首次运行被 macOS 拦截

发布的二进制没有 Developer ID 签名和公证。通过浏览器下载的文件带有隔离属性，首次运行时可能被 macOS 拦截，提示无法验证该文件。如果你信任该文件，可以打开“系统设置 → 隐私与安全性”，在页面下方点击“仍要打开”，然后重新运行。一键安装通过 `curl` 下载，文件不带隔离属性，按 macOS 的机制通常不会出现该提示，但尚未在真实环境中实测。公司管理的设备请按公司策略处理。

`SHA256SUMS` 与压缩包来自同一 Release，只能发现下载损坏，不能证明来源。

### 升级与卸载

- **升级**：重新运行一键安装命令。脚本先校验，再原子替换已安装的文件，并显示新旧版本；配置不受影响，无需重新录入。手动安装的话，重复手动安装的步骤即可。
- **卸载**：删除二进制文件即可，例如 `rm ~/.local/bin/jumpdb`。配置目录 `~/.config/jumpdb/` 中保存着 Token 和密钥，卸载不会自动删除它，请自行决定是否执行 `rm -rf ~/.config/jumpdb`。

## 快速开始

1. **准备信息**：在网页正常登录，取得授权 Token 和要连接的资源 ID。门户地址和两把协议密钥向维护者获取。不要把这些信息贴进命令行参数、聊天或日志。
2. **录入配置**：

   ```bash
   jumpdb token set
   ```

   第一次运行会隐藏录入门户地址、两把密钥和 Token；以后再运行只更新 Token。
3. **启动隧道**，二选一：

   ```bash
   jumpdb connect --id 856      # 终端方式，默认监听 127.0.0.1:9011
   jumpdb ui                    # 网页控制页，可保存常用资源并同时运行多个隧道
   ```

4. **连接数据库**：在客户端中新建 MySQL 连接，主机填 `127.0.0.1`，端口填 `9011`（或你指定的端口），账号使用 `connect` 输出中显示的账号。需要查看初始会话的密码时，`connect` 加上 `--show-password`，或在控制页点击“显示密码”。
5. **停止**：在运行 `jumpdb` 的终端按 `Ctrl+C`，所有连接会关闭，端口也会释放。

各命令的完整参数用 `jumpdb <命令> --help` 查看，例如 `jumpdb connect --help`。

## 工作原理

1. `connect` 先绑定本地端口，再申请一次初始会话，显示数据库类型和账号。此时还没有连接代理。
2. 数据库客户端每建立一条连接，`jumpdb` 都会另外申请一次运维会话并解密连接参数，不复用初始会话。
3. 连上代理后，`jumpdb` 先激活该会话，再完成握手，之后在客户端和代理之间双向转发字节。数据库的认证和查询仍由客户端完成。

## 使用说明

### 配置文件

- 位置默认是 `~/.config/jumpdb/config.json`。如果 `$XDG_CONFIG_HOME` 是绝对路径，则使用 `$XDG_CONFIG_HOME/jumpdb/config.json`。
- 目录权限为 `0700`，文件权限为 `0600`，每次保存都先写临时文件再原子替换。它是**权限受限的明文文件**，不是加密保险库，请像保护其他凭据一样保护本机及备份。
- 门户必须是 HTTPS 根地址。两把密钥按原始字符串录入，各恰好 16 字节，不是十六进制或 Base64。Token 只填值，不含 Cookie 名称。
- 只能在交互终端中录入，不接受参数、管道或文件输入。录入中途按 `Ctrl+C` 不会保存任何内容。保存时不验证 Token 是否有效。
- 配置损坏、权限不是 `0600` 或文件是符号链接时，`jumpdb` 会报错，不会自动重置。请先检查、修复或移走该文件。需要重新录入全部字段时，运行 `jumpdb token set --configure`。
- 不会读取或迁移旧工具的 `~/.ssotool_mac_conf.json`，也不再读取早期版本使用的 `~/Library/Application Support/jumpdb/config.json`。如需沿用旧配置，确认新位置没有配置后手动移动过去，并保持目录 `0700`、文件 `0600`。

### 资源 ID

- 默认（`--id-type auto`）把无前导零的十进制非负整数当作数字发送，其他值当作字符串发送，例如 `00856` 保留为字符串。
- `--id-type string` 强制按字符串发送；`--id-type number` 强制按数字发送，并接受前导零。Shell 引号不影响类型，`--id "856"` 与 `--id 856` 相同。

### 连接状态与退出

- `connect` 会逐个报告各阶段：本地正在监听、会话申请成功、代理连接成功、会话激活成功、代理握手成功。这些都**不代表**数据库登录成功，登录结果以客户端为准。
- 任一客户端连接在上述阶段失败时，`connect` 会停止整个隧道并退出。会话申请或激活失败时，`jumpdb` 不推断具体原因，请依次检查 Token、资源权限、网络连接及数据库密钥。
- 退出码：`0` 正常结束或按 `Ctrl+C` 取消，`1` 运行失败，`2` 参数错误。

### Web 控制页

`jumpdb ui` 在 `127.0.0.1` 的随机端口启动一个本地网页，并自动打开浏览器（`--no-open` 只打印地址）。

- 终端打印的地址含本次运行的随机访问令牌，每次运行都不同，请勿分享。关闭浏览器不会停止控制页；在终端按 `Ctrl+C` 才会停止控制页及其所有隧道。
- 页面上的配置语义与 `token set` 相同。密钥和 Token 不回显，只显示“已设置”。修改门户或密钥前需要先停止所有隧道。
- 可以保存常用资源（ID、类型、本地端口、别名），并同时运行多个隧道。某个隧道的连接失败只会停止该隧道，不影响其他隧道。
- 更新 Token 后，下一条新连接就会使用新 Token，已建立的连接不受影响。
- 页面显示每条连接的阶段、时长、实时字节数和结束原因。这些数据只保存在内存中，退出即清空。
- 外观可选跟随系统、浅色或深色，需要 Safari 17.5、Chrome 123、Firefox 120 或更新版本的浏览器。

## 安全说明

- 隧道只监听 `127.0.0.1`，不开放到局域网。它**不是** SSH，也不额外加密流量，不能当作加密隧道使用。
- 与门户的通信使用 HTTPS 并校验证书，没有跳过校验的选项。
- 输出和日志不包含 Token、密钥、会话 ID 或代理地址。参数错误时不会回显参数值。
- 只有显式使用 `--show-password`（且输出是终端）或在控制页点击“显示密码”时才会显示密码，不会复制到剪贴板。终端录屏和滚动历史仍可能留下密码。

## 已知限制

- 仅支持 MySQL；初始会话不是 MySQL 时拒绝连接。
- 真实环境只验证了 DBX（Apple Silicon）。Navicat、`mysql` 命令行等其他客户端，空闲超过 10 秒、长查询、Token 失效提示，以及 Intel 实机都尚未在真实环境验证。
- 不支持自动网页登录、自动获取或续期 Token、开机自启和自动更新。
- 保存过资源列表或非默认主题后，旧版本 `jumpdb` 会拒绝读取该配置。
- 内置的中间证书有效期至 2027-11-02，门户换证时需要更新程序。

完整的验证范围见 [docs/verification.md](docs/verification.md)。

## 开发

- [开发与测试](docs/development.md)：构建、测试、smoke 及兼容性向量。
- [CI 与发布](docs/release.md)：工作流、发布流程及 `install.sh` 维护。
- [协议与实现约束](docs/protocol.md)、[CLI 与控制页行为约束](docs/behavior.md)。
- [验证记录](docs/verification.md)：已验证、未验证项及排障结论。
