# 08 — Critical Failures: What Will Actually Cause Trouble

Each scenario traced through code, with probability and severity. These are the ones that bite in production, not the academic ones (those are in `04_gap_analysis.md`).

---

## KILL-1 — The autoscaler runs but does nothing (operator trusts a dead gauge)

**Scenario**: Operator follows the README's primary configuration: `-autoscale -limit "POST /v1/messages:8"` with more explicit limits.

**Failure path**: `main.go:188-197` creates a per-route limiter for `:8`. Every matching request acquires from it (`proxy.go:1626-1639`). The feed guard `slotLimiter == p.limiter` (`proxy.go:1412`) is false → the feed block never executes. The TUI's "Autoscaler (AIMD)" panel (`tui.go:1456-1473`) renders `Target == Initial` forever. The operator believes the proxy is self-tuning; the provider is being hammered at the static per-route limit. If the operator set `-autoscale-initial` high expecting the controller to back off on 429s, the 429s arrive, the gauge never moves, and the provider temp-bans.

**Probability**: High (the documented configuration path).
**Severity**: Critical (feature silently inert; operator confidence misplaced).
**Mitigation in code**: none. The only diagnostics are the dead TUI panel and the static log line at startup.

---

## KILL-2 — Retry-enabled 5xx storms read as success; the target climbs while the upstream melts down

**Scenario**: Upstream returns 503 under load. Retries enabled (default `-retry -1`). `-autoscale-5xx` left at default false.

**Failure path**: Each request 503s once or twice, then 200s (or a mixed stream where some succeed). `DefaultCheckRetry` (`transport.go:37`) retries 5xx. The proxy's single feed sees only the final 200 → `OnSuccess` (`proxy.go:1417-1419`) → target **increases** (`autoscale.go:330-352`). The breaker is the only thing pulling the other way — and if it trips, the retry transport returns `ErrCircuitOpen`, `retryCircuitOpen()` suppresses the feed entirely (`proxy.go:1642`, `proxy.go:2253-2254`). Net: **the autoscaler never decreases in the entire 5xx episode** — either it sees successes (retries recovered) or it sees nothing (breaker open).

**Probability**: High whenever retries + 5xx co-occur, which is the norm.
**Severity**: High. The controller actively works against the operator in the exact scenario it exists for.
**Mitigation in code**: `-autoscale-5xx` only catches the exhausted-retry case (final 5xx), not the recovered-retry case. No mitigation exists for KILL-2's main path.

---

## KILL-3 — Concurrency collapse from a single burst of concurrent 429s

**Scenario**: A load spike pushes the limit over the provider threshold; 8 in-flight requests all return 429 in one round-trip.

**Failure path**: Each 429's `serveLimited` defer calls `OnFailure` (`proxy.go:1415-1416`). Each halves the shared target: `8 → 4 → 2 → 1` within a few milliseconds (the `a.mu` serializes the arithmetic, so the collapses compound: the first sets 4, the second sets 2, the third 1). Recovery to `max` takes `(max-1)/step × cooldown` successes — with defaults, 15 × 5 s = **75 s** of deliberately throttled throughput.

**Probability**: Medium-High (any concurrency-spike episode).
**Severity**: Medium. Not a crash; a 75 s throughput cliff that is disproportionate to the (possibly transient) cause. Compounded by the fact that the 429s may have been caused by the proxy's own cooldown/teardown races (see `07_comparison.md` §7.2), making the response a self-inflicted amplification.
**Mitigation in code**: none (no decrease cooldown, no measure-based target).

---

## KILL-4 — Library consumers silently get drift between reported target and enforced limit

**Scenario**: A developer uses the `proxy` library (not the CLI) and passes both `WithAdaptiveHeadroom(true)` and `WithAutoscaler(a)` — the comment at `proxy.go:463` says "the caller must ensure only one is active" and nothing enforces it.

**Failure path**: `AdaptiveReduce`'s timer (`queue.go:214`) fires `restoreAdaptiveSlot` (`queue.go:236-263`), decrementing `withheld` that the autoscaler's `target` assumes. `target` now exceeds effective capacity. Next `OnSuccess` calls `RestoreSlot`, inserting a token the controller thinks is a restore → effective limit grows *above* target. `WithholdSlot`'s ignored return (`autoscale.go:398`) means the controller never notices. The limiter's real capacity and the reported `Target` diverge permanently.

**Probability**: Low (requires library misuse), but **Certain** once it happens — drift is permanent, not transient.
**Severity**: Medium. The proxy will enforce a limit different from what the TUI and the operator believe.
**Mitigation in code**: none; requires `proxy.New` to reject the combination.

---

## KILL-5 — `-global-concurrency` below the autoscaler target makes decreases no-ops

**Scenario**: `-autoscale -global-concurrency 2 -autoscale-max 8`, upstream 429s.

**Failure path**: Requests hold a default-limiter slot and a global slot (`proxy.go:1474-1495`). The global limiter caps upstream concurrency at 2 regardless of the default limiter's effective limit. 429s (for whatever reason) halve the target; the halving changes nothing the upstream can observe. The target collapses to `min` and stays, because 429s persist, while the actual concurrency is pinned at 2 by the global limiter. The controller is fighting a constraint it does not control.

**Probability**: Medium (any combination of autoscale + global-concurrency).
**Severity**: Medium. Feature degradation, not failure; the target is misleading in the TUI.

---

## KILL-6 — Operators tune `-autoscale-ratio` to the margin, then a transient N+1 collapses the limit

**Scenario**: Operator sets `-autoscale-ratio 0.2` (aggressive) to handle a strict provider. A single N+1 accounting race (the thing adaptive headroom handles with one slot) triggers a 429.

**Failure path**: One 429 → `floor(16 × 0.2) = 3` — an 80% cut from one teardown race. Recovery: 13 × 5 s = 65 s. The feature the operator chose instead of adaptive headroom (because it's "smarter") delivers a strictly worse outcome for the exact failure adaptive headroom was built to absorb.

**Probability**: Medium (aggressive ratio configs).
**Severity**: Medium-High, because the README explicitly positions autoscale as the replacement for adaptive headroom (README:66 "autoscaling subsumes it") — without stating that it replaces a precisely-scaled 1-slot response with a proportionally blind one.

---

## Summary

| Kill | Trigger | Probability | Severity | Fix |
|------|---------|-------------|----------|-----|
| KILL-1 | `-limit ...:N` + `-autoscale` | High | Critical | GAP-01 fix |
| KILL-2 | retries + 5xx, default flags | High | High | GAP-02 fix |
| KILL-3 | concurrent 429 burst | Medium-High | Medium | decrease cooldown / measure-based target |
| KILL-4 | library double-config | Low (Certain once done) | Medium | enforce exclusivity |
| KILL-5 | autoscale + global-concurrency | Medium | Medium | GAP-05 fix |
| KILL-6 | aggressive ratio + N+1 race | Medium | Medium-High | measure-based target |
