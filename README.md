# Orbi Bluetooth Bring-up Helper

This repository contains the smallest custom component still required to expose
the built-in CSR controller in the Netgear Orbi RBR50 v1 and RBS50 v1 to the
standard Linux Bluetooth stack. Version `0.2.0` is a breaking reduction from
the former BLE service.

The helper does **not** provide an rpcd or ubus API, scan for devices, connect
or disconnect peripherals, filter names, run D-Bus, manage `bluetoothd`, or
replace BlueZ storage and policy. Standard BlueZ already provides those
features through `bluetoothd`, `bluetoothctl`, Adapter1, and Device1.

## Why a helper is still required

Current upstream and OpenWrt tools cover most of the bring-up path:

- BlueZ `hciattach` supports BCSP at 115200 baud without hardware flow
  control. Its BCSP initializer explicitly enables even parity and performs
  the BCSP synchronization handshake.
- OpenWrt `procd` can supervise a foreground `hciattach`, and the stock
  `bluetoothd` service can manage an adapter that appears after daemon start.
- BlueZ and the kernel provide the normal HCI, D-Bus, discovery, connection,
  pairing, and storage layers.

They do not cover the complete Orbi-specific prerequisite:

- BlueZ `btattach` starts from `cfmakeraw()` and exposes flow-control and speed
  options, but no even-parity option required by this controller.
- BlueZ removed `bccmd` and its CSR transport implementation in 2020; BlueZ
  5.87 and OpenWrt's `bluez-utils` package do not build or install it.
- Deprecated `hcitool cmd` can submit a raw vendor command, but it reads the
  first HCI event and does not correlate the CSR sequence, variable, key,
  store, status, or returned payload. It also prints its command text before
  the send call, so that text cannot prove warm-reset dispatch. This helper
  writes the HCI raw socket directly and validates all required fields.
- The kernel BCSP implementation is a line-discipline protocol and has no
  generic CSR BCSP serdev or device-tree driver that performs this board's
  volatile PSRAM initialization.
- The UART has no stable serial alias. It must be selected through the exact
  board, device-tree, platform, serial-base, TTY, and HCI ancestry.

Reintroducing the removed CSR utility suite would add thousands of lines of
unmaintained transport and diagnostic code. This helper retains only the
verified UART identity checks and the single required CSR transaction.

The source evidence used for this reduction is pinned to:

- BlueZ
  [`a0580dad`](https://github.com/bluez/bluez/tree/a0580dad5655e86314a0a777eb5f602c237a7e19)
  for `hciattach`, `btattach`, and HCI socket behavior;
- BlueZ
  [`e2e198ce`](https://github.com/bluez/bluez/commit/e2e198ced468122df7b431cb06158123ba60a2f2)
  for removal of the legacy CSR utility suite;
- OpenWrt packages
  [`aca5ca2f`](https://github.com/openwrt/packages/tree/aca5ca2ffb64046b9ec5ca139176e0b2673999fc)
  for BlueZ 5.87 packaging; and
- OpenWrt
  [`b5bc4bec`](https://github.com/openwrt/openwrt/tree/b5bc4becb424b4d89f48e372675811821aa155bd)
  for `bluetooth-uart`, procd, and HCI UART kernel packaging.

## Exact hardware boundary

The helper accepts only these exact board compatibles:

- `netgear,rbr50`
- `netgear,rbs50`

It requires the enabled `/soc/serial@78b0000` node with compatible
`qcom,msm-uartdm-v1.4`, registers `0x078b0000` / `0x200`, and a unique
`ttyMSM*` descendant. It accepts either:

- the older direct `platform/msm_serial -> tty` hierarchy; or
- Linux 6.18's exact
  `platform/msm_serial -> serial-base/ctrl -> serial-base/port -> tty`
  hierarchy.

Every layer must retain the same device-tree node and exact subsystem and
driver identity. Multiple HCI descendants of the target UART, ambiguous TTYs,
changed symlinks, consoles, open TTYs, unsupported boards, disabled nodes, and
unexpected ancestry fail closed. Unrelated HCI controllers are outside this
helper's scope and do not block the target.

## Commands

Build the executable as `orbi-bt-setup`:

```sh
go build -trimpath -buildvcs=false -o orbi-bt-setup .
```

The command surface is deliberately limited to four package-integration steps:

```text
orbi-bt-setup tty
orbi-bt-setup hci
orbi-bt-setup prepare
orbi-bt-setup verify
```

- `tty` verifies the exact board and unused UART, then prints its `/dev` path.
- `hci` verifies that exactly one HCI controller is attached below that UART
  and prints its name.
- `prepare` performs the initial ANA_FREQ GET, creates the volatile pending
  marker, writes 26000 kHz exactly once to PSRAM store 8, verifies the
  readback, and dispatches the warm reset.
- `verify` is called after the package reattaches the same TTY. It verifies the
  saved UART identity, performs the post-reset GET, brings the HCI down for
  standard BlueZ, and clears the pending marker.

`prepare` and `verify` never start, stop, or supervise `hciattach`,
`bluetoothd`, D-Bus, or any peripheral operation.

## OpenWrt integration contract

The downstream package owns process orchestration. Its synchronous procd
pre-start must:

1. Run `orbi-bt-setup tty` and retain the returned TTY path.
2. Start a temporary foreground attachment:

   ```sh
   /usr/bin/hciattach -n -t 10 -s 115200 \
     "$tty" bcsp 115200 noflow
   ```

3. Wait until `orbi-bt-setup hci` succeeds, then run
   `orbi-bt-setup prepare`.
4. Terminate and reap only that temporary `hciattach`; wait until its HCI
   disappears.
5. Reattach the same TTY with the same command, wait for `hci`, then run
   `orbi-bt-setup verify`.
6. Terminate and reap that verified temporary attachment and wait for its HCI
   to disappear.
7. Register the final foreground `hciattach` command as the procd-supervised
   instance. The stock `bluetoothd` service may then start normally and will
   handle the final adapter through its standard management interface.

The pre-start must not retry after `prepare` has created
`/var/run/orbi-bt-setup/ana-freq.pending`. A failure after that point leaves
the marker in volatile storage and requires a reboot before another attempt.
Before the marker exists, the package may clean up its own temporary
`hciattach` and retry explicitly.

At boot, the setup service must run before the stock `bluetoothd` init script.
For any manual rerun, the package or administrator must first stop
`bluetoothd` and keep it stopped until `verify` completes and the temporary HCI
has disappeared. The helper deliberately does not stop or restart the daemon.

Required runtime components are:

- `kmod-hci-uart` built with BCSP and `hciextn=Y`;
- `bluez-utils` for `hciattach` and `hciconfig`;
- the stock `bluez-daemon`, D-Bus, and procd services.

No rpcd ACL, LuCI backend, private D-Bus, private `bluetoothd`, or replacement
BlueZ storage belongs in this package.

## CSR transaction

The retained transaction is intentionally narrow:

1. Bring the verified HCI up.
2. GET `PSKEY_ANA_FREQ` (`0x01fe`) through variable `0x7003`.
3. Record the pending marker.
4. SET 26000 kHz in PSRAM store 8.
5. GET and verify the value.
6. Dispatch variable `0x4002` for a warm reset.
7. After the downstream package reattaches the same TTY, GET and verify the
   value again.
8. Bring the HCI down and clear the marker.

The helper requires a complete successful raw-socket write before treating a
command as dispatched, then checks the CSR response shape, type, length,
sequence, variable, status, key, word count, store, and value. It does not
access EEPROM, ART, calibration, bootloader data, factory addresses, pairing
keys, or application GATT data.

## Validation

```sh
gofmt -l *.go
go mod verify
go mod tidy -diff
go test -mod=readonly -race -count=1 -timeout=90s ./...
go vet -mod=readonly ./...
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 \
  go build -mod=readonly -trimpath -buildvcs=false -o orbi-bt-setup .
```

The tests use temporary sysfs and process fixtures and synthetic HCI names.
They do not access Bluetooth hardware.

The RBR50/RBS50 hardware evidence for versions 0.1.0 and 0.1.1 validates the
UART discovery, CSR PDU, ANA_FREQ value and store, warm reset, same-TTY
reattach, and post-GET requirements. Version 0.2.0 replaces the deprecated
`hcitool` subprocess with a direct HCI raw socket and changes process
orchestration by handing a final third attachment to stock `bluetoothd`.
These changes require targeted device validation before claiming a complete
0.2.0 runtime pass.

## License

All source in this repository is GPL-2.0-only and carries matching SPDX
identifiers. The complete license text is in [LICENSE](LICENSE). Version 0.2.0
has no third-party Go module dependencies.
