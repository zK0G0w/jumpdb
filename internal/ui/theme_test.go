package ui_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zK0G0w/jumpdb/internal/config"
)

func TestThemeIsPersistedAndPresetInPage(t *testing.T) {
	h := start(t, true)
	view, _ := h.state()
	if view.Config.Theme != "system" {
		t.Fatal("默认主题应为跟随系统")
	}
	resources := []config.Resource{{ID: "856", IDType: "auto", Port: 9011}}
	if status, _ := h.post("/api/resources", map[string]any{"resources": resources}); status != http.StatusOK {
		t.Fatal("保存资源失败")
	}
	if status, result := h.post("/api/theme", map[string]any{"theme": "dark"}); status != http.StatusOK {
		t.Fatalf("保存主题失败：%v", result["error"])
	}
	loaded, err := config.Load(h.path)
	if err != nil || loaded.Theme != "dark" || loaded.Token != fixtureConfig().Token || len(loaded.Resources) != 1 {
		t.Fatal("保存主题应只改变主题字段")
	}
	if view, _ := h.state(); view.Config.Theme != "dark" {
		t.Fatal("状态未返回已保存的主题")
	}
	// 页面直接带上已保存的主题，脚本取回状态前不会先以系统配色渲染。
	if status, body := h.request(http.MethodGet, "/?token="+h.token, nil, nil); status != http.StatusOK || !strings.Contains(string(body), `data-theme="dark"`) {
		t.Fatal("页面未预置已保存的主题")
	}
	if status, _ := h.post("/api/theme", map[string]any{"theme": "system"}); status != http.StatusOK {
		t.Fatal("恢复跟随系统失败")
	}
	if loaded, _ := config.Load(h.path); loaded.Theme != "" {
		t.Fatal("跟随系统应以空值保存")
	}
	if _, body := h.request(http.MethodGet, "/?token="+h.token, nil, nil); !strings.Contains(string(body), `data-theme="system"`) {
		t.Fatal("跟随系统时页面应保持默认主题")
	}
	for _, theme := range []string{"", "auto", `dark" onload="x`} {
		if status, _ := h.post("/api/theme", map[string]any{"theme": theme}); status != http.StatusBadRequest {
			t.Fatalf("应拒绝无效主题 %q", theme)
		}
	}
	if status, _ := h.request(http.MethodPost, "/api/theme", map[string]any{"theme": "dark"}, map[string]string{"Origin": "https://evil.invalid"}); status != http.StatusForbidden {
		t.Fatal("跨站请求不得修改主题")
	}
}

func TestThemeRequiresConfiguration(t *testing.T) {
	h := start(t, false)
	if status, _ := h.post("/api/theme", map[string]any{"theme": "dark"}); status != http.StatusBadRequest {
		t.Fatal("未配置时不应保存主题")
	}
	if view, _ := h.state(); view.Config.Configured || view.Config.Theme != "system" {
		t.Fatal("未配置时主题应为跟随系统")
	}
}
