// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

const testAddress = "02:00:00:00:00:01"

var testAdapterPath = dbus.ObjectPath("/org/bluez/hci4")
var testDevicePath = dbus.ObjectPath("/org/bluez/hci4/dev_02_00_00_00_00_01")

func testObjects() objects {
	return objects{
		testAdapterPath: {adapterIface: {
			"Address": dbus.MakeVariant("02:00:00:00:00:FE"),
			"Powered": dbus.MakeVariant(true), "Discovering": dbus.MakeVariant(false),
		}},
		testDevicePath: {deviceIface: {
			"Adapter": dbus.MakeVariant(testAdapterPath), "Address": dbus.MakeVariant(testAddress),
			"AddressType": dbus.MakeVariant("random"), "Alias": dbus.MakeVariant(" Example sensor "),
			"Connected": dbus.MakeVariant(false), "Paired": dbus.MakeVariant(false),
			"ServicesResolved": dbus.MakeVariant(false), "RSSI": dbus.MakeVariant(int16(-60)),
		}},
	}
}

type fakeBus struct {
	data      objects
	objectsFn func(context.Context) (objects, error)
	callFn    func(context.Context, dbus.ObjectPath, string, ...any) error
	waitFn    func(context.Context, dbus.ObjectPath) error
	calls     []string
}

func (b *fakeBus) Objects(ctx context.Context) (objects, error) {
	if b.objectsFn != nil {
		return b.objectsFn(ctx)
	}
	return b.data, nil
}

func (b *fakeBus) Call(ctx context.Context, path dbus.ObjectPath, method string, args ...any) error {
	b.calls = append(b.calls, method)
	if b.callFn != nil {
		return b.callFn(ctx, path, method, args...)
	}
	return nil
}

func (b *fakeBus) WaitConnected(ctx context.Context, path dbus.ObjectPath) error {
	if b.waitFn != nil {
		return b.waitFn(ctx, path)
	}
	return nil
}

func TestNamedDevicePresentationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"", false}, {" \t\r\n", false}, {": - _ .", false},
		{testAddress, false}, {"020000000001", false}, {"02-00-00-00-00-01", false},
		{"02_00_00_00_00_01", false}, {"0200.0000.0001", false},
		{"02 00 00 00 00 01", false}, {"02\u200300\u200300\u200300\u200300\u200301", false},
		{"Example sensor", true}, {"  Example sensor  ", true},
		{"Sensor 02:00:00:00:00:01", true}, {"02:00:00:00:00:02", true},
		{"CAFE", true}, {"not-an-address", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasDeviceDisplayName(tc.name, testAddress); got != tc.want {
				t.Fatalf("hasDeviceDisplayName(%q)=%v", tc.name, got)
			}
			data := testObjects()
			data[testDevicePath][deviceIface]["Alias"] = dbus.MakeVariant(tc.name)
			_, devices, err := snapshot(data, "hci4")
			if err != nil || (len(devices) == 1) != tc.want {
				t.Fatalf("snapshot=%+v err=%v", devices, err)
			}
		})
	}
	if value, ok := normalizedDeviceIdentifier(" 02-aa_BB.cc\tDD\u2003ef "); !ok || value != "02AABBCCDDEF" {
		t.Fatalf("identifier=%q ok=%v", value, ok)
	}
}

func TestSnapshotShapeAndOwnership(t *testing.T) {
	data := testObjects()
	a, devices, err := snapshot(data, "hci4")
	if err != nil || a.HCI != "hci4" || len(devices) != 1 ||
		devices[0].Name != "Example sensor" || devices[0].RSSI == nil ||
		*devices[0].RSSI != -60 || devices[0].UUIDs == nil {
		t.Fatalf("snapshot=%+v %+v err=%v", a, devices, err)
	}
	data[testDevicePath][deviceIface]["Adapter"] = dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0"))
	if _, _, err := snapshot(data, "hci4"); err == nil {
		t.Fatal("cross-controller device accepted")
	}
	data = testObjects()
	data[testDevicePath][deviceIface]["RSSI"] = dbus.MakeVariant(int32(-60))
	if _, _, err := snapshot(data, "hci4"); err == nil {
		t.Fatal("wrong RSSI type accepted")
	}
	data = testObjects()
	data[testDevicePath][deviceIface]["Alias"] = dbus.MakeVariant(testAddress)
	if _, _, err := findDevice(data, "hci4", testAddress); err == nil {
		t.Fatal("hidden device accepted by selection")
	}
}

func TestScanCleanupAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := &fakeBus{}
	bus.callFn = func(ctx context.Context, path dbus.ObjectPath, method string, args ...any) error {
		if path != testAdapterPath {
			t.Fatal("scan touched another adapter")
		}
		switch method {
		case adapterIface + ".SetDiscoveryFilter":
			filter := args[0].(map[string]dbus.Variant)
			if filter["Transport"].Value() != "le" || filter["Discoverable"].Value() != false {
				t.Fatalf("unexpected filter=%v", filter)
			}
		case adapterIface + ".StartDiscovery":
			cancel()
		case adapterIface + ".StopDiscovery":
			deadline, ok := ctx.Deadline()
			if ctx.Err() != nil || !ok || time.Until(deadline) > 2*time.Second {
				t.Fatal("scan cleanup was cancelled or unbounded")
			}
		}
		return nil
	}
	if err := scan(ctx, bus, "hci4", 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("scan=%v", err)
	}
	expected := []string{adapterIface + ".SetDiscoveryFilter", adapterIface + ".StartDiscovery", adapterIface + ".StopDiscovery"}
	if !reflect.DeepEqual(bus.calls, expected) {
		t.Fatalf("scan lifecycle=%v", bus.calls)
	}
}

func TestScanHonorsFiveSecondBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	bus := &fakeBus{}
	start := time.Now()
	if err := scan(ctx, bus, "hci4", 5); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Second || elapsed >= 7*time.Second {
		t.Fatalf("scan duration=%s", elapsed)
	}
}

func TestConnectCleansOnlyItsOwnAttempt(t *testing.T) {
	for _, alreadyConnected := range []bool{false, true} {
		bus := &fakeBus{data: testObjects()}
		bus.data[testDevicePath][deviceIface]["Connected"] = dbus.MakeVariant(alreadyConnected)
		bus.waitFn = func(ctx context.Context, path dbus.ObjectPath) error {
			deadline, ok := ctx.Deadline()
			if path != testDevicePath || !ok || time.Until(deadline) > 10*time.Second {
				t.Fatal("connection wait has wrong identity or budget")
			}
			return context.DeadlineExceeded
		}
		if err := changeConnection(context.Background(), bus, "hci4", testAddress, true); err == nil {
			t.Fatal("failed connection returned success")
		}
		if alreadyConnected {
			if len(bus.calls) != 0 {
				t.Fatalf("failed wait disconnected a pre-existing connection: %v", bus.calls)
			}
		} else if !reflect.DeepEqual(bus.calls, []string{deviceIface + ".Connect", deviceIface + ".Disconnect"}) {
			t.Fatalf("new connection cleanup=%v", bus.calls)
		}
	}
}

func TestConnectionIsIdempotentAndNeverPairs(t *testing.T) {
	bus := &fakeBus{data: testObjects()}
	p := bus.data[testDevicePath][deviceIface]
	p["Connected"], p["ServicesResolved"] = dbus.MakeVariant(true), dbus.MakeVariant(true)
	if err := changeConnection(context.Background(), bus, "hci4", testAddress, true); err != nil || len(bus.calls) != 0 {
		t.Fatalf("ready connection=%v calls=%v", err, bus.calls)
	}
	if err := changeConnection(context.Background(), bus, "hci4", testAddress, false); err == nil {
		t.Fatal("disconnect accepted a still-connected BlueZ snapshot")
	}
	bus.callFn = func(_ context.Context, _ dbus.ObjectPath, method string, _ ...any) error {
		if method != deviceIface+".Disconnect" {
			t.Fatalf("unexpected device operation %s", method)
		}
		p["Connected"] = dbus.MakeVariant(false)
		return nil
	}
	if err := changeConnection(context.Background(), bus, "hci4", testAddress, false); err != nil {
		t.Fatal(err)
	}
	if err := changeConnection(context.Background(), bus, "hci4", testAddress, false); err != nil {
		t.Fatal(err)
	}
	for _, call := range bus.calls {
		if strings.Contains(call, "Pair") || strings.Contains(call, "Trusted") {
			t.Fatalf("pairing operation was issued: %s", call)
		}
	}
}

func TestStatusDoesNotReturnReadyAfterConcurrentStop(t *testing.T) {
	testStatePath(t)
	s := newServer(context.Background())
	s.current = initialState("ready", "")
	s.hci = "hci4"
	s.bus = &fakeBus{objectsFn: func(context.Context) (objects, error) {
		if err := s.phase("stopping", ""); err != nil {
			t.Fatal(err)
		}
		return testObjects(), nil
	}}
	reply := s.status(context.Background())
	if !reply.OK || reply.State.Phase != "stopping" || reply.State.Adapter != nil {
		t.Fatalf("stale readiness returned: %+v", reply)
	}
}
