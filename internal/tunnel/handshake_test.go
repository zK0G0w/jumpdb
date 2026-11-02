package tunnel_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zK0G0w/jumpdb/internal/secdb"
	"github.com/zK0G0w/jumpdb/internal/tunnel"
)

// 代理按长度字节切分用户名；写错时多余字节会混入数据库流，登录包失效。
func TestHandshakeLengthByteMatchesUsernameBytes(t *testing.T) {
	for _, username := range []string{"root", "read_all", "root_fbpay_mgr", "运维", strings.Repeat("u", 255)} {
		t.Run(username[:min(len(username), 16)], func(t *testing.T) {
			received := make(chan string, 1)
			proxyPort := startProxy(t, func(conn net.Conn) {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				prefix := make([]byte, len("yabPsid")+1)
				if _, err := io.ReadFull(conn, prefix); err != nil {
					received <- ""
					return
				}
				name := make([]byte, prefix[len(prefix)-1])
				if _, err := io.ReadFull(conn, name); err != nil {
					received <- ""
					return
				}
				received <- string(prefix[:len(prefix)-1]) + "|" + string(name)
				io.WriteString(conn, "successready")
				io.Copy(io.Discard, conn)
			})
			client := serveOne(t, proxyPort, tunnel.Config{
				Provider: func(context.Context) (secdb.Params, error) {
					return secdb.Params{YxzIP: "127.0.0.1", SessionID: "sid", Username: username}, nil
				},
			})
			if got := <-received; got != "yabPsid|"+username {
				t.Fatalf("代理按长度字节解析出的用户名不正确: %q", got)
			}
			ready := make([]byte, len("ready"))
			if _, err := io.ReadFull(client, ready); err != nil || string(ready) != "ready" {
				t.Fatal("握手后数据库字节未完整转发")
			}
		})
	}
}

func TestHandshakeRejectsUsernameLongerThanLengthByte(t *testing.T) {
	proxyBytes := make(chan int, 1)
	proxyPort := startProxy(t, func(conn net.Conn) {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		n, _ := io.Copy(io.Discard, conn)
		proxyBytes <- int(n)
	})
	events := make(chan tunnel.Event, 4)
	client := serveOne(t, proxyPort, tunnel.Config{
		Provider: func(context.Context) (secdb.Params, error) {
			return secdb.Params{YxzIP: "127.0.0.1", SessionID: "sid", Username: strings.Repeat("u", 256)}, nil
		},
		OnEvent: func(event tunnel.Event) { events <- event },
	})
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("超长用户名的连接未关闭")
	}
	if n := <-proxyBytes; n != 0 {
		t.Fatalf("超长用户名仍向代理发送了 %d 字节", n)
	}
	if last := lastEvent(events, 3); last.Stage != tunnel.StageHandshake || last.Success {
		t.Fatal("超长用户名未报告为握手失败")
	}
}

func TestActivateRunsAfterDialAndBeforeHandshake(t *testing.T) {
	accepted := make(chan struct{})
	var activated atomic.Bool
	order := make(chan error, 1)
	proxyPort := startProxy(t, func(conn net.Conn) {
		defer conn.Close()
		close(accepted)
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		handshake := make([]byte, len("yabPsid\x04root"))
		if _, err := io.ReadFull(conn, handshake); err != nil {
			order <- err
			return
		}
		if !activated.Load() {
			order <- errors.New("握手先于激活到达代理")
			return
		}
		order <- nil
		io.WriteString(conn, "successready")
		io.Copy(io.Discard, conn)
	})
	var activatedSession atomic.Value
	client := serveOne(t, proxyPort, tunnel.Config{
		Provider: func(context.Context) (secdb.Params, error) {
			return secdb.Params{YxzIP: "127.0.0.1", SessionID: "sid", Username: "root"}, nil
		},
		Activate: func(_ context.Context, params secdb.Params) error {
			select {
			case <-accepted:
			case <-time.After(3 * time.Second):
				return errors.New("激活时代理连接尚未建立")
			}
			activatedSession.Store(params.SessionID)
			activated.Store(true)
			return nil
		},
	})
	if err := <-order; err != nil {
		t.Fatal(err)
	}
	if activatedSession.Load() != "sid" {
		t.Fatal("激活未使用本连接申请到的会话")
	}
	ready := make([]byte, len("ready"))
	if _, err := io.ReadFull(client, ready); err != nil || string(ready) != "ready" {
		t.Fatal("激活后握手未完成转发")
	}
}

func TestActivateFailureSkipsHandshake(t *testing.T) {
	proxyBytes := make(chan int, 1)
	proxyPort := startProxy(t, func(conn net.Conn) {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		n, _ := io.Copy(io.Discard, conn)
		proxyBytes <- int(n)
	})
	events := make(chan tunnel.Event, 4)
	client := serveOne(t, proxyPort, tunnel.Config{
		Provider: func(context.Context) (secdb.Params, error) {
			return secdb.Params{YxzIP: "127.0.0.1", SessionID: "sid", Username: "root"}, nil
		},
		Activate: func(context.Context, secdb.Params) error { return errors.New("fixture-private-error") },
		OnEvent:  func(event tunnel.Event) { events <- event },
	})
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("激活失败的连接未关闭")
	}
	if n := <-proxyBytes; n != 0 {
		t.Fatalf("激活失败后仍向代理发送了 %d 字节握手", n)
	}
	if last := lastEvent(events, 3); last.Stage != tunnel.StageActivate || last.Success {
		t.Fatal("激活失败未报告为激活阶段失败")
	}
}

// serveOne 启动隧道并建立一个本地客户端连接，测试结束时取消隧道。
func serveOne(t *testing.T, proxyPort int, cfg tunnel.Config) net.Conn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cfg.ProxyPort = proxyPort
	done := make(chan error, 1)
	go func() { done <- tunnel.Serve(ctx, listener, cfg) }()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	client.SetDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() {
		client.Close()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return client
}

// lastEvent 收集预期数量的阶段通知并返回最后一个，避免与连接关闭竞争。
func lastEvent(events <-chan tunnel.Event, count int) tunnel.Event {
	var last tunnel.Event
	for range count {
		select {
		case last = <-events:
		case <-time.After(3 * time.Second):
			return tunnel.Event{}
		}
	}
	return last
}
