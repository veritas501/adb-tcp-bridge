package bridge

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"

	"adb-tcp-bridge/src/internal/adbwire"
)

const (
	defaultVersion    uint32 = 0x01000000
	defaultMaxPayload uint32 = 4096
	maxWritePayload   uint32 = 64 * 1024
	authTokenSize            = 20
)

type session struct {
	config Config
	conn   net.Conn

	writeMu sync.Mutex
	closeMu sync.Mutex
	closed  bool

	serviceMu   sync.Mutex
	nextLocalID uint32
	services    map[uint32]*service

	authorized bool
	version    uint32
	maxPayload uint32
	authToken  []byte
}

func newSession(config Config, conn net.Conn) *session {
	config.Logger = normalizeLogger(config.Logger)
	return &session{
		config:      config,
		conn:        conn,
		nextLocalID: 1,
		services:    make(map[uint32]*service),
		version:     defaultVersion,
		maxPayload:  defaultMaxPayload,
	}
}

func (s *session) run(ctx context.Context) {
	done := make(chan struct{})
	defer close(done)
	defer s.close()

	go func() {
		select {
		case <-ctx.Done():
			s.close()
		case <-done:
		}
	}()

	for {
		packet, err := adbwire.ReadPacket(s.conn)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, io.EOF) {
				s.config.Logger.Error().
					Err(err).
					Str("remote_addr", s.remoteAddr()).
					Str("remote_host", splitRemoteHost(s.remoteAddr())).
					Msg("session read failed")
			}
			return
		}
		if err := s.handlePacket(ctx, packet); err != nil {
			s.config.Logger.Error().
				Err(err).
				Str("remote_addr", s.remoteAddr()).
				Str("remote_host", splitRemoteHost(s.remoteAddr())).
				Msg("session error")
			return
		}
	}
}

func (s *session) remoteAddr() string {
	if s.conn == nil || s.conn.RemoteAddr() == nil {
		return ""
	}
	return s.conn.RemoteAddr().String()
}

func (s *session) handlePacket(ctx context.Context, packet adbwire.Packet) error {
	switch packet.Command {
	case adbwire.CmdSync:
		return s.writePacket(adbwire.Packet{Command: adbwire.CmdSync, Arg0: 1, Arg1: packet.Arg1})
	case adbwire.CmdCnxn:
		s.version = packet.Arg0
		if packet.Arg1 > 0 {
			s.maxPayload = packet.Arg1
			if s.maxPayload > maxWritePayload {
				s.maxPayload = maxWritePayload
			}
		}
		if s.config.AuthMode == AuthNone {
			s.authorized = true
			return s.sendConnection()
		}
		return s.sendAuthChallenge()
	case adbwire.CmdAuth:
		return s.handleAuth(packet)
	case adbwire.CmdStls:
		return errors.New("ADB TLS transport is unsupported")
	case adbwire.CmdOpen:
		return s.handleOpen(ctx, packet)
	case adbwire.CmdOkay:
		return s.handleOkay(packet)
	case adbwire.CmdWrte:
		return s.handleWrite(packet)
	case adbwire.CmdClse:
		return s.handleClose(packet)
	default:
		return errors.New("unknown adb packet command")
	}
}

func (s *session) sendAuthChallenge() error {
	token := make([]byte, authTokenSize)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	s.authToken = token
	return s.writePacket(adbwire.Packet{
		Command: adbwire.CmdAuth,
		Arg0:    adbwire.AuthToken,
		Payload: token,
	})
}

func (s *session) handleAuth(packet adbwire.Packet) error {
	if s.config.AuthMode == AuthNone {
		return nil
	}

	switch packet.Arg0 {
	case adbwire.AuthSignature:
		// accept-all 模式不做 RSA 验签；任意签名都视为通过，避免 adb
		// client 在签名阶段打印 misleading authentication failure。
		s.authorized = true
		return s.sendConnection()
	case adbwire.AuthRSAPublicKey:
		s.authorized = true
		return s.sendConnection()
	default:
		return errors.New("unsupported adb auth method")
	}
}

func (s *session) sendConnection() error {
	return s.writePacket(adbwire.Packet{
		Command: adbwire.CmdCnxn,
		Arg0:    s.version,
		Arg1:    s.maxPayload,
		Payload: append([]byte(s.config.DeviceID), 0),
	})
}

func (s *session) handleOpen(ctx context.Context, packet adbwire.Packet) error {
	if !s.authorized {
		return errors.New("unauthorized adb open")
	}
	if len(packet.Payload) == 0 || packet.Payload[len(packet.Payload)-1] != 0 {
		return errors.New("invalid adb service name")
	}

	name := string(packet.Payload[:len(packet.Payload)-1])
	localID := s.allocateLocalID()
	svc := newService(s, localID, packet.Arg0, name)
	s.putService(localID, svc)
	go svc.run(ctx)
	return nil
}

func (s *session) handleOkay(packet adbwire.Packet) error {
	svc := s.getService(packet.Arg1)
	if svc == nil {
		return nil
	}
	svc.ack()
	return nil
}

func (s *session) handleWrite(packet adbwire.Packet) error {
	svc := s.getService(packet.Arg1)
	if svc == nil {
		return nil
	}
	if err := svc.write(packet.Payload); err != nil {
		return err
	}
	return s.writePacket(adbwire.Packet{
		Command: adbwire.CmdOkay,
		Arg0:    svc.localID,
		Arg1:    svc.remoteID,
	})
}

func (s *session) handleClose(packet adbwire.Packet) error {
	svc := s.getService(packet.Arg1)
	if svc == nil {
		return nil
	}
	svc.close()
	return nil
}

func (s *session) allocateLocalID() uint32 {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()

	localID := s.nextLocalID
	s.nextLocalID++
	if s.nextLocalID == 0 {
		s.nextLocalID = 1
	}
	return localID
}

func (s *session) putService(localID uint32, svc *service) {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	s.services[localID] = svc
}

func (s *session) getService(localID uint32) *service {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	return s.services[localID]
}

func (s *session) removeService(localID uint32) {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	delete(s.services, localID)
}

func (s *session) writePacket(packet adbwire.Packet) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return io.ErrClosedPipe
	}
	return adbwire.WritePacket(s.conn, packet)
}

func (s *session) close() {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return
	}
	s.closed = true
	s.closeMu.Unlock()

	s.serviceMu.Lock()
	services := make([]*service, 0, len(s.services))
	for _, svc := range s.services {
		services = append(services, svc)
	}
	s.services = make(map[uint32]*service)
	s.serviceMu.Unlock()

	for _, svc := range services {
		svc.close()
	}
	_ = s.conn.Close()
}
