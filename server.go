// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type server struct {
	ctx       context.Context
	mu        sync.RWMutex
	current   state
	bus       bluetoothBus
	hci       string
	operation chan struct{}
	ready     chan struct{}
	shutdown  func()
	stopOnce  sync.Once
}

func newServer(ctx context.Context) *server {
	return &server{
		ctx: ctx, current: initialState("starting", "Initializing the built-in BLE controller"),
		operation: make(chan struct{}, 1), ready: make(chan struct{}),
	}
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("refusing non-directory or symlink %s", path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

func saveState(s state) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	dir := filepath.Dir(stateFilePath)
	if err := privateDirectory(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(raw)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), stateFilePath)
}

func (s *server) phase(phase, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if phase == "ready" && (s.ctx.Err() != nil || s.current.Phase == "stopping") {
		return context.Canceled
	}
	s.current = initialState(phase, detail)
	return saveState(s.current)
}

func stateResult(current state) result {
	reply := result{OK: current.Phase != "failed", State: &current}
	if current.Phase == "failed" {
		reply.Error = current.Detail
	}
	return reply
}

func (s *server) status(ctx context.Context) result {
	s.mu.RLock()
	current, bus, hci := s.current, s.bus, s.hci
	s.mu.RUnlock()
	if current.Phase != "ready" {
		return stateResult(current)
	}
	if bus == nil {
		return failure(errors.New("ready BLE service has no BlueZ connection"))
	}
	data, err := bus.Objects(ctx)
	s.mu.RLock()
	latest := s.current
	s.mu.RUnlock()
	if latest.Phase != "ready" {
		return stateResult(latest)
	}
	if err != nil {
		return failure(fmt.Errorf("read BlueZ state: %w", err))
	}
	a, devices, err := snapshot(data, hci)
	if err != nil {
		return failure(err)
	}
	current.Adapter, current.Devices = a, devices
	return stateResult(current)
}

func serverTimeout(action string) time.Duration {
	switch action {
	case "status", "wait-ready":
		return 4 * time.Second
	case "shutdown":
		return 2 * time.Second
	default:
		return 14 * time.Second
	}
}

func (s *server) handle(r request) result {
	ctx, cancel := context.WithTimeout(s.ctx, serverTimeout(r.Action))
	defer cancel()
	switch r.Action {
	case "wait-ready":
		select {
		case <-s.ready:
		case <-ctx.Done():
		}
		return s.status(ctx)
	case "shutdown":
		if s.shutdown == nil {
			return failure(errors.New("BLE shutdown is unavailable"))
		}
		err := s.phase("stopping", "Releasing the owned Bluetooth session")
		s.stopOnce.Do(s.shutdown)
		if err != nil {
			return failure(err)
		}
		return s.status(context.Background())
	case "status":
		return s.status(ctx)
	}
	if r.Action != "scan" && r.Action != "connect" && r.Action != "disconnect" {
		return apiFailure("INVALID_REQUEST", "unknown BLE operation", false)
	}
	if r.Action == "scan" && (r.Seconds < 5 || r.Seconds > 10) {
		return apiFailure("INVALID_REQUEST", "scan duration must be 5 through 10 seconds", false)
	}
	if r.Action != "scan" && !addressPattern.MatchString(r.Address) {
		return apiFailure("INVALID_REQUEST", "invalid Bluetooth address", false)
	}
	select {
	case s.operation <- struct{}{}:
		defer func() { <-s.operation }()
	default:
		return apiFailure("BUSY", "another BLE operation is in progress", true)
	}
	s.mu.RLock()
	ready, bus, hci := s.current.Phase == "ready", s.bus, s.hci
	s.mu.RUnlock()
	if !ready || bus == nil || ctx.Err() != nil {
		return apiFailure("SERVICE_NOT_READY", "BLE service is not ready", true)
	}
	var err error
	switch r.Action {
	case "scan":
		err = scan(ctx, bus, hci, r.Seconds)
	case "connect", "disconnect":
		err = changeConnection(ctx, bus, hci, strings.ToUpper(r.Address), r.Action == "connect")
	}
	if err != nil {
		log.Printf("BLE %s failed: %v", r.Action, err)
		code := "DEVICE_NOT_FOUND"
		if r.Action == "disconnect" {
			code = "DEVICE_NOT_CONNECTED"
		}
		return apiFailure(code, err.Error(), true)
	}
	return s.status(ctx)
}

func (s *server) serve(listener net.Listener, handlers *sync.WaitGroup) error {
	slots := make(chan struct{}, 8)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			log.Print("BLE request limit reached")
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-slots }()
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(18 * time.Second)); err != nil {
				log.Printf("set BLE request deadline: %v", err)
				return
			}
			var r request
			decoder := json.NewDecoder(io.LimitReader(conn, maxInput+1))
			decoder.DisallowUnknownFields()
			var reply result
			if err := decoder.Decode(&r); err != nil {
				reply = failure(err)
			} else {
				reply = s.handle(r)
			}
			if err := json.NewEncoder(conn).Encode(reply); err != nil {
				log.Printf("write BLE response: %v", err)
			}
		}()
	}
}

func removeSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket %s", path)
	}
	return os.Remove(path)
}

func protectedStep(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	return ctx, cancel, nil
}

func runDaemon(parent context.Context) error {
	unlock, err := takeDaemonLock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := privateDirectory(runDir); err != nil {
		return err
	}
	if err := removeSocket(socketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socketPath, 0600); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	s := newServer(ctx)
	s.shutdown = cancel
	if err := saveState(s.current); err != nil {
		return err
	}
	var handlers sync.WaitGroup
	serveDone := make(chan error, 1)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		serveDone <- s.serve(listener, &handlers)
	}()
	r := &runtime{}
	startup, stopStartup := context.WithTimeout(ctx, 170*time.Second)
	err = r.prepare(startup)
	stopStartup()
	if err == nil {
		s.mu.Lock()
		s.bus, s.hci = r.bluez, r.transport.controller()
		s.mu.Unlock()
		err = s.phase("ready", "ANA_FREQ 26000 kHz verified in PSRAM after one SET, warm reset and same-TTY reattach")
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		err = nil
	}
	if err != nil {
		err = errors.Join(err, s.phase("failed", err.Error()))
	}
	close(s.ready)
	if err == nil && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case serveErr := <-serveDone:
			err = fmt.Errorf("BLE socket listener: %w", serveErr)
		case <-r.bluezChild.done:
			err = fmt.Errorf("BlueZ exited unexpectedly: %v", r.bluezChild.err)
		case <-r.busChild.done:
			err = fmt.Errorf("D-Bus exited unexpectedly: %v", r.busChild.err)
		case <-r.transport.exited():
			err = errors.New("owned hciattach exited unexpectedly")
		}
	}
	if phaseErr := s.phase("stopping", "Releasing the owned Bluetooth session"); phaseErr != nil {
		err = errors.Join(err, phaseErr)
	}
	cancel()
	s.operation <- struct{}{}
	err = errors.Join(err, r.cleanup())
	<-s.operation
	if err != nil {
		err = errors.Join(err, s.phase("failed", err.Error()))
	} else {
		err = s.phase("stopped", "Device-tree UART binding retained")
	}
	listener.Close()
	<-acceptDone
	handlers.Wait()
	return err
}
