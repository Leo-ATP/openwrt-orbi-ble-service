// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"
)

func csrTestReply(sequence, status, value uint16) []byte {
	return csrPDU(1, 9, sequence, csrPSVarID, status,
		csrANAFrequencyKey, 1, csrPSRAMStore, value)
}

func csrTestSequence(t *testing.T, pdu []byte) uint16 {
	t.Helper()
	if len(pdu) != 19 || pdu[0] != 0xc2 {
		t.Fatalf("unexpected CSR PDU: %x", pdu)
	}
	return binary.LittleEndian.Uint16(pdu[5:7])
}

func TestCSRReadMatchesEveryIdentityField(t *testing.T) {
	const sequence = 0x1001
	for _, word := range []int{0, 1, 2, 3, 5, 6, 7} {
		t.Run(fmt.Sprint(word), func(t *testing.T) {
			run := func(_ context.Context, hci string,
				pdu []byte) (csrCommandResult, error) {
				if hci != "hci4" || csrTestSequence(t, pdu) != sequence {
					t.Fatal("unexpected CSR read request")
				}
				reply := csrTestReply(sequence, 0, csrANAFrequency)
				reply[1+word*2] ^= 1
				return csrCommandResult{Dispatched: true, VendorEvent: reply}, nil
			}
			if _, _, err := readRBRANAFrequency(
				context.Background(), "hci4", sequence, run); err == nil {
				t.Fatalf("mismatched word %d accepted", word)
			}
		})
	}
	run := func(_ context.Context, _ string,
		pdu []byte) (csrCommandResult, error) {
		return csrCommandResult{
			Dispatched:  true,
			VendorEvent: csrTestReply(csrTestSequence(t, pdu), 8, 0),
		}, nil
	}
	status, _, err := readRBRANAFrequency(
		context.Background(), "hci4", sequence, run)
	if err != nil || status != 8 {
		t.Fatalf("pre-SET status was hidden: status=%d err=%v", status, err)
	}
}

func TestCSRSetRejectsChangedPayloadBeforeWarm(t *testing.T) {
	for word := 0; word < 9; word++ {
		t.Run(fmt.Sprint(word), func(t *testing.T) {
			calls := 0
			run := func(_ context.Context, _ string,
				pdu []byte) (csrCommandResult, error) {
				calls++
				sequence := csrTestSequence(t, pdu)
				reply := csrTestReply(sequence, 0, csrANAFrequency)
				reply[1+word*2] ^= 1
				return csrCommandResult{Dispatched: true, VendorEvent: reply}, nil
			}
			err := setRBRANAFrequency(context.Background(), "hci4", 0x1002, run)
			if err == nil || calls != 1 {
				t.Fatalf("mismatched SET word %d: calls=%d err=%v", word, calls, err)
			}
		})
	}
}

func TestCSRRejectsMissingMalformedAndUndispatchedReplies(t *testing.T) {
	for _, result := range []csrCommandResult{
		{},
		{Dispatched: true},
		{Dispatched: true, VendorEvent: csrTestReply(1, 0, csrANAFrequency)[:18]},
		{Dispatched: true, VendorEvent: append([]byte{0}, csrTestReply(1, 0, csrANAFrequency)[1:]...)},
	} {
		run := func(context.Context, string, []byte) (csrCommandResult, error) {
			return result, nil
		}
		if _, _, err := readRBRANAFrequency(
			context.Background(), "hci4", 1, run); err == nil {
			t.Fatalf("invalid result accepted: %+v", result)
		}
	}
}

func TestWarmRequiresProvenDispatchAndBoundedTimeout(t *testing.T) {
	old := csrCommandTimeout
	csrCommandTimeout = 5 * time.Millisecond
	t.Cleanup(func() { csrCommandTimeout = old })
	for _, tc := range []struct {
		dispatched, timeout, want bool
	}{
		{true, true, true}, {false, true, false}, {true, false, false},
	} {
		run := func(ctx context.Context, _ string,
			pdu []byte) (csrCommandResult, error) {
			result := csrCommandResult{Dispatched: tc.dispatched}
			if tc.timeout {
				<-ctx.Done()
				return result, ctx.Err()
			}
			if csrTestSequence(t, pdu) != 7 {
				t.Fatal("unexpected warm-reset sequence")
			}
			return result, errors.New("injected command failure")
		}
		err := warmRBRController(context.Background(), "hci4", 7, run)
		if (err == nil) != tc.want {
			t.Fatalf("case=%+v err=%v", tc, err)
		}
	}
}
