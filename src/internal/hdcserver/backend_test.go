package hdcserver

import (
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

func (s *fakeHDCServer) close() {
	s.closeOnce.Do(func() {
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
