package daemon

import (
	"fmt"
	"io"
	"os"

	"adb-tcp-bridge/src/internal/control"
	"github.com/rs/zerolog"
)

// OpenLogger opens the daemon log file and optionally mirrors to stderr.
// ConsoleWriter format matches the historical main CLI time layout.
func OpenLogger(logPath string, level zerolog.Level, foreground bool) (zerolog.Logger, io.Closer, error) {
	if err := control.EnsureDir(logPath); err != nil {
		return zerolog.Logger{}, nil, err
	}

	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return zerolog.Logger{}, nil, fmt.Errorf("open log file %s: %w", logPath, err)
	}

	var out io.Writer = file
	if foreground {
		out = io.MultiWriter(os.Stderr, file)
	}

	writer := zerolog.ConsoleWriter{
		Out:        out,
		TimeFormat: "01-02 15:04:05",
	}
	logger := zerolog.New(writer).Level(level).With().Timestamp().Logger()
	return logger, file, nil
}
