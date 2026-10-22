// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var version = "0.1.0"

const (
	runDir     = "/var/run/ble-service"
	socketPath = runDir + "/control.sock"
	statePath  = runDir + "/state.json"
	busPath    = runDir + "/bus"
	initPath   = "/etc/init.d/ble"
	configDir  = "/usr/share/ble-service"
	maxInput   = 24576
)

var addressPattern = regexp.MustCompile(`^(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)
var stateFilePath = statePath

type request struct {
	Action  string `json:"action"`
	Address string `json:"address,omitempty"`
	Seconds int    `json:"seconds,omitempty"`
}

type result struct {
	OK        bool   `json:"ok"`
	State     *state `json:"state,omitempty"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
	Retryable *bool  `json:"retryable,omitempty"`
}

type state struct {
	Version string   `json:"version"`
	Phase   string   `json:"phase"`
	Detail  string   `json:"detail,omitempty"`
	Adapter *adapter `json:"adapter,omitempty"`
	Devices []device `json:"devices"`
}

type adapter struct {
	HCI         string `json:"hci"`
	Address     string `json:"address"`
	Powered     bool   `json:"powered"`
	Discovering bool   `json:"discovering"`
}

type device struct {
	Address          string   `json:"address"`
	AddressType      string   `json:"address_type"`
	Name             string   `json:"name"`
	RSSI             *int16   `json:"rssi,omitempty"`
	Connected        bool     `json:"connected"`
	Paired           bool     `json:"paired"`
	ServicesResolved bool     `json:"services_resolved"`
	UUIDs            []string `json:"uuids"`
}

func initialState(phase, detail string) state {
	return state{Version: version, Phase: phase, Detail: detail, Devices: []device{}}
}

func apiFailure(code, message string, retryable bool) result {
	return result{Code: code, Error: message, Retryable: &retryable}
}

func failure(err error) result {
	return apiFailure("INTERNAL", err.Error(), false)
}

func failureWithState(err error, s state) result {
	reply := failure(err)
	reply.State = &s
	return reply
}

func signature() map[string]map[string]any {
	return map[string]map[string]any{
		"status": {}, "start": {}, "stop": {},
		"scan":       {"seconds": 32},
		"connect":    {"address": ""},
		"disconnect": {"address": ""},
	}
}

func parseParameters(action string, input io.Reader) (request, error) {
	r := request{Action: action}
	if _, ok := signature()[action]; !ok {
		return r, errors.New("unknown BLE method")
	}
	raw, err := io.ReadAll(io.LimitReader(input, maxInput+1))
	if err != nil {
		return r, err
	}
	if len(raw) > maxInput || len(bytes.TrimSpace(raw)) == 0 {
		return r, errors.New("invalid BLE request size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return r, errors.New("BLE parameters must be an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return r, err
		}
		key, ok := keyToken.(string)
		if !ok || seen[key] {
			return r, errors.New("duplicate or invalid BLE parameter")
		}
		seen[key] = true
		switch {
		case key == "ubus_rpc_session":
			var session string
			err = decoder.Decode(&session)
		case key == "address" && (action == "connect" || action == "disconnect"):
			err = decoder.Decode(&r.Address)
		case key == "seconds" && action == "scan":
			err = decoder.Decode(&r.Seconds)
		default:
			return r, fmt.Errorf("unexpected parameter %q", key)
		}
		if err != nil {
			return r, fmt.Errorf("invalid %s: %w", key, err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return r, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return r, errors.New("trailing BLE request data")
	}
	if action == "scan" && (r.Seconds < 5 || r.Seconds > 10) {
		return r, errors.New("scan duration must be 5 through 10 seconds")
	}
	if action == "connect" || action == "disconnect" {
		if !addressPattern.MatchString(r.Address) {
			return r, errors.New("a complete Bluetooth address is required")
		}
		r.Address = strings.ToUpper(r.Address)
	}
	return r, nil
}

func command(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error {
		err := cmd.Process.Signal(syscall.SIGTERM)
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("%s: %w: %s", name, err, text)
	}
	return text, nil
}

func callSocket(ctx context.Context, r request) (result, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return result{}, err
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		return result{}, errors.New("BLE request requires a deadline")
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return result{}, err
	}
	if err := json.NewEncoder(conn).Encode(r); err != nil {
		return result{}, err
	}
	var reply result
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&reply); err != nil {
		return result{}, err
	}
	return reply, nil
}

func storedState() (state, error) {
	raw, err := os.ReadFile(stateFilePath)
	if errors.Is(err, os.ErrNotExist) {
		return initialState("stopped", ""), nil
	}
	if err != nil {
		return state{}, err
	}
	var s state
	if err := json.Unmarshal(raw, &s); err != nil {
		return state{}, fmt.Errorf("invalid saved BLE state: %w", err)
	}
	if s.Phase != "stopped" && s.Phase != "failed" {
		return initialState("failed",
			"BLE process is unavailable; stop the service before starting it again"), nil
	}
	return initialState(s.Phase, s.Detail), nil
}

func unavailable(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET)
}

func statusReply(ctx context.Context) result {
	reply, err := callSocket(ctx, request{Action: "status"})
	if err == nil {
		return reply
	}
	if !unavailable(err) && !errors.Is(err, io.EOF) {
		return failure(fmt.Errorf("read BLE service: %w", err))
	}
	s, err := storedState()
	if err != nil {
		return failure(err)
	}
	return result{OK: true, State: &s}
}

func recordFailure(err error) result {
	s := initialState("failed", err.Error())
	if saveErr := saveState(s); saveErr != nil {
		return failure(errors.Join(err, fmt.Errorf("save failed state: %w", saveErr)))
	}
	return failureWithState(err, s)
}

func rpcTimeout(action string) time.Duration {
	switch action {
	case "status":
		return 6 * time.Second
	case "start", "stop":
		return 8 * time.Second
	default:
		return 16 * time.Second
	}
}

func rpc(ctx context.Context, r request) result {
	switch r.Action {
	case "status":
		return statusReply(ctx)
	case "start":
		if _, err := command(ctx, initPath, "start"); err != nil {
			return recordFailure(err)
		}
		for {
			reply, err := callSocket(ctx, request{Action: "wait-ready"})
			if err == nil {
				return reply
			}
			if errors.Is(err, io.EOF) {
				return statusReply(ctx)
			}
			if !unavailable(err) {
				return failure(err)
			}
			select {
			case <-ctx.Done():
				if s, stateErr := storedState(); stateErr == nil && s.Phase == "failed" {
					return failureWithState(errors.New(s.Detail), s)
				}
				return failure(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	case "stop":
		shutdown, cancel := context.WithTimeout(ctx, 3*time.Second)
		reply, err := callSocket(shutdown, request{Action: "shutdown"})
		cancel()
		if err == nil {
			return reply
		}
		if !unavailable(err) && !errors.Is(err, io.EOF) {
			return failure(fmt.Errorf("request BLE shutdown: %w", err))
		}
		if _, err := command(ctx, initPath, "stop"); err != nil {
			return recordFailure(err)
		}
		return statusReply(ctx)
	default:
		reply, err := callSocket(ctx, r)
		if err != nil {
			return apiFailure("BACKEND_STOPPED", "BLE service is not available", true)
		}
		return reply
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "--version", "-V":
			fmt.Println("ble-service", version)
			return
		case "list":
			if err := json.NewEncoder(os.Stdout).Encode(signature()); err != nil {
				log.Fatal(err)
			}
			return
		case "daemon":
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if err := runDaemon(ctx); err != nil {
				log.Fatal(err)
			}
			return
		}
	}
	if len(os.Args) != 3 || os.Args[1] != "call" {
		log.Fatal("usage: ble --version | list | call <method> | daemon")
	}
	r, err := parseParameters(os.Args[2], os.Stdin)
	var reply result
	if err != nil {
		reply = apiFailure("INVALID_REQUEST", err.Error(), false)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout(r.Action))
		reply = rpc(ctx, r)
		cancel()
	}
	if !reply.OK {
		log.Printf("ble %s failed: %s", os.Args[2], reply.Error)
	}
	if err := json.NewEncoder(os.Stdout).Encode(reply); err != nil {
		log.Fatal(err)
	}
}
