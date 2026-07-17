package hdcserver

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultAddr = "127.0.0.1:8710"

	handshakeMessage = "OHOS HDC"
	handshakeSize    = 44
	connectKeyOffset = 12
	connectKeySize   = 32

	syncChunkSize = 64 * 1024

	shellV2Stdin      byte = 0
	shellV2Stdout     byte = 1
	shellV2Exit       byte = 3
	shellV2CloseStdin byte = 4
	shellV2WindowSize byte = 5
	hdcFileChunkSize  int  = 49152
	hdcFileHeaderSize int  = 64

	pullOKMarker   = "\x01OK\x02"
	pullFailMarker = "\x01NO\x02"

	fileMode = 0o100644
	dirMode  = 0o040755

	hdcKernelChannelClose    uint16 = 2
	hdcKernelWakeupSlaveTask uint16 = 12
	hdcFileInit              uint16 = 3000
	hdcFileCheck             uint16 = 3001
	hdcFileBegin             uint16 = 3002
	hdcFileData              uint16 = 3003
	hdcFileFinish            uint16 = 3004
)

type Backend struct {
	Addr    string
	Timeout time.Duration
	Dialer  net.Dialer
}

func New(addr string) *Backend {
	if addr == "" {
		addr = DefaultAddr
	}
	return &Backend{Addr: addr, Timeout: 30 * time.Second}
}

func (b *Backend) Description() string {
	return "hdc:" + b.addr()
}

func (b *Backend) ReadProperties(ctx context.Context, serial string) (map[string]string, error) {
	output, err := b.runCommand(ctx, "", []byte("list targets -v"))
	if err != nil {
		return nil, err
	}
	properties, ok := parseTargetListProperties(output, serial)
	if !ok {
		return nil, fmt.Errorf("hdc target %q not found in list targets -v", serial)
	}
	b.fillProductProperties(ctx, serial, properties)
	return properties, nil
}

func (b *Backend) fillProductProperties(ctx context.Context, serial string, properties map[string]string) {
	if value := b.firstParam(ctx, serial, "const.product.name", "const.product.devicetype"); value != "" {
		properties["ro.product.name"] = value
	}
	if value := b.firstParam(ctx, serial, "const.product.model", "const.product.software.model"); value != "" {
		properties["ro.product.model"] = value
	}
	if value := b.firstParam(ctx, serial, "const.product.device", "const.product.board", "const.product.devicetype"); value != "" {
		properties["ro.product.device"] = value
	}
}

func (b *Backend) firstParam(ctx context.Context, serial string, keys ...string) string {
	for _, key := range keys {
		output, err := b.runCommand(ctx, serial, []byte("shell param get "+key))
		if err != nil {
			continue
		}
		if value := cleanParamValue(output); value != "" {
			return value
		}
	}
	return ""
}

func (b *Backend) OpenService(ctx context.Context, serial string, service string) (net.Conn, error) {
	if service == "sync:" {
		return b.openSync(ctx, serial), nil
	}
	if spec, ok := parseShellService(service); ok {
		return b.openShell(ctx, serial, spec)
	}
	return nil, fmt.Errorf("hdc backend does not support adb service %q", service)
}

func (b *Backend) openShell(ctx context.Context, serial string, spec shellSpec) (net.Conn, error) {
	command := []byte("shell\x00")
	if spec.command != "" {
		command = []byte("shell " + spec.command)
	}
	channel, err := b.openChannel(ctx, serial, command)
	if err != nil {
		return nil, err
	}

	client, server := net.Pipe()
	go serveShell(server, channel, spec.v2)
	return client, nil
}

func serveShell(conn net.Conn, channel *channelConn, shellV2 bool) {
	defer conn.Close()
	defer channel.Close()

	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = conn.Close()
			_ = channel.Close()
		})
	}

	go func() {
		defer closeBoth()
		if shellV2 {
			_ = copyShellV2Input(channel, conn)
			return
		}
		_, _ = io.Copy(channel, conn)
	}()

	if shellV2 {
		copyShellV2Output(conn, channel)
		_ = writeShellV2Packet(conn, shellV2Exit, uint32LE(0))
	} else {
		_, _ = io.Copy(conn, channel)
	}
	closeBoth()
}

func copyShellV2Input(dst io.Writer, src io.Reader) error {
	for {
		streamID, payload, err := readShellV2Packet(src)
		if err != nil {
			return err
		}
		switch streamID {
		case shellV2Stdin:
			if len(payload) > 0 {
				if _, err := dst.Write(payload); err != nil {
					return err
				}
			}
		case shellV2CloseStdin:
			return nil
		case shellV2WindowSize:
			// hdc server owns the device PTY size; ignore adb resize frames.
		}
	}
}

func copyShellV2Output(dst io.Writer, src io.Reader) {
	buf := make([]byte, syncChunkSize)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if writeShellV2Packet(dst, shellV2Stdout, buf[:n]) != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func readShellV2Packet(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.LittleEndian.Uint32(header[1:])
	payload := make([]byte, int(length))
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return header[0], payload, nil
}

func writeShellV2Packet(w io.Writer, streamID byte, payload []byte) error {
	var header [5]byte
	header[0] = streamID
	binary.LittleEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

func (b *Backend) openSync(ctx context.Context, serial string) net.Conn {
	client, server := newBufferedPipe()
	go func() {
		defer server.Close()
		b.serveSync(ctx, serial, server)
	}()
	return client
}

func newBufferedPipe() (net.Conn, net.Conn) {
	clientToServer := make(chan []byte, 1024)
	serverToClient := make(chan []byte, 1024)
	client := &bufferedPipeConn{readCh: serverToClient, writeCh: clientToServer, closed: make(chan struct{})}
	server := &bufferedPipeConn{readCh: clientToServer, writeCh: serverToClient, closed: make(chan struct{})}
	return client, server
}

type bufferedPipeConn struct {
	readCh    <-chan []byte
	writeCh   chan<- []byte
	readBuf   []byte
	closeOnce sync.Once
	closed    chan struct{}
}

func (c *bufferedPipeConn) Read(p []byte) (int, error) {
	for len(c.readBuf) == 0 {
		select {
		case data, ok := <-c.readCh:
			if !ok {
				return 0, io.EOF
			}
			c.readBuf = data
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

func (c *bufferedPipeConn) Write(p []byte) (n int, err error) {
	data := append([]byte(nil), p...)
	defer func() {
		if recover() != nil {
			n = 0
			err = net.ErrClosed
		}
	}()
	select {
	case c.writeCh <- data:
		return len(p), nil
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *bufferedPipeConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		close(c.writeCh)
	})
	return nil
}

func (c *bufferedPipeConn) LocalAddr() net.Addr              { return dummyAddr("buffered-pipe") }
func (c *bufferedPipeConn) RemoteAddr() net.Addr             { return dummyAddr("buffered-pipe") }
func (c *bufferedPipeConn) SetDeadline(time.Time) error      { return nil }
func (c *bufferedPipeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *bufferedPipeConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr string

func (a dummyAddr) Network() string { return string(a) }
func (a dummyAddr) String() string  { return string(a) }

func (b *Backend) serveSync(ctx context.Context, serial string, conn net.Conn) {
	for {
		id, payload, err := readSyncRequest(conn)
		if err != nil {
			return
		}
		switch id {
		case "STAT":
			b.handleStat(ctx, serial, conn, string(payload))
		case "LIST":
			b.handleList(ctx, serial, conn, string(payload))
		case "SEND":
			b.handleSend(ctx, serial, conn, string(payload))
		case "RECV":
			b.handleRecv(ctx, serial, conn, string(payload))
		case "QUIT":
			return
		default:
			writeSyncFail(conn, fmt.Sprintf("hdc backend does not support sync command %q", id))
			return
		}
	}
}

func (b *Backend) handleStat(ctx context.Context, serial string, conn net.Conn, remote string) {
	info, err := b.remotePathInfo(ctx, serial, remote)
	if err != nil {
		writeSyncStat(conn, remoteEntry{})
		return
	}
	writeSyncStat(conn, info)
}

func (b *Backend) handleList(ctx context.Context, serial string, conn net.Conn, remote string) {
	entries, err := b.remoteList(ctx, serial, remote)
	if err != nil {
		writeSyncFail(conn, err.Error())
		return
	}
	for _, entry := range entries {
		if err := writeSyncDent(conn, entry); err != nil {
			return
		}
	}
	writeSyncDentDone(conn)
}

func writeSyncStat(w io.Writer, entry remoteEntry) {
	var response [16]byte
	copy(response[:4], "STAT")
	binary.LittleEndian.PutUint32(response[4:8], entry.mode)
	binary.LittleEndian.PutUint32(response[8:12], uint32(entry.size))
	binary.LittleEndian.PutUint32(response[12:16], entry.mtime)
	_, _ = w.Write(response[:])
}

func writeSyncDent(w io.Writer, entry remoteEntry) error {
	var header [20]byte
	copy(header[:4], "DENT")
	binary.LittleEndian.PutUint32(header[4:8], entry.mode)
	binary.LittleEndian.PutUint32(header[8:12], uint32(entry.size))
	binary.LittleEndian.PutUint32(header[12:16], entry.mtime)
	binary.LittleEndian.PutUint32(header[16:20], uint32(len(entry.name)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := io.WriteString(w, entry.name)
	return err
}

func writeSyncDentDone(w io.Writer) {
	var header [20]byte
	copy(header[:4], "DONE")
	_, _ = w.Write(header[:])
}

func (b *Backend) handleSend(ctx context.Context, serial string, conn net.Conn, spec string) {
	remote, modeText, ok := strings.Cut(spec, ",")
	if !ok || strings.TrimSpace(remote) == "" {
		writeSyncFail(conn, "invalid SEND path")
		return
	}
	mode, _ := strconv.ParseUint(modeText, 10, 32)
	if uint32(mode)&0o170000 == 0o040000 {
		if err := b.makeRemoteDir(ctx, serial, remote); err != nil {
			writeSyncFail(conn, err.Error())
			return
		}
		writeSyncOkay(conn)
		return
	}

	tmp, err := os.CreateTemp("", "adb-hdc-push-*")
	if err != nil {
		writeSyncFail(conn, err.Error())
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	for {
		id, payload, err := readSyncRequest(conn)
		if err != nil {
			_ = tmp.Close()
			return
		}
		switch id {
		case "DATA":
			if _, err := tmp.Write(payload); err != nil {
				_ = tmp.Close()
				writeSyncFail(conn, err.Error())
				return
			}
		case "DONE":
			if err := tmp.Close(); err != nil {
				writeSyncFail(conn, err.Error())
				return
			}
			if err := b.pushFile(ctx, serial, tmpName, remote, uint32(mode)); err != nil {
				writeSyncFail(conn, err.Error())
				return
			}
			writeSyncOkay(conn)
			return
		default:
			_ = tmp.Close()
			writeSyncFail(conn, fmt.Sprintf("unexpected %s during SEND", id))
			return
		}
	}
}

func (b *Backend) pushFile(ctx context.Context, serial string, local string, remote string, mode uint32) error {
	file, err := os.Open(local)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}

	if err := b.makeRemoteDir(ctx, serial, path.Dir(remote)); err != nil {
		return err
	}

	command := "file send remote " + hdcCommandQuote(path.Base(remote)) + " " + hdcCommandQuote(remote)
	channel, err := b.openChannel(ctx, serial, []byte(command))
	if err != nil {
		return err
	}
	defer channel.Close()

	cmd, _, err := channel.readCommand()
	if err != nil {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return err
	}
	if cmd != hdcFileInit {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return fmt.Errorf("unexpected hdc file init command %d", cmd)
	}
	if err := channel.writeCommand(hdcKernelWakeupSlaveTask, nil); err != nil {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return err
	}
	if err := channel.writeCommand(hdcFileCheck, encodeTransferConfig(uint64(stat.Size()), remote, path.Base(remote), uint64(stat.ModTime().Unix()))); err != nil {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return err
	}

	cmd, _, err = channel.readCommand()
	if err != nil {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return err
	}
	if cmd != hdcFileBegin {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return fmt.Errorf("unexpected hdc file begin command %d", cmd)
	}

	buf := make([]byte, hdcFileChunkSize)
	var index uint64
	for {
		n, readErr := file.Read(buf)
		if n > 0 {
			if err := channel.writeCommand(hdcFileData, encodeTransferPayload(index, buf[:n])); err != nil {
				_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
				return err
			}
			index += uint64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
			return readErr
		}
	}

	cmd, payload, err := channel.readCommand()
	if err != nil {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return err
	}
	if cmd != hdcFileFinish {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return fmt.Errorf("unexpected hdc file finish command %d", cmd)
	}
	if len(payload) > 0 && payload[0] != 0 {
		if err := channel.writeCommand(hdcFileFinish, []byte{0}); err != nil {
			_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
			return err
		}
	}
	if err := channel.writeCommand(hdcKernelChannelClose, []byte{1}); err != nil {
		return err
	}
	return nil
}

func (b *Backend) makeRemoteDir(ctx context.Context, serial string, remote string) error {
	_, err := b.runCommand(ctx, serial, []byte("shell mkdir -p "+shellQuote(remote)))
	return err
}

func (b *Backend) handleRecv(ctx context.Context, serial string, conn net.Conn, remote string) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		writeSyncFail(conn, "empty RECV path")
		return
	}

	channel, err := b.openChannel(ctx, serial, []byte("shell "+pullFileCommand(remote)))
	if err != nil {
		writeSyncFail(conn, err.Error())
		return
	}
	defer channel.Close()

	marker := make([]byte, len(pullOKMarker))
	_, err = io.ReadFull(channel, marker)
	if err != nil {
		writeSyncFail(conn, err.Error())
		return
	}
	if string(marker) == pullFailMarker {
		message, _ := io.ReadAll(channel)
		writeSyncFail(conn, strings.TrimSpace(string(message)))
		return
	}
	if string(marker) != pullOKMarker {
		writeSyncFail(conn, "unexpected hdc pull response")
		return
	}

	buf := make([]byte, syncChunkSize)
	for {
		n, err := channel.Read(buf)
		if n > 0 {
			if writeSyncData(conn, buf[:n]) != nil {
				return
			}
		}
		if err == io.EOF {
			writeSyncDone(conn)
			return
		}
		if err != nil {
			writeSyncFail(conn, err.Error())
			return
		}
	}
}

func pullFileCommand(remote string) string {
	quoted := shellQuote(remote)
	return "if [ -r " + quoted + " ] && [ -f " + quoted + " ]; then " +
		"printf '\\001OK\\002'; cat " + quoted + "; " +
		"else printf '\\001NO\\002'; printf " + shellQuote("remote open "+remote+": no such file or directory") + "; fi"
}

type remoteEntry struct {
	name  string
	mode  uint32
	size  uint64
	mtime uint32
}

func (b *Backend) remotePathInfo(ctx context.Context, serial string, remote string) (remoteEntry, error) {
	output, err := b.runCommand(ctx, serial, []byte("shell "+remoteStatCommand(remote)))
	if err != nil {
		return remoteEntry{}, err
	}
	line := strings.TrimSpace(string(output))
	if line == "" || strings.HasPrefix(line, "n\t") {
		return remoteEntry{}, os.ErrNotExist
	}
	entry, err := parseRemoteEntry(line)
	entry.name = path.Base(remote)
	return entry, err
}

func (b *Backend) remoteList(ctx context.Context, serial string, remote string) ([]remoteEntry, error) {
	output, err := b.runCommand(ctx, serial, []byte("shell "+remoteListCommand(remote)))
	if err != nil {
		return nil, err
	}
	var entries []remoteEntry
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		entry, err := parseRemoteEntry(line)
		if err != nil {
			continue
		}
		if entry.name == "." || entry.name == ".." || entry.name == "" {
			continue
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func parseRemoteEntry(line string) (remoteEntry, error) {
	parts := strings.SplitN(line, "\t", 4)
	if len(parts) < 3 {
		return remoteEntry{}, fmt.Errorf("invalid remote entry %q", line)
	}
	size, _ := strconv.ParseUint(parts[1], 10, 64)
	mtime64, _ := strconv.ParseUint(parts[2], 10, 32)
	entry := remoteEntry{size: size, mtime: uint32(mtime64)}
	switch parts[0] {
	case "d":
		entry.mode = dirMode
	case "f":
		entry.mode = fileMode
	default:
		entry.mode = 0
	}
	if len(parts) == 4 {
		entry.name = parts[3]
	}
	return entry, nil
}

func remoteStatCommand(remote string) string {
	quoted := shellQuote(remote)
	return "p=" + quoted + "; " +
		"if [ -d \"$p\" ]; then printf 'd\\t0\\t0\\n'; " +
		"elif [ -f \"$p\" ]; then s=$(wc -c < \"$p\" 2>/dev/null | tr -d ' '); printf 'f\\t%s\\t0\\n' \"$s\"; " +
		"else printf 'n\\t0\\t0\\n'; fi"
}

func remoteListCommand(remote string) string {
	quoted := shellQuote(remote)
	return "d=" + quoted + "; " +
		"for f in \"$d\"/* \"$d\"/.[!.]* \"$d\"/..?*; do " +
		"[ -e \"$f\" ] || continue; b=${f##*/}; " +
		"if [ -d \"$f\" ]; then printf 'd\\t0\\t0\\t%s\\n' \"$b\"; " +
		"elif [ -f \"$f\" ]; then s=$(wc -c < \"$f\" 2>/dev/null | tr -d ' '); printf 'f\\t%s\\t0\\t%s\\n' \"$s\" \"$b\"; " +
		"fi; done"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func hdcCommandQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n\r\"'") {
		return value
	}
	return "\"" + strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "\"", "\\\"") + "\""
}

func encodeTransferConfig(fileSize uint64, remote string, optionalName string, mtime uint64) []byte {
	var out []byte
	out = appendProtoVarintField(out, 1, fileSize)
	out = appendProtoVarintField(out, 2, mtime)
	out = appendProtoVarintField(out, 3, mtime)
	out = appendProtoBytesField(out, 4, nil)
	out = appendProtoBytesField(out, 5, []byte(remote))
	out = appendProtoBytesField(out, 6, []byte(optionalName))
	out = appendProtoVarintField(out, 7, 0)
	out = appendProtoVarintField(out, 8, 0)
	out = appendProtoVarintField(out, 9, 0)
	out = appendProtoBytesField(out, 10, nil)
	out = appendProtoBytesField(out, 11, nil)
	out = appendProtoBytesField(out, 12, nil)
	out = appendProtoBytesField(out, 13, nil)
	return out
}

func encodeTransferPayload(index uint64, data []byte) []byte {
	var head []byte
	head = appendProtoVarintField(head, 1, index)
	head = appendProtoVarintField(head, 2, 0)
	head = appendProtoVarintField(head, 3, uint64(len(data)))
	head = appendProtoVarintField(head, 4, uint64(len(data)))
	payload := make([]byte, hdcFileHeaderSize, hdcFileHeaderSize+len(data))
	copy(payload, head)
	payload = append(payload, data...)
	return payload
}

func appendProtoVarintField(out []byte, fieldNumber int, value uint64) []byte {
	out = appendProtoVarint(out, uint64(fieldNumber<<3))
	return appendProtoVarint(out, value)
}

func appendProtoBytesField(out []byte, fieldNumber int, value []byte) []byte {
	out = appendProtoVarint(out, uint64(fieldNumber<<3|2))
	out = appendProtoVarint(out, uint64(len(value)))
	return append(out, value...)
}

func appendProtoVarint(out []byte, value uint64) []byte {
	for value >= 0x80 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}

func readSyncRequest(r io.Reader) (string, []byte, error) {
	var header [8]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return "", nil, err
	}
	id := string(header[:4])
	length := binary.LittleEndian.Uint32(header[4:8])
	if id == "DONE" {
		return id, nil, nil
	}
	payload := make([]byte, int(length))
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return "", nil, err
		}
	}
	return id, payload, nil
}

func writeSyncData(w io.Writer, payload []byte) error {
	if err := writeSyncHeader(w, "DATA", uint32(len(payload))); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func writeSyncDone(w io.Writer) { _ = writeSyncHeader(w, "DONE", 0) }
func writeSyncOkay(w io.Writer) { _ = writeSyncHeader(w, "OKAY", 0) }

func writeSyncFail(w io.Writer, message string) {
	if len(message) > 0xffff {
		message = message[:0xffff]
	}
	if writeSyncHeader(w, "FAIL", uint32(len(message))) != nil {
		return
	}
	_, _ = io.WriteString(w, message)
}

func writeSyncHeader(w io.Writer, id string, length uint32) error {
	var header [8]byte
	copy(header[:4], id)
	binary.LittleEndian.PutUint32(header[4:8], length)
	_, err := w.Write(header[:])
	return err
}

func (b *Backend) runCommand(ctx context.Context, serial string, command []byte) ([]byte, error) {
	channel, err := b.openChannel(ctx, serial, command)
	if err != nil {
		return nil, err
	}
	defer channel.Close()

	var output []byte
	buf := make([]byte, syncChunkSize)
	for {
		n, err := channel.Read(buf)
		if n > 0 {
			output = append(output, buf[:n]...)
		}
		if err == io.EOF {
			return output, nil
		}
		if err != nil {
			return output, err
		}
	}
}

func (b *Backend) openChannel(ctx context.Context, serial string, command []byte) (*channelConn, error) {
	if b.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.Timeout)
		defer cancel()
	}
	conn, err := b.Dialer.DialContext(ctx, "tcp", b.addr())
	if err != nil {
		return nil, err
	}
	if b.Timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(b.Timeout))
	}

	handshake, err := readHDCFrame(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if len(handshake) < handshakeSize || string(handshake[:len(handshakeMessage)]) != handshakeMessage {
		_ = conn.Close()
		return nil, fmt.Errorf("invalid hdc server handshake")
	}
	if len(serial) > connectKeySize {
		_ = conn.Close()
		return nil, fmt.Errorf("hdc connect key too long")
	}
	response := append([]byte(nil), handshake[:handshakeSize]...)
	clear(response[connectKeyOffset : connectKeyOffset+connectKeySize])
	copy(response[connectKeyOffset:], serial)
	if err := writeHDCFrame(conn, response); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := writeHDCFrame(conn, command); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return &channelConn{conn: conn, timeout: b.Timeout}, nil
}

type channelConn struct {
	conn    net.Conn
	readMu  sync.Mutex
	writeMu sync.Mutex
	buf     []byte
	timeout time.Duration
}

func (c *channelConn) touchDeadline() {
	if c.timeout > 0 {
		_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	}
}

func (c *channelConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(c.buf) == 0 {
		c.touchDeadline()
		frame, err := readHDCFrame(c.conn)
		if err != nil {
			return 0, err
		}
		c.buf = frame
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *channelConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.touchDeadline()
	if err := writeHDCFrame(c.conn, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *channelConn) Close() error { return c.conn.Close() }

func (c *channelConn) readCommand() (uint16, []byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	c.touchDeadline()
	frame, err := readHDCFrame(c.conn)
	if err != nil {
		return 0, nil, err
	}
	if len(frame) < 2 {
		return 0, nil, fmt.Errorf("short hdc command frame")
	}
	return binary.LittleEndian.Uint16(frame[:2]), frame[2:], nil
}

func (c *channelConn) writeCommand(command uint16, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.touchDeadline()
	frame := make([]byte, 2, 2+len(payload))
	binary.LittleEndian.PutUint16(frame[:2], command)
	frame = append(frame, payload...)
	return writeHDCFrame(c.conn, frame)
}

func readHDCFrame(r io.Reader) ([]byte, error) {
	var lengthBuf [4]byte
	if _, err := io.ReadFull(r, lengthBuf[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(lengthBuf[:])
	payload := make([]byte, int(length))
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}
	return payload, nil
}

func writeHDCFrame(w io.Writer, payload []byte) error {
	var lengthBuf [4]byte
	binary.BigEndian.PutUint32(lengthBuf[:], uint32(len(payload)))
	if _, err := w.Write(lengthBuf[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

func parseShellService(service string) (shellSpec, bool) {
	if command, ok := strings.CutPrefix(service, "exec:"); ok {
		return shellSpec{command: command}, true
	}
	if !strings.HasPrefix(service, "shell") {
		return shellSpec{}, false
	}
	prefix, command, ok := strings.Cut(service, ":")
	if !ok {
		return shellSpec{}, false
	}
	if prefix != "shell" && !strings.HasPrefix(prefix, "shell,") {
		return shellSpec{}, false
	}
	return shellSpec{command: command, v2: shellServiceHasOption(prefix, "v2")}, true
}

func shellServiceHasOption(prefix string, option string) bool {
	for _, part := range strings.Split(prefix, ",") {
		if part == option {
			return true
		}
	}
	return false
}

type shellSpec struct {
	command string
	v2      bool
}

func parseGetprop(output []byte) map[string]string {
	properties := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		key, value, ok := parseGetpropLine(scanner.Text())
		if ok {
			properties[key] = value
		}
	}
	return properties
}

func parseTargetListProperties(output []byte, serial string) (map[string]string, bool) {
	var first map[string]string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] == "[Empty]" {
			continue
		}
		if len(fields) < 4 {
			continue
		}
		props := fallbackProperties(fields[0], fields[3])
		if first == nil {
			first = props
		}
		if fields[0] == serial {
			return props, true
		}
	}
	if serial == "any" && first != nil {
		return first, true
	}
	return nil, false
}

func fallbackProperties(target string, devName string) map[string]string {
	model := devName
	if isGenericDevName(model) {
		model = "OpenHarmony"
	}
	return map[string]string{
		"ro.product.name":   "openharmony",
		"ro.product.model":  model,
		"ro.product.device": "openharmony",
	}
}

func isGenericDevName(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "" || value == "localhost" || value == "unknown" || value == "unknown..."
}

func cleanParamValue(output []byte) string {
	value := strings.Trim(strings.TrimSpace(string(output)), "\x00")
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	if strings.Contains(lower, "get parameter") && strings.Contains(lower, "fail") {
		return ""
	}
	if strings.HasPrefix(lower, "fail") || strings.HasPrefix(lower, "error") {
		return ""
	}
	return value
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

func uint32LE(value uint32) []byte {
	var out [4]byte
	binary.LittleEndian.PutUint32(out[:], value)
	return out[:]
}

func (b *Backend) addr() string {
	if b.Addr == "" {
		return DefaultAddr
	}
	return b.Addr
}
