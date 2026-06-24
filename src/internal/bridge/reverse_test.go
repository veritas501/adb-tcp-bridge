package bridge

import (
	"bytes"
	"testing"
	"time"

	"adb-tcp-bridge/src/internal/adbwire"
)

func TestReverseProtocolHelpers(t *testing.T) {
	if got := reverseProtocolString("host tcp:1 tcp:2\n"); !bytes.Equal(got, []byte("0011host tcp:1 tcp:2\n")) {
		t.Fatalf("reverseProtocolString() = %q", got)
	}

	if got := reverseFail("bad"); !bytes.Equal(got, []byte("FAIL0003bad")) {
		t.Fatalf("reverseFail() = %q", got)
	}

	if got := resolvedReverseLocal("tcp:0", []byte("OKAY000512345")); got != "tcp:12345" {
		t.Fatalf("resolvedReverseLocal() = %q, want tcp:12345", got)
	}
}

func TestOpenOutboundSendsOpenAndForwardsData(t *testing.T) {
	adbClient, adbServer := newMemoryConn()
	defer adbClient.Close()
	defer adbServer.Close()

	local, remote := newMemoryConn()
	defer remote.Close()

	session := newSession(Config{Serial: "serial"}, adbServer)
	defer session.close()

	session.openOutbound("tcp:27183", local)

	openPacket, err := adbwire.ReadPacket(adbClient)
	if err != nil {
		t.Fatalf("ReadPacket(open) error = %v", err)
	}
	if openPacket.Command != adbwire.CmdOpen {
		t.Fatalf("open command = %s, want OPEN", openPacket.Command)
	}
	if !bytes.Equal(openPacket.Payload, []byte("tcp:27183\x00")) {
		t.Fatalf("open payload = %q, want tcp target", openPacket.Payload)
	}

	if err := session.handleOkay(adbwire.Packet{
		Command: adbwire.CmdOkay,
		Arg0:    42,
		Arg1:    openPacket.Arg0,
	}); err != nil {
		t.Fatalf("handleOkay(open) error = %v", err)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := remote.Write([]byte("hello"))
		writeDone <- err
	}()

	writePacket, err := adbwire.ReadPacket(adbClient)
	if err != nil {
		t.Fatalf("ReadPacket(WRTE) error = %v", err)
	}
	if writePacket.Command != adbwire.CmdWrte {
		t.Fatalf("write command = %s, want WRTE", writePacket.Command)
	}
	if writePacket.Arg0 != openPacket.Arg0 || writePacket.Arg1 != 42 {
		t.Fatalf("write ids = (%d,%d), want (%d,42)", writePacket.Arg0, writePacket.Arg1, openPacket.Arg0)
	}
	if !bytes.Equal(writePacket.Payload, []byte("hello")) {
		t.Fatalf("write payload = %q, want hello", writePacket.Payload)
	}

	if err := session.handleOkay(adbwire.Packet{
		Command: adbwire.CmdOkay,
		Arg0:    42,
		Arg1:    openPacket.Arg0,
	}); err != nil {
		t.Fatalf("handleOkay(write) error = %v", err)
	}

	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("remote.Write() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("remote.Write() did not complete")
	}
}
