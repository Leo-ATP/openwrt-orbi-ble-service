//go:build !linux

// SPDX-License-Identifier: GPL-2.0-only

package main

import "errors"

func takeDaemonLock() (func(), error) {
	return nil, errors.New("the BLE runtime requires Linux")
}
