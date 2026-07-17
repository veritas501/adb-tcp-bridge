package bridge

import (
	"context"
	"net"

	"adb-tcp-bridge/src/internal/adbhost"
)

// DeviceBackend opens ADB device services against the real target transport.
// The adb backend forwards services to a local adb server; the hdc backend
// translates selected ADB services to hdc commands.
type DeviceBackend interface {
	OpenService(ctx context.Context, serial string, service string) (net.Conn, error)
	ReadProperties(ctx context.Context, serial string) (map[string]string, error)
	Description() string
}

type adbServerBackend struct {
	client *adbhost.Client
}

func newADBServerBackend(client *adbhost.Client) adbServerBackend {
	return adbServerBackend{client: client}
}

func NewADBServerBackend(client *adbhost.Client) DeviceBackend {
	return newADBServerBackend(client)
}

func (b adbServerBackend) OpenService(ctx context.Context, serial string, service string) (net.Conn, error) {
	return b.client.OpenService(ctx, serial, service)
}

func (b adbServerBackend) ReadProperties(ctx context.Context, serial string) (map[string]string, error) {
	return b.client.ReadProperties(ctx, serial)
}

func (b adbServerBackend) Description() string {
	return "adb:" + b.client.Addr
}
