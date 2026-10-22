// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
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
	csrCommandTimeout  = 5 * time.Second
	anaFreqPendingPath = runDir + "/ana-freq.pending"
	hciExtensionPath   = "/sys/module/hci_uart/parameters/hciextn"
)

type csrCommandRunner func(context.Context, string, ...string) (string, error)

type hciTrace struct {
	commands     [][]byte
	vendorEvents [][]byte
}

func enableRBRHCI(parent context.Context, hci string, run csrCommandRunner) error {
	if !hciPattern.MatchString(hci) || run == nil {
		return errors.New("owned HCI command runner is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, csrCommandTimeout)
	defer cancel()
	if _, err := run(ctx, "hciconfig", hci, "up"); err != nil {
		return fmt.Errorf("bring %s up for CSR access: %w", hci, err)
	}
	return nil
}

func disableRBRHCI(parent context.Context, hci string, run csrCommandRunner) error {
	if !hciPattern.MatchString(hci) || run == nil {
		return errors.New("owned HCI command runner is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, csrCommandTimeout)
	defer cancel()
	if _, err := run(ctx, "hciconfig", hci, "down"); err != nil {
		return fmt.Errorf("bring %s down before BlueZ startup: %w", hci, err)
	}
	output, err := run(ctx, "hciconfig", hci)
	if err != nil {
		return fmt.Errorf("confirm %s is down: %w", hci, err)
	}
	if !strings.Contains(output, "DOWN") {
		return fmt.Errorf("%s did not report DOWN before BlueZ startup", hci)
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

func csrCommandArgs(hci string, pdu []byte) ([]string, error) {
	if !hciPattern.MatchString(hci) {
		return nil, errors.New("invalid HCI controller identity")
	}
	args := []string{"-i", hci, "cmd", "0x3f", "0x0000"}
	for _, value := range pdu {
		args = append(args, fmt.Sprintf("0x%02x", value))
	}
	return args, nil
}

func parseHCITrace(output string) (hciTrace, error) {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	var trace hciTrace
	for index := 0; index < len(lines); index++ {
		header := strings.TrimSpace(lines[index])
		commandBlock := strings.HasPrefix(header, "< HCI Command:")
		eventBlock := strings.HasPrefix(header, "> HCI Event: 0xff ")
		if !commandBlock && !eventBlock {
			continue
		}
		marker := strings.LastIndex(header, "plen ")
		if marker < 0 {
			return hciTrace{}, errors.New("HCI trace header omits payload length")
		}
		length, err := strconv.Atoi(strings.TrimSpace(header[marker+len("plen "):]))
		if err != nil || length < 0 || length > 255 {
			return hciTrace{}, errors.New("HCI trace payload length is invalid")
		}
		data := make([]byte, 0, length)
		for index+1 < len(lines) && len(data) < length {
			index++
			line := strings.TrimSpace(lines[index])
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "< HCI ") || strings.HasPrefix(line, "> HCI ") {
				return hciTrace{}, errors.New("HCI trace payload is incomplete")
			}
			for _, token := range strings.Fields(line) {
				if len(token) != 2 {
					return hciTrace{}, errors.New("HCI trace contains a malformed byte")
				}
				value, err := strconv.ParseUint(token, 16, 8)
				if err != nil {
					return hciTrace{}, errors.New("HCI trace contains a malformed byte")
				}
				data = append(data, byte(value))
			}
		}
		if len(data) != length {
			return hciTrace{}, errors.New("HCI trace payload length does not match header")
		}
		if commandBlock {
			trace.commands = append(trace.commands, data)
		} else {
			trace.vendorEvents = append(trace.vendorEvents, data)
		}
	}
	if len(trace.commands) == 0 {
		return hciTrace{}, errors.New("hcitool did not report the dispatched HCI command")
	}
	return trace, nil
}

func runCSRCommand(ctx context.Context, hci string, pdu []byte, run csrCommandRunner) (hciTrace, error) {
	if run == nil {
		return hciTrace{}, errors.New("CSR command runner is unavailable")
	}
	args, err := csrCommandArgs(hci, pdu)
	if err != nil {
		return hciTrace{}, err
	}
	output, runErr := run(ctx, "hcitool", args...)
	trace, parseErr := parseHCITrace(output)
	if parseErr != nil {
		return hciTrace{}, errors.Join(runErr, parseErr)
	}
	if len(trace.commands) != 1 || !bytes.Equal(trace.commands[0], pdu) {
		return hciTrace{}, errors.Join(runErr,
			errors.New("hcitool command echo does not match the requested CSR PDU"))
	}
	return trace, runErr
}

func csrResponseWords(trace hciTrace) ([9]uint16, error) {
	var words [9]uint16
	if len(trace.vendorEvents) != 1 {
		return words, errors.New("CSR response is missing or duplicated")
	}
	event := trace.vendorEvents[0]
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
	trace, err := runCSRCommand(ctx, hci, pdu, run)
	if err != nil {
		return 0, 0, fmt.Errorf("read CSR ANA_FREQ: %w", err)
	}
	words, err := csrResponseWords(trace)
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
	setTrace, err := runCSRCommand(setContext, hci, setPDU, run)
	finishSet()
	if err != nil {
		return fmt.Errorf("set CSR ANA_FREQ: %w", err)
	}
	words, err := csrResponseWords(setTrace)
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
	trace, err := runCSRCommand(ctx, hci, warmPDU, run)
	if len(trace.commands) != 1 || !bytes.Equal(trace.commands[0], warmPDU) {
		return errors.Join(err, errors.New("CSR warm reset dispatch was not proven by an exact command echo"))
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

func checkANAFrequencyMarker(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("a previous ANA_FREQ transaction did not finish; reboot the router before Start")
}

func writeANAFrequencyMarker(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString("ANA_FREQ transaction pending\n")
	return errors.Join(writeErr, f.Close())
}

func clearANAFrequencyMarker(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
