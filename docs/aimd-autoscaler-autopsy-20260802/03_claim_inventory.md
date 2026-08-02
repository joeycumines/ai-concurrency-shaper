# 03 — Claim Inventory: Everything Claimed, With Verdicts

Every significant claim about the feature, traced to code or tests. Verdicts: **Confirmed** (code/tests prove it), **Partially True** (true under conditions), **False** (contradicted), **Aspirational** (claimed, not implemented), **Cannot Verify** (no code path).

## 3.1 README claims

| # | Claim (source) | Verdict | Evidence |
|---|----------------|---------|----------|
| C1 | "the proxy can adjust the default limiter's capacity automatically using an AIMD controller" (README:62) | **Confirmed** | `autoscale.go:330-404`; `queue.go` WithholdSlot/RestoreSlot |
| C2 | "This finds the upstream's real concurrency ceiling without guessing it." (README:62) | **False** — see 3.2 | The signal path is blind to retried 5xx and transport errors; the controller only reacts to 429s by default (`01_core_anatomy.md` §1.6); in per-route-limited configs it is starved entirely (GAP-01). |
| C3 | "Only the default limiter (the one set by -concurrency) is autoscaled; per-route limiters keep their static limits." (README:75) | **Confirmed but misleading** — see GAP-01 | `proxy.go:1412` guard `slotLimiter == p.limiter`; `proxy.go:1626-1639` `acquireSlot` |
| C4 | "Queue timeouts, client cancels, and proxy-internal panics are **not** treated as upstream signals and never move the target." (README:75) | **Confirmed** | `reachedUpstream` guard; `!localPanic` guard; tests `NotOnQueueTimeout`, `NotOnClientCancel` |
| C5 | "The decrease magnitude is governed by -autoscale-ratio (not Retry-After)." (README:75) | **Confirmed** | `autoscale.go:391-393` (`_ = retryAfter`) |
| C6 | "Mutually exclusive with -adaptive-headroom (enabling both disables headroom; autoscaling subsumes it)." (README:66) | **Partially True** — CLI only | `main.go:218-221` logs a warning and disables headroom. The `proxy` library itself does **not** enforce exclusivity; both options can be passed together and will fight over `withheld` (GAP-07). |
| C7 | "When autoscaling is on, the limiter is created at -autoscale-max and the controller withholds slots down to -autoscale-initial." (README:75) | **Confirmed** | `main.go:224-234`; `autoscale.go:313-320` |
| C8 | All 36 CLI flags documented (WIP.md claims "33") | **Confirmed for 36** | `go run . -h` lists 36; README tables cover all 36 (Core 12 + Protection 7 + Autoscale 8 + Breaker 7 + Retry 2). WIP.md's "33" is stale. |

## 3.2 The central claim, dismantled

**"Finds the upstream's real concurrency ceiling without guessing it" (C2).**

Three separate reasons this is false as stated:

1. **The ceiling-finder only measures 429s.** With default settings (`-autoscale-5xx=false`, `-retry-skip-429=true`), the only decrease signal is a final 429. A provider whose saturation manifests as 5xx or connection resets — rather than clean 429s — is invisible to the controller (`01_core_anatomy.md` §1.6).
2. **Retries actively mask the signal.** A 5xx that is retried to a 2xx feeds `OnSuccess` — the upstream failure *increases* the target (empirically confirmed, `09_evidence.md`).
3. **The routing architecture starves it.** Any `-limit "METHOD /path:N"` with an explicit `:N` creates a dedicated route limiter (`main.go:188-197`), so that route's traffic — usually *the* traffic of interest — never feeds the controller (GAP-01, `08_critical_failures.md` KILL-1).

The *mechanics* of AIMD are exactly as claimed. The *outcome* ("finds the ceiling") is only true in the narrow configuration where the default limiter gates the traffic of interest and the upstream rate-limits with 429s.

## 3.3 Code-comment claims

| # | Claim (source) | Verdict | Evidence |
|---|----------------|---------|----------|
| C9 | "Retry-After … deliberately NOT used to scale the magnitude of a decrease … prevents a malicious or buggy upstream from collapsing the concurrency limit to the floor by sending enormous Retry-After values." (autoscale.go:26-32, 370-376, 391-393) | **False as a security rationale** — see `05_debunking.md` §5.2 | A malicious upstream can already collapse the limit to `min` in `⌈log(max/min)/log(1/ratio)⌉` requests by sending plain 429s. Retry-After adds nothing to the attacker's power. |
| C10 | "It is always <= withheld." (queue.go:84-86, comment on `pendingAbsorbs`) | **Confirmed** | By construction: both increment together; absorbs and restores decrement both. The only forced zeroing (`queue.go:352`, `246`) happens when `withheld == 0`. |
| C11 | "This prevents a malicious upstream from collapsing the concurrency limit by sending enormous Retry-After values." (autoscale.go:32) | **False** (same as C9) | A malicious upstream collapses it faster with plain 429s than with Retry-After. |
| C12 | "The slot release mechanisms [are] orthogonal … it adjusts the limiter's effective capacity, not when this specific slot is returned." (proxy.go:1399-1401) | **Confirmed** | The feed precedes the release/penalty logic in the same defer but mutates only limiter capacity. |
| C13 | "This is mutually exclusive with adaptive headroom — the caller (main.go) must ensure only one is active." (proxy.go:463) | **Confirmed** — the exclusivity is delegated, not enforced | `proxy.New` accepts both; only `main.go:218-221` mediates. GAP-07. |
| C14 | "The controller … (max - initial) slots are withheld so the starting effective limit equals initial." (autoscale.go:22-23) | **Confirmed** | `autoscale.go:314-320`; `TestNew_InitialWithholding`. |
| C15 | "On increase, RestoreSlot is called once per step; on decrease, WithholdSlot is called once per unit of reduction." (autoscale.go:24-25) | **Confirmed** | `autoscale.go:346`, `398`. |

## 3.4 Test-assertion claims

| # | Claim | Verdict | Evidence |
|---|-------|---------|----------|
| C16 | "WithholdSlot … is the non-timer-based counterpart to AdaptiveReduce" | **Confirmed** | Same drain-or-pend semantics, no timer (`queue.go:307-318`). |
| C17 | `TestAutoscaler_ConcurrentCalls` asserts the autoscaler is race-free under mixed success/failure | **Confirmed** (runs green under `-race`) | `09_evidence.md`. |
| C18 | `TestProxy_AutoscalerConverges` asserts "EffectiveLimit must match the target" | **Confirmed** — but the test's own premise (upstream rejects at >3 concurrent) is a toy model, not a provider | `09_evidence.md`; `07_comparison.md` §7.5. |

## 3.5 WIP.md / blueprint.json claims

| # | Claim | Verdict | Evidence |
|---|-------|---------|----------|
| C19 | "BRANCH IS RELEASE-READY … All work done. Working tree clean." (WIP.md:3) | **Partially True** — tests green, but the feature's headline claim (C2) is unmet and four gaps are unfixed (GAP-01..GAP-04) | `09_evidence.md`; `04_gap_analysis.md` |
| C20 | "The autoscaler guard `slotLimiter == p.limiter` is then TRUE, so blanket-limited traffic correctly feeds the autoscaler." (WIP.md:24-26) | **Confirmed** | `TestProxy_AutoscalerWithLimitAll_FeedsFromBlanketLimited` |
| C21 | "All 33 CLI flags documented in README" (WIP.md:36) | **False** (stale count) | 36 flags exist; all 36 are documented. |
| C22 | blueprint.json task 3: "matching routes still feed it" | **Partially True** — matching routes feed it **only if they have no explicit `:N` limit** | `main.go:188-197`; GAP-01 |

## 3.6 The implied-behavior claims (not written anywhere, but implied by the feature's existence)

| # | Implied claim | Verdict |
|---|---------------|---------|
| C23 | "Enabling -autoscale is useful in the documented primary configuration (`-limit ...:N`)." | **False** — the autoscaler is starved in exactly that configuration (GAP-01). |
| C24 | "The controller converges to a stable value near the upstream ceiling." | **Uncertain** — sawtooth dynamics and low-load ratcheting prevent a stable point; see `07_comparison.md` §7.5. |
| C25 | "Retrying 5xx does not interfere with the controller's view of upstream health." | **False** — empirically confirmed masking (`09_evidence.md`). |
