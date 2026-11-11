// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"io"
	stdlog "log"
	"os"
	"os/signal"

	"github.com/sagernet/sing-box/log"
)

func main() {
	// Neither standard library diagnostics nor third-party stderr writes may
	// leak configuration/credentials. stdout belongs exclusively to JSON Lines.
	if suppressStderr() != nil {
		// Fail closed before reading any configuration. Do not print the setup
		// error to the inherited stream that could not be secured.
		os.Exit(1)
	}
	stdlog.SetOutput(io.Discard)
	log.SetStdLogger(log.NewNOPFactory().Logger())
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	newServer(os.Stdout, boxBackend{}).run(ctx, os.Stdin)
}
