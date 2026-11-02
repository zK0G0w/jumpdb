package cli

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zK0G0w/jumpdb/internal/config"
	"github.com/zK0G0w/jumpdb/internal/secdb"
	"github.com/zK0G0w/jumpdb/internal/session"
)

type safeBuffer struct {
	mutex sync.Mutex
	data  bytes.Buffer
}

func (buffer *safeBuffer) Write(data []byte) (int, error) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.data.Write(data)
}

func (buffer *safeBuffer) String() string {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.data.String()
}

func fixtureConfig() config.Data {
	return config.Data{Portal: "https://portal.invalid", TransKey: strings.Repeat("t", 16), DBKey: strings.Repeat("d", 16), Token: "fixture-token"}
}

func testApp(t *testing.T) (*application, *safeBuffer, string) {
	t.Helper()
	output := &safeBuffer{}
	path := filepath.Join(t.TempDir(), "jumpdb", "config.json")
	app := &application{
		out: output, errOut: output, inputTerminal: true,
		path: func() (string, error) { return path, nil },
		newSession: func(context.Context, session.Options) (sessionClient, error) {
			t.Error("不应访问门户")
			return nil, errors.New("不应访问门户")
		},
	}
	return app, output, path
}

func TestArgumentsAndNonInteractiveInput(t *testing.T) {
	for _, args := range [][]string{
		{"unknown-private-argument"}, {"token"}, {"token", "set", "fixture-token"},
		{"token", "set", "--token=fixture-token"}, {"connect"}, {"connect", "--id", "856", "extra"},
		{"connect", "--id", "856", "--port", "0"}, {"connect", "--id", "856", "--port", "65536"},
		{"connect", "--id", "856", "--port", "fixture-token"}, {"connect", "--id", "856", "--id-type", "bad"},
		{"connect", "--id", "9223372036854775808"}, {"connect", "--id", "abc", "--id-type", "number"},
	} {
		app, output, _ := testApp(t)
		if code := app.run(context.Background(), args); code != 2 {
			t.Fatalf("参数错误退出码为 %d", code)
		}
		if strings.Contains(output.String(), "fixture-token") || strings.Contains(output.String(), "unknown-private") {
			t.Fatal("参数错误泄露输入")
		}
	}
	for _, args := range [][]string{nil, {"--help"}, {"token", "set", "--help"}, {"connect", "--help"}} {
		app, output, _ := testApp(t)
		if app.run(context.Background(), args) != 0 || !strings.Contains(output.String(), "--show-password") {
			t.Fatal("帮助不可用")
		}
	}
	app, _, _ := testApp(t)
	app.inputTerminal = false
	if app.run(context.Background(), []string{"token", "set"}) != 1 {
		t.Fatal("应拒绝非终端输入")
	}
}

func TestTokenFirstSetupUpdateAndReconfigure(t *testing.T) {
	app, output, path := testApp(t)
	data := fixtureConfig()
	values := []string{data.Portal, data.TransKey, data.DBKey, data.Token}
	app.prompt = func(context.Context, string) (string, error) {
		if len(values) == 0 {
			t.Fatal("多余录入")
		}
		value := values[0]
		values = values[1:]
		return value, nil
	}
	for _, configure := range []bool{false, false, true} {
		args := []string{"token", "set"}
		if configure {
			args = append(args, "--configure")
			data.Portal = "https://second.invalid"
			values = []string{data.Portal, data.TransKey, data.DBKey, data.Token}
		}
		if app.run(context.Background(), args) != 0 || len(values) != 0 {
			t.Fatal("录入失败或字段数量不正确")
		}
		loaded, err := config.Load(path)
		if err != nil || !reflect.DeepEqual(loaded, data) {
			t.Fatal("持久化数据不正确")
		}
		data.Token = "fixture-replacement"
		values = []string{data.Token}
	}
	for _, secret := range []string{data.Portal, data.TransKey, data.DBKey, data.Token} {
		if strings.Contains(output.String(), secret) {
			t.Fatal("录入输出泄露配置")
		}
	}
}

func TestTokenSetPreservesResourcesWrittenDuringPrompt(t *testing.T) {
	app, _, path := testApp(t)
	if err := config.Save(path, fixtureConfig()); err != nil {
		t.Fatal(err)
	}
	resources := []config.Resource{{ID: "856", IDType: "auto", Port: 9011, Name: "fixture"}}
	app.prompt = func(context.Context, string) (string, error) {
		// 模拟 Web 控制页在终端录入期间保存了资源列表。
		data := fixtureConfig()
		data.Resources = resources
		if err := config.Save(path, data); err != nil {
			t.Fatal(err)
		}
		return "fixture-replacement", nil
	}
	if app.run(context.Background(), []string{"token", "set"}) != 0 {
		t.Fatal("更新 Token 失败")
	}
	loaded, err := config.Load(path)
	if err != nil || loaded.Token != "fixture-replacement" || !reflect.DeepEqual(loaded.Resources, resources) {
		t.Fatal("更新 Token 覆盖了录入期间写入的资源列表")
	}
}

func TestCancelledAndInvalidInputDoesNotSave(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, cancelled := range []bool{false, true} {
			app, _, path := testApp(t)
			if existing {
				if err := config.Save(path, fixtureConfig()); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(path)
			app.prompt = func(context.Context, string) (string, error) {
				if cancelled {
					return "", context.Canceled
				}
				return "", nil
			}
			want := 1
			if cancelled {
				want = 0
			}
			if code := app.run(context.Background(), []string{"token", "set"}); code != want {
				t.Fatalf("退出码 %d，期望 %d", code, want)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("取消或错误录入改变了原文件")
			}
		}
	}
}

func TestDamagedConfigurationAndPasswordRedirection(t *testing.T) {
	app, _, path := testApp(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture-damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	app.prompt = func(context.Context, string) (string, error) {
		t.Fatal("不应覆盖损坏配置")
		return "", nil
	}
	if app.run(context.Background(), []string{"token", "set", "--configure"}) != 1 {
		t.Fatal("损坏配置没有报错")
	}
	if app.run(context.Background(), []string{"connect", "--id", "856", "--show-password"}) != 1 {
		t.Fatal("密码不应重定向")
	}
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

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待可观测结果超时")
}

func assertReleased(t *testing.T, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal("停止后未释放端口")
	}
	listener.Close()
}

func encodeFixture(t *testing.T, params secdb.Params) string {
	t.Helper()
	key, _ := secdb.ParseKey(fixtureConfig().DBKey)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"type": 8, "username": params.Username, "password": params.Password, "sessionId": params.SessionID, "yxzIp": params.YxzIP})
	padding := aes.BlockSize - len(raw)%aes.BlockSize
	raw = append(raw, bytes.Repeat([]byte{byte(padding)}, padding)...)
	iv := []byte(strings.Repeat("i", aes.BlockSize))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(raw, raw)
	encoded := base64.StdEncoding.EncodeToString(raw)
	return string(iv[:8]) + encoded[:8] + string(iv[8:]) + encoded[8:]
}

func attachPortal(t *testing.T, app *application, handler http.Handler) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	app.newSession = func(ctx context.Context, options session.Options) (sessionClient, error) {
		options.Portal = server.URL
		return session.New(ctx, options)
	}
}

func TestConnectIDsCredentialsAndPerConnectionSessions(t *testing.T) {
	for _, test := range []struct {
		id, kind string
		want     any
		show     bool
	}{
		{"856", "auto", float64(856), false}, {"00856", "auto", "00856", false},
		{"856", "string", "856", true}, {"00856", "number", float64(856), false},
		{"resource", "auto", "resource", false},
	} {
		t.Run(test.id+test.kind, func(t *testing.T) {
			app, output, path := testApp(t)
			if err := config.Save(path, fixtureConfig()); err != nil {
				t.Fatal(err)
			}
			params := secdb.Params{Username: "fixture-user", Password: "fixture-private-password", SessionID: "fixture-session", YxzIP: "127.0.0.1"}
			encrypted := encodeFixture(t, params)
			var logins, activations atomic.Int32
			attachPortal(t, app, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var body struct {
					B struct {
						IDs       []any  `json:"ids"`
						SessionID string `json:"sessionId"`
					} `json:"b"`
				}
				if json.NewDecoder(request.Body).Decode(&body) != nil {
					t.Error("请求格式无效")
					return
				}
				if request.URL.Path == "/3.0/hostOpsService/checkSSOTool" {
					if body.B.SessionID != params.SessionID {
						t.Error("激活未携带本次会话 ID")
					}
					activations.Add(1)
					fmt.Fprint(writer, `{"code":0,"data":{"status":1}}`)
					return
				}
				if len(body.B.IDs) == 0 {
					fmt.Fprint(writer, `{}`)
					return
				}
				if body.B.IDs[0] != test.want {
					t.Error("CLI 未保留预期 ID 类型")
				}
				logins.Add(1)
				json.NewEncoder(writer).Encode(map[string]any{"code": 0, "data": map[string]string{"db": encrypted}})
			}))
			proxy, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer proxy.Close()
			app.proxyPort = proxy.Addr().(*net.TCPAddr).Port
			proxyDone := make(chan struct{})
			go func() {
				defer close(proxyDone)
				var workers sync.WaitGroup
				defer workers.Wait()
				for range 2 {
					connection, err := proxy.Accept()
					if err != nil {
						return
					}
					workers.Add(1)
					go func() {
						defer workers.Done()
						defer connection.Close()
						connection.SetDeadline(time.Now().Add(5 * time.Second))
						expected := handshakeFor(params)
						got := make([]byte, len(expected))
						if _, err := io.ReadFull(connection, got); err != nil || string(got) != expected {
							t.Error("代理握手不正确")
							return
						}
						io.WriteString(connection, "successready")
						io.Copy(connection, connection)
					}()
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			port := availablePort(t)
			args := []string{"connect", "--id", test.id, "--id-type", test.kind, "--port", strconv.Itoa(port)}
			if test.show {
				app.outputTerminal = true
				args = append(args, "--show-password")
			}
			done := make(chan int, 1)
			go func() { done <- app.run(ctx, args) }()
			waitFor(t, func() bool { return strings.Contains(output.String(), "等待客户端") })
			if strings.Contains(output.String(), "代理握手成功\n") || logins.Load() != 1 {
				t.Fatal("初始申请被误报为握手成功")
			}
			for range 2 {
				connection, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				connection.SetDeadline(time.Now().Add(5 * time.Second))
				ready := make([]byte, 5)
				if _, err := io.ReadFull(connection, ready); err != nil || string(ready) != "ready" {
					t.Fatal("数据库前导字节丢失")
				}
				if _, err := io.WriteString(connection, "query"); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(connection, ready); err != nil || string(ready) != "query" {
					t.Fatal("转发失败")
				}
			}
			cancel()
			select {
			case code := <-done:
				if code != 0 {
					t.Fatal("正常取消退出码不正确")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("取消未停止 CLI")
			}
			<-proxyDone
			if logins.Load() != 3 {
				t.Fatal("不是初始申请加每客户端一次申请")
			}
			if activations.Load() != 2 {
				t.Fatal("不是每个客户端连接各激活一次（初始会话不激活）")
			}
			if strings.Contains(output.String(), params.Password) != test.show {
				t.Fatal("密码显式显示策略不正确")
			}
			for _, secret := range []string{params.SessionID, fixtureConfig().Token, fixtureConfig().DBKey, fixtureConfig().Portal} {
				if strings.Contains(output.String(), secret) {
					t.Fatal("输出泄露凭据")
				}
			}
			assertReleased(t, port)
		})
	}
}

func TestConnectFailuresExitAndReleasePort(t *testing.T) {
	for _, stage := range []string{"probe", "initial", "decode", "session", "dial", "activate", "handshake"} {
		t.Run(stage, func(t *testing.T) {
			app, output, path := testApp(t)
			if err := config.Save(path, fixtureConfig()); err != nil {
				t.Fatal(err)
			}
			params := secdb.Params{Username: "fixture-user", SessionID: "fixture-session", YxzIP: "127.0.0.1"}
			encrypted := encodeFixture(t, params)
			var logins atomic.Int32
			attachPortal(t, app, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var body struct {
					B struct {
						IDs []any `json:"ids"`
					} `json:"b"`
				}
				json.NewDecoder(request.Body).Decode(&body)
				if request.URL.Path == "/3.0/hostOpsService/checkSSOTool" {
					if stage == "activate" {
						fmt.Fprint(writer, `{"code":500,"msg":"fixture-private-response"}`)
					} else {
						fmt.Fprint(writer, `{"code":0,"data":{"status":1}}`)
					}
					return
				}
				if len(body.B.IDs) == 0 {
					if stage == "probe" {
						writer.WriteHeader(http.StatusForbidden)
						fmt.Fprint(writer, "fixture-private-response")
					} else {
						fmt.Fprint(writer, `{}`)
					}
					return
				}
				count := logins.Add(1)
				if stage == "initial" || stage == "session" && count > 1 {
					fmt.Fprint(writer, `{"code":901,"msg":"fixture-private-response"}`)
					return
				}
				if stage == "decode" {
					fmt.Fprint(writer, `{"code":0,"data":{"db":"fixture-private-response"}}`)
					return
				}
				json.NewEncoder(writer).Encode(map[string]any{"code": 0, "data": map[string]string{"db": encrypted}})
			}))
			app.proxyPort = availablePort(t)
			if stage == "activate" || stage == "handshake" {
				proxy, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer proxy.Close()
				app.proxyPort = proxy.Addr().(*net.TCPAddr).Port
				go func() {
					connection, err := proxy.Accept()
					if err != nil {
						return
					}
					defer connection.Close()
					connection.SetDeadline(time.Now().Add(5 * time.Second))
					handshake := make([]byte, len(handshakeFor(params)))
					io.ReadFull(connection, handshake)
					io.WriteString(connection, "failure")
				}()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			port := availablePort(t)
			done := make(chan int, 1)
			go func() { done <- app.run(ctx, []string{"connect", "--id", "856", "--port", strconv.Itoa(port)}) }()
			if stage == "session" || stage == "dial" || stage == "activate" || stage == "handshake" {
				waitFor(t, func() bool { return strings.Contains(output.String(), "等待客户端") })
				connection, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				connection.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, err := connection.Read(make([]byte, 1)); err == nil {
					t.Fatal("失败连接未关闭")
				}
			}
			if code := <-done; code != 1 {
				t.Fatalf("失败退出码为 %d", code)
			}
			want := map[string]string{"probe": "门户时间探测失败", "initial": "初始会话申请失败", "decode": "会话解码失败", "session": "客户端会话申请失败", "dial": "代理连接失败", "activate": "会话激活失败", "handshake": "代理握手失败"}[stage]
			if !strings.Contains(output.String(), want) || strings.Contains(output.String(), "fixture-private-response") {
				t.Fatal("错误阶段缺失或泄露远端响应")
			}
			assertReleased(t, port)
		})
	}
}

func TestPortConflictBeforePortalAccess(t *testing.T) {
	app, output, path := testApp(t)
	if err := config.Save(path, fixtureConfig()); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if app.run(context.Background(), []string{"connect", "--id", "856", "--port", strconv.Itoa(port)}) != 1 || !strings.Contains(output.String(), "端口是否已被占用") {
		t.Fatal("端口占用处理不正确")
	}
}

type loginFunc func(context.Context, session.ResourceID) (secdb.Params, error)

func (login loginFunc) Login(ctx context.Context, id session.ResourceID) (secdb.Params, error) {
	return login(ctx, id)
}

func (loginFunc) Activate(context.Context, string) error { return nil }

// handshakeFor 按协议构造代理握手：长度字节是用户名的字节数。
func handshakeFor(params secdb.Params) string {
	return "yabP" + params.SessionID + string([]byte{byte(len(params.Username))}) + params.Username
}

func TestConnectionFailureStopsOtherActiveConnections(t *testing.T) {
	app, output, path := testApp(t)
	if err := config.Save(path, fixtureConfig()); err != nil {
		t.Fatal(err)
	}
	params := secdb.Params{Type: "MySQL", YxzIP: "127.0.0.1", SessionID: "fixture", Username: "user"}
	var calls atomic.Int32
	app.newSession = func(context.Context, session.Options) (sessionClient, error) {
		return loginFunc(func(context.Context, session.ResourceID) (secdb.Params, error) {
			if calls.Add(1) > 2 {
				return secdb.Params{}, errors.New("fixture-private-error")
			}
			return params, nil
		}), nil
	}
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	app.proxyPort = proxy.Addr().(*net.TCPAddr).Port
	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		connection, err := proxy.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		connection.SetDeadline(time.Now().Add(5 * time.Second))
		io.ReadFull(connection, make([]byte, len("yabPfixture\x04user")))
		io.WriteString(connection, "successready")
		io.Copy(io.Discard, connection)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	port := availablePort(t)
	done := make(chan int, 1)
	go func() { done <- app.run(ctx, []string{"connect", "--id", "856", "--port", strconv.Itoa(port)}) }()
	waitFor(t, func() bool { return strings.Contains(output.String(), "等待客户端") })
	active, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	active.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(active, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	failed, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer failed.Close()
	if code := <-done; code != 1 {
		t.Fatal("单连接失败未触发非零退出")
	}
	if _, err := active.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal("先前活跃连接未被关闭")
	}
	<-proxyDone
	if strings.Contains(output.String(), "fixture-private-error") {
		t.Fatal("泄露 Provider 原始错误")
	}
	assertReleased(t, port)
}
