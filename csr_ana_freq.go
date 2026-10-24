// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	csrANAFrequency    uint16 = 26000
	csrANAFrequencyKey        = 0x01fe
	csrPSRAMStore             = 8
	csrPSVarID                = 0x7003
	csrWarmResetVarID         = 0x4002
)

var (
	csrCommandTimeout = 5 * time.Second
)

type csrCommandResult struct {
	Dispatched  bool
	VendorEvent []byte
}

type csrCommandRunner func(context.Context, string, []byte) (csrCommandResult, error)
type commandRunner func(context.Context, string, ...string) (string, error)

func enableRBRHCI(parent context.Context, hci string, run commandRunner) error {
	if !hciPattern.MatchString(hci) || run == nil {
		return errors.New("verified HCI command runner is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, csrCommandTimeout)
	defer cancel()
	if _, err := run(ctx, "hciconfig", hci, "up"); err != nil {
		return fmt.Errorf("bring %s up for CSR access: %w", hci, err)
	}
	return nil
}

func disableRBRHCI(parent context.Context, hci string, run commandRunner) error {
	if !hciPattern.MatchString(hci) || run == nil {
		return errors.New("verified HCI command runner is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, csrCommandTimeout)
	defer cancel()
	if _, err := run(ctx, "hciconfig", hci, "down"); err != nil {
		return fmt.Errorf("bring %s down before stack handoff: %w", hci, err)
	}
	output, err := run(ctx, "hciconfig", hci)
	if err != nil {
		return fmt.Errorf("confirm %s is down: %w", hci, err)
	}
	if !strings.Contains(output, "DOWN") {
		return fmt.Errorf("%s did not report DOWN before stack handoff", hci)
	}
	return nil
}

func csrPDU(words ...uint16) []byte {
	pdu := make([]byte, 1+len(words)*2)
	pdu[0] = 0xc2
	for index, word := range words {
		binary.LittleEndian.PutUint16(pdu[1+index*2:], word)
	}
	return pdu
}

func runCSRCommand(ctx context.Context, hci string, pdu []byte,
	run csrCommandRunner) (csrCommandResult, error) {
	if run == nil {
		return csrCommandResult{}, errors.New("CSR command runner is unavailable")
	}
	if !hciPattern.MatchString(hci) {
		return csrCommandResult{}, errors.New("invalid HCI controller identity")
	}
	result, err := run(ctx, hci, pdu)
	if !result.Dispatched {
		return result, errors.Join(err, errors.New("CSR command was not dispatched"))
	}
	return result, err
}

func csrResponseWords(result csrCommandResult) ([9]uint16, error) {
	var words [9]uint16
	event := result.VendorEvent
	if len(event) != 19 || event[0] != 0xc2 {
		return words, errors.New("CSR response has an invalid shape")
	}
	for index := range words {
		words[index] = binary.LittleEndian.Uint16(event[1+index*2:])
	}
	return words, nil
}

func matchingANAFrequency(words [9]uint16, sequence uint16) bool {
	return words[0] == 1 && words[1] == 9 && words[2] == sequence &&
		words[3] == csrPSVarID && words[5] == csrANAFrequencyKey &&
		words[6] == 1 && words[7] == csrPSRAMStore
}

func readRBRANAFrequency(parent context.Context, hci string, sequence uint16, run csrCommandRunner) (uint16, uint16, error) {
	pdu := csrPDU(0, 9, sequence, csrPSVarID, 0, csrANAFrequencyKey, 1, csrPSRAMStore, 0)
	ctx, cancel := context.WithTimeout(parent, csrCommandTimeout)
	defer cancel()
	result, err := runCSRCommand(ctx, hci, pdu, run)
	if err != nil {
		return 0, 0, fmt.Errorf("read CSR ANA_FREQ: %w", err)
	}
	words, err := csrResponseWords(result)
	if err != nil {
		return 0, 0, err
	}
	if !matchingANAFrequency(words, sequence) {
		return 0, 0, errors.New("CSR ANA_FREQ response fields do not match the request")
	}
	return words[4], words[8], nil
}

func setRBRANAFrequency(parent context.Context, hci string, sequence uint16, run csrCommandRunner) error {
	setPDU := csrPDU(2, 9, sequence, csrPSVarID, 0,
		csrANAFrequencyKey, 1, csrPSRAMStore, csrANAFrequency)
	setContext, finishSet := context.WithTimeout(parent, csrCommandTimeout)
	setResult, err := runCSRCommand(setContext, hci, setPDU, run)
	finishSet()
	if err != nil {
		return fmt.Errorf("set CSR ANA_FREQ: %w", err)
	}
	words, err := csrResponseWords(setResult)
	if err != nil {
		return err
	}
	// The prototype observed type 1 for SET as well as GET; match that exact reply.
	if !matchingANAFrequency(words, sequence) || words[4] != 0 || words[8] != csrANAFrequency {
		return errors.New("CSR ANA_FREQ SET response is not a successful matching response")
	}
	status, value, err := readRBRANAFrequency(parent, hci, sequence+1, run)
	if err != nil {
		return err
	}
	if status != 0 || value != csrANAFrequency {
		return fmt.Errorf("CSR ANA_FREQ PSRAM readback is status=%d value=%d", status, value)
	}
	return warmRBRController(parent, hci, sequence+2, run)
}

func warmRBRController(parent context.Context, hci string, sequence uint16, run csrCommandRunner) error {
	warmPDU := csrPDU(2, 9, sequence, csrWarmResetVarID, 0, 0, 0, 0, 0)
	ctx, cancel := context.WithTimeout(parent, csrCommandTimeout)
	defer cancel()
	result, err := runCSRCommand(ctx, hci, warmPDU, run)
	if !result.Dispatched {
		return errors.Join(err, errors.New("CSR warm reset was not dispatched"))
	}
	if err != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("dispatch CSR warm reset: %w", err)
	}
	return nil
}

func requireHCIExtension(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read HCI vendor extension state: %w", err)
	}
	if strings.TrimSpace(string(raw)) != "Y" {
		return errors.New("HCI vendor extensions are not enabled")
	}
	return nil
}
