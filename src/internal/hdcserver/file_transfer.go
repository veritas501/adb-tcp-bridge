package hdcserver

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

func (b *Backend) pushFile(ctx context.Context, serial string, local string, remote string, mode uint32) error {
	file, err := os.Open(local)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}

	if err := b.makeRemoteDir(ctx, serial, path.Dir(remote)); err != nil {
		return err
	}

	command := "file send remote " + hdcCommandQuote(path.Base(remote)) + " " + hdcCommandQuote(remote)
	channel, err := b.openChannel(ctx, serial, []byte(command))
	if err != nil {
		return err
	}
	defer channel.Close()

	cmd, _, err := channel.readCommand()
	if err != nil {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return err
	}
	if cmd != hdcFileInit {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return fmt.Errorf("unexpected hdc file init command %d", cmd)
	}
	if err := channel.writeCommand(hdcKernelWakeupSlaveTask, nil); err != nil {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return err
	}
	if err := channel.writeCommand(hdcFileCheck, encodeTransferConfig(uint64(stat.Size()), remote, path.Base(remote), uint64(stat.ModTime().Unix()))); err != nil {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return err
	}

	cmd, _, err = channel.readCommand()
	if err != nil {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return err
	}
	if cmd != hdcFileBegin {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return fmt.Errorf("unexpected hdc file begin command %d", cmd)
	}

	buf := make([]byte, hdcFileChunkSize)
	var index uint64
	for {
		n, readErr := file.Read(buf)
		if n > 0 {
			if err := channel.writeCommand(hdcFileData, encodeTransferPayload(index, buf[:n])); err != nil {
				_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
				return err
			}
			index += uint64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
			return readErr
		}
	}

	cmd, payload, err := channel.readCommand()
	if err != nil {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return err
	}
	if cmd != hdcFileFinish {
		_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
		return fmt.Errorf("unexpected hdc file finish command %d", cmd)
	}
	if len(payload) > 0 && payload[0] != 0 {
		if err := channel.writeCommand(hdcFileFinish, []byte{0}); err != nil {
			_ = channel.writeCommand(hdcKernelChannelClose, []byte{1})
			return err
		}
	}
	if err := channel.writeCommand(hdcKernelChannelClose, []byte{1}); err != nil {
		return err
	}
	return nil
}

func (b *Backend) makeRemoteDir(ctx context.Context, serial string, remote string) error {
	_, err := b.runCommand(ctx, serial, []byte("shell mkdir -p "+shellQuote(remote)))
	return err
}

func hdcCommandQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n\r\"'") {
		return value
	}
	return "\"" + strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "\"", "\\\"") + "\""
}

func encodeTransferConfig(fileSize uint64, remote string, optionalName string, mtime uint64) []byte {
	var out []byte
	out = appendProtoVarintField(out, 1, fileSize)
	out = appendProtoVarintField(out, 2, mtime)
	out = appendProtoVarintField(out, 3, mtime)
	out = appendProtoBytesField(out, 4, nil)
	out = appendProtoBytesField(out, 5, []byte(remote))
	out = appendProtoBytesField(out, 6, []byte(optionalName))
	out = appendProtoVarintField(out, 7, 0)
	out = appendProtoVarintField(out, 8, 0)
	out = appendProtoVarintField(out, 9, 0)
	out = appendProtoBytesField(out, 10, nil)
	out = appendProtoBytesField(out, 11, nil)
	out = appendProtoBytesField(out, 12, nil)
	out = appendProtoBytesField(out, 13, nil)
	return out
}

func encodeTransferPayload(index uint64, data []byte) []byte {
	var head []byte
	head = appendProtoVarintField(head, 1, index)
	head = appendProtoVarintField(head, 2, 0)
	head = appendProtoVarintField(head, 3, uint64(len(data)))
	head = appendProtoVarintField(head, 4, uint64(len(data)))
	payload := make([]byte, hdcFileHeaderSize, hdcFileHeaderSize+len(data))
	copy(payload, head)
	payload = append(payload, data...)
	return payload
}

func appendProtoVarintField(out []byte, fieldNumber int, value uint64) []byte {
	out = appendProtoVarint(out, uint64(fieldNumber<<3))
	return appendProtoVarint(out, value)
}

func appendProtoBytesField(out []byte, fieldNumber int, value []byte) []byte {
	out = appendProtoVarint(out, uint64(fieldNumber<<3|2))
	out = appendProtoVarint(out, uint64(len(value)))
	return append(out, value...)
}

func appendProtoVarint(out []byte, value uint64) []byte {
	for value >= 0x80 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}
