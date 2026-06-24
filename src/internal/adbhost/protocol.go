package adbhost

import (
	"fmt"
	"strconv"
)

// ADB host 协议的响应状态前缀（4 字节 ASCII）。
const (
	StatusOKAY = "OKAY"
	StatusFAIL = "FAIL"
)

// maxFrameLen 是长度前缀能表示的最大字节数（4 位十六进制）。
const maxFrameLen = 0xffff

// EncodeFrame 构造 host 协议的长度前缀帧：4 位小写十六进制长度 + payload。
// payload 超过 maxFrameLen 时返回错误。
func EncodeFrame(payload string) (string, error) {
	if len(payload) > maxFrameLen {
		return "", fmt.Errorf("adb host frame too large")
	}
	return fmt.Sprintf("%04x%s", len(payload), payload), nil
}

// EncodeFrameBytes 与 EncodeFrame 相同，但返回 []byte，便于直接写入响应流。
func EncodeFrameBytes(payload string) ([]byte, error) {
	frame, err := EncodeFrame(payload)
	if err != nil {
		return nil, err
	}
	return []byte(frame), nil
}

// FailMessage 构造一个完整的 FAIL 响应：FAIL + 长度前缀的 reason。
// reason 过长时会被截断到 maxFrameLen，保证始终能产出合法响应。
func FailMessage(reason string) []byte {
	if len(reason) > maxFrameLen {
		reason = reason[:maxFrameLen]
	}
	return []byte(fmt.Sprintf("%s%04x%s", StatusFAIL, len(reason), reason))
}

// HasStatus 检查响应是否以指定的 4 字节状态前缀（OKAY/FAIL）开头。
func HasStatus(response []byte, status string) bool {
	return len(response) >= 4 && string(response[:4]) == status
}

// ParseLengthPrefixed 从 data[offset:] 解析一个长度前缀帧（4 位十六进制长度
// + 内容），返回内容；当剩余字节不足或长度无法解析时返回 ok=false。
func ParseLengthPrefixed(data []byte, offset int) (string, bool) {
	if offset < 0 || len(data) < offset+4 {
		return "", false
	}
	length, err := strconv.ParseUint(string(data[offset:offset+4]), 16, 16)
	if err != nil {
		return "", false
	}
	end := offset + 4 + int(length)
	if len(data) < end {
		return "", false
	}
	return string(data[offset+4 : end]), true
}
