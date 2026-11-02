#!/usr/bin/env python3
"""生成 internal/secdb 的兼容性测试向量。

用合成密钥加密合成明文，再交给旧版 Python 实现（ssotool_mac.py）解密校验，
只有旧实现解出的字段与预期一致的向量才会写入输出文件。这样 Go 测试通过即说明
Go 实现与旧实现在这些样本上的行为一致，而不只是 Go 自身的加解密回环。

校验内容：dbIp、dbPort、username、password、dbname、yxzIp、sessionId 取自旧版
parse_params()；数据库类型取自旧版 render() 输出的“数据库类型”一行；额外字段名取自
parse_params() 结果中的其余键。

Go 版与旧实现有以下有意差异，脚本按差异显式换算后再比对，而不是声称两边一致：
  - 被消费的内层密文字段：旧实现合并结果保留该字段，Go 版不放进 Extra（脚本断言旧实现
    确实保留了它，再从比对中剔除）。
  - 数据库类型名称：旧实现缺失时显示“未知类型”、未知数字显示“type=N(未知)”、数字 9
    显示“SQLServer”；Go 版分别为空串、“type=N”、“SQL Server”（见 LEGACY_TYPE_LABEL）。
  - 参数首尾的 "/"：旧实现会误删，相关场景无法由旧实现校验，改在 Go 测试中构造。

向量只含合成密钥和合成数据，不含任何真实门户地址、密钥或凭据。

用法（旧项目只读导入，不在旧目录写入任何文件）:
    uv venv /tmp/jumpdb-vectors-venv
    uv pip install --python /tmp/jumpdb-vectors-venv/bin/python pycryptodome
    /tmp/jumpdb-vectors-venv/bin/python -I -B \
        scripts/gen_secdb_vectors.py --legacy <旧项目目录> \
        --out internal/secdb/testdata/vectors.json
"""
import argparse
import base64
import json
import random
import sys

from Crypto.Cipher import AES
from Crypto.Util.Padding import pad

# 合成密钥，仅用于测试，与任何真实环境无关
TRANS_KEY = b"TestTransKey0001"
DB_KEY = b"TestDbKey0000002"

IV_CHARSET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
KNOWN_FIELDS = {"type", "dbServiceType", "dbIp", "dbPort", "username",
                "password", "dbname", "yxzIp", "sessionId"}

rng = random.Random(20261102)

# Go 版类型名 -> 旧版 render() 显示名，仅列出两边不同的情况
LEGACY_TYPE_LABEL = {"": "未知类型", "SQL Server(9)": "SQLServer"}


def encrypt(plain_obj, key):
    """返回 (iv, b64)；IV 取可打印字符，与真实数据经 URL 传输的形态一致。"""
    iv = "".join(rng.choice(IV_CHARSET) for _ in range(16))
    plain = json.dumps(plain_obj, ensure_ascii=False).encode("utf-8")
    ct = AES.new(key, AES.MODE_CBC, iv.encode("latin1")).encrypt(pad(plain, 16))
    return iv, base64.b64encode(ct).decode()


def enc_db_field(obj, key=DB_KEY):
    # decrypt_db_field 的逆过程: IV = db[0:8] + db[16:24]，密文 = db[8:16] + db[24:]
    iv, b64 = encrypt(obj, key)
    return iv[0:8] + b64[0:8] + iv[8:16] + b64[8:]


def enc_sso_layer(obj, key):
    """unscramble 的逆过程: param = iv[8:16] + b64[0:8] + iv[0:8] + b64[8:]。

    旧版 strip_head 会在第一个 "//" 处截断并剥掉首尾 "/"，参数本身含这些字符时
    旧实现无法正确解析；这里重新生成 IV 避开，保证向量能被旧实现验证。
    """
    while True:
        iv, b64 = encrypt(obj, key)
        param = iv[8:16] + b64[0:8] + iv[0:8] + b64[8:]
        if "//" not in param and not param.startswith("/") and not param.endswith("/"):
            return param


def expect(type_="", db_ip="", db_port=0, username="", password="", dbname="",
           yxz_ip="", session_id="", extra_keys=()):
    """Go 侧 Params 的预期值，手写字面量，作为独立于实现的事实来源。"""
    return {"type": type_, "dbIp": db_ip, "dbPort": db_port, "username": username,
            "password": password, "dbname": dbname, "yxzIp": yxz_ip,
            "sessionId": session_id, "extraKeys": sorted(extra_keys)}


def build_cases():
    cases = []

    # ---- HTTP 响应 data.db 格式 ----
    cases.append({
        "name": "db_type_number",
        "format": "dbfield",
        "input": enc_db_field({"type": 8, "dbIp": "198.51.100.5", "dbPort": 3306,
                               "username": "测试用户", "password": "p@ss-中文",
                               "dbname": "订单库", "yxzIp": "192.0.2.10",
                               "sessionId": "sess-0001"}),
        "expect": expect("MySQL", "198.51.100.5", 3306, "测试用户", "p@ss-中文",
                         "订单库", "192.0.2.10", "sess-0001"),
    })
    cases.append({
        "name": "db_type_string_port_string_extra",
        "format": "dbfield",
        "input": enc_db_field({"type": "mysql", "dbIp": "198.51.100.6", "dbPort": "3307",
                               "username": "u2", "password": "pw2", "yxzIp": "192.0.2.11",
                               "sessionId": "sess-0002", "oracleRole": "normal",
                               "备注": "中文值"}),
        "expect": expect("MySQL", "198.51.100.6", 3307, "u2", "pw2", "", "192.0.2.11",
                         "sess-0002", ["oracleRole", "备注"]),
    })
    cases.append({
        "name": "db_service_type_fallback_no_port",
        "format": "dbfield",
        "input": enc_db_field({"dbServiceType": "postgres", "dbIp": "198.51.100.7",
                               "username": "pg用户", "password": "pw3",
                               "yxzIp": "192.0.2.12", "sessionId": "sess-0003"}),
        "expect": expect("PostgreSQL", "198.51.100.7", 0, "pg用户", "pw3", "",
                         "192.0.2.12", "sess-0003"),
    })
    cases.append({
        "name": "db_unknown_type_number",
        "format": "dbfield",
        "input": enc_db_field({"type": 99, "dbIp": "198.51.100.8", "dbPort": 1521,
                               "username": "未知", "password": "pw4",
                               "yxzIp": "192.0.2.13", "sessionId": "sess-0004"}),
        "expect": expect("type=99", "198.51.100.8", 1521, "未知", "pw4", "",
                         "192.0.2.13", "sess-0004"),
    })
    # 数字 9 与字符串 sqlserver 在 Go 版统一为 "SQL Server"
    cases.append({
        "name": "db_type_sqlserver_number",
        "format": "dbfield",
        "input": enc_db_field({"type": 9, "dbIp": "198.51.100.9", "dbPort": 1433,
                               "username": "sa用户", "password": "pw5",
                               "yxzIp": "192.0.2.14", "sessionId": "sess-0005"}),
        "expect": expect("SQL Server", "198.51.100.9", 1433, "sa用户", "pw5", "",
                         "192.0.2.14", "sess-0005"),
    })

    # ---- ssosecdb 格式：外层 TRANS_KEY，内层 DB_KEY ----
    def sso_db(prefix, suffix, n):
        inner = {"username": f"内层用户{n}", "password": f"inner-pw-{n}",
                 "dbIp": f"198.51.100.{20 + n}", "dbPort": 3306, "type": "mysql",
                 "yxzIp": f"192.0.2.{20 + n}", "sessionId": f"inner-sess-{n}"}
        outer = {"action": "db", "db": enc_sso_layer(inner, DB_KEY),
                 "sessionId": "outer-sess", "webPort": 443}
        return prefix + enc_sso_layer(outer, TRANS_KEY) + suffix, expect(
            "MySQL", f"198.51.100.{20 + n}", 3306, f"内层用户{n}", f"inner-pw-{n}", "",
            f"192.0.2.{20 + n}", f"inner-sess-{n}", ["action", "webPort"])

    for name, prefix, suffix, n in [("sso_header_scheme", "ssosecdb://", "/", 1),
                                    ("sso_header_double_slash", "ssosecdb//", "/", 2),
                                    ("sso_no_header", "", "", 3)]:
        raw, exp = sso_db(prefix, suffix, n)
        cases.append({"name": name, "format": "ssosecdb", "input": raw,
                      "consumed": "db", "expect": exp})

    cases.append({
        "name": "sso_action_ad",
        "format": "ssosecdb",
        "input": "ssosecdb//" + enc_sso_layer({"action": "ad", "msg": "广告位"}, TRANS_KEY) + "/",
        "expect": expect(extra_keys=["action", "msg"]),
    })
    cases.append({
        "name": "sso_action_missing",
        "format": "ssosecdb",
        "input": "ssosecdb//" + enc_sso_layer({"username": "外层用户", "foo": "bar"},
                                              TRANS_KEY) + "/",
        "expect": expect(username="外层用户", extra_keys=["foo"]),
    })

    # 内层密文不在 outer[action] 下：先跳过一个无法解密的长串，再命中 payload
    inner = {"username": "兜底用户", "password": "fallback-pw", "dbIp": "198.51.100.30",
             "dbPort": 3306, "type": 8, "yxzIp": "192.0.2.30", "sessionId": "fb-sess"}
    outer = {"action": "db", "db": "short", "trace": "A" * 48,
             "payload": enc_sso_layer(inner, DB_KEY)}
    cases.append({
        "name": "sso_fallback_candidate",
        "format": "ssosecdb",
        "input": "ssosecdb//" + enc_sso_layer(outer, TRANS_KEY) + "/",
        "consumed": "payload",
        "expect": expect("MySQL", "198.51.100.30", 3306, "兜底用户", "fallback-pw", "",
                         "192.0.2.30", "fb-sess", ["action", "db", "trace"]),
    })

    # action 为 null 不是字符串，旧实现直接返回外层；外层长串不会被当作内层候选
    cases.append({
        "name": "sso_action_null",
        "format": "ssosecdb",
        "input": "ssosecdb//" + enc_sso_layer({"action": None, "username": "空动作用户",
                                              "": "B" * 48}, TRANS_KEY) + "/",
        "expect": expect(username="空动作用户", extra_keys=["", "action"]),
    })
    return cases


def verify_with_legacy(tool, case):
    """用旧实现解密，并核对与手写预期一致。"""
    if case["format"] == "dbfield":
        got = json.loads(tool.decrypt_db_field(case["input"]))
        # 真实 HTTP 路径：整段响应 JSON 交给 parse_params
        via_http = tool.parse_params(json.dumps({"code": 0, "data": {"db": case["input"]}},
                                                ensure_ascii=False))
        via_http.pop("_sessionId", None)
        assert via_http == got, f"{case['name']}: HTTP 路径与直接解密结果不一致"
    else:
        got = tool.parse_params(case["input"])
    assert "_raw" not in got, f"{case['name']}: 旧实现未解出 JSON"

    exp = case["expect"]
    for field in ("dbIp", "username", "password", "dbname", "yxzIp", "sessionId"):
        assert got.get(field, "") == exp[field], f"{case['name']}: 字段 {field} 不一致"
    assert int(got.get("dbPort", 0)) == exp["dbPort"], f"{case['name']}: dbPort 不一致"

    # 类型规整逻辑在旧版 render() 中，取其“数据库类型”一行比对
    label = tool.render(got).splitlines()[0].split(":", 1)[1].strip()
    want = exp["type"]
    if want.startswith("type="):
        want += "(未知)"
    elif got.get("type") == 9:
        want = LEGACY_TYPE_LABEL[want + "(9)"]
    else:
        want = LEGACY_TYPE_LABEL.get(want, want)
    assert label == want, f"{case['name']}: 类型 {label!r} 与预期 {want!r} 不一致"

    # 有意差异：旧实现保留被消费的密文字段，Go 版不保留
    consumed = case.get("consumed")
    if consumed:
        assert consumed in got, f"{case['name']}: 旧实现未保留被消费字段 {consumed}"
    extra = set(got) - KNOWN_FIELDS - {consumed}
    assert sorted(extra) == exp["extraKeys"], f"{case['name']}: 额外字段 {sorted(extra)}"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--legacy", required=True, help="旧 Python 项目目录（只读导入）")
    ap.add_argument("--out", required=True, help="向量输出路径")
    args = ap.parse_args()

    sys.path.insert(0, args.legacy)
    import ssotool_mac as tool
    # 运行时替换为合成密钥，旧实现的解密函数在调用时读取模块级常量
    tool.KEY_TRANS = TRANS_KEY
    tool.KEY_DB = DB_KEY

    cases = build_cases()
    for case in cases:
        verify_with_legacy(tool, case)

    doc = {
        "note": "由 scripts/gen_secdb_vectors.py 生成，已经旧版 Python 实现解密校验；仅含合成密钥和合成数据",
        "keys": {"trans": TRANS_KEY.decode(), "db": DB_KEY.decode()},
        "cases": cases,
    }
    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(doc, f, ensure_ascii=False, indent=2)
        f.write("\n")
    print(f"已生成 {len(cases)} 条向量，全部通过旧实现校验 -> {args.out}")


if __name__ == "__main__":
    main()
