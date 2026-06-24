package adbhost

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout = 10 * time.Second
	statusOKAY     = "OKAY"
	statusFAIL     = "FAIL"
)

type Client struct {
	Addr    string
	Timeout time.Duration
	Dialer  net.Dialer
}

func New(addr string) *Client {
	return &Client{
		Addr:    addr,
		Timeout: defaultTimeout,
	}
}

func (c *Client) OpenService(ctx context.Context, serial string, service string) (net.Conn, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}

	if err := c.sendCommand(conn, "host:transport:"+serial); err != nil {
		conn.Close()
		return nil, err
	}
	if err := c.sendCommand(conn, service); err != nil {
		conn.Close()
		return nil, err
	}

	// service stream 进入长连接模式，清掉命令阶段 deadline。
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// RunService 打开 service 并读完整个响应直到对端关闭连接。
// 仅适用于短小的一次性响应（如 reverse 控制命令），切勿用于
// shell: 等流式/长连接 service——io.ReadAll 会无界缓冲且永不返回。
func (c *Client) RunService(ctx context.Context, serial string, service string) ([]byte, error) {
	conn, err := c.OpenService(ctx, serial, service)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	return io.ReadAll(conn)
}

func (c *Client) ReadProperties(ctx context.Context, serial string) (map[string]string, error) {
	conn, err := c.OpenService(ctx, serial, "shell:getprop")
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	return parseGetprop(conn)
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	return c.Dialer.DialContext(ctx, "tcp", c.Addr)
}

func (c *Client) sendCommand(conn net.Conn, command string) error {
	if c.Timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(c.Timeout))
	}

	if len(command) > 0xffff {
		return fmt.Errorf("adb host command too large")
	}
	frame := fmt.Sprintf("%04X%s", len(command), command)
	if _, err := io.WriteString(conn, frame); err != nil {
		return err
	}

	var status [4]byte
	if _, err := io.ReadFull(conn, status[:]); err != nil {
		return err
	}
	switch string(status[:]) {
	case statusOKAY:
		return nil
	case statusFAIL:
		return readFailure(conn)
	default:
		return fmt.Errorf("unexpected adb host status %q", string(status[:]))
	}
}

func readFailure(conn net.Conn) error {
	var lengthBuf [4]byte
	if _, err := io.ReadFull(conn, lengthBuf[:]); err != nil {
		return err
	}
	length, err := strconv.ParseUint(string(lengthBuf[:]), 16, 16)
	if err != nil {
		return fmt.Errorf("invalid adb host failure length")
	}
	msg := make([]byte, int(length))
	if length > 0 {
		if _, err := io.ReadFull(conn, msg); err != nil {
			return err
		}
	}
	return fmt.Errorf("adb host command failed: %s", string(msg))
}

func parseGetprop(r io.Reader) (map[string]string, error) {
	properties := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		key, value, ok := parseGetpropLine(scanner.Text())
		if ok {
			properties[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return properties, nil
}

func parseGetpropLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "[") {
		return "", "", false
	}

	parts := strings.SplitN(line, "]: [", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	key := strings.TrimPrefix(parts[0], "[")
	value := strings.TrimSuffix(parts[1], "]")
	if key == "" {
		return "", "", false
	}
	return key, value, true
}
