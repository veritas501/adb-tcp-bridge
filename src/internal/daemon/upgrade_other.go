//go:build !unix

package daemon

import (
	"context"
	"fmt"
	"net"

	"adb-tcp-bridge/src/internal/control"
	"github.com/rs/zerolog"
)

// restartSupported 报告当前平台是否支持 fd 传递式优雅重启。
func restartSupported() bool {
	return false
}

// handoff 在非 Unix 平台不支持 fd 传递：restart 请求会在 dispatch 阶段被
// 拒绝，这里保留签名以防未来平台接入。
func (s *Server) handoff(req control.Request) {
	s.failHandoff(fmt.Errorf("graceful restart is not supported on this platform"))
}

// ParseInheritFds 在非 Unix 平台不支持 listener 继承。
func ParseInheritFds(spec string) ([]int, error) {
	return nil, fmt.Errorf("listener inheritance is not supported on this platform")
}

// RestoreInherited 在非 Unix 平台不支持 listener 继承。
func RestoreInherited(parent context.Context, manager *Manager, fds []int, state []BridgeRestore, logger *zerolog.Logger) (net.Listener, error) {
	return nil, fmt.Errorf("listener inheritance is not supported on this platform")
}
