package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"syscall"

	"adb-tcp-bridge/src/internal/adbhost"
	"github.com/rs/zerolog"
)

const (
	DefaultListenHost      = "0.0.0.0"
	DefaultListenStartPort = 35555
	fallbackDeviceID       = "device::ro.product.name=adb_tcp_bridge;ro.product.model=ADB TCP Bridge;ro.product.device=adb_tcp_bridge;"
)

type AuthMode string

const (
	AuthNone      AuthMode = "none"
	AuthAcceptAll AuthMode = "accept-all"

	// 兼容旧命名：语义仍是收到任意 public key 即放行。
	AuthAcceptPublicKey AuthMode = "accept-publickey"
)

type Config struct {
	ListenHost      string
	ListenStartPort int
	Serial          string
	Host            *adbhost.Client
	Backend         DeviceBackend
	AuthMode        AuthMode
	DeviceID        string
	Logger          *zerolog.Logger
}

type Server struct {
	config Config
}

func NewServer(config Config) (*Server, error) {
	if config.ListenHost == "" {
		config.ListenHost = DefaultListenHost
	}
	if config.ListenStartPort == 0 {
		config.ListenStartPort = DefaultListenStartPort
	}
	if config.ListenStartPort < 1 || config.ListenStartPort > 65535 {
		return nil, fmt.Errorf("invalid listen start port %d", config.ListenStartPort)
	}
	if config.Serial == "" {
		return nil, errors.New("serial is required")
	}
	if config.Host == nil {
		config.Host = adbhost.New("127.0.0.1:5037")
	}
	if config.Backend == nil {
		config.Backend = newADBServerBackend(config.Host)
	}
	if config.AuthMode == "" {
		config.AuthMode = AuthAcceptAll
	}
	if config.AuthMode != AuthNone && config.AuthMode != AuthAcceptAll && config.AuthMode != AuthAcceptPublicKey {
		return nil, fmt.Errorf("unsupported auth mode %q", config.AuthMode)
	}
	config.Logger = normalizeLogger(config.Logger)
	if config.DeviceID == "" {
		deviceID, err := loadDeviceID(context.Background(), config.Backend, config.Serial)
		if err != nil {
			config.Logger.Warn().
				Err(err).
				Str("serial", config.Serial).
				Str("backend", config.Backend.Description()).
				Msg("failed to load device properties, using fallback device id")
			deviceID = fallbackDeviceID
		}
		config.DeviceID = deviceID
	}
	return &Server{config: config}, nil
}

func loadDeviceID(ctx context.Context, backend DeviceBackend, serial string) (string, error) {
	properties, err := backend.ReadProperties(ctx, serial)
	if err != nil {
		return "", err
	}
	return formatDeviceID(properties), nil
}

func formatDeviceID(properties map[string]string) string {
	return fmt.Sprintf(
		"device::ro.product.name=%s;ro.product.model=%s;ro.product.device=%s;",
		properties["ro.product.name"],
		properties["ro.product.model"],
		properties["ro.product.device"],
	)
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	listener, err := s.listen(ctx)
	if err != nil {
		return err
	}
	defer listener.Close()

	s.config.Logger.Info().
		Str("listen_addr", listener.Addr().String()).
		Str("serial", s.config.Serial).
		Str("backend", s.config.Backend.Description()).
		Msg("listening")

	var wg sync.WaitGroup
	defer wg.Wait()

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		wg.Add(1)
		s.config.Logger.Info().
			Str("remote_addr", conn.RemoteAddr().String()).
			Str("remote_host", splitRemoteHost(conn.RemoteAddr().String())).
			Msg("connect from host")
		go func() {
			defer wg.Done()
			newSession(s.config, conn).run(ctx)
		}()
	}
}

func (s *Server) listen(ctx context.Context) (net.Listener, error) {
	listenConfig := &net.ListenConfig{}
	for port := s.config.ListenStartPort; port <= 65535; port++ {
		addr := net.JoinHostPort(s.config.ListenHost, strconv.Itoa(port))
		listener, err := listenConfig.Listen(ctx, "tcp", addr)
		if err == nil {
			return listener, nil
		}
		if !isAddrInUse(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no available listen port from %d", s.config.ListenStartPort)
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

func normalizeLogger(logger *zerolog.Logger) *zerolog.Logger {
	if logger != nil {
		return logger
	}
	nop := zerolog.Nop()
	return &nop
}

func splitRemoteHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
