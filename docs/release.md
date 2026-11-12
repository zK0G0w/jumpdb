# CI 与发布

修改工作流、分发行为或 `install.sh` 前先读本文。

## 工作流

CI 与发布共用 `.github/workflows/ci.yml`，新增的验证应放在这个共用流程中。

- **检查**（`ci.yml`）：PR、推送 `main`、手动运行时触发，也被发布流程复用。在 `macos-15`（arm64）和 `macos-15-intel`（amd64）原生 runner 上分别确认运行架构，执行全量测试、`race`、`vet`、`build` 及 `scripts/smoke_cli.py`，通过后打包。临时构建产物保留 7 天。
- **Windows 检查**（`ci.yml` 的 `check-windows`）：在 `windows-2025`（amd64）和 `windows-11-arm`（arm64）原生 runner 上确认运行架构，执行全量测试、`vet`、`build`，核对 `jumpdb version` 并运行 `scripts/smoke_install_windows.ps1`，然后打包为 zip。`race` 仅在 amd64 运行（Go 不支持 `windows/arm64` 的 `-race`）。不运行 `smoke_cli.py`：它依赖 `/bin/sh`、假 `curl` 与 macOS 行为。
- **版本注入**：只有标签触发的运行才用 ldflags 把标签名注入 `internal/cli.version`，PR 与 `main` 构建保持 `dev`。标签名必须匹配 `^v[0-9A-Za-z.+-]+$`，构建后核对 `jumpdb version` 输出的版本与架构。
- **发布**（`release.yml`）：推送 `v*` 标签触发。macOS 与 Windows 共四个架构的检查全部通过后，直接使用本次检查的压缩包，不重新构建；`install.sh` 与 `install.ps1` 从同一提交检出。先创建草稿并上传全部资产，成功后再公开。标签含 `-`（如 `v0.1.0-rc.1`）时标为预发布。
- **权限**：检查 job 只有 `contents: read`；只有发布 job 有 `contents: write`，使用内置 `GITHUB_TOKEN`，无需个人 PAT。合成验证不使用业务凭据，CI 只运行合成门户和本机回环服务。
- **Actions 版本**：官方 Actions 固定完整提交 SHA，注释标注版本号。升级时核对官方版本与完整 SHA。

首次使用前需在仓库设置中启用 GitHub Actions，并确保仓库或组织策略允许所用的官方 Actions 及发布 job 的写权限。若上传或公开失败，可能留下草稿：先人工检查 Release 及资产，再决定清理并重跑。流程不会覆盖已有的同标签 Release，也不使用 `--clobber`。

## Release 资产

| 资产 | 说明 |
|---|---|
| `jumpdb-darwin-arm64.tar.gz`、`jumpdb-darwin-amd64.tar.gz` | 仅含可执行文件 `jumpdb` 与 `README.md`，不含配置、测试样本或凭据 |
| `jumpdb-windows-amd64.zip`、`jumpdb-windows-arm64.zip` | 仅含 `jumpdb.exe` 与 `README.md`；无 `install.sh` 对应物，用户手动解压并配置 PATH |
| `SHA256SUMS` | 四个压缩包的 SHA-256；只能发现下载损坏，不能证明来源 |
| `install.sh` | macOS 一键安装脚本，不列入 `SHA256SUMS` |
| `install.ps1` | Windows 一键安装脚本，不列入 `SHA256SUMS` |

Release 说明由 `release.yml` 生成，给出固定到本标签的安装命令，并链接到该标签的 README，不复制完整教程。macOS 产物没有 Developer ID 签名和公证，可能被 Gatekeeper 或企业策略拦截，Windows 产物没有代码签名，可能被 SmartScreen 拦截；不宣称 Go 二进制天然免拦截，签名与公证需要另外的分发身份。

## install.sh 维护

- 默认通过 `releases/latest/download/` 下载，GitHub 只把最新**稳定版**重定向到这里，预发布版必须用 `JUMPDB_VERSION` 指定。
- 仓库路径 `zK0G0w/jumpdb` 写死在脚本中；仓库改名或迁移时需同步修改脚本、README 和 smoke 中的地址。
- 行为约束：不使用 `sudo`，不修改 shell 配置，不读取或改动 jumpdb 配置，不移除隔离属性（不执行 `xattr`），不提供可改下载来源的开关；安装先写同目录临时文件再原子替换。
- 压缩包的文件名或内容结构变化时，`install.sh` 与 smoke 中的 `make_release()` 需一起修改。
- 修改后按 [development.md](development.md#smoke_clipy) 运行 smoke。

## install.ps1 维护

- 与 `install.sh` 相同：默认 `latest`（只指向最新稳定版），`JUMPDB_VERSION` 指定版本，仓库路径写死，不提供可改下载来源的开关；先写同目录临时文件，确认能运行后再替换。
- 默认目录 `%LOCALAPPDATA%\Programs\jumpdb`，`JUMPDB_INSTALL_DIR` 覆盖。与 `install.sh` 不同，它会把安装目录追加到**当前用户**的 `PATH`（直接写 `HKCU\Environment`，保留值类型与未展开的 `%VAR%` 条目），不修改系统 `PATH`，不需要管理员权限，不执行 `Unblock-File`。
- 必须兼容 Windows PowerShell 5.1：不用 `??`、三元运算符、`&&` 等 PowerShell 7 语法；运行原生命令时临时放宽 `$ErrorActionPreference`。
- 文件保存为**无 BOM** 的 UTF-8。Release 资产响应没有 charset，Windows PowerShell 5.1 的 `irm` 按 ISO-8859-1 解码且忽略 BOM（CI run `43142975259` 实测：BOM 变成 `ï»¿` 与首行 `#` 连成命令名，中文乱码，安装失败）。所以安装命令写成 `iex ([Text.Encoding]::UTF8.GetString((iwr -useb <地址>).Content))`，README、Release 说明与 smoke 必须一致，不能改回 `irm | iex`。smoke 会拒绝带 BOM 的 `install.ps1`。
- 通过 `iex` 执行时与用户共用会话：逻辑全部放在 `& { }` 脚本块内，失败时用 `$LASTEXITCODE` 而不是 `exit`，避免关闭用户的终端。
- 运行中的 `jumpdb.exe` 不能覆盖但可以改名：先把旧文件改名为 `.jumpdb.old.<PID>.exe`，删不掉时留到下次安装清理。
- 压缩包的文件名或内容结构变化时，`install.ps1` 与 smoke 中的 `New-Release` 需一起修改。

## 发布步骤

标签推送会触发远端发布，必须由维护者另行授权后执行：

```bash
git tag v0.1.0
git push origin v0.1.0
```

建议先发布预发布标签（如 `v0.1.0-rc.1`），确认发布 job 成功、资产齐全，并用 `JUMPDB_VERSION` 实际执行一次安装；正式版发布后再验证不指定版本的 `latest` 路径。

## 本地打包

与 CI 打包参数一致，产物写入被 Git 忽略的 `dist/`。本地构建不能代替 Release，也只能在本机架构上运行 smoke。

```bash
rm -rf dist && mkdir -p dist
for arch in arm64 amd64; do
  mkdir -p "dist/$arch"
  CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -o "dist/$arch/jumpdb" ./cmd/jumpdb
  cp README.md "dist/$arch/README.md"
  COPYFILE_DISABLE=1 tar -czf "dist/jumpdb-darwin-$arch.tar.gz" -C "dist/$arch" jumpdb README.md
done
(cd dist && shasum -a 256 jumpdb-darwin-arm64.tar.gz jumpdb-darwin-amd64.tar.gz > SHA256SUMS)
```

## 证据类型

工作流静态校验（如 `actionlint`）、本机执行工作流命令、GitHub 原生双架构运行与 Release 上传是不同层级的证据，按实际运行结果报告，不能互相替代。当前的验证状态见 [verification.md](verification.md)。

Runner 架构与标签依据 [GitHub 官方 runner 镜像清单](https://github.com/actions/runner-images#available-images)，发布参数依据 [GitHub CLI 的 Release 文档](https://cli.github.com/manual/gh_release_create)。
