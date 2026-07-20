# 软件实现

## 协议分层

`adb-tcp-bridge` 同时面对三类协议边界：

1. 外部 adb client 使用 ADB wire packet 连接 bridge。
2. ADB 后端使用 adb server host protocol 连接本机 adb server。
3. HDC 后端使用 HDC server channel/frame 协议连接 OpenHarmony 设备。

bridge 的核心实现策略是：保留外部 ADB wire 语义，把每个 ADB `OPEN` service 映射成一个 `net.Conn`，再由不同后端负责连接真实目标。

## ADB wire packet

实现位置：`src/internal/adbwire/packet.go`。

ADB packet header 固定 24 字节：

| 字段 | 大小 | 说明 |
|------|------|------|
| command | 4 | 小端 uint32，ASCII 命令反向编码。 |
| arg0 | 4 | 命令参数。 |
| arg1 | 4 | 命令参数。 |
| payload length | 4 | payload 字节数。 |
| checksum | 4 | payload 字节累加和。 |
| magic | 4 | `command ^ 0xffffffff`。 |

当前实现支持识别 `SYNC`、`CNXN`、`OPEN`、`OKAY`、`CLSE`、`WRTE`、`AUTH`、`STLS`。读取时会校验 magic；payload 上限为 `16 << 20`，用于防止异常客户端声明超大 payload 造成内存膨胀；checksum 为非零时才校验，这兼容部分历史实现。

写 packet 时会重新计算 payload length、checksum 和 magic。

## 会话握手与 auth

实现位置：`src/internal/bridge/session.go`。

握手流程：

1. 外部 adb client 发送 `CNXN`。
2. bridge 选择协议版本和最大 payload：
   - 默认版本：`0x01000000`。
   - 默认最大 payload：`4096`。
   - 写回 payload 上限：`64 * 1024`。
3. 如果 `AuthMode == none`，直接发送 bridge 的 `CNXN`。
4. 否则发送 20 字节随机 `AUTH TOKEN`。
5. `accept-all` 模式收到 `AUTH SIGNATURE` 或 `AUTH RSAPUBLICKEY` 后都视为通过，并发送 `CNXN`。

安全边界：`accept-all` 只保持 adb client 交互流程顺畅，不提供真实密钥认证。若端口暴露到不可信网络，必须依赖网络层访问控制。

TLS 边界：收到 `STLS` 时返回 `ADB TLS transport is unsupported`。同时，`CNXN` 阶段不会盲目回显新版本，避免诱导现代 adb 进入当前未实现的 TLS transport。

## 普通 service 转发

实现位置：`src/internal/bridge/service.go`。

外部 adb client 打开一个 service 时，典型流程如下：

```text
client OPEN(service) -> session.handleOpen
  -> startService(localID, clientRemoteID)
  -> service.run
      -> backend.OpenService(serial, service)
      -> client OKAY(localID, clientRemoteID)
      -> backend conn read => client WRTE
client WRTE => backend conn write => client OKAY
client OKAY => release service write-side flow control
client CLSE / backend EOF / session close => service.finish => client CLSE
```

`sendWriteAndWaitAck` 是后端到 client 方向的关键流控点：它发送 `WRTE` 后等待对端 `OKAY`，因此读取 buffer 在 ACK 返回前不会被复用，避免额外拷贝。

## ADB server host protocol 后端

实现位置：`src/internal/adbhost/` 与 `src/internal/bridge/backend.go`。

adb server host protocol 命令使用 `4 位十六进制长度 + payload` 帧。例如打开真实设备 service：

1. TCP 连接本机 adb server，默认地址 `127.0.0.1:5037`。
2. 发送 `host:transport:<serial>`，读取 `OKAY` 或 `FAIL`。
3. 发送具体 service，例如 `shell:getprop`、`sync:`、`reverse:...`。
4. 命令阶段成功后清除 deadline，将连接作为长期 service stream 返回。

`RunService` 只用于短小一次性响应，例如 reverse 控制命令；注释明确禁止用于 `shell:` 这类长连接 service，因为 `io.ReadAll` 会无界缓冲且可能永不返回。

读取设备属性时，ADB 后端打开 `shell:getprop` 并解析 `[key]: [value]` 行，用于构造 ADB `CNXN` 的 device banner。

## HDC server channel 后端

实现位置：`src/internal/hdcserver/backend.go`。

HDC 后端默认连接 `127.0.0.1:8710`，每个 channel 使用 4 字节大端长度前缀帧。打开 channel 时会：

1. 读取 HDC server handshake。
2. 校验 `OHOS HDC` 标识和 handshake 长度。
3. 把 connect key 写入 handshake 响应的固定区域。
4. 发送目标 HDC command。
5. 返回 `channelConn`，由上层按 `net.Conn` 使用。

### shell / exec

`OpenService` 支持 `shell:`、`exec:` 和带选项的 shell service。后端将其转换为 HDC `shell` 命令：

- 普通 shell：在 ADB service conn 和 HDC channel 之间直接双向复制。
- shell v2：转换 stdin/stdout/exit/close-stdin 等 shell v2 packet；stdout 写回 adb 侧时保留 stream id 和长度格式。

### sync:

ADB `sync:` 是一个多命令子协议。HDC 后端用 `net.Pipe` 向 bridge 返回一个本地连接，并在 goroutine 中处理请求：

| ADB sync 命令 | HDC 后端行为 |
|---------------|--------------|
| `STAT` | 通过远端 shell 获取路径信息，写回 `STAT`。 |
| `LIST` | 通过远端 shell 列目录，逐项写回 `DENT`，最后 `DONE`。 |
| `SEND` | 先把 adb 侧 DATA 写入临时文件，再用 HDC native file send task 推送。目录模式会转为 `mkdir -p`。 |
| `RECV` | 用远端 shell 包装命令读取文件，按 `DATA` 分块写回，结束时 `DONE`。 |
| `QUIT` | 结束 sync 会话。 |

`SEND` 的 HDC native file send 过程会依次处理 `FileInit`、`WakeupSlaveTask`、`FileCheck`、`FileBegin`、多个 `FileData`、`FileFinish` 和 channel close。文件分块大小为 `49152` 字节。

### localabstract / localfilesystem / localreserved / tcp

ADB 的设备本地 socket 服务（例如 LLDB 的 `localabstract:lldb-platform-live`）在 HDC 侧没有等价的一次性 channel 命令，只能通过 `fport` 规则暴露。

处理流程：

1. 解析 ADB service 名，得到 HDC remotenode（`local:` 映射为 `localfilesystem:`）。
2. 在 `40000-60000` 高位区间随机挑选临时端口，向 HDC server 发送 `fport tcp:<port> <remotenode>`。
3. 成功后 dial 该本地端口，返回包装过的 `net.Conn`。
4. 连接 `Close` 时发送 `fport rm tcp:<port> <remotenode>`，避免临时映射泄漏。

端口不能先在 bridge 进程内 `Listen` 再交给 `fport`：在 WSL 访问 Windows `hdc.exe` 时，Linux 侧占用过的端口会让 Windows 侧 bind 失败（`TCP Port listen failed`）。因此采用随机端口 + 重试。

### 设备属性

HDC 后端先运行 `list targets -v` 查找目标，再用 `shell param get` 查询产品名称、型号和设备名，最后填充为 ADB banner 需要的 `ro.product.*` 键。查不到时上层 `bridge.NewServer` 会记录 warning，并使用 fallback device id。

## adb reverse 实现

实现位置：`src/internal/bridge/reverse.go`。

`reverse:` 是本地合成 service，不进入普通后端转发。处理流程：

1. `session.localHandler` 识别 `reverse:` 前缀。
2. `reverseManager.handle` 解析控制命令。
3. `forward` 在 bridge 本机监听 `127.0.0.1:<port>`，并通过真实设备后端运行设备侧 reverse 命令。
4. 本地 listener 收到连接后，`session.openOutbound` 向外部 adb client 发送新的 ADB `OPEN`。
5. 外部 adb client `OKAY` 后，反向连接数据进入普通 service 流控路径。

`killforward`、`killforward-all` 和 session close 都会关闭本地 listener；`evict` 同时通知设备侧撤销对应 forward。

## 错误处理原则

- 协议格式错误直接终止当前 session 或 service，避免继续在不可信状态上转发。
- 设备属性读取失败不会阻止服务启动；server 记录 warning 并使用 fallback banner。
- HDC 后端对未知 ADB service 返回显式错误，不做静默 fallback。
- 普通 EOF、主动关闭和 context 取消不作为错误日志输出；非预期读失败才记录 error。
