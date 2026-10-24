//go:build linux

// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func takeSetupLock(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another Orbi Bluetooth setup command is running: %w", err)
	}
	return func() { file.Close() }, nil
}

func fileIdentity(info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("UART file identity is unavailable")
	}
	return uint64(stat.Dev), stat.Ino, nil
}

func linuxDeviceNumbers(device uint64) (uint64, uint64) {
	major := (device >> 8) & 0xfff
	major |= (device >> 32) &^ 0xfff
	minor := device & 0xff
	minor |= (device >> 12) &^ 0xff
	return major, minor
}

func characterDeviceInfo(info os.FileInfo) (uint64, uint64, uint64, error) {
	if info.Mode()&os.ModeCharDevice == 0 {
		return 0, 0, 0, errors.New("path is not a character device")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, errors.New("character device identity is unavailable")
	}
	device := uint64(stat.Rdev)
	major, minor := linuxDeviceNumbers(device)
	return major, minor, device, nil
}

func characterDeviceIdentity(path string) (uint64, uint64, uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, 0, 0, errors.New("refusing symlink Bluetooth UART device")
	}
	return characterDeviceInfo(info)
}
