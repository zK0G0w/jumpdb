package tunnel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
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
	StageHandshake Stage = "handshake"
)

// Event 表示某阶段的结果；主动取消不报告为失败。
type Event struct {
	Stage   Stage
	Success bool
}

// Config 描述本地隧道的代理端口、会话申请器和握手超时。
type Config struct {
	ProxyPort        int
	HandshakeTimeout time.Duration
	Provider         Provider
	// OnEvent 可被不同连接并发调用，应及时返回；不传递原始错误以免泄露凭据。
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
		go func() {
			defer s.wg.Done()
			s.handle(conn)
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

func (s *server) handle(client net.Conn) {
	defer func() {
		s.remove(client)
		_ = client.Close()
	}()
	if s.ctx.Err() != nil {
		return
	}

	params, err := s.cfg.Provider(s.ctx)
	if err != nil || params.YxzIP == "" || params.SessionID == "" || params.Username == "" {
		s.report(StageSession, false)
		return
	}
	s.report(StageSession, true)

	dialer := net.Dialer{Timeout: s.cfg.HandshakeTimeout}
	proxy, err := dialer.DialContext(s.ctx, "tcp", net.JoinHostPort(params.YxzIP, strconv.Itoa(s.cfg.ProxyPort)))
	if err != nil {
		s.report(StageDial, false)
		return
	}
	s.add(proxy)
	defer func() {
		s.remove(proxy)
		_ = proxy.Close()
	}()
	s.report(StageDial, true)

	if err := s.handshake(proxy, params); err != nil {
		s.report(StageHandshake, false)
		return
	}
	s.report(StageHandshake, true)

	proxyReader := bufio.NewReader(proxy)
	copyDone := make(chan struct{}, 2)
	go func() {
		copyDirection(client, proxyReader)
		copyDone <- struct{}{}
	}()
	go func() {
		copyDirection(proxy, client)
		copyDone <- struct{}{}
	}()
	<-copyDone
	_ = client.Close()
	_ = proxy.Close()
	<-copyDone
}

func (s *server) report(stage Stage, success bool) {
	if s.cfg.OnEvent != nil && s.ctx.Err() == nil {
		s.cfg.OnEvent(Event{Stage: stage, Success: success})
	}
}

func (s *server) handshake(proxy net.Conn, params secdb.Params) error {
	if deadline := time.Now().Add(s.cfg.HandshakeTimeout); s.cfg.HandshakeTimeout > 0 {
		if err := proxy.SetDeadline(deadline); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(proxy, "yabP"+params.SessionID+"\x04"+params.Username); err != nil {
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

func copyDirection(dst net.Conn, reader io.Reader) {
	_, _ = io.Copy(dst, reader)
	if halfCloser, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = halfCloser.CloseWrite()
	}

}
