package secdb

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// secretMarker 出现在构造的明文里，任何错误信息都不应包含它。
var secretMarker = "LEAK" + strings.Repeat("-marker", 3)

// fixedIV 让构造的密文可复现，测试结果不随随机 IV 波动。
var fixedIV = strings.Repeat("Iv", aes.BlockSize/2)

// encryptRaw 用固定 IV 加密已按块对齐的明文（不自动补 padding），返回 IV 和 Base64 密文。
// 仅用于构造错误和边界场景；兼容性由 testdata 中经旧实现校验的向量保证。
func encryptRaw(t *testing.T, key Key, aligned []byte) (iv, b64 string) {
	t.Helper()
	return encryptRawIV(t, key, fixedIV, aligned)
}

func encryptRawIV(t *testing.T, key Key, iv string, aligned []byte) (string, string) {
	t.Helper()
	if len(aligned)%aes.BlockSize != 0 {
		t.Fatalf("明文长度 %d 未按块对齐", len(aligned))
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, len(aligned))
	cipher.NewCBCEncrypter(block, []byte(iv)).CryptBlocks(ct, aligned)
	return iv, base64.StdEncoding.EncodeToString(ct)
}

func pkcs7(b []byte) []byte {
	n := aes.BlockSize - len(b)%aes.BlockSize
	return append(append([]byte{}, b...), bytes.Repeat([]byte{byte(n)}, n)...)
}

func dbFieldOf(t *testing.T, key Key, aligned []byte) string {
	iv, b64 := encryptRaw(t, key, aligned)
	return iv[0:8] + b64[0:8] + iv[8:16] + b64[8:]
}

func ssoLayerOf(t *testing.T, key Key, aligned []byte) string {
	iv, b64 := encryptRaw(t, key, aligned)
	return iv[8:16] + b64[0:8] + iv[0:8] + b64[8:]
}

func jsonOf(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func firstCase(t *testing.T, vf vectorFile, format string) string {
	t.Helper()
	return casesOf(t, vf, format)[0].Input
}

func TestDecodeErrorsAreClassifiedWithoutLeakingSecrets(t *testing.T) {
	vf, transKey, dbKey := loadVectors(t)
	validDB := firstCase(t, vf, "dbfield")
	secretPlain := []byte(`{"password":"` + secretMarker + `"`) // 截断的 JSON

	type tc struct {
		name   string
		decode func() (Params, error)
		want   error
		input  string
	}
	db := func(name, input string, want error) tc {
		return tc{name, func() (Params, error) { return DecodeDBField(dbKey, input) }, want, input}
	}
	sso := func(name, input string, want error) tc {
		return tc{name, func() (Params, error) { return DecodeSSOSecDB(transKey, dbKey, input) }, want, input}
	}
	outerWith := func(v map[string]any) string {
		return "ssosecdb//" + ssoLayerOf(t, transKey, pkcs7(jsonOf(t, v))) + "/"
	}

	cases := []tc{
		// ErrFormat：篡改合法向量
		db("非ASCII字符", validDB[:30]+"é"+validDB[30:], ErrFormat),
		db("长度不足", validDB[:24], ErrFormat),
		db("Base64中混入换行", validDB[:40]+"\n"+validDB[40:], ErrFormat),
		db("Base64长度不是4的倍数", validDB[:len(validDB)-1], ErrFormat),
		db("密文长度不是16的倍数", validDB[:len(validDB)-4], ErrFormat),
		sso("只有协议头", "ssosecdb///", ErrFormat),

		// ErrDecrypt
		{"密钥不匹配", func() (Params, error) { return DecodeDBField(transKey, validDB) }, ErrDecrypt, validDB},
		db("padding为0", dbFieldOf(t, dbKey, append(bytes.Repeat([]byte("x"), 15), 0)), ErrDecrypt),
		db("padding字节不一致", dbFieldOf(t, dbKey, append(bytes.Repeat([]byte("x"), 13), 3, 3, 2)), ErrDecrypt),
		db("padding超过块大小", dbFieldOf(t, dbKey, append(bytes.Repeat([]byte("x"), 15), 17)), ErrDecrypt),
		db("明文不是UTF-8", dbFieldOf(t, dbKey, pkcs7([]byte(secretMarker+"\xff\xfe"))), ErrDecrypt),

		// ErrPayload
		db("明文不是JSON", dbFieldOf(t, dbKey, pkcs7(secretPlain)), ErrPayload),
		db("明文是JSON数组", dbFieldOf(t, dbKey, pkcs7([]byte(`["`+secretMarker+`"]`))), ErrPayload),
		db("明文是null", dbFieldOf(t, dbKey, pkcs7([]byte(`null`))), ErrPayload),
		db("username是布尔值", dbFieldOf(t, dbKey, pkcs7(jsonOf(t, map[string]any{
			"username": true, "password": secretMarker}))), ErrPayload),
		db("dbname是数组", dbFieldOf(t, dbKey, pkcs7(jsonOf(t, map[string]any{
			"dbname": []any{secretMarker}}))), ErrPayload),
		db("type是对象", dbFieldOf(t, dbKey, pkcs7(jsonOf(t, map[string]any{
			"type": map[string]any{"x": secretMarker}}))), ErrPayload),
		sso("外层不是JSON", "ssosecdb//"+ssoLayerOf(t, transKey, pkcs7(secretPlain))+"/", ErrPayload),
		sso("缺少内层密文", outerWith(map[string]any{"action": "db", "db": "short", "password": secretMarker}), ErrPayload),
		// action 的值来自解密明文，错误信息不能带出
		sso("action值不进错误信息", outerWith(map[string]any{"action": secretMarker}), ErrPayload),
		sso("内层明文不是JSON", outerWith(map[string]any{
			"action": "db", "db": ssoLayerOf(t, dbKey, pkcs7(secretPlain))}), ErrPayload),
		sso("内层密钥不匹配", outerWith(map[string]any{
			"action": "db", "db": ssoLayerOf(t, transKey, pkcs7(jsonOf(t, map[string]any{"password": secretMarker})))}), ErrDecrypt),
	}
	all := []error{ErrFormat, ErrDecrypt, ErrPayload}
	forbidden := []string{vf.Keys.Trans, vf.Keys.DB, secretMarker, "invalid character", "unexpected end"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := c.decode()
			if err == nil {
				t.Fatalf("期望错误 %v，实际成功: %v", c.want, p)
			}
			for _, e := range all {
				if errors.Is(err, e) != (e == c.want) {
					t.Errorf("错误分类不对: errors.Is(err, %v) = %v; err = %v", e, !(e == c.want), err)
				}
			}
			msg := err.Error()
			for _, s := range append(forbidden, c.input, c.input[min(8, len(c.input)):]) {
				if len(s) >= 8 && strings.Contains(msg, s) {
					t.Errorf("错误信息泄露了敏感内容（长度 %d）: %s", len(s), msg)
				}
			}
			t.Log(msg)
		})
	}
}
