package hdcserver

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

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
	if length > maxHDCFrameSize {
		return nil, fmt.Errorf("hdc frame too large: %d", length)
	}
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
