package bridge

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	"adb-tcp-bridge/src/internal/adbhost"
)

const reverseListenHost = "127.0.0.1"

type reverseManager struct {
	session *session

	mu       sync.Mutex
	mappings map[string]*reverseMapping
}

type reverseMapping struct {
	local    string
	remote   string
	listener net.Listener
}

func newReverseManager(session *session) *reverseManager {
	return &reverseManager{
		session:  session,
		mappings: make(map[string]*reverseMapping),
	}
}

func (m *reverseManager) handle(ctx context.Context, command string) []byte {
	switch {
	case command == "list-forward":
		return reverseProtocolString(m.list())
	case command == "killforward-all":
		return m.killAll(ctx)
	case strings.HasPrefix(command, "killforward:"):
		return m.kill(ctx, strings.TrimPrefix(command, "killforward:"))
	case strings.HasPrefix(command, "forward:"):
		return m.forward(ctx, strings.TrimPrefix(command, "forward:"))
	default:
		return reverseFail("not a reverse forwarding command")
	}
}

func (m *reverseManager) forward(ctx context.Context, spec string) []byte {
	noRebind := false
	if strings.HasPrefix(spec, "norebind:") {
		noRebind = true
		spec = strings.TrimPrefix(spec, "norebind:")
	}

	local, remote, ok := strings.Cut(spec, ";")
	if !ok || local == "" || remote == "" || strings.HasPrefix(remote, "*") {
		return reverseFail("bad forward: " + spec)
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(reverseListenHost, "0"))
	if err != nil {
		return reverseFail("cannot bind listener: " + err.Error())
	}

	m.mu.Lock()
	if m.mappings[local] != nil && noRebind {
		m.mu.Unlock()
		listener.Close()
		return reverseFail("cannot rebind existing socket")
	}
	m.mu.Unlock()
	m.evict(ctx, local)

	port := listener.Addr().(*net.TCPAddr).Port
	// Device adbd still owns the device-side listener. It connects back to this
	// bridge-local port, then the bridge opens the requested host-side target on
	// the external adb transport.
	deviceCommand := fmt.Sprintf("forward:%s;tcp:%d", local, port)
	response, err := m.runDeviceReverse(ctx, deviceCommand)
	if err != nil {
		listener.Close()
		return reverseFail(err.Error())
	}
	if !reverseResponseOK(response) {
		listener.Close()
		return response
	}

	resolvedLocal := resolvedReverseLocal(local, response)
	if resolvedLocal != local {
		m.evict(ctx, resolvedLocal)
	}

	mapping := &reverseMapping{
		local:    resolvedLocal,
		remote:   remote,
		listener: listener,
	}
	m.mu.Lock()
	m.mappings[resolvedLocal] = mapping
	m.mu.Unlock()

	go m.accept(mapping)
	return response
}

// evict 移除 key 对应的映射并关闭其 listener，同时通知设备撤销该 forward。
func (m *reverseManager) evict(ctx context.Context, key string) {
	m.mu.Lock()
	mapping := m.mappings[key]
	if mapping != nil {
		delete(m.mappings, key)
	}
	m.mu.Unlock()

	if mapping != nil {
		mapping.listener.Close()
		_, _ = m.runDeviceReverse(ctx, "killforward:"+mapping.local)
	}
}

func (m *reverseManager) accept(mapping *reverseMapping) {
	for {
		conn, err := mapping.listener.Accept()
		if err != nil {
			return
		}
		m.session.openOutbound(mapping.remote, conn)
	}
}

func (m *reverseManager) kill(ctx context.Context, local string) []byte {
	if local == "" {
		return reverseFail("bad killforward: " + local)
	}

	m.mu.Lock()
	mapping := m.mappings[local]
	if mapping != nil {
		delete(m.mappings, local)
	}
	m.mu.Unlock()

	if mapping == nil {
		return reverseFail("listener '" + local + "' not found")
	}

	mapping.listener.Close()
	response, err := m.runDeviceReverse(ctx, "killforward:"+local)
	if err != nil {
		return reverseFail(err.Error())
	}
	return response
}

func (m *reverseManager) killAll(ctx context.Context) []byte {
	for _, mapping := range m.drainMappings() {
		mapping.listener.Close()
		if _, err := m.runDeviceReverse(ctx, "killforward:"+mapping.local); err != nil {
			return reverseFail(err.Error())
		}
	}
	return []byte("OKAY")
}

func (m *reverseManager) closeAll() {
	if m == nil || m.session.config.Host == nil {
		return
	}

	mappings := m.drainMappings()
	if len(mappings) == 0 {
		return
	}

	ctx := context.Background()
	if m.session.config.Host.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.session.config.Host.Timeout)
		defer cancel()
	}
	for _, mapping := range mappings {
		mapping.listener.Close()
		_, _ = m.runDeviceReverse(ctx, "killforward:"+mapping.local)
	}
}

// drainMappings 取出并清空当前所有映射，调用方负责关闭 listener
// 并向设备发送 killforward。
func (m *reverseManager) drainMappings() []*reverseMapping {
	m.mu.Lock()
	defer m.mu.Unlock()

	mappings := make([]*reverseMapping, 0, len(m.mappings))
	for local, mapping := range m.mappings {
		mappings = append(mappings, mapping)
		delete(m.mappings, local)
	}
	return mappings
}

func (m *reverseManager) list() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder
	for _, mapping := range m.mappings {
		b.WriteString("host ")
		b.WriteString(mapping.local)
		b.WriteByte(' ')
		b.WriteString(mapping.remote)
		b.WriteByte('\n')
	}
	return b.String()
}

func (m *reverseManager) runDeviceReverse(ctx context.Context, command string) ([]byte, error) {
	return m.session.config.Host.RunService(ctx, m.session.config.Serial, "reverse:"+command)
}

func reverseResponseOK(response []byte) bool {
	return adbhost.HasStatus(response, adbhost.StatusOKAY)
}

func reverseProtocolString(value string) []byte {
	frame, err := adbhost.EncodeFrameBytes(value)
	if err != nil {
		return reverseFail(err.Error())
	}
	return frame
}

func reverseFail(reason string) []byte {
	return adbhost.FailMessage(reason)
}

// resolvedReverseLocal 当本地端口为自动分配（tcp:0）时，从设备返回的
// OKAY 长度前缀响应里解析出实际端口，拼成 tcp:<port>。
func resolvedReverseLocal(local string, response []byte) string {
	if local != "tcp:0" || !adbhost.HasStatus(response, adbhost.StatusOKAY) {
		return local
	}
	port, ok := adbhost.ParseLengthPrefixed(response, 4)
	if !ok {
		return local
	}
	return "tcp:" + port
}
