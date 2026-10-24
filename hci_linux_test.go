// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"encoding/binary"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestMatchingCSRHeader(t *testing.T) {
	request := csrPDU(0, 9, 0x1234, csrPSVarID, 0,
		csrANAFrequencyKey, 1, csrPSRAMStore, 0)
	response := csrTestReply(0x1234, 0, csrANAFrequency)
	if !matchingCSRHeader(response, request) {
		t.Fatal("matching CSR response was rejected")
	}
	response[5] ^= 1
	if matchingCSRHeader(response, request) {
		t.Fatal("wrong CSR sequence was accepted")
	}
}

func TestReadVendorEvent(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	payload := csrTestReply(7, 0, csrANAFrequency)
	packet := append([]byte{hciEventPacket, hciVendorEvent, byte(len(payload))}, payload...)
	go func() {
		_, _ = writer.Write(packet)
		_ = writer.Close()
	}()
	event, err := readVendorEvent(reader)
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(event[5:7]) != 7 {
		t.Fatalf("wrong event: %x", event)
	}
}

func TestNonblockingSocketSupportsDeadline(t *testing.T) {
	sockets, err := syscall.Socketpair(syscall.AF_UNIX,
		syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(sockets[1])
	file := os.NewFile(uintptr(sockets[0]), "deadline-test")
	if file == nil {
		t.Fatal("wrap socket")
	}
	defer file.Close()
	if err := file.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("nonblocking socket deadline: %v", err)
	}
}
