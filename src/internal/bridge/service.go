package bridge

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"adb-tcp-bridge/src/internal/adbwire"
)

type service struct {
	session  *session
	localID  uint32
	remoteID atomic.Uint32
	name     string

	connMu sync.Mutex
	conn   net.Conn

	closeOnce sync.Once
	done      chan struct{}
	ackCh     chan struct{}
	opened    bool
}

func newService(session *session, localID uint32, remoteID uint32, name string) *service {
	s := &service{
		session: session,
		localID: localID,
		name:    name,
		done:    make(chan struct{}),
		ackCh:   make(chan struct{}, 1),
	}
	s.remoteID.Store(remoteID)
	return s
}

func (s *service) run(ctx context.Context) {
	defer s.finish()

	conn, err := s.session.config.Backend.OpenService(ctx, s.session.config.Serial, s.name)
	if err != nil {
		s.session.config.Logger.Error().
			Err(err).
			Str("service", s.name).
			Uint32("local_id", s.localID).
			Uint32("remote_id", s.remoteID.Load()).
			Msg("open adb service failed")
		return
	}
	s.setConn(conn)
	s.opened = true

	if err := s.session.writePacket(adbwire.Packet{
		Command: adbwire.CmdOkay,
		Arg0:    s.localID,
		Arg1:    s.remoteID.Load(),
	}); err != nil {
		return
	}

	s.pumpConnToClient(conn, "read adb service failed")
}

func (s *service) runOutbound(conn net.Conn) {
	defer s.finish()

	s.setConn(conn)
	if err := s.sendOpenAndWaitAck(); err != nil {
		s.session.config.Logger.Error().
			Err(err).
			Str("service", s.name).
			Uint32("local_id", s.localID).
			Msg("open reverse target failed")
		return
	}

	s.pumpConnToClient(conn, "read reverse connection failed")
}

// pumpConnToClient 把 conn 上读到的数据以 WRTE 包转发给 ADB client，
// 直到读出错或连接关闭。sendWriteAndWaitAck 会阻塞到对端 ACK，
// 因此 buffer 在每次写完成前不会被复用，可直接传切片无需拷贝。
func (s *service) pumpConnToClient(conn net.Conn, failMsg string) {
	buffer := make([]byte, int(s.session.maxPayload))
	for {
		n, err := conn.Read(buffer)
		if n > 0 {
			if err := s.sendWriteAndWaitAck(buffer[:n]); err != nil {
				return
			}
		}
		if err != nil {
			if s.shouldLogReadError(err) {
				s.session.config.Logger.Error().
					Err(err).
					Str("service", s.name).
					Uint32("local_id", s.localID).
					Uint32("remote_id", s.remoteID.Load()).
					Msg(failMsg)
			}
			return
		}
	}
}

func (s *service) sendOpenAndWaitAck() error {
	payload := append([]byte(s.name), 0)
	if err := s.session.writePacket(adbwire.Packet{
		Command: adbwire.CmdOpen,
		Arg0:    s.localID,
		Payload: payload,
	}); err != nil {
		return err
	}

	select {
	case <-s.ackCh:
		return nil
	case <-s.done:
		return io.ErrClosedPipe
	}
}

// sendPayloadAndClose 用于本地合成响应（如 reverse: 控制命令）：直接回一个
// OKAY，把预先算好的 payload 以单个 WRTE 发回 client，随后关闭。它复用了流式
// service 的 ack/流控/CLSE 簿记，但不读取 conn、不开 transport。
func (s *service) sendPayloadAndClose(payload []byte) {
	defer s.finish()

	s.opened = true
	if err := s.session.writePacket(adbwire.Packet{
		Command: adbwire.CmdOkay,
		Arg0:    s.localID,
		Arg1:    s.remoteID.Load(),
	}); err != nil {
		return
	}
	_ = s.sendWriteAndWaitAck(payload)
}

func (s *service) write(payload []byte) error {
	s.connMu.Lock()
	conn := s.conn
	s.connMu.Unlock()
	if conn == nil {
		return io.ErrClosedPipe
	}
	_, err := conn.Write(payload)
	return err
}

// shouldLogReadError 判断读失败是否值得记 Error。
// EOF、本端关闭、对端 RST/EPIPE 属于流正常结束或调试器断开，不刷错误日志。
func (s *service) shouldLogReadError(err error) bool {
	if err == nil || isBenignConnClose(err) {
		return false
	}

	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func isBenignConnClose(err error) bool {
	if err == nil || err == io.EOF || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	if isConnectionReset(err) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	// 兜底：部分平台把 RST 包成字符串错误。
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "forcibly closed") ||
		strings.Contains(msg, "broken pipe")
}

func (s *service) ack(remoteID uint32) {
	s.remoteID.CompareAndSwap(0, remoteID)
	s.opened = true

	select {
	case s.ackCh <- struct{}{}:
	default:
	}
}

func (s *service) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.connMu.Lock()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.connMu.Unlock()
	})
}

func (s *service) finish() {
	s.close()
	s.session.removeService(s.localID)

	localID := s.localID
	remoteID := s.remoteID.Load()
	if !s.opened && remoteID == 0 {
		return
	}
	if !s.opened {
		localID = 0
	}
	_ = s.session.writePacket(adbwire.Packet{
		Command: adbwire.CmdClse,
		Arg0:    localID,
		Arg1:    remoteID,
	})
}

func (s *service) setConn(conn net.Conn) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.conn = conn
}

func (s *service) sendWriteAndWaitAck(payload []byte) error {
	remoteID := s.remoteID.Load()
	if remoteID == 0 {
		return io.ErrClosedPipe
	}
	if err := s.session.writePacket(adbwire.Packet{
		Command: adbwire.CmdWrte,
		Arg0:    s.localID,
		Arg1:    remoteID,
		Payload: payload,
	}); err != nil {
		return err
	}

	select {
	case <-s.ackCh:
		return nil
	case <-s.done:
		return io.ErrClosedPipe
	}
}
