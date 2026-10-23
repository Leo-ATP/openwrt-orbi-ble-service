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

func deviceTreeNode(path string) (string, bool, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	} else if err != nil {
		return "", false, err
	}
	node, err := filepath.EvalSymlinks(path)
	return node, true, err
}

func uartPlatformDevice(sysfs, node, port, device string) (string, bool, error) {
	if !beneath(port, sysfs) || !beneath(device, sysfs) {
		return "", true, errors.New("Bluetooth TTY device escapes sysfs")
	}
	ofNode, exists, err := deviceTreeNode(filepath.Join(device, "of_node"))
	if !exists {
		return "", false, nil
	}
	if err != nil {
		return "", true, fmt.Errorf("resolve Bluetooth UART device-tree identity: %w", err)
	}
	if ofNode != node {
		return "", false, nil
	}

	platformDriver := filepath.Join(sysfs, "bus", "platform", "drivers", "msm_serial")
	portDriver := filepath.Join(sysfs, "bus", "serial-base", "drivers", "port")
	ctrlDriver := filepath.Join(sysfs, "bus", "serial-base", "drivers", "ctrl")
	platformSubsystem := filepath.Join(sysfs, "bus", "platform")
	serialSubsystem := filepath.Join(sysfs, "bus", "serial-base")
	platformDevices := filepath.Join(sysfs, "devices", "platform")
	seen := map[string]bool{}
	for layer := 0; layer < 3; layer++ {
		if seen[device] {
			return "", true, errors.New("Bluetooth UART ancestry contains a loop")
		}
		seen[device] = true
		if layer != 0 {
			ofNode, exists, err = deviceTreeNode(filepath.Join(device, "of_node"))
			if !exists {
				return "", true, errors.New("Bluetooth UART parent lacks a device-tree identity")
			}
			if err != nil {
				return "", true, fmt.Errorf("resolve Bluetooth UART parent device-tree identity: %w", err)
			}
			if ofNode != node {
				return "", true, errors.New("Bluetooth UART ancestry changed device-tree identity before msm_serial")
			}
		}
		driver, err := filepath.EvalSymlinks(filepath.Join(device, "driver"))
		if err != nil {
			return "", true, fmt.Errorf("resolve Bluetooth UART driver for %s: %w",
				filepath.Base(device), err)
		}
		subsystem, err := filepath.EvalSymlinks(filepath.Join(device, "subsystem"))
		if err != nil {
			return "", true, fmt.Errorf("resolve Bluetooth UART subsystem for %s: %w",
				filepath.Base(device), err)
		}
		switch layer {
		case 0:
			if driver == platformDriver {
				if subsystem != platformSubsystem {
					return "", true, fmt.Errorf(
						"Bluetooth UART msm_serial owner has unexpected subsystem %q", subsystem)
				}
				if !beneath(device, platformDevices) {
					return "", true, errors.New("Bluetooth UART msm_serial owner is not a platform device")
				}
				if !beneath(port, device) {
					return "", true, errors.New("Bluetooth TTY does not belong to its platform device")
				}
				return device, true, nil
			}
			if driver != portDriver {
				return "", true, fmt.Errorf("Bluetooth UART has unexpected first-level driver %q", driver)
			}
			if subsystem != serialSubsystem {
				return "", true, fmt.Errorf(
					"Bluetooth UART port has unexpected subsystem %q", subsystem)
			}
		case 1:
			if driver != ctrlDriver {
				return "", true, fmt.Errorf(
					"Bluetooth UART serial-base port is not parented by the ctrl driver: %q", driver)
			}
			if subsystem != serialSubsystem {
				return "", true, fmt.Errorf(
					"Bluetooth UART ctrl has unexpected subsystem %q", subsystem)
			}
		case 2:
			if driver != platformDriver {
				return "", true, fmt.Errorf(
					"Bluetooth UART serial-base ctrl is not parented by msm_serial: %q", driver)
			}
			if subsystem != platformSubsystem {
				return "", true, fmt.Errorf(
					"Bluetooth UART msm_serial owner has unexpected subsystem %q", subsystem)
			}
			if !beneath(device, platformDevices) {
				return "", true, errors.New("Bluetooth UART msm_serial owner is not a platform device")
			}
			if !beneath(port, device) {
				return "", true, errors.New("Bluetooth TTY does not belong to its platform device")
			}
			return device, true, nil
		}
		parent := filepath.Dir(device)
		if parent == device || !beneath(parent, sysfs) {
			return "", true, errors.New("Bluetooth UART ancestry left sysfs before reaching msm_serial")
		}
		device = parent
	}
	return "", true, errors.New("Bluetooth UART ancestry did not reach msm_serial")
}

func discoverUART(sysfs string) (*uart, error) {
	sysfs, err := filepath.EvalSymlinks(sysfs)
	if err != nil {
		return nil, fmt.Errorf("resolve sysfs root: %w", err)
	}
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
			return nil, fmt.Errorf("resolve Bluetooth TTY device: %w", err)
		}
		platform, matches, err := uartPlatformDevice(sysfs, node, port, device)
		if err != nil {
			return nil, err
		}
		if !matches {
			continue
		}
		if selected != nil {
			return nil, errors.New("Bluetooth UART has multiple TTY candidates")
		}
		info, err := os.Stat(port)
		if err != nil {
			return nil, err
		}
		selected = &uart{name: name, path: port, device: platform, info: info}
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
