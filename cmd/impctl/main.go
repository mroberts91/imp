// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// impctl is the human interface to impd, over its Unix domain socket. Thin
// by design: argument parsing, output formatting, and calls into
// pkg/client
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mroberts91/imp/api/v1alpha1"
)

var (
	version   = "dev"
	commit    = "unknown"
	branch    = "unknown"
	buildTime = "unknown"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		printError(err)
		return 1
	}
	return 0
}

func printError(err error) {
	var inv *v1alpha1.InvalidError
	if errors.As(err, &inv) && len(inv.Errs) > 0 {
		fmt.Fprintln(os.Stderr, "error: the object is invalid:")
		for _, fe := range inv.Errs {
			fmt.Fprintf(os.Stderr, "  * %s\n", fe.Error())
		}
		return
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
}
