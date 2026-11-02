package tunnel_test

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/zK0G0w/jumpdb/internal/secdb"
	"github.com/zK0G0w/jumpdb/internal/tunnel"
)

// recorder 并发收集事件，供测试按连接序号检查。
type recorder struct {
	mu     sync.Mutex
	events []tunnel.Event
}

func (r *recorder) add(event tunnel.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) find(stage tunnel.Stage, conn uint64) (tunnel.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, event := range r.events {
		if event.Stage == stage && (conn == 0 || event.Conn == conn) {
			return event, true
		}
	}
	return tunnel.Event{}, false
}

func waitEvent(t *testing.T, r *recorder, stage tunnel.Stage, conn uint64) tunnel.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if event, ok := r.find(stage, conn); ok {
			return event
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %s 事件超时", stage)
	return tunnel.Event{}
}

// echoProxy 完成握手后先发送 ready，再回显客户端数据；closeAfter 非零时读满后主动关闭。
func echoProxy(t *testing.T, closeAfter int) int {
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
			go echo(conn, closeAfter)
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func echo(conn net.Conn, closeAfter int) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, len("yabPsid\x04root"))); err != nil {
		return
	}
	io.WriteString(conn, "successready")
	if closeAfter > 0 {
		buffer := make([]byte, closeAfter)
		if _, err := io.ReadFull(conn, buffer); err == nil {
			conn.Write(buffer)
		}
		return
	}
	io.Copy(conn, conn)
}

func observedServe(t *testing.T, proxyPort int, r *recorder) (string, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- tunnel.Serve(ctx, listener, tunnel.Config{
			ProxyPort: proxyPort,
			Provider: func(context.Context) (secdb.Params, error) {
				return secdb.Params{YxzIP: "127.0.0.1", SessionID: "sid", Username: "root"}, nil
			},
			OnEvent: r.add,
		})
	}()
	stop := sync.OnceValue(func() error {
		cancel()
		return <-done
	})
	t.Cleanup(func() { stop() })
	return listener.Addr().String(), stop
}

func exchange(t *testing.T, client net.Conn) {
	t.Helper()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	buffer := make([]byte, 5)
	if _, err := io.ReadFull(client, buffer); err != nil || string(buffer) != "ready" {
		t.Fatal("未收到握手后的数据库字节")
	}
	if _, err := io.WriteString(client, "query"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, buffer); err != nil || string(buffer) != "query" {
		t.Fatal("回显不正确")
	}
}

func TestEventsIdentifyConnectionsAndReportLiveTraffic(t *testing.T) {
	r := &recorder{}
	address, _ := observedServe(t, echoProxy(t, 0), r)
	first, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	exchange(t, first)
	second, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	exchange(t, second)

	one := waitEvent(t, r, tunnel.StageHandshake, 1)
	two := waitEvent(t, r, tunnel.StageHandshake, 2)
	if one.Conn == two.Conn || one.Accepted.IsZero() || one.Time.Before(one.Accepted) {
		t.Fatal("连接序号或时间不正确")
	}
	session := waitEvent(t, r, tunnel.StageSession, 1)
	if !session.Accepted.Equal(one.Accepted) {
		t.Fatal("同一连接的事件接入时间不一致")
	}
	if one.Traffic == nil || one.Traffic.Up() != 5 || one.Traffic.Down() != 10 {
		t.Fatal("握手成功后无法读取实时字节数")
	}
}

func TestClosedEventReportsReasonAndTotals(t *testing.T) {
	t.Run("客户端关闭", func(t *testing.T) {
		r := &recorder{}
		address, _ := observedServe(t, echoProxy(t, 0), r)
		client, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		exchange(t, client)
		client.Close()
		closed := waitEvent(t, r, tunnel.StageClosed, 1)
		if closed.Reason != tunnel.ReasonClient || !closed.Success || closed.Traffic.Up() != 5 || closed.Traffic.Down() != 10 {
			t.Fatal("客户端关闭的原因或字节数不正确")
		}
	})
	t.Run("上游关闭", func(t *testing.T) {
		r := &recorder{}
		address, _ := observedServe(t, echoProxy(t, 5), r)
		client, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		exchange(t, client)
		closed := waitEvent(t, r, tunnel.StageClosed, 1)
		if closed.Reason != tunnel.ReasonUpstream {
			t.Fatal("上游关闭原因不正确")
		}
	})
	t.Run("主动停止", func(t *testing.T) {
		r := &recorder{}
		address, stop := observedServe(t, echoProxy(t, 0), r)
		client, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		exchange(t, client)
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		// Serve 返回前必须已报告停止，调用方据此在返回后读取完整结果。
		closed, ok := r.find(tunnel.StageClosed, 1)
		if !ok || closed.Reason != tunnel.ReasonStopped {
			t.Fatal("主动停止未在 Serve 返回前报告")
		}
	})
}
