package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
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
	if err := control.EnsureDir(s.socketPath); err != nil {
		return err
	}

	if err := s.prepareSocket(); err != nil {
		return err
	}

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen control socket %s: %w", s.socketPath, err)
	}
	_ = os.Chmod(s.socketPath, 0o600)

	s.rootCtx, s.cancel = context.WithCancel(ctx)
	defer s.cancel()

	var wg sync.WaitGroup
	defer func() {
		_ = listener.Close()
		_ = os.Remove(s.socketPath)
		s.manager.StopAll()
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
