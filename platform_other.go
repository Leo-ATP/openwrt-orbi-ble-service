//go:build !linux

// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"errors"
	"os"
)

func takeSetupLock(string) (func(), error) {
	return nil, errors.New("Orbi Bluetooth setup requires Linux")
}

func fileIdentity(os.FileInfo) (uint64, uint64, error) {
	return 0, 0, errors.New("UART file identity requires Linux")
}

func characterDeviceInfo(os.FileInfo) (uint64, uint64, uint64, error) {
	return 0, 0, 0, errors.New("character device identity requires Linux")
}

func characterDeviceIdentity(string) (uint64, uint64, uint64, error) {
	return 0, 0, 0, errors.New("character device identity requires Linux")
}
