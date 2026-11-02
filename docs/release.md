# CI 与发布

修改工作流、分发行为或 `install.sh` 前先读本文。

## 工作流

CI 与发布共用 `.github/workflows/ci.yml`，新增的验证应放在这个共用流程中。

- **检查**（`ci.yml`）：PR、推送 `main`、手动运行时触发，也被发布流程复用。在 `macos-15`（arm64）和 `macos-15-intel`（amd64）原生 runner 上分别确认运行架构，执行全量测试、`race`、`vet`、`build` 及 `scripts/smoke_cli.py`，通过后打包。临时构建产物保留 7 天。
- **版本注入**：只有标签触发的运行才用 ldflags 把标签名注入 `internal/cli.version`，PR 与 `main` 构建保持 `dev`。标签名必须匹配 `^v[0-9A-Za-z.+-]+$`，构建后核对 `jumpdb version` 输出的版本与架构。
- **发布**（`release.yml`）：推送 `v*` 标签触发。两个架构的检查全部通过后，直接使用本次检查的压缩包，不重新构建；`install.sh` 从同一提交检出。先创建草稿并上传全部资产，成功后再公开。标签含 `-`（如 `v0.1.0-rc.1`）时标为预发布。
- **权限**：检查 job 只有 `contents: read`；只有发布 job 有 `contents: write`，使用内置 `GITHUB_TOKEN`，无需个人 PAT。合成验证不使用业务凭据，CI 只运行合成门户和本机回环服务。
- **Actions 版本**：官方 Actions 固定完整提交 SHA，注释标注版本号。升级时核对官方版本与完整 SHA。

首次使用前需在仓库设置中启用 GitHub Actions，并确保仓库或组织策略允许所用的官方 Actions 及发布 job 的写权限。若上传或公开失败，可能留下草稿：先人工检查 Release 及资产，再决定清理并重跑。流程不会覆盖已有的同标签 Release，也不使用 `--clobber`。

## Release 资产

| 资产 | 说明 |
|---|---|
| `jumpdb-darwin-arm64.tar.gz`、`jumpdb-darwin-amd64.tar.gz` | 仅含可执行文件 `jumpdb` 与 `README.md`，不含配置、测试样本或凭据 |
| `SHA256SUMS` | 两个压缩包的 SHA-256；只能发现下载损坏，不能证明来源 |
| `install.sh` | 一键安装脚本，不列入 `SHA256SUMS` |

Release 说明由 `release.yml` 生成，给出固定到本标签的安装命令，并链接到该标签的 README，不复制完整教程。产物没有 Developer ID 签名和公证，可能被 Gatekeeper 或企业策略拦截；不宣称 Go 二进制天然免拦截，签名与公证需要另外的分发身份。

## install.sh 维护

- 默认通过 `releases/latest/download/` 下载，GitHub 只把最新**稳定版**重定向到这里，预发布版必须用 `JUMPDB_VERSION` 指定。
- 仓库路径 `zK0G0w/jumpdb` 写死在脚本中；仓库改名或迁移时需同步修改脚本、README 和 smoke 中的地址。
- 行为约束：不使用 `sudo`，不修改 shell 配置，不读取或改动 jumpdb 配置，不移除隔离属性（不执行 `xattr`），不提供可改下载来源的开关；安装先写同目录临时文件再原子替换。
- 压缩包的文件名或内容结构变化时，`install.sh` 与 smoke 中的 `make_release()` 需一起修改。
- 修改后按 [development.md](development.md#smoke_clipy) 运行 smoke。

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
