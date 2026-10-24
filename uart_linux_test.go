// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	platformDriver := filepath.Join(sysfs, "bus", "platform", "drivers", "msm_serial")
	if err := os.MkdirAll(platformDriver, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureLink(t, node, filepath.Join(platform, "of_node"))
	fixtureLink(t, platformDriver, filepath.Join(platform, "driver"))
	fixtureLink(t, filepath.Join(sysfs, "bus", "platform"), filepath.Join(platform, "subsystem"))
	parent := platform
	if nested {
		ctrl := filepath.Join(platform, "78b0000.serial:0")
		port := filepath.Join(ctrl, "78b0000.serial:0.0")
		ctrlDriver := filepath.Join(sysfs, "bus", "serial-base", "drivers", "ctrl")
		portDriver := filepath.Join(sysfs, "bus", "serial-base", "drivers", "port")
		if err := os.MkdirAll(ctrlDriver, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(portDriver, 0700); err != nil {
			t.Fatal(err)
		}
		fixtureLink(t, node, filepath.Join(ctrl, "of_node"))
		fixtureLink(t, ctrlDriver, filepath.Join(ctrl, "driver"))
		fixtureLink(t, filepath.Join(sysfs, "bus", "serial-base"), filepath.Join(ctrl, "subsystem"))
		fixtureLink(t, node, filepath.Join(port, "of_node"))
		fixtureLink(t, portDriver, filepath.Join(port, "driver"))
		fixtureLink(t, filepath.Join(sysfs, "bus", "serial-base"), filepath.Join(port, "subsystem"))
		parent = port
	}
	port := filepath.Join(parent, "tty", "ttyMSM7")
	fixtureFile(t, filepath.Join(port, "dev"), []byte("4:64\n"))
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

func TestUARTDiscoveryHandlesFlatAndSerialBaseHierarchy(t *testing.T) {
	for _, board := range []string{"netgear,rbr50", "netgear,rbs50"} {
		t.Run(board, func(t *testing.T) {
			for _, nested := range []bool{false, true} {
				name := "flat"
				if nested {
					name = "serial-base"
				}
				t.Run(name, func(t *testing.T) {
					sysfs, u := uartFixture(t, board, nested)
					if u.name != "ttyMSM7" || filepath.Base(u.device) != "78b0000.serial" {
						t.Fatalf("wrong TTY or platform identity: %+v", u)
					}
					if err := u.verify(sysfs); err != nil {
						t.Fatal(err)
					}
					fixtureHCI(t, sysfs, u, "hci4")
					hci, _, err := hciForUART(sysfs, u)
					if err != nil || hci != "hci4" {
						t.Fatalf("owned HCI=%q err=%v", hci, err)
					}
				})
			}
		})
	}
}

func replaceFixtureLink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	fixtureLink(t, target, path)
}

func TestUARTDiscoveryValidatesSerialBaseHierarchy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string, *uart)
		want   string
	}{
		{
			name: "wrong-port-driver-path",
			mutate: func(t *testing.T, sysfs string, u *uart) {
				port, err := filepath.EvalSymlinks(filepath.Join(u.path, "device"))
				if err != nil {
					t.Fatal(err)
				}
				wrong := filepath.Join(sysfs, "bus", "wrong", "drivers", "port")
				if err := os.MkdirAll(wrong, 0700); err != nil {
					t.Fatal(err)
				}
				replaceFixtureLink(t, wrong, filepath.Join(port, "driver"))
			},
			want: "unexpected first-level driver",
		},
		{
			name: "wrong-port-subsystem",
			mutate: func(t *testing.T, sysfs string, u *uart) {
				port, err := filepath.EvalSymlinks(filepath.Join(u.path, "device"))
				if err != nil {
					t.Fatal(err)
				}
				replaceFixtureLink(t, filepath.Join(sysfs, "bus", "platform"),
					filepath.Join(port, "subsystem"))
			},
			want: "port has unexpected subsystem",
		},
		{
			name: "wrong-ctrl-driver",
			mutate: func(t *testing.T, sysfs string, u *uart) {
				port, err := filepath.EvalSymlinks(filepath.Join(u.path, "device"))
				if err != nil {
					t.Fatal(err)
				}
				ctrl := filepath.Dir(port)
				replaceFixtureLink(t, filepath.Join(sysfs, "bus", "serial-base", "drivers", "port"),
					filepath.Join(ctrl, "driver"))
			},
			want: "port is not parented by the ctrl driver",
		},
		{
			name: "wrong-ctrl-subsystem",
			mutate: func(t *testing.T, sysfs string, u *uart) {
				port, err := filepath.EvalSymlinks(filepath.Join(u.path, "device"))
				if err != nil {
					t.Fatal(err)
				}
				replaceFixtureLink(t, filepath.Join(sysfs, "bus", "platform"),
					filepath.Join(filepath.Dir(port), "subsystem"))
			},
			want: "ctrl has unexpected subsystem",
		},
		{
			name: "missing-port-driver",
			mutate: func(t *testing.T, _ string, u *uart) {
				port, err := filepath.EvalSymlinks(filepath.Join(u.path, "device"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(port, "driver")); err != nil {
					t.Fatal(err)
				}
			},
			want: "resolve Bluetooth UART driver",
		},
		{
			name: "missing-msm-serial-driver",
			mutate: func(t *testing.T, _ string, u *uart) {
				if err := os.Remove(filepath.Join(u.device, "driver")); err != nil {
					t.Fatal(err)
				}
			},
			want: "resolve Bluetooth UART driver",
		},
		{
			name: "wrong-msm-serial-driver-path",
			mutate: func(t *testing.T, sysfs string, u *uart) {
				wrong := filepath.Join(sysfs, "bus", "wrong", "drivers", "msm_serial")
				if err := os.MkdirAll(wrong, 0700); err != nil {
					t.Fatal(err)
				}
				replaceFixtureLink(t, wrong, filepath.Join(u.device, "driver"))
			},
			want: "ctrl is not parented by msm_serial",
		},
		{
			name: "wrong-platform-subsystem",
			mutate: func(t *testing.T, sysfs string, u *uart) {
				replaceFixtureLink(t, filepath.Join(sysfs, "bus", "serial-base"),
					filepath.Join(u.device, "subsystem"))
			},
			want: "msm_serial owner has unexpected subsystem",
		},
		{
			name: "different-parent-of-node",
			mutate: func(t *testing.T, sysfs string, u *uart) {
				port, err := filepath.EvalSymlinks(filepath.Join(u.path, "device"))
				if err != nil {
					t.Fatal(err)
				}
				other := filepath.Join(sysfs, "firmware", "devicetree", "base", "soc", "serial@other")
				if err := os.MkdirAll(other, 0700); err != nil {
					t.Fatal(err)
				}
				replaceFixtureLink(t, other, filepath.Join(filepath.Dir(port), "of_node"))
			},
			want: "changed device-tree identity",
		},
		{
			name: "driver-symlink-loop",
			mutate: func(t *testing.T, _ string, u *uart) {
				port, err := filepath.EvalSymlinks(filepath.Join(u.path, "device"))
				if err != nil {
					t.Fatal(err)
				}
				driver := filepath.Join(port, "driver")
				if err := os.Remove(driver); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(driver, driver); err != nil {
					t.Fatal(err)
				}
			},
			want: "resolve Bluetooth UART driver",
		},
		{
			name: "of-node-symlink-loop",
			mutate: func(t *testing.T, _ string, u *uart) {
				port, err := filepath.EvalSymlinks(filepath.Join(u.path, "device"))
				if err != nil {
					t.Fatal(err)
				}
				ofNode := filepath.Join(port, "of_node")
				if err := os.Remove(ofNode); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(ofNode, ofNode); err != nil {
					t.Fatal(err)
				}
			},
			want: "resolve Bluetooth UART device-tree identity",
		},
		{
			name: "device-symlink-loop",
			mutate: func(t *testing.T, _ string, u *uart) {
				device := filepath.Join(u.path, "device")
				if err := os.Remove(device); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(device, device); err != nil {
					t.Fatal(err)
				}
			},
			want: "resolve Bluetooth TTY device",
		},
		{
			name: "device-outside-sysfs",
			mutate: func(t *testing.T, _ string, u *uart) {
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.MkdirAll(outside, 0700); err != nil {
					t.Fatal(err)
				}
				replaceFixtureLink(t, outside, filepath.Join(u.path, "device"))
			},
			want: "escapes sysfs",
		},
		{
			name: "msm-serial-owner-not-platform-device",
			mutate: func(t *testing.T, sysfs string, u *uart) {
				tree := filepath.Join(sysfs, "firmware", "devicetree", "base")
				node := filepath.Join(tree, "soc", "serial@78b0000")
				platform := filepath.Join(sysfs, "devices", "virtual", "orphan.serial")
				ctrl := filepath.Join(platform, "orphan.serial:0")
				port := filepath.Join(ctrl, "orphan.serial:0.0")
				fixtureLink(t, node, filepath.Join(platform, "of_node"))
				fixtureLink(t, filepath.Join(sysfs, "bus", "platform", "drivers", "msm_serial"),
					filepath.Join(platform, "driver"))
				fixtureLink(t, filepath.Join(sysfs, "bus", "platform"),
					filepath.Join(platform, "subsystem"))
				fixtureLink(t, node, filepath.Join(ctrl, "of_node"))
				fixtureLink(t, filepath.Join(sysfs, "bus", "serial-base", "drivers", "ctrl"),
					filepath.Join(ctrl, "driver"))
				fixtureLink(t, filepath.Join(sysfs, "bus", "serial-base"),
					filepath.Join(ctrl, "subsystem"))
				fixtureLink(t, node, filepath.Join(port, "of_node"))
				fixtureLink(t, filepath.Join(sysfs, "bus", "serial-base", "drivers", "port"),
					filepath.Join(port, "driver"))
				fixtureLink(t, filepath.Join(sysfs, "bus", "serial-base"),
					filepath.Join(port, "subsystem"))
				replaceFixtureLink(t, port, filepath.Join(u.path, "device"))
			},
			want: "msm_serial owner is not a platform device",
		},
		{
			name: "tty-outside-platform-ancestor",
			mutate: func(t *testing.T, sysfs string, u *uart) {
				tree := filepath.Join(sysfs, "firmware", "devicetree", "base")
				node := filepath.Join(tree, "soc", "serial@78b0000")
				platform := filepath.Join(sysfs, "devices", "platform", "soc", "other.serial")
				ctrl := filepath.Join(platform, "other.serial:0")
				port := filepath.Join(ctrl, "other.serial:0.0")
				fixtureLink(t, node, filepath.Join(platform, "of_node"))
				fixtureLink(t, filepath.Join(sysfs, "bus", "platform", "drivers", "msm_serial"),
					filepath.Join(platform, "driver"))
				fixtureLink(t, filepath.Join(sysfs, "bus", "platform"),
					filepath.Join(platform, "subsystem"))
				fixtureLink(t, node, filepath.Join(ctrl, "of_node"))
				fixtureLink(t, filepath.Join(sysfs, "bus", "serial-base", "drivers", "ctrl"),
					filepath.Join(ctrl, "driver"))
				fixtureLink(t, filepath.Join(sysfs, "bus", "serial-base"),
					filepath.Join(ctrl, "subsystem"))
				fixtureLink(t, node, filepath.Join(port, "of_node"))
				fixtureLink(t, filepath.Join(sysfs, "bus", "serial-base", "drivers", "port"),
					filepath.Join(port, "driver"))
				fixtureLink(t, filepath.Join(sysfs, "bus", "serial-base"),
					filepath.Join(port, "subsystem"))
				replaceFixtureLink(t, port, filepath.Join(u.path, "device"))
			},
			want: "does not belong to its platform device",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sysfs, u := uartFixture(t, "netgear,rbs50", true)
			tc.mutate(t, sysfs, u)
			if _, err := discoverUART(sysfs); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestUARTDiscoveryIgnoresUnrelatedTTY(t *testing.T) {
	sysfs, expected := uartFixture(t, "netgear,rbs50", true)
	tree := filepath.Join(sysfs, "firmware", "devicetree", "base")
	otherNode := filepath.Join(tree, "soc", "serial@other")
	otherDevice := filepath.Join(sysfs, "devices", "platform", "soc", "other.serial")
	otherTTY := filepath.Join(otherDevice, "tty", "ttyMSM8")
	if err := os.MkdirAll(otherNode, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureLink(t, otherNode, filepath.Join(otherDevice, "of_node"))
	fixtureLink(t, otherDevice, filepath.Join(otherTTY, "device"))
	fixtureLink(t, otherTTY, filepath.Join(sysfs, "class", "tty", "ttyMSM8"))
	got, err := discoverUART(sysfs)
	if err != nil {
		t.Fatal(err)
	}
	if got.name != expected.name || got.path != expected.path || got.device != expected.device {
		t.Fatalf("unrelated TTY changed selection: got=%+v want=%+v", got, expected)
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

func TestAvailableUARTRejectsExistingHCI(t *testing.T) {
	sysfs, u := uartFixture(t, "netgear,rbr50", true)
	fixtureHCI(t, sysfs, u, "hci4")
	if _, err := availableUART(sysfs, t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("existing HCI was accepted")
	}
}

func TestAttachedUARTIgnoresUnrelatedHCI(t *testing.T) {
	sysfs, u := uartFixture(t, "netgear,rbr50", true)
	fixtureHCI(t, sysfs, u, "hci4")
	foreign := filepath.Join(sysfs, "devices", "usb", "bluetooth", "hci8")
	if err := os.MkdirAll(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureLink(t, foreign, filepath.Join(sysfs, "class", "bluetooth", "hci8"))
	_, hci, _, err := attachedUART(sysfs)
	if err != nil || hci != "hci4" {
		t.Fatalf("target HCI=%q err=%v", hci, err)
	}
}

func TestAttachedUARTRejectsMultipleTargetHCI(t *testing.T) {
	sysfs, u := uartFixture(t, "netgear,rbr50", true)
	fixtureHCI(t, sysfs, u, "hci4")
	fixtureHCI(t, sysfs, u, "hci5")
	if _, _, _, err := attachedUART(sysfs); err == nil {
		t.Fatal("multiple target HCI controllers were accepted")
	}
}

func TestAvailableUARTChecksCharacterDeviceAndConsole(t *testing.T) {
	sysfs, u := uartFixture(t, "netgear,rbs50", true)
	foreign := filepath.Join(sysfs, "devices", "usb", "bluetooth", "hci8")
	if err := os.MkdirAll(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	fixtureLink(t, foreign, filepath.Join(sysfs, "class", "bluetooth", "hci8"))
	dev, proc := t.TempDir(), t.TempDir()
	info, err := os.Stat("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	major, minor, device, err := characterDeviceInfo(info)
	if err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, filepath.Join(u.path, "dev"),
		[]byte(fmt.Sprintf("%d:%d\n", major, minor)))
	node := filepath.Join(dev, u.name)
	if err := syscall.Mknod(node, syscall.S_IFCHR|0600, int(device)); err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("creating a character-device fixture requires permission")
		}
		t.Fatal(err)
	}
	fixtureFile(t, filepath.Join(proc, "consoles"), nil)
	got, err := availableUART(sysfs, dev, proc)
	if err != nil || got.name != u.name {
		t.Fatalf("available UART=%+v err=%v", got, err)
	}
	fixtureFile(t, filepath.Join(proc, "consoles"), []byte(u.name+" -W- (EC p a) 4:64\n"))
	if _, err := availableUART(sysfs, dev, proc); err == nil {
		t.Fatal("console UART was accepted")
	}
}

func TestLinuxDeviceNumberDecoding(t *testing.T) {
	info, err := os.Stat("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	major, minor, _, err := characterDeviceInfo(info)
	if err != nil {
		t.Fatal(err)
	}
	if major != 1 || minor != 3 {
		t.Fatalf("/dev/null decoded as %d:%d", major, minor)
	}
}

func TestAvailableUARTRejectsDeviceSymlink(t *testing.T) {
	sysfs, u := uartFixture(t, "netgear,rbs50", true)
	dev, proc := t.TempDir(), t.TempDir()
	if err := os.Symlink("/dev/null", filepath.Join(dev, u.name)); err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, filepath.Join(proc, "consoles"), nil)
	if _, err := availableUART(sysfs, dev, proc); err == nil {
		t.Fatal("symlink character device was accepted")
	}
}

func TestRefuseTTYAlreadyOpenedByAnotherProcess(t *testing.T) {
	proc := t.TempDir()
	info, err := os.Stat("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	_, _, device, err := characterDeviceInfo(info)
	if err != nil {
		t.Fatal(err)
	}
	fixtureLink(t, "/dev/null", filepath.Join(proc, "123", "fd", "4"))
	if err := refuseTTYUsers(proc, device); err == nil {
		t.Fatal("open TTY was accepted")
	}
}
