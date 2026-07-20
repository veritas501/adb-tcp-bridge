package hdcserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadPropertiesUsesHDCServerTargetList(t *testing.T) {
	server := newFakeHDCServer(t)
	backend := New(server.addr)
	backend.Timeout = 5 * time.Second

	props, err := backend.ReadProperties(context.Background(), "TARGET123")
	if err != nil {
		t.Fatalf("ReadProperties() error = %v", err)
	}
	if got, want := props["ro.product.name"], "ALN-AL80"; got != want {
		t.Fatalf("ro.product.name = %q, want %q", got, want)
	}
	if got, want := props["ro.product.model"], "ALN_AL80"; got != want {
		t.Fatalf("ro.product.model = %q, want %q", got, want)
	}
	if got, want := props["ro.product.device"], "HWALN"; got != want {
		t.Fatalf("ro.product.device = %q, want %q", got, want)
	}
	server.assertCommand(t, "list targets -v")
	server.assertSerial(t, "")
}

func TestShellServiceBridgesHDCFrames(t *testing.T) {
	server := newFakeHDCServer(t)
	backend := New(server.addr)
	backend.Timeout = 5 * time.Second

	conn, err := backend.OpenService(context.Background(), "SERIAL", "shell:whoami")
	if err != nil {
		t.Fatalf("OpenService(shell:) error = %v", err)
	}
	defer conn.Close()

	output, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll(shell) error = %v", err)
	}
	if got, want := string(output), "shell\n"; got != want {
		t.Fatalf("shell output = %q, want %q", got, want)
	}
	server.assertCommand(t, "shell whoami")
}

func TestLocalAbstractServiceUsesHDCFport(t *testing.T) {
	server := newFakeHDCServer(t)
	backend := New(server.addr)
	backend.Timeout = 5 * time.Second

	conn, err := backend.OpenService(context.Background(), "SERIAL", "localabstract:lldb-platform-live")
	if err != nil {
		t.Fatalf("OpenService(localabstract:) error = %v", err)
	}

	output := make([]byte, len(server.forwardPayload))
	if _, err := io.ReadFull(conn, output); err != nil {
		t.Fatalf("ReadFull(localabstract) error = %v", err)
	}
	if got, want := string(output), string(server.forwardPayload); got != want {
		t.Fatalf("localabstract payload = %q, want %q", got, want)
	}

	setupCmd := <-server.commands
	if !strings.HasPrefix(setupCmd, "fport tcp:") || !strings.HasSuffix(setupCmd, " localabstract:lldb-platform-live") {
		t.Fatalf("setup command = %q, want fport tcp:<port> localabstract:lldb-platform-live", setupCmd)
	}
	localNode := strings.Fields(setupCmd)[1]

	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Close 会异步撤销 fport；等待 cleanup 发出的 rm 命令。
	deadline := time.After(2 * time.Second)
	for {
		select {
		case cmd := <-server.commands:
			if cmd == "fport rm "+localNode+" localabstract:lldb-platform-live" {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for fport rm of %s", localNode)
		}
	}
}

func TestLocalFilesystemServiceMapsToHDCFport(t *testing.T) {
	server := newFakeHDCServer(t)
	backend := New(server.addr)
	backend.Timeout = 5 * time.Second

	conn, err := backend.OpenService(context.Background(), "SERIAL", "local:/dev/socket/paramservice")
	if err != nil {
		t.Fatalf("OpenService(local:) error = %v", err)
	}
	defer conn.Close()

	setupCmd := <-server.commands
	if !strings.HasPrefix(setupCmd, "fport tcp:") || !strings.HasSuffix(setupCmd, " localfilesystem:/dev/socket/paramservice") {
		t.Fatalf("setup command = %q, want fport ... localfilesystem:/dev/socket/paramservice", setupCmd)
	}
}

func TestParseForwardService(t *testing.T) {
	tests := []struct {
		service string
		remote  string
		ok      bool
	}{
		{"localabstract:lldb-platform-live", "localabstract:lldb-platform-live", true},
		{"localabstract:@lldb-platform-live", "localabstract:@lldb-platform-live", true},
		{"localfilesystem:/tmp/x", "localfilesystem:/tmp/x", true},
		{"localreserved:name", "localreserved:name", true},
		{"tcp:12345", "tcp:12345", true},
		{"local:/dev/socket/x", "localfilesystem:/dev/socket/x", true},
		{"localabstract:", "", false},
		{"tcp:", "", false},
		{"tcp:abc", "", false},
		{"shell:ls", "", false},
		{"sync:", "", false},
	}
	for _, tt := range tests {
		remote, ok := parseForwardService(tt.service)
		if ok != tt.ok || remote != tt.remote {
			t.Fatalf("parseForwardService(%q) = (%q, %v), want (%q, %v)", tt.service, remote, ok, tt.remote, tt.ok)
		}
	}
}

func TestOpenServiceRejectsUnsupportedService(t *testing.T) {
	backend := New("127.0.0.1:1")
	_, err := backend.OpenService(context.Background(), "SERIAL", "jdwp:123")
	if err == nil {
		t.Fatal("OpenService(jdwp:) error = nil, want unsupported error")
	}
	if !strings.Contains(err.Error(), "does not support adb service") {
		t.Fatalf("error = %v, want unsupported service message", err)
	}
}

func TestLocalAbstractMissingNodeFailsOpen(t *testing.T) {
	server := newFakeHDCServer(t)
	backend := New(server.addr)
	backend.Timeout = 5 * time.Second

	_, err := backend.OpenService(context.Background(), "SERIAL", "localabstract:missing-gdbserver")
	if err == nil {
		t.Fatal("OpenService(missing abstract) error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "remote closed immediately") {
		t.Fatalf("error = %v, want remote closed immediately", err)
	}

	// setup 后 cleanup 会 rm；至少应有一条 fport 命令。
	select {
	case cmd := <-server.commands:
		if !strings.HasPrefix(cmd, "fport tcp:") {
			t.Fatalf("first command = %q, want fport setup", cmd)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fport setup command")
	}
}

func TestSyncSendUsesHDCFileSendCommand(t *testing.T) {
	server := newFakeHDCServer(t)
	backend := New(server.addr)
	backend.Timeout = 5 * time.Second
	conn, err := backend.OpenService(context.Background(), "SERIAL", "sync:")
	if err != nil {
		t.Fatalf("OpenService(sync:) error = %v", err)
	}
	defer conn.Close()

	writeSyncRequestForTest(t, conn, "SEND", []byte("/data/local/tmp/pushed.txt,33206"), 0)
	writeSyncRequestForTest(t, conn, "DATA", []byte("hello from adb push"), 0)
	writeSyncRequestForTest(t, conn, "DONE", nil, uint32(time.Now().Unix()))

	id, payload := readSyncResponseForTest(t, conn)
	if id != "OKAY" || len(payload) != 0 {
		t.Fatalf("sync response = %q %q, want OKAY empty", id, payload)
	}
	if got, want := readFile(t, server.sentFile), "hello from adb push"; got != want {
		t.Fatalf("sent file = %q, want %q", got, want)
	}
	if got, want := readFile(t, server.remoteFile), "/data/local/tmp/pushed.txt"; got != want {
		t.Fatalf("remote path = %q, want %q", got, want)
	}
	server.assertCommandContains(t, "shell mkdir -p", "/data/local/tmp")
	server.assertCommandContains(t, "file send remote", "/data/local/tmp/pushed.txt")
}

func TestSyncStatAndListExposeDirectories(t *testing.T) {
	server := newFakeHDCServer(t)
	backend := New(server.addr)
	backend.Timeout = 5 * time.Second
	conn, err := backend.OpenService(context.Background(), "SERIAL", "sync:")
	if err != nil {
		t.Fatalf("OpenService(sync:) error = %v", err)
	}
	defer conn.Close()

	writeSyncRequestForTest(t, conn, "STAT", []byte("/data/local/tmp/tree"), 0)
	mode, _, _ := readSyncStatForTest(t, conn)
	if mode&0o170000 != 0o040000 {
		t.Fatalf("STAT mode = %#o, want directory", mode)
	}

	writeSyncRequestForTest(t, conn, "LIST", []byte("/data/local/tmp/tree"), 0)
	entries := readSyncDentsForTest(t, conn)
	if len(entries) != 2 {
		t.Fatalf("LIST entries = %#v, want 2 entries", entries)
	}
	if entries[0].name != "child.txt" || entries[0].mode&0o170000 != 0o100000 {
		t.Fatalf("first entry = %#v, want child.txt file", entries[0])
	}
	if entries[1].name != "subdir" || entries[1].mode&0o170000 != 0o040000 {
		t.Fatalf("second entry = %#v, want subdir directory", entries[1])
	}
}

func TestSyncRecvStreamsHDCRemoteFile(t *testing.T) {
	server := newFakeHDCServer(t)
	backend := New(server.addr)
	backend.Timeout = 5 * time.Second
	conn, err := backend.OpenService(context.Background(), "SERIAL", "sync:")
	if err != nil {
		t.Fatalf("OpenService(sync:) error = %v", err)
	}
	defer conn.Close()

	writeSyncRequestForTest(t, conn, "RECV", []byte("/data/local/tmp/pulled.txt"), 0)

	id, payload := readSyncResponseForTest(t, conn)
	if id != "DATA" {
		t.Fatalf("first sync response = %q, want DATA", id)
	}
	if got, want := string(payload), "hello from hdc pull"; got != want {
		t.Fatalf("pulled payload = %q, want %q", got, want)
	}
	id, payload = readSyncResponseForTest(t, conn)
	if id != "DONE" || len(payload) != 0 {
		t.Fatalf("second sync response = %q %q, want DONE empty", id, payload)
	}
	server.assertCommandContains(t, "shell if ", "/data/local/tmp/pulled.txt")
}

func TestParseRemoteEntryRejectsInvalidMetadata(t *testing.T) {
	for _, line := range []string{
		"f\tnot-size\t0\tbad.txt",
		"f\t1\tnot-mtime\tbad.txt",
		"x\t1\t0\tbad.txt",
	} {
		if _, err := parseRemoteEntry(line); err == nil {
			t.Fatalf("parseRemoteEntry(%q) error = nil, want error", line)
		}
	}
}

func TestReadHDCFrameRejectsOversizeFrame(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], maxHDCFrameSize+1)
	if _, err := readHDCFrame(bytes.NewReader(header[:])); err == nil {
		t.Fatal("readHDCFrame() error = nil, want oversize error")
	}
}

func TestReadSyncRequestRejectsOversizePayload(t *testing.T) {
	var request [8]byte
	copy(request[:4], "DATA")
	binary.LittleEndian.PutUint32(request[4:], maxSyncPayloadSize+1)
	if _, _, err := readSyncRequest(bytes.NewReader(request[:])); err == nil {
		t.Fatal("readSyncRequest() error = nil, want oversize error")
	}
}

type fakeHDCServer struct {
	addr           string
	listener       net.Listener
	done           chan struct{}
	commands       chan string
	serials        chan string
	sentFile       string
	remoteFile     string
	recvRemoteFile string
	closeOnce      sync.Once

	// fport 规则：localNode -> remoteNode，并持有对应本地 listener。
	// 用于模拟 HDC fport 把设备侧节点暴露为 host TCP。
	forwardMu      sync.Mutex
	forwardRules   map[string]string
	forwardListen  map[string]net.Listener
	forwardPayload []byte
}

func newFakeHDCServer(t *testing.T) *fakeHDCServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	dir := t.TempDir()
	server := &fakeHDCServer{
		addr:           listener.Addr().String(),
		listener:       listener,
		done:           make(chan struct{}),
		commands:       make(chan string, 16),
		serials:        make(chan string, 16),
		sentFile:       filepath.Join(dir, "sent"),
		remoteFile:     filepath.Join(dir, "remote"),
		recvRemoteFile: filepath.Join(dir, "recv_remote"),
		forwardRules:   make(map[string]string),
		forwardListen:  make(map[string]net.Listener),
		forwardPayload: []byte("forward-payload"),
	}

	go server.serve()
	t.Cleanup(server.close)
	return server
}

func (s *fakeHDCServer) serve() {
	defer close(s.done)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeHDCServer) handle(conn net.Conn) {
	defer conn.Close()
	handshake := make([]byte, handshakeSize)
	copy(handshake, handshakeMessage)
	binary.BigEndian.PutUint32(handshake[connectKeyOffset:], 1)
	if writeHDCFrame(conn, handshake) != nil {
		return
	}
	clientHandshake, err := readHDCFrame(conn)
	if err != nil {
		return
	}
	serialBytes := clientHandshake[connectKeyOffset : connectKeyOffset+connectKeySize]
	if end := strings.IndexByte(string(serialBytes), 0); end >= 0 {
		serialBytes = serialBytes[:end]
	}
	s.serials <- string(serialBytes)
	commandBytes, err := readHDCFrame(conn)
	if err != nil {
		return
	}
	command := strings.TrimRight(string(commandBytes), "\x00")
	s.commands <- command

	switch {
	case command == "list targets -v":
		_ = writeHDCFrame(conn, []byte("TARGET123\tUSB\tConnected\tlocalhost\thdc\n"))
	case command == "shell param get const.product.name":
		_ = writeHDCFrame(conn, []byte("ALN-AL80\n"))
	case command == "shell param get const.product.model":
		_ = writeHDCFrame(conn, []byte("ALN_AL80\n"))
	case command == "shell param get const.product.device":
		_ = writeHDCFrame(conn, []byte("HWALN\n"))
	case command == "shell whoami":
		_ = writeHDCFrame(conn, []byte("shell\n"))
	case strings.HasPrefix(command, "shell p="):
		if strings.Contains(command, "/data/local/tmp/tree") {
			_ = writeHDCFrame(conn, []byte("d\t0\t0\n"))
		} else {
			_ = writeHDCFrame(conn, []byte("f\t19\t0\n"))
		}
	case strings.HasPrefix(command, "shell d="):
		_ = writeHDCFrame(conn, []byte("f\t19\t0\tchild.txt\nd\t0\t0\tsubdir\n"))
	case strings.HasPrefix(command, "shell if "):
		if strings.Contains(command, "/data/local/tmp/pulled.txt") {
			_ = os.WriteFile(s.recvRemoteFile, []byte("/data/local/tmp/pulled.txt"), 0o644)
			_ = writeHDCFrame(conn, []byte(pullOKMarker+"hello from hdc pull"))
		} else {
			_ = writeHDCFrame(conn, []byte(pullFailMarker+"remote open missing: no such file or directory"))
		}
	case strings.HasPrefix(command, "shell mkdir -p "):
		_ = writeHDCFrame(conn, nil)
	case strings.HasPrefix(command, "shell rm -f "):
		_ = writeHDCFrame(conn, nil)
	case strings.HasPrefix(command, "file send remote "):
		s.handleNativeFileSend(conn, command)
	case strings.HasPrefix(command, "fport "):
		s.handleFport(conn, command)
	}
}

// handleFport 模拟 HDC fport / fport rm。setup 时在指定本地 TCP 端口监听，
// accept 后回写固定 payload，便于验证 OpenService(localabstract:) 的 dial 路径。
func (s *fakeHDCServer) handleFport(conn net.Conn, command string) {
	fields := strings.Fields(command)
	if len(fields) < 2 {
		_ = writeHDCFrame(conn, []byte("[Fail]Incorrect forward command\r\n"))
		return
	}
	switch fields[1] {
	case "rm":
		if len(fields) < 4 {
			_ = writeHDCFrame(conn, []byte("[Fail]Incorrect forward command\r\n"))
			return
		}
		localNode := fields[2]
		remoteNode := fields[3]
		s.forwardMu.Lock()
		currentRemote, ok := s.forwardRules[localNode]
		listener := s.forwardListen[localNode]
		if ok && currentRemote == remoteNode {
			delete(s.forwardRules, localNode)
			delete(s.forwardListen, localNode)
		} else {
			ok = false
		}
		s.forwardMu.Unlock()
		if !ok {
			_ = writeHDCFrame(conn, []byte("[Fail]Remove forward ruler failed, ruler is not exist "+localNode+" "+remoteNode+"\r\n"))
			return
		}
		if listener != nil {
			_ = listener.Close()
		}
		_ = writeHDCFrame(conn, []byte("Remove forward ruler success, ruler:"+localNode+" "+remoteNode+"\r\n"))
		return
	case "ls":
		s.forwardMu.Lock()
		var lines []string
		for localNode, remoteNode := range s.forwardRules {
			lines = append(lines, "SERIAL\t"+localNode+" "+remoteNode+"\t[Forward]")
		}
		s.forwardMu.Unlock()
		if len(lines) == 0 {
			_ = writeHDCFrame(conn, []byte("[Empty]\r\n"))
			return
		}
		_ = writeHDCFrame(conn, []byte(strings.Join(lines, "\n")+"\n"))
		return
	}

	if len(fields) != 3 {
		_ = writeHDCFrame(conn, []byte("[Fail]Incorrect forward command\r\n"))
		return
	}
	localNode := fields[1]
	remoteNode := fields[2]
	if !strings.HasPrefix(localNode, "tcp:") {
		_ = writeHDCFrame(conn, []byte("[Fail]Forward parament failed\r\n"))
		return
	}
	port := strings.TrimPrefix(localNode, "tcp:")
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		_ = writeHDCFrame(conn, []byte("[Fail]TCP Port listen failed at "+port+"\r\n"))
		return
	}

	s.forwardMu.Lock()
	if _, exists := s.forwardRules[localNode]; exists {
		s.forwardMu.Unlock()
		_ = listener.Close()
		_ = writeHDCFrame(conn, []byte("[Fail]TCP Port listen failed at "+port+"\r\n"))
		return
	}
	s.forwardRules[localNode] = remoteNode
	s.forwardListen[localNode] = listener
	payload := append([]byte(nil), s.forwardPayload...)
	s.forwardMu.Unlock()

	go s.serveForwardListener(localNode, listener, payload)
	_ = writeHDCFrame(conn, []byte("Forwardport result:OK\r\n"))
}

func (s *fakeHDCServer) serveForwardListener(localNode string, listener net.Listener, payload []byte) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		// 约定：remote 名以 missing- 开头时模拟“目标 abstract 不存在”——
		// dial 成功后立刻关闭，对应真实 HDC 对缺失节点的行为。
		s.forwardMu.Lock()
		remote := s.forwardRules[localNode]
		s.forwardMu.Unlock()
		if strings.HasPrefix(remote, "localabstract:missing-") || strings.HasPrefix(remote, "localfilesystem:missing-") {
			_ = conn.Close()
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			_, _ = c.Write(payload)
			buf := make([]byte, 64)
			_, _ = c.Read(buf)
		}(conn)
	}
}

func (s *fakeHDCServer) handleNativeFileSend(conn net.Conn, command string) {
	remote := command[strings.LastIndex(command, " ")+1:]
	remote = strings.Trim(remote, "\"")
	_ = os.WriteFile(s.remoteFile, []byte(remote), 0o644)
	if writeHDCCommandFrame(conn, hdcFileInit, []byte("placeholder "+remote)) != nil {
		return
	}
	cmd, _, err := readHDCCommandFrame(conn)
	if err != nil || cmd != hdcKernelWakeupSlaveTask {
		return
	}
	cmd, _, err = readHDCCommandFrame(conn)
	if err != nil || cmd != hdcFileCheck {
		return
	}
	if writeHDCCommandFrame(conn, hdcFileBegin, nil) != nil {
		return
	}
	var content []byte
	for len(content) < len("hello from adb push") {
		cmd, payload, err := readHDCCommandFrame(conn)
		if err != nil || cmd != hdcFileData {
			return
		}
		if len(payload) > hdcFileHeaderSize {
			content = append(content, payload[hdcFileHeaderSize:]...)
		}
	}
	_ = os.WriteFile(s.sentFile, content, 0o644)
	if writeHDCCommandFrame(conn, hdcFileFinish, []byte{1}) != nil {
		return
	}
	cmd, _, err = readHDCCommandFrame(conn)
	if err != nil || cmd != hdcFileFinish {
		return
	}
	cmd, _, _ = readHDCCommandFrame(conn)
	if cmd != hdcKernelChannelClose {
		return
	}
}

func readHDCCommandFrame(conn net.Conn) (uint16, []byte, error) {
	frame, err := readHDCFrame(conn)
	if err != nil {
		return 0, nil, err
	}
	if len(frame) < 2 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return binary.LittleEndian.Uint16(frame[:2]), frame[2:], nil
}

func writeHDCCommandFrame(conn net.Conn, command uint16, payload []byte) error {
	frame := make([]byte, 2, 2+len(payload))
	binary.LittleEndian.PutUint16(frame[:2], command)
	frame = append(frame, payload...)
	return writeHDCFrame(conn, frame)
}

func extractBetween(text string, before string, after string) string {
	start := strings.Index(text, before)
	if start < 0 {
		return ""
	}
	start += len(before)
	end := strings.Index(text[start:], after)
	if end < 0 {
		return text[start:]
	}
	return text[start : start+end]
}

func (s *fakeHDCServer) assertCommand(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-s.commands:
		if got != want {
			t.Fatalf("hdc command = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for hdc command %q", want)
	}
}

func (s *fakeHDCServer) assertCommandContains(t *testing.T, wants ...string) {
	t.Helper()
	select {
	case got := <-s.commands:
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Fatalf("hdc command = %q, want to contain %q", got, want)
			}
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for hdc command containing %q", wants)
	}
}

func (s *fakeHDCServer) assertSerial(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-s.serials:
		if got != want {
			t.Fatalf("hdc serial = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for hdc serial %q", want)
	}
}

func (s *fakeHDCServer) closeForwards() {
	s.forwardMu.Lock()
	listeners := make([]net.Listener, 0, len(s.forwardListen))
	for localNode, listener := range s.forwardListen {
		listeners = append(listeners, listener)
		delete(s.forwardListen, localNode)
		delete(s.forwardRules, localNode)
	}
	s.forwardMu.Unlock()
	for _, listener := range listeners {
		if listener != nil {
			_ = listener.Close()
		}
	}
}

func (s *fakeHDCServer) close() {
	s.closeOnce.Do(func() {
		s.closeForwards()
		_ = s.listener.Close()
		<-s.done
	})
}

func writeSyncRequestForTest(t *testing.T, conn net.Conn, id string, payload []byte, length uint32) {
	t.Helper()
	if length == 0 {
		length = uint32(len(payload))
	}
	var header [8]byte
	copy(header[:4], id)
	binary.LittleEndian.PutUint32(header[4:8], length)
	if _, err := conn.Write(header[:]); err != nil {
		t.Fatalf("write %s header: %v", id, err)
	}
	if id != "DONE" && len(payload) > 0 {
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write %s payload: %v", id, err)
		}
	}
}

func readSyncResponseForTest(t *testing.T, conn net.Conn) (string, []byte) {
	t.Helper()
	var header [8]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		t.Fatalf("read sync response header: %v", err)
	}
	id := string(header[:4])
	length := binary.LittleEndian.Uint32(header[4:8])
	payload := make([]byte, int(length))
	if length > 0 {
		if _, err := io.ReadFull(conn, payload); err != nil {
			t.Fatalf("read sync response payload: %v", err)
		}
	}
	return id, payload
}

func readSyncStatForTest(t *testing.T, conn net.Conn) (uint32, uint32, uint32) {
	t.Helper()
	var response [16]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		t.Fatalf("read STAT response: %v", err)
	}
	if got, want := string(response[:4]), "STAT"; got != want {
		t.Fatalf("stat id = %q, want %q", got, want)
	}
	return binary.LittleEndian.Uint32(response[4:8]),
		binary.LittleEndian.Uint32(response[8:12]),
		binary.LittleEndian.Uint32(response[12:16])
}

func readSyncDentsForTest(t *testing.T, conn net.Conn) []remoteEntry {
	t.Helper()
	var entries []remoteEntry
	for {
		var header [20]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			t.Fatalf("read DENT header: %v", err)
		}
		id := string(header[:4])
		if id == "DONE" {
			return entries
		}
		if id != "DENT" {
			t.Fatalf("dent id = %q, want DENT/DONE", id)
		}
		nameLen := binary.LittleEndian.Uint32(header[16:20])
		name := make([]byte, int(nameLen))
		if _, err := io.ReadFull(conn, name); err != nil {
			t.Fatalf("read DENT name: %v", err)
		}
		entries = append(entries, remoteEntry{
			name:  string(name),
			mode:  binary.LittleEndian.Uint32(header[4:8]),
			size:  uint64(binary.LittleEndian.Uint32(header[8:12])),
			mtime: binary.LittleEndian.Uint32(header[12:16]),
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
