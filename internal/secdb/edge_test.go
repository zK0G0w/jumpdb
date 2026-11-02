package secdb

import (
	"fmt"
	"strings"
	"testing"
)

// ssoParamWithIV 用指定外层 IV 构造 ssosecdb 参数（不含协议头），内层固定放在 outer["db"]。
func ssoParamWithIV(t *testing.T, transKey, dbKey Key, iv string, outer map[string]any, inner map[string]any) string {
	t.Helper()
	outer["action"] = "db"
	outer["db"] = ssoLayerOf(t, dbKey, pkcs7(jsonOf(t, inner)))
	ivOut, b64 := encryptRawIV(t, transKey, iv, pkcs7(jsonOf(t, outer)))
	return ivOut[8:16] + b64[0:8] + ivOut[0:8] + b64[8:]
}

// "/" 是合法的 Base64 和 IV 字符，参数首尾的 "/" 不能被当作分隔符误删。
func TestDecodeSSOSecDBKeepsSlashesThatBelongToParam(t *testing.T) {
	_, transKey, dbKey := loadVectors(t)
	inner := map[string]any{"username": "斜杠用户", "yxzIp": "192.0.2.40", "sessionId": "slash-sess"}

	// 外层 IV 第 9 个字符为 "/"，参数首字符就是 "/"
	leading := ssoParamWithIV(t, transKey, dbKey, "AAAAAAAA/BBBBBBB", map[string]any{}, inner)
	if leading[0] != '/' {
		t.Fatalf("构造失败：参数首字符为 %q", leading[0])
	}

	// 同时调整填充字段的长度和内容，直到密文长度是 48 的倍数（Base64 无 "=" 填充）且最后一个字符是 "/"
	var trailing string
	for i := 0; i < 100000 && trailing == ""; i++ {
		pad := strings.Repeat("x", i%48) + fmt.Sprint(i)
		p := ssoParamWithIV(t, transKey, dbKey, fixedIV, map[string]any{"pad": pad}, inner)
		if strings.HasSuffix(p, "/") && !strings.Contains(p, "=") {
			trailing = p
		}
	}
	if trailing == "" {
		t.Fatal("未能构造以 \"/\" 结尾的参数")
	}

	for name, param := range map[string]string{"首字符为斜杠": leading, "末字符为斜杠": trailing} {
		for form, input := range map[string]string{
			"无协议头":              param,
			"ssosecdb//带结尾分隔符":  "ssosecdb//" + param + "/",
			"ssosecdb://带结尾分隔符": "ssosecdb://" + param + "/",
			"ssosecdb//无结尾分隔符":  "ssosecdb//" + param,
			"首尾空白":              "  ssosecdb//" + param + "/\n",
		} {
			t.Run(name+"/"+form, func(t *testing.T) {
				got, err := DecodeSSOSecDB(transKey, dbKey, input)
				if err != nil {
					t.Fatalf("DecodeSSOSecDB: %v", err)
				}
				if got.Username != "斜杠用户" || got.SessionID != "slash-sess" {
					t.Errorf("解出字段不对: %v", got)
				}
			})
		}
	}
}

// 纯数字账号等 JSON 数字按原始十进制文本转为字符串，与旧实现 str() 的用法兼容。
func TestDecodeDBFieldAcceptsNumbersInStringFields(t *testing.T) {
	_, _, dbKey := loadVectors(t)
	input := dbFieldOf(t, dbKey, pkcs7([]byte(
		`{"username":12345,"dbname":0,"dbIp":"198.51.100.41","yxzIp":"192.0.2.41","sessionId":20261102}`)))
	got, err := DecodeDBField(dbKey, input)
	if err != nil {
		t.Fatalf("DecodeDBField: %v", err)
	}
	if got.Username != "12345" || got.DBName != "0" || got.SessionID != "20261102" {
		t.Errorf("数字字段未转为字符串: username=%q dbname=%q sessionId=%q", got.Username, got.DBName, got.SessionID)
	}
}
