package bridge

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"adb-tcp-bridge/src/internal/adbwire"
)

func TestAcceptAllAuthorizesSignature(t *testing.T) {
	client, server := newMemoryConn()
	defer client.Close()
	defer server.Close()

	session := newSession(Config{
		AuthMode: AuthAcceptAll,
		DeviceID: "device::ro.product.name=test;ro.product.model=test;ro.product.device=test;",
		Serial:   "serial",
	}, server)
	session.authToken = []byte("01234567890123456789")

	errCh := make(chan error, 1)
	go func() {
		errCh <- session.handleAuth(adbwire.Packet{
			Command: adbwire.CmdAuth,
			Arg0:    adbwire.AuthSignature,
			Payload: []byte("any signature"),
		})
	}()

	packet, err := adbwire.ReadPacket(client)
	if err != nil {
		t.Fatalf("ReadPacket() error = %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("handleAuth() error = %v", err)
	}
	if !session.authorized {
		t.Fatal("session was not authorized")
	}
	if packet.Command != adbwire.CmdCnxn {
		t.Fatalf("packet command = %s, want CNXN", packet.Command)
	}
	if !bytes.HasPrefix(packet.Payload, []byte("device::")) {
		t.Fatalf("packet payload = %q, want device id", packet.Payload)
	}
}

func newMemoryConn() (net.Conn, net.Conn) {
	return net.Pipe()
}

func TestSessionExitsWhenContextIsCanceled(t *testing.T) {
	client, server := newMemoryConn()
	defer client.Close()
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	session := newSession(Config{Serial: "serial"}, server)
	done := make(chan struct{})
	go func() {
		defer close(done)
		session.run(ctx)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session did not exit after context cancellation")
	}
}

func TestSessionRejectsStlsWithClearError(t *testing.T) {
	_, server := newMemoryConn()
	defer server.Close()

	session := newSession(Config{Serial: "serial"}, server)
	err := session.handlePacket(context.Background(), adbwire.Packet{Command: adbwire.CmdStls})
	if err == nil {
		t.Fatal("handlePacket() error = nil, want unsupported error")
	}
	if !strings.Contains(err.Error(), "TLS transport is unsupported") {
		t.Fatalf("handlePacket() error = %q, want TLS unsupported message", err)
	}
}

func TestConnectionCapsAdvertisedVersion(t *testing.T) {
	client, server := newMemoryConn()
	defer client.Close()
	defer server.Close()

	session := newSession(Config{AuthMode: AuthNone, Serial: "serial", DeviceID: "device::test;"}, server)
	errCh := make(chan error, 1)
	go func() {
		errCh <- session.handlePacket(context.Background(), adbwire.Packet{
			Command: adbwire.CmdCnxn,
			Arg0:    defaultVersion + 1,
			Arg1:    defaultMaxPayload,
		})
	}()

	packet, err := adbwire.ReadPacket(client)
	if err != nil {
		t.Fatalf("ReadPacket() error = %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("handlePacket() error = %v", err)
	}
	if packet.Command != adbwire.CmdCnxn {
		t.Fatalf("packet command = %s, want CNXN", packet.Command)
	}
	if packet.Arg0 != defaultVersion {
		t.Fatalf("advertised version = %#x, want %#x", packet.Arg0, defaultVersion)
	}
}

func TestLocalHandlerRoutesReverseOnly(t *testing.T) {
	_, server := newMemoryConn()
	defer server.Close()

	session := newSession(Config{Serial: "serial"}, server)

	if h := session.localHandler("shell:ls"); h != nil {
		t.Fatal("localHandler(shell:) = non-nil, want nil (should use default transport)")
	}
	if h := session.localHandler("reverse:list-forward"); h == nil {
		t.Fatal("localHandler(reverse:) = nil, want a local responder")
	}
}

// resultBackend 返回固定结果的后端：err 非空时 OpenService 失败，
// 否则返回一个已关闭对端的管道连接，让转发尽快读到 EOF 结束。
type resultBackend struct {
	err error
}

func (b resultBackend) OpenService(context.Context, string, string) (net.Conn, error) {
	if b.err != nil {
		return nil, b.err
	}
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

func (b resultBackend) ReadProperties(context.Context, string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (b resultBackend) Description() string { return "resultBackend" }

// service 转发路径按后端连接结果上报 OnBackendResult：连接成功报 true，
// 打开设备服务失败报 false。
func TestServiceReportsBackendResult(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		client, server := newMemoryConn()
		defer client.Close()
		defer server.Close()

		results := make(chan bool, 2)
		session := newSession(Config{
			Backend:         resultBackend{},
			OnBackendResult: func(ok bool) { results <- ok },
		}, server)

		svc := newService(session, 1, 2, "shell:v1")
		done := make(chan struct{})
		go func() {
			svc.run(context.Background())
			close(done)
		}()

		// 读取 OKAY 与随后的 CLSE，避免 session 写阻塞；随后 pump 读到
		// EOF 自行退出。
		for range 2 {
			if _, err := adbwire.ReadPacket(client); err != nil {
				t.Fatalf("ReadPacket(okay/clse) error = %v", err)
			}
		}
		select {
		case ok := <-results:
			if !ok {
				t.Fatal("backend result = false, want true")
			}
		case <-time.After(time.Second):
			t.Fatal("no backend result reported on success")
		}
		<-done
	})

	t.Run("failure", func(t *testing.T) {
		client, server := newMemoryConn()
		defer client.Close()
		defer server.Close()

		results := make(chan bool, 2)
		session := newSession(Config{
			Backend:         resultBackend{err: errors.New("device offline")},
			OnBackendResult: func(ok bool) { results <- ok },
		}, server)

		svc := newService(session, 1, 2, "shell:v1")
		done := make(chan struct{})
		go func() {
			svc.run(context.Background())
			close(done)
		}()

		select {
		case ok := <-results:
			if ok {
				t.Fatal("backend result = true, want false")
			}
		case <-time.After(time.Second):
			t.Fatal("no backend result reported on failure")
		}
		// OpenService 失败后 finish 仍会写 CLSE（remoteID 非零），读掉避免阻塞。
		if _, err := adbwire.ReadPacket(client); err != nil {
			t.Fatalf("ReadPacket(clse) error = %v", err)
		}
		<-done
	})
}

// reverse 控制通道直连设备：打开失败报 false，完整读到响应报 true。
func TestReverseReportsBackendResult(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		_, server := newMemoryConn()
		defer server.Close()

		results := make(chan bool, 2)
		session := newSession(Config{
			Backend:         resultBackend{err: errors.New("device offline")},
			OnBackendResult: func(ok bool) { results <- ok },
		}, server)

		m := newReverseManager(session)
		if _, err := m.runDeviceReverse(context.Background(), "list-forward"); err == nil {
			t.Fatal("runDeviceReverse() error = nil, want error")
		}
		select {
		case ok := <-results:
			if ok {
				t.Fatal("backend result = true, want false")
			}
		case <-time.After(time.Second):
			t.Fatal("no backend result reported on reverse failure")
		}
	})

	t.Run("success", func(t *testing.T) {
		_, server := newMemoryConn()
		defer server.Close()

		results := make(chan bool, 2)
		session := newSession(Config{
			Backend:         resultBackend{},
			OnBackendResult: func(ok bool) { results <- ok },
		}, server)

		m := newReverseManager(session)
		if _, err := m.runDeviceReverse(context.Background(), "list-forward"); err != nil {
			t.Fatalf("runDeviceReverse() error = %v", err)
		}
		select {
		case ok := <-results:
			if !ok {
				t.Fatal("backend result = false, want true")
			}
		case <-time.After(time.Second):
			t.Fatal("no backend result reported on reverse success")
		}
	})
}
