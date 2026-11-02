// Package secdb 解码运维门户下发的数据库连接参数。
//
// 支持两种格式，均为 AES-128-CBC + PKCS7，密文为标准 Base64，IV 与密文前 8 个字符交错拼接：
//
//   - HTTP 响应 data.db：IV = s[0:8] + s[16:24]，密文 = s[8:16] + s[24:]，单层，用 DB 密钥解出资源 JSON。
//   - ssosecdb 链接参数：IV = s[16:24] + s[0:8]，密文 = s[8:16] + s[24:]，外层用传输密钥解出
//     {action, ...}，再从外层字段中找出内层密文，用 DB 密钥解出资源 JSON 并与外层合并。
//
// 解析采用严格模式：输入必须是纯 ASCII，Base64 和 PKCS7 均严格校验。所有错误都归入
// ErrFormat、ErrDecrypt、ErrPayload 三类之一，错误信息只含长度、字段名、层级等诊断信息，
// 不含密钥、密文或解出的明文。
package secdb

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	// ErrFormat 表示输入本身不合法：非 ASCII、长度不足、Base64 非法或密文长度不是块大小的整数倍。
	ErrFormat = errors.New("secdb: 输入格式错误")
	// ErrDecrypt 表示解密结果无效：PKCS7 padding 不合法或明文不是合法 UTF-8。
	// AES-CBC 下密钥不匹配和密文损坏无法区分，都会落到这一类。
	ErrDecrypt = errors.New("secdb: 解密失败（密钥不匹配或数据已损坏）")
	// ErrPayload 表示解密成功但内容不符合协议预期：不是 JSON 对象、字段类型错误等。
	ErrPayload = errors.New("secdb: 解密内容不符合协议格式")
)

// Key 是 AES-128 密钥，类型本身保证恰好 16 字节。
type Key [16]byte

// ParseKey 把字符串形式的密钥 s 转换为 Key。
//
// 只校验字节长度必须为 16，不限制字符集。错误信息只包含实际长度，不包含密钥内容。
func ParseKey(s string) (Key, error) {
	var k Key
	if len(s) != len(k) {
		return k, fmt.Errorf("secdb: 密钥长度必须为 %d 字节，实际为 %d 字节", len(k), len(s))
	}
	copy(k[:], s)
	return k, nil
}

// String 返回脱敏占位符，防止密钥经 %v、%s 进入日志。
func (Key) String() string { return "[REDACTED]" }

// GoString 返回脱敏占位符，防止密钥经 %#v 或 panic 输出泄露。
func (Key) GoString() string { return "secdb.Key([REDACTED])" }

// DecodeDBField 用数据库资源密钥 dbKey 解码 HTTP 响应中 data.db 字段的原始字符串 s。
//
// 输入原样解析，不做去空白等清理。返回的 Params 不保证含有任何必填字段，必填校验由调用方负责。
// 错误属于 ErrFormat、ErrDecrypt 或 ErrPayload 之一。
func DecodeDBField(dbKey Key, s string) (Params, error) {
	plain, err := decryptLayer(dbKey, s, dbFieldIV, "data.db")
	if err != nil {
		return Params{}, err
	}
	obj, err := parseObject(plain, "data.db")
	if err != nil {
		return Params{}, err
	}
	return paramsFromObject(obj)
}

// ivLayout 从混排后的输入中取出 16 字节 IV，两种格式的 IV 两半顺序相反。
type ivLayout func(s string) string

func dbFieldIV(s string) string  { return s[0:8] + s[16:24] }
func ssoSecDBIV(s string) string { return s[16:24] + s[0:8] }

// minScrambledLen 是混排输入的最短长度：至少要覆盖 IV 和密文前 8 个字符所占的 24 个字符，
// 并且后面还有密文，与旧实现 "长度需 >24" 的判断一致。
const minScrambledLen = 25

// decryptLayer 还原 IV 和 Base64 密文并解密一层，返回已校验为合法 UTF-8 的明文。
// layer 只用于错误信息定位，例如 "data.db"、"外层"、"内层候选 payload"。
func decryptLayer(key Key, s string, iv ivLayout, layer string) ([]byte, error) {
	// 要求纯 ASCII，保证按字节切片与旧实现按字符切片的偏移一致
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return nil, fmt.Errorf("%w: %s 在字节偏移 %d 处含非 ASCII 字符", ErrFormat, layer, i)
		}
	}
	if len(s) < minScrambledLen {
		return nil, fmt.Errorf("%w: %s 长度 %d 过短（至少 %d）", ErrFormat, layer, len(s), minScrambledLen)
	}

	b64 := s[8:16] + s[24:]
	// Go 的 base64 解码器会静默跳过 \r 和 \n，这里先逐字符校验，保证严格模式
	for i := 0; i < len(b64); i++ {
		if !isStdBase64Char(b64[i]) {
			return nil, fmt.Errorf("%w: %s 的 Base64 密文在偏移 %d 处含非法字符", ErrFormat, layer, i)
		}
	}
	ct, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("%w: %s 的 Base64 密文不合法（长度 %d）", ErrFormat, layer, len(b64))
	}
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("%w: %s 的密文长度 %d 不是 %d 的正整数倍", ErrFormat, layer, len(ct), aes.BlockSize)
	}

	block, err := aes.NewCipher(key[:])
	if err != nil {
		// Key 类型保证 16 字节，这里不会失败
		panic(err)
	}
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, []byte(iv(s))).CryptBlocks(plain, ct)

	plain, ok := unpadPKCS7(plain)
	if !ok {
		return nil, fmt.Errorf("%w: %s 的 PKCS7 padding 不合法", ErrDecrypt, layer)
	}
	if !utf8.Valid(plain) {
		return nil, fmt.Errorf("%w: %s 的明文不是合法 UTF-8", ErrDecrypt, layer)
	}
	return plain, nil
}

func isStdBase64Char(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' ||
		c == '+' || c == '/' || c == '='
}

// unpadPKCS7 严格校验并去除 PKCS7 padding：padding 长度为 1..16，且每个 padding 字节都等于该长度。
func unpadPKCS7(b []byte) ([]byte, bool) {
	n := int(b[len(b)-1])
	if n == 0 || n > aes.BlockSize || n > len(b) {
		return nil, false
	}
	if !bytes.Equal(b[len(b)-n:], bytes.Repeat([]byte{byte(n)}, n)) {
		return nil, false
	}
	return b[:len(b)-n], true
}

// parseObject 把明文解析为 JSON 对象，保留每个字段的原始值。
func parseObject(plain []byte, layer string) (map[string]json.RawMessage, error) {
	obj, _, err := parseOrderedObject(plain, layer)
	return obj, err
}

// parseOrderedObject 一次遍历把明文解析为 JSON 对象，同时返回字段首次出现的顺序；
// 内层候选的尝试顺序依赖它，Go map 本身不保序。重复字段与 json.Unmarshal 一致，后出现的值覆盖前值。
// 不包装 encoding/json 的错误：其错误文本可能带出明文字符。
func parseOrderedObject(plain []byte, layer string) (map[string]json.RawMessage, []string, error) {
	invalid := fmt.Errorf("%w: %s 的明文不是合法 JSON 对象", ErrPayload, layer)
	dec := json.NewDecoder(bytes.NewReader(plain))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil, invalid
	}
	obj := make(map[string]json.RawMessage)
	var order []string
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, nil, invalid
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, invalid
		}
		if _, dup := obj[key]; !dup {
			order = append(order, key)
		}
		obj[key] = v
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, nil, invalid
	}
	// 对象之后不允许再有其他内容，与 json.Unmarshal 的严格程度一致
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, invalid
	}
	return obj, order, nil
}

// DecodeSSOSecDB 用外层传输密钥 transKey 和内层资源密钥 dbKey 解码 ssosecdb 链接或其参数部分。
//
// 输入先去掉首尾空白和精确匹配的 "ssosecdb://" 或 "ssosecdb//" 前缀，再去掉末尾作为分隔符的 "/"；
// 没有前缀时整段作为参数。外层解出后按旧实现规则处理：
//   - action 为 "ad"、缺失或不是字符串时，直接用外层字段构造结果；
//   - 否则依次尝试内层候选：先取 outer[action]（字符串且长度大于 24），再按字段出现顺序取其他长度大于 40
//     的字符串值；第一个能解出 JSON 对象的候选胜出，内层字段覆盖外层字段。
//
// 与旧实现的有意差异：
//   - 外层不是 JSON 对象时报 ErrPayload，旧实现返回解出的明文；
//   - 没有内层候选或所有候选都解不出 JSON 对象时报错，旧实现静默返回外层字段；
//   - 被消费的内层密文字段不进入 Extra，旧实现保留在合并结果中；
//   - 参数首尾属于 Base64 或 IV 的 "/" 保留，旧实现会误删。
//
// 错误属于 ErrFormat、ErrDecrypt 或 ErrPayload 之一。
func DecodeSSOSecDB(transKey, dbKey Key, s string) (Params, error) {
	param := stripSSOHeader(s)
	plain, err := decryptLayer(transKey, param, ssoSecDBIV, "外层")
	if err != nil {
		return Params{}, err
	}
	outer, order, err := parseOrderedObject(plain, "外层")
	if err != nil {
		return Params{}, err
	}

	// null 也不是字符串（json.Unmarshal 会把 null 静默解成空串，需单独排除）
	var action string
	if raw, ok := outer["action"]; !ok || isNull(raw) || json.Unmarshal(raw, &action) != nil || action == "ad" {
		return paramsFromObject(outer)
	}

	cands := innerCandidates(outer, order, action)
	if len(cands) == 0 {
		// action 的值来自解密明文，不写进错误信息
		return Params{}, fmt.Errorf("%w: 外层 action 指向的内层密文字段不存在", ErrPayload)
	}
	var firstErr error
	for _, c := range cands {
		layer := fmt.Sprintf("内层候选字段 %q", c.field)
		plain, err := decryptLayer(dbKey, c.value, ssoSecDBIV, layer)
		if err == nil {
			var inner map[string]json.RawMessage
			if inner, err = parseObject(plain, layer); err == nil {
				return paramsFromObject(mergeLayers(outer, inner, c.field))
			}
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	// 错误类别取首个候选（最可能是真正的内层密文）的失败原因，信息只列字段名和数量
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = strconv.Quote(c.field)
	}
	return Params{}, fmt.Errorf("%w; 内层 %d 个候选字段 [%s] 均未解出 JSON 对象",
		firstErr, len(cands), strings.Join(names, ", "))
}

// stripSSOHeader 去掉首尾空白、精确匹配的协议头，以及链接末尾作为分隔符的 "/"。
//
// "/" 是合法的 Base64 和 IV 字符，参数本身可能以 "/" 开头或结尾，所以不能像旧实现那样
// 剥掉首尾所有 "/"，也不能在第一个 "//" 处截断。末尾 "/" 是否为分隔符按长度判断：
// 参数去掉 16 个 IV 字符后剩下的 Base64 密文长度必须是 4 的倍数，不满足时才去掉末尾 "/"。
func stripSSOHeader(s string) string {
	s = strings.TrimSpace(s)
	for _, h := range []string{"ssosecdb://", "ssosecdb//"} {
		if rest, ok := strings.CutPrefix(s, h); ok {
			s = rest
			break
		}
	}
	for strings.HasSuffix(s, "/") && (len(s)-16)%4 != 0 {
		s = s[:len(s)-1]
	}
	return s
}

type candidate struct {
	field string
	value string
}

// innerCandidates 按旧实现顺序收集内层密文候选，同值只保留第一次出现的字段。
// 长度按字符计数，与旧实现 Python len() 一致。
func innerCandidates(outer map[string]json.RawMessage, order []string, action string) []candidate {
	var out []candidate
	seen := map[string]bool{}
	add := func(field string, minLen int) {
		var v string
		if json.Unmarshal(outer[field], &v) != nil || utf8.RuneCountInString(v) <= minLen || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, candidate{field, v})
	}
	if _, ok := outer[action]; ok {
		add(action, 24)
	}
	for _, k := range order {
		add(k, 40)
	}
	return out
}

// mergeLayers 合并外层和内层：内层覆盖外层，内层缺 action 时保留外层的 action；
// 被消费的密文字段（除非内层自己也有同名字段）不保留。
func mergeLayers(outer, inner map[string]json.RawMessage, consumed string) map[string]json.RawMessage {
	merged := make(map[string]json.RawMessage, len(outer)+len(inner))
	for k, v := range outer {
		if k != consumed {
			merged[k] = v
		}
	}
	for k, v := range inner {
		merged[k] = v
	}
	return merged
}
