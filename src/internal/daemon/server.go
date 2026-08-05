package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"adb-tcp-bridge/src/internal/control"
	"github.com/rs/zerolog"
)

// Server is the daemon control plane over a Unix domain socket.
type Server struct {
	socketPath string
	logPath    string
	manager    *Manager
	logger     *zerolog.Logger

	rootCtx context.Context
	cancel  context.CancelFunc

	// inheritedListener 是优雅重启时由旧 daemon 移交的控制面 listener；
	// 非 nil 时 Run 直接接管，不再绑定 socket 文件。
	inheritedListener net.Listener
	// udsListener 是当前控制面 listener，restart handoff 时提取 fd。
	udsListener net.Listener
	// restarting 置位后暂停 accept，直到 handoff 完成或失败恢复。
	restarting atomic.Bool
	// handedOff 置位表示 listener 已移交新 daemon：退出清理跳过 socket
	// 文件删除与 bridge 停止（它们已由新 daemon 持有）。
	handedOff atomic.Bool
}

// NewServer constructs a control-plane server.
func NewServer(socketPath string, logPath string, manager *Manager, logger *zerolog.Logger) *Server {
	if logger == nil {
		nop := zerolog.Nop()
		logger = &nop
	}
	return &Server{
		socketPath: socketPath,
		logPath:    logPath,
		manager:    manager,
		logger:     logger,
	}
}

// Run listens on the control socket until ctx is canceled.
func (s *Server) Run(ctx context.Context) error {
	var listener net.Listener
	if s.inheritedListener != nil {
		// 优雅重启接管：socket 文件由旧 daemon 创建且仍在用，跳过
		// prepareSocket（否则会误判为 stale 而删除）。
		listener = s.inheritedListener
		s.logger.Info().
			Str("socket", s.socketPath).
			Msg("daemon took over inherited control socket")
	} else {
		if err := control.EnsureDir(s.socketPath); err != nil {
			return err
		}
		if err := s.prepareSocket(); err != nil {
			return err
		}
		var err error
		listener, err = net.Listen("unix", s.socketPath)
		if err != nil {
			return fmt.Errorf("listen control socket %s: %w", s.socketPath, err)
		}
		_ = os.Chmod(s.socketPath, 0o600)
	}
	s.udsListener = listener

	s.rootCtx, s.cancel = context.WithCancel(ctx)
	defer s.cancel()

	var wg sync.WaitGroup
	defer func() {
		_ = listener.Close()
		if !s.handedOff.Load() {
			_ = os.Remove(s.socketPath)
			s.manager.StopAll()
		}
		wg.Wait()
	}()

	go func() {
		<-s.rootCtx.Done()
		_ = listener.Close()
	}()

	s.logger.Info().
		Str("socket", s.socketPath).
		Str("log_path", s.logPath).
		Msg("daemon listening")

	for {
		if s.restarting.Load() {
			// 优雅重启移交期间暂停 accept：窗口内排队的连接由新 daemon
			// 接管后处理；本进程退出时未处理的连接随之关闭。
			// 同时响应 rootCtx 取消（handoff 完成或外部信号触发退出）。
			if s.rootCtx.Err() != nil {
				return nil
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		conn, err := listener.Accept()
		if err != nil {
			if s.rootCtx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(conn)
		}()
	}
}

// SetInheritedListener 让 Run 接管旧 daemon 移交的控制面 listener（优雅重启路径）。
// 必须在 Run 之前调用。
func (s *Server) SetInheritedListener(l net.Listener) {
	s.inheritedListener = l
}

// failHandoff 记录 handoff 失败并恢复 accept，daemon 继续服务。
// 由 handoff 实现（upgrade_*.go）在失败路径调用。
func (s *Server) failHandoff(err error) {
	s.logger.Error().Err(err).Msg("graceful restart aborted")
	s.restarting.Store(false)
}

func (s *Server) prepareSocket() error {
	if _, err := os.Stat(s.socketPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// Socket file exists: try dial to detect a live daemon.
	conn, err := net.DialTimeout("unix", s.socketPath, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("daemon already running at %s", s.socketPath)
	}
	if removeErr := os.Remove(s.socketPath); removeErr != nil && !os.IsNotExist(removeErr) {
		return fmt.Errorf("remove stale socket %s: %w", s.socketPath, removeErr)
	}
	s.logger.Warn().Str("socket", s.socketPath).Msg("removed stale control socket")
	return nil
}
