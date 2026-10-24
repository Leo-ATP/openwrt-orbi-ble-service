//go:build !linux

// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"errors"
)

func hciCSRCommand(context.Context, string, []byte) (csrCommandResult, error) {
	return csrCommandResult{}, errors.New("raw HCI commands require Linux")
}
