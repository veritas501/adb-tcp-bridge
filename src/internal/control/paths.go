package control

import (
	"fmt"
	"os"
	"path/filepath"
)

// SocketPath resolves the daemon control socket path.
// Order: explicit path, ADBB_SOCKET, $XDG_RUNTIME_DIR/adbb/adbb.sock, $HOME/.adbb/adbb.sock.
func SocketPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("ADBB_SOCKET"); env != "" {
		return env, nil
	}
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "adbb", "adbb.sock"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for default socket path: %w", err)
	}
	return filepath.Join(home, ".adbb", "adbb.sock"), nil
}

// LogPath resolves the daemon log file path.
// Order: explicitLog, ADBB_LOG, same directory as SocketPath(explicitSocket)/adbb.log.
func LogPath(explicitLog string, explicitSocket string) (string, error) {
	if explicitLog != "" {
		return explicitLog, nil
	}
	if env := os.Getenv("ADBB_LOG"); env != "" {
		return env, nil
	}
	socketPath, err := SocketPath(explicitSocket)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(socketPath), "adbb.log"), nil
}

// EnsureDir creates the parent directory of path with mode 0700.
func EnsureDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	return nil
}
