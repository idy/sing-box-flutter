// SPDX-License-Identifier: GPL-3.0-or-later
//go:build !windows

package main

import "os"

func openProcessStderrSink() (*os.File, error) {
	return os.OpenFile(os.DevNull, os.O_WRONLY, 0)
}

// Host builds exercise protocol and configuration tests, not a production helper.
// Only Windows needs the runtime/native process-handle redirection contract.
func redirectProcessStderr(*os.File) error { return nil }
