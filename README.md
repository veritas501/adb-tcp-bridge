# adb-tcp-bridge

`adb-tcp-bridge` exposes an ADB- or HDC-connected device as an ADB-over-TCP
endpoint. External adb clients connect to the bridge over TCP; the bridge
translates device-side ADB packets into either the local adb server host
protocol or the HDC host/server protocol.

A single **daemon** process can manage multiple devices. Short-lived CLI
commands (`start` / `stop` / `list` / …) talk to the daemon over a Unix domain
socket and auto-start it when needed.

The bridge never touches USB directly — the local adb server (usually
`127.0.0.1:5037`) keeps owning the real USB transport.

```text
external adb client
  --TCP--> adb-tcp-bridge (per-device listener)
    --adb host protocol--> local adb server --> Android adbd
    --hdc host protocol--> hdc server       --> OpenHarmony hdcd

CLI (atb start/stop/list/…)
  --Unix socket--> daemon control plane --> multi-device Manager
```

## Why

A device that is only reachable over USB is bound to the one machine it is
plugged into. `adb-tcp-bridge` turns it into a network-addressable adb target so
that any machine on the network — a CI runner, a colleague's laptop, a VM
without USB passthrough — can `adb connect` to it as if it were a wireless
debugging endpoint, without re-plugging the device or running `adb` on the host
machine.

## Prerequisites

- **Go 1.22+** (declared in `go.mod`) — only needed to build from source.
- **adb** (Android SDK platform-tools) on the host running the bridge, with the
  local adb server reachable at the address passed via `--server`.
- **One USB-connected Android device** already visible to the local adb server
  (or an HDC target when using `--backend hdc`).

Start the local adb server and confirm the device is attached before launching
the bridge:

```bash
adb start-server
adb devices   # copy the <serial> of your USB device
```

## Quickstart

```bash
make release                      # builds ./atb (stripped, trimmed)
./atb start <serial>              # auto-starts daemon; prints listen_addr
```

From another machine (or the same one), connect a regular adb client to the
printed address:

```bash
adb connect <bridge-host>:35555   # use the port from start output if different
adb devices
```

`atb start` is short-lived: it prints one `listen_addr` line and exits. The
daemon keeps the bridge running in the background. Daemon logs go to a fixed
log file (see below); use `atb logs` / `atb logs -f` to inspect them.

> If `35555` is already in use, the bridge automatically tries the next port
> upward. Always use the address printed by `atb start`.

Legacy form `atb <serial>` is equivalent to `atb start <serial>`.

## Commands

| Command | Description |
|---------|-------------|
| `atb start [flags] <serial>` | Start a bridge for one device; prints `listen_addr`. Auto-starts daemon. |
| `atb stop <serial>` | Stop one bridge. |
| `atb list` | List running bridges (`serial`, `backend`, `listen_addr`, `state`). |
| `atb status [serial]` | Daemon/bridge status; includes `log_path`. |
| `atb logs [-n N] [-f]` | Read local daemon log file (default last 200 lines; `-f` follow). |
| `atb version` | Print binary version, commit, and build date. |
| `atb kill-server` | Shut down the daemon. |
| `atb daemon` | Run the daemon in the foreground (stderr + log file). |

### Shared flags

| Flag / env | Default | Description |
|------------|---------|-------------|
| `--socket` / `ATB_SOCKET` | `$XDG_RUNTIME_DIR/atb/atb.sock` or `~/.atb/atb.sock` | Daemon control socket. |
| `--log-file` / `ATB_LOG` | same directory as socket: `atb.log` | Daemon log file. |
| `--log-level` | `info` | Log level for daemon: `debug`, `info`, `warn`, `error`. |

### `start` flags

| Flag | Default | Description |
|------|---------|-------------|
| `--host` | `0.0.0.0` | TCP listen host. |
| `--port` | `35555` | First TCP listen port to try. If occupied, walks upward. |
| `--server` | `127.0.0.1:5037` | Local adb server address. |
| `--backend` | `adb` | Target backend: `adb` or `hdc`. |
| `--hdc-server` | `127.0.0.1:8710` | HDC server address when `--backend hdc`. |
| `--auth` | `accept-all` | Auth mode: `accept-all` or `none`. |

Example — fixed port, debug logging via daemon:

```bash
./atb start --port 40000 --log-level debug <serial>
./atb logs -n 50
```

Expose an OpenHarmony device through an existing HDC server:

```bash
./atb start --backend hdc <hdc-target>
adb connect <bridge-host>:35555
adb -s <bridge-host>:35555 shell
```

For the HDC backend, `<hdc-target>` is a value from `hdc list targets` and is
passed to the HDC server as the same connect key used by `hdc -t <hdc-target>`.
If your HDC server only has one target, `any` is usually sufficient.

## More documentation

- [Usage guide](docs/usage.md)
- [Code architecture](docs/architecture.md)
- [Implementation details](docs/implementation.md)

## Scope

Implemented:

- Multi-device daemon + short-lived CLI over Unix domain socket control plane.
- Auto-start daemon on `start` / `list` / `status`; local log file + `atb logs`.
- ADB packet codec for `SYNC/CNXN/AUTH/OPEN/OKAY/WRTE/CLSE`.
- adb server host protocol framing: `4-hex length + command`.
- hdc server channel framing: `4-byte big-endian length + payload`.
- One adb server transport connection per opened ADB service.
- HDC backend translation for `shell:`/`exec:`, `sync:` push/pull,
  including recursive directory pull via `STAT`/`LIST` and file push through
  HDC native `FileInit/FileCheck/FileBegin/FileData/FileFinish` task frames,
  and device-local services (`localabstract:`/`localfilesystem:`/`localreserved:`/`tcp:`/`local:`) via HDC `fport`.
- `WRTE/OKAY` flow control for device-to-client data.
- `adb reverse` commands, including reverse connection data proxying back to
  the external adb client transport.
- Bridge-side auth that accepts any adb public key (no RSA verification).

Not implemented:

- Direct USB access (delegated to the local adb server).
- Wireless Debugging TLS transport (`A_STLS` / `STLS`).
- Full adb server host-side commands such as `host:devices`.
- RSA signature verification for bridge-side auth.
- Log rotation (v1 appends continuously).

## Development

```bash
make fmt        # go fmt ./...
make test       # go test ./...
make build      # go build -o atb ./src/cmd/adb-tcp-bridge
make release    # optimized build: -trimpath -ldflags "-s -w"
```

Cross-compile release binaries for Linux, macOS, and Windows (amd64 + arm64),
written to `dist/`:

```bash
make release-cross
```
