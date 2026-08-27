// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Command ocr-acp is a standalone ACP v1 server that exposes OpenCodeReview
// as a specialized review agent, per ROADMAP.md "ACP server adapter".
//
// Design constraint (from PR #679 / OSPP proposal): the protocol layer stays
// fully decoupled from internal/ — this binary talks to the existing `ocr`
// CLI as a subprocess and never links the review engine.
//
// Transports: stdio today; streamable HTTP is stubbed out pending the ACP v1
// HTTP profile settling (tracked in PROTOTYPE.md next steps).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alibaba/open-code-review/cmd/ocr-acp/acp"
	"github.com/alibaba/open-code-review/cmd/ocr-acp/wrapper"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[ocr-acp] fatal: %v\n", err)
		os.Exit(2)
	}
}

func run() error {
	var (
		transport = flag.String("transport", "stdio", "wire transport: stdio or http")
		addr      = flag.String("addr", "127.0.0.1:8080", "listen address when --transport=http")
		ocrBin    = flag.String("ocr", "", `path to the ocr binary (absolute, on PATH, or "mock")`)
		timeout   = flag.Duration("timeout", 30*time.Minute, "per-prompt timeout for one ocr invocation")
	)
	flag.Parse()

	if *ocrBin == "" {
		*ocrBin = os.Getenv("OCR_BIN")
	}
	wrap, err := wrapper.New(*ocrBin)
	if err != nil {
		flag.Usage()
		return err
	}

	switch *transport {
	case "stdio":
		return serveStdio(wrap, *timeout)
	case "http":
		return fmt.Errorf("--transport http is not implemented in this prototype; see PROTOTYPE.md next steps (planned addr %s)", *addr)
	default:
		return fmt.Errorf("unknown --transport %q: expected stdio or http", *transport)
	}
}

// serveStdio runs the protocol loop until stdin closes or SIGINT/SIGTERM.
func serveStdio(wrap *wrapper.Wrapper, runTimeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn := acp.NewConn(os.Stdin, os.Stdout)
	srv := acp.NewServer(ctx, conn, wrap, runTimeout)
	defer srv.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "[ocr-acp] shutting down on signal")
		return nil
	}
}
