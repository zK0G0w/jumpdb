// Package config 管理仅保存在本机的门户配置和凭据。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zK0G0w/jumpdb/internal/secdb"
	"github.com/zK0G0w/jumpdb/internal/session"
)

// Data 只用于本地持久化，不包含任何默认门户或协议密钥。
type Data struct {
	Portal   string `json:"portal"`
	TransKey string `json:"trans_key"`
	DBKey    string `json:"db_key"`
	Token    string `json:"token"`
	// Resources 是 Web 控制页保存的常用资源，不含凭据；旧配置缺少该字段时为空。
	Resources []Resource `json:"resources,omitempty"`
	// Theme 是 Web 控制页的外观偏好，取值 light 或 dark；空值表示跟随系统，不写入文件。
	Theme string `json:"theme,omitempty"`
}

// Resource 是一个可在 Web 控制页一键启动的资源条目，不会自动启动。
type Resource struct {
	// ID 是门户资源 ID 原文，按 IDType 解释，不允许为空。
	ID string `json:"id"`
	// IDType 取值 auto、string 或 number，语义与 connect --id-type 一致。
	IDType string `json:"id_type"`
	// Port 是本地监听端口，范围 1..65535。
	Port int `json:"port"`
	// Name 是可选别名，最多 64 个字符，不允许控制字符。
	Name string `json:"name,omitempty"`
}

// Validate 检查资源条目；错误不包含用户输入。
func (resource Resource) Validate() error {
	if _, err := ParseResourceID(resource.ID, resource.IDType); err != nil {
		return err
	}
	if resource.Port < 1 || resource.Port > 65535 {
		return errors.New("资源端口必须在 1..65535 范围内")
	}
	if utf8.RuneCountInString(resource.Name) > 64 || strings.IndexFunc(resource.Name, unicode.IsControl) >= 0 || !utf8.ValidString(resource.Name) {
		return errors.New("资源别名最多 64 个字符且不能含控制字符")
	}
	return nil
}

// ParseResourceID 按类型解释资源 ID：auto 将无前导零的十进制非负整数作为数字，
// 其余作为字符串；string 保留原文；number 强制 int64 范围内的非负整数。
func ParseResourceID(value, kind string) (session.ResourceID, error) {
	if value == "" || (kind != "auto" && kind != "string" && kind != "number") {
		return session.ResourceID{}, errors.New("资源 ID 或类型无效")
	}
	digits := true
	for _, character := range value {
		if character < '0' || character > '9' {
			digits = false
			break
		}
	}
	if kind == "number" || kind == "auto" && digits && (len(value) == 1 || value[0] != '0') {
		number, err := strconv.ParseInt(value, 10, 64)
		if !digits || err != nil {
			return session.ResourceID{}, errors.New("数字资源 ID 必须是 int64 范围内的非负整数")
		}
		return session.NumberID(number), nil
	}
	return session.StringID(value), nil
}

func (Data) String() string   { return "config.Data([REDACTED])" }
func (Data) GoString() string { return "config.Data([REDACTED])" }

// Validate 检查生产配置；错误不包含用户输入。
func (data Data) Validate() error {
	portal, err := url.Parse(data.Portal)
	if err != nil || portal.Scheme != "https" || portal.Hostname() == "" || portal.User != nil || portal.RawQuery != "" || portal.ForceQuery || portal.Fragment != "" || (portal.Path != "" && portal.Path != "/") || portal.RawPath != "" || strings.ContainsAny(data.Portal, "\r\n\t ") {
		return errors.New("门户地址必须是无账号、查询参数和路径的 HTTPS 地址")
	}
	if port := portal.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return errors.New("门户端口无效")
		}
	}
	if _, err := secdb.ParseKey(data.TransKey); err != nil {
		return errors.New("传输密钥必须恰好为 16 字节")
	}
	if _, err := secdb.ParseKey(data.DBKey); err != nil {
		return errors.New("数据库密钥必须恰好为 16 字节")
	}
	if data.Token == "" {
		return errors.New("Token 不能为空")
	}
	for _, character := range data.Token {
		if character <= 0x20 || character >= 0x7f || strings.ContainsRune(";\"\\", character) {
			return errors.New("Token 含有不允许的 Cookie 字符，请仅录入 Token 值")
		}
	}
	for _, resource := range data.Resources {
		if err := resource.Validate(); err != nil {
			return err
		}
	}
	if data.Theme != "" && data.Theme != "light" && data.Theme != "dark" {
		return errors.New("控制页主题无效")
	}
	return nil
}

// DefaultPath 优先使用绝对路径的 XDG_CONFIG_HOME，否则回退到 ~/.config；不查找旧配置。
func DefaultPath() (string, error) {
	directory := os.Getenv("XDG_CONFIG_HOME")
	// 相对路径会随工作目录变化，不能用来定位用户凭据。
	if !filepath.IsAbs(directory) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("无法确定用户配置目录")
		}
		directory = filepath.Join(home, ".config")
	}
	return filepath.Join(directory, "jumpdb", "config.json"), nil
}

// Load 只读取权限受限的普通文件；不存在时返回 os.ErrNotExist。
func Load(path string) (Data, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Data{}, os.ErrNotExist
	}
	if err != nil {
		return Data{}, errors.New("无法检查本地配置")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return Data{}, errors.New("配置必须是权限为 0600 的普通文件，请先检查文件及权限")
	}
	file, err := os.Open(path)
	if err != nil {
		return Data{}, errors.New("无法读取本地配置")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(raw) > 65536 {
		return Data{}, errors.New("无法读取本地配置或配置过大")
	}
	var data Data
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&data) != nil || decoder.Decode(new(any)) != io.EOF {
		return Data{}, errors.New("本地配置格式无效，未修改原文件")
	}
	if err := data.Validate(); err != nil {
		return Data{}, err
	}
	return data, nil
}

// Update 重新读取磁盘上的当前配置（不存在时从空配置开始），应用 change 后原子保存。
// 只改动 change 涉及的字段，保留其他进程在此期间写入的内容（如 Web 控制页的资源列表）；
// 两个进程同一瞬间保存时后写入者生效，原子替换保证文件不损坏。
func Update(path string, change func(*Data) error) (Data, error) {
	data, err := Load(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Data{}, err
	}
	if err := change(&data); err != nil {
		return Data{}, err
	}
	if err := Save(path, data); err != nil {
		return Data{}, err
	}
	return data, nil
}

// Save 在同目录写入 0600 临时文件后原子替换，失败时保留原配置。
func Save(path string, data Data) error {
	if err := data.Validate(); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return errors.New("无法创建配置目录")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return errors.New("配置目录必须是普通目录")
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return errors.New("无法限制配置目录权限")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("拒绝替换非普通配置文件")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("无法检查原配置")
	}
	file, err := os.CreateTemp(directory, ".config-*")
	if err != nil {
		return errors.New("无法创建临时配置")
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return errors.New("无法限制配置文件权限")
	}
	if err := json.NewEncoder(file).Encode(data); err != nil {
		return errors.New("无法写入配置")
	}
	if err := file.Sync(); err != nil {
		return errors.New("无法同步配置")
	}
	if err := file.Close(); err != nil {
		return errors.New("无法关闭配置文件")
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return errors.New("无法替换配置，原配置未修改")
	}
	return nil
}
