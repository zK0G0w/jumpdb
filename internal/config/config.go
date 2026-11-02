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

	"github.com/zK0G0w/jumpdb/internal/secdb"
)

// Data 只用于本地持久化，不包含任何默认门户或协议密钥。
type Data struct {
	Portal   string `json:"portal"`
	TransKey string `json:"trans_key"`
	DBKey    string `json:"db_key"`
	Token    string `json:"token"`
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
