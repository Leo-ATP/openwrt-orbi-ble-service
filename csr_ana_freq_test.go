// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func csrTestOutput(pdu, response []byte) string {
	var output strings.Builder
	fmt.Fprintf(&output, "< HCI Command: ogf 0x3f, ocf 0x0000, plen %d\n  ", len(pdu))
	for _, value := range pdu {
		fmt.Fprintf(&output, "%02X ", value)
	}
	output.WriteByte('\n')
	if response != nil {
		fmt.Fprintf(&output, "> HCI Event: 0xff plen %d\n  ", len(response))
		for _, value := range response {
			fmt.Fprintf(&output, "%02X ", value)
		}
		output.WriteByte('\n')
	}
	return output.String()
}

func csrTestPDU(t *testing.T, args []string) []byte {
	t.Helper()
	if len(args) != 24 || args[0] != "-i" || args[2] != "cmd" ||
		args[3] != "0x3f" || args[4] != "0x0000" {
		t.Fatalf("unexpected CSR command: %v", args)
	}
	pdu := make([]byte, len(args)-5)
	for i, text := range args[5:] {
		value, err := strconv.ParseUint(text, 0, 8)
		if err != nil {
			t.Fatal(err)
		}
		pdu[i] = byte(value)
	}
	return pdu
}

func csrTestReply(sequence, status, value uint16) []byte {
	return csrPDU(1, 9, sequence, csrPSVarID, status,
		csrANAFrequencyKey, 1, csrPSRAMStore, value)
}

func TestCSRReadMatchesEveryIdentityField(t *testing.T) {
	const sequence = 0x1001
	for _, word := range []int{0, 1, 2, 3, 5, 6, 7} {
		t.Run(fmt.Sprint(word), func(t *testing.T) {
			run := func(_ context.Context, _ string, args ...string) (string, error) {
				pdu := csrTestPDU(t, args)
				reply := csrTestReply(sequence, 0, csrANAFrequency)
				reply[1+word*2] ^= 1
				return csrTestOutput(pdu, reply), nil
			}
			if _, _, err := readRBRANAFrequency(context.Background(), "hci4", sequence, run); err == nil {
				t.Fatalf("mismatched word %d accepted", word)
			}
		})
	}
	run := func(_ context.Context, _ string, args ...string) (string, error) {
		return csrTestOutput(csrTestPDU(t, args), csrTestReply(sequence, 8, 0)), nil
	}
	status, _, err := readRBRANAFrequency(context.Background(), "hci4", sequence, run)
	if err != nil || status != 8 {
		t.Fatalf("pre-SET status was hidden: status=%d err=%v", status, err)
	}
}

func TestCSRSetRejectsChangedPayloadBeforeWarm(t *testing.T) {
	for word := 0; word < 9; word++ {
		t.Run(fmt.Sprint(word), func(t *testing.T) {
			calls := 0
			run := func(_ context.Context, _ string, args ...string) (string, error) {
				calls++
				pdu := csrTestPDU(t, args)
				reply := csrTestReply(0x1002, 0, csrANAFrequency)
				reply[1+word*2] ^= 1
				return csrTestOutput(pdu, reply), nil
			}
			err := setRBRANAFrequency(context.Background(), "hci4", 0x1002, run)
			if err == nil || calls != 1 {
				t.Fatalf("mismatched SET word %d: calls=%d err=%v", word, calls, err)
			}
		})
	}
}

func TestCSRRejectsMissingDuplicateAndMalformedReplies(t *testing.T) {
	pdu := csrPDU(0, 9, 1, csrPSVarID, 0, csrANAFrequencyKey, 1, csrPSRAMStore, 0)
	reply := csrTestReply(1, 0, csrANAFrequency)
	for _, output := range []string{
		"",
		csrTestOutput(pdu, nil),
		csrTestOutput(pdu, reply[:18]),
		csrTestOutput(pdu, reply) + csrTestOutput(pdu, reply),
		strings.Replace(csrTestOutput(pdu, reply), "plen 19", "plen 18", 1),
		strings.Replace(csrTestOutput(pdu, reply), "C2", "ZZ", 1),
	} {
		run := func(context.Context, string, ...string) (string, error) { return output, nil }
		if _, _, err := readRBRANAFrequency(context.Background(), "hci4", 1, run); err == nil {
			t.Fatalf("invalid trace accepted: %q", output)
		}
	}
}

func TestWarmRequiresExactDispatchAndBoundedTimeout(t *testing.T) {
	old := csrCommandTimeout
	csrCommandTimeout = 5 * time.Millisecond
	t.Cleanup(func() { csrCommandTimeout = old })
	for _, tc := range []struct {
		echo, timeout, want bool
	}{
		{true, true, true}, {false, true, false}, {true, false, false},
	} {
		run := func(ctx context.Context, _ string, args ...string) (string, error) {
			var out string
			if tc.echo {
				out = csrTestOutput(csrTestPDU(t, args), nil)
			}
			if tc.timeout {
				<-ctx.Done()
				return out, ctx.Err()
			}
			return out, errors.New("injected command failure")
		}
		err := warmRBRController(context.Background(), "hci4", 7, run)
		if (err == nil) != tc.want {
			t.Fatalf("case=%+v err=%v", tc, err)
		}
	}
}

type fakeTransport struct {
	hci, port          string
	attachFn, detachFn func(context.Context) error
	verifyErr          error
}

func (f *fakeTransport) controller() string      { return f.hci }
func (f *fakeTransport) tty() string             { return f.port }
func (f *fakeTransport) verify() error           { return f.verifyErr }
func (f *fakeTransport) exited() <-chan struct{} { return nil }
func (f *fakeTransport) attach(ctx context.Context) error {
	if f.attachFn != nil {
		return f.attachFn(ctx)
	}
	return nil
}
func (f *fakeTransport) detach(ctx context.Context) error {
	if f.detachFn != nil {
		return f.detachFn(ctx)
	}
	return nil
}

func testANAPaths(t *testing.T) {
	t.Helper()
	oldMarker, oldExtension, oldTimeout := anaFreqPendingPath, hciExtensionPath, csrCommandTimeout
	dir := t.TempDir()
	anaFreqPendingPath, hciExtensionPath = filepath.Join(dir, "pending"), filepath.Join(dir, "hciextn")
	csrCommandTimeout = 5 * time.Millisecond
	if err := os.WriteFile(hciExtensionPath, []byte("Y\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		anaFreqPendingPath, hciExtensionPath, csrCommandTimeout = oldMarker, oldExtension, oldTimeout
	})
}

func TestANATransactionAndFailureMarker(t *testing.T) {
	for _, failurePoint := range []string{"", "pre-get", "set", "readback", "detach", "attach", "tty", "post-get", "down", "owner"} {
		t.Run(failurePoint, func(t *testing.T) {
			testANAPaths(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := &fakeTransport{hci: "hci4", port: "ttyMSM7"}
			r := &runtime{transport: transport}
			var calls []string
			transport.detachFn = func(ctx context.Context) error {
				calls = append(calls, "detach")
				if ctx.Err() != nil {
					t.Fatal("shutdown cancelled protected detach")
				}
				if failurePoint == "detach" {
					return errors.New("injected detach failure")
				}
				return nil
			}
			transport.attachFn = func(ctx context.Context) error {
				calls = append(calls, "attach")
				if ctx.Err() != nil {
					t.Fatal("shutdown cancelled protected reattach")
				}
				if failurePoint == "attach" {
					return errors.New("injected attach failure")
				}
				transport.hci = "hci5"
				if failurePoint == "tty" {
					transport.port = "ttyMSM8"
				}
				if failurePoint == "owner" {
					transport.verifyErr = errors.New("controller now belongs to someone else")
				}
				return nil
			}
			gets, sets, warms := 0, 0, 0
			r.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("unbounded initialization command")
				}
				switch name {
				case "sync":
					calls = append(calls, name)
					return "", nil
				case "hciconfig":
					if args[0] != transport.hci {
						t.Fatal("command reached the previous HCI index")
					}
					if len(args) == 1 {
						calls = append(calls, "inspect")
						return transport.hci + ": Type: Primary Bus: UART\n\tDOWN", nil
					}
					calls = append(calls, args[1])
					if args[1] == "down" && failurePoint == "down" {
						return "", errors.New("injected down failure")
					}
					return "", nil
				case "hcitool":
					pdu := csrTestPDU(t, args)
					kind := binary.LittleEndian.Uint16(pdu[1:3])
					sequence := binary.LittleEndian.Uint16(pdu[5:7])
					varID := binary.LittleEndian.Uint16(pdu[7:9])
					if varID == csrWarmResetVarID {
						warms++
						calls = append(calls, "warm")
						<-ctx.Done()
						return csrTestOutput(pdu, nil), ctx.Err()
					}
					if binary.LittleEndian.Uint16(pdu[15:17]) != csrPSRAMStore {
						t.Fatal("non-volatile CSR store requested")
					}
					value := csrANAFrequency
					if kind == 2 {
						sets++
						calls = append(calls, "set")
						if _, err := os.Stat(anaFreqPendingPath); err != nil {
							t.Fatal("SET started before pending marker")
						}
						cancel()
						if failurePoint == "set" {
							return "", errors.New("injected SET failure")
						}
					} else {
						gets++
						calls = append(calls, "get")
						if gets == 1 && failurePoint == "pre-get" {
							return "", errors.New("injected initial GET failure")
						}
						if (gets == 2 && failurePoint == "readback") || (gets == 3 && failurePoint == "post-get") {
							value = 25000
						}
					}
					return csrTestOutput(pdu, csrTestReply(sequence, 0, value)), nil
				}
				t.Fatalf("unexpected command %s %v", name, args)
				return "", nil
			}
			err := r.prepareANAFrequency(ctx)
			if (err == nil) != (failurePoint == "") {
				t.Fatalf("point=%s calls=%v err=%v", failurePoint, calls, err)
			}
			if sets > 1 || warms > 1 {
				t.Fatalf("mutation repeated: set=%d warm=%d", sets, warms)
			}
			if failurePoint == "" {
				expected := []string{"up", "get", "sync", "set", "get", "warm", "detach", "attach", "up", "get", "down", "inspect"}
				if !reflect.DeepEqual(calls, expected) {
					t.Fatalf("sequence=%v", calls)
				}
			}
			_, markerErr := os.Stat(anaFreqPendingPath)
			pending := failurePoint != "" && failurePoint != "pre-get"
			if pending != (markerErr == nil) {
				t.Fatalf("point=%s marker=%v", failurePoint, markerErr)
			}
			if pending {
				r.runCommand = func(context.Context, string, ...string) (string, error) {
					t.Fatal("pending mutation allowed another command")
					return "", nil
				}
				if err := r.prepareANAFrequency(context.Background()); err == nil {
					t.Fatal("pending transaction accepted")
				}
			}
		})
	}
}

func TestOwnedRawCommandsFailClosed(t *testing.T) {
	r := &runtime{
		transport: &fakeTransport{verifyErr: errors.New("foreign HCI")},
		runCommand: func(context.Context, string, ...string) (string, error) {
			t.Fatal("raw command reached a foreign controller")
			return "", nil
		},
	}
	for _, name := range []string{"hcitool", "hciconfig"} {
		if _, err := r.run(context.Background(), name, "hci4"); err == nil {
			t.Fatal("foreign controller accepted")
		}
	}
}
