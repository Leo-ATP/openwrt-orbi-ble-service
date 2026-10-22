// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorageRejectsUnownedDirectory(t *testing.T) {
	old := bluezStorageDir
	bluezStorageDir = t.TempDir()
	t.Cleanup(func() { bluezStorageDir = old })
	path := filepath.Join(bluezStorageDir, "other-owner")
	if err := os.WriteFile(path, []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &runtime{}
	if err := r.prepareStorage(); err == nil {
		t.Fatal("unowned storage accepted")
	}
	if err := r.cleanupStorage(); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "retain" {
		t.Fatal("unowned storage changed")
	}
}

func TestStorageMarkerAndCleanup(t *testing.T) {
	old := bluezStorageDir
	bluezStorageDir = filepath.Join(t.TempDir(), "bluetooth")
	t.Cleanup(func() { bluezStorageDir = old })
	r := &runtime{}
	if err := r.prepareStorage(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(bluezStorageDir, bluezStorageMarker)
	if err := os.WriteFile(marker, []byte("another owner\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.cleanupStorage(); err == nil {
		t.Fatal("changed owner marker was ignored")
	}
	if err := os.WriteFile(marker, []byte(bluezStorageIdentity), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.cleanupStorage(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bluezStorageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned storage remained: %v", err)
	}
}

func TestDetachRequiresEveryTransportLayerToStop(t *testing.T) {
	failure := context.DeadlineExceeded
	for _, tc := range []struct {
		bluez, bus, client error
		want               bool
	}{
		{nil, nil, nil, true},
		{failure, nil, nil, false},
		{nil, failure, nil, false},
		{nil, nil, failure, false},
	} {
		if got := runtimeCanDetach(tc.bluez, tc.bus, tc.client); got != tc.want {
			t.Fatalf("unsafe detach gate: %+v", tc)
		}
	}
	closed, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitPrivateBusClosed(closed, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waitPrivateBusClosed(context.Background(), time.Millisecond); err == nil {
		t.Fatal("open bus lifetime accepted")
	}
}

func TestRefusesExistingBluetoothDaemon(t *testing.T) {
	proc := t.TempDir()
	dir := filepath.Join(proc, "123")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte("bluetoothd\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := refuseOtherBluetoothd(proc); err == nil {
		t.Fatal("existing Bluetooth service accepted")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("other process was changed")
	}
}
