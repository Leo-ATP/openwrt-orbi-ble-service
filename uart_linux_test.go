// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func fixtureFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func fixtureLink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func uartFixture(t *testing.T, board string, nested bool) (string, *uart) {
	t.Helper()
	sysfs := filepath.Join(t.TempDir(), "sys")
	tree := filepath.Join(sysfs, "firmware", "devicetree", "base")
	node := filepath.Join(tree, "soc", "serial@78b0000")
	fixtureFile(t, filepath.Join(tree, "compatible"), []byte(board+"\x00qcom,ipq4019\x00"))
	fixtureFile(t, filepath.Join(node, "compatible"), []byte("qcom,msm-uartdm-v1.4\x00qcom,msm-uartdm\x00"))
	fixtureFile(t, filepath.Join(node, "status"), []byte("okay\x00"))
	reg := make([]byte, 8)
	binary.BigEndian.PutUint32(reg, 0x078b0000)
	binary.BigEndian.PutUint32(reg[4:], 0x200)
	fixtureFile(t, filepath.Join(node, "reg"), reg)
	platform := filepath.Join(sysfs, "devices", "platform", "soc", "78b0000.serial")
	driver := filepath.Join(sysfs, "bus", "platform", "drivers", "msm_serial")
	if err := os.MkdirAll(driver, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureLink(t, node, filepath.Join(platform, "of_node"))
	fixtureLink(t, driver, filepath.Join(platform, "driver"))
	parent := platform
	if nested {
		parent = filepath.Join(platform, "78b0000.serial:0", "78b0000.serial:0.0")
	}
	port := filepath.Join(parent, "tty", "ttyMSM7")
	fixtureLink(t, parent, filepath.Join(port, "device"))
	fixtureLink(t, port, filepath.Join(sysfs, "class", "tty", "ttyMSM7"))
	u, err := discoverUART(sysfs)
	if err != nil {
		t.Fatal(err)
	}
	return sysfs, u
}

func fixtureHCI(t *testing.T, sysfs string, u *uart, name string) {
	t.Helper()
	path := filepath.Join(u.path, "bluetooth", name)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureLink(t, path, filepath.Join(sysfs, "class", "bluetooth", name))
}

func TestUARTDiscoveryHandlesSerialBaseAndNonFixedTTY(t *testing.T) {
	for _, board := range []string{"netgear,rbr50", "netgear,rbs50"} {
		t.Run(board, func(t *testing.T) {
			for _, nested := range []bool{false, true} {
				sysfs, u := uartFixture(t, board, nested)
				if u.name != "ttyMSM7" {
					t.Fatalf("fixed TTY assumption: %+v", u)
				}
				if err := u.verify(sysfs); err != nil {
					t.Fatal(err)
				}
				fixtureHCI(t, sysfs, u, "hci4")
				hci, _, err := hciForUART(sysfs, u)
				if err != nil || hci != "hci4" {
					t.Fatalf("owned HCI=%q err=%v", hci, err)
				}
			}
		})
	}
}

func TestUARTDiscoveryFailsClosed(t *testing.T) {
	for _, board := range []string{"netgear,rbr50", "netgear,rbs50"} {
		for _, field := range []string{"status", "reg", "compatible", "driver", "ambiguous"} {
			t.Run(board+"/"+field, func(t *testing.T) {
				sysfs, u := uartFixture(t, board, true)
				tree := filepath.Join(sysfs, "firmware", "devicetree", "base")
				node := filepath.Join(tree, "soc", "serial@78b0000")
				switch field {
				case "status":
					fixtureFile(t, filepath.Join(node, "status"), []byte("disabled\x00"))
				case "reg":
					fixtureFile(t, filepath.Join(node, "reg"), make([]byte, 8))
				case "compatible":
					fixtureFile(t, filepath.Join(node, "compatible"), []byte("not-the-uart\x00"))
				case "driver":
					if err := os.Remove(filepath.Join(u.device, "driver")); err != nil {
						t.Fatal(err)
					}
				case "ambiguous":
					fixtureLink(t, u.path, filepath.Join(sysfs, "class", "tty", "ttyMSM8"))
				}
				if _, err := discoverUART(sysfs); err == nil {
					t.Fatal("invalid or unproven UART accepted")
				}
			})
		}
	}
}

func TestUARTDiscoveryRejectsOtherBoards(t *testing.T) {
	for _, board := range []string{
		"", "qcom,ipq4019", "netgear,rbr40", "netgear,rbs40",
		"netgear,rbr50v2", "netgear,rbs50v2",
		"prefix-netgear,rbs50", "netgear,rbs50-extra",
	} {
		t.Run(board, func(t *testing.T) {
			sysfs, _ := uartFixture(t, "netgear,rbr50", true)
			path := filepath.Join(sysfs, "firmware", "devicetree", "base", "compatible")
			fixtureFile(t, path, []byte(board+"\x00qcom,ipq4019\x00"))
			if _, err := discoverUART(sysfs); err == nil {
				t.Fatal("unsupported board or partial compatible match accepted")
			}
		})
	}
}

func TestForeignHCIAndChangedIdentityAreNeverDetached(t *testing.T) {
	for _, changed := range []bool{false, true} {
		sysfs, u := uartFixture(t, "netgear,rbr50", true)
		fixtureHCI(t, sysfs, u, "hci4")
		hci, info, err := hciForUART(sysfs, u)
		if err != nil {
			t.Fatal(err)
		}
		transport := &uartTransport{
			uart: u, sysfs: sysfs, hci: hci, hciInfo: info,
			child: &child{done: make(chan struct{})},
		}
		if changed {
			path := filepath.Join(u.path, "bluetooth", "hci4")
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		} else {
			foreign := filepath.Join(sysfs, "devices", "usb", "bluetooth", "hci8")
			if err := os.MkdirAll(foreign, 0700); err != nil {
				t.Fatal(err)
			}
			fixtureLink(t, foreign, filepath.Join(sysfs, "class", "bluetooth", "hci8"))
		}
		if err := transport.verify(); err == nil {
			t.Fatal("foreign or replaced controller accepted")
		}
		// A signal would panic on this process-free fixture if the guard failed.
		if err := transport.detach(context.Background()); err == nil {
			t.Fatal("foreign or replaced controller detached")
		}
	}
}

func TestExistingHCIBlocksAttachBeforeAnyCommand(t *testing.T) {
	sysfs, u := uartFixture(t, "netgear,rbr50", true)
	fixtureHCI(t, sysfs, u, "hci4")
	transport := &uartTransport{uart: u, sysfs: sysfs, proc: t.TempDir(), dev: t.TempDir()}
	if err := transport.attach(context.Background()); err == nil || transport.child != nil {
		t.Fatal("attach attempted to replace an existing HCI")
	}
}

func TestTransportStatusCanOverlapCompletedDetach(t *testing.T) {
	sysfs, u := uartFixture(t, "netgear,rbr50", true)
	done := make(chan struct{})
	close(done)
	transport := &uartTransport{
		uart: u, sysfs: sysfs, hci: "hci4", child: &child{done: done},
	}
	readerStarted, readerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(readerDone)
		close(readerStarted)
		for i := 0; i < 100; i++ {
			transport.controller()
			transport.exited()
			_ = transport.verify()
		}
	}()
	<-readerStarted
	if err := transport.detach(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-readerDone
	if transport.controller() != "" || transport.exited() != nil {
		t.Fatal("completed attachment owner was retained")
	}
}

func TestRefuseTTYAlreadyOpenedByAnotherProcess(t *testing.T) {
	proc := t.TempDir()
	device := filepath.Join(t.TempDir(), "ttyMSM7")
	fixtureFile(t, device, nil)
	fixtureLink(t, device, filepath.Join(proc, "123", "fd", "4"))
	if err := refuseTTYUsers(proc, device); err == nil {
		t.Fatal("open TTY was accepted")
	}
}

func TestStorageSymlinkIsNotFollowed(t *testing.T) {
	old := bluezStorageDir
	root := t.TempDir()
	target := filepath.Join(root, "other-owner")
	fixtureFile(t, filepath.Join(target, bluezStorageMarker), []byte(bluezStorageIdentity))
	bluezStorageDir = filepath.Join(root, "bluetooth")
	fixtureLink(t, target, bluezStorageDir)
	t.Cleanup(func() { bluezStorageDir = old })
	r := &runtime{}
	if err := r.prepareStorage(); err == nil {
		t.Fatal("storage symlink was accepted")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("symlink target was changed")
	}
}
