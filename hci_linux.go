//go:build linux

// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const (
	afBluetooth = 31
	btProtoHCI  = 1
	solHCI      = 0
	hciFilter   = 2

	hciCommandPacket = 0x01
	hciEventPacket   = 0x04
	hciVendorEvent   = 0xff
	hciChannelRaw    = 0x00
)

type rawSockaddrHCI struct {
	Family  uint16
	Device  uint16
	Channel uint16
}

type rawHCIFilter struct {
	TypeMask  uint32
	EventMask [2]uint32
	Opcode    uint16
	Padding   uint16
}

func openRawHCI(name string) (*os.File, error) {
	index, err := strconv.Atoi(strings.TrimPrefix(name, "hci"))
	if err != nil || index < 0 || index > 0xffff || !hciPattern.MatchString(name) {
		return nil, errors.New("invalid HCI controller identity")
	}
	fd, err := syscall.Socket(afBluetooth,
		syscall.SOCK_RAW|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, btProtoHCI)
	if err != nil {
		return nil, err
	}
	closeFD := true
	defer func() {
		if closeFD {
			syscall.Close(fd)
		}
	}()
	address := rawSockaddrHCI{
		Family: afBluetooth, Device: uint16(index), Channel: hciChannelRaw,
	}
	_, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd),
		uintptr(unsafe.Pointer(&address)), unsafe.Sizeof(address))
	runtime.KeepAlive(&address)
	if errno != 0 {
		return nil, fmt.Errorf("bind raw HCI socket: %w", errno)
	}
	filter := rawHCIFilter{
		TypeMask:  1 << hciEventPacket,
		EventMask: [2]uint32{0, 1 << 31},
	}
	_, _, errno = syscall.Syscall6(syscall.SYS_SETSOCKOPT, uintptr(fd),
		solHCI, hciFilter, uintptr(unsafe.Pointer(&filter)), unsafe.Sizeof(filter), 0)
	runtime.KeepAlive(&filter)
	if errno != 0 {
		return nil, fmt.Errorf("set raw HCI filter: %w", errno)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		return nil, errors.New("wrap raw HCI socket")
	}
	closeFD = false
	return file, nil
}

func writeHCIPacket(file *os.File, packet []byte) error {
	for {
		written, err := file.Write(packet)
		if written == 0 && (errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN)) {
			continue
		}
		if err != nil {
			return err
		}
		if written != len(packet) {
			return fmt.Errorf("raw HCI socket wrote %d of %d bytes", written, len(packet))
		}
		return nil
	}
}

func readVendorEvent(file *os.File) ([]byte, error) {
	packet := make([]byte, 260)
	length, err := file.Read(packet)
	if err != nil {
		return nil, err
	}
	if length < 3 || packet[0] != hciEventPacket || packet[1] != hciVendorEvent {
		return nil, errors.New("raw HCI socket returned an unexpected packet")
	}
	payloadLength := int(packet[2])
	if length != payloadLength+3 {
		return nil, errors.New("raw HCI vendor event length mismatch")
	}
	return append([]byte(nil), packet[3:length]...), nil
}

func matchingCSRHeader(event, pdu []byte) bool {
	return len(event) >= 9 && len(pdu) >= 9 && event[0] == 0xc2 &&
		binary.LittleEndian.Uint16(event[5:7]) ==
			binary.LittleEndian.Uint16(pdu[5:7]) &&
		binary.LittleEndian.Uint16(event[7:9]) ==
			binary.LittleEndian.Uint16(pdu[7:9])
}

func hciCSRCommand(ctx context.Context, hci string, pdu []byte) (csrCommandResult, error) {
	if len(pdu) > 255 {
		return csrCommandResult{}, errors.New("CSR command is too large")
	}
	file, err := openRawHCI(hci)
	if err != nil {
		return csrCommandResult{}, err
	}
	defer file.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := file.SetDeadline(deadline); err != nil {
			return csrCommandResult{}, fmt.Errorf("set raw HCI deadline: %w", err)
		}
	}
	stopClose := context.AfterFunc(ctx, func() { file.Close() })
	defer stopClose()
	packet := make([]byte, 4+len(pdu))
	packet[0] = hciCommandPacket
	binary.LittleEndian.PutUint16(packet[1:3], 0xfc00)
	packet[3] = byte(len(pdu))
	copy(packet[4:], pdu)
	if err := writeHCIPacket(file, packet); err != nil {
		return csrCommandResult{}, fmt.Errorf("dispatch CSR command: %w", err)
	}
	result := csrCommandResult{Dispatched: true}
	for {
		event, err := readVendorEvent(file)
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, fmt.Errorf("read CSR response: %w", err)
		}
		if matchingCSRHeader(event, pdu) {
			result.VendorEvent = event
			return result, nil
		}
	}
}
