package daemon

import (
	"fmt"

	"adb-tcp-bridge/src/internal/adbhost"
	"adb-tcp-bridge/src/internal/bridge"
	"adb-tcp-bridge/src/internal/hdcserver"
)

// NewDeviceBackend builds the ADB or HDC backend used by managed bridges.
func NewDeviceBackend(name, adbServerAddr, hdcAddr string) (bridge.DeviceBackend, error) {
	switch name {
	case "adb":
		return bridge.NewADBServerBackend(adbhost.New(adbServerAddr)), nil
	case "hdc":
		return hdcserver.New(hdcAddr), nil
	default:
		return nil, fmt.Errorf("unsupported backend %q", name)
	}
}
