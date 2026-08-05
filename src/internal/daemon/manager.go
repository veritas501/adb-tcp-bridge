package daemon

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"adb-tcp-bridge/src/internal/bridge"
	"adb-tcp-bridge/src/internal/control"
	"github.com/rs/zerolog"
)

const (
	stopWaitTimeout          = 5 * time.Second
	defaultCleanAfterFailure = 10 * time.Minute
	defaultReapInterval      = 30 * time.Second
)

// Manager tracks running bridge instances keyed by serial.
type Manager struct {
	logger *zerolog.Logger
	mu     sync.Mutex
	items  map[string]*managedBridge

	// cleanAfterFailure 是设备失联后的清理阈值：从最近一次失败的
	// lastFailedAt 起，cleanAfterFailure 内没有任何成功连接就摘除 bridge。
	// reapInterval 是后台检查周期。两者默认 10 分钟 / 30 秒，测试可调小。
	cleanAfterFailure time.Duration
	reapInterval      time.Duration
	reapOnce          sync.Once
}

type managedBridge struct {
	cancel   context.CancelFunc
	info     control.BridgeInfo
	done     <-chan error
	listener net.Listener
	restore  BridgeRestore

	// lastFailedAt 是最近一次设备连接失败的起点；零值表示设备当前健康。
	// 失败只在从健康变为失败时记录一次，后续连续失败不刷新起点，保证
	// "失败后 cleanAfterFailure 内没有重新起来过"的窗口始终从首次失败算起；
	// 任何一次成功连接将其清零。
	lastFailedAt time.Time
}

// StartConfig is the daemon-side start request after flags/defaults are applied.
type StartConfig struct {
	Serial          string
	ListenHost      string
	ListenStartPort int
	Backend         bridge.DeviceBackend
	BackendName     string // "adb" | "hdc"
	// ADBServer / HDCServer 记录后端地址，供优雅重启时原样重建 Backend。
	ADBServer string
	HDCServer string
	AuthMode  bridge.AuthMode
}

// NewManager creates an empty multi-device bridge manager.
func NewManager(logger *zerolog.Logger) *Manager {
	if logger == nil {
		nop := zerolog.Nop()
		logger = &nop
	}
	return &Manager{
		logger:            logger,
		items:             make(map[string]*managedBridge),
		cleanAfterFailure: defaultCleanAfterFailure,
		reapInterval:      defaultReapInterval,
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

	// reaper 随首个 bridge 启动，生命周期绑定 daemon 的 parent ctx。
	m.reapOnce.Do(func() { go m.runReaper(parent) })

	server, err := bridge.NewServer(bridge.Config{
		ListenHost:      cfg.ListenHost,
		ListenStartPort: cfg.ListenStartPort,
		Serial:          cfg.Serial,
		Backend:         cfg.Backend,
		AuthMode:        cfg.AuthMode,
		Logger:          m.logger,
		// 转发层每次尝试连接设备后回调：成功=设备可达，失败=设备失联。
		OnBackendResult: func(ok bool) { m.onBackendResult(cfg.Serial, ok) },
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
	// listener 已绑定，Listen 不再依赖 ctx；Serve 的生命周期由 startItem 管理。
	cancel()

	restore := BridgeRestore{
		Serial:      cfg.Serial,
		ListenHost:  cfg.ListenHost,
		BackendName: cfg.BackendName,
		ADBServer:   cfg.ADBServer,
		HDCServer:   cfg.HDCServer,
		AuthMode:    string(cfg.AuthMode),
		DeviceID:    server.DeviceID(),
	}
	return m.startItem(parent, cfg, server, listener, restore)
}

// startItem 登记一台 bridge 并启动 Serve goroutine，Start 与 Adopt 共用。
// listener 已绑定：Start 来自 Listen，Adopt 来自旧 daemon 移交。
func (m *Manager) startItem(parent context.Context, cfg StartConfig, server *bridge.Server, listener net.Listener, restore BridgeRestore) (control.BridgeInfo, error) {
	ctx, cancel := context.WithCancel(parent)
	info := control.BridgeInfo{
		Serial:     cfg.Serial,
		Backend:    cfg.BackendName,
		Auth:       string(cfg.AuthMode),
		ListenAddr: bridge.FormatListenAddr(cfg.ListenHost, listener.Addr()),
		State:      "running",
	}

	done := make(chan error, 1)
	item := &managedBridge{
		cancel:   cancel,
		info:     info,
		done:     done,
		listener: listener,
		restore:  restore,
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

// Adopt 恢复一台由旧 daemon 移交的 bridge：listener 已绑定且状态完整，
// 直接开始服务，不重新绑定端口、不重新查询设备属性。
func (m *Manager) Adopt(parent context.Context, r BridgeRestore, listener net.Listener) (control.BridgeInfo, error) {
	if r.Serial == "" {
		return control.BridgeInfo{}, fmt.Errorf("serial is required")
	}

	m.mu.Lock()
	if existing, ok := m.items[r.Serial]; ok && existing.info.State == "running" {
		m.mu.Unlock()
		return control.BridgeInfo{}, fmt.Errorf("bridge already running for serial %q", r.Serial)
	}
	m.mu.Unlock()

	// reaper 随首个 bridge 启动，生命周期绑定 daemon 的 parent ctx。
	m.reapOnce.Do(func() { go m.runReaper(parent) })

	backend, err := NewDeviceBackend(r.BackendName, r.ADBServer, r.HDCServer)
	if err != nil {
		return control.BridgeInfo{}, err
	}
	server, err := bridge.NewServer(bridge.Config{
		ListenHost: r.ListenHost,
		Serial:     r.Serial,
		Backend:    backend,
		AuthMode:   bridge.AuthMode(r.AuthMode),
		DeviceID:   r.DeviceID,
		Logger:     m.logger,
		// 转发层每次尝试连接设备后回调：成功=设备可达，失败=设备失联。
		OnBackendResult: func(ok bool) { m.onBackendResult(r.Serial, ok) },
	})
	if err != nil {
		return control.BridgeInfo{}, err
	}

	return m.startItem(parent, StartConfig{
		Serial:      r.Serial,
		ListenHost:  r.ListenHost,
		Backend:     backend,
		BackendName: r.BackendName,
		ADBServer:   r.ADBServer,
		HDCServer:   r.HDCServer,
		AuthMode:    bridge.AuthMode(r.AuthMode),
	}, server, listener, r)
}

// SnapshotItem 是 Snapshot 的一项：恢复配置与对应 TCP listener 配对。
type SnapshotItem struct {
	Restore  BridgeRestore
	Listener net.Listener
}

// Snapshot 返回所有运行中 bridge 的恢复状态与其 TCP listener，供优雅重启
// 时把监听 fd 移交给新 daemon。同一调用内 slice 顺序自洽（Restore 与
// Listener 一一对应）；调用方不得关闭 listener。
func (m *Manager) Snapshot() []SnapshotItem {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]SnapshotItem, 0, len(m.items))
	for _, item := range m.items {
		out = append(out, SnapshotItem{Restore: item.restore, Listener: item.listener})
	}
	return out
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

// onBackendResult 记录一次后端连接结果：成功清除失联计时（设备重新可达），
// 失败仅在设备从健康变为失联时开始计时。由转发层 goroutine 调用，频率低。
func (m *Manager) onBackendResult(serial string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	item, found := m.items[serial]
	if !found {
		return
	}
	if ok {
		item.lastFailedAt = time.Time{}
		return
	}
	if item.lastFailedAt.IsZero() {
		item.lastFailedAt = time.Now()
	}
}

// runReaper 周期检查失联超时的 bridge，直到 ctx 取消。
func (m *Manager) runReaper(ctx context.Context) {
	ticker := time.NewTicker(m.reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reapExpired()
		}
	}
}

// reapExpired 摘除失联超时的 bridge：从 lastFailedAt 起 cleanAfterFailure 内
// 没有任何成功连接即视为失联。与 Stop 相同，先移除 map 条目再 cancel，
// 避免与并发 Stop/Serve 退出竞态。
func (m *Manager) reapExpired() {
	now := time.Now()
	m.mu.Lock()
	var expired []*managedBridge
	for serial, item := range m.items {
		if !item.lastFailedAt.IsZero() && now.Sub(item.lastFailedAt) >= m.cleanAfterFailure {
			delete(m.items, serial)
			expired = append(expired, item)
		}
	}
	m.mu.Unlock()

	for _, item := range expired {
		item.cancel()
		select {
		case <-item.done:
		case <-time.After(stopWaitTimeout):
			m.logger.Warn().
				Str("serial", item.info.Serial).
				Msg("timed out waiting for bridge cleanup")
		}
		m.logger.Info().
			Str("serial", item.info.Serial).
			Str("listen_addr", item.info.ListenAddr).
			Dur("unhealthy_for", time.Since(item.lastFailedAt)).
			Msg("bridge cleaned up: no successful device connection since failure")
	}
}
