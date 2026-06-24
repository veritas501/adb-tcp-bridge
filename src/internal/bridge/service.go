package bridge

import (
	"context"
	"io"
	"net"
	"sync"

	"adb-tcp-bridge/src/internal/adbwire"
)

type service struct {
	session  *session
	localID  uint32
	remoteID uint32
	name     string

	connMu sync.Mutex
	conn   net.Conn

	closeOnce sync.Once
	done      chan struct{}
	ackCh     chan struct{}
	opened    bool
}

func newService(session *session, localID uint32, remoteID uint32, name string) *service {
	return &service{
		session:  session,
		localID:  localID,
		remoteID: remoteID,
		name:     name,
		done:     make(chan struct{}),
		ackCh:    make(chan struct{}, 1),
	}
}

func (s *service) run(ctx context.Context) {
	defer s.finish()

	conn, err := s.session.config.Host.OpenService(ctx, s.session.config.Serial, s.name)
	if err != nil {
		s.session.config.Logger.Error().
			Err(err).
			Str("service", s.name).
			Uint32("local_id", s.localID).
			Uint32("remote_id", s.remoteID).
			Msg("open adb service failed")
		return
	}
	s.setConn(conn)
	s.opened = true

	if err := s.session.writePacket(adbwire.Packet{
		Command: adbwire.CmdOkay,
		Arg0:    s.localID,
		Arg1:    s.remoteID,
	}); err != nil {
		return
	}

	buffer := make([]byte, int(s.session.maxPayload))
	for {
		n, err := conn.Read(buffer)
		if n > 0 {
			payload := append([]byte(nil), buffer[:n]...)
			if err := s.sendWriteAndWaitAck(payload); err != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				s.session.config.Logger.Error().
					Err(err).
					Str("service", s.name).
					Uint32("local_id", s.localID).
					Uint32("remote_id", s.remoteID).
					Msg("read adb service failed")
			}
			return
		}
	}
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

func (s *service) ack() {
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
	if !s.opened {
		localID = 0
	}
	_ = s.session.writePacket(adbwire.Packet{
		Command: adbwire.CmdClse,
		Arg0:    localID,
		Arg1:    s.remoteID,
	})
}

func (s *service) setConn(conn net.Conn) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.conn = conn
}

func (s *service) sendWriteAndWaitAck(payload []byte) error {
	if err := s.session.writePacket(adbwire.Packet{
		Command: adbwire.CmdWrte,
		Arg0:    s.localID,
		Arg1:    s.remoteID,
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
