# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 语言规范

你可以用英文思考，但请使用中文回答用户（除非用户特殊要求）

## 代码规范

- 写代码时需要添加适量的注释和文档增加代码的可读性和可维护性
- 注释、文档可以用中文，但报错、字符串必须用英文
- 提交代码前需要使用各自语言的format工具对代码进行format（Go 用 `go fmt` / `make fmt`）

## Build / Test / Run

所有开发命令通过根目录 Makefile 驱动（`GO ?= go`，可被环境变量覆盖）：

```bash
make fmt            # go fmt ./...
make test           # go test ./...
make build          # go build -o adbb ./src/cmd/adb-tcp-bridge
make release        # 优化构建：-trimpath -ldflags "-s -w"，产出 ./adbb
make release-cross  # 交叉编译 linux/darwin/windows × amd64/arm64 到 dist/
```

跑单个测试 / 单个包（标准 Go test）：

```bash
go test ./src/internal/bridge/ -run TestFormatDeviceID -v
```

运行二进制（需要本地 adb server 已起、设备已 USB 连接）：

```bash
./adbb <serial>          # serial 来自 adb devices，默认监听 0.0.0.0:35555
```

## 项目布局约定（非显而易见）

- **module 路径包含 `src/`**：`go.mod` 的 module 名是 `adb-tcp-bridge`，但源码在 `src/` 下，因此 import 路径形如 `adb-tcp-bridge/src/internal/bridge`。新增包要放在 `src/` 下。
- **入口在 `./src/cmd/adb-tcp-bridge`**（不是惯例的 `./cmd`），对应 Makefile 里的 `MAIN := ./src/cmd/adb-tcp-bridge`。
- **测试是标准 `testing`**，无外部依赖；`bridge` 包的端口递增测试会真实占用本机端口（见 `server_test.go` 的 `reservePortPair`），CI/并发跑测试时留意端口冲突。

## 架构

`adb-tcp-bridge`（二进制 `adbb`）把 USB 连接的 Android 设备暴露为 ADB-over-TCP 端点。它**不直接碰 USB**——真正的 USB 传输由本地 adb server（默认 `127.0.0.1:5037`）持有，bridge 只做协议翻译和转发。

数据流：`external adb client --TCP--> bridge --host 协议--> 本地 adb server --USB--> 设备 adbd`

代码按 ADB 协议栈分三层，每层一个 internal 包：

- `src/internal/adbwire` —— **设备侧 wire 协议**。24 字节定长包头（command/arg0/arg1/length/checksum/magic）+ payload 的二进制编解码（`ReadPacket`/`WritePacket`），校验 magic 与 checksum。定义 `SYNC/CNXN/AUTH/OPEN/OKAY/WRTE/CLSE/STLS` 命令。
- `src/internal/adbhost` —— **adb server 的 host 协议 client**。ASCII 帧格式（`4 位 hex 长度 + 命令`，返回 `OKAY`/`FAIL`）。`OpenService` 对一个 service 先发 `host:transport:<serial>` 再发 service 命令，成功后清掉 deadline 进入长连接流模式。
- `src/internal/bridge` —— **翻译层**，把 wire 事件映射成 host 协议动作。

`bridge` 包内部的核心抽象（理解数据路径的关键）：

- `Server`：监听 TCP，每个入站连接创建一个 `session`。
- `session`（`session.go`）：维护一个 wire 协议会话，处理 `CNXN`/`AUTH` 握手，按 `localID` 索引管理多个并发的 `service`，把 `OPEN/OKAY/WRTE/CLSE` 分发到对应 service。
- `service`（`service.go`）：对应设备侧的一个 ADB service（如 `shell:...`）。**每个 OPEN 的 service 在本地 adb server 上开一条独立的 transport 连接**（扇出模型：N 个 service = N 条到 5037 的连接），并在 device→client 方向做 `WRTE/OKAY` 流控（`sendWriteAndWaitAck` 等待对端 ACK 再发下一块，防压垮 adb client）。

握手与边界（改相关代码前先知道）：

- `auth` 默认 `accept-all`：发起 token 挑战但**收到任意 signature/publickey 即放行，不做 RSA 验签**（避免 adb client 在签名阶段报误导性的 auth failure）。`none` 模式跳过整个握手。`accept-publickey` 是兼容旧命名。
- `DeviceID`（`CNXN` 响应里的 payload）在 server 启动时通过 `shell:getprop` 读 `ro.product.*` 拼装，读不到就回退到 `server.go` 里的硬编码 `fallbackDeviceID`。
- 端口分配（`Server.listen`）：从 `-port`（默认 `35555`）起逐个向上尝试到 65535，遇到 `EADDRINUSE` 跳到下一个，实际监听地址会打进日志。
- **明确不支持**：直连 USB、Wireless Debugging TLS 传输（`session.handlePacket` 收到 `CmdStls` 直接返回 unsupported）、adb server 的 host 侧命令（如 `host:devices`）、bridge 侧 RSA 验签。
