package client

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTailFileLastLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atb.log")
	content := "line1\nline2\nline3\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := TailFile(path, 2, &buf); err != nil {
		t.Fatalf("TailFile() error = %v", err)
	}
	got := strings.TrimSpace(buf.String())
	want := "line2\nline3"
	if got != want {
		t.Fatalf("TailFile() = %q, want %q", got, want)
	}
}

func TestTailFileMissing(t *testing.T) {
	err := TailFile(filepath.Join(t.TempDir(), "missing.log"), 10, &bytes.Buffer{})
	if err == nil {
		t.Fatal("TailFile() error = nil, want not found")
	}
	if !strings.Contains(err.Error(), "log file not found") {
		t.Fatalf("TailFile() error = %q, want log file not found", err)
	}
}

func TestFollowFileReadsAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atb.log")
	if err := os.WriteFile(path, []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var buf bytes.Buffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- FollowFile(ctx, path, &buf)
	}()

	// Give FollowFile time to open and seek EOF.
	time.Sleep(50 * time.Millisecond)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("appended\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "appended") {
			cancel()
			if err := <-errCh; err != nil {
				t.Fatalf("FollowFile() error = %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-errCh
	t.Fatalf("FollowFile() did not observe append, got %q", buf.String())
}
