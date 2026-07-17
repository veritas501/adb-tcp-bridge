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
	if !strings.Contains(err.Error(), "missing device serial/connect key") {
		t.Fatalf("Execute() error = %q, want missing serial message", err)
	}
}

func TestNormalizeSingleDashLongFlags(t *testing.T) {
	cmd := newRootCommand()
	got := normalizeSingleDashLongFlags([]string{"-backend", "hdc", "-hdc-server=127.0.0.1:8710", "-x"}, cmd)
	want := []string{"--backend", "hdc", "--hdc-server=127.0.0.1:8710", "-x"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("normalizeSingleDashLongFlags() = %#v, want %#v", got, want)
	}
}
