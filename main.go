// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var version = "0.2.0"

const (
	runDir           = "/var/run/orbi-bt-setup"
	pendingPath      = runDir + "/ana-freq.pending"
	lockPath         = "/var/run/orbi-bt-setup.lock"
	sysfsPath        = "/sys"
	devPath          = "/dev"
	procPath         = "/proc"
	hciExtensionPath = "/sys/module/hci_uart/parameters/hciextn"
)

type application struct {
	sysfs        string
	dev          string
	proc         string
	pending      string
	hciExtension string
	runCommand   commandRunner
	runCSR       csrCommandRunner
	stdout       io.Writer
}

var commandSurface = [...]string{"tty", "hci", "prepare", "verify"}

func knownCommand(name string) bool {
	for _, command := range commandSurface {
		if name == command {
			return true
		}
	}
	return false
}

func command(ctx context.Context, name string, args ...string) (string, error) {
	var executable string
	switch name {
	case "hciconfig":
		executable = "/usr/bin/hciconfig"
	case "sync":
		executable = "/bin/sync"
	default:
		return "", fmt.Errorf("unsupported command %q", name)
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Cancel = func() error {
		err := cmd.Process.Signal(syscall.SIGTERM)
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	output, err := cmd.CombinedOutput()
	text := string(output)
	if err != nil {
		return text, fmt.Errorf("%s: %w: %s", name, err, text)
	}
	return text, nil
}

func defaultApplication() *application {
	return &application{
		sysfs: sysfsPath, dev: devPath, proc: procPath,
		pending: pendingPath, hciExtension: hciExtensionPath,
		runCommand: command, runCSR: hciCSRCommand, stdout: os.Stdout,
	}
}

func (a *application) execute(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: orbi-bt-setup tty | hci | prepare | verify")
	}
	if !knownCommand(args[0]) {
		return fmt.Errorf("unknown command %q", args[0])
	}
	switch args[0] {
	case "tty":
		if err := requireNoPendingTransaction(a.pending); err != nil {
			return err
		}
		u, err := availableUART(a.sysfs, a.dev, a.proc)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.stdout, filepath.Join(a.dev, u.name))
		return err
	case "hci":
		_, hci, _, err := attachedUART(a.sysfs)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.stdout, hci)
		return err
	case "prepare":
		if err := requireNoPendingTransaction(a.pending); err != nil {
			return err
		}
		session, err := newControllerSession(a.sysfs, a.runCommand, a.runCSR)
		if err != nil {
			return err
		}
		return prepareANAFrequency(ctx, session, a.pending, a.hciExtension)
	case "verify":
		session, err := newControllerSession(a.sysfs, a.runCommand, a.runCSR)
		if err != nil {
			return err
		}
		return verifyANAFrequency(ctx, session, a.pending, a.hciExtension)
	}
	return errors.New("unreachable command")
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-V") {
		fmt.Println("orbi-bt-setup", version)
		return
	}
	unlock, err := takeSetupLock(lockPath)
	if err != nil {
		log.Fatal(err)
	}
	defer unlock()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := defaultApplication().execute(ctx, os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
