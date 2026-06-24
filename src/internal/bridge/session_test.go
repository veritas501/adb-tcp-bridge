package bridge

import (
	"bytes"
	"context"
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
