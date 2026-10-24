// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type controllerSession struct {
	sysfs      string
	uart       *uart
	hci        string
	hciInfo    os.FileInfo
	runCommand commandRunner
	runCSR     csrCommandRunner
}

type pendingTransaction struct {
	TTYName      string `json:"tty_name"`
	TTYPath      string `json:"tty_path"`
	PlatformPath string `json:"platform_path"`
	Device       uint64 `json:"device"`
	Inode        uint64 `json:"inode"`
	PostSequence uint16 `json:"post_sequence"`
}

func newControllerSession(sysfs string, runCommand commandRunner,
	runCSR csrCommandRunner) (*controllerSession, error) {
	if runCommand == nil || runCSR == nil {
		return nil, errors.New("controller command runner is unavailable")
	}
	u, hci, info, err := attachedUART(sysfs)
	if err != nil {
		return nil, err
	}
	return &controllerSession{
		sysfs: sysfs, uart: u, hci: hci, hciInfo: info,
		runCommand: runCommand, runCSR: runCSR,
	}, nil
}

func (s *controllerSession) verify() error {
	if err := s.uart.verify(s.sysfs); err != nil {
		return err
	}
	hci, info, err := hciForUART(s.sysfs, s.uart)
	if err != nil {
		return err
	}
	if hci != s.hci || info == nil || !os.SameFile(info, s.hciInfo) {
		return errors.New("HCI controller identity changed")
	}
	return nil
}

func (s *controllerSession) command(ctx context.Context, name string,
	args ...string) (string, error) {
	if name == "hciconfig" {
		if err := s.verify(); err != nil {
			return "", err
		}
	}
	return s.runCommand(ctx, name, args...)
}

func (s *controllerSession) csr(ctx context.Context, hci string,
	pdu []byte) (csrCommandResult, error) {
	if hci != s.hci {
		return csrCommandResult{}, errors.New("CSR command targeted an unexpected HCI controller")
	}
	if err := s.verify(); err != nil {
		return csrCommandResult{}, err
	}
	return s.runCSR(ctx, hci, pdu)
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("refusing non-directory or symlink %s", path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

func requireNoPendingTransaction(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("a previous ANA_FREQ transaction did not finish; reboot before retrying")
}

func pendingForUART(u *uart, postSequence uint16) (pendingTransaction, error) {
	device, inode, err := fileIdentity(u.info)
	if err != nil {
		return pendingTransaction{}, err
	}
	return pendingTransaction{
		TTYName: u.name, TTYPath: u.path, PlatformPath: u.device,
		Device: device, Inode: inode, PostSequence: postSequence,
	}, nil
}

func writePendingTransaction(path string, pending pendingTransaction) error {
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	encodeErr := json.NewEncoder(file).Encode(pending)
	return errors.Join(encodeErr, file.Close())
}

func readPendingTransaction(path string) (pendingTransaction, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return pendingTransaction{}, err
	}
	var pending pendingTransaction
	if err := json.Unmarshal(raw, &pending); err != nil {
		return pendingTransaction{}, fmt.Errorf("invalid pending ANA_FREQ transaction: %w", err)
	}
	if pending.TTYName == "" || pending.TTYPath == "" || pending.PlatformPath == "" {
		return pendingTransaction{}, errors.New("pending ANA_FREQ transaction omits UART identity")
	}
	return pending, nil
}

func clearPendingTransaction(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func verifyPendingUART(pending pendingTransaction, u *uart) error {
	device, inode, err := fileIdentity(u.info)
	if err != nil {
		return err
	}
	if pending.TTYName != u.name || pending.TTYPath != u.path ||
		pending.PlatformPath != u.device || pending.Device != device ||
		pending.Inode != inode {
		return errors.New("Bluetooth UART identity changed across warm reset")
	}
	return nil
}

func protectedStep(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	return ctx, cancel, nil
}

func prepareANAFrequency(parent context.Context, session *controllerSession,
	pendingPath, extensionPath string) error {
	if err := requireNoPendingTransaction(pendingPath); err != nil {
		return err
	}
	if err := requireHCIExtension(extensionPath); err != nil {
		return err
	}
	if err := enableRBRHCI(parent, session.hci, session.command); err != nil {
		return err
	}
	sequence := uint16(time.Now().UnixNano())
	// The initial GET proves the complete CSR response identity. Its status
	// and current value intentionally do not suppress the required PSRAM SET.
	if _, _, err := readRBRANAFrequency(parent, session.hci, sequence, session.csr); err != nil {
		return err
	}
	mutation, finish, err := protectedStep(parent, 45*time.Second)
	if err != nil {
		return err
	}
	defer finish()
	syncContext, finishSync := context.WithTimeout(mutation, 15*time.Second)
	_, syncErr := session.command(syncContext, "sync")
	finishSync()
	if syncErr != nil {
		return fmt.Errorf("sync before ANA_FREQ transaction: %w", syncErr)
	}
	pending, err := pendingForUART(session.uart, sequence+4)
	if err != nil {
		return err
	}
	if err := writePendingTransaction(pendingPath, pending); err != nil {
		return fmt.Errorf("record ANA_FREQ transaction: %w", err)
	}
	if err := setRBRANAFrequency(mutation, session.hci, sequence+1, session.csr); err != nil {
		return fmt.Errorf("ANA_FREQ transaction did not finish; reboot before retrying: %w", err)
	}
	return nil
}

func verifyANAFrequency(parent context.Context, session *controllerSession,
	pendingPath, extensionPath string) error {
	pending, err := readPendingTransaction(pendingPath)
	if err != nil {
		return fmt.Errorf("read pending ANA_FREQ transaction: %w", err)
	}
	if err := verifyPendingUART(pending, session.uart); err != nil {
		return err
	}
	if err := requireHCIExtension(extensionPath); err != nil {
		return err
	}
	if err := enableRBRHCI(parent, session.hci, session.command); err != nil {
		return err
	}
	status, value, err := readRBRANAFrequency(parent, session.hci,
		pending.PostSequence, session.csr)
	if err != nil {
		return err
	}
	if status != 0 || value != csrANAFrequency {
		return fmt.Errorf("post-warm ANA_FREQ is status=%d value=%d", status, value)
	}
	if err := disableRBRHCI(parent, session.hci, session.command); err != nil {
		return err
	}
	if err := clearPendingTransaction(pendingPath); err != nil {
		return fmt.Errorf("clear ANA_FREQ transaction marker: %w", err)
	}
	return nil
}
