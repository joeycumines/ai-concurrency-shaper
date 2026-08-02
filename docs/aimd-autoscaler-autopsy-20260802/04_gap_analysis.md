# 04 — Gap Analysis: Known Limitations, Ranked by Severity

Severity scale: CRITICAL (real failure in production), HIGH (significant deviation from stated behavior), MEDIUM (moderate fidelity impact), LOW (minor), NEGLIGIBLE (academic).

---

## GAP-01 — CRITICAL — The autoscaler is starved in the primary per-route configuration

**What's missing**: Feedback from traffic that runs on per-route limiters.

**Where**: `main.go:188-197` (route limiter creation) × `proxy.go:1412` (the `slotLimiter == p.limiter` guard) × `proxy.go:1626-1639` (`acquireSlot`).

**The mechanism**: `-limit "POST /v1/messages:8"` creates a dedicated `queue.Limiter` keyed by route. Matching traffic acquires from *that* limiter, so `slotLimiter == p.limiter` is false, and the feed block is skipped. The controller only ever sees traffic that falls through to the default limiter:
- requests matching `-limit "METHOD /path"` patterns with **no** `:N` (limit zero → no route limiter), or
- all non-matching traffic **only if** `-limit-all` is set.

**Impact**: In the most common way people use this tool (`-limit "POST /v1/messages:8"`-style explicit limits), `-autoscale` runs but the target never moves. The TUI will show a static "Autoscaler (AIMD)" panel with `Target == Initial` forever. The feature is silently inert. This is the highest-impact gap: the README's "Dynamic Concurrency" section never warns that per-route-limited traffic produces zero feedback.

**A fix would look like**: either (a) wire the autoscaler to the binding limiter per route (autoscaler-per-limiter or a selector), or (b) document loudly that autoscaling only applies when the default limiter is the binding constraint and validate/refuse the combination in `main.go`, or (c) feed the signal but scale the *withhold* on whichever limiter actually bound the exchange.

**Why it's not a trivial bug**: feeding per-route 429s to an autoscaler that only controls the default limiter would be *worse* — decreases would not reduce upstream-observed concurrency (the route limiter still admits 8), so the target would collapse to min while 429s persist. The guard is correct given the wiring; the wiring is the flaw.

---

## GAP-02 — HIGH — Retried failures are invisible to the controller

**What's missing**: Per-attempt failure signaling.

**Where**: `proxy.go:1412-1421` (single feed after the full exchange) × `internal/retry/transport.go` (`RoundTrip` retry loop, `DefaultCheckRetry` at `transport.go:37`).

**The mechanism**: The retry transport retries 5xx and transport errors internally and returns only the final result. The autoscaler feed runs once, on the final status. A request that 503'd twice and 200'd once feeds `OnSuccess` and **increases** the target. Empirically confirmed (`09_evidence.md`, scratch experiment: `Total5xxs=0 TotalIncreases=1` after a 503→200).

**Impact**: Under sustained 5xx load with retries enabled, the controller's view of upstream health is inverted: upstream failures read as successes. The circuit breaker sees each attempt (via `markBreakerAttemptFailure`), the autoscaler sees none of them. The two defense mechanisms disagree about reality.

**A fix would look like**: feed the breaker's per-attempt failure stream (the same one `markBreakerAttemptFailure` produces) to the autoscaler, or count retried attempts as suppressed successes (no increase when any attempt failed).

---

## GAP-03 — HIGH — Transport errors are invisible to the controller

**What's missing**: A decrease on status 0 (connection refused, reset, timeout).

**Where**: `autoscale.go:376-382` — the `default:` branch ignores everything that isn't 429/403/5xx, including `statusCode == 0`.

**The mechanism**: `isUpstreamFailureStatus(rec, now)` (`proxy.go:1641-1646`) returns true for status 0 (via `IsFailureStatusWithHeaders`), so `OnFailure(0, …)` is *called* — and then discarded by the switch.

**Impact**: The symptoms of upstream saturation that are *most* reliably correlated with overload — connection resets and timeouts — never move the target. The breaker treats status 0 as a failure; the autoscaler treats it as nothing. GAP-02 and GAP-03 together mean: with default settings, **the only thing that can lower the target is a clean 429**.

**A fix would look like**: add `case statusCode == 0: totalTransportErrors++; isDecrease = true` (or gate it behind the same flag as 5xx). The risk — decreasing on network blips — is real, which is presumably why it was excluded; but the exclusion is undocumented and asymmetric with the breaker.

---

## GAP-04 — MEDIUM — Aborted 2xx exchanges feed `OnSuccess`

**What's missing**: An aborted-exchange guard in the autoscaler feed.

**Where**: `proxy.go:1412-1421` (no `rec.aborted` check) vs `proxy.go:1562` (the breaker path skips aborted exchanges).

**The mechanism**: Client disconnects after the upstream wrote 200 but before the body is copied: `rec.status == 200`, `rec.aborted == true`. `isUpstreamFailureStatus` → false; `isBreakerSuccessStatus(rec, now, 0)` → true (status 2xx). The autoscaler increases. Empirically confirmed (`09_evidence.md`: aborted 200 → `TotalSuccesses=1 TotalIncreases=1 Target=5`).

**Impact**: The breaker *defers* success until the body is fully copied (`deferBreakerSuccess`, `transport.go:122-136`); the autoscaler has no equivalent. Under streaming workloads with impatient clients, the controller's success counter is inflated by exchanges the breaker would not count as clean. Moderate — the upstream did serve the request at the observed concurrency, so an increase is defensible; the defect is the **inconsistency with the breaker's success semantics**, which will confuse operators comparing the two panels.

**A fix would look like**: add `&& !rec.aborted` to the success branch (and decide explicitly whether aborted failures still decrease).

---

## GAP-05 — MEDIUM — The autoscaler adjusts the wrong limiter when a global limiter binds

**What's missing**: Awareness that `-global-concurrency` may be the binding constraint.

**Where**: `main.go:238-240` (global limiter creation) × `proxy.go:1412` guard (autoscaler only controls `p.limiter`) × `proxy.go:1474-1495` (global acquire after slot acquire).

**The mechanism**: With `-global-concurrency N`, every request holds a default-limiter slot *and* a global slot. If `N < autoscaler target`, the global limiter caps upstream concurrency. The controller keeps seeing 429s (or successes) and adjusts the default limiter, whose capacity is no longer the binding constraint — the decrease is a no-op on the actual concurrency.

**Impact**: The controller fights a constraint it cannot influence; the target can collapse to `min` while the upstream stays at `N`. Not a crash, but the "self-tuning" promise is void exactly when the operator stacked two limiters.

**A fix would look like**: refuse `-autoscale` + `-global-concurrency` below `-autoscale-max`, or feed the autoscaler with the global limiter when it binds.

---

## GAP-06 — MEDIUM — Transport idle-pool sizing ignores `-autoscale-max`

**What's missing**: `upstreamMaxIdleConnsPerHost` uses `-concurrency`, not the autoscaled ceiling.

**Where**: `main.go:288` → `main.go:506-535` (the sizing function reads `concurrency`); `main.go:222-234` (the autoscaled limiter's real capacity is `asMax`).

**The mechanism**: `MaxIdleConnsPerHost` is sized to the *static* concurrency, but the autoscaler can drive effective concurrency up to `asMax`. When `asMax > concurrency >= 20` (the floor), the keep-alive pool is undersized for the autoscaled ceiling.

**Impact**: Connection churn (idle connections evicted mid-burst) exactly when the autoscaler is probing upward. Not a correctness bug; throughput degradation in the aggressive-probe phase. Masked for most configs by the floor of 20.

**A fix would look like**: `upstreamMaxIdleConnsPerHost(…, asMax, …)` when autoscaling.

---

## GAP-07 — MEDIUM — Library-level coexistence of adaptive headroom and autoscaler causes unbounded target drift

**What's missing**: Enforcement that `AdaptiveReduce` (timer-based) and `WithholdSlot`/`RestoreSlot` (autoscaler) never both run on the same limiter.

**Where**: `proxy.go:463` (comment: "the caller (main.go) must ensure only one is active") × `queue.go:185-223` (`AdaptiveReduce` + timer) × `queue.go:318-369`.

**The mechanism**: The CLI path (`main.go:218-221`) disables adaptive headroom when `-autoscale` is set. A library user who passes both `WithAdaptiveHeadroom(true)` and `WithAutoscaler(a)` gets no warning. When `AdaptiveReduce`'s timer fires (`restoreAdaptiveSlot`, `queue.go:236-263`), it decrements `withheld` the autoscaler believes it owns; the autoscaler's `target` then exceeds the limiter's effective capacity by one, permanently, until the next increase pushes further past. `WithholdSlot`'s ignored return value (GAP-08) turns this into unbounded drift.

**Impact**: Library users silently get a limiter whose capacity no longer matches the reported target. CLI users are protected.

**A fix would look like**: reject both options in `proxy.New` (return an error), or make `AdaptiveReduce` consult the autoscaler.

---

## GAP-08 — LOW — `WithholdSlot()`'s return value is ignored, so target↔limiter sync is an assumption

**Where**: `autoscale.go:398` (`a.limiter.WithholdSlot()` in the decrease loop, return discarded).

**The mechanism**: In the synced state this can never fail (proof in `02_queue_mechanics.md` §2.3 — `WithholdSlot` fails iff effective limit ≤ min, and the decrease loop never requests below min). It only fails under GAP-07 external perturbation. When it fails, `a.target` moves but `withheld` doesn't — drift.

**Impact**: None in CLI operation. Latent in the library API.

---

## GAP-09 — LOW — Negative `-autoscale-max` panics instead of erroring cleanly

**Where**: `main.go:224` (`asMax` used if non-zero; a negative value skips the fallback) → `queue.NewLimiterWithCooldown(asMax, …)` panics at `queue.go:104` before `autoscale.New` can validate.

**The mechanism**: `-autoscale-max -1` → `asMax = -1` → `NewLimiterWithCooldown(-1)` panics. Empirically reproduced (`09_evidence.md`). `autoscale.New` would have returned a clean error if it ran first.

**Impact**: CLI misuse crashes instead of printing an error. Same class as the pre-existing `-concurrency -1` panic — consistent with existing behavior, but the autoscale path had a natural place to validate and didn't.

**A fix would look like**: validate `asMax >= 1` before constructing the limiter, or construct the limiter after `autoscale.New` validation (the limiter is available via `WithLimiter`).

---

## GAP-10 — LOW — TUI queue-depth bar is scaled by `-concurrency`, not the autoscaled max

**Where**: `tui.go:1383` and `tui.go:1727` (`queueMax := m.conc * 4`).

**The mechanism**: `m.conc` is `-concurrency` (`main.go:401`), which is bypassed when `-autoscale-max` differs. With `-concurrency 4 -autoscale-max 100`, the queue bar saturates at 16 while real queues can reach ~100.

**Impact**: Cosmetic; the gauge and header (which use `as.Target`/`as.Max`) are correct.

---

## GAP-11 — NEGLIGIBLE — `-autoscale-initial` default of "start at max" is the least informative starting point

**Where**: `main.go:227-229` (`asInitial = asMax` when 0).

**The mechanism**: Starting at `max` means the first burst of overload halves the limit before any learning occurs. Starting low and probing up is the conventional AIMD warm-up.

**Impact**: None correctness-wise; a tuning opinion. Documented (README:69).
