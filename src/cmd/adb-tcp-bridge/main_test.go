package main

import (
	"strings"
	"testing"
)

func TestRootCommandRequiresSerial(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "missing device serial") {
		t.Fatalf("Execute() error = %q, want missing serial message", err)
	}
}
