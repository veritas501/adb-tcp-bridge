package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"adb-tcp-bridge/src/internal/bridge"
	"adb-tcp-bridge/src/internal/client"
	"adb-tcp-bridge/src/internal/control"
	"adb-tcp-bridge/src/internal/daemon"
	"adb-tcp-bridge/src/internal/hdcserver"
	"adb-tcp-bridge/src/internal/version"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

// errHelpShown is returned after bare "atb" prints full help so main exits 2
// without reprinting a short error line.
var errHelpShown = fmt.Errorf("help shown")

func main() {
	cmd := newRootCommand()
	cmd.SetArgs(normalizeSingleDashLongFlags(os.Args[1:], cmd))
	if err := cmd.Execute(); err != nil {
		if err == errHelpShown {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func newRootCommand() *cobra.Command {
	var (
		socketFlag  string
		logFileFlag string
		logLevel    = zerolog.InfoLevel.String()
	)

	root := &cobra.Command{
		Use:   "atb",
		Short: "Expose ADB/HDC-connected devices as ADB-over-TCP",
		// Examples are stable product notes only. Commands and flags are listed by
		// cobra from AddCommand / flag registration so new subcommands show up
		// without editing a hand-maintained help string.
		Example: `  atb start <serial>
  atb start --host 0.0.0.0 --port 35555 <serial>
  atb start --backend hdc <hdc-target>
  atb <serial>
  atb list
  atb status
  atb logs -n 50
  atb daemon --log-level debug`,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Compatibility: atb <serial> == atb start <serial>
			if len(args) == 1 && !strings.HasPrefix(args[0], "-") {
				return runStart(cmd.Context(), socketFlag, logFileFlag, logLevel, startFlags{
					serial:     args[0],
					listenHost: bridge.DefaultListenHost,
					listenPort: bridge.DefaultListenStartPort,
					serverAddr: "127.0.0.1:5037",
					backend:    "adb",
					hdcAddr:    hdcserver.DefaultAddr,
					authMode:   string(bridge.AuthAcceptAll),
				})
			}
			if len(args) == 0 {
				_ = cmd.Help()
				return errHelpShown
			}
			_ = cmd.Help()
			return fmt.Errorf("unknown command %q", args[0])
		},
	}

	// Keep root help focused on product commands; completion remains available
	// via "atb completion" only if explicitly enabled later.
	root.CompletionOptions.DisableDefaultCmd = true

	root.PersistentFlags().StringVar(&socketFlag, "socket", "", "daemon control socket path (env ATB_SOCKET)")
	root.PersistentFlags().StringVar(&logFileFlag, "log-file", "", "daemon log file path (env ATB_LOG)")
	root.PersistentFlags().StringVar(&logLevel, "log-level", logLevel, "log level: debug, info, warn, error")

	root.AddCommand(
		newVersionCommand(),
		newDaemonCommand(&socketFlag, &logFileFlag, &logLevel),
		newStartCommand(&socketFlag, &logFileFlag, &logLevel),
		newStopCommand(&socketFlag, &logFileFlag, &logLevel),
		newListCommand(&socketFlag, &logFileFlag, &logLevel),
		newStatusCommand(&socketFlag, &logFileFlag, &logLevel),
		newRestartCommand(&socketFlag, &logFileFlag, &logLevel),
		newLogsCommand(&socketFlag, &logFileFlag),
		newKillServerCommand(&socketFlag, &logFileFlag, &logLevel),
	)

	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\ncommit: %s\nbuilt: %s\n", version.Module, version.Version, version.Commit, version.BuildDate)
		},
	}
}

func newDaemonCommand(socketFlag, logFileFlag, logLevel *string) *cobra.Command {
	var inherit string
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the multi-device bridge daemon in the foreground",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			level, err := zerolog.ParseLevel(*logLevel)
			if err != nil {
				return fmt.Errorf("invalid log level %q", *logLevel)
			}
			socketPath, err := control.SocketPath(*socketFlag)
			if err != nil {
				return err
			}
			logPath, err := control.LogPath(*logFileFlag, *socketFlag)
			if err != nil {
				return err
			}

			logger, closer, err := daemon.OpenLogger(logPath, level, true)
			if err != nil {
				return err
			}
			defer closer.Close()

			manager := daemon.NewManager(&logger)
			server := daemon.NewServer(socketPath, logPath, manager, &logger)

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// 优雅重启接管路径：--inherit 由旧 daemon exec 时传入，恢复其
			// 移交的 listener 与 bridges，就绪后正常进入 Run 的 accept 循环。
			if inherit != "" {
				fds, err := daemon.ParseInheritFds(inherit)
				if err != nil {
					return err
				}
				state, err := daemon.DecodeRestoreState(os.Getenv(daemon.RestoreStateEnv))
				if err != nil {
					return err
				}
				uds, err := daemon.RestoreInherited(ctx, manager, fds, state, &logger)
				if err != nil {
					return err
				}
				server.SetInheritedListener(uds)
			}
			return server.Run(ctx)
		},
	}
	// 仅供优雅重启内部使用，由旧 daemon 在 exec 新进程时构造。
	cmd.Flags().StringVar(&inherit, "inherit", "", "inherited listener fds: control,tcp...,ready (internal)")
	return cmd
}

type startFlags struct {
	serial     string
	listenHost string
	listenPort int
	serverAddr string
	backend    string
	hdcAddr    string
	authMode   string
}

func newStartCommand(socketFlag, logFileFlag, logLevel *string) *cobra.Command {
	f := startFlags{
		listenHost: bridge.DefaultListenHost,
		listenPort: bridge.DefaultListenStartPort,
		serverAddr: "127.0.0.1:5037",
		backend:    "adb",
		hdcAddr:    hdcserver.DefaultAddr,
		authMode:   string(bridge.AuthAcceptAll),
	}

	cmd := &cobra.Command{
		Use:   "start [flags] <serial|connect-key>",
		Short: "Start a bridge for one device (auto-starts daemon if needed)",
		Long: `Start a TCP bridge for one device and print the listen address.

The daemon is auto-started if it is not already running. This command is
short-lived: it prints one listen_addr line and exits while the daemon keeps
the bridge running.

Port selection: --port is the first port to try (default 35555). If occupied,
the bridge walks upward until a free port is found; always use the printed
address for adb connect.

For ADB backend, <serial> is from "adb devices". For HDC backend, use a target
from "hdc list targets" (or "any" when only one target exists).`,
		Example: `  atb start <serial>
  atb start --port 40000 <serial>
  atb start --backend hdc --hdc-server 127.0.0.1:8710 <hdc-target>
  adb connect <printed-listen-addr>`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f.serial = args[0]
			return runStart(cmd.Context(), *socketFlag, *logFileFlag, *logLevel, f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.listenHost, "host", f.listenHost, "TCP listen host")
	flags.IntVar(&f.listenPort, "port", f.listenPort, "first TCP listen port to try")
	flags.StringVar(&f.serverAddr, "server", f.serverAddr, "local adb server address")
	flags.StringVar(&f.backend, "backend", f.backend, "target backend: adb or hdc")
	flags.StringVar(&f.hdcAddr, "hdc-server", f.hdcAddr, "hdc server address when --backend hdc")
	flags.StringVar(&f.authMode, "auth", f.authMode, "auth mode: accept-all or none")
	return cmd
}

func runStart(ctx context.Context, socketFlag, logFileFlag, logLevel string, f startFlags) error {
	c, err := newClient(socketFlag, logFileFlag, logLevel)
	if err != nil {
		return err
	}
	c.AutoStart = true

	resp, err := c.Call(ctx, control.Request{
		Op:        control.OpStart,
		Serial:    f.serial,
		Host:      f.listenHost,
		Port:      f.listenPort,
		Backend:   f.backend,
		Server:    f.serverAddr,
		HDCServer: f.hdcAddr,
		Auth:      f.authMode,
	})
	if err != nil {
		return err
	}
	if resp.Bridge == nil || resp.Bridge.ListenAddr == "" {
		return fmt.Errorf("daemon returned empty listen address")
	}
	printStartResult(os.Stdout, *resp.Bridge)
	return nil
}

func newStopCommand(socketFlag, logFileFlag, logLevel *string) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <serial|connect-key>",
		Short: "Stop a running bridge for one device",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serial := args[0]
			c, err := newClient(*socketFlag, *logFileFlag, *logLevel)
			if err != nil {
				return err
			}
			c.AutoStart = false
			_, err = c.CallNoStart(cmd.Context(), control.Request{
				Op:     control.OpStop,
				Serial: serial,
			})
			if err != nil && client.IsNotRunning(err) {
				return fmt.Errorf("daemon not running")
			}
			if err != nil {
				return err
			}
			fmt.Printf("Stopped bridge for %s\n", serial)
			return nil
		},
	}
}

func newListCommand(socketFlag, logFileFlag, logLevel *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List running bridges",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*socketFlag, *logFileFlag, *logLevel)
			if err != nil {
				return err
			}
			c.AutoStart = true
			resp, err := c.Call(cmd.Context(), control.Request{Op: control.OpList})
			if err != nil {
				return err
			}
			printBridgeTable(os.Stdout, resp.Bridges)
			return nil
		},
	}
}

func newStatusCommand(socketFlag, logFileFlag, logLevel *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status [serial|connect-key]",
		Short: "Show daemon / bridge status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*socketFlag, *logFileFlag, *logLevel)
			if err != nil {
				return err
			}
			c.AutoStart = true

			req := control.Request{Op: control.OpStatus}
			if len(args) == 1 {
				req.Serial = args[0]
			}
			resp, err := c.Call(cmd.Context(), req)
			if err != nil {
				return err
			}

			if len(args) == 1 {
				if resp.Bridge == nil {
					return fmt.Errorf("no bridge for serial %q", args[0])
				}
				printBridgeDetail(os.Stdout, *resp.Bridge, resp.LogPath)
				return nil
			}

			fmt.Println("Daemon is running")
			if resp.LogPath != "" {
				fmt.Printf("  log: %s\n", resp.LogPath)
			}
			if len(resp.Bridges) == 0 {
				fmt.Println("  bridges: none")
				return nil
			}
			fmt.Println()
			printBridgeTable(os.Stdout, resp.Bridges)
			return nil
		},
	}
}

func printStartResult(w io.Writer, b control.BridgeInfo) {
	backend := b.Backend
	if backend == "" {
		backend = "unknown"
	}
	fmt.Fprintf(w, "Started bridge for %s (%s)\n", b.Serial, backend)
	fmt.Fprintf(w, "  listen:  %s\n", b.ListenAddr)
	fmt.Fprintf(w, "  connect: adb connect %s\n", connectTarget(b.ListenAddr))
}

func printBridgeDetail(w io.Writer, b control.BridgeInfo, logPath string) {
	fmt.Fprintf(w, "Serial:  %s\n", b.Serial)
	fmt.Fprintf(w, "Backend: %s\n", emptyDash(b.Backend))
	fmt.Fprintf(w, "Listen:  %s\n", emptyDash(b.ListenAddr))
	fmt.Fprintf(w, "State:   %s\n", emptyDash(b.State))
	if b.Auth != "" {
		fmt.Fprintf(w, "Auth:    %s\n", b.Auth)
	}
	if b.Error != "" {
		fmt.Fprintf(w, "Error:   %s\n", b.Error)
	}
	if logPath != "" {
		fmt.Fprintf(w, "Log:     %s\n", logPath)
	}
	if b.ListenAddr != "" && b.State == "running" {
		fmt.Fprintf(w, "Connect: adb connect %s\n", connectTarget(b.ListenAddr))
	}
}

func printBridgeTable(w io.Writer, bridges []control.BridgeInfo) {
	if len(bridges) == 0 {
		fmt.Fprintln(w, "No running bridges.")
		return
	}

	sorted := append([]control.BridgeInfo(nil), bridges...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Serial < sorted[j].Serial
	})

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERIAL\tBACKEND\tLISTEN\tSTATE")
	for _, b := range sorted {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			b.Serial,
			emptyDash(b.Backend),
			emptyDash(b.ListenAddr),
			emptyDash(b.State),
		)
	}
	_ = tw.Flush()
}

func connectTarget(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return listenAddr
	}
	// adb connect needs a concrete host; wildcard listeners are reached via localhost.
	switch host {
	case "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newLogsCommand(socketFlag, logFileFlag *string) *cobra.Command {
	var (
		tail   = 200
		follow bool
	)
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show daemon log file (local; does not auto-start daemon)",
		Long: `Read the daemon log file from disk (not over the control socket).

Default path is next to the control socket (atb.log), overridable with
--log-file or ATB_LOG. Does not start the daemon; if the file is missing,
reports an error. After kill-server the historical log file can still be read.`,
		Example: `  atb logs
  atb logs -n 50
  atb logs -n 0          # entire file
  atb logs -f            # follow new lines`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logPath, err := control.LogPath(*logFileFlag, *socketFlag)
			if err != nil {
				return err
			}
			if err := client.TailFile(logPath, tail, os.Stdout); err != nil {
				return err
			}
			if !follow {
				return nil
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return client.FollowFile(ctx, logPath, os.Stdout)
		},
	}
	cmd.Flags().IntVarP(&tail, "tail", "n", tail, "number of lines from the end of the log (0 = entire file)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow log file for new content")
	return cmd
}

func newRestartCommand(socketFlag, logFileFlag, logLevel *string) *cobra.Command {
	var binary string
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon without changing listen addresses",
		Long: `Restart the daemon in place: every bridge listen address stays the same
and running bridges are restored automatically by the new daemon process.

The old daemon hands its listeners to a new daemon process and exits once the
new daemon is ready; the control socket and all bridge ports keep their
addresses. Already-connected clients are dropped and must reconnect to the
same addresses.

To update the binary, replace it on disk first and run "atb restart": the new
daemon runs the replaced executable. Alternatively pass --binary to point at
a specific path (it does not replace the installed binary).`,
		Example: `  atb restart
  atb restart --binary /opt/atb/atb-new`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if binary != "" {
				if _, err := os.Stat(binary); err != nil {
					return fmt.Errorf("restart binary %q: %w", binary, err)
				}
			}
			c, err := newClient(*socketFlag, *logFileFlag, *logLevel)
			if err != nil {
				return err
			}
			c.AutoStart = false
			req := control.Request{Op: control.OpRestart, Binary: binary}
			if _, err := c.CallNoStart(cmd.Context(), req); err != nil {
				if client.IsNotRunning(err) {
					return fmt.Errorf("daemon is not running")
				}
				return err
			}
			// 旧 daemon 收到请求后立即停止 accept，因此下一次成功响应必然
			// 来自接管后的新 daemon。
			if err := c.WaitReady(cmd.Context(), 15*time.Second); err != nil {
				return fmt.Errorf("restart requested but new daemon did not come up: %w", err)
			}
			fmt.Println("Daemon restarted")
			return nil
		},
	}
	cmd.Flags().StringVar(&binary, "binary", "", "executable for the new daemon (default: current daemon binary)")
	return cmd
}

func newKillServerCommand(socketFlag, logFileFlag, logLevel *string) *cobra.Command {
	return &cobra.Command{
		Use:   "kill-server",
		Short: "Shut down the daemon (does not auto-start)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*socketFlag, *logFileFlag, *logLevel)
			if err != nil {
				return err
			}
			c.AutoStart = false
			_, err = c.CallNoStart(cmd.Context(), control.Request{Op: control.OpShutdown})
			if err != nil {
				if client.IsNotRunning(err) {
					fmt.Println("Daemon is not running")
					return nil
				}
				return err
			}
			fmt.Println("Daemon stopped")
			return nil
		},
	}
}

func newClient(socketFlag, logFileFlag, logLevel string) (*client.Client, error) {
	socketPath, err := control.SocketPath(socketFlag)
	if err != nil {
		return nil, err
	}
	logPath, err := control.LogPath(logFileFlag, socketFlag)
	if err != nil {
		return nil, err
	}
	return client.New(socketPath, logPath, logLevel), nil
}

// normalizeSingleDashLongFlags rewrites registered single-dash long flags to double-dash form.
// It walks the full command tree so root persistent flags and subcommand flags both match.
func normalizeSingleDashLongFlags(args []string, cmd *cobra.Command) []string {
	normalized := make([]string, len(args))
	copy(normalized, args)
	for i, arg := range normalized {
		if len(arg) < 3 || !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
			continue
		}
		name := strings.TrimPrefix(arg, "-")
		if before, _, ok := strings.Cut(name, "="); ok {
			name = before
		}
		if lookupFlag(cmd, name) != nil {
			normalized[i] = "-" + arg
		}
	}
	return normalized
}

func lookupFlag(cmd *cobra.Command, name string) interface{} {
	if f := cmd.PersistentFlags().Lookup(name); f != nil {
		return f
	}
	if f := cmd.Flags().Lookup(name); f != nil {
		return f
	}
	for _, sub := range cmd.Commands() {
		if f := sub.Flags().Lookup(name); f != nil {
			return f
		}
		if f := sub.PersistentFlags().Lookup(name); f != nil {
			return f
		}
	}
	return nil
}
