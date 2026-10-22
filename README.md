# Orbi BLE Service

Orbi BLE Service is an experimental, GPL-2.0-only BLE central backend for the
built-in controller in the Netgear Orbi RBR50 v1 and RBS50 v1. It is the
standalone upstream source for the backend proposed in
[openwrt/packages#30836](https://github.com/openwrt/packages/pull/30836).

The repository builds as a regular Go `main` package. It intentionally does not
contain the OpenWrt package Makefile, procd init script, rpcd wrapper layout,
D-Bus or BlueZ configuration, LuCI code, or feed directory structure. Those
integration files remain the responsibility of downstream packages.

## Supported hardware and scope

The runtime accepts only these exact device-tree board identities:

- `netgear,rbr50`
- `netgear,rbs50`

Both identify the v1 hardware path used by this project. Other Orbi models, v2
identities, USB adapters, arbitrary HCI devices, and generic BlueZ management
are outside the project scope.

The controller UART must be enabled by the running device tree at
`/soc/serial@78b0000` with:

- compatible `qcom,msm-uartdm-v1.4`;
- registers `0x078b0000` / `0x200`;
- an `msm_serial` TTY descendant;
- GPIO8-11 assigned by the board device tree; and
- the standard `hci_uart` BCSP transport with `hciextn=Y`.

The service discovers the TTY and HCI relationship through sysfs. It does not
assume `ttyMSM1` or `hci0`, register a UART at runtime, rebind the UART, alter a
console, or touch GPIO48.

Historical prototypes ran on RBR50 v1 and RBS50 v1 with OpenWrt 25.12.5 and
Linux 6.12.94. Those tests used the prototype's legacy runtime UART module.
They do not establish that this repository, its downstream package, or every
OpenWrt version has passed hardware validation. Deployments must be revalidated
with the UART enabled by the target device tree.

## RPC contract

The executable implements rpcd's exec protocol and exposes exactly six public
methods:

| Method | Parameters | Purpose |
| --- | --- | --- |
| `status` | `{}` | Return lifecycle, adapter, and named-device state |
| `start` | `{}` | Ask the downstream init service to start the daemon |
| `stop` | `{}` | Request an owned shutdown, then use the init service if needed |
| `scan` | `{"seconds": 5}` | Run an LE scan for 5 through 10 seconds |
| `connect` | `{"address": "02:00:00:00:00:01"}` | Connect and wait for services |
| `disconnect` | `{"address": "02:00:00:00:00:01"}` | Disconnect a selected device |

`ubus_rpc_session` metadata is accepted. Unknown fields, duplicate fields,
non-object input, trailing JSON, incomplete addresses, and scan durations
outside 5 through 10 seconds are rejected. Lowercase addresses are normalized
to uppercase.

The lifecycle phases are `stopped`, `starting`, `ready`, `stopping`, and
`failed`. A successful `start` can still report `starting`, and `stop` normally
reports `stopping` before teardown completes. Callers must inspect
`state.phase`; `ok` alone does not imply readiness.

The current presentation policy returns only devices with a non-empty BlueZ
`Alias` that is not just separators and does not normalize to the device's own
address. This policy is intentionally preserved from the prototype and may
hide legitimate unnamed peripherals.

## Controller initialization and ownership

Every Start performs the preserved CSR sequence:

1. Attach the device-tree UART through foreground `hciattach` using BCSP.
2. Read `PSKEY_ANA_FREQ` (`0x01fe`) as an identity and response gate.
3. Write exactly one value of 26000 kHz to PSRAM store 8.
4. Read the value back, dispatch a warm reset, detach, and reattach the same
   TTY.
5. Read the value again after reset and bring the controller down before BlueZ
   starts.

The pending marker under `/var/run/ble-service` is retained after an incomplete
mutation and blocks another attempt until reboot. The service never writes
EEPROM, ART, calibration data, bootloader state, factory addresses, or pairing
keys.

Runtime ownership is deliberately exclusive:

- no other `bluetoothd` or HCI controller may be present;
- the foreground `hciattach`, service-owned isolated `dbus-daemon`, and
  service-owned `bluetoothd` are direct children;
- `/var/lib/bluetooth` must be absent or carry the service ownership marker;
- the control socket is root-only and local, with no TCP listener; and
- changed ownership or incomplete shutdown is an error, not permission to
  terminate another process.

The backend does not provide pairing, trust, an application-specific GATT API,
automatic scanning, automatic reconnect, anonymous APIs, automatic peripheral
workflows, deployment tools, device credentials, private calibration, or
captured broadcaster data. Unit tests use only synthetic locally administered
Bluetooth addresses and generic device names.

## Runtime dependencies

Building requires Go 1.24 or newer. The only Go module dependency is
`github.com/godbus/dbus/v5` v5.1.0, pinned by `go.mod` and `go.sum`.

Hardware use additionally requires Linux/OpenWrt and downstream integration
that provides:

- `hci_uart` with BCSP and vendor extensions;
- `hciattach`, `hciconfig`, and `hcitool`;
- `dbus-daemon` and `bluetoothd`;
- a root-only init service at `/etc/init.d/ble`; and
- isolated D-Bus and BlueZ configuration under `/usr/share/ble-service`.

This repository does not install or configure those components and does not
disable an existing Bluetooth service.

## Build and test

No Bluetooth hardware is used by the test suite:

```sh
go mod download
gofmt -l *.go
go test -mod=readonly -race -count=1 -timeout=90s ./...
go vet -mod=readonly ./...
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 \
  go build -mod=readonly -trimpath -buildvcs=false -o ble-service .
```

Linux-specific tests use isolated temporary sysfs/process fixtures. The
isolated D-Bus integration test requires `dbus-daemon` to be available. A
successful Go cross-build is not an OpenWrt SDK build or hardware validation.

## License

Project-owned source files are licensed under GPL-2.0-only and carry matching
SPDX identifiers. The complete license text is in [LICENSE](LICENSE).

`github.com/godbus/dbus/v5` remains under its upstream BSD-2-Clause license.
Its copyright and license notice are retained verbatim in
[LICENSE.godbus](LICENSE.godbus); the dependency is not vendored or relicensed.
