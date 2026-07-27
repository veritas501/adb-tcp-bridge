package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"adb-tcp-bridge/src/internal/bridge"
	"adb-tcp-bridge/src/internal/control"
	"github.com/rs/zerolog"
)

const stopWaitTimeout = 5 * time.Second

// Manager tracks running bridge instances keyed by serial.
type Manager struct {
	logger *zerolog.Logger
	mu     sync.Mutex
	items  map[string]*managedBridge
}

type managedBridge struct {
	cancel context.CancelFunc
	info   control.BridgeInfo
	done   <-chan error
}

// StartConfig is the daemon-side start request after flags/defaults are applied.
type StartConfig struct {
	Serial          string
	ListenHost      string
	ListenStartPort int
	Backend         bridge.DeviceBackend
	BackendName     string // "adb" | "hdc"
	AuthMode        bridge.AuthMode
}

// NewManager creates an empty multi-device bridge manager.
func NewManager(logger *zerolog.Logger) *Manager {
	if logger == nil {
		nop := zerolog.Nop()
		logger = &nop
	}
	return &Manager{
		logger: logger,
		items:  make(map[string]*managedBridge),
	}
}

// Start creates a bridge, binds a listen port, and serves in a background goroutine.
func (m *Manager) Start(parent context.Context, cfg StartConfig) (control.BridgeInfo, error) {
	if cfg.Serial == "" {
		return control.BridgeInfo{}, fmt.Errorf("serial is required")
	}

	m.mu.Lock()
	if existing, ok := m.items[cfg.Serial]; ok && existing.info.State == "running" {
		m.mu.Unlock()
		return control.BridgeInfo{}, fmt.Errorf("bridge already running for serial %q", cfg.Serial)
	}
	m.mu.Unlock()

	server, err := bridge.NewServer(bridge.Config{
		ListenHost:      cfg.ListenHost,
		ListenStartPort: cfg.ListenStartPort,
		Serial:          cfg.Serial,
		Backend:         cfg.Backend,
		AuthMode:        cfg.AuthMode,
		Logger:          m.logger,
	})
	if err != nil {
		return control.BridgeInfo{}, err
	}

	ctx, cancel := context.WithCancel(parent)
	listener, err := server.Listen(ctx)
	if err != nil {
		cancel()
		return control.BridgeInfo{}, err
	}

	info := control.BridgeInfo{
		Serial:     cfg.Serial,
		Backend:    cfg.BackendName,
		Auth:       string(cfg.AuthMode),
		ListenAddr: bridge.FormatListenAddr(cfg.ListenHost, listener.Addr()),
		State:      "running",
	}

	done := make(chan error, 1)
	item := &managedBridge{
		cancel: cancel,
		info:   info,
		done:   done,
	}

	m.mu.Lock()
	if existing, ok := m.items[cfg.Serial]; ok && existing.info.State == "running" {
		m.mu.Unlock()
		cancel()
		_ = listener.Close()
		return control.BridgeInfo{}, fmt.Errorf("bridge already running for serial %q", cfg.Serial)
	}
	m.items[cfg.Serial] = item
	m.mu.Unlock()

	go func() {
		err := server.Serve(ctx, listener)
		_ = listener.Close()
		done <- err
		close(done)

		m.mu.Lock()
		defer m.mu.Unlock()
		current, ok := m.items[cfg.Serial]
		if !ok || current != item {
			return
		}
		delete(m.items, cfg.Serial)
		if err != nil && ctx.Err() == nil {
			m.logger.Error().
				Err(err).
				Str("serial", cfg.Serial).
				Msg("bridge serve exited with error")
		}
	}()

	return info, nil
}

// Stop cancels one bridge and waits for Serve to exit.
func (m *Manager) Stop(serial string) error {
	m.mu.Lock()
	item, ok := m.items[serial]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no bridge for serial %q", serial)
	}
	// Take ownership so the serve goroutine's delete is a no-op race-safe path.
	delete(m.items, serial)
	m.mu.Unlock()

	item.cancel()
	select {
	case <-item.done:
	case <-time.After(stopWaitTimeout):
		m.logger.Warn().Str("serial", serial).Msg("timed out waiting for bridge stop")
	}
	return nil
}

// List returns a snapshot of running bridges.
func (m *Manager) List() []control.BridgeInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]control.BridgeInfo, 0, len(m.items))
	for _, item := range m.items {
		out = append(out, item.info)
	}
	return out
}

// Status returns one running bridge by serial.
func (m *Manager) Status(serial string) (control.BridgeInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	item, ok := m.items[serial]
	if !ok {
		return control.BridgeInfo{}, fmt.Errorf("no bridge for serial %q", serial)
	}
	return item.info, nil
}

// StopAll cancels every bridge and waits for each Serve to exit.
func (m *Manager) StopAll() {
	m.mu.Lock()
	items := make([]*managedBridge, 0, len(m.items))
	for serial, item := range m.items {
		items = append(items, item)
		delete(m.items, serial)
	}
	m.mu.Unlock()

	for _, item := range items {
		item.cancel()
	}
	for _, item := range items {
		select {
		case <-item.done:
		case <-time.After(stopWaitTimeout):
			m.logger.Warn().
				Str("serial", item.info.Serial).
				Msg("timed out waiting for bridge stop during StopAll")
		}
	}
}
