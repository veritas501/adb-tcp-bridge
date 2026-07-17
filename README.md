# adb-tcp-bridge

`adb-tcp-bridge` exposes an ADB- or HDC-connected device as an ADB-over-TCP
endpoint. External adb clients connect to the bridge over TCP; the bridge
translates device-side ADB packets into either the local adb server host
protocol or the HDC host/server protocol.

The bridge never touches USB directly — the local adb server (usually
`127.0.0.1:5037`) keeps owning the real USB transport.

```text
external adb client
  --TCP--> adb-tcp-bridge
    --adb host protocol--> local adb server --> Android adbd
    --hdc host protocol--> hdc server       --> OpenHarmony hdcd
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
  local adb server reachable at the address passed via `-server`.
- **One USB-connected Android device** already visible to the local adb server.

Start the local adb server and confirm the device is attached before launching
the bridge:

```bash
adb start-server
adb devices   # copy the <serial> of your USB device
```

## Quickstart

```bash
make release                 # builds ./adbb (stripped, trimmed)
./adbb <serial>              # listens on 0.0.0.0:35555 by default
```

From another machine (or the same one), connect a regular adb client to the
bridge:

```bash
adb connect <bridge-host>:35555
adb devices
```

You should see the bridge log a `listening` line reporting the actual
`listen_addr`, then `connect from host` as the adb client attaches. After
`adb connect`, `adb devices` lists the device as `<bridge-host>:35555  device`
and you can run `adb shell`, `adb push`, etc. against it.

> If `35555` is already in use, the bridge automatically tries the next port
> upward and logs the real `listen_addr` — use that port in `adb connect`.

## Usage

```
adbb [flags] <serial|connect-key>
```

For the ADB backend, `<serial>` is the serial reported by `adb devices` for the
USB-connected Android device. For the HDC backend, `<connect-key>` is the target
reported by `hdc list targets` and accepted by `hdc -t`. Exactly one positional
argument is expected.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-host` | `0.0.0.0` | TCP listen host. |
| `-port` | `35555` | First TCP listen port to try. If occupied, the bridge walks upward until a free port is found. |
| `-server` | `127.0.0.1:5037` | Local adb server address. |
| `-backend` | `adb` | Target backend: `adb` or `hdc`. |
| `-hdc-server` | `127.0.0.1:8710` | HDC server address when `-backend hdc`. |
| `-auth` | `accept-all` | Auth mode: `accept-all` (accept any adb public key) or `none` (skip the auth handshake). |
| `-log-level` | `info` | Log level: `debug`, `info`, `warn`, `error`. |

Example — listen on a fixed port and forward through a non-default adb server:

```bash
./adbb -port 40000 -server 127.0.0.1:5037 -log-level debug <serial>
```

Expose an OpenHarmony device through an existing HDC server:

```bash
./adbb -backend hdc <hdc-target>
adb connect <bridge-host>:35555
adb -s <bridge-host>:35555 shell
```

For the HDC backend, `<hdc-target>` is a value from `hdc list targets` and is
passed to the HDC server as the same connect key used by `hdc -t <hdc-target>`.
If your HDC server only has one target, `any` is usually sufficient.
`-hdc-server` only needs to be set when your HDC server is not listening on the
default `127.0.0.1:8710`.

## Scope

Implemented:

- ADB packet codec for `SYNC/CNXN/AUTH/OPEN/OKAY/WRTE/CLSE`.
- adb server host protocol framing: `4-hex length + command`.
- hdc server channel framing: `4-byte big-endian length + payload`.
- One adb server transport connection per opened ADB service.
- HDC backend translation for `shell:`/`exec:` and `sync:` push/pull,
  including recursive directory pull via `STAT`/`LIST` and file push through
  HDC native `FileInit/FileCheck/FileBegin/FileData/FileFinish` task frames.
- `WRTE/OKAY` flow control for device-to-client data.
- `adb reverse` commands, including reverse connection data proxying back to
  the external adb client transport.
- Bridge-side auth that accepts any adb public key (no RSA verification).

Not implemented:

- Direct USB access (delegated to the local adb server).
- Wireless Debugging TLS transport (`A_STLS` / `STLS`).
- Full adb server host-side commands such as `host:devices`.
- RSA signature verification for bridge-side auth.

## Development

```bash
make fmt        # go fmt ./...
make test       # go test ./...
make build      # go build -o adbb ./src/cmd/adb-tcp-bridge
make release    # optimized build: -trimpath -ldflags "-s -w"
```

Cross-compile release binaries for Linux, macOS, and Windows (amd64 + arm64),
written to `dist/`:

```bash
make release-cross
```
