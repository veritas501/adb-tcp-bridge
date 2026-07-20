package hdcserver

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	forwardListenHost   = "127.0.0.1"
	forwardSetupRetries = 16
	forwardCleanupWait  = 5 * time.Second
	// 不预绑定端口：在 WSL 访问 Windows hdc.exe 时，Linux 侧 listen 过的端口
	// 会让 Windows 侧 fport bind 失败。改为在高位区间随机挑选并重试。
	forwardPortMin = 40000
	forwardPortMax = 60000
	// HDC fport 对不存在的 abstract socket 仍返回 setup OK，但 dial 后对端会
	// 立刻关闭。用短读探测把这种情况变成 OpenService 失败，而不是先 OKAY 再 RST。
	forwardAliveProbe = 100 * time.Millisecond
)

// openForward 将 ADB 设备侧本地服务（localabstract/localfilesystem/localreserved/tcp）
// 翻译为 HDC fport：在 hdc server 本机临时监听一个 TCP 端口，映射到设备节点，
// 再 dial 该端口得到可读写的 net.Conn。连接关闭时撤销 fport 规则。
//
// 之所以不能直接开 HDC channel 连 abstract socket，是因为 HDC 没有等价于
// ADB OPEN localabstract: 的一次性流式命令；设备侧 socket 只能通过 fport 规则暴露。
func (b *Backend) openForward(ctx context.Context, serial string, remoteNode string) (net.Conn, error) {
	var lastErr error
	for range forwardSetupRetries {
		port, err := randomForwardPort()
		if err != nil {
			return nil, err
		}
		localNode := "tcp:" + strconv.Itoa(port)
		setupCmd := "fport " + localNode + " " + remoteNode
		output, err := b.runFportCommand(ctx, serial, setupCmd)
		if err != nil {
			lastErr = err
			// 端口占用或跨 OS 端口冲突时换端口重试。
			if strings.Contains(output, "TCP Port listen failed") {
				continue
			}
			return nil, fmt.Errorf("hdc fport setup %q: %w", setupCmd, err)
		}

		cleanup := func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), forwardCleanupWait)
			defer cancel()
			_, _ = b.runFportCommand(cleanupCtx, serial, "fport rm "+localNode+" "+remoteNode)
		}

		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(forwardListenHost, strconv.Itoa(port)))
		if err != nil {
			cleanup()
			lastErr = fmt.Errorf("dial hdc fport %s: %w", localNode, err)
			// WSL/Windows 端口空间不一致或 fport 尚未就绪时换端口重试。
			continue
		}

		liveConn, err := ensureForwardAlive(conn)
		if err != nil {
			_ = conn.Close()
			cleanup()
			return nil, fmt.Errorf("hdc fport %s -> %s: %w", localNode, remoteNode, err)
		}

		return &forwardConn{
			Conn:    liveConn,
			cleanup: cleanup,
		}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("exhausted %d local port attempts", forwardSetupRetries)
	}
	return nil, fmt.Errorf("hdc fport setup for %s: %w", remoteNode, lastErr)
}

// ensureForwardAlive 探测 fport 连接是否在建立后立刻被远端关闭。
// 不存在的 abstract/unix socket 常见表现是 dial 成功后马上 EOF/RST。
// 若短窗口内读到数据，用 prefixConn 把这一个字节还回后续 Read。
func ensureForwardAlive(conn net.Conn) (net.Conn, error) {
	_ = conn.SetReadDeadline(time.Now().Add(forwardAliveProbe))
	var buf [1]byte
	n, err := conn.Read(buf[:])
	_ = conn.SetReadDeadline(time.Time{})

	if n > 0 {
		return &prefixConn{Conn: conn, prefix: append([]byte(nil), buf[:n]...)}, nil
	}
	if err == nil {
		return conn, nil
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return conn, nil
	}
	if err == io.EOF || isConnectionReset(err) {
		return nil, fmt.Errorf("remote closed immediately (node missing or refused)")
	}
	return nil, err
}

// runFportCommand 执行一次性 fport 管理命令，并按 HDC 文本响应判断成败。
// 成功响应形如 "Forwardport result:OK" / "Remove forward ruler success..."；
// 失败响应以 "[Fail]" 开头。
func (b *Backend) runFportCommand(ctx context.Context, serial string, command string) (string, error) {
	output, err := b.runCommand(ctx, serial, []byte(command))
	text := strings.TrimSpace(string(output))
	if isHDCFailureText(text) {
		if text == "" {
			text = "empty hdc fport failure response"
		}
		return text, fmt.Errorf("%s", text)
	}
	if err != nil {
		return text, err
	}
	return text, nil
}

func isHDCFailureText(text string) bool {
	if text == "" {
		return false
	}
	if strings.Contains(text, "[Fail]") {
		return true
	}
	lower := strings.ToLower(text)
	return strings.HasPrefix(lower, "fail") || strings.HasPrefix(lower, "error")
}

// parseForwardService 识别可通过 HDC fport 打开的 ADB 设备本地服务，
// 返回 HDC remotenode（schema:content）。ADB 的 local: 映射为 localfilesystem:。
func parseForwardService(service string) (remoteNode string, ok bool) {
	switch {
	case strings.HasPrefix(service, "localabstract:"):
		if name := strings.TrimPrefix(service, "localabstract:"); name != "" {
			return service, true
		}
	case strings.HasPrefix(service, "localfilesystem:"):
		if name := strings.TrimPrefix(service, "localfilesystem:"); name != "" {
			return service, true
		}
	case strings.HasPrefix(service, "localreserved:"):
		if name := strings.TrimPrefix(service, "localreserved:"); name != "" {
			return service, true
		}
	case strings.HasPrefix(service, "tcp:"):
		port := strings.TrimPrefix(service, "tcp:")
		if port != "" && isAllDigits(port) {
			return service, true
		}
	case strings.HasPrefix(service, "local:"):
		// ADB local: 是 filesystem unix socket 的简写。
		if name := strings.TrimPrefix(service, "local:"); name != "" {
			return "localfilesystem:" + name, true
		}
	}
	return "", false
}

func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func randomForwardPort() (int, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, err
	}
	span := forwardPortMax - forwardPortMin + 1
	return forwardPortMin + int(binary.BigEndian.Uint32(buf[:])%uint32(span)), nil
}

// forwardConn 包装 dial 到 hdc fport 本地端口的连接；Close 时撤销对应 fport 规则，
// 避免临时端口映射在调试工具反复 OPEN 后泄漏。
type forwardConn struct {
	net.Conn
	cleanupOnce sync.Once
	cleanup     func()
}

func (c *forwardConn) Close() error {
	err := c.Conn.Close()
	if c.cleanup != nil {
		c.cleanupOnce.Do(c.cleanup)
	}
	return err
}

// prefixConn 把 ensureForwardAlive 探测时读到的首字节还回后续 Read。
type prefixConn struct {
	net.Conn
	mu     sync.Mutex
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	return c.Conn.Read(p)
}
