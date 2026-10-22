// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func testStatePath(t *testing.T) {
	t.Helper()
	old := stateFilePath
	stateFilePath = filepath.Join(t.TempDir(), "state.json")
	t.Cleanup(func() { stateFilePath = old })
}

func TestPublicMethodsAreExactlySix(t *testing.T) {
	expected := map[string]map[string]any{
		"status": {}, "start": {}, "stop": {},
		"scan": {"seconds": 32}, "connect": {"address": ""}, "disconnect": {"address": ""},
	}
	if !reflect.DeepEqual(signature(), expected) {
		t.Fatalf("unexpected RPC surface: %#v", signature())
	}
	for _, method := range []string{"send", "events", "pair", "trust", "wait-ready", "shutdown"} {
		if _, err := parseParameters(method, strings.NewReader("{}")); err == nil {
			t.Fatalf("internal or non-basic method %q was exposed", method)
		}
	}
}

func TestParameters(t *testing.T) {
	for _, tc := range []struct {
		action, input string
		valid         bool
	}{
		{"status", `{}`, true},
		{"start", `{"ubus_rpc_session":"session"}`, true},
		{"stop", `{}`, true},
		{"scan", `{"seconds":5}`, true},
		{"scan", `{"seconds":10}`, true},
		{"scan", `{"seconds":4}`, false},
		{"scan", `{"seconds":11}`, false},
		{"scan", `{"seconds":5.5}`, false},
		{"scan", `{"seconds":"5"}`, false},
		{"scan", `{"seconds":null}`, false},
		{"scan", `{}`, false},
		{"scan", `{"seconds":5,"seconds":10}`, false},
		{"status", `[]`, false},
		{"status", `null`, false},
		{"status", `{} {}`, false},
		{"status", `{"action":"start"}`, false},
		{"status", `{"address":"02:00:00:00:00:01"}`, false},
		{"status", `{"ubus_rpc_session":"a","ubus_rpc_session":"b"}`, false},
		{"connect", `{"address":"02:aa:00:00:00:01"}`, true},
		{"disconnect", `{"address":"02:00:00:00:00:01"}`, true},
		{"connect", `{"address":"02-00-00-00-00-01"}`, false},
		{"connect", `{"address":"02:00:00:00:00:01;reboot"}`, false},
		{"connect", `{"address":"02:00:00:00:00:01","client_id":"ignored"}`, false},
		{"disconnect", `{"address":"02:00:00:00:00:01","lease_id":"ignored"}`, false},
		{"connect", `{"address":null}`, false},
		{"status", strings.Repeat(" ", maxInput+1), false},
	} {
		t.Run(tc.action+tc.input[:min(len(tc.input), 70)], func(t *testing.T) {
			r, err := parseParameters(tc.action, strings.NewReader(tc.input))
			if (err == nil) != tc.valid {
				t.Fatalf("request=%+v err=%v", r, err)
			}
			if err == nil && r.Address != strings.ToUpper(r.Address) {
				t.Fatalf("address was not normalized: %s", r.Address)
			}
		})
	}
}

func TestJSONStateHasNoApplicationFields(t *testing.T) {
	raw, err := json.Marshal(initialState("stopped", ""))
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"devices", "phase", "version"}) || string(data["devices"]) != "[]" {
		t.Fatalf("unexpected state shape: %s", raw)
	}
}

func TestStoredStateNeverInventsReadiness(t *testing.T) {
	testStatePath(t)
	s, err := storedState()
	if err != nil || s.Phase != "stopped" || s.Devices == nil {
		t.Fatalf("missing state: %+v %v", s, err)
	}
	for _, phase := range []string{"starting", "ready", "stopping", "failed", "stopped"} {
		if err := saveState(initialState(phase, "detail")); err != nil {
			t.Fatal(err)
		}
		got, err := storedState()
		expected := phase
		if phase != "stopped" && phase != "failed" {
			expected = "failed"
		}
		if err != nil || got.Phase != expected || got.Adapter != nil || len(got.Devices) != 0 {
			t.Fatalf("%s: state=%+v err=%v", phase, got, err)
		}
	}
	if err := os.WriteFile(stateFilePath, []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := storedState(); err == nil {
		t.Fatal("corrupt state was hidden")
	}
}

func TestTimeoutBudgets(t *testing.T) {
	for _, tc := range []struct {
		method       string
		rpc, backend time.Duration
	}{
		{"status", 6 * time.Second, 4 * time.Second},
		{"start", 8 * time.Second, 4 * time.Second},
		{"stop", 8 * time.Second, 2 * time.Second},
		{"scan", 16 * time.Second, 14 * time.Second},
		{"connect", 16 * time.Second, 14 * time.Second},
		{"disconnect", 16 * time.Second, 14 * time.Second},
	} {
		backendMethod := tc.method
		if tc.method == "start" {
			backendMethod = "wait-ready"
		} else if tc.method == "stop" {
			backendMethod = "shutdown"
		}
		if rpcTimeout(tc.method) != tc.rpc || serverTimeout(backendMethod) != tc.backend {
			t.Fatalf("wrong timeout for %s", tc.method)
		}
	}
}

func TestProtectedStepFinishesAfterStopButRejectsLateWork(t *testing.T) {
	parent, stop := context.WithCancel(context.Background())
	step, finish, err := protectedStep(parent, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	stop()
	if step.Err() != nil {
		t.Fatal("in-flight protected step inherited shutdown cancellation")
	}
	if _, cancel, err := protectedStep(parent, time.Second); !errors.Is(err, context.Canceled) {
		if cancel != nil {
			cancel()
		}
		t.Fatalf("new step after shutdown: %v", err)
	}
	if deadline, ok := step.Deadline(); !ok || time.Until(deadline) > time.Second {
		t.Fatal("protected step lost its bound")
	}
}

func TestShutdownDoesNotResurrectReady(t *testing.T) {
	testStatePath(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newServer(ctx)
	s.shutdown = cancel
	reply := s.handle(request{Action: "shutdown"})
	if !reply.OK || reply.State.Phase != "stopping" || ctx.Err() == nil {
		t.Fatalf("shutdown=%+v", reply)
	}
	if err := s.phase("ready", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("startup overwrote shutdown: %v", err)
	}
	reply = s.handle(request{Action: "scan", Seconds: 5})
	if reply.OK || reply.Code != "SERVICE_NOT_READY" || reply.Retryable == nil || !*reply.Retryable {
		t.Fatalf("operation after shutdown=%+v", reply)
	}
}

func TestServerBusyAndScanBoundaries(t *testing.T) {
	s := newServer(context.Background())
	for _, seconds := range []int{-1, 0, 4, 11} {
		reply := s.handle(request{Action: "scan", Seconds: seconds})
		if reply.Code != "INVALID_REQUEST" || reply.Retryable == nil || *reply.Retryable {
			t.Fatalf("seconds=%d reply=%+v", seconds, reply)
		}
	}
	for _, seconds := range []int{5, 10} {
		reply := s.handle(request{Action: "scan", Seconds: seconds})
		if reply.Code != "SERVICE_NOT_READY" {
			t.Fatalf("valid boundary rejected: %+v", reply)
		}
	}
	s.operation <- struct{}{}
	reply := s.handle(request{Action: "scan", Seconds: 5})
	<-s.operation
	if reply.Code != "BUSY" {
		t.Fatalf("concurrent mutation=%+v", reply)
	}
}

func TestShutdownStillCancelsWhenStateCannotBeSaved(t *testing.T) {
	testStatePath(t)
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	stateFilePath = filepath.Join(parent, "state.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newServer(ctx)
	s.shutdown = cancel
	reply := s.handle(request{Action: "shutdown"})
	if reply.OK || reply.Code != "INTERNAL" || ctx.Err() == nil {
		t.Fatalf("state failure suppressed shutdown: %+v", reply)
	}
}

func TestFailedStatusKeepsOptionalErrorFields(t *testing.T) {
	reply := stateResult(initialState("failed", "startup failed"))
	if reply.OK || reply.Error != "startup failed" || reply.Code != "" || reply.Retryable != nil {
		t.Fatalf("failed status contract changed: %+v", reply)
	}
}

func TestCleanupNeverReplacesNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	if err := os.WriteFile(path, []byte("other owner"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeSocket(path); err == nil {
		t.Fatal("non-socket accepted")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "other owner" {
		t.Fatal("other owner's file changed")
	}
}
