package tunnel_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/zK0G0w/jumpdb/internal/secdb"
	"github.com/zK0G0w/jumpdb/internal/tunnel"
)

func TestStageEventsAndCancellation(t *testing.T) {
	for _, stage := range []tunnel.Stage{tunnel.StageSession, tunnel.StageDial, tunnel.StageActivate, tunnel.StageHandshake} {
		t.Run(string(stage), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			proxy, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer proxy.Close()
			proxyPort := proxy.Addr().(*net.TCPAddr).Port
			if stage == tunnel.StageDial {
				proxy.Close()
			} else {
				go func() {
					connection, err := proxy.Accept()
					if err != nil {
						return
					}
					defer connection.Close()
					connection.SetDeadline(time.Now().Add(time.Second))
					io.ReadFull(connection, make([]byte, len("yabPfixture\x04user")))
					io.WriteString(connection, "failure")
				}()
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			events := make(chan tunnel.Event, 4)
			done := make(chan error, 1)
			go func() {
				done <- tunnel.Serve(ctx, listener, tunnel.Config{
					ProxyPort: proxyPort,
					Provider: func(context.Context) (secdb.Params, error) {
						if stage == tunnel.StageSession {
							return secdb.Params{}, errors.New("fixture-private-error")
						}
						return secdb.Params{YxzIP: "127.0.0.1", SessionID: "fixture", Username: "user"}, nil
					},
					Activate: func(context.Context, secdb.Params) error {
						if stage == tunnel.StageActivate {
							return errors.New("fixture-private-error")
						}
						return nil
					},
					OnEvent: func(event tunnel.Event) {
						events <- event
						if !event.Success {
							cancel()
						}
					},
				})
			}()
			connection, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("阶段通知后未退出")
			}
			close(events)
			var last tunnel.Event
			for event := range events {
				last = event
			}
			if last.Stage != stage || last.Success {
				t.Fatal("失败阶段不正确")
			}
		})
	}
}

func TestCancellationDuringProviderAndHandshakeDoesNotReportFailure(t *testing.T) {
	for _, phase := range []string{"provider", "handshake"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan struct{})
			proxyDone := make(chan struct{})
			proxyPort := 0
			if phase == "handshake" {
				proxyPort = startProxy(t, func(connection net.Conn) {
					defer connection.Close()
					defer close(proxyDone)
					connection.SetDeadline(time.Now().Add(3 * time.Second))
					if _, err := io.ReadFull(connection, make([]byte, len("yabPfixture\x04user"))); err != nil {
						return
					}
					close(ready)
					io.Copy(io.Discard, connection)
				})
			} else {
				close(proxyDone)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				done <- tunnel.Serve(ctx, listener, tunnel.Config{
					ProxyPort: proxyPort,
					Provider: func(ctx context.Context) (secdb.Params, error) {
						if phase == "provider" {
							close(ready)
							<-ctx.Done()
							return secdb.Params{}, ctx.Err()
						}
						return secdb.Params{YxzIP: "127.0.0.1", SessionID: "fixture", Username: "user"}, nil
					},
					OnEvent: func(event tunnel.Event) {
						if !event.Success {
							t.Error("主动取消被报告为连接失败")
						}
					},
				})
			}()
			connection, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			select {
			case <-ready:
			case <-time.After(3 * time.Second):
				t.Fatal("未进入待取消阶段")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("取消超时")
			}
			<-proxyDone
		})
	}
}
