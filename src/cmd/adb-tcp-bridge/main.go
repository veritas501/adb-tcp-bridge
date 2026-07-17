package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"adb-tcp-bridge/src/internal/adbhost"
	"adb-tcp-bridge/src/internal/bridge"
	"adb-tcp-bridge/src/internal/hdcserver"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

func main() {
	cmd := newRootCommand()
	cmd.SetArgs(normalizeSingleDashLongFlags(os.Args[1:], cmd))
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func newRootCommand() *cobra.Command {
	var (
		listenHost = bridge.DefaultListenHost
		listenPort = bridge.DefaultListenStartPort
		serverAddr = "127.0.0.1:5037"
		authMode   = string(bridge.AuthAcceptAll)
		logLevel   = zerolog.InfoLevel.String()
		backend    = "adb"
		hdcAddr    = hdcserver.DefaultAddr
	)

	cmd := &cobra.Command{
		Use:           "adbb [flags] <serial|connect-key>",
		Short:         "Expose an ADB/HDC-connected device as ADB-over-TCP",
		Args:          validateArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			level, err := zerolog.ParseLevel(logLevel)
			if err != nil {
				return fmt.Errorf("invalid log level %q", logLevel)
			}
			logger := newLogger(os.Stderr, level)

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			deviceBackend, err := newDeviceBackend(backend, serverAddr, hdcAddr)
			if err != nil {
				return err
			}

			server, err := bridge.NewServer(bridge.Config{
				ListenHost:      listenHost,
				ListenStartPort: listenPort,
				Serial:          args[0],
				Backend:         deviceBackend,
				AuthMode:        bridge.AuthMode(authMode),
				Logger:          &logger,
			})
			if err != nil {
				return err
			}
			return server.ListenAndServe(ctx)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&listenHost, "host", listenHost, "TCP listen host")
	flags.IntVar(&listenPort, "port", listenPort, "first TCP listen port to try")
	flags.StringVar(&serverAddr, "server", serverAddr, "local adb server address")
	flags.StringVar(&backend, "backend", backend, "target backend: adb or hdc")
	flags.StringVar(&hdcAddr, "hdc-server", hdcAddr, "hdc server address when -backend hdc")
	flags.StringVar(&authMode, "auth", authMode, "auth mode: accept-all or none")
	flags.StringVar(&logLevel, "log-level", logLevel, "log level: debug, info, warn, error")

	return cmd
}

func newDeviceBackend(name string, adbServerAddr string, hdcAddr string) (bridge.DeviceBackend, error) {
	switch name {
	case "adb":
		return bridge.NewADBServerBackend(adbhost.New(adbServerAddr)), nil
	case "hdc":
		return hdcserver.New(hdcAddr), nil
	default:
		return nil, fmt.Errorf("unsupported backend %q", name)
	}
}

func newLogger(out io.Writer, level zerolog.Level) zerolog.Logger {
	writer := zerolog.ConsoleWriter{
		Out:        out,
		TimeFormat: "01-02 15:04:05",
	}
	return zerolog.New(writer).Level(level).With().Timestamp().Logger()
}

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
		if cmd.Flags().Lookup(name) != nil {
			normalized[i] = "-" + arg
		}
	}
	return normalized
}

func validateArgs(cmd *cobra.Command, args []string) error {
	switch len(args) {
	case 0:
		return fmt.Errorf("missing device serial/connect key\n\nUsage:\n  %s\n\nRun 'adbb --help' for more options.", cmd.UseLine())
	case 1:
		return nil
	default:
		return fmt.Errorf("expected one device serial, got %d", len(args))
	}
}
