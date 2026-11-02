package secdb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
)

// Params 是解码并规整后的数据库连接参数。
//
// secdb 只做规整，不判断必填：例如 ssosecdb 的 action=ad 结果本来就不含数据库字段。
// 建立隧道所需的 YxzIP、SessionID、Username 由调用方校验非空。
type Params struct {
	// Type 是数据库类型的规范名称，例如 "MySQL"；无法识别时保留原值（数字形如 "type=99"），缺失时为空。
	Type string
	// DBIP 是目标数据库地址，仅用于展示，客户端实际连接本地隧道端口。
	DBIP string
	// DBPort 是目标数据库端口，仅用于展示；原值可以是数字或纯数字字符串，缺失或无效时为 0。
	DBPort int
	// Username 是数据库账号，同时用于代理握手。
	Username string
	// Password 是数据库口令，属于凭据，不出现在 String、GoString 和日志输出中。
	Password string
	// DBName 是默认数据库名，可为空。
	DBName string
	// YxzIP 是代理服务地址（不含端口），用于建立隧道。
	YxzIP string
	// SessionID 是代理握手凭证，属于凭据，不出现在 String、GoString 和日志输出中。
	SessionID string
	// Extra 保存未识别字段的原始 JSON 值，用于诊断；输出时只列字段名，不输出值。
	Extra map[string]json.RawMessage
}

// dbTypeByNumber 对应旧实现 DB_TYPE_NUM（数字 type 的映射）。
// 旧实现把 9 显示为 "SQLServer"，这里与字符串映射统一为 "SQL Server"，保证同一种库只有一个规范名称。
var dbTypeByNumber = map[int64]string{
	7: "DB2", 8: "MySQL", 9: "SQL Server", 10: "Oracle", 13: "PostgreSQL", 14: "GaussDB",
}

// dbTypeByName 对应旧实现 DB_TYPE_NAMES（字符串 type 按小写匹配）。
var dbTypeByName = map[string]string{
	"mysql": "MySQL", "mssql": "SQL Server", "sqlserver": "SQL Server", "oracle": "Oracle",
	"db2": "DB2", "postgresql": "PostgreSQL", "postgres": "PostgreSQL", "gauss": "GaussDB",
	"dm": "达梦 DM",
}

// knownFields 是会被映射到 Params 具名字段的 JSON 字段，其余字段进入 Extra。
var knownFields = map[string]bool{
	"type": true, "dbServiceType": true, "dbIp": true, "dbPort": true, "username": true,
	"password": true, "dbname": true, "yxzIp": true, "sessionId": true,
}

// paramsFromObject 把 JSON 对象规整为 Params。已知字符串字段类型不对时返回 ErrPayload，
// 说明协议格式与预期不符，不静默吞掉。
func paramsFromObject(obj map[string]json.RawMessage) (Params, error) {
	var p Params
	stringFields := []struct {
		name string
		dst  *string
	}{
		{"dbIp", &p.DBIP}, {"username", &p.Username}, {"password", &p.Password},
		{"dbname", &p.DBName}, {"yxzIp", &p.YxzIP}, {"sessionId", &p.SessionID},
	}
	for _, f := range stringFields {
		v, err := optionalString(obj, f.name)
		if err != nil {
			return Params{}, err
		}
		*f.dst = v
	}

	t, err := normalizeType(obj)
	if err != nil {
		return Params{}, err
	}
	p.Type = t
	p.DBPort = normalizePort(obj["dbPort"])

	for k, v := range obj {
		if knownFields[k] {
			continue
		}
		if p.Extra == nil {
			p.Extra = make(map[string]json.RawMessage)
		}
		p.Extra[k] = v
	}
	return p, nil
}

// optionalString 读取字符串字段；缺失或为 null 视为空串。
// JSON 数字按原始十进制文本转为字符串，兼容纯数字账号（旧实现用 str() 取值）；
// 对象、数组、布尔值说明协议格式与预期不符，报错。
func optionalString(obj map[string]json.RawMessage, name string) (string, error) {
	raw, ok := obj[name]
	if !ok || isNull(raw) {
		return "", nil
	}
	if isJSONNumber(raw) {
		return string(bytes.TrimSpace(raw)), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%w: 字段 %s 应为字符串", ErrPayload, name)
	}
	return s, nil
}

// normalizeType 按旧实现规则规整数据库类型：
// type 为数字时按编号映射；否则取 type 或（type 为空时）dbServiceType 的字符串，按小写映射。
func normalizeType(obj map[string]json.RawMessage) (string, error) {
	raw, ok := obj["type"]
	if ok && !isNull(raw) {
		var num json.Number
		if err := json.Unmarshal(raw, &num); err == nil && isJSONNumber(raw) {
			if n, err := num.Int64(); err == nil {
				if name, ok := dbTypeByNumber[n]; ok {
					return name, nil
				}
			}
			return "type=" + num.String(), nil
		}
	}
	t, err := optionalString(obj, "type")
	if err != nil {
		return "", err
	}
	// 与旧实现一致：type 为空串时回退到 dbServiceType
	if t == "" {
		if t, err = optionalString(obj, "dbServiceType"); err != nil {
			return "", err
		}
	}
	if name, ok := dbTypeByName[strings.ToLower(t)]; ok {
		return name, nil
	}
	return t, nil
}

// normalizePort 接受整数或纯数字字符串，取值须在 1..65535；其余情况返回 0，因为端口只用于展示。
func normalizePort(raw json.RawMessage) int {
	if raw == nil {
		return 0
	}
	var s string
	if isJSONNumber(raw) {
		s = string(bytes.TrimSpace(raw))
	} else if err := json.Unmarshal(raw, &s); err != nil {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}

func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// isJSONNumber 判断原始值是否为 JSON 数字字面量（json.Number 也能解码带引号的数字字符串，需排除）。
func isJSONNumber(raw json.RawMessage) bool {
	b := bytes.TrimSpace(raw)
	return len(b) > 0 && (b[0] == '-' || '0' <= b[0] && b[0] <= '9')
}

const redacted = "[REDACTED]"

// String 返回脱敏后的描述：Password、SessionID 只显示是否存在，Extra 只列字段名。
// fmt 的 %v、%+v、%s 都会走到这里。
func (p Params) String() string {
	return fmt.Sprintf("{Type:%q DBIP:%q DBPort:%d Username:%q Password:%s DBName:%q YxzIP:%q SessionID:%s Extra:%v}",
		p.Type, p.DBIP, p.DBPort, p.Username, redact(p.Password), p.DBName, p.YxzIP, redact(p.SessionID), p.extraKeys())
}

// GoString 让 %#v 也输出脱敏内容，避免调试打印或 panic 带出凭据。
func (p Params) GoString() string { return "secdb.Params" + p.String() }

// LogValue 实现 slog.LogValuer，结构化日志中同样脱敏。
func (p Params) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("type", p.Type),
		slog.String("dbIp", p.DBIP),
		slog.Int("dbPort", p.DBPort),
		slog.String("username", p.Username),
		slog.String("password", redact(p.Password)),
		slog.String("dbname", p.DBName),
		slog.String("yxzIp", p.YxzIP),
		slog.String("sessionId", redact(p.SessionID)),
		slog.Any("extraKeys", p.extraKeys()),
	)
}

// redact 对非空凭据返回占位符；空值原样显示为 ""，便于诊断字段缺失。
func redact(s string) string {
	if s == "" {
		return `""`
	}
	return redacted
}

func (p Params) extraKeys() []string {
	keys := make([]string, 0, len(p.Extra))
	for k := range p.Extra {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
