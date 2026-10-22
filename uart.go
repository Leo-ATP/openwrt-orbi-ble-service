// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	hciPattern = regexp.MustCompile(`^hci[0-9]+$`)
	ttyPattern = regexp.MustCompile(`^ttyMSM[0-9]+$`)
)

type uart struct {
	name   string
	path   string
	device string
	info   os.FileInfo
}

func beneath(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(relative)
}

func hasCompatible(raw []byte, expected string) bool {
	for _, item := range strings.Split(string(raw), "\x00") {
		if item == expected {
			return true
		}
	}
	return false
}

func discoverUART(sysfs string) (*uart, error) {
	tree := filepath.Join(sysfs, "firmware", "devicetree", "base")
	board, err := os.ReadFile(filepath.Join(tree, "compatible"))
	if err != nil {
		return nil, err
	}
	if !hasCompatible(board, "netgear,rbr50") && !hasCompatible(board, "netgear,rbs50") {
		return nil, errors.New("this candidate supports only the Orbi RBR50/RBS50 v1 built-in controller")
	}
	node, err := filepath.EvalSymlinks(filepath.Join(tree, "soc", "serial@78b0000"))
	if err != nil {
		return nil, fmt.Errorf("find device-tree Bluetooth UART: %w", err)
	}
	compatible, err := os.ReadFile(filepath.Join(node, "compatible"))
	if err != nil || !hasCompatible(compatible, "qcom,msm-uartdm-v1.4") {
		return nil, errors.New("Bluetooth UART has an unexpected device-tree compatible")
	}
	reg, err := os.ReadFile(filepath.Join(node, "reg"))
	if err != nil || len(reg) != 8 || binary.BigEndian.Uint32(reg) != 0x078b0000 ||
		binary.BigEndian.Uint32(reg[4:]) != 0x200 {
		return nil, errors.New("Bluetooth UART has unexpected device-tree registers")
	}
	status, err := os.ReadFile(filepath.Join(node, "status"))
	if err != nil || (strings.TrimRight(string(status), "\x00") != "okay" &&
		strings.TrimRight(string(status), "\x00") != "ok") {
		return nil, errors.New("Bluetooth UART is not enabled by the device tree")
	}
	entries, err := filepath.Glob(filepath.Join(sysfs, "class", "tty", "ttyMSM*"))
	if err != nil {
		return nil, err
	}
	var selected *uart
	for _, entry := range entries {
		name := filepath.Base(entry)
		if !ttyPattern.MatchString(name) {
			continue
		}
		port, err := filepath.EvalSymlinks(entry)
		if err != nil {
			return nil, err
		}
		device, err := filepath.EvalSymlinks(filepath.Join(entry, "device"))
		if err != nil {
			return nil, err
		}
		for parent := device; beneath(parent, sysfs); parent = filepath.Dir(parent) {
			ofNode, err := filepath.EvalSymlinks(filepath.Join(parent, "of_node"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if ofNode != node {
				break
			}
			driver, err := filepath.EvalSymlinks(filepath.Join(parent, "driver"))
			if err != nil || filepath.Base(driver) != "msm_serial" {
				return nil, errors.New("Bluetooth UART is not bound to msm_serial")
			}
			if !beneath(port, parent) {
				return nil, errors.New("Bluetooth TTY does not belong to its platform device")
			}
			if selected != nil {
				return nil, errors.New("Bluetooth UART has multiple TTY candidates")
			}
			info, err := os.Stat(port)
			if err != nil {
				return nil, err
			}
			selected = &uart{name: name, path: port, device: parent, info: info}
			break
		}
	}
	if selected == nil {
		return nil, errors.New("enabled Bluetooth UART has no unique ttyMSM device")
	}
	return selected, nil
}

func (u *uart) verify(sysfs string) error {
	current, err := discoverUART(sysfs)
	if err != nil {
		return err
	}
	if current.name != u.name || current.path != u.path || current.device != u.device ||
		!os.SameFile(current.info, u.info) {
		return errors.New("Bluetooth UART identity changed")
	}
	return nil
}

func hciEntries(sysfs string) ([]string, error) {
	entries, err := filepath.Glob(filepath.Join(sysfs, "class", "bluetooth", "hci*"))
	if err != nil {
		return nil, err
	}
	var result []string
	for _, entry := range entries {
		if hciPattern.MatchString(filepath.Base(entry)) {
			result = append(result, entry)
		}
	}
	return result, nil
}

func hciForUART(sysfs string, u *uart) (string, os.FileInfo, error) {
	entries, err := hciEntries(sysfs)
	if err != nil {
		return "", nil, err
	}
	var found string
	var info os.FileInfo
	for _, entry := range entries {
		path, err := filepath.EvalSymlinks(entry)
		if err != nil {
			return "", nil, err
		}
		if !beneath(path, u.path) {
			return "", nil, errors.New("another Bluetooth controller is present")
		}
		if found != "" {
			return "", nil, errors.New("multiple HCI controllers belong to the UART")
		}
		info, err = os.Stat(entry)
		if err != nil {
			return "", nil, err
		}
		found = filepath.Base(entry)
	}
	return found, info, nil
}

type controllerTransport interface {
	attach(context.Context) error
	detach(context.Context) error
	verify() error
	controller() string
	tty() string
	exited() <-chan struct{}
}

type uartTransport struct {
	mu      sync.RWMutex
	uart    *uart
	sysfs   string
	dev     string
	proc    string
	child   *child
	hci     string
	hciInfo os.FileInfo
}

func (t *uartTransport) controller() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.hci
}

func (t *uartTransport) tty() string { return t.uart.name }

func (t *uartTransport) exited() <-chan struct{} {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.child == nil {
		return nil
	}
	return t.child.done
}

func (t *uartTransport) attach(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.child != nil {
		return errors.New("an HCI attachment is already owned")
	}
	if err := t.uart.verify(t.sysfs); err != nil {
		return err
	}
	entries, err := hciEntries(t.sysfs)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("another Bluetooth controller is present; refusing attachment")
	}
	consoles, err := os.ReadFile(filepath.Join(t.proc, "consoles"))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(consoles), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 0 && fields[0] == t.uart.name {
			return errors.New("Bluetooth UART is a console; refusing attachment")
		}
	}
	device := filepath.Join(t.dev, t.uart.name)
	info, err := os.Stat(device)
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return errors.New("Bluetooth UART character device is unavailable")
	}
	if err := refuseTTYUsers(t.proc, device); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(t.sysfs, "module", "hci_uart")); errors.Is(err, os.ErrNotExist) {
		if _, err := command(ctx, "/sbin/modprobe", "hci_uart"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// Keep the foreground process as our child, rather than trusting a PID file.
	t.child, err = startChild("/usr/bin/hciattach", "-n", "-t", "10", "-s", "115200",
		device, "bcsp", "115200", "noflow")
	if err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		t.hci, t.hciInfo, err = hciForUART(t.sysfs, t.uart)
		if err != nil {
			return err
		}
		if t.hci != "" {
			return t.verifyLocked()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.child.done:
			return fmt.Errorf("hciattach exited during attachment: %v", t.child.err)
		case <-ticker.C:
		}
	}
}

func refuseTTYUsers(proc, device string) error {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !numericPID(entry.Name()) {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(proc, entry.Name(), "fd"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(proc, entry.Name(), "fd", fd.Name()))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if target == device {
				return errors.New("Bluetooth UART is already open by another process")
			}
		}
	}
	return nil
}

func (t *uartTransport) verify() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.verifyLocked()
}

func (t *uartTransport) verifyLocked() error {
	if t.child == nil || !t.child.running() || t.hci == "" || t.hciInfo == nil {
		return errors.New("owned hciattach is not running")
	}
	if err := t.uart.verify(t.sysfs); err != nil {
		return err
	}
	hci, info, err := hciForUART(t.sysfs, t.uart)
	if err != nil {
		return err
	}
	if hci != t.hci || info == nil || !os.SameFile(info, t.hciInfo) {
		return errors.New("HCI controller ownership changed")
	}
	return nil
}

func (t *uartTransport) detach(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.child == nil {
		return nil
	}
	if err := t.uart.verify(t.sysfs); err != nil {
		return err
	}
	hci, info, err := hciForUART(t.sysfs, t.uart)
	if err != nil {
		return err
	}
	if hci != "" && t.hci != "" &&
		(hci != t.hci || info == nil || !os.SameFile(info, t.hciInfo)) {
		return errors.New("HCI ownership changed; refusing to detach another owner")
	}
	if err := t.child.stop(); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		hci, _, err := hciForUART(t.sysfs, t.uart)
		if err != nil {
			return err
		}
		if hci == "" {
			t.child, t.hci, t.hciInfo = nil, "", nil
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("owned HCI did not disappear: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
