package adbwire

import (
	"bytes"
	"testing"
)

func TestPacketRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := Packet{
		Command: CmdWrte,
		Arg0:    7,
		Arg1:    9,
		Payload: []byte("hello"),
	}

	if err := WritePacket(&buf, want); err != nil {
		t.Fatalf("WritePacket() error = %v", err)
	}
	got, err := ReadPacket(&buf)
	if err != nil {
		t.Fatalf("ReadPacket() error = %v", err)
	}
	if got.Command != want.Command || got.Arg0 != want.Arg0 || got.Arg1 != want.Arg1 {
		t.Fatalf("ReadPacket() = %+v, want %+v", got, want)
	}
	if !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("payload = %q, want %q", got.Payload, want.Payload)
	}
}
