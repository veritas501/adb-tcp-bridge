package client

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"adb-tcp-bridge/src/internal/control"
)

func TestClientCallRoundTrip(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "adbb.sock")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			return
		}
		var req control.Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			return
		}
		resp := control.Response{
			OK:      true,
			Version: control.ProtocolVersion,
			LogPath: "/tmp/adbb.log",
		}
		if req.Op == control.OpList {
			resp.Bridges = []control.BridgeInfo{{
				Serial:     "s1",
				Backend:    "adb",
				ListenAddr: "127.0.0.1:35555",
				State:      "running",
			}}
		}
		data, _ := json.Marshal(resp)
		_, _ = conn.Write(append(data, '\n'))
	}()

	c := New(socketPath, "", "info")
	c.AutoStart = false
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := c.Call(ctx, control.Request{Op: control.OpList})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !resp.OK || len(resp.Bridges) != 1 || resp.Bridges[0].Serial != "s1" {
		t.Fatalf("Call() response = %+v", resp)
	}
}

func TestClientCallErrorResponse(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "adbb.sock")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		_ = scanner.Scan()
		data, _ := json.Marshal(control.Response{OK: false, Error: "bridge already running for serial \"x\""})
		_, _ = conn.Write(append(data, '\n'))
	}()

	c := New(socketPath, "", "info")
	c.AutoStart = false
	_, err = c.Call(context.Background(), control.Request{Op: control.OpStart, Serial: "x"})
	if err == nil {
		t.Fatal("Call() error = nil, want daemon error")
	}
	if err.Error() != `bridge already running for serial "x"` {
		t.Fatalf("Call() error = %q", err)
	}
}
