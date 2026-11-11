// SPDX-License-Identifier: GPL-3.0-or-later
package main

import "os"

func suppressStderr() error {
	sink, err := openProcessStderrSink()
	if err != nil {
		return err
	}
	if err := redirectProcessStderr(sink); err != nil {
		_ = sink.Close()
		return err
	}
	// Retain the sink until process exit, including runtime diagnostics emitted
	// after main returns. Assigning this variable alone does not redirect them.
	os.Stderr = sink
	return nil
}
