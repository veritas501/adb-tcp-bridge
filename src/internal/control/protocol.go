package control

const (
	OpStart    = "start"
	OpStop     = "stop"
	OpList     = "list"
	OpStatus   = "status"
	OpShutdown = "shutdown"
	OpVersion  = "version"

	ProtocolVersion = 1
)

// Request is one NDJSON control request from CLI to daemon.
type Request struct {
	Op        string `json:"op"`
	Serial    string `json:"serial,omitempty"`
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	Backend   string `json:"backend,omitempty"` // "adb" | "hdc"
	Server    string `json:"server,omitempty"`  // adb server addr
	HDCServer string `json:"hdc_server,omitempty"`
	Auth      string `json:"auth,omitempty"` // "accept-all" | "none"
}

// BridgeInfo describes one managed bridge instance.
type BridgeInfo struct {
	Serial     string `json:"serial"`
	Backend    string `json:"backend"`
	Auth       string `json:"auth"`
	ListenAddr string `json:"listen_addr"`
	State      string `json:"state"` // "running" | "error"
	Error      string `json:"error,omitempty"`
}

// Response is one NDJSON control response from daemon to CLI.
type Response struct {
	OK      bool         `json:"ok"`
	Error   string       `json:"error,omitempty"`
	Version int          `json:"version,omitempty"`
	Bridges []BridgeInfo `json:"bridges,omitempty"`
	Bridge  *BridgeInfo  `json:"bridge,omitempty"`
	LogPath string       `json:"log_path,omitempty"`
}
