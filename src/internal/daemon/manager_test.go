package daemon

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"adb-tcp-bridge/src/internal/bridge"
	"github.com/rs/zerolog"
)

type stubBackend struct{}

func (stubBackend) OpenService(context.Context, string, string) (net.Conn, error) {
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

func (stubBackend) ReadProperties(context.Context, string) (map[string]string, error) {
	return map[string]string{
		"ro.product.name":   "stub",
		"ro.product.model":  "Stub Device",
		"ro.product.device": "stub",
	}, nil
}

func (stubBackend) Description() string { return "stub" }

func TestManagerStartListStop(t *testing.T) {
	logger := zerolog.Nop()
	manager := NewManager(&logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer manager.StopAll()

	startPort := freePort(t)
	info, err := manager.Start(ctx, StartConfig{
		Serial:          "serial-a",
		ListenHost:      "127.0.0.1",
		ListenStartPort: startPort,
		Backend:         stubBackend{},
		BackendName:     "adb",
		AuthMode:        bridge.AuthAcceptAll,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if info.Serial != "serial-a" {
		t.Fatalf("Serial = %q, want serial-a", info.Serial)
	}
	wantAddr := "127.0.0.1:" + strconv.Itoa(startPort)
	if info.ListenAddr != wantAddr {
		t.Fatalf("ListenAddr = %q, want %q", info.ListenAddr, wantAddr)
	}
	if info.State != "running" {
		t.Fatalf("State = %q, want running", info.State)
	}

	list := manager.List()
	if len(list) != 1 {
		t.Fatalf("List() len = %d, want 1", len(list))
	}
	if list[0].Serial != "serial-a" || list[0].ListenAddr != info.ListenAddr {
		t.Fatalf("List()[0] = %+v, want serial-a %s", list[0], info.ListenAddr)
	}

	if _, err := manager.Start(ctx, StartConfig{
		Serial:          "serial-a",
		ListenHost:      "127.0.0.1",
		ListenStartPort: freePort(t),
		Backend:         stubBackend{},
		BackendName:     "adb",
		AuthMode:        bridge.AuthAcceptAll,
	}); err == nil {
		t.Fatal("duplicate Start() error = nil, want error")
	}

	if err := manager.Stop("serial-a"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if got := manager.List(); len(got) != 0 {
		t.Fatalf("List() after Stop len = %d, want 0", len(got))
	}
	if err := manager.Stop("serial-a"); err == nil {
		t.Fatal("Stop() missing serial error = nil, want error")
	}
}

func TestManagerStatus(t *testing.T) {
	logger := zerolog.Nop()
	manager := NewManager(&logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer manager.StopAll()

	info, err := manager.Start(ctx, StartConfig{
		Serial:          "serial-b",
		ListenHost:      "127.0.0.1",
		ListenStartPort: freePort(t),
		Backend:         stubBackend{},
		BackendName:     "hdc",
		AuthMode:        bridge.AuthNone,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	got, err := manager.Status("serial-b")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got.ListenAddr != info.ListenAddr || got.Backend != "hdc" {
		t.Fatalf("Status() = %+v, want listen %s backend hdc", got, info.ListenAddr)
	}
	if _, err := manager.Status("missing"); err == nil {
		t.Fatal("Status(missing) error = nil, want error")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen free port: %v", err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected addr type %T", ln.Addr())
	}
	return addr.Port
}

func TestManagerStartRequiresSerial(t *testing.T) {
	logger := zerolog.Nop()
	manager := NewManager(&logger)
	_, err := manager.Start(context.Background(), StartConfig{
		ListenHost:      "127.0.0.1",
		ListenStartPort: 1,
		Backend:         stubBackend{},
	})
	if err == nil || err.Error() != "serial is required" {
		t.Fatalf("Start() error = %v, want serial is required", err)
	}
}

// Ensure StopAll returns promptly when nothing is running.
func TestManagerStopAllEmpty(t *testing.T) {
	logger := zerolog.Nop()
	manager := NewManager(&logger)
	done := make(chan struct{})
	go func() {
		manager.StopAll()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StopAll() hung on empty manager")
	}
}
