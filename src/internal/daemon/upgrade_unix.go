//go:build unix

package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"adb-tcp-bridge/src/internal/control"
	"github.com/rs/zerolog"
)

const (
	// restartReadyWait 是旧 daemon 等待新 daemon 恢复全部 listener 的超时。
	restartReadyWait = 15 * time.Second

	// readyOK 是新 daemon 恢复完成后写入 ready pipe 的字节。
	readyOK byte = 1
)

// restartSupported 报告当前平台是否支持 fd 传递式优雅重启。
func restartSupported() bool {
	return true
}

// handoff 把控制面与全部 bridge 的 listener fd 移交给新 daemon 进程，
// 新 daemon 恢复完成并通知后本进程退出。任一步失败则恢复 accept，
// daemon 继续运行（handoff 由独立 goroutine 执行，失败不影响控制面）。
func (s *Server) handoff(req control.Request) {
	execPath := req.Binary
	if execPath == "" {
		var err error
		execPath, err = os.Executable()
		if err != nil {
			s.failHandoff(fmt.Errorf("resolve executable for restart: %w", err))
			return
		}
	}

	snap := s.manager.Snapshot()

	// 提取 listener 的 dup fd：首位是控制面 UDS，其后按快照顺序是各 bridge
	// 的 TCP listener，末位是 ready pipe 写端。--inherit 的 fd 列表与
	// exec.Cmd.ExtraFiles 顺序一一对应（ExtraFiles 从 fd 3 开始编号）。
	files := make([]*os.File, 0, 2+len(snap))
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	if s.udsListener == nil {
		s.failHandoff(fmt.Errorf("control listener unavailable"))
		return
	}
	// 关键：net.Listen("unix") 创建的 listener 关闭时会 unlink socket 文件，
	// 而该文件在移交后由新 daemon 继续使用。必须显式关闭 unlink 行为，
	// 否则旧 daemon 退出（listener.Close）会删掉新 daemon 的 socket 路径。
	if ul, ok := s.udsListener.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	udsFile, err := listenerFile(s.udsListener)
	if err != nil {
		s.failHandoff(fmt.Errorf("dup control socket fd: %w", err))
		return
	}
	files = append(files, udsFile)
	for _, item := range snap {
		f, err := listenerFile(item.Listener)
		if err != nil {
			s.failHandoff(fmt.Errorf("dup listener fd for %q: %w", item.Restore.Serial, err))
			return
		}
		files = append(files, f)
	}

	readyR, readyW, err := os.Pipe()
	if err != nil {
		s.failHandoff(fmt.Errorf("create ready pipe: %w", err))
		return
	}
	defer readyR.Close()
	files = append(files, readyW)

	state, err := encodeRestoreState(restoreStates(snap))
	if err != nil {
		s.failHandoff(fmt.Errorf("encode restore state: %w", err))
		return
	}

	fdSpec := make([]string, len(files))
	for i := range files {
		fdSpec[i] = strconv.Itoa(3 + i)
	}

	args := []string{
		"daemon",
		"--socket", s.socketPath,
		"--log-file", s.logPath,
		"--log-level", s.logger.GetLevel().String(),
		"--inherit", strings.Join(fdSpec, ","),
	}
	cmd := exec.Command(execPath, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.ExtraFiles = files
	// 过滤环境里可能残留的旧 ATB_RESTORE（本进程可能本身是 --inherit 启动，
	// 环境里已有上一次 restart 的 state）：getenv 返回第一个匹配，若不删除
	// 旧键，子进程会读到过期配置，导致恢复错乱或长度校验失败。
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, RestoreStateEnv+"=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, RestoreStateEnv+"="+state)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	s.logger.Info().
		Str("binary", execPath).
		Str("inherit", strings.Join(fdSpec, ",")).
		Int("bridges", len(snap)).
		Msg("handing off listeners to new daemon")
	if err := cmd.Start(); err != nil {
		s.failHandoff(fmt.Errorf("start new daemon: %w", err))
		return
	}
	// 父进程不再需要 ready pipe 写端：立即关闭，子进程崩溃时读端能及时
	// 收到 EOF，而不是拖满整个等待超时。
	_ = readyW.Close()
	files[len(files)-1] = nil // 防 defer 重复 close

	// 等待新 daemon 恢复全部 listener。读到 EOF 或超时表示新进程未就绪：
	// 回收子进程并恢复本进程服务。
	if err := readyR.SetReadDeadline(time.Now().Add(restartReadyWait)); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		s.failHandoff(fmt.Errorf("set ready deadline: %w", err))
		return
	}
	buf := make([]byte, 1)
	n, err := readyR.Read(buf)
	if err != nil || n != 1 || buf[0] != readyOK {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		s.failHandoff(fmt.Errorf("new daemon not ready (read %d bytes, err %v)", n, err))
		return
	}

	// 移交完成：退出清理跳过 socket 删除与 bridge 停止（新 daemon 已持有）。
	s.handedOff.Store(true)
	s.logger.Info().Msg("new daemon ready, exiting")
	s.cancel()
}

// listenerFile 返回 listener 的可继承 fd 副本（dup）。
// 注意：UnixListener 的 unlink-on-close 行为需调用方按需显式设置
// （handoff 时关闭 unlink，防止旧进程退出删除新 daemon 的 socket 路径）。
func listenerFile(l net.Listener) (*os.File, error) {
	switch t := l.(type) {
	case *net.TCPListener:
		return t.File()
	case *net.UnixListener:
		return t.File()
	default:
		return nil, fmt.Errorf("unsupported listener type %T", l)
	}
}

func restoreStates(snap []SnapshotItem) []BridgeRestore {
	out := make([]BridgeRestore, len(snap))
	for i, item := range snap {
		out[i] = item.Restore
	}
	return out
}

// ParseInheritFds 解析 --inherit 的逗号分隔 fd 列表（ExtraFiles 起始 fd 3）。
// 约定：首个 fd 是控制面 UDS，末位是 ready pipe 写端，中间是各 bridge 的
// TCP listener，顺序与恢复配置一一对应。
func ParseInheritFds(spec string) ([]int, error) {
	parts := strings.Split(spec, ",")
	if len(parts) < 2 {
		return nil, fmt.Errorf("inherit spec needs control fd and ready fd, got %q", spec)
	}
	fds := make([]int, 0, len(parts))
	for _, part := range parts {
		fd, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || fd < 3 {
			return nil, fmt.Errorf("invalid inherit fd %q", part)
		}
		fds = append(fds, fd)
	}
	return fds, nil
}

// RestoreInherited 在新 daemon 进程内恢复旧 daemon 移交的全部 listener：
// 逐个 Adopt bridge（顺序与恢复配置一一对应），最后接管控制面 UDS 并通知
// 旧 daemon 就绪。任一 bridge 恢复失败则整体失败：新进程退出，旧 daemon
// 感知后恢复服务，不产生半接管状态。
func RestoreInherited(parent context.Context, manager *Manager, fds []int, state []BridgeRestore, logger *zerolog.Logger) (net.Listener, error) {
	ready := os.NewFile(uintptr(fds[len(fds)-1]), "atb-ready")
	defer ready.Close()

	tcpFds := fds[1 : len(fds)-1]
	if len(tcpFds) != len(state) {
		return nil, fmt.Errorf("inherited %d tcp fds but %d bridge states", len(tcpFds), len(state))
	}
	for i, fd := range tcpFds {
		f := os.NewFile(uintptr(fd), "atb-tcp")
		l, err := net.FileListener(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("restore tcp listener %d: %w", i, err)
		}
		if _, err := manager.Adopt(parent, state[i], l); err != nil {
			_ = l.Close()
			return nil, fmt.Errorf("adopt bridge %q: %w", state[i].Serial, err)
		}
		logger.Info().Str("serial", state[i].Serial).Msg("restored bridge")
	}

	f := os.NewFile(uintptr(fds[0]), "atb-uds")
	udsListener, err := net.FileListener(f)
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("restore control socket: %w", err)
	}

	// 通知旧 daemon 移交完成。写失败（旧 daemon 已退出）不影响本进程接管。
	if _, err := ready.Write([]byte{readyOK}); err != nil {
		logger.Warn().Err(err).Msg("notify old daemon of takeover failed")
	}
	return udsListener, nil
}
