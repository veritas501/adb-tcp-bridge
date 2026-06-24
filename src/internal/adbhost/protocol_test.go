package adbhost

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncodeFrame(t *testing.T) {
	got, err := EncodeFrame("host:devices")
	if err != nil {
		t.Fatalf("EncodeFrame() error = %v", err)
	}
	if got != "000chost:devices" {
		t.Fatalf("EncodeFrame() = %q, want 000chost:devices", got)
	}

	if _, err := EncodeFrame(strings.Repeat("x", maxFrameLen+1)); err == nil {
		t.Fatal("EncodeFrame() expected error for oversized payload")
	}
}

func TestFailMessage(t *testing.T) {
	if got := FailMessage("bad"); !bytes.Equal(got, []byte("FAIL0003bad")) {
		t.Fatalf("FailMessage() = %q, want FAIL0003bad", got)
	}
}

func TestHasStatus(t *testing.T) {
	if !HasStatus([]byte("OKAY0000"), StatusOKAY) {
		t.Fatal("HasStatus() = false, want true for OKAY prefix")
	}
	if HasStatus([]byte("OK"), StatusOKAY) {
		t.Fatal("HasStatus() = true, want false for short input")
	}
	if HasStatus([]byte("FAIL"), StatusOKAY) {
		t.Fatal("HasStatus() = true, want false for FAIL prefix")
	}
}

func TestParseLengthPrefixed(t *testing.T) {
	value, ok := ParseLengthPrefixed([]byte("OKAY000512345"), 4)
	if !ok || value != "12345" {
		t.Fatalf("ParseLengthPrefixed() = (%q, %v), want (12345, true)", value, ok)
	}

	if _, ok := ParseLengthPrefixed([]byte("OKAY00"), 4); ok {
		t.Fatal("ParseLengthPrefixed() ok = true, want false for truncated length")
	}
	if _, ok := ParseLengthPrefixed([]byte("OKAY0009short"), 4); ok {
		t.Fatal("ParseLengthPrefixed() ok = true, want false for truncated content")
	}
	if _, ok := ParseLengthPrefixed([]byte("OKAYzzzz"), 4); ok {
		t.Fatal("ParseLengthPrefixed() ok = true, want false for invalid hex length")
	}
	if _, ok := ParseLengthPrefixed([]byte("OK"), -1); ok {
		t.Fatal("ParseLengthPrefixed() ok = true, want false for negative offset")
	}
}
