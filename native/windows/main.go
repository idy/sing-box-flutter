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
	if sink, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		os.Stderr = sink
		defer sink.Close()
	}
	stdlog.SetOutput(io.Discard)
	log.SetStdLogger(log.NewNOPFactory().Logger())
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	newServer(os.Stdout, boxBackend{}).run(ctx, os.Stdin)
}
