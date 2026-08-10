// Command pinsgen regenerates the "Pinned revisions" table of
// internal/transcode/pins.md from internal/transcode/contracts.lock.json
// (the authoritative wire-contract pin registry).
//
// It is invoked by the go:generate directive in contracts.go:
//
//	go generate ./internal/transcode
//
// The working directory must be internal/transcode (the go:generate
// directive guarantees this). The schema inventories and governance
// sections of pins.md are preserved verbatim; only the generated table is
// rewritten. See VerifyContractsLock in contracts.go for the drift test
// that proves pins.md never drifts from the lock.
package main

import (
	"fmt"
	"os"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

func main() {
	lock, err := transcode.LoadContractsLock()
	if err != nil {
		fatal(err)
	}
	pins, err := os.ReadFile("pins.md")
	if err != nil {
		fatal(fmt.Errorf("read pins.md: %w", err))
	}
	out, err := transcode.GeneratePinsMarkdown(lock, pins)
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile("pins.md", out, 0o644); err != nil {
		fatal(fmt.Errorf("write pins.md: %w", err))
	}
	// Self-verify: the regenerated document must satisfy the lock (the drift
	// test's invariant), so a broken generator cannot produce a broken pin.
	if err := transcode.VerifyContractsLock(lock, out); err != nil {
		fatal(fmt.Errorf("verify regenerated pins.md: %w", err))
	}
	fmt.Println("pins.md: regenerated pinned revisions from contracts.lock.json")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "pinsgen:", err)
	os.Exit(1)
}
