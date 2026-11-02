package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zK0G0w/jumpdb/internal/config"
)

func TestThemeRoundTripAndDefaultOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, fixture()); err != nil {
		t.Fatal(err)
	}
	// 跟随系统以空值表示且不写入文件，未改主题的配置仍可被不认识该字段的旧版本读取。
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "theme") {
		t.Fatal("默认主题不应写入配置文件")
	}
	for _, theme := range []string{"light", "dark", ""} {
		data := fixture()
		data.Theme = theme
		if err := config.Save(path, data); err != nil {
			t.Fatalf("保存主题 %q 失败：%v", theme, err)
		}
		if loaded, err := config.Load(path); err != nil || loaded.Theme != theme {
			t.Fatalf("主题 %q 保存后读取不一致", theme)
		}
	}
}

func TestInvalidThemeIsRejected(t *testing.T) {
	for _, theme := range []string{"system", "Dark", "auto", " light"} {
		data := fixture()
		data.Theme = theme
		if err := data.Validate(); err == nil {
			t.Fatalf("应拒绝无效主题 %q", theme)
		}
	}
}
