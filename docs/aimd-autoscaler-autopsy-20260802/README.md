# The AIMD Autoscaler Autopsy

**Subject**: The `wip` branch diff vs `main` — the AIMD (Additive Increase, Multiplicative Decrease) dynamic concurrency controller, the `internal/queue` refactor that generalizes the existing adaptive-headroom ("dynamic headroom") mechanism, and the question of whether the result is *optimal*.

**Date**: 2026-08-02

**Base**: `main` (tip `e4ae833`) · **Branch**: `wip` (tip `3b3c00e`)

---

## What was examined

| Area | Verdict |
|------|---------|
| `internal/autoscale/autoscale.go` (new, 450 lines) | Read in full |
| `internal/queue/queue.go` diff (184 lines) | Read in full, both sides (`main` and `wip`) |
| `internal/proxy/proxy.go` autoscaler feed + guards | Read in full (serveLimited, classification helpers) |
| `internal/retry/transport.go` (interaction surface) | Read in full (retry loop, DefaultCheckRetry) |
| `main.go` flag wiring, snapshot loop, transport sizing | Read in full |
| `internal/tui/tui.go` autoscaler display | Read in full (header, dashboard, concurrency gauge) |
| Tests: `autoscale_test.go` (432 lines), `queue_test.go` additions, `proxy_test.go` additions | Read in full, executed |
| Claims: `README.md`, `WIP.md`, `blueprint.json`, code comments | Catalogued and verified |
| Running evidence: full suite, race detector, two scratch experiments, panic reproduction | Executed |

## What was NOT examined (and why)

- The TUI test suite (`tuitest`, 99s) and PTY behavior were not individually read; they are orthogonal to the autoscaler. Their autoscaler-relevant additions are display-only.
- The circuit breaker (`internal/circuitbreaker`) internals were not re-audited; it is treated as a black box whose *interface* (`IsFailureStatusWithHeaders`, `ParseRetryAfter`, `RecordFailure/Success`) the autoscaler feed depends on.
- The `-limit-all` feature (from `main`) is only examined at its intersection with the autoscaler.

## The one-sentence verdict

**The AIMD autoscaler is a mechanically sound, well-tested generalization of the existing adaptive-headroom mechanism whose *signal path* is blind to exactly the failures that indicate upstream saturation — retried 5xx responses and transport errors — and whose *wiring* to the default limiter makes it inert in the most common per-route-limited configurations; the claim that it "finds the upstream's real concurrency ceiling" is substantially overstated.**

## Critical caveats

1. **This analysis cannot tell you whether the AIMD controller converges in production.** The tests prove the mechanics (target math, limiter state) but the convergence claim rests on the assumption that upstream 429s are a monotonic function of concurrency — which this proxy's own cooldowns and the domain's token/accounting behavior violate in ways a test harness cannot reproduce. See `07_comparison.md` and `10_honest_conclusions.md`.
2. **The report treats the documentation as suspect by default.** Every README/WIP/comment claim was traced to code or tests. Where the code does not support the claim, the claim is marked FALSE regardless of how reasonable it sounds.
3. **Line numbers are from the current working tree at analysis time** (`wip` @ `3b3c00e`). They are evidence anchors, not stable APIs.
4. **No load-testing was performed against a real provider.** The domain-reality analysis (`07_comparison.md`) is inference from the proxy's own code and published provider rate-limiting behavior, not an empirical benchmark.

---

## Document index and reading order

| # | Document | Purpose |
|---|----------|---------|
| 01 | `01_core_anatomy.md` | How the AIMD autoscaler actually works — the signal path, the guards, the target math, and the full inventory of what does and does not feed it |
| 02 | `02_queue_mechanics.md` | The queue refactor: `pendingAbsorbs`, `WithholdSlot`/`RestoreSlot`, the invariants, the deadlock it fixed, and the cooldown interaction |
| 03 | `03_claim_inventory.md` | Every documented claim about the feature, with verdicts (Confirmed / Partially True / False / Aspirational) |
| 04 | `04_gap_analysis.md` | Ranked gaps, from "autoscaler starved in per-route configs" (CRITICAL) to cosmetic TUI issues (LOW) |
| 05 | `05_debunking.md` | The specific claims that look right but are wrong, including the Retry-After "security" rationale and the "release-ready" WIP state |
| 07 | `07_comparison.md` | Head-to-head: existing dynamic headroom vs the new AIMD autoscaler, the overlapping behavior, and the "what is OPTIMAL" analysis |
| 08 | `08_critical_failures.md` | The scenarios that cause real failure, traced through code with probability and severity |
| 09 | `09_evidence.md` | What actually runs and what it produces: full suite, race detector, scratch experiments, panic reproduction |
| 10 | `10_honest_conclusions.md` | The True / Uncertain / False synthesis — the only document you need if you read one |

**Suggested reading**: README → 10 → 07 → 01 → 04 → 08. The queue mechanics (02) and claim inventory (03) are reference material.
