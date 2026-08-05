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

// startTestBridge 用测试参数启动一个 bridge，返回 manager，便于测清理行为。
func startTestBridge(t *testing.T, manager *Manager, serial string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	info, err := manager.Start(ctx, StartConfig{
		Serial:          serial,
		ListenHost:      "127.0.0.1",
		ListenStartPort: freePort(t),
		Backend:         stubBackend{},
		BackendName:     "adb",
		AuthMode:        bridge.AuthNone,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if info.State != "running" {
		t.Fatalf("State = %q, want running", info.State)
	}
}

// waitForBridges 轮询直到 List 长度等于 want，或超时失败。
func waitForBridges(t *testing.T, manager *Manager, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(manager.List()) != want {
		if time.Now().After(deadline) {
			t.Fatalf("bridge count = %d, want %d", len(manager.List()), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 设备失联超过 cleanAfterFailure 且期间无成功连接时，reaper 自动清理 bridge。
func TestManagerReapCleansFailedBridge(t *testing.T) {
	logger := zerolog.Nop()
	manager := NewManager(&logger)
	manager.reapInterval = 5 * time.Millisecond
	manager.cleanAfterFailure = 30 * time.Millisecond
	defer manager.StopAll()

	startTestBridge(t, manager, "serial-gone")
	manager.onBackendResult("serial-gone", false)

	// 尚未到期：不应清理
	time.Sleep(10 * time.Millisecond)
	if len(manager.List()) != 1 {
		t.Fatalf("bridge cleaned before expiry, List len = %d", len(manager.List()))
	}

	waitForBridges(t, manager, 0)
}

// 失败后成功连接会重置失联计时；再次失败重新计时后才清理。
func TestManagerReapResetsOnSuccessfulConnection(t *testing.T) {
	logger := zerolog.Nop()
	manager := NewManager(&logger)
	// reaper 周期拉长，避免后台 goroutine 干扰手动 reapExpired 的确定性。
	manager.reapInterval = time.Hour
	manager.cleanAfterFailure = 30 * time.Millisecond
	defer manager.StopAll()

	startTestBridge(t, manager, "serial-recover")
	manager.onBackendResult("serial-recover", false)
	manager.onBackendResult("serial-recover", true) // 设备恢复，计时清零
	manager.reapExpired()
	if len(manager.List()) != 1 {
		t.Fatalf("healthy bridge reaped, List len = %d", len(manager.List()))
	}

	manager.onBackendResult("serial-recover", false)
	time.Sleep(40 * time.Millisecond)
	manager.reapExpired()
	if len(manager.List()) != 0 {
		t.Fatalf("failed bridge not reaped after expiry, List len = %d", len(manager.List()))
	}
}

// 持续失败不刷新计时起点：窗口始终从首次失败算起，避免重试流量无限推迟清理。
func TestManagerReapFailureClockStartsOnce(t *testing.T) {
	logger := zerolog.Nop()
	manager := NewManager(&logger)
	manager.reapInterval = time.Hour
	manager.cleanAfterFailure = 40 * time.Millisecond
	defer manager.StopAll()

	startTestBridge(t, manager, "serial-flaky")
	manager.onBackendResult("serial-flaky", false)
	first := manager.items["serial-flaky"].lastFailedAt
	time.Sleep(20 * time.Millisecond)
	manager.onBackendResult("serial-flaky", false)
	if !manager.items["serial-flaky"].lastFailedAt.Equal(first) {
		t.Fatalf("failure clock refreshed on repeated failure, want start %v, got %v",
			first, manager.items["serial-flaky"].lastFailedAt)
	}

	// 距首次失败已超阈值，即使期间持续失败也应清理。
	time.Sleep(30 * time.Millisecond)
	manager.reapExpired()
	if len(manager.List()) != 0 {
		t.Fatalf("bridge not reaped after continuous failures, List len = %d", len(manager.List()))
	}
}

// Stop 返回后（Serve 已等待所有 session 结束），迟到的结果事件不应复活条目。
func TestManagerResultIgnoredAfterStop(t *testing.T) {
	logger := zerolog.Nop()
	manager := NewManager(&logger)
	defer manager.StopAll()

	startTestBridge(t, manager, "serial-stopped")
	if err := manager.Stop("serial-stopped"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	manager.onBackendResult("serial-stopped", false)
	if len(manager.List()) != 0 {
		t.Fatalf("stale result event resurrected bridge, List len = %d", len(manager.List()))
	}
}
