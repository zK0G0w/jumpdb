# 协议与实现约束

修改 `internal/session`、`internal/secdb`、`internal/tunnel` 前先读本文。具体算法与错误码以旧源码为准（见 [legacy-reference.md](legacy-reference.md)），不要凭经验重写。当前验证状态见 [verification.md](verification.md)。

## 1. 会话申请

- 门户地址来自用户本地配置（真实值与旧源码 `BASE` 一致，但不写入本仓库）。
- 请求为 `POST /3.0/hostOpsService/loginResource`，请求体结构为 `{"c":{},"b":{"ids":[资源ID]}}`。
- 保留数字与字符串 ID 的语义；携带 `YAB_AUTH_TOKEN` Cookie、校准后的毫秒时间戳和 nonce。
- 先以空资源列表探测门户时间，之后每次申请都更新时钟偏差。与旧版首次申请不同，不对业务码 `44` 自动重试。

## 2. 解密

- AES-128-CBC、PKCS7；正确还原 IV 和 Base64 密文，严格检查长度及 padding。
- 错误可诊断，但不泄露密钥、解出的口令或 Token。
- HTTP 响应 `data.db` 与 `ssosecdb` 两种格式的 IV 重排不同，两者的解析兼容性都要保留，但不因此扩展 CLI 功能。
- HTTP 会话主路径只使用 DB 密钥；传输密钥仅保存在配置中。
- 与旧版的有意差异写在 `DecodeSSOSecDB` 文档注释和 `scripts/gen_secdb_vectors.py` 中；向量生成方法见 [development.md](development.md#兼容性向量)。

## 3. 会话刷新

每个客户端 TCP 连接都重新调用 `loginResource`，使用该次返回的 `yxzIp`、`sessionId` 和用户名建立代理连接。这不等于 Token 自动续期，也不保证已有连接永不过期。

## 4. 代理握手

- 连接 `yxzIp:33306`，发送 `b"yabP" + sessionId + bytes([len(username)]) + username`，收到 `success` 后开始双向转发。
- 中间单字节是用户名的**字节长度**，不是分隔符（`root` 恰好为 `0x04`）。写死 `\x04` 会让非 4 字节用户名的多余字节混入数据库流，表现为握手和 greeting 正常，但登录包无响应。用户名超过 255 字节时拒绝握手。
- 处理 TCP 分片和粘包：精确读取 7 字节 `success`，不能假设一次读取就是完整响应，也不能吞掉紧随其后的数据库协议字节。

## 5. 会话激活

- 每个客户端连接在代理连接建立之后、发送握手之前的一刻调用 `POST /3.0/hostOpsService/checkSSOTool`，请求体 `{"c":{},"b":{"sessionId":...}}`，签名与时间校准同 `loginResource`，以 `code == 0` 判定成功。
- 部分资源强制激活：未激活时握手仍返回 `success`，但不发送 greeting。激活与握手之间不得插入其他门户请求。
- 激活失败按阶段失败处理并停止隧道。初始会话不建隧道，也不激活。
- 依据：维护者提供的旧版 v1.4 更新记录及源码；修复长度字节并接入激活后，真实资源已登录成功。

## 6. 转发与退出

- 代理连接与握手可设超时；进入转发前清除握手期限，正常空闲和长查询不能被固定的短超时切断。
- 统一管理监听器、活跃 socket 和取消信号，保证停止后资源全部释放。
- 不把数据库协议错误或转发阶段的断开误报为会话、连接、激活或握手阶段的失败。

## 7. TLS 与凭据

- HTTPS 必须验证证书，禁止用跳过校验掩盖证书链问题。
- 门户不下发中间证书：只嵌入公开的中间证书（`internal/session/intermediate.pem`），与系统根证书一起校验，不嵌入整包根证书。该证书有效期至 2027-11-02 12:24:33 UTC。
- 本地隧道不是 SSH，当前代理协议也没有额外的 TLS，不能宣传成加密隧道。
- 协议密钥和门户地址不硬编码，二进制中不能包含用户凭据。
