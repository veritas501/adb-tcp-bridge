package hdcserver

import (
	"errors"
	"net"
	"strings"
	"syscall"
)

// isConnectionReset 判断是否为对端 RST/强制关闭。
// HDC fport 在目标 abstract socket 不存在时，常以 connection reset 结束连接。
func isConnectionReset(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if errors.Is(opErr.Err, syscall.ECONNRESET) {
			return true
		}
		if opErr.Err != nil {
			msg := strings.ToLower(opErr.Err.Error())
			if strings.Contains(msg, "connection reset") || strings.Contains(msg, "forcibly closed") {
				return true
			}
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "forcibly closed")
}
