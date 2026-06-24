# adb-tcp-bridge

`adb-tcp-bridge` exposes a USB-connected Android device as an ADB-over-TCP
endpoint. External adb clients connect to the bridge over TCP; the bridge
translates device-side ADB packets into the local adb server host protocol and
forwards them through the local adb server to the device over USB.

The bridge never touches USB directly — the local adb server (usually
`127.0.0.1:5037`) keeps owning the real USB transport.

```text
external adb client
  --TCP--> adb-tcp-bridge
    --host protocol--> local adb server
      --USB--> device adbd
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
adbb [flags] <serial>
```

`<serial>` is the serial reported by `adb devices` for the USB-connected
device. Exactly one positional argument is expected.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-host` | `0.0.0.0` | TCP listen host. |
| `-port` | `35555` | First TCP listen port to try. If occupied, the bridge walks upward until a free port is found. |
| `-server` | `127.0.0.1:5037` | Local adb server address. |
| `-auth` | `accept-all` | Auth mode: `accept-all` (accept any adb public key) or `none` (skip the auth handshake). |
| `-log-level` | `info` | Log level: `debug`, `info`, `warn`, `error`. |

Example — listen on a fixed port and forward through a non-default adb server:

```bash
./adbb -port 40000 -server 127.0.0.1:5037 -log-level debug <serial>
```

## Scope

Implemented:

- ADB packet codec for `SYNC/CNXN/AUTH/OPEN/OKAY/WRTE/CLSE`.
- adb server host protocol framing: `4-hex length + command`.
- One adb server transport connection per opened ADB service.
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
