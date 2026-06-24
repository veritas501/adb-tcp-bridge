package bridge

import (
	"context"
	"net"
	"strconv"
	"testing"
)

func TestNewServerDefaultListenConfig(t *testing.T) {
	server, err := NewServer(Config{
		DeviceID: "device::test;",
		Serial:   "serial",
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if server.config.ListenHost != DefaultListenHost {
		t.Fatalf("ListenHost = %q, want %q", server.config.ListenHost, DefaultListenHost)
	}
	if server.config.ListenStartPort != DefaultListenStartPort {
		t.Fatalf("ListenStartPort = %d, want %d", server.config.ListenStartPort, DefaultListenStartPort)
	}
}

func TestFormatDeviceID(t *testing.T) {
	got := formatDeviceID(map[string]string{
		"ro.product.name":   "oriole",
		"ro.product.model":  "Pixel 6",
		"ro.product.device": "oriole",
	})
	want := "device::ro.product.name=oriole;ro.product.model=Pixel 6;ro.product.device=oriole;"
	if got != want {
		t.Fatalf("formatDeviceID() = %q, want %q", got, want)
	}
}

func TestListenTriesNextPortWhenStartPortIsBusy(t *testing.T) {
	startPort, busy := reservePortPair(t)
	defer busy.Close()

	server, err := NewServer(Config{
		DeviceID:        "device::test;",
		Serial:          "serial",
		ListenHost:      "127.0.0.1",
		ListenStartPort: startPort,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	listener, err := server.listen(context.Background())
	if err != nil {
		t.Fatalf("listen() error = %v", err)
	}
	defer listener.Close()

	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort() error = %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("Atoi() error = %v", err)
	}
	if port != startPort+1 {
		t.Fatalf("listen port = %d, want %d", port, startPort+1)
	}
}

func reservePortPair(t *testing.T) (int, net.Listener) {
	t.Helper()

	for port := DefaultListenStartPort; port < 65535; port++ {
		first, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}

		second, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1)))
		if err != nil {
			first.Close()
			continue
		}
		second.Close()
		return port, first
	}
	t.Fatal("no consecutive free ports available")
	return 0, nil
}
