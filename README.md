# Orbi Bluetooth Bring-up Helper

`orbi-bt-setup` is the minimal hardware bring-up helper required for the
built-in CSR controller in the Netgear Orbi RBR50 v1 and RBS50 v1. Version
`0.3.0` is a GPL-2.0-only C rewrite with no Go toolchain, rpcd, ubus, D-Bus, or
third-party runtime dependency.

The helper does not scan, connect, pair, filter devices, manage BlueZ storage,
or run `bluetoothd`. Standard BlueZ owns all normal adapter and peripheral
behavior. The downstream OpenWrt package owns `hciattach` and procd
orchestration.

## Why any custom code remains

Current standard components cover most of the path:

- BlueZ `hciattach` supports BCSP at 115200 baud without hardware flow
  control. Its BCSP initializer enables even parity and performs the link
  synchronization handshake.
- OpenWrt procd can supervise the final foreground `hciattach`.
- The stock `bluetoothd` provides the standard management, D-Bus, discovery,
  connection, pairing, and storage layers.

They do not cover the complete Orbi-specific prerequisite:

- `btattach` starts from raw termios and has no even-parity option.
- BlueZ removed `bccmd` and the legacy CSR transport suite in 2020; BlueZ 5.87
  and OpenWrt do not install it.
- Deprecated `hcitool cmd` prints its command before the send call, reads only
  the first event, and cannot reliably correlate the complete CSR response.
- Linux BCSP provides the transport but no generic device-tree setup driver for
  this board's volatile PSRAM initialization.
- The UART has no stable alias and must be proven through the exact
  device-tree, platform, serial-base, TTY, and HCI ancestry.

The source evidence is pinned to:

- BlueZ
  [`a0580dad`](https://github.com/bluez/bluez/tree/a0580dad5655e86314a0a777eb5f602c237a7e19);
- the BlueZ CSR removal commit
  [`e2e198ce`](https://github.com/bluez/bluez/commit/e2e198ced468122df7b431cb06158123ba60a2f2);
- OpenWrt packages
  [`aca5ca2f`](https://github.com/openwrt/packages/tree/aca5ca2ffb64046b9ec5ca139176e0b2673999fc);
  and
- OpenWrt
  [`b5bc4bec`](https://github.com/openwrt/openwrt/tree/b5bc4becb424b4d89f48e372675811821aa155bd).

## Hardware boundary

The helper accepts only:

- `netgear,rbr50`
- `netgear,rbs50`

It requires the enabled `/soc/serial@78b0000` node with compatible
`qcom,msm-uartdm-v1.4`, registers `0x078b0000` / `0x200`, and one matching
`ttyMSM*` descendant. It accepts:

- legacy `platform/msm_serial -> tty`; or
- Linux 6.18
  `platform/msm_serial -> serial-base/ctrl -> serial-base/port -> tty`.

Every matched layer must retain the same device-tree node and exact driver and
subsystem identities. The `/dev` character device must be a direct node, not a
symlink, and its major/minor must match sysfs. Consoles and TTYs already open
by another process are rejected.

Only HCI controllers below the proven TTY are considered. Unrelated
controllers are ignored; zero or multiple matching controllers fail the
attached phases.

The helper opens filesystem roots and state with `openat`, `fstatat`,
`readlinkat`, `O_NOFOLLOW`, fixed-size buffers, and explicit path containment
checks.

## Commands

```text
orbi-bt-setup tty
orbi-bt-setup hci
orbi-bt-setup prepare
orbi-bt-setup verify
```

- `tty` proves the board and currently unused UART, then prints `/dev/ttyMSM*`.
- `hci` proves one attached HCI below that UART and prints its name.
- `prepare` performs the initial GET, records the pending mutation, SETs
  `PSKEY_ANA_FREQ` to 26000 kHz in PSRAM store 8, verifies the readback, and
  dispatches the warm reset.
- `verify` runs after the package reattaches the same TTY. It verifies the
  saved TTY identity, performs the post-reset GET, brings the HCI down, and
  clears the pending marker.

All commands take a non-blocking `flock`. The pending marker is a strict
versioned text record created with `O_EXCL`, `O_NOFOLLOW`, mode `0600`, and
`fsync`. It records canonical TTY/platform paths, filesystem device and inode,
major/minor, and the post-reset sequence. It is removed only after a successful
`verify`.

## CSR and HCI checks

The helper uses an `AF_BLUETOOTH` raw HCI socket directly. A complete socket
write is required before a command is considered dispatched. Vendor responses
must match the requested sequence and variable before the complete response is
checked.

For ANA_FREQ, the helper verifies:

- response type and word count;
- sequence and variable `0x7003`;
- key `0x01fe`;
- one-word value in store 8;
- SET response status and value;
- immediate readback status/value;
- post-reset readback status/value.

Warm reset uses variable `0x4002`. A bounded response timeout is accepted only
after the full command write succeeded; same-TTY reattachment and post-GET
remain mandatory.

Linux BCSP's `hciextn` parameter is defined in `hci_bcsp.c`, linked into
`hci_uart.ko`, defaults to true, and is exposed with mode `0644`. The helper
only verifies that `/sys/module/hci_uart/parameters/hciextn` reads `Y`. It
never changes the parameter.

No EEPROM, ART, calibration, bootloader, factory address, pairing key, or
application GATT data is accessed.

## OpenWrt integration contract

The package must run setup synchronously before registering the final procd
instance:

1. Acquire a separate package-level exclusive lock and retain it until both
   temporary attachments are gone and the final procd instance is registered.
   This outer lock is distinct from the helper's per-command lock.
2. Stop `bluetoothd`, disable any getty or other respawning owner of the target
   UART, then run `orbi-bt-setup tty` and retain the returned TTY.
3. Start a temporary child:

   ```sh
   /usr/bin/hciattach -n -t 10 -s 115200 \
     "$tty" bcsp 115200 noflow
   ```

4. Bounded-poll `orbi-bt-setup hci`, then run `prepare`.
5. Terminate and reap only that child; wait for its HCI to disappear.
6. Reattach the same TTY, bounded-poll `hci`, then run `verify`.
7. Terminate and reap only that child; wait for its HCI to disappear.
8. Register the final identical foreground `hciattach` command with procd,
   then release the package-level lock.
9. Start the stock `bluetoothd` normally; it may discover the final adapter
   after daemon start.

At boot, the setup service must start before `bluetoothd`. Before any manual
rerun, stop `bluetoothd` and keep it stopped until `verify` completes and the
temporary HCI disappears. A getty or other UART owner must be disabled, not
merely killed, so it cannot respawn between `tty` and `prepare`.

Before the marker exists, an explicit retry is possible after cleaning up the
package-owned child. After `prepare` creates the marker, any failure requires a
reboot; automatic retries are prohibited.

Runtime dependencies:

- `kmod-hci-uart` with `CONFIG_BT_HCIUART_BCSP=y` and `hciextn=Y`;
- `bluez-utils` for `hciattach` and `hciconfig`;
- stock `bluez-daemon`, D-Bus, and procd.

There is no rpcd or LuCI backend dependency.

## Build and test

Native build and tests:

```sh
make
make test
make sanitize
make analyze
```

The normal build enables:

```text
-Wall -Wextra -Werror -Wconversion -Wformat=2 -Wshadow
-Wstrict-prototypes -Wmissing-prototypes -Wpointer-arith
-Wcast-qual -Wwrite-strings -fstack-protector-strong
-fPIE -Wl,-z,relro,-z,now -pie
```

`make sanitize` runs the fixture and mutation suite with ASan and UBSan.
`make analyze` runs GCC `-fanalyzer`.

OpenWrt ARMv7 musl example:

```sh
make clean
make \
  CC="$TOOLCHAIN/bin/arm-openwrt-linux-muslgnueabi-gcc" \
  STRIP="$TOOLCHAIN/bin/arm-openwrt-linux-muslgnueabi-strip" \
  CFLAGS="-Os -pipe -mcpu=cortex-a7 -mfpu=neon-vfpv4 -mfloat-abi=hard" \
  all strip
```

Tests use temporary device-tree, sysfs, procfs, HCI, response mutation, and
pending-marker fixtures. They do not access Bluetooth hardware.

The RBR50 and RBS50 v0.1.1 evidence validates the common physical UART and CSR
requirements. The C implementation still requires one targeted device
validation, preferably the currently reachable RBR50, covering cold boot,
ANA_FREQ, same-TTY reattachment, stock BlueZ handoff, and controlled restart.
The other board is needed only if that test exposes a board-specific
difference.

## Source layout

```text
include/orbi_bt_setup.h
src/main.c
src/setup.c
src/sysfs.c
src/csr.c
tests/test_main.c
Makefile
LICENSE
```

## License

All source is GPL-2.0-only. The complete text is in [LICENSE](LICENSE).
