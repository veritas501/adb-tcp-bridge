package adbwire

import (
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	// ADB packet header is always 24 bytes:
	// command, arg0, arg1, payload length, checksum, magic.
	HeaderSize = 24

	// 防止异常客户端声明超大 payload 导致内存膨胀。
	MaxPayloadSize = 16 << 20
)

type Command uint32

const (
	CmdSync Command = 0x434e5953 // "SYNC"
	CmdCnxn Command = 0x4e584e43 // "CNXN"
	CmdOpen Command = 0x4e45504f // "OPEN"
	CmdOkay Command = 0x59414b4f // "OKAY"
	CmdClse Command = 0x45534c43 // "CLSE"
	CmdWrte Command = 0x45545257 // "WRTE"
	CmdAuth Command = 0x48545541 // "AUTH"
	CmdStls Command = 0x534c5453 // "STLS"
)

const (
	AuthToken        uint32 = 1
	AuthSignature    uint32 = 2
	AuthRSAPublicKey uint32 = 3
)

type Packet struct {
	Command Command
	Arg0    uint32
	Arg1    uint32
	Payload []byte
}

func (c Command) String() string {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(c))
	if utf8.Valid(buf[:]) {
		return string(buf[:])
	}
	return fmt.Sprintf("0x%08x", uint32(c))
}

func Checksum(payload []byte) uint32 {
	var sum uint32
	for _, b := range payload {
		sum += uint32(b)
	}
	return sum
}

func Magic(command Command) uint32 {
	return uint32(command) ^ 0xffffffff
}

func ReadPacket(r io.Reader) (Packet, error) {
	var header [HeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Packet{}, err
	}

	command := Command(binary.LittleEndian.Uint32(header[0:4]))
	arg0 := binary.LittleEndian.Uint32(header[4:8])
	arg1 := binary.LittleEndian.Uint32(header[8:12])
	length := binary.LittleEndian.Uint32(header[12:16])
	check := binary.LittleEndian.Uint32(header[16:20])
	magic := binary.LittleEndian.Uint32(header[20:24])

	if magic != Magic(command) {
		return Packet{}, fmt.Errorf("invalid packet magic for %s", command)
	}
	if length > MaxPayloadSize {
		return Packet{}, fmt.Errorf("packet payload too large: %d", length)
	}

	payload := make([]byte, int(length))
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Packet{}, err
		}
	}
	if check != 0 && check != Checksum(payload) {
		return Packet{}, fmt.Errorf("invalid packet checksum for %s", command)
	}

	return Packet{
		Command: command,
		Arg0:    arg0,
		Arg1:    arg1,
		Payload: payload,
	}, nil
}

func WritePacket(w io.Writer, packet Packet) error {
	var header [HeaderSize]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(packet.Command))
	binary.LittleEndian.PutUint32(header[4:8], packet.Arg0)
	binary.LittleEndian.PutUint32(header[8:12], packet.Arg1)
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(packet.Payload)))
	binary.LittleEndian.PutUint32(header[16:20], Checksum(packet.Payload))
	binary.LittleEndian.PutUint32(header[20:24], Magic(packet.Command))

	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(packet.Payload) == 0 {
		return nil
	}
	_, err := w.Write(packet.Payload)
	return err
}
