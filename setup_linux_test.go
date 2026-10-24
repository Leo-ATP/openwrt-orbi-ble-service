// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func setupFixture(t *testing.T) (string, *uart, string, string) {
	t.Helper()
	sysfs, u := uartFixture(t, "netgear,rbs50", true)
	fixtureHCI(t, sysfs, u, "hci4")
	extension := filepath.Join(t.TempDir(), "hciextn")
	if err := os.WriteFile(extension, []byte("Y\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return sysfs, u, filepath.Join(t.TempDir(), "ana-freq.pending"), extension
}

func replaceHCI(t *testing.T, sysfs string, u *uart, oldName, newName string) {
	t.Helper()
	if err := os.Remove(filepath.Join(sysfs, "class", "bluetooth", oldName)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(u.path, "bluetooth", oldName)); err != nil {
		t.Fatal(err)
	}
	fixtureHCI(t, sysfs, u, newName)
}

func TestPrepareAndVerifyANAFrequency(t *testing.T) {
	sysfs, u, pendingPath, extensionPath := setupFixture(t)
	oldTimeout := csrCommandTimeout
	csrCommandTimeout = 5 * time.Millisecond
	t.Cleanup(func() { csrCommandTimeout = oldTimeout })
	var calls []string
	gets := 0
	runCommand := func(ctx context.Context, name string, args ...string) (string, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded setup command")
		}
		switch name {
		case "sync":
			calls = append(calls, "sync")
			return "", nil
		case "hciconfig":
			hci := args[0]
			if len(args) == 1 {
				calls = append(calls, "inspect:"+hci)
				return hci + ": Type: Primary Bus: UART\n\tDOWN", nil
			}
			calls = append(calls, args[1]+":"+hci)
			return "", nil
		default:
			t.Fatalf("unexpected command %s", name)
			return "", nil
		}
	}
	runCSR := func(ctx context.Context, hci string,
		pdu []byte) (csrCommandResult, error) {
		kind := binary.LittleEndian.Uint16(pdu[1:3])
		sequence := csrTestSequence(t, pdu)
		varID := binary.LittleEndian.Uint16(pdu[7:9])
		if varID == csrWarmResetVarID {
			calls = append(calls, "warm:"+hci)
			<-ctx.Done()
			return csrCommandResult{Dispatched: true}, ctx.Err()
		}
		if binary.LittleEndian.Uint16(pdu[15:17]) != csrPSRAMStore {
			t.Fatal("non-volatile CSR store requested")
		}
		if kind == 2 {
			calls = append(calls, "set:"+hci)
			if _, err := os.Stat(pendingPath); err != nil {
				t.Fatal("SET started before the pending marker")
			}
		} else {
			gets++
			calls = append(calls, "get:"+hci)
		}
		return csrCommandResult{
			Dispatched:  true,
			VendorEvent: csrTestReply(sequence, 0, csrANAFrequency),
		}, nil
	}
	session, err := newControllerSession(sysfs, runCommand, runCSR)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareANAFrequency(context.Background(), session, pendingPath, extensionPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatal("successful mutation did not retain its pending marker")
	}
	replaceHCI(t, sysfs, u, "hci4", "hci5")
	session, err = newControllerSession(sysfs, runCommand, runCSR)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyANAFrequency(context.Background(), session, pendingPath, extensionPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed transaction retained its marker: %v", err)
	}
	expected := []string{
		"up:hci4", "get:hci4", "sync", "set:hci4", "get:hci4", "warm:hci4",
		"up:hci5", "get:hci5", "down:hci5", "inspect:hci5",
	}
	if !reflect.DeepEqual(calls, expected) || gets != 3 {
		t.Fatalf("calls=%v gets=%d", calls, gets)
	}
}

func TestPrepareMarkerBoundaries(t *testing.T) {
	for _, failure := range []string{"pre-get", "sync", "set"} {
		t.Run(failure, func(t *testing.T) {
			sysfs, _, pendingPath, extensionPath := setupFixture(t)
			runCommand := func(_ context.Context, name string, _ ...string) (string, error) {
				switch name {
				case "hciconfig":
					return "", nil
				case "sync":
					if failure == "sync" {
						return "", errors.New("injected sync failure")
					}
					return "", nil
				default:
					return "", errors.New("unexpected command")
				}
			}
			runCSR := func(_ context.Context, _ string,
				pdu []byte) (csrCommandResult, error) {
				kind := binary.LittleEndian.Uint16(pdu[1:3])
				sequence := csrTestSequence(t, pdu)
				if failure == "pre-get" && kind == 0 {
					return csrCommandResult{}, errors.New("injected initial GET failure")
				}
				if failure == "set" && kind == 2 {
					return csrCommandResult{}, errors.New("injected SET failure")
				}
				return csrCommandResult{
					Dispatched:  true,
					VendorEvent: csrTestReply(sequence, 0, csrANAFrequency),
				}, nil
			}
			session, err := newControllerSession(sysfs, runCommand, runCSR)
			if err != nil {
				t.Fatal(err)
			}
			if err := prepareANAFrequency(context.Background(), session,
				pendingPath, extensionPath); err == nil {
				t.Fatal("injected failure was hidden")
			}
			_, markerErr := os.Stat(pendingPath)
			wantMarker := failure == "set"
			if wantMarker != (markerErr == nil) {
				t.Fatalf("failure=%s marker=%v", failure, markerErr)
			}
		})
	}
}

func TestPendingTransactionBlocksAnotherPrepare(t *testing.T) {
	sysfs, _, pendingPath, extensionPath := setupFixture(t)
	if err := os.MkdirAll(filepath.Dir(pendingPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runCommand := func(context.Context, string, ...string) (string, error) {
		t.Fatal("pending transaction reached a command")
		return "", nil
	}
	runCSR := func(context.Context, string, []byte) (csrCommandResult, error) {
		t.Fatal("pending transaction reached a CSR command")
		return csrCommandResult{}, nil
	}
	session, err := newControllerSession(sysfs, runCommand, runCSR)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareANAFrequency(context.Background(), session,
		pendingPath, extensionPath); err == nil {
		t.Fatal("pending transaction was accepted")
	}
}

func TestVerifyFailureRetainsPendingMarker(t *testing.T) {
	for _, failure := range []string{"post-get", "down"} {
		t.Run(failure, func(t *testing.T) {
			sysfs, u, pendingPath, extensionPath := setupFixture(t)
			pending, err := pendingForUART(u, 7)
			if err != nil {
				t.Fatal(err)
			}
			if err := writePendingTransaction(pendingPath, pending); err != nil {
				t.Fatal(err)
			}
			runCommand := func(_ context.Context, name string, args ...string) (string, error) {
				switch name {
				case "hciconfig":
					if len(args) > 1 && args[1] == "down" && failure == "down" {
						return "", errors.New("injected down failure")
					}
					if len(args) == 1 {
						return args[0] + ": Type: Primary Bus: UART\n\tDOWN", nil
					}
					return "", nil
				default:
					return "", errors.New("unexpected command")
				}
			}
			runCSR := func(_ context.Context, _ string,
				pdu []byte) (csrCommandResult, error) {
				value := csrANAFrequency
				if failure == "post-get" {
					value = 25000
				}
				return csrCommandResult{
					Dispatched:  true,
					VendorEvent: csrTestReply(csrTestSequence(t, pdu), 0, value),
				}, nil
			}
			session, err := newControllerSession(sysfs, runCommand, runCSR)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyANAFrequency(context.Background(), session,
				pendingPath, extensionPath); err == nil {
				t.Fatal("injected verify failure was hidden")
			}
			if _, err := os.Stat(pendingPath); err != nil {
				t.Fatal("verify failure cleared the pending marker")
			}
		})
	}
}

func TestVerifyRejectsChangedUARTIdentity(t *testing.T) {
	sysfs, u, pendingPath, extensionPath := setupFixture(t)
	pending, err := pendingForUART(u, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePendingTransaction(pendingPath, pending); err != nil {
		t.Fatal(err)
	}
	pending.TTYPath += ".changed"
	raw, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	runCommand := func(context.Context, string, ...string) (string, error) {
		t.Fatal("changed UART reached a command")
		return "", nil
	}
	runCSR := func(context.Context, string, []byte) (csrCommandResult, error) {
		t.Fatal("changed UART reached a CSR command")
		return csrCommandResult{}, nil
	}
	session, err := newControllerSession(sysfs, runCommand, runCSR)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyANAFrequency(context.Background(), session, pendingPath, extensionPath)
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("changed UART error=%v", err)
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatal("identity failure cleared the pending marker")
	}
}

func TestProtectedStepFinishesAfterCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	protected, finish, err := protectedStep(parent, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	cancel()
	if protected.Err() != nil {
		t.Fatal("protected mutation inherited cancellation")
	}
	if _, _, err := protectedStep(parent, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("new mutation after cancellation: %v", err)
	}
}
