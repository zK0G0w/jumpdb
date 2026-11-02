package tunnel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zK0G0w/jumpdb/internal/secdb"
)

const (
	defaultProxyPort    = 33306
	defaultHandshakeTTL = 10 * time.Second
	// 只消费 success 本身，后续字节交给数据库协议转发。
	handshakeResponse = "success"
)

// Provider 为每个本地客户端连接申请一次新的代理会话。
type Provider func(context.Context) (secdb.Params, error)

// Stage 是单连接建立过程中可观测的阶段，不携带凭据或远端地址。
type Stage string

const (
	StageSession   Stage = "session"
	StageDial      Stage = "dial"
	StageActivate  Stage = "activate"
	StageHandshake Stage = "handshake"
	// StageClosed 只在已进入转发的连接结束时报告，Success 恒为 true，由 Reason 说明结束原因。
	StageClosed Stage = "closed"
)

// CloseReason 说明已转发连接的结束原因；不区分正常关闭与网络错误。
type CloseReason string

const (
	ReasonClient   CloseReason = "client"
	ReasonUpstream CloseReason = "upstream"
	ReasonStopped  CloseReason = "stopped"
)

// Traffic 记录单个连接已转发的字节数，可在转发期间并发读取。
type Traffic struct {
	up   atomic.Int64
	down atomic.Int64
}

// Up 返回从本地客户端发往代理的字节数。
func (traffic *Traffic) Up() int64 { return traffic.up.Load() }

// Down 返回从代理发往本地客户端的字节数，含握手后紧随的数据库字节。
func (traffic *Traffic) Down() int64 { return traffic.down.Load() }

// Event 表示某连接某阶段的结果；主动取消不报告为失败。
type Event struct {
	Stage   Stage
	Success bool
	// Conn 是本次 Serve 内从 1 递增的连接序号，用于关联同一连接的多个事件。
	Conn uint64
	// Accepted 是该连接被接入的时间，Time 是本事件发生的时间。
	Accepted time.Time
	Time     time.Time
	// Traffic 仅在握手成功及 StageClosed 事件中非空，前者可用于观察实时字节数。
	Traffic *Traffic
	// Reason 仅在 StageClosed 事件中有效。
	Reason CloseReason
}

// Config 描述本地隧道的代理端口、会话申请器、会话激活和握手超时。
type Config struct {
	ProxyPort        int
	HandshakeTimeout time.Duration
	Provider         Provider
	// Activate 可选；在代理连接建立后、发送握手前一刻调用，失败则不握手。
	// 部分资源要求会话先激活，且激活与握手的间隔须尽量短。
	Activate func(context.Context, secdb.Params) error
	// OnEvent 可被不同连接并发调用，应及时返回；不传递原始错误以免泄露凭据。
	// 所有调用都在 Serve 返回前完成。
	OnEvent func(Event)
}

// Serve 接受本地客户端连接，并将每条连接转发到对应的代理会话。
// listener 的生命周期由调用方创建；ctx 取消时 Serve 会关闭 listener 和所有活跃连接。
func Serve(ctx context.Context, listener net.Listener, cfg Config) error {
	if ctx == nil {
		return errors.New("tunnel: nil context")
	}
	if listener == nil {
		return errors.New("tunnel: nil listener")
	}
	if cfg.Provider == nil {
		return errors.New("tunnel: nil session provider")
	}
	if cfg.ProxyPort == 0 {
		cfg.ProxyPort = defaultProxyPort
	}
	if cfg.ProxyPort < 1 || cfg.ProxyPort > 65535 {
		return errors.New("tunnel: invalid proxy port")
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = defaultHandshakeTTL
	}
	if cfg.HandshakeTimeout < 0 {
		return errors.New("tunnel: invalid handshake timeout")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &server{ctx: ctx, listener: listener, cfg: cfg, conns: make(map[net.Conn]struct{})}
	watcherDone := make(chan struct{})
	go s.watchCancellation(watcherDone)

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			cancel()
			_ = listener.Close()
			s.closeAll()
			close(watcherDone)
			s.wg.Wait()
			return err
		}
		s.add(conn)
		s.wg.Add(1)
		info := connInfo{id: s.next.Add(1), accepted: time.Now()}
		go func() {
			defer s.wg.Done()
			s.handle(conn, info)
		}()
	}

	s.closeAll()
	close(watcherDone)
	s.wg.Wait()
	return nil
}

type server struct {
	ctx      context.Context
	listener net.Listener
	cfg      Config

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
	next  atomic.Uint64
}

// connInfo 标识单个本地客户端连接，随该连接的所有事件一并报告。
type connInfo struct {
	id       uint64
	accepted time.Time
}

func (s *server) watchCancellation(done <-chan struct{}) {
	select {
	case <-s.ctx.Done():
		s.listener.Close()
		s.closeAll()
	case <-done:
	}
}

func (s *server) add(conn net.Conn) {
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		_ = conn.Close()
		return
	}
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *server) remove(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func (s *server) closeAll() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (s *server) handle(client net.Conn, info connInfo) {
	defer func() {
		s.remove(client)
		_ = client.Close()
	}()
	if s.ctx.Err() != nil {
		return
	}

	params, err := s.cfg.Provider(s.ctx)
	if err != nil || params.YxzIP == "" || params.SessionID == "" || params.Username == "" {
		s.report(info, StageSession, false, nil)
		return
	}
	s.report(info, StageSession, true, nil)

	dialer := net.Dialer{Timeout: s.cfg.HandshakeTimeout}
	proxy, err := dialer.DialContext(s.ctx, "tcp", net.JoinHostPort(params.YxzIP, strconv.Itoa(s.cfg.ProxyPort)))
	if err != nil {
		s.report(info, StageDial, false, nil)
		return
	}
	s.add(proxy)
	defer func() {
		s.remove(proxy)
		_ = proxy.Close()
	}()
	s.report(info, StageDial, true, nil)

	if s.cfg.Activate != nil {
		if err := s.cfg.Activate(s.ctx, params); err != nil {
			s.report(info, StageActivate, false, nil)
			return
		}
		s.report(info, StageActivate, true, nil)
	}

	if err := s.handshake(proxy, params); err != nil {
		s.report(info, StageHandshake, false, nil)
		return
	}
	traffic := &Traffic{}
	s.report(info, StageHandshake, true, traffic)

	proxyReader := bufio.NewReader(proxy)
	copyDone := make(chan CloseReason, 2)
	go func() {
		copyDirection(client, &traffic.down, proxyReader)
		copyDone <- ReasonUpstream
	}()
	go func() {
		copyDirection(proxy, &traffic.up, client)
		copyDone <- ReasonClient
	}()
	// 先结束的方向即关闭方；停止隧道会关闭两端，此时归为主动停止。
	reason := <-copyDone
	if s.ctx.Err() != nil {
		reason = ReasonStopped
	}
	_ = client.Close()
	_ = proxy.Close()
	<-copyDone
	if s.cfg.OnEvent != nil {
		// 主动停止也要报告，调用方需要知道连接已在 Serve 返回前结束。
		s.cfg.OnEvent(Event{Stage: StageClosed, Success: true, Conn: info.id, Accepted: info.accepted, Time: time.Now(), Traffic: traffic, Reason: reason})
	}
}

func (s *server) report(info connInfo, stage Stage, success bool, traffic *Traffic) {
	if s.cfg.OnEvent != nil && s.ctx.Err() == nil {
		s.cfg.OnEvent(Event{Stage: stage, Success: success, Conn: info.id, Accepted: info.accepted, Time: time.Now(), Traffic: traffic})
	}
}

func (s *server) handshake(proxy net.Conn, params secdb.Params) error {
	// 握手格式为 yabP + sessionId + 用户名字节长度（1 字节）+ 用户名；
	// 长度写错时代理会把多出的用户名字节混入数据库流，登录包随之失效。
	if len(params.Username) > 255 {
		return errors.New("tunnel: username too long for handshake")
	}
	if deadline := time.Now().Add(s.cfg.HandshakeTimeout); s.cfg.HandshakeTimeout > 0 {
		if err := proxy.SetDeadline(deadline); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(proxy, "yabP"+params.SessionID+string([]byte{byte(len(params.Username))})+params.Username); err != nil {
		return err
	}
	response := make([]byte, len(handshakeResponse))
	if _, err := io.ReadFull(proxy, response); err != nil {
		return err
	}
	if string(response) != handshakeResponse {
		return errors.New("tunnel: proxy handshake rejected")
	}
	return proxy.SetDeadline(time.Time{})
}

func copyDirection(dst net.Conn, counter *atomic.Int64, reader io.Reader) {
	_, _ = io.Copy(countingWriter{writer: dst, counter: counter}, reader)
	if halfCloser, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = halfCloser.CloseWrite()
	}

}

// countingWriter 统计实际写出的字节数，供转发期间实时读取。
type countingWriter struct {
	writer  io.Writer
	counter *atomic.Int64
}

func (w countingWriter) Write(buffer []byte) (int, error) {
	n, err := w.writer.Write(buffer)
	w.counter.Add(int64(n))
	return n, err
}
