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
			return nil, err
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
	size, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return remoteEntry{}, fmt.Errorf("invalid remote entry size %q in %q", parts[1], line)
	}
	mtime64, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return remoteEntry{}, fmt.Errorf("invalid remote entry mtime %q in %q", parts[2], line)
	}
	entry := remoteEntry{size: size, mtime: uint32(mtime64)}
	switch parts[0] {
	case "d":
		entry.mode = dirMode
	case "f":
		entry.mode = fileMode
	default:
		return remoteEntry{}, fmt.Errorf("invalid remote entry type %q in %q", parts[0], line)
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
	if length > maxSyncPayloadSize {
		return "", nil, fmt.Errorf("sync payload too large: %d", length)
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
