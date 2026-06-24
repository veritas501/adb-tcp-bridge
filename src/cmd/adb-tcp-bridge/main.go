package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"adb-tcp-bridge/src/internal/adbhost"
	"adb-tcp-bridge/src/internal/bridge"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
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
	)

	cmd := &cobra.Command{
		Use:           "adbb [flags] <serial>",
		Short:         "Expose a USB-connected Android device as ADB-over-TCP",
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

			server, err := bridge.NewServer(bridge.Config{
				ListenHost:      listenHost,
				ListenStartPort: listenPort,
				Serial:          args[0],
				Host:            adbhost.New(serverAddr),
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
	flags.StringVar(&authMode, "auth", authMode, "auth mode: accept-all or none")
	flags.StringVar(&logLevel, "log-level", logLevel, "log level: debug, info, warn, error")

	return cmd
}

func newLogger(out io.Writer, level zerolog.Level) zerolog.Logger {
	writer := zerolog.ConsoleWriter{
		Out:        out,
		TimeFormat: "01-02 15:04:05",
	}
	return zerolog.New(writer).Level(level).With().Timestamp().Logger()
}

func validateArgs(cmd *cobra.Command, args []string) error {
	switch len(args) {
	case 0:
		return fmt.Errorf("missing device serial\n\nUsage:\n  %s\n\nRun 'adbb --help' for more options.", cmd.UseLine())
	case 1:
		return nil
	default:
		return fmt.Errorf("expected one device serial, got %d", len(args))
	}
}
