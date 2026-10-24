// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestCommandSurfaceIsOnlyBringUpPhases(t *testing.T) {
	expected := [...]string{"tty", "hci", "prepare", "verify"}
	if !reflect.DeepEqual(commandSurface, expected) {
		t.Fatalf("unexpected command surface: %v", commandSurface)
	}
	a := &application{stdout: &bytes.Buffer{}}
	err := a.execute(context.Background(), []string{"unknown"})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown command accepted: %v", err)
	}
}

func TestCommandSurfaceRejectsExtraArguments(t *testing.T) {
	a := &application{stdout: &bytes.Buffer{}}
	for _, args := range [][]string{{}, {"tty", "extra"}, {"prepare", "extra"}} {
		if err := a.execute(context.Background(), args); err == nil {
			t.Fatalf("arguments accepted: %v", args)
		}
	}
}
