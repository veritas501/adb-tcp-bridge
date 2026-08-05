package daemon

import (
	"context"
	"net"
	"testing"

	"adb-tcp-bridge/src/internal/bridge"
	"github.com/rs/zerolog"
)

// 优雅重启：Snapshot 输出的恢复配置与 listener 状态完整、字段正确。
func TestManagerSnapshotRestoreState(t *testing.T) {
	logger := zerolog.Nop()
	manager := NewManager(&logger)
	startTestBridge(t, manager, "snap-1")

	items := manager.Snapshot()
	if len(items) != 1 {
		t.Fatalf("Snapshot() len = %d, want 1", len(items))
	}
	item := items[0]
	r := item.Restore
	if r.Serial != "snap-1" || r.ListenHost != "127.0.0.1" {
		t.Fatalf("Restore = %+v, want serial snap-1 on 127.0.0.1", r)
	}
	if r.BackendName != "adb" || r.AuthMode != string(bridge.AuthNone) {
		t.Fatalf("Restore backend/auth = %q/%q, want adb/none", r.BackendName, r.AuthMode)
	}
	if item.Listener == nil {
		t.Fatal("Snapshot() Listener is nil")
	}
	info := manager.List()[0]
	if got := bridge.FormatListenAddr(r.ListenHost, item.Listener.Addr()); got != info.ListenAddr {
		t.Fatalf("Snapshot listener addr = %s, want %s", got, info.ListenAddr)
	}
}

// Snapshot 的顺序自洽：同一调用内 Restore 与 Listener 一一对应。
func TestManagerSnapshotOrderSelfConsistent(t *testing.T) {
	logger := zerolog.Nop()
	manager := NewManager(&logger)
	for _, serial := range []string{"a", "b", "c"} {
		startTestBridge(t, manager, serial)
	}

	items := manager.Snapshot()
	if len(items) != 3 {
		t.Fatalf("Snapshot() len = %d, want 3", len(items))
	}
	seen := map[string]string{}
	for _, item := range items {
		seen[item.Restore.Serial] = item.Listener.Addr().String()
	}
	for _, serial := range []string{"a", "b", "c"} {
		if _, ok := seen[serial]; !ok {
			t.Fatalf("Snapshot() missing serial %q", serial)
		}
	}
}

// 优雅重启：Adopt 用已绑定 listener 直接恢复 bridge，端口与状态保持。
func TestManagerAdoptRestoresBridge(t *testing.T) {
	logger := zerolog.Nop()
	manager := NewManager(&logger)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	r := BridgeRestore{
		Serial:      "adopt-1",
		ListenHost:  "127.0.0.1",
		BackendName: "adb",
		ADBServer:   "127.0.0.1:5037",
		AuthMode:    string(bridge.AuthAcceptAll),
		DeviceID:    "device::ro.product.name=t;ro.product.model=m;ro.product.device=d;",
	}
	info, err := manager.Adopt(context.Background(), r, ln)
	if err != nil {
		t.Fatalf("Adopt() error = %v", err)
	}
	if info.State != "running" {
		t.Fatalf("State = %q, want running", info.State)
	}
	// 监听地址必须与传入 listener 一致：不重新绑定端口。
	want := bridge.FormatListenAddr("127.0.0.1", ln.Addr())
	if info.ListenAddr != want {
		t.Fatalf("ListenAddr = %s, want %s", info.ListenAddr, want)
	}

	list := manager.List()
	if len(list) != 1 || list[0].Serial != "adopt-1" {
		t.Fatalf("List() = %+v, want single adopt-1", list)
	}

	// Adopt 后重复恢复同一 serial 必须报错（与 Start 的幂等约束一致）。
	if _, err := manager.Adopt(context.Background(), r, ln); err == nil {
		t.Fatal("Adopt() duplicate serial succeeded, want error")
	}

	if err := manager.Stop("adopt-1"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if got := manager.List(); len(got) != 0 {
		t.Fatalf("List() after stop = %+v, want empty", got)
	}
}

// Adopt 校验必填字段与非法后端。
func TestManagerAdoptValidation(t *testing.T) {
	logger := zerolog.Nop()
	manager := NewManager(&logger)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	if _, err := manager.Adopt(context.Background(), BridgeRestore{Serial: ""}, ln); err == nil {
		t.Fatal("Adopt() empty serial succeeded, want error")
	}
	bad := BridgeRestore{Serial: "x", BackendName: "bogus"}
	if _, err := manager.Adopt(context.Background(), bad, ln); err == nil {
		t.Fatal("Adopt() unknown backend succeeded, want error")
	}
	if got := manager.List(); len(got) != 0 {
		t.Fatalf("List() after failed adopts = %+v, want empty", got)
	}
}

// 恢复配置序列化 round-trip：exec 传递后能原样解析。
func TestRestoreStateRoundTrip(t *testing.T) {
	bridges := []BridgeRestore{
		{Serial: "s1", ListenHost: "0.0.0.0", BackendName: "adb", ADBServer: "127.0.0.1:5037", AuthMode: "accept-all", DeviceID: "d1"},
		{Serial: "s2", ListenHost: "0.0.0.0", BackendName: "hdc", HDCServer: "127.0.0.1:8710", AuthMode: "none", DeviceID: "d2"},
	}
	encoded, err := encodeRestoreState(bridges)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeRestoreState(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 2 || decoded[1].Serial != "s2" || decoded[1].HDCServer != "127.0.0.1:8710" {
		t.Fatalf("round-trip mismatch: %+v", decoded)
	}
	if _, err := DecodeRestoreState("!!!not-base64!!!"); err == nil {
		t.Fatal("DecodeRestoreState accepted garbage, want error")
	}
}
