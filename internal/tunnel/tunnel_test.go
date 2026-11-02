package tunnel_test

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zK0G0w/jumpdb/internal/secdb"
	"github.com/zK0G0w/jumpdb/internal/tunnel"
)

func TestServeForwardsAfterSplitHandshakeAndPreservesFollowingBytes(t *testing.T) {
	proxyPort := startProxy(t, func(conn net.Conn) {
		defer conn.Close()
		handshake := make([]byte, len("yabPsession-1\x05alice"))
		if _, err := io.ReadFull(conn, handshake); err != nil {
			t.Errorf("read handshake: %v", err)
			return
		}
		if got, want := string(handshake), "yabPsession-1\x05alice"; got != want {
			t.Errorf("handshake = %q, want %q", got, want)
			return
		}
		if _, err := conn.Write([]byte("suc")); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
		if _, err := conn.Write([]byte("cessgreeting")); err != nil {
			return
		}
		time.Sleep(150 * time.Millisecond)
		request := make([]byte, 64*1024)
		if _, err := io.ReadFull(conn, request); err != nil {
			t.Errorf("read forwarded request: %v", err)
			return
		}
		if _, err := conn.Write(request); err != nil {
			t.Errorf("write forwarded response: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- tunnel.Serve(ctx, listener, tunnel.Config{
			ProxyPort:        proxyPort,
			HandshakeTimeout: 100 * time.Millisecond,
			Provider: func(context.Context) (secdb.Params, error) {
				return secdb.Params{YxzIP: "127.0.0.1", SessionID: "session-1", Username: "alice"}, nil
			},
		})
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer client.Close()

	greeting := make([]byte, len("greeting"))
	if _, err := io.ReadFull(client, greeting); err != nil {
		cancel()
		t.Fatalf("read greeting: %v", err)
	}
	if string(greeting) != "greeting" {
		t.Fatalf("greeting = %q", greeting)
	}

	request := make([]byte, 64*1024)
	for i := range request {
		request[i] = byte(i)
	}
	if _, err := client.Write(request); err != nil {
		cancel()
		t.Fatal(err)
	}
	response := make([]byte, len(request))
	if _, err := io.ReadFull(client, response); err != nil {
		cancel()
		t.Fatalf("read response: %v", err)
	}
	if string(response) != string(request) {
		t.Fatal("forwarded response differs from request")
	}

	cancel()
	if err := <-serverDone; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

func TestServeCancellationClosesActiveConnectionsAndListener(t *testing.T) {
	proxyPort := startProxy(t, func(conn net.Conn) {
		defer conn.Close()
		handshake := make([]byte, len("yabPsession-2\x03bob"))
		if _, err := io.ReadFull(conn, handshake); err != nil {
			return
		}
		_, _ = conn.Write([]byte("successready"))
		<-time.After(time.Second)
	})

	ctx, cancel := context.WithCancel(context.Background())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- tunnel.Serve(ctx, listener, tunnel.Config{
			ProxyPort: proxyPort,
			Provider: func(context.Context) (secdb.Params, error) {
				return secdb.Params{YxzIP: "127.0.0.1", SessionID: "session-2", Username: "bob"}, nil
			},
		})
	}()

	client, err := net.Dial("tcp", addr)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer client.Close()
	greeting := make([]byte, len("ready"))
	if _, err := io.ReadFull(client, greeting); err != nil {
		cancel()
		t.Fatal(err)
	}

	cancel()
	if err := <-serverDone; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("active client remained open after cancellation")
	}

	rebound, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listener was not released: %v", err)
	}
	rebound.Close()
}

func TestServeRequestsOneSessionPerClient(t *testing.T) {
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyListener.Close()
	proxyPort := proxyListener.Addr().(*net.TCPAddr).Port
	const handshake = "yabPsess\x01u"
	proxyErrors := make(chan error, 3)
	proxyDone := make(chan struct{}, 3)
	go func() {
		for range 3 {
			conn, err := proxyListener.Accept()
			if err != nil {
				proxyErrors <- err
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				got := make([]byte, len(handshake))
				if _, err := io.ReadFull(conn, got); err != nil {
					proxyErrors <- err
					return
				}
				if string(got) != handshake {
					proxyErrors <- io.ErrUnexpectedEOF
					return
				}
				if _, err := conn.Write([]byte("successready")); err != nil {
					proxyErrors <- err
					return
				}
				_, _ = io.Copy(io.Discard, conn)
				proxyDone <- struct{}{}
			}(conn)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	var calls atomic.Int32
	go func() {
		serverDone <- tunnel.Serve(ctx, listener, tunnel.Config{
			ProxyPort: proxyPort,
			Provider: func(context.Context) (secdb.Params, error) {
				calls.Add(1)
				return secdb.Params{YxzIP: "127.0.0.1", SessionID: "sess", Username: "u"}, nil
			},
		})
	}()

	clients := make([]net.Conn, 3)
	for i := range clients {
		clients[i], err = net.Dial("tcp", listener.Addr().String())
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		ready := make([]byte, len("ready"))
		if _, err := io.ReadFull(clients[i], ready); err != nil {
			cancel()
			t.Fatalf("client %d read ready: %v", i, err)
		}
		if string(ready) != "ready" {
			cancel()
			t.Fatalf("client %d ready = %q", i, ready)
		}
	}
	if got := calls.Load(); got != 3 {
		cancel()
		t.Fatalf("provider calls = %d, want 3", got)
	}
	for _, client := range clients {
		client.Close()
	}
	for range 3 {
		select {
		case err := <-proxyErrors:
			t.Fatalf("proxy connection: %v", err)
		case <-proxyDone:
		case <-time.After(time.Second):
			t.Fatal("proxy connection did not close")
		}
	}
	cancel()
	if err := <-serverDone; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

func startProxy(t *testing.T, handler func(net.Conn)) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			handler(conn)
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return port
}
