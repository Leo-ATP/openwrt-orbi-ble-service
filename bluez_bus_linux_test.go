// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestBluezBusOutlivesHandshakeAndClosesWithDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	socket := filepath.Join(t.TempDir(), "bus")
	cmd := exec.CommandContext(ctx, "dbus-daemon", "--session", "--nofork",
		"--print-address=1", "--address=unix:path="+dbus.EscapeBusAddressValue(socket))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	defer func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	handshake, finish := context.WithTimeout(ctx, time.Second)
	client, err := connectBluezBus(handshake, socket)
	finish()
	if err != nil {
		t.Fatal(err)
	}
	defer client.conn.Close()
	var names []string
	if err := client.conn.BusObject().CallWithContext(ctx,
		"org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	reaped = true
	if err != nil {
		t.Fatal(err)
	}
	if err := waitPrivateBusClosed(client.conn.Context(), 2*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestBluezBusStalledAuthenticationIsCancelled(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "stall")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = connectBluezBus(ctx, socket)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("duration=%s err=%v", time.Since(start), err)
	}
	select {
	case conn := <-accepted:
		conn.Close()
	case <-time.After(time.Second):
		t.Fatal("test connection was not accepted")
	}
}
