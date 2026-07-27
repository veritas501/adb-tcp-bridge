# 使用指导

## 适用场景

`adbb` 适合把只通过 USB 接到一台主机的设备临时暴露成网络上的 ADB 目标。外部机器使用普通 `adb connect <bridge-host>:<port>` 连接；桥接主机继续通过本机 `adb server` 或 `hdc server` 访问真实设备。

一个 daemon 进程可同时管理多台设备。CLI 子命令通过 Unix domain socket 控制 daemon；`start` / `list` / `status` 在 daemon 未运行时会自动后台拉起。

```text
external adb client
  --TCP--> adbb (per-device listener)
    --adb host protocol--> local adb server --> Android adbd
    --hdc host protocol--> hdc server       --> OpenHarmony hdcd

CLI
  --UDS--> daemon Manager --> bridge.Server
```

## 前置条件

- Go 1.22+：仅从源码构建时需要，版本由 `go.mod` 声明。
- 使用 ADB 后端时：桥接主机已安装 Android SDK platform-tools，并且本机 adb server 可连接目标设备。
- 使用 HDC 后端时：桥接主机已有可用 HDC server，默认地址为 `127.0.0.1:8710`。
- 网络侧客户端只需要普通 `adb`，不需要直连 USB 设备。

## 构建

命令来自项目 `Makefile`。

```bash
make release
```

该命令生成当前平台的 `./adbb`。开发调试时也可以使用：

```bash
make build
make test
make fmt
```

跨平台发布包：

```bash
make release-cross
```

生成物写入 `dist/`，覆盖 Linux、macOS、Windows 的 amd64 与 arm64 目标。

## ADB 后端：暴露 Android 设备

1. 在桥接主机确认 USB 设备已被本机 adb server 识别：

   ```bash
   adb start-server
   adb devices
   ```

2. 使用 `adb devices` 输出中的 `<serial>` 启动桥接：

   ```bash
   ./adbb start <serial>
   ```

   命令打印一行 `listen_addr` 后退出。默认从 `0.0.0.0:35555` 起监听；端口占用时向上寻找，以打印的地址为准。daemon 若不存在会自动拉起。

3. 在另一台机器或本机连接桥接端口：

   ```bash
   adb connect <bridge-host>:35555
   adb devices
   adb -s <bridge-host>:35555 shell
   ```

4. 结束使用：

   ```bash
   adb disconnect <bridge-host>:35555
   ./adbb stop <serial>          # 停止该设备的 bridge
   ./adbb kill-server            # 关闭整个 daemon
   ```

## HDC 后端：暴露 OpenHarmony 设备

1. 在桥接主机查询 HDC target：

   ```bash
   hdc list targets
   ```

2. 使用 HDC target 或 connect key 启动桥接：

   ```bash
   ./adbb start --backend hdc <hdc-target>
   ```

   如果 HDC server 不在默认地址，显式指定：

   ```bash
   ./adbb start --backend hdc --hdc-server 127.0.0.1:8710 <hdc-target>
   ```

3. 外部仍使用普通 adb 客户端连接：

   ```bash
   adb connect <bridge-host>:35555
   adb -s <bridge-host>:35555 shell
   ```

HDC 后端当前翻译 `shell:`、`exec:`、`sync:`，以及 `localabstract:` / `localfilesystem:` / `localreserved:` / `tcp:` / `local:`（通过 HDC `fport`）；覆盖常见 `adb shell`、`adb push`、`adb pull` 和调试器 abstract socket 路径；不支持任意 ADB service。

## 多设备示例

```bash
./adbb start serial-a
./adbb start --port 40000 serial-b
./adbb list
# serial-a	adb	0.0.0.0:35555	running
# serial-b	adb	0.0.0.0:40000	running

./adbb stop serial-a
./adbb status
```

## 命令一览

| 命令 | 说明 | 自动拉起 daemon |
|------|------|-----------------|
| `adbb start [flags] <serial>` | 启动一台设备的 bridge，stdout 打印 `listen_addr` | 是 |
| `adbb stop <serial>` | 停止一台 | 否（daemon 未运行时报错） |
| `adbb list` | 列出 running bridges | 是 |
| `adbb status [serial]` | 状态摘要；含 `log_path` | 是 |
| `adbb logs [-n N] [-f]` | 读本地 daemon 日志文件 | 否（只读文件） |
| `adbb kill-server` | 关闭 daemon | 否（未运行时提示并 exit 0） |
| `adbb daemon` | 前台运行 daemon（stderr + 日志文件） | — |

兼容旧调用：`adbb <serial>` 等价于 `adbb start <serial>`（短生命周期，不阻塞）。

## 参数

### 共享 / 持久 flags

| 参数 / 环境变量 | 默认值 | 说明 |
|-----------------|--------|------|
| `--socket` / `ADBB_SOCKET` | `$XDG_RUNTIME_DIR/adbb/adbb.sock`，否则 `~/.adbb/adbb.sock` | 控制面 Unix socket。 |
| `--log-file` / `ADBB_LOG` | 与 socket 同目录的 `adbb.log` | daemon 日志文件。 |
| `--log-level` | `info` | daemon 日志级别。 |

### `start` flags

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--host` | `0.0.0.0` | TCP 监听主机。 |
| `--port` | `35555` | 起始监听端口；占用时向上递增。 |
| `--server` | `127.0.0.1:5037` | ADB 后端使用的本机 adb server 地址。 |
| `--backend` | `adb` | 后端类型：`adb` 或 `hdc`。 |
| `--hdc-server` | `127.0.0.1:8710` | HDC 后端使用的 HDC server 地址。 |
| `--auth` | `accept-all` | ADB auth 模式：`accept-all` 或 `none`。 |

### `logs` flags

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-n` / `--tail` | `200` | 输出末尾 N 行；`0` 表示全文。 |
| `-f` / `--follow` | false | 输出 tail 后继续 follow 新内容；SIGINT 退出。 |

兼容旧脚本时，程序会把已注册的单横线长参数规范化为双横线形式；例如 `-backend hdc` 与 `--backend hdc` 等价。

## Auth 模式

- `accept-all`：默认模式。桥接端发起 ADB auth challenge，但收到任意签名或 RSA public key 后即放行；不做 RSA 签名验签。
- `none`：跳过 auth handshake，直接返回 `CNXN`。

如果桥接端口暴露在不可信网络，必须通过防火墙、隧道或只监听可信地址限制访问；默认 auth 不提供设备级身份校验。

## 日志与排障

- daemon 日志写入固定文件（默认与 socket 同目录的 `adbb.log`），由 daemon 进程自身 append，不依赖 CLI 重定向。
- 查看日志：

  ```bash
  ./adbb logs -n 50
  ./adbb logs -f          # follow
  ```

- `adbb status` 会打印 `log_path\t<path>`。
- 启动成功：日志包含 `listening`，字段包括 `listen_addr`、`serial`、`backend`。
- 客户端连接：日志包含 `connect from host`，字段包括 `remote_addr` 和 `remote_host`。
- `adb connect` 使用的端口应以 `adbb start` 打印的 `listen_addr` 为准；不要假设一定是 `35555`。
- ADB 后端连接失败时，先确认 `adb devices` 能看到目标 `<serial>`，并确认 `--server` 指向正确 adb server。
- HDC 后端连接失败时，先确认 `hdc list targets` 输出包含 `<hdc-target>`，并确认 `--hdc-server` 地址正确。
- 前台调试 daemon 输出：`./adbb daemon`（同时写 stderr 与日志文件）。
- Wireless Debugging TLS transport 不在当前实现范围内；桥接端面向普通明文 ADB transport。
