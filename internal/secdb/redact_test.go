package secdb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestParseKeyRequiresExactly16Bytes(t *testing.T) {
	for _, s := range []string{"", "short", strings.Repeat("k", 15), strings.Repeat("k", 17), "中文密钥中文密钥"} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(长度 %d) 应失败", len(s))
		} else if len(s) >= 8 && strings.Contains(err.Error(), s) {
			t.Errorf("错误信息包含密钥内容: %v", err)
		}
	}
	// 只校验字节长度，不限制字符集
	for _, s := range []string{strings.Repeat("k", 16), "\x00\xff" + strings.Repeat("k", 14), "中文密钥1234"} {
		if _, err := ParseKey(s); err != nil {
			t.Errorf("ParseKey(长度 %d) 应成功: %v", len(s), err)
		}
	}
}

func TestKeyFormattingIsRedacted(t *testing.T) {
	k, err := ParseKey("SuperSecretKey16")
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{fmt.Sprint(k), fmt.Sprintf("%v %s %+v %#v", k, k, k, k),
		fmt.Sprintf("%v", struct{ K Key }{k}), fmt.Sprintf("%#v", struct{ K Key }{k})} {
		if strings.Contains(out, "SuperSecret") || !strings.Contains(out, "REDACTED") {
			t.Errorf("密钥输出未脱敏: %s", out)
		}
	}
}

func TestParamsOutputRedactsCredentialsAndExtraValues(t *testing.T) {
	p := Params{
		Type: "MySQL", DBIP: "198.51.100.9", DBPort: 3306, Username: "展示用户",
		Password: "pw-should-not-leak", DBName: "库名", YxzIP: "192.0.2.9",
		SessionID: "sess-should-not-leak",
		Extra:     map[string]json.RawMessage{"token": json.RawMessage(`"extra-should-not-leak"`)},
	}
	var logBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&logBuf, nil)).Info("会话", "params", p)
	var jsonLog bytes.Buffer
	slog.New(slog.NewJSONHandler(&jsonLog, nil)).Info("会话", "params", p)

	outputs := map[string]string{
		"%v": fmt.Sprintf("%v", p), "%+v": fmt.Sprintf("%+v", p), "%#v": fmt.Sprintf("%#v", p),
		"%s": fmt.Sprintf("%s", p), "指针%v": fmt.Sprintf("%v", &p),
		"slog文本": logBuf.String(), "slogJSON": jsonLog.String(),
	}
	for name, out := range outputs {
		if strings.Contains(out, "should-not-leak") {
			t.Errorf("%s 输出泄露了凭据或 Extra 值: %s", name, out)
		}
		// 非敏感字段与 Extra 字段名仍需可见，便于诊断
		for _, visible := range []string{"MySQL", "198.51.100.9", "3306", "展示用户", "库名", "192.0.2.9", "token"} {
			if !strings.Contains(out, visible) {
				t.Errorf("%s 输出缺少 %q: %s", name, visible, out)
			}
		}
	}
}
