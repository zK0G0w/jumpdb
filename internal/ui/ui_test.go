package ui_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zK0G0w/jumpdb/internal/config"
	"github.com/zK0G0w/jumpdb/internal/secdb"
	"github.com/zK0G0w/jumpdb/internal/session"
)

func TestRejectsMissingTokenForeignHostAndCrossSiteRequests(t *testing.T) {
	h := start(t, true)
	if len(h.token) < 32 {
		t.Fatal("访问令牌过短")
	}
	for name, test := range map[string]struct {
		method, path string
		headers      map[string]string
		want         int
	}{
		"页面缺少令牌":     {http.MethodGet, "/", map[string]string{"X-Jumpdb-Token": ""}, http.StatusForbidden},
		"页面令牌错误":     {http.MethodGet, "/?token=wrong", nil, http.StatusForbidden},
		"页面令牌正确":     {http.MethodGet, "/?token=" + h.token, nil, http.StatusOK},
		"接口缺少令牌":     {http.MethodGet, "/api/state", map[string]string{"X-Jumpdb-Token": ""}, http.StatusForbidden},
		"接口令牌错误":     {http.MethodGet, "/api/state", map[string]string{"X-Jumpdb-Token": "wrong"}, http.StatusForbidden},
		"DNS 重绑定":    {http.MethodGet, "/api/state", map[string]string{"Host": "rebind.invalid:" + strings.Split(h.host, ":")[1]}, http.StatusForbidden},
		"localhost":  {http.MethodGet, "/api/state", map[string]string{"Host": "localhost:" + strings.Split(h.host, ":")[1]}, http.StatusForbidden},
		"跨站提交":       {http.MethodPost, "/api/resources", map[string]string{"Origin": "https://evil.invalid"}, http.StatusForbidden},
		"缺少 Origin":  {http.MethodPost, "/api/resources", map[string]string{"Origin": ""}, http.StatusForbidden},
		"非 JSON 提交":  {http.MethodPost, "/api/resources", map[string]string{"Content-Type": "text/plain"}, http.StatusForbidden},
		"密码只允许 POST": {http.MethodGet, "/api/tunnels/password", nil, http.StatusMethodNotAllowed},
		"未知路径":       {http.MethodGet, "/api/unknown", nil, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := h.request(test.method, test.path, map[string]any{"resources": []any{}}, test.headers)
			if status != test.want {
				t.Fatalf("状态码 %d，期望 %d", status, test.want)
			}
			assertNoSecrets(t, string(body))
		})
	}
	status, body := h.request(http.MethodGet, "/static/app.js", nil, map[string]string{"X-Jumpdb-Token": ""})
	if status != http.StatusOK || len(body) == 0 {
		t.Fatal("静态脚本不可用")
	}
	if status, body := h.request(http.MethodGet, "/static/logo.svg", nil, map[string]string{"X-Jumpdb-Token": ""}); status != http.StatusOK || !strings.Contains(string(body), "<svg") {
		t.Fatal("标志图片不可用")
	}
	request, _ := http.NewRequest(http.MethodGet, h.base+"/?token="+h.token, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	for header, want := range map[string]string{"Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY", "Cache-Control": "no-store"} {
		if response.Header.Get(header) != want {
			t.Fatalf("缺少安全响应头 %s", header)
		}
	}
	if !strings.Contains(response.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatal("缺少内容安全策略")
	}
}

func TestConfigurationFirstSetupTokenUpdateAndReconfigure(t *testing.T) {
	h := start(t, false)
	view, _ := h.state()
	if view.Config.Configured {
		t.Fatal("未配置时应提示首次配置")
	}
	if status, _ := h.post("/api/config", map[string]any{"token": "fixture-only-token"}); status != http.StatusBadRequest {
		t.Fatal("首次配置必须填写全部字段")
	}
	data := fixtureConfig()
	full := map[string]any{"portal": data.Portal, "transKey": data.TransKey, "dbKey": data.DBKey, "token": data.Token, "configure": true}
	if status, result := h.post("/api/config", full); status != http.StatusOK {
		t.Fatalf("首次配置失败：%v", result["error"])
	}
	info, err := os.Stat(h.path)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("配置权限不正确")
	}
	view, raw := h.state()
	if !view.Config.Configured || view.Config.Portal != data.Portal {
		t.Fatal("配置后应回填门户地址")
	}
	assertNoSecrets(t, raw)

	resources := []config.Resource{{ID: "856", IDType: "auto", Port: 9011, Name: "订单库"}}
	if status, _ := h.post("/api/resources", map[string]any{"resources": resources}); status != http.StatusOK {
		t.Fatal("保存资源失败")
	}
	// 未要求重新配置时只更新 Token，忽略其他字段。
	if status, _ := h.post("/api/config", map[string]any{"token": "fixture-replacement", "portal": "https://ignored.invalid"}); status != http.StatusOK {
		t.Fatal("更新 Token 失败")
	}
	loaded, err := config.Load(h.path)
	if err != nil || loaded.Token != "fixture-replacement" || loaded.Portal != data.Portal || len(loaded.Resources) != 1 || loaded.Resources[0].Name != "订单库" {
		t.Fatal("只更新 Token 时改变了其他字段或资源")
	}
	full["portal"] = "https://second.invalid"
	if status, _ := h.post("/api/config", full); status != http.StatusOK {
		t.Fatal("无隧道运行时应允许重新配置")
	}
	if loaded, _ := config.Load(h.path); loaded.Portal != "https://second.invalid" || len(loaded.Resources) != 1 {
		t.Fatal("重新配置未生效或丢失资源")
	}
	if status, result := h.post("/api/config", map[string]any{"token": "bad token"}); status != http.StatusBadRequest || strings.Contains(result["error"].(string), "bad token") {
		t.Fatal("无效 Token 应拒绝且不回显")
	}
	if status, _ := h.post("/api/resources", map[string]any{"resources": []config.Resource{{ID: "", IDType: "auto", Port: 9011}}}); status != http.StatusBadRequest {
		t.Fatal("无效资源应拒绝")
	}
}

func TestDamagedConfigurationIsReportedAndNotOverwritten(t *testing.T) {
	h := start(t, true)
	if err := os.WriteFile(h.path, []byte("fixture-private-damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	view, raw := h.state()
	if view.Config.Error == "" {
		t.Fatal("损坏配置未报告")
	}
	assertNoSecrets(t, raw)
	data := fixtureConfig()
	h.post("/api/config", map[string]any{"portal": data.Portal, "transKey": data.TransKey, "dbKey": data.DBKey, "token": data.Token, "configure": true})
	if raw, _ := os.ReadFile(h.path); string(raw) != "fixture-private-damaged" {
		t.Fatal("覆盖了损坏的配置")
	}
}

func TestTunnelsRunConcurrentlyWithConnectionObservability(t *testing.T) {
	h := start(t, true)
	firstPort, secondPort := availablePort(t), availablePort(t)
	first := h.startTunnel(firstPort)
	second := h.startTunnel(secondPort)
	tunnel := h.waitTunnel(first, func(view tunnelView) bool { return view.Status == "listening" })
	if tunnel.Type != "MySQL" || tunnel.Username != fixtureParams.Username || tunnel.Host != "127.0.0.1" || tunnel.Port != firstPort {
		t.Fatal("隧道连接信息不正确")
	}
	h.waitTunnel(second, func(view tunnelView) bool { return view.Status == "listening" })
	if status, _ := h.post("/api/tunnels/start", map[string]any{"id": "856", "id_type": "auto", "port": firstPort}); status != http.StatusConflict {
		t.Fatal("同端口重复启动应报冲突")
	}

	conn := dialTunnel(t, firstPort)
	dialTunnel(t, secondPort)
	tunnel = h.waitTunnel(first, func(view tunnelView) bool {
		return len(view.Connections) == 1 && view.Connections[0].Status == "active" && view.Connections[0].Up == 5
	})
	if got := tunnel.Connections[0]; got.Down != 10 || strings.Join(got.Stages, ",") != "session,dial,activate,handshake" {
		t.Fatal("连接阶段或字节数不正确")
	}
	conn.Close()
	h.waitTunnel(first, func(view tunnelView) bool {
		return len(view.Connections) == 1 && view.Connections[0].Status == "closed" && view.Connections[0].Reason == "client"
	})

	status, result := h.post("/api/tunnels/password", map[string]any{"id": first})
	if status != http.StatusOK || result["password"] != fixturePassword {
		t.Fatal("显式请求时应返回初始会话密码")
	}
	_, raw := h.state()
	assertNoSecrets(t, raw)

	active := dialTunnel(t, firstPort)
	if status, _ := h.post("/api/tunnels/stop", map[string]any{"id": first}); status != http.StatusOK {
		t.Fatal("停止隧道失败")
	}
	tunnel = h.waitTunnel(first, func(view tunnelView) bool { return view.Status == "stopped" })
	assertReleased(t, firstPort)
	if _, err := active.Read(make([]byte, 1)); err == nil {
		t.Fatal("停止隧道未关闭其活跃连接")
	}
	if last := tunnel.Connections[len(tunnel.Connections)-1]; last.Status != "closed" || last.Reason != "stopped" {
		t.Fatal("停止时活跃连接的结束原因不正确")
	}
	if status, _ := h.post("/api/tunnels/password", map[string]any{"id": first}); status != http.StatusConflict {
		t.Fatal("停止后不应再提供密码")
	}
	h.waitTunnel(second, func(view tunnelView) bool {
		return view.Status == "listening" && len(view.Connections) == 1 && view.Connections[0].Status == "active"
	})
	if status, _ := h.post("/api/tunnels/dismiss", map[string]any{"id": first}); status != http.StatusOK {
		t.Fatal("清除已停止隧道失败")
	}
	if status, _ := h.post("/api/tunnels/dismiss", map[string]any{"id": second}); status != http.StatusConflict {
		t.Fatal("运行中的隧道不应被清除")
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	assertReleased(t, secondPort)
}

func TestConnectionFailureStopsOnlyThatTunnel(t *testing.T) {
	h := start(t, true)
	healthyPort, failingPort := availablePort(t), availablePort(t)
	healthy := h.startTunnel(healthyPort)
	h.waitTunnel(healthy, func(view tunnelView) bool { return view.Status == "listening" })
	active := dialTunnel(t, healthyPort)

	failing := h.startTunnel(failingPort)
	h.waitTunnel(failing, func(view tunnelView) bool { return view.Status == "listening" })
	h.portal.setLogin(func(context.Context, session.ResourceID) (secdb.Params, error) {
		return secdb.Params{}, errors.New("fixture-private-error")
	})
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(failingPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tunnel := h.waitTunnel(failing, func(view tunnelView) bool { return view.Status == "failed" })
	if tunnel.FailureStage != "session" || !strings.Contains(tunnel.Failure, "Token") {
		t.Fatal("失败阶段或提示不正确")
	}
	assertReleased(t, failingPort)
	if view := h.waitTunnel(healthy, func(view tunnelView) bool { return true }); view.Status != "listening" {
		t.Fatal("其他隧道受到影响")
	}
	if _, err := active.Write([]byte("again")); err != nil {
		t.Fatal("其他隧道的活跃连接被关闭")
	}
	_, raw := h.state()
	assertNoSecrets(t, raw)
}

func TestStartFailuresAreReportedPerTunnel(t *testing.T) {
	h := start(t, true)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	status, result := h.post("/api/tunnels/start", map[string]any{"id": "856", "id_type": "auto", "port": occupied.Addr().(*net.TCPAddr).Port})
	if status != http.StatusConflict || !strings.Contains(result["error"].(string), "端口") {
		t.Fatal("端口占用应报错")
	}
	if status, _ := h.post("/api/tunnels/start", map[string]any{"id": "", "id_type": "auto", "port": 9011}); status != http.StatusBadRequest {
		t.Fatal("无效资源应拒绝启动")
	}

	h.portal.setLogin(func(context.Context, session.ResourceID) (secdb.Params, error) {
		return secdb.Params{}, session.ErrBusiness
	})
	port := availablePort(t)
	id := h.startTunnel(port)
	tunnel := h.waitTunnel(id, func(view tunnelView) bool { return view.Status == "failed" })
	if tunnel.FailureStage != "initial" || !strings.Contains(tunnel.Failure, "初始会话申请失败") {
		t.Fatal("初始会话失败未按阶段报告")
	}
	assertReleased(t, port)

	h.portal.setLogin(func(context.Context, session.ResourceID) (secdb.Params, error) {
		params := fixtureParams
		params.Type = "PostgreSQL"
		return params, nil
	})
	id = h.startTunnel(port)
	if tunnel := h.waitTunnel(id, func(view tunnelView) bool { return view.Status == "failed" }); !strings.Contains(tunnel.Failure, "MySQL") {
		t.Fatal("非 MySQL 类型应拒绝")
	}
	assertReleased(t, port)
}

func TestTokenHotReloadAndExternalConfigChange(t *testing.T) {
	h := start(t, true)
	port := availablePort(t)
	id := h.startTunnel(port)
	h.waitTunnel(id, func(view tunnelView) bool { return view.Status == "listening" })
	dialTunnel(t, port)

	// 另一个终端执行 token set：下一个新连接应使用磁盘上的新 Token。
	if _, err := config.Update(h.path, func(data *config.Data) error {
		data.Token = "fixture-external-token"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dialTunnel(t, port)
	tokens := h.portal.sessionTokens()
	if tokens[len(tokens)-1] != "fixture-external-token" {
		t.Fatal("Token 未热生效")
	}

	data := fixtureConfig()
	full := map[string]any{"portal": "https://second.invalid", "transKey": data.TransKey, "dbKey": data.DBKey, "token": data.Token, "configure": true}
	if status, _ := h.post("/api/config", full); status != http.StatusConflict {
		t.Fatal("隧道运行时应拒绝修改门户或密钥")
	}
	if status, _ := h.post("/api/config", map[string]any{"token": "fixture-page-token"}); status != http.StatusOK {
		t.Fatal("隧道运行时应允许更新 Token")
	}

	if _, err := config.Update(h.path, func(data *config.Data) error {
		data.Portal = "https://external.invalid"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tunnel := h.waitTunnel(id, func(view tunnelView) bool { return view.Status == "failed" })
	if !strings.Contains(tunnel.Failure, "外部变更") {
		t.Fatal("外部修改门户后未提示重启隧道")
	}
}

func TestExternalKeyChangeRejectsNewConnections(t *testing.T) {
	h := start(t, true)
	port := availablePort(t)
	id := h.startTunnel(port)
	h.waitTunnel(id, func(view tunnelView) bool { return view.Status == "listening" })
	if _, err := config.Update(h.path, func(data *config.Data) error {
		data.DBKey = strings.Repeat("e", 16)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tunnel := h.waitTunnel(id, func(view tunnelView) bool { return view.Status == "failed" }); !strings.Contains(tunnel.Failure, "外部变更") {
		t.Fatal("外部修改密钥后未提示重启隧道")
	}
}

// 连接 A 用旧 Token 申请会话后、激活前，连接 B 触发了 Token 切换；
// A 的激活仍须由申请该会话的旧客户端发起。
func TestActivationUsesTheClientThatIssuedTheSession(t *testing.T) {
	h := start(t, true)
	port := availablePort(t)
	id := h.startTunnel(port)
	h.waitTunnel(id, func(view tunnelView) bool { return view.Status == "listening" })
	original := fixtureConfig().Token
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.portal.setLoginHook(func(token string) {
		if token == original {
			once.Do(func() {
				close(held)
				<-release
			})
		}
	})
	first, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("第一个连接未进入会话申请")
	}
	if _, err := config.Update(h.path, func(data *config.Data) error {
		data.Token = "fixture-rotated-token"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dialTunnel(t, port)
	close(release)
	first.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(first, make([]byte, 5)); err != nil {
		t.Fatal("第一个连接未完成握手")
	}
	if got := h.portal.activationTokens(); len(got) != 2 || got[0] != "fixture-rotated-token" || got[1] != original {
		t.Fatalf("激活使用的客户端不正确：%d 次", len(got))
	}
}
