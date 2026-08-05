package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"adb-tcp-bridge/src/internal/control"
)

// Client talks to the atb daemon over a Unix domain socket.
type Client struct {
	SocketPath   string
	LogPath      string
	LogLevel     string
	Executable   string
	StartTimeout time.Duration
	// AutoStart enables ensureDaemon on dial failure. Default true for Call.
	// Commands that must not auto-start should set this false or use CallNoStart.
	AutoStart bool
}

// New creates a client with defaults for timeout and auto-start.
func New(socketPath, logPath, logLevel string) *Client {
	if logLevel == "" {
		logLevel = "info"
	}
	return &Client{
		SocketPath:   socketPath,
		LogPath:      logPath,
		LogLevel:     logLevel,
		StartTimeout: 5 * time.Second,
		AutoStart:    true,
	}
}

// Call dials the daemon (auto-starting if needed), sends one request, and returns the response.
func (c *Client) Call(ctx context.Context, req control.Request) (control.Response, error) {
	return c.call(ctx, req, c.AutoStart)
}

// CallNoStart dials without auto-starting the daemon.
func (c *Client) CallNoStart(ctx context.Context, req control.Request) (control.Response, error) {
	return c.call(ctx, req, false)
}

func (c *Client) call(ctx context.Context, req control.Request, autoStart bool) (control.Response, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		if !autoStart {
			return control.Response{}, err
		}
		if err := c.ensureDaemon(ctx); err != nil {
			return control.Response{}, err
		}
		conn, err = c.dial(ctx)
		if err != nil {
			return control.Response{}, fmt.Errorf("dial daemon after start: %w", err)
		}
	}
	defer conn.Close()

	data, err := json.Marshal(req)
	if err != nil {
		return control.Response{}, err
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return control.Response{}, fmt.Errorf("write request: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return control.Response{}, fmt.Errorf("read response: %w", err)
		}
		return control.Response{}, errors.New("empty response from daemon")
	}

	var resp control.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return control.Response{}, fmt.Errorf("decode response: %w", err)
	}
	if !resp.OK {
		if resp.Error == "" {
			return resp, errors.New("daemon request failed")
		}
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// WaitReady 轮询控制面直到 daemon 响应一次版本探测，用于 restart 后确认
// 新 daemon 已接管。旧 daemon 收到 restart 请求后立即停止 accept，因此
// 任何成功响应都来自新 daemon；dial 失败或读超时则重试直到 deadline。
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	req := control.Request{Op: control.OpVersion}
	for {
		conn, err := c.dial(ctx)
		if err == nil {
			// 探测可能排队等待新 daemon accept，限制单次等待避免无限 hang。
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			data, _ := json.Marshal(req)
			if _, err := conn.Write(append(data, '\n')); err == nil {
				scanner := bufio.NewScanner(conn)
				scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
				if scanner.Scan() {
					var resp control.Response
					if err := json.Unmarshal(scanner.Bytes(), &resp); err == nil && resp.OK && resp.Version > 0 {
						_ = conn.Close()
						return nil
					}
				}
			}
			_ = conn.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for daemon at %s", c.SocketPath)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	if c.SocketPath == "" {
		return nil, errors.New("socket path is required")
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", c.SocketPath)
}

func (c *Client) ensureDaemon(ctx context.Context) error {
	executable := c.Executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return fmt.Errorf("resolve executable for auto-start: %w", err)
		}
	}
	logLevel := c.LogLevel
	if logLevel == "" {
		logLevel = "info"
	}

	args := []string{"daemon", "--socket", c.SocketPath}
	if c.LogPath != "" {
		args = append(args, "--log-file", c.LogPath)
	}
	if logLevel != "" {
		args = append(args, "--log-level", logLevel)
	}

	cmd := exec.Command(executable, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	configureDaemonCmd(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	// Detach: do not Wait; release process table entry on Unix.
	go func() { _ = cmd.Process.Release() }()

	timeout := c.StartTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := c.dial(ctx)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for daemon at %s", c.SocketPath)
}

// IsNotRunning reports whether err indicates the daemon control socket is unavailable.
func IsNotRunning(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return errors.Is(err, os.ErrNotExist) || os.IsNotExist(err)
}
