// Package ui 提供仅监听本机的 Web 控制页：录入配置、管理多个隧道并观测连接。
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	"github.com/zK0G0w/jumpdb/internal/config"
	"github.com/zK0G0w/jumpdb/internal/secdb"
	"github.com/zK0G0w/jumpdb/internal/session"
)

//go:embed web
var webFiles embed.FS

// Session 是控制页申请会话和激活所需的门户能力。
type Session interface {
	Login(context.Context, session.ResourceID) (secdb.Params, error)
	Activate(context.Context, string) error
}

// Options 描述控制页依赖；NewSession 和 ProxyPort 便于测试注入合成门户与代理。
type Options struct {
	// ConfigPath 是本地配置文件路径，不允许为空。
	ConfigPath string
	// NewSession 以给定门户、Token 和数据库密钥创建会话客户端，不允许为空。
	NewSession func(context.Context, session.Options) (Session, error)
	// ProxyPort 为 0 时使用隧道默认代理端口。
	ProxyPort int
	// Ready 在开始接受请求后调用一次，参数为含访问令牌的完整控制页地址。
	Ready func(address string)
}

// Serve 在 listener 上提供控制页，直到 ctx 取消；取消时停止全部隧道并释放端口。
// listener 必须绑定 127.0.0.1，控制页只接受与其地址完全一致的 Host。
func Serve(ctx context.Context, listener net.Listener, options Options) error {
	if options.ConfigPath == "" || options.NewSession == nil {
		return errors.New("ui: 缺少配置路径或会话工厂")
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		return errors.New("ui: 控制页只能监听 127.0.0.1")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return errors.New("ui: 无法生成访问令牌")
	}
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &server{
		ctx: ctx, options: options, static: static,
		token: hex.EncodeToString(secret), host: address.String(), origin: "http://" + address.String(),
	}
	httpServer := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	if options.Ready != nil {
		options.Ready(s.origin + "/?token=" + s.token)
	}
	var result error
	select {
	case <-ctx.Done():
	case <-served:
		cancel()
		result = errors.New("ui: 控制页监听失败")
	}
	// 先关闭控制页并等待进行中的请求，确保不再启动新隧道；再等待全部隧道释放端口。
	shutdown, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	if httpServer.Shutdown(shutdown) != nil {
		_ = httpServer.Close()
	}
	s.tunnels.wg.Wait()
	return result
}

type server struct {
	ctx     context.Context
	options Options
	static  fs.FS
	token   string
	host    string
	origin  string
	// configMu 串行化本进程内的配置写入；跨进程依赖 config.Update 的读后原子替换。
	configMu sync.Mutex
	tunnels  tunnels
}

// apiError 是返回给页面的错误；消息不得包含用户输入、凭据或远端原始响应。
type apiError struct {
	status  int
	message string
}

func (err apiError) Error() string { return err.message }

func (s *server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	header := writer.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	// 只接受与监听地址完全一致的 Host，防止 DNS 重绑定把外部域名解析到本机后访问控制页。
	if request.Host != s.host {
		http.Error(writer, "forbidden", http.StatusForbidden)
		return
	}
	switch {
	case request.URL.Path == "/":
		if request.Method != http.MethodGet {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.validToken(request.URL.Query().Get("token")) {
			http.Error(writer, "forbidden", http.StatusForbidden)
			return
		}
		s.serveFile(writer, "index.html")
	case path.Dir(request.URL.Path) == "/static":
		// 静态资源是公开源码的一部分，不含凭据，无需令牌。
		s.serveFile(writer, path.Base(request.URL.Path))
	case path.Dir(request.URL.Path) == "/api" || path.Dir(request.URL.Path) == "/api/tunnels":
		s.serveAPI(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (s *server) validToken(value string) bool {
	return subtle.ConstantTimeCompare([]byte(value), []byte(s.token)) == 1
}

func (s *server) serveFile(writer http.ResponseWriter, name string) {
	raw, err := fs.ReadFile(s.static, name)
	if err != nil {
		http.Error(writer, "not found", http.StatusNotFound)
		return
	}
	writer.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
	_, _ = writer.Write(raw)
}

func (s *server) serveAPI(writer http.ResponseWriter, request *http.Request) {
	if !s.validToken(request.Header.Get("X-Jumpdb-Token")) {
		writeJSON(writer, http.StatusForbidden, map[string]string{"error": "访问令牌无效"})
		return
	}
	routes := map[string]func(*http.Request) (any, error){
		"/api/config":           s.saveConfig,
		"/api/resources":        s.saveResources,
		"/api/tunnels/start":    s.startTunnel,
		"/api/tunnels/stop":     s.stopTunnel,
		"/api/tunnels/dismiss":  s.dismissTunnel,
		"/api/tunnels/password": s.revealPassword,
	}
	if request.URL.Path == "/api/state" {
		if request.Method != http.MethodGet {
			writeJSON(writer, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 GET"})
			return
		}
		writeJSON(writer, http.StatusOK, s.state())
		return
	}
	handler, ok := routes[request.URL.Path]
	if !ok {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "接口不存在"})
		return
	}
	if request.Method != http.MethodPost {
		writeJSON(writer, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	// 修改类请求必须来自本控制页且为 JSON：跨站表单无法设置 JSON 类型，跨站脚本带不上本页 Origin。
	mediaType, _, _ := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if request.Header.Get("Origin") != s.origin || mediaType != "application/json" {
		writeJSON(writer, http.StatusForbidden, map[string]string{"error": "拒绝跨站或非 JSON 请求"})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	result, err := handler(request)
	if err != nil {
		var failure apiError
		if !errors.As(err, &failure) {
			failure = apiError{http.StatusBadRequest, err.Error()}
		}
		writeJSON(writer, failure.status, map[string]string{"error": failure.message})
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func decode(request *http.Request, target any) error {
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return apiError{http.StatusBadRequest, "请求格式无效"}
	}
	return nil
}

type configView struct {
	Configured bool              `json:"configured"`
	Error      string            `json:"error,omitempty"`
	Portal     string            `json:"portal,omitempty"`
	Resources  []config.Resource `json:"resources"`
}

type stateView struct {
	Config  configView   `json:"config"`
	Tunnels []tunnelView `json:"tunnels"`
}

// state 汇总配置与隧道状态；密钥和 Token 从不返回，门户地址不属于秘密，可回填核对。
func (s *server) state() stateView {
	view := stateView{Config: configView{Resources: []config.Resource{}}, Tunnels: s.tunnels.views()}
	data, err := config.Load(s.options.ConfigPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		view.Config.Error = err.Error()
	default:
		view.Config.Configured = true
		view.Config.Portal = data.Portal
		if data.Resources != nil {
			view.Config.Resources = data.Resources
		}
	}
	return view
}

func (s *server) saveConfig(request *http.Request) (any, error) {
	var input struct {
		Portal    string `json:"portal"`
		TransKey  string `json:"transKey"`
		DBKey     string `json:"dbKey"`
		Token     string `json:"token"`
		Configure bool   `json:"configure"`
	}
	if err := decode(request, &input); err != nil {
		return nil, err
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	current, err := config.Load(s.options.ConfigPath)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// 语义与 token set 一致：首次或显式重新配置时写入全部字段，否则只更新 Token。
	configure := input.Configure || !exists
	if !configure && input.Token == "" {
		return nil, errors.New("Token 不能为空")
	}
	if configure && (input.Portal == "" || input.TransKey == "" || input.DBKey == "" || input.Token == "") {
		return nil, errors.New("首次配置或重新配置需填写全部字段")
	}
	// 运行中的隧道已锁定门户和密钥；中途更换会让同一隧道的连接落到不同环境。
	// 检查与写入期间持有隧道锁，避免其间启动新隧道。锁顺序固定为 configMu → tunnels.mu。
	s.tunnels.mu.Lock()
	defer s.tunnels.mu.Unlock()
	if configure && exists && (input.Portal != current.Portal || input.TransKey != current.TransKey || input.DBKey != current.DBKey) && s.tunnels.anyRunningLocked() {
		return nil, apiError{http.StatusConflict, "修改门户地址或密钥前请先停止所有隧道"}
	}
	if _, err := config.Update(s.options.ConfigPath, func(data *config.Data) error {
		if configure {
			data.Portal, data.TransKey, data.DBKey = input.Portal, input.TransKey, input.DBKey
		}
		data.Token = input.Token
		return nil
	}); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil
}

func (s *server) saveResources(request *http.Request) (any, error) {
	var input struct {
		Resources []config.Resource `json:"resources"`
	}
	if err := decode(request, &input); err != nil {
		return nil, err
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if _, err := config.Load(s.options.ConfigPath); errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("请先完成配置再保存资源")
	}
	if _, err := config.Update(s.options.ConfigPath, func(data *config.Data) error {
		data.Resources = input.Resources
		return nil
	}); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil
}
