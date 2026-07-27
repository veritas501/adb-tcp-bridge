package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"

	"adb-tcp-bridge/src/internal/bridge"
	"adb-tcp-bridge/src/internal/control"
	"adb-tcp-bridge/src/internal/hdcserver"
)

const (
	defaultADBServer = "127.0.0.1:5037"
	defaultBackend   = "adb"
)

// handleConn reads one NDJSON request, writes one NDJSON response, then closes.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	// Allow slightly larger control payloads than default 64KiB token size.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		_ = writeResponse(conn, control.Response{OK: false, Error: "empty request"})
		return
	}
	if err := scanner.Err(); err != nil {
		_ = writeResponse(conn, control.Response{OK: false, Error: err.Error()})
		return
	}

	var req control.Request
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		_ = writeResponse(conn, control.Response{OK: false, Error: fmt.Sprintf("invalid request json: %v", err)})
		return
	}

	resp := s.dispatch(req)
	_ = writeResponse(conn, resp)

	if req.Op == control.OpShutdown && resp.OK {
		if s.cancel != nil {
			s.cancel()
		}
	}
}

func (s *Server) dispatch(req control.Request) control.Response {
	switch req.Op {
	case control.OpVersion:
		return control.Response{OK: true, Version: control.ProtocolVersion, LogPath: s.logPath}

	case control.OpStart:
		return s.handleStart(req)

	case control.OpStop:
		if req.Serial == "" {
			return control.Response{OK: false, Error: "serial is required"}
		}
		if err := s.manager.Stop(req.Serial); err != nil {
			return control.Response{OK: false, Error: err.Error()}
		}
		return control.Response{OK: true, LogPath: s.logPath}

	case control.OpList:
		return control.Response{OK: true, Bridges: s.manager.List(), LogPath: s.logPath}

	case control.OpStatus:
		if req.Serial == "" {
			return control.Response{OK: true, Bridges: s.manager.List(), LogPath: s.logPath}
		}
		info, err := s.manager.Status(req.Serial)
		if err != nil {
			return control.Response{OK: false, Error: err.Error(), LogPath: s.logPath}
		}
		return control.Response{OK: true, Bridge: &info, LogPath: s.logPath}

	case control.OpShutdown:
		return control.Response{OK: true, LogPath: s.logPath}

	default:
		return control.Response{OK: false, Error: "unknown op"}
	}
}

func (s *Server) handleStart(req control.Request) control.Response {
	if req.Serial == "" {
		return control.Response{OK: false, Error: "serial is required"}
	}

	host := req.Host
	if host == "" {
		host = bridge.DefaultListenHost
	}
	port := req.Port
	if port == 0 {
		port = bridge.DefaultListenStartPort
	}
	backendName := req.Backend
	if backendName == "" {
		backendName = defaultBackend
	}
	adbServer := req.Server
	if adbServer == "" {
		adbServer = defaultADBServer
	}
	hdcAddr := req.HDCServer
	if hdcAddr == "" {
		hdcAddr = hdcserver.DefaultAddr
	}
	auth := req.Auth
	if auth == "" {
		auth = string(bridge.AuthAcceptAll)
	}

	deviceBackend, err := NewDeviceBackend(backendName, adbServer, hdcAddr)
	if err != nil {
		return control.Response{OK: false, Error: err.Error()}
	}

	info, err := s.manager.Start(s.rootCtx, StartConfig{
		Serial:          req.Serial,
		ListenHost:      host,
		ListenStartPort: port,
		Backend:         deviceBackend,
		BackendName:     backendName,
		AuthMode:        bridge.AuthMode(auth),
	})
	if err != nil {
		return control.Response{OK: false, Error: err.Error()}
	}
	return control.Response{OK: true, Bridge: &info, LogPath: s.logPath}
}

func writeResponse(conn net.Conn, resp control.Response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = conn.Write(append(data, '\n'))
	return err
}
