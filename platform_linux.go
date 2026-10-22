// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"fmt"
	"os"
	"syscall"
)

func takeDaemonLock() (func(), error) {
	f, err := os.OpenFile("/var/run/ble-service.lock",
		os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another BLE server owns the runtime: %w", err)
	}
	return func() { f.Close() }, nil
}
