//go:build windows

package client

import "os/exec"

func configureDaemonCmd(cmd *exec.Cmd) {
	// Windows has no Setsid; process is still detached by not waiting and
	// redirecting stdio. CreationFlags could be added later if needed.
}
