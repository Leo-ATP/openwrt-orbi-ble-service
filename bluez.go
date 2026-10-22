// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/godbus/dbus/v5"
)

const (
	bluezName     = "org.bluez"
	adapterIface  = "org.bluez.Adapter1"
	deviceIface   = "org.bluez.Device1"
	propertyIface = "org.freedesktop.DBus.Properties"
)

type properties = map[string]dbus.Variant
type objects map[dbus.ObjectPath]map[string]properties

type bluetoothBus interface {
	Objects(context.Context) (objects, error)
	Call(context.Context, dbus.ObjectPath, string, ...any) error
	WaitConnected(context.Context, dbus.ObjectPath) error
}

type bluez struct {
	conn   *dbus.Conn
	verify func() error
}

func connectBluezBus(ctx context.Context, path string) (*bluez, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("D-Bus connection requires a deadline")
	}
	owned, cancel := context.WithCancel(context.Background())
	stopHandshake := context.AfterFunc(ctx, cancel)
	conn, err := dbus.Connect(
		"unix:path="+dbus.EscapeBusAddressValue(path), dbus.WithContext(owned))
	stopHandshake()
	err = errors.Join(err, ctx.Err(), owned.Err())
	if err != nil {
		cancel()
		if conn != nil {
			_ = conn.Close()
		}
		return nil, err
	}
	context.AfterFunc(conn.Context(), cancel)
	return &bluez{conn: conn}, nil
}

func (b *bluez) Objects(ctx context.Context) (objects, error) {
	if b.verify != nil {
		if err := b.verify(); err != nil {
			return nil, err
		}
	}
	var data objects
	err := b.conn.Object(bluezName, "/").CallWithContext(ctx,
		"org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&data)
	return data, err
}

func (b *bluez) Call(ctx context.Context, path dbus.ObjectPath, method string, args ...any) error {
	if b.verify != nil {
		if err := b.verify(); err != nil {
			return err
		}
	}
	return b.conn.Object(bluezName, path).CallWithContext(ctx, method, 0, args...).Err
}

func (b *bluez) WaitConnected(ctx context.Context, path dbus.ObjectPath) error {
	signals := make(chan *dbus.Signal, 32)
	b.conn.Signal(signals)
	defer b.conn.RemoveSignal(signals)
	options := []dbus.MatchOption{
		dbus.WithMatchSender(bluezName),
		dbus.WithMatchInterface(propertyIface),
		dbus.WithMatchObjectPath(path),
	}
	if err := b.conn.AddMatchSignalContext(ctx, options...); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := b.conn.RemoveMatchSignalContext(cleanup, options...); err != nil {
			log.Printf("remove connection signal match: %v", err)
		}
	}()
	for {
		data, err := b.Objects(ctx)
		if err != nil {
			return err
		}
		p, ok := data[path][deviceIface]
		if !ok {
			return errors.New("selected device disappeared")
		}
		connected, err := property[bool](p, "Connected")
		if err != nil {
			return err
		}
		resolved, err := property[bool](p, "ServicesResolved")
		if err != nil {
			return err
		}
		if connected && resolved {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-signals:
			if !ok {
				return errors.New("D-Bus signal connection closed")
			}
		}
	}
}

func property[T any](p properties, key string) (T, error) {
	var zero T
	v, ok := p[key]
	if !ok {
		return zero, fmt.Errorf("BlueZ property %s is absent", key)
	}
	value, ok := v.Value().(T)
	if !ok {
		return zero, fmt.Errorf("BlueZ property %s has an unexpected type", key)
	}
	return value, nil
}

func normalizedDeviceIdentifier(value string) (string, bool) {
	var result strings.Builder
	for _, character := range strings.ToUpper(strings.TrimSpace(value)) {
		switch {
		case character >= '0' && character <= '9',
			character >= 'A' && character <= 'F':
			result.WriteRune(character)
		case character == ':', character == '-', character == '_',
			character == '.', unicode.IsSpace(character):
		default:
			return "", false
		}
	}
	return result.String(), true
}

func hasDeviceDisplayName(name, address string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	nameIdentifier, macLike := normalizedDeviceIdentifier(name)
	if !macLike {
		return true
	}
	if nameIdentifier == "" {
		return false
	}
	addressIdentifier, _ := normalizedDeviceIdentifier(address)
	return nameIdentifier != addressIdentifier
}

func snapshot(data objects, hci string) (*adapter, []device, error) {
	path := dbus.ObjectPath("/org/bluez/" + hci)
	p, ok := data[path][adapterIface]
	if !ok {
		return nil, nil, errors.New("owned Bluetooth adapter is absent from BlueZ")
	}
	a := &adapter{HCI: hci}
	var err error
	if a.Address, err = property[string](p, "Address"); err != nil {
		return nil, nil, err
	}
	if a.Powered, err = property[bool](p, "Powered"); err != nil {
		return nil, nil, err
	}
	if a.Discovering, err = property[bool](p, "Discovering"); err != nil {
		return nil, nil, err
	}
	devices := []device{}
	for objectPath, interfaces := range data {
		p, ok := interfaces[deviceIface]
		if !ok || !strings.HasPrefix(string(objectPath), string(path)+"/dev_") {
			continue
		}
		owner, err := property[dbus.ObjectPath](p, "Adapter")
		if err != nil || owner != path {
			return nil, nil, errors.New("ambiguous BlueZ device adapter ownership")
		}
		d := device{UUIDs: []string{}}
		if d.Address, err = property[string](p, "Address"); err != nil || !addressPattern.MatchString(d.Address) {
			return nil, nil, errors.New("invalid BlueZ device address")
		}
		d.Address = strings.ToUpper(d.Address)
		if d.AddressType, err = property[string](p, "AddressType"); err != nil {
			return nil, nil, err
		}
		if d.AddressType != "public" && d.AddressType != "random" {
			return nil, nil, errors.New("invalid BlueZ device address type")
		}
		if d.Name, err = property[string](p, "Alias"); err != nil {
			return nil, nil, err
		}
		d.Name = strings.TrimSpace(d.Name)
		if !hasDeviceDisplayName(d.Name, d.Address) {
			continue
		}
		if d.Connected, err = property[bool](p, "Connected"); err != nil {
			return nil, nil, err
		}
		if d.Paired, err = property[bool](p, "Paired"); err != nil {
			return nil, nil, err
		}
		if d.ServicesResolved, err = property[bool](p, "ServicesResolved"); err != nil {
			return nil, nil, err
		}
		if _, exists := p["RSSI"]; exists {
			rssi, err := property[int16](p, "RSSI")
			if err != nil {
				return nil, nil, err
			}
			d.RSSI = &rssi
		}
		if _, exists := p["UUIDs"]; exists {
			d.UUIDs, err = property[[]string](p, "UUIDs")
			if err != nil {
				return nil, nil, err
			}
		}
		devices = append(devices, d)
	}
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].Connected != devices[j].Connected {
			return devices[i].Connected
		}
		return devices[i].Address < devices[j].Address
	})
	return a, devices, nil
}

func findDevice(data objects, hci, address string) (dbus.ObjectPath, device, error) {
	_, devices, err := snapshot(data, hci)
	if err != nil {
		return "", device{}, err
	}
	var selected device
	matches := 0
	for _, d := range devices {
		if d.Address == address {
			selected = d
			matches++
		}
	}
	if matches != 1 {
		return "", device{}, errors.New("selected device is absent or ambiguous; scan again")
	}
	path := dbus.ObjectPath("/org/bluez/" + hci + "/dev_" + strings.ReplaceAll(address, ":", "_"))
	p, ok := data[path][deviceIface]
	if !ok {
		return "", device{}, errors.New("selected device path is not available")
	}
	actual, err := property[string](p, "Address")
	if err != nil || !strings.EqualFold(actual, address) {
		return "", device{}, errors.New("selected device identity changed")
	}
	return path, selected, nil
}

func scan(ctx context.Context, bus bluetoothBus, hci string, seconds int) (err error) {
	path := dbus.ObjectPath("/org/bluez/" + hci)
	filter := map[string]dbus.Variant{
		"Transport": dbus.MakeVariant("le"), "DuplicateData": dbus.MakeVariant(false),
		"Discoverable": dbus.MakeVariant(false),
	}
	if err := bus.Call(ctx, path, adapterIface+".SetDiscoveryFilter", filter); err != nil {
		return err
	}
	if err := bus.Call(ctx, path, adapterIface+".StartDiscovery"); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err = errors.Join(err, bus.Call(cleanup, path, adapterIface+".StopDiscovery"))
	}()
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func changeConnection(ctx context.Context, bus bluetoothBus, hci, address string, connect bool) error {
	data, err := bus.Objects(ctx)
	if err != nil {
		return err
	}
	path, d, err := findDevice(data, hci, address)
	if err != nil {
		return err
	}
	if !connect {
		if !d.Connected {
			return nil
		}
		if err := bus.Call(ctx, path, deviceIface+".Disconnect"); err != nil {
			return err
		}
		current, err := bus.Objects(ctx)
		if err != nil {
			return err
		}
		p, exists := current[path][deviceIface]
		if !exists {
			return nil
		}
		connected, err := property[bool](p, "Connected")
		if err != nil {
			return err
		}
		if connected {
			return errors.New("BlueZ still reports the selected device as connected")
		}
		return nil
	}
	if d.Connected && d.ServicesResolved {
		return nil
	}
	started := !d.Connected
	operation, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if started {
		err = bus.Call(operation, path, deviceIface+".Connect")
	}
	if err == nil {
		err = bus.WaitConnected(operation, path)
	}
	if err != nil && started {
		cleanup, finish := context.WithTimeout(context.Background(), 2*time.Second)
		defer finish()
		if cleanupErr := bus.Call(cleanup, path, deviceIface+".Disconnect"); cleanupErr != nil {
			var dbusErr dbus.Error
			if !errors.As(cleanupErr, &dbusErr) || dbusErr.Name != "org.bluez.Error.NotConnected" {
				return errors.Join(err, fmt.Errorf("cancel connection: %w", cleanupErr))
			}
		}
	}
	return err
}
