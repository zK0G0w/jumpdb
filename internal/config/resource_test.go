package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zK0G0w/jumpdb/internal/config"
	"github.com/zK0G0w/jumpdb/internal/session"
)

func TestResourcesRoundTripAndLegacyFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{"portal":"https://portal.invalid","trans_key":"` + strings.Repeat("t", 16) + `","db_key":"` + strings.Repeat("d", 16) + `","token":"fixture-token"}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil || len(loaded.Resources) != 0 {
		t.Fatal("旧格式配置应按空资源列表读取")
	}
	loaded.Resources = []config.Resource{
		{ID: "856", IDType: "auto", Port: 9011, Name: "订单库-生产"},
		{ID: "00856", IDType: "string", Port: 9012},
	}
	if err := config.Save(path, loaded); err != nil {
		t.Fatal(err)
	}
	again, err := config.Load(path)
	if err != nil || !reflect.DeepEqual(again, loaded) {
		t.Fatal("资源列表保存后读取不一致")
	}
}

func TestInvalidResourcesAreRejected(t *testing.T) {
	for name, resource := range map[string]config.Resource{
		"空 ID":    {ID: "", IDType: "auto", Port: 9011},
		"未知类型":    {ID: "856", IDType: "bad", Port: 9011},
		"数字溢出":    {ID: "9223372036854775808", IDType: "number", Port: 9011},
		"端口过小":    {ID: "856", IDType: "auto", Port: 0},
		"端口过大":    {ID: "856", IDType: "auto", Port: 65536},
		"别名含控制字符": {ID: "856", IDType: "auto", Port: 9011, Name: "fixture\nname"},
		"别名过长":    {ID: "856", IDType: "auto", Port: 9011, Name: strings.Repeat("名", 65)},
	} {
		data := fixture()
		data.Resources = []config.Resource{resource}
		if err := data.Validate(); err == nil {
			t.Fatalf("%s：应拒绝无效资源", name)
		}
	}
}

func TestParseResourceIDKeepsCLISemantics(t *testing.T) {
	for _, test := range []struct {
		value, kind string
		want        session.ResourceID
		wantErr     bool
	}{
		{"856", "auto", session.NumberID(856), false}, {"00856", "auto", session.StringID("00856"), false},
		{"856", "string", session.StringID("856"), false}, {"00856", "number", session.NumberID(856), false},
		{"resource", "auto", session.StringID("resource"), false}, {"abc", "number", session.ResourceID{}, true},
		{"9223372036854775808", "auto", session.ResourceID{}, true}, {"", "auto", session.ResourceID{}, true}, {"856", "bad", session.ResourceID{}, true},
	} {
		id, err := config.ParseResourceID(test.value, test.kind)
		if (err != nil) != test.wantErr {
			t.Fatalf("%s/%s 错误判断不正确", test.value, test.kind)
		}
		if !test.wantErr && !reflect.DeepEqual(id, test.want) {
			t.Fatalf("%s/%s 类型不正确", test.value, test.kind)
		}
	}
}

func TestUpdatePreservesFieldsWrittenByOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jumpdb", "config.json")
	if _, err := config.Update(path, func(data *config.Data) error {
		*data = fixture()
		return nil
	}); err != nil {
		t.Fatal("不存在时应从空配置开始更新")
	}
	// 模拟另一个进程在本次读取后写入资源列表。
	other := fixture()
	other.Resources = []config.Resource{{ID: "856", IDType: "auto", Port: 9011}}
	if err := config.Save(path, other); err != nil {
		t.Fatal(err)
	}
	updated, err := config.Update(path, func(data *config.Data) error {
		data.Token = "fixture-replacement"
		return nil
	})
	if err != nil || updated.Token != "fixture-replacement" || len(updated.Resources) != 1 {
		t.Fatal("更新 Token 丢失了其他进程写入的资源")
	}
	loaded, err := config.Load(path)
	if err != nil || !reflect.DeepEqual(loaded, updated) {
		t.Fatal("返回值与磁盘内容不一致")
	}

	before, _ := os.ReadFile(path)
	if _, err := config.Update(path, func(*config.Data) error { return errors.New("fixture") }); err == nil {
		t.Fatal("修改函数失败时应返回错误")
	}
	if _, err := config.Update(path, func(data *config.Data) error {
		data.Token = ""
		return nil
	}); err == nil {
		t.Fatal("修改结果无效时应拒绝保存")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("失败的更新改变了原文件")
	}
}
