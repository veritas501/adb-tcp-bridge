package main

import (
	"bytes"
	"strings"
	"testing"

	"adb-tcp-bridge/src/internal/control"
	"github.com/spf13/cobra"
)

func TestRootCommandBareShowsFullHelp(t *testing.T) {
	cmd := newRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err != errHelpShown {
		t.Fatalf("Execute() error = %v, want errHelpShown", err)
	}
	assertRootHelp(t, out.String())
}

func TestStartCommandRequiresSerial(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetArgs([]string{"start"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want error")
	}
	// cobra ExactArgs message
	if !strings.Contains(err.Error(), "accepts 1 arg") && !strings.Contains(strings.ToLower(err.Error()), "arg") {
		t.Fatalf("Execute() error = %q, want missing serial/args message", err)
	}
}

func TestNormalizeSingleDashLongFlags(t *testing.T) {
	cmd := newRootCommand()
	got := normalizeSingleDashLongFlags([]string{"-backend", "hdc", "-hdc-server=127.0.0.1:8710", "-x", "start"}, cmd)
	want := []string{"--backend", "hdc", "--hdc-server=127.0.0.1:8710", "-x", "start"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("normalizeSingleDashLongFlags() = %#v, want %#v", got, want)
	}
}

func TestRootHelpContainsCommandOverview(t *testing.T) {
	cmd := newRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--help error = %v", err)
	}
	assertRootHelp(t, out.String())
}

func assertRootHelp(t *testing.T, text string) {
	t.Helper()
	// Dynamic sections come from cobra registrations, not a hand-written table.
	for _, want := range []string{
		"Available Commands:",
		"start",
		"stop",
		"list",
		"status",
		"logs",
		"kill-server",
		"version",
		"daemon",
		"--socket",
		"ATB_SOCKET",
		"Examples:",
		"atb start <serial>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("root help missing %q; got:\n%s", want, text)
		}
	}
	if strings.Count(text, "Available Commands:") != 1 {
		t.Fatalf("Available Commands appears %d times; got:\n%s", strings.Count(text, "Available Commands:"), text)
	}
	if strings.Count(text, "Examples:") != 1 {
		t.Fatalf("Examples appears %d times; got:\n%s", strings.Count(text, "Examples:"), text)
	}
	for _, unwanted := range []string{"Auto-start daemon", "Control socket", "Log file", "Legacy form"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("root help should stay concise but contains %q; got:\n%s", unwanted, text)
		}
	}
}

func TestStartHelp(t *testing.T) {
	cmd := newRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"start", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("start --help error = %v", err)
	}
	if !strings.Contains(out.String(), "walks upward") {
		t.Fatalf("start --help missing port note; got:\n%s", out.String())
	}
}

func TestVersionCommand(t *testing.T) {
	cmd := newRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version error = %v", err)
	}
	for _, want := range []string{"adb-tcp-bridge", "commit:", "built:"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("version output missing %q; got:\n%s", want, out.String())
		}
	}
}

func TestRootHelpIncludesNewCommandAutomatically(t *testing.T) {
	cmd := newRootCommand()
	cmd.AddCommand(&cobra.Command{
		Use:   "probe",
		Short: "probe command for help regression",
	})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Help(); err != nil {
		t.Fatalf("Help() error = %v", err)
	}
	if !strings.Contains(out.String(), "probe") {
		t.Fatalf("dynamically added command missing from help:\n%s", out.String())
	}
}

func TestPrintStartResult(t *testing.T) {
	var buf bytes.Buffer
	printStartResult(&buf, control.BridgeInfo{
		Serial:     "e9768fa9",
		Backend:    "adb",
		ListenAddr: "0.0.0.0:35557",
		State:      "running",
	})
	got := buf.String()
	for _, want := range []string{
		"Started bridge for e9768fa9 (adb)",
		"listen:  0.0.0.0:35557",
		"connect: adb connect 127.0.0.1:35557",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("printStartResult missing %q; got:\n%s", want, got)
		}
	}
}

func TestPrintBridgeTable(t *testing.T) {
	var buf bytes.Buffer
	printBridgeTable(&buf, []control.BridgeInfo{
		{Serial: "b", Backend: "hdc", ListenAddr: "127.0.0.1:40000", State: "running"},
		{Serial: "a", Backend: "adb", ListenAddr: "127.0.0.1:35555", State: "running"},
	})
	got := buf.String()
	if !strings.Contains(got, "SERIAL") || !strings.Contains(got, "BACKEND") {
		t.Fatalf("missing header: %s", got)
	}
	// Sorted by serial: a before b
	idxA := strings.Index(got, "a ")
	if idxA < 0 {
		idxA = strings.Index(got, "a\t")
	}
	if idxA < 0 {
		// tabwriter may keep spaces
		idxA = strings.Index(got, "a  ")
	}
	idxB := strings.Index(got, "b ")
	if idxB < 0 {
		idxB = strings.Index(got, "b  ")
	}
	if idxA < 0 || idxB < 0 || idxA > idxB {
		t.Fatalf("expected serial a before b; got:\n%s", got)
	}
	if !strings.Contains(got, "127.0.0.1:35555") || !strings.Contains(got, "hdc") {
		t.Fatalf("missing row data:\n%s", got)
	}
}

func TestPrintBridgeTableEmpty(t *testing.T) {
	var buf bytes.Buffer
	printBridgeTable(&buf, nil)
	if got := strings.TrimSpace(buf.String()); got != "No running bridges." {
		t.Fatalf("empty table = %q", got)
	}
}

func TestConnectTargetWildcard(t *testing.T) {
	if got := connectTarget("0.0.0.0:35557"); got != "127.0.0.1:35557" {
		t.Fatalf("connectTarget wildcard = %q", got)
	}
	if got := connectTarget("127.0.0.1:40000"); got != "127.0.0.1:40000" {
		t.Fatalf("connectTarget explicit = %q", got)
	}
}
