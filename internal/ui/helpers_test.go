package ui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zK0G0w/jumpdb/internal/config"
	"github.com/zK0G0w/jumpdb/internal/secdb"
	"github.com/zK0G0w/jumpdb/internal/session"
	"github.com/zK0G0w/jumpdb/internal/ui"
)

const fixturePassword = "fixture-password-marker"

// 代理地址用 localhost 而非页面本就显示的 127.0.0.1，才能检查出代理地址泄露。
var fixtureParams = secdb.Params{Type: "MySQL", Username: "fixture-user", Password: fixturePassword, SessionID: "fixture-session-marker", YxzIP: "localhost"}

// handshakeLength 是代理收到的握手长度；每次申请的会话 ID 带两位序号以互相区分。
var handshakeLength = len("yabP") + len(fixtureParams.SessionID) + 2 + 1 + len(fixtureParams.Username)

func fixtureConfig() config.Data {
	return config.Data{Portal: "https://portal.invalid", TransKey: strings.Repeat("t", 16), DBKey: strings.Repeat("d", 16), Token: "fixture-token"}
}

// fakeSession 模拟以某个 Token 创建的门户客户端，不访问网络。
type fakeSession struct {
	portal *portal
	token  string
}

func (fake fakeSession) Login(ctx context.Context, id session.ResourceID) (secdb.Params, error) {
	p := fake.portal
	p.mu.Lock()
	login, hook := p.login, p.loginHook
	p.logins++
	params := fixtureParams
	params.SessionID += fmt.Sprintf("%02d", p.logins%100)
	p.mu.Unlock()
	if hook != nil {
		hook(fake.token)
	}
	if login != nil {
		return login(ctx, id)
	}
	return params, nil
}

// Activate 记录发起激活的客户端所用 Token，用于检查激活与申请会话是否为同一客户端。
func (fake fakeSession) Activate(context.Context, string) error {
	fake.portal.mu.Lock()
	defer fake.portal.mu.Unlock()
	fake.portal.activations = append(fake.portal.activations, fake.token)
	return nil
}

// portal 记录创建会话及激活时使用的 Token，并允许测试切换登录行为。
type portal struct {
	mu          sync.Mutex
	tokens      []string
	activations []string
	logins      int
	login       func(context.Context, session.ResourceID) (secdb.Params, error)
	loginHook   func(token string)
}

func (p *portal) newSession(_ context.Context, options session.Options) (ui.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokens = append(p.tokens, options.Token)
	return fakeSession{portal: p, token: options.Token}, nil
}

func (p *portal) setLoginHook(hook func(token string)) {
	p.mu.Lock()
	p.loginHook = hook
	p.mu.Unlock()
}

func (p *portal) activationTokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.activations...)
}

func (p *portal) setLogin(login func(context.Context, session.ResourceID) (secdb.Params, error)) {
	p.mu.Lock()
	p.login = login
	p.mu.Unlock()
}

func (p *portal) sessionTokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.tokens...)
}

// echoProxy 校验握手后发送 ready 并回显客户端数据，可接受多个连接。
func echoProxy(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := io.ReadFull(conn, make([]byte, handshakeLength)); err != nil {
					return
				}
				io.WriteString(conn, "successready")
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

// harness 是运行中的控制页及其合成依赖。
type harness struct {
	t      *testing.T
	base   string
	host   string
	token  string
	path   string
	portal *portal
	stop   func() error
}

func start(t *testing.T, configured bool) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jumpdb", "config.json")
	if configured {
		if err := config.Save(path, fixtureConfig()); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &portal{}
	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ui.Serve(ctx, listener, ui.Options{
			ConfigPath: path,
			NewSession: p.newSession,
			ProxyPort:  echoProxy(t),
			Ready:      func(address string) { ready <- address },
		})
	}()
	var address string
	select {
	case address = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("控制页未就绪")
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Hostname() != "127.0.0.1" {
		t.Fatal("控制页地址必须是 127.0.0.1")
	}
	stop := sync.OnceValue(func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			return context.DeadlineExceeded
		}
	})
	t.Cleanup(func() { stop() })
	return &harness{t: t, base: "http://" + parsed.Host, host: parsed.Host, token: parsed.Query().Get("token"), path: path, portal: p, stop: stop}
}

// request 以浏览器同源请求的方式访问控制页；headers 可覆盖默认的令牌、Origin 和 Host。
func (h *harness) request(method, path string, body any, headers map[string]string) (int, []byte) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, h.base+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	request.Header.Set("X-Jumpdb-Token", h.token)
	if method == http.MethodPost {
		request.Header.Set("Origin", h.base)
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		if key == "Host" {
			request.Host = value
			continue
		}
		if value == "" {
			request.Header.Del(key)
		} else {
			request.Header.Set(key, value)
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, raw
}

func (h *harness) post(path string, body any) (int, map[string]any) {
	h.t.Helper()
	status, raw := h.request(http.MethodPost, path, body, nil)
	var result map[string]any
	json.Unmarshal(raw, &result)
	return status, result
}

// state 返回解析后的状态以及原始响应，后者用于检查是否泄露凭据。
func (h *harness) state() (stateView, string) {
	h.t.Helper()
	status, raw := h.request(http.MethodGet, "/api/state", nil, nil)
	if status != http.StatusOK {
		h.t.Fatalf("读取状态失败：%d", status)
	}
	var view stateView
	if err := json.Unmarshal(raw, &view); err != nil {
		h.t.Fatal(err)
	}
	return view, string(raw)
}

func (h *harness) waitTunnel(id int, condition func(tunnelView) bool) tunnelView {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		view, _ := h.state()
		for _, tunnel := range view.Tunnels {
			if tunnel.ID == id && condition(tunnel) {
				return tunnel
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("等待隧道状态超时")
	return tunnelView{}
}

func (h *harness) startTunnel(port int) int {
	h.t.Helper()
	status, result := h.post("/api/tunnels/start", map[string]any{"id": "856", "id_type": "auto", "port": port, "name": "fixture"})
	if status != http.StatusOK {
		h.t.Fatalf("启动隧道失败：%d %v", status, result["error"])
	}
	return int(result["id"].(float64))
}

type stateView struct {
	Config struct {
		Configured bool              `json:"configured"`
		Error      string            `json:"error"`
		Portal     string            `json:"portal"`
		Resources  []config.Resource `json:"resources"`
		Theme      string            `json:"theme"`
	} `json:"config"`
	Tunnels []tunnelView `json:"tunnels"`
}

type tunnelView struct {
	ID           int    `json:"id"`
	Status       string `json:"status"`
	Failure      string `json:"failure"`
	FailureStage string `json:"failureStage"`
	Type         string `json:"type"`
	Username     string `json:"username"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Connections  []struct {
		ID     int      `json:"id"`
		Status string   `json:"status"`
		Stages []string `json:"stages"`
		Up     int64    `json:"up"`
		Down   int64    `json:"down"`
		Reason string   `json:"reason"`
	} `json:"connections"`
	Events []struct {
		Conn    int    `json:"conn"`
		Stage   string `json:"stage"`
		Success bool   `json:"success"`
	} `json:"events"`
}

func availablePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func dialTunnel(t *testing.T, port int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	ready := make([]byte, 5)
	if _, err := io.ReadFull(conn, ready); err != nil || string(ready) != "ready" {
		t.Fatal("隧道未完成握手转发")
	}
	if _, err := io.WriteString(conn, "query"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, ready); err != nil || string(ready) != "query" {
		t.Fatal("隧道回显不正确")
	}
	return conn
}

func assertReleased(t *testing.T, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal("端口未释放")
	}
	listener.Close()
}

func assertNoSecrets(t *testing.T, body string) {
	t.Helper()
	data := fixtureConfig()
	for _, secret := range []string{data.TransKey, data.DBKey, data.Token, fixturePassword, fixtureParams.SessionID, fixtureParams.YxzIP, "fixture-private"} {
		if strings.Contains(body, secret) {
			t.Fatal("响应泄露凭据或原始错误")
		}
	}
}
