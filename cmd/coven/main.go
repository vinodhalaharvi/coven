// coven is the v2 binary. Replaces cmd/demo from v1.
//
// v2 hides the per-agent flag soup behind a single ControlPlane. There is
// no -bootstrap-gin, -bootstrap-connect, -conv-proto, etc. The router decides
// which agents run based on what changed. The user provides only the
// project root and the Claude API config.
//
// Usage:
//
//	coven -root <path>
//
// Currently a stub — the ControlPlane returned is a placeholder until the
// router/worktree/integrator components are built. This binary exists now
// so the integration shape is verified and so we can iterate against a
// real entry point as components land.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/vinodhalaharvi/coven/controlplane"
)

func main() {
	var (
		root = flag.String("root", ".", "project root to watch")
	)
	flag.Parse()

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve root: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stat(absRoot); err != nil {
		fmt.Fprintf(os.Stderr, "root not accessible: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("coven v2 (stub) — watching %s\n", absRoot)
	fmt.Println("(router/worktree/integrator components pending; this binary currently no-ops on file changes)")

	cp := controlplane.New(controlplane.Config{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Graceful shutdown on Ctrl-C.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nshutting down...")
		cancel()
	}()

	if err := cp.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "controlplane error: %v\n", err)
		os.Exit(1)
	}
}
