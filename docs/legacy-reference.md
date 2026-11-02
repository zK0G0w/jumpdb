# 旧 Python 项目参考

旧项目是维护者本机上的 Python 版 `ssotool-mac` 目录，不随本仓库发布，路径需向维护者确认。旧项目可能有多份副本，以维护者指定的最新版本为准；已核对至 v1.4（含 `CHANGELOG.md`），协议行为的变化先看其更新记录。

## 使用规则

- 旧目录只读：所有新实现写入当前仓库，不修改、删除旧项目，也不在其中写入文件（包括 venv、缓存）。
- 协议密钥只在实现解密时从源码核对，不在文档、日志或聊天中抄录。
- 旧 README 含明文敏感信息，不整体复制到新仓库；其中的旧绝对路径、口令及“永不过期”等描述不能沿用。
- 不调用旧 Python 进程，也不解析其 stdout。

## 按职责阅读

以下文件均相对于旧项目目录。

| 实现任务 | 必读源码与关注点 |
|---|---|
| 会话请求、激活、时钟校准、错误处理 | `yab_flow.py`：`BASE`、`LOGIN_API`、`make_headers()`、`login_resource()`、`signed_post()`、`check_sso_tool()`、`main()` 中的首次请求、`get_session()` 和 `activate()` |
| 解密及字段语义 | `ssotool_mac.py`：`KEY_TRANS`、`KEY_DB`、`unscramble()`、`decrypt_db_field()`、`parse_params()`；HTTP `data.db` 与 `ssosecdb` 两种格式的 IV 重排不同 |
| 代理握手、激活时机、每连接申请会话、TCP 转发 | `local_agent.py`：`open_tunnel()`、`TunnelHandler`、`start_tunnel()`；参考协议行为，不照搬资源管理缺陷。v1.4 中未被调用的 `report_ready()`（`ssoToolReport`）关闭了主机名校验且接口已移除，不移植 |
| HTTPS 信任链 | `certs/ca-bundle.pem`、`certs/intermediate.pem`：门户不下发中间证书，只需嵌入公开的中间证书，与系统根证书一起校验，不嵌入整包根证书 |
| 现有人工操作流程 | `README.md`（注意上文的敏感信息规则） |

## 源码层面已确定的事实

- `ssotool_mac.py` 的旧 `--tunnel` 调用传入结果字典，而当前 `local_agent.start_tunnel()` 要求会话回调，位置参数含义也已改变，因此它不是有效的移植参考入口；有效入口是 `yab_flow.py --tunnel`。
- 旧版会把口令、会话等信息输出到终端并写入剪贴板；新版本只在显式 `--show-password` 时于终端显示，不实现剪贴板复制。
- 旧版握手后保留 10 秒 socket 超时、v1.2 之前的粘包误判，以及握手长度字节写死 `\x04` 的问题，见 [verification.md](verification.md#排障结论)。
