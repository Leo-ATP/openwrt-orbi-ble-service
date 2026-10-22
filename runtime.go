// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	bluezStorageMarker   = ".ble-service-owner"
	bluezStorageIdentity = "ble-service-v1\n"
)

var bluezStorageDir = "/var/lib/bluetooth"

type child struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startChild(name string, args ...string) (*child, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "DBUS_SYSTEM_BUS_ADDRESS=unix:path="+busPath)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &child{cmd: cmd, done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

func (c *child) running() bool {
	if c == nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *child) stop() error {
	if !c.running() {
		return nil
	}
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-c.done:
		return nil
	case <-time.After(4 * time.Second):
		if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		select {
		case <-c.done:
			return nil
		case <-time.After(2 * time.Second):
			return errors.New("owned Bluetooth child did not exit")
		}
	}
}

func numericPID(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func refuseOtherBluetoothd(proc string) error {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !numericPID(entry.Name()) {
			continue
		}
		name, err := os.ReadFile(filepath.Join(proc, entry.Name(), "comm"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(name)) == "bluetoothd" {
			return errors.New("another bluetoothd owns the host; it will not be stopped or replaced")
		}
	}
	return nil
}

type runtime struct {
	transport  controllerTransport
	busChild   *child
	bluezChild *child
	bluez      *bluez
	storage    bool
	runCommand csrCommandRunner
}

func (r *runtime) run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "hciconfig" || name == "hcitool" {
		if r.transport == nil {
			return "", errors.New("no owned HCI transport")
		}
		if err := r.transport.verify(); err != nil {
			return "", err
		}
	}
	if r.runCommand != nil {
		return r.runCommand(ctx, name, args...)
	}
	return command(ctx, name, args...)
}

func (r *runtime) prepareStorage() error {
	info, err := os.Lstat(bluezStorageDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return errors.New("BlueZ storage must be a service-owned directory, not a symlink")
	default:
		raw, err := os.ReadFile(filepath.Join(bluezStorageDir, bluezStorageMarker))
		if err != nil || string(raw) != bluezStorageIdentity {
			return errors.New("existing BlueZ storage is not owned by this service")
		}
		if err := os.RemoveAll(bluezStorageDir); err != nil {
			return err
		}
	}
	if err := privateDirectory(bluezStorageDir); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(bluezStorageDir, bluezStorageMarker),
		[]byte(bluezStorageIdentity), 0600); err != nil {
		return err
	}
	r.storage = true
	return nil
}

func (r *runtime) cleanupStorage() error {
	if !r.storage {
		return nil
	}
	info, err := os.Lstat(bluezStorageDir)
	if errors.Is(err, os.ErrNotExist) {
		r.storage = false
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("owned BlueZ storage changed type")
	}
	raw, err := os.ReadFile(filepath.Join(bluezStorageDir, bluezStorageMarker))
	if err != nil || string(raw) != bluezStorageIdentity {
		return errors.New("owned BlueZ storage marker changed")
	}
	if err := os.RemoveAll(bluezStorageDir); err != nil {
		return err
	}
	r.storage = false
	return nil
}

func (r *runtime) prepareANAFrequency(ctx context.Context) error {
	if err := checkANAFrequencyMarker(anaFreqPendingPath); err != nil {
		return err
	}
	if err := requireHCIExtension(hciExtensionPath); err != nil {
		return err
	}
	hci := r.transport.controller()
	if err := enableRBRHCI(ctx, hci, r.run); err != nil {
		return err
	}
	sequence := uint16(time.Now().UnixNano())
	if _, _, err := readRBRANAFrequency(ctx, hci, sequence, r.run); err != nil {
		return err
	}
	mutation, finish, err := protectedStep(ctx, 100*time.Second)
	if err != nil {
		return err
	}
	defer finish()
	syncContext, finishSync := context.WithTimeout(mutation, 15*time.Second)
	_, syncErr := r.run(syncContext, "sync")
	finishSync()
	if syncErr != nil {
		return fmt.Errorf("sync before ANA_FREQ transaction: %w", syncErr)
	}
	if err := writeANAFrequencyMarker(anaFreqPendingPath); err != nil {
		return fmt.Errorf("record ANA_FREQ transaction: %w", err)
	}
	fail := func(err error) error {
		return fmt.Errorf("ANA_FREQ transaction did not finish; reboot the router before another Start: %w", err)
	}
	if err := setRBRANAFrequency(mutation, hci, sequence+1, r.run); err != nil {
		return fail(err)
	}
	originalTTY := r.transport.tty()
	detach, finishDetach := context.WithTimeout(mutation, 22*time.Second)
	err = r.transport.detach(detach)
	finishDetach()
	if err != nil {
		return fail(err)
	}
	attach, finishAttach := context.WithTimeout(mutation, 32*time.Second)
	err = r.transport.attach(attach)
	finishAttach()
	if err != nil {
		return fail(err)
	}
	if r.transport.tty() != originalTTY {
		return fail(errors.New("UART changed during ANA_FREQ reattach"))
	}
	hci = r.transport.controller()
	if err := enableRBRHCI(mutation, hci, r.run); err != nil {
		return fail(err)
	}
	status, value, err := readRBRANAFrequency(mutation, hci, sequence+4, r.run)
	if err != nil {
		return fail(err)
	}
	if status != 0 || value != csrANAFrequency {
		return fail(fmt.Errorf("post-warm ANA_FREQ is status=%d value=%d", status, value))
	}
	if err := disableRBRHCI(mutation, hci, r.run); err != nil {
		return fail(err)
	}
	if err := clearANAFrequencyMarker(anaFreqPendingPath); err != nil {
		return fail(fmt.Errorf("clear ANA_FREQ transaction marker: %w", err))
	}
	return nil
}

func (r *runtime) prepare(ctx context.Context) error {
	if err := refuseOtherBluetoothd("/proc"); err != nil {
		return err
	}
	if err := checkANAFrequencyMarker(anaFreqPendingPath); err != nil {
		return err
	}
	uart, err := discoverUART("/sys")
	if err != nil {
		return err
	}
	r.transport = &uartTransport{uart: uart, sysfs: "/sys", dev: "/dev", proc: "/proc"}
	if err := r.prepareStorage(); err != nil {
		return err
	}
	attach, finishAttach, err := protectedStep(ctx, 32*time.Second)
	if err != nil {
		return err
	}
	err = r.transport.attach(attach)
	finishAttach()
	if err != nil {
		return err
	}
	if err := r.prepareANAFrequency(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := refuseOtherBluetoothd("/proc"); err != nil {
		return err
	}
	if err := r.transport.verify(); err != nil {
		return err
	}
	if err := removeSocket(busPath); err != nil {
		return err
	}
	r.busChild, err = startChild("/usr/bin/dbus-daemon", "--nofork", "--config-file="+configDir+"/bus.conf")
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 30; attempt++ {
		r.bluez, err = connectBluezBus(ctx, busPath)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.busChild.done:
			return fmt.Errorf("private D-Bus exited: %v", r.busChild.err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err != nil {
		return fmt.Errorf("private D-Bus did not become ready: %w", err)
	}
	r.bluez.verify = r.transport.verify
	signals := make(chan *dbus.Signal, 32)
	r.bluez.conn.Signal(signals)
	defer r.bluez.conn.RemoveSignal(signals)
	if err := r.bluez.conn.AddMatchSignalContext(ctx, dbus.WithMatchSender(bluezName),
		dbus.WithMatchInterface("org.freedesktop.DBus.ObjectManager")); err != nil {
		return err
	}
	r.bluezChild, err = startChild("/usr/bin/bluetoothd", "-n", "-f", configDir+"/main.conf", "-P", "*")
	if err != nil {
		return err
	}
	path := dbus.ObjectPath("/org/bluez/" + r.transport.controller())
	for {
		data, objectErr := r.bluez.Objects(ctx)
		if objectErr == nil {
			if _, ok := data[path][adapterIface]; ok {
				break
			}
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), objectErr)
		case <-r.bluezChild.done:
			return fmt.Errorf("BlueZ exited during startup: %v", r.bluezChild.err)
		case <-r.busChild.done:
			return fmt.Errorf("private D-Bus exited during startup: %v", r.busChild.err)
		case <-signals:
		}
	}
	for _, setting := range []struct {
		name  string
		value bool
	}{{"Discoverable", false}, {"Pairable", false}, {"Powered", true}} {
		if err := r.bluez.Call(ctx, path, propertyIface+".Set", adapterIface,
			setting.name, dbus.MakeVariant(setting.value)); err != nil {
			return err
		}
	}
	return nil
}

func waitPrivateBusClosed(ctx context.Context, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil
	case <-timer.C:
		return errors.New("private D-Bus client did not close after daemon exit")
	}
}

func runtimeCanDetach(bluezStopErr, busStopErr, busCloseErr error) bool {
	return bluezStopErr == nil && busStopErr == nil && busCloseErr == nil
}

func (r *runtime) cleanup() error {
	var errs []error
	if r.bluez != nil && r.transport != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := r.bluez.Call(ctx, dbus.ObjectPath("/org/bluez/"+r.transport.controller()),
			propertyIface+".Set", adapterIface, "Powered", dbus.MakeVariant(false))
		cancel()
		if err != nil {
			log.Printf("power off before teardown: %v", err)
		}
	}
	bluezStopErr := r.bluezChild.stop()
	errs = append(errs, bluezStopErr)
	if bluezStopErr == nil {
		errs = append(errs, r.cleanupStorage())
	}
	busStopErr := r.busChild.stop()
	errs = append(errs, busStopErr)
	var busCloseErr error
	if r.bluez != nil && busStopErr == nil {
		busCloseErr = waitPrivateBusClosed(r.bluez.conn.Context(), 2*time.Second)
		errs = append(errs, busCloseErr)
	}
	if r.transport != nil {
		if runtimeCanDetach(bluezStopErr, busStopErr, busCloseErr) {
			ctx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
			errs = append(errs, r.transport.detach(ctx))
			cancel()
		} else {
			errs = append(errs, errors.New("Bluetooth shutdown is incomplete; retaining the HCI owner"))
		}
	}
	return errors.Join(errs...)
}
