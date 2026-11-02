package config_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zK0G0w/jumpdb/internal/config"
)

func fixture() config.Data {
	return config.Data{Portal: "https://portal.invalid", TransKey: "0123456789abcdef", DBKey: "fedcba9876543210", Token: "fixture-token"}
}

func TestSaveLoadAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jumpdb", "config.json")
	if _, err := config.Load(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("缺失文件应明确报告")
	}
	data := fixture()
	for range 2 {
		if err := config.Save(path, data); err != nil {
			t.Fatal(err)
		}
		for target, permission := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != permission {
				t.Fatal("权限不正确")
			}
		}
		loaded, err := config.Load(path)
		if err != nil || loaded != data {
			t.Fatal("保存后读取不一致")
		}
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := config.Load(path); err == nil {
			t.Fatal("不应读取权限过宽的文件")
		}
		data.Token = "fixture-updated-token"
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatal("遗留了临时配置文件")
	}
}

func TestInvalidSavePreservesOriginal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := fixture()
	if err := config.Save(path, data); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	data.Token = ""
	if err := config.Save(path, data); err == nil {
		t.Fatal("空 Token 应拒绝保存")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("失败保存改变了原文件")
	}
}

func TestRejectsSymlinksAndMalformedFiles(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("fixture-private-marker"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(link); err == nil {
		t.Fatal("不应读取符号链接")
	}
	if err := config.Save(link, fixture()); err == nil {
		t.Fatal("不应覆盖符号链接")
	}
	if _, err := config.Load(target); err == nil || strings.Contains(err.Error(), "fixture-private-marker") {
		t.Fatal("格式错误缺失或泄露内容")
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != "fixture-private-marker" {
		t.Fatal("损坏文件被修改")
	}
}

func TestValidationAndRedaction(t *testing.T) {
	for _, portal := range []string{"http://portal.invalid", "https://user@portal.invalid", "https://portal.invalid/path", "https://portal.invalid?q=1", "https://portal.invalid:99999", "https://portal.invalid/#fragment", "https://portal.invalid?"} {
		data := fixture()
		data.Portal = portal
		if err := data.Validate(); err == nil || strings.Contains(err.Error(), portal) {
			t.Fatal("地址校验缺失或泄露地址")
		}
	}
	for _, token := range []string{"", "fixture\nvalue", "fixture;value", "fixture value", "fixture\"value", "fixture\\value"} {
		data := fixture()
		data.Token = token
		if err := data.Validate(); err == nil {
			t.Fatal("接受了无效 Token")
		}
	}
	data := fixture()
	for _, output := range []string{fmt.Sprint(data), fmt.Sprintf("%+v", data), fmt.Sprintf("%#v", data)} {
		for _, secret := range []string{data.Portal, data.TransKey, data.DBKey, data.Token} {
			if strings.Contains(output, secret) {
				t.Fatal("配置格式化泄露内容")
			}
		}
	}
}
