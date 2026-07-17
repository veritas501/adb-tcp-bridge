package hdcserver

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
)

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

func uint32LE(value uint32) []byte {
	var out [4]byte
	binary.LittleEndian.PutUint32(out[:], value)
	return out[:]
}
