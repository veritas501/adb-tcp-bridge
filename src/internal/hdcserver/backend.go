package hdcserver

import (
	"context"
	"fmt"
	"net"
	"time"
)

const (
	DefaultAddr = "127.0.0.1:8710"

	handshakeMessage = "OHOS HDC"
	handshakeSize    = 44
	connectKeyOffset = 12
	connectKeySize   = 32

	syncChunkSize      = 64 * 1024
	maxHDCFrameSize    = 16 << 20
	maxSyncPayloadSize = 16 << 20

	shellV2Stdin      byte = 0
	shellV2Stdout     byte = 1
	shellV2Exit       byte = 3
	shellV2CloseStdin byte = 4
	shellV2WindowSize byte = 5
	hdcFileChunkSize  int  = 49152
	hdcFileHeaderSize int  = 64

	pullOKMarker   = "\x01OK\x02"
	pullFailMarker = "\x01NO\x02"

	fileMode = 0o100644
	dirMode  = 0o040755

	hdcKernelChannelClose    uint16 = 2
	hdcKernelWakeupSlaveTask uint16 = 12
	hdcFileInit              uint16 = 3000
	hdcFileCheck             uint16 = 3001
	hdcFileBegin             uint16 = 3002
	hdcFileData              uint16 = 3003
	hdcFileFinish            uint16 = 3004
)

type Backend struct {
	Addr    string
	Timeout time.Duration
	Dialer  net.Dialer
}

func New(addr string) *Backend {
	if addr == "" {
		addr = DefaultAddr
	}
	return &Backend{Addr: addr, Timeout: 30 * time.Second}
}

func (b *Backend) Description() string {
	return "hdc:" + b.addr()
}

func (b *Backend) ReadProperties(ctx context.Context, serial string) (map[string]string, error) {
	output, err := b.runCommand(ctx, "", []byte("list targets -v"))
	if err != nil {
		return nil, err
	}
	properties, ok := parseTargetListProperties(output, serial)
	if !ok {
		return nil, fmt.Errorf("hdc target %q not found in list targets -v", serial)
	}
	b.fillProductProperties(ctx, serial, properties)
	return properties, nil
}

func (b *Backend) fillProductProperties(ctx context.Context, serial string, properties map[string]string) {
	if value := b.firstParam(ctx, serial, "const.product.name", "const.product.devicetype"); value != "" {
		properties["ro.product.name"] = value
	}
	if value := b.firstParam(ctx, serial, "const.product.model", "const.product.software.model"); value != "" {
		properties["ro.product.model"] = value
	}
	if value := b.firstParam(ctx, serial, "const.product.device", "const.product.board", "const.product.devicetype"); value != "" {
		properties["ro.product.device"] = value
	}
}

func (b *Backend) firstParam(ctx context.Context, serial string, keys ...string) string {
	for _, key := range keys {
		output, err := b.runCommand(ctx, serial, []byte("shell param get "+key))
		if err != nil {
			continue
		}
		if value := cleanParamValue(output); value != "" {
			return value
		}
	}
	return ""
}

func (b *Backend) OpenService(ctx context.Context, serial string, service string) (net.Conn, error) {
	if service == "sync:" {
		return b.openSync(ctx, serial), nil
	}
	if spec, ok := parseShellService(service); ok {
		return b.openShell(ctx, serial, spec)
	}
	// localabstract/localfilesystem/localreserved/tcp 通过 HDC fport 暴露为本地 TCP 流。
	if remoteNode, ok := parseForwardService(service); ok {
		return b.openForward(ctx, serial, remoteNode)
	}
	return nil, fmt.Errorf("hdc backend does not support adb service %q", service)
}

func (b *Backend) addr() string {
	if b.Addr == "" {
		return DefaultAddr
	}
	return b.Addr
}
