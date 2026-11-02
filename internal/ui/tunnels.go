package ui

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/zK0G0w/jumpdb/internal/config"
	"github.com/zK0G0w/jumpdb/internal/secdb"
	"github.com/zK0G0w/jumpdb/internal/session"
	"github.com/zK0G0w/jumpdb/internal/tunnel"
)

const (
	statusStarting  = "starting"
	statusListening = "listening"
	statusStopping  = "stopping"
	statusStopped   = "stopped"
	statusFailed    = "failed"

	// 观测数据只保存在内存，按隧道限量，避免长时间运行时无限增长。
	maxEvents          = 200
	maxEndedConnection = 50
)

var stageNames = map[tunnel.Stage]string{
	tunnel.StageSession:   "客户端会话申请",
	tunnel.StageDial:      "代理连接",
	tunnel.StageActivate:  "会话激活",
	tunnel.StageHandshake: "代理握手",
}

// tunnels 管理控制页启动的全部隧道；各隧道相互独立，单个失败只停止它自己。
type tunnels struct {
	mu   sync.Mutex
	list []*tunnelRecord
	next int
	wg   sync.WaitGroup
}

type tunnelRecord struct {
	id           int
	resource     config.Resource
	status       string
	failure      string
	failureStage string
	dbType       string
	username     string
	// password 是初始会话密码，仅在隧道监听期间按显式请求返回，停止后清除。
	password    string
	started     time.Time
	cancel      context.CancelFunc
	connections []*connectionRecord
	events      []eventView
}

type connectionRecord struct {
	id          uint64
	accepted    time.Time
	ended       time.Time
	stages      []string
	status      string
	failedStage string
	reason      string
	traffic     *tunnel.Traffic
}

type tunnelView struct {
	ID           int              `json:"id"`
	Resource     config.Resource  `json:"resource"`
	Status       string           `json:"status"`
	Failure      string           `json:"failure,omitempty"`
	FailureStage string           `json:"failureStage,omitempty"`
	Type         string           `json:"type,omitempty"`
	Username     string           `json:"username,omitempty"`
	Host         string           `json:"host"`
	Port         int              `json:"port"`
	Started      time.Time        `json:"started"`
	Active       int              `json:"active"`
	Connections  []connectionView `json:"connections"`
	Events       []eventView      `json:"events"`
}

type connectionView struct {
	ID          uint64     `json:"id"`
	Accepted    time.Time  `json:"accepted"`
	Ended       *time.Time `json:"ended,omitempty"`
	DurationMs  int64      `json:"durationMs"`
	Stages      []string   `json:"stages"`
	Status      string     `json:"status"`
	FailedStage string     `json:"failedStage,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	Up          int64      `json:"up"`
	Down        int64      `json:"down"`
}

type eventView struct {
	Time    time.Time `json:"time"`
	Conn    uint64    `json:"conn"`
	Stage   string    `json:"stage"`
	Success bool      `json:"success"`
	Reason  string    `json:"reason,omitempty"`
}

func running(status string) bool {
	return status == statusStarting || status == statusListening || status == statusStopping
}

func (t *tunnels) anyRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, record := range t.list {
		if running(record.status) {
			return true
		}
	}
	return false
}

func (t *tunnels) find(id int) (*tunnelRecord, error) {
	for _, record := range t.list {
		if record.id == id {
			return record, nil
		}
	}
	return nil, apiError{http.StatusNotFound, "隧道不存在"}
}

func decodeID(request *http.Request) (int, error) {
	var input struct {
		ID int `json:"id"`
	}
	return input.ID, decode(request, &input)
}

func (s *server) startTunnel(request *http.Request) (any, error) {
	var resource config.Resource
	if err := decode(request, &resource); err != nil {
		return nil, err
	}
	if err := resource.Validate(); err != nil {
		return nil, err
	}
	id, _ := config.ParseResourceID(resource.ID, resource.IDType)
	t := &s.tunnels
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil, apiError{http.StatusServiceUnavailable, "控制页正在退出"}
	}
	kept := t.list[:0]
	for _, record := range t.list {
		if record.resource.Port == resource.Port {
			if running(record.status) {
				return nil, apiError{http.StatusConflict, "该端口已有隧道在运行"}
			}
			// 同端口重新启动时替换已结束的旧记录，避免列表堆积。
			continue
		}
		kept = append(kept, record)
	}
	// 与 connect 一致：先绑定端口再访问门户，端口占用时不产生会话。
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(resource.Port)))
	if err != nil {
		return nil, apiError{http.StatusConflict, "无法监听本地端口，请检查端口是否已被占用"}
	}
	t.list = kept
	t.next++
	ctx, cancel := context.WithCancel(s.ctx)
	record := &tunnelRecord{id: t.next, resource: resource, status: statusStarting, started: time.Now(), cancel: cancel}
	t.list = append(t.list, record)
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer cancel()
		defer listener.Close()
		stage, failure := s.serveTunnel(ctx, record, listener, id)
		t.finish(record, stage, failure)
	}()
	return map[string]int{"id": record.id}, nil
}

func (s *server) stopTunnel(request *http.Request) (any, error) {
	id, err := decodeID(request)
	if err != nil {
		return nil, err
	}
	t := &s.tunnels
	t.mu.Lock()
	defer t.mu.Unlock()
	record, err := t.find(id)
	if err != nil {
		return nil, err
	}
	if record.status == statusStarting || record.status == statusListening {
		record.status = statusStopping
		record.cancel()
	}
	return map[string]bool{"ok": true}, nil
}

func (s *server) dismissTunnel(request *http.Request) (any, error) {
	id, err := decodeID(request)
	if err != nil {
		return nil, err
	}
	t := &s.tunnels
	t.mu.Lock()
	defer t.mu.Unlock()
	record, err := t.find(id)
	if err != nil {
		return nil, err
	}
	if running(record.status) {
		return nil, apiError{http.StatusConflict, "请先停止隧道再清除"}
	}
	kept := t.list[:0]
	for _, item := range t.list {
		if item != record {
			kept = append(kept, item)
		}
	}
	t.list = kept
	return map[string]bool{"ok": true}, nil
}

// revealPassword 仅响应显式 POST 请求，与 connect --show-password 的显式授权语义一致。
func (s *server) revealPassword(request *http.Request) (any, error) {
	id, err := decodeID(request)
	if err != nil {
		return nil, err
	}
	t := &s.tunnels
	t.mu.Lock()
	defer t.mu.Unlock()
	record, err := t.find(id)
	if err != nil {
		return nil, err
	}
	if record.status != statusListening || record.password == "" {
		return nil, apiError{http.StatusConflict, "隧道未在监听或尚未取得初始会话"}
	}
	return map[string]string{"password": record.password}, nil
}

// serveTunnel 申请初始会话后运行隧道，直到停止或任一连接阶段失败；返回失败阶段及提示，正常停止时均为空。
func (s *server) serveTunnel(ctx context.Context, record *tunnelRecord, listener net.Listener, id session.ResourceID) (string, string) {
	data, err := config.Load(s.options.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return "config", "尚未配置，请先在页面完成配置"
	}
	if err != nil {
		return "config", err.Error()
	}
	key, _ := secdb.ParseKey(data.DBKey)
	client, err := s.options.NewSession(ctx, session.Options{Portal: data.Portal, Token: data.Token, DBKey: key})
	if ctx.Err() != nil {
		return "", ""
	}
	if err != nil {
		return "probe", "门户时间探测失败：" + session.Describe(err)
	}
	params, err := client.Login(ctx, id)
	if ctx.Err() != nil {
		return "", ""
	}
	if err != nil {
		return "initial", "初始会话申请失败：" + session.Describe(err)
	}
	if params.Type != "MySQL" {
		return "initial", "初始会话的数据库类型不是 MySQL，首版不支持连接"
	}
	s.tunnels.listening(record, params)

	sessions := &provider{server: s, locked: data, id: id, token: data.Token, client: client}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mutex sync.Mutex
	var failedStage, failure string
	err = tunnel.Serve(ctx, listener, tunnel.Config{
		ProxyPort: s.options.ProxyPort,
		Provider:  sessions.provide,
		// 每条连接的新会话都需激活后再握手；初始会话不建隧道，无需激活。
		Activate: sessions.activate,
		OnEvent: func(event tunnel.Event) {
			s.tunnels.observe(record, event)
			if event.Success {
				return
			}
			// 与 connect 相同采用失败即停止，但范围只限本隧道，其他隧道照常运行。
			mutex.Lock()
			if failedStage == "" {
				failedStage, failure = string(event.Stage), sessions.failureMessage(event.Stage)
			}
			mutex.Unlock()
			cancel()
		},
	})
	mutex.Lock()
	defer mutex.Unlock()
	if failedStage != "" {
		return failedStage, failure
	}
	if err != nil {
		return "listen", "本地隧道停止：监听失败"
	}
	return "", ""
}

func (t *tunnels) listening(record *tunnelRecord, params secdb.Params) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if record.status == statusStarting {
		record.status = statusListening
	}
	record.dbType, record.username, record.password = params.Type, params.Username, params.Password
}

func (t *tunnels) finish(record *tunnelRecord, stage, failure string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	record.status, record.password = statusStopped, ""
	if stage != "" {
		record.status, record.failureStage, record.failure = statusFailed, stage, failure
	}
	// 停止时尚在申请会话的连接不会再有事件，统一标为主动停止。
	for _, connection := range record.connections {
		if connection.ended.IsZero() {
			connection.ended, connection.status, connection.reason = now, "closed", string(tunnel.ReasonStopped)
		}
	}
}

// observe 将隧道事件归并到连接记录，并保留最近的事件流水。
func (t *tunnels) observe(record *tunnelRecord, event tunnel.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	record.events = append(record.events, eventView{Time: event.Time, Conn: event.Conn, Stage: string(event.Stage), Success: event.Success, Reason: string(event.Reason)})
	if len(record.events) > maxEvents {
		record.events = append([]eventView(nil), record.events[len(record.events)-maxEvents:]...)
	}
	var connection *connectionRecord
	for _, item := range record.connections {
		if item.id == event.Conn {
			connection = item
			break
		}
	}
	if connection == nil {
		connection = &connectionRecord{id: event.Conn, accepted: event.Accepted, status: "connecting", stages: []string{}}
		record.connections = append(record.connections, connection)
	}
	switch {
	case event.Stage == tunnel.StageClosed:
		connection.ended, connection.status, connection.reason = event.Time, "closed", string(event.Reason)
	case !event.Success:
		connection.ended, connection.status, connection.failedStage = event.Time, "failed", string(event.Stage)
	default:
		connection.stages = append(connection.stages, string(event.Stage))
		if event.Stage == tunnel.StageHandshake {
			connection.status, connection.traffic = "active", event.Traffic
		}
	}
	ended := 0
	for _, item := range record.connections {
		if !item.ended.IsZero() {
			ended++
		}
	}
	if ended > maxEndedConnection {
		kept := record.connections[:0]
		for _, item := range record.connections {
			if !item.ended.IsZero() && ended > maxEndedConnection {
				ended--
				continue
			}
			kept = append(kept, item)
		}
		record.connections = kept
	}
}

func (t *tunnels) views() []tunnelView {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	views := make([]tunnelView, 0, len(t.list))
	for _, record := range t.list {
		view := tunnelView{
			ID: record.id, Resource: record.resource, Status: record.status, Failure: record.failure, FailureStage: record.failureStage,
			Type: record.dbType, Username: record.username, Host: "127.0.0.1", Port: record.resource.Port, Started: record.started,
			Connections: make([]connectionView, 0, len(record.connections)), Events: append([]eventView{}, record.events...),
		}
		for _, connection := range record.connections {
			item := connectionView{
				ID: connection.id, Accepted: connection.accepted, Stages: append([]string{}, connection.stages...),
				Status: connection.status, FailedStage: connection.failedStage, Reason: connection.reason,
			}
			end := now
			if !connection.ended.IsZero() {
				ended := connection.ended
				item.Ended, end = &ended, ended
			} else {
				view.Active++
			}
			item.DurationMs = end.Sub(connection.accepted).Milliseconds()
			if connection.traffic != nil {
				item.Up, item.Down = connection.traffic.Up(), connection.traffic.Down()
			}
			view.Connections = append(view.Connections, item)
		}
		views = append(views, view)
	}
	return views
}

// provider 为每个客户端连接申请会话：每次从磁盘读取 Token 以便热生效，
// 门户地址和密钥在隧道启动时锁定，被外部修改时拒绝本次连接。
type provider struct {
	server *server
	locked config.Data
	id     session.ResourceID

	mu     sync.Mutex
	token  string
	client Session
	hint   string
}

func (p *provider) provide(ctx context.Context) (secdb.Params, error) {
	client, err := p.current(ctx)
	if err != nil {
		return secdb.Params{}, err
	}
	return client.Login(ctx, p.id)
}

func (p *provider) current(ctx context.Context) (Session, error) {
	data, err := config.Load(p.server.options.ConfigPath)
	p.mu.Lock()
	defer p.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		p.hint = "本地配置已被删除，请重新配置后重启隧道"
		return nil, err
	}
	if err != nil {
		p.hint = err.Error()
		return nil, err
	}
	if data.Portal != p.locked.Portal || data.TransKey != p.locked.TransKey || data.DBKey != p.locked.DBKey {
		p.hint = "门户地址或密钥已在外部变更，请重启隧道"
		return nil, errors.New("ui: 配置已在外部变更")
	}
	if data.Token == p.token {
		return p.client, nil
	}
	// Token 变化时以新 Token 创建客户端；探测发生在拨号之前，不会插入激活与握手之间。
	key, _ := secdb.ParseKey(data.DBKey)
	client, err := p.server.options.NewSession(ctx, session.Options{Portal: data.Portal, Token: data.Token, DBKey: key})
	if err != nil {
		p.hint = "使用新 Token 探测门户失败：" + session.Describe(err)
		return nil, err
	}
	p.token, p.client = data.Token, client
	return client, nil
}

func (p *provider) activate(ctx context.Context, params secdb.Params) error {
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	return client.Activate(ctx, params.SessionID)
}

func (p *provider) failureMessage(stage tunnel.Stage) string {
	message := stageNames[stage] + "失败，已停止本隧道并关闭其活跃连接"
	if stage != tunnel.StageSession && stage != tunnel.StageActivate {
		return message
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hint != "" {
		return message + "；" + p.hint
	}
	return message + "；请检查 Token、资源权限、门户连接及数据库密钥"
}
