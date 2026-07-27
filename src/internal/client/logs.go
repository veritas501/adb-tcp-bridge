package client

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// TailFile writes the last n lines of path to w.
// n > 0: last n lines; n == 0: entire file.
func TailFile(path string, n int, w io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("log file not found: %s (is the daemon running?)", path)
		}
		return err
	}
	defer file.Close()

	if n == 0 {
		_, err = io.Copy(w, file)
		return err
	}
	if n < 0 {
		return fmt.Errorf("invalid tail count %d", n)
	}

	scanner := bufio.NewScanner(file)
	// Large log lines are rare; still raise the token limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lines := make([]string, 0, n)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// FollowFile polls path from the current EOF and writes new bytes to w until ctx is canceled.
func FollowFile(ctx context.Context, path string, w io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("log file not found: %s (is the daemon running?)", path)
		}
		return err
	}
	defer file.Close()

	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		n, readErr := file.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if readErr != nil {
			return readErr
		}
	}
}
