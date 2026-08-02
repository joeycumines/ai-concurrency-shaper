# 05 — Debunking: Conclusions That Look Right But Are Wrong

These are the findings most likely to produce false confidence. Each is stated as it is believed, then dismantled with evidence.

---

## 5.1 "The AIMD autoscaler finds the upstream's real concurrency ceiling without guessing"

**Why it looks right**: The mechanics are textbook AIMD — additive increase, multiplicative decrease, floor and ceiling. The unit tests show the target converging to the toy upstream's threshold (`TestProxy_AutoscalerConverges`). The README states it plainly (README:62).

**Why it's wrong**: The convergence test's upstream returns 429 at >3 concurrent and 200 at ≤3 — a monotonic concurrency↔status map. Real providers violate this in three ways that are *built into this proxy's own architecture*:

1. **The proxy's cooldowns manufacture the N+1 the controller then reacts to.** `-release-cooldown`, `-cancel-cooldown`, `-failure-hold` create "dead zones" where the upstream can observe more concurrency than the limiter admits. The adaptive-headroom feature was built specifically because providers see N+1 due to teardown/accounting races (see `07_comparison.md` §7.2). The AIMD controller's response to that same N+1 artifact is a **50% cut** — a proportional response to a ±1 transient.
2. **Retries and transport errors make the controller blind to saturation** (GAP-02, GAP-03). The "ceiling" is only discoverable via clean 429s.
3. **Per-route limiting starves it entirely** (GAP-01).

**The actual conclusion**: The autoscaler reliably finds the *429-threshold of the default-limited traffic* — a real, narrow quantity — not "the upstream's real concurrency ceiling." In per-route configurations it finds nothing at all.

---

## 5.2 "Ignoring Retry-After prevents a malicious upstream from collapsing the concurrency limit"

**Why it looks right**: The docstring says it twice (`autoscale.go:26-32`, `autoscale.go:370-376`). A 3600-second Retry-After *could* in theory justify a collapse-to-min.

**Why it's wrong — the arithmetic**: A malicious upstream does not need Retry-After. It sends a plain 429 on every request. Each one halves the target. From `max=8`, three 429s reach `min=1`. Retry-After changes neither the destination nor the attacker's effort — it only changes *how many requests* are needed (1 with a huge value vs. 3 without). The defense does not defend against anything the attacker cannot already do trivially.

**The actual conclusion**: The stated security rationale is a red herring. The *real* reason to ignore Retry-After — which is defensible — is simplicity and predictability: the ratio gives operators one knob with a documented effect, and the breaker (which *does* consume Retry-After for penalty scaling) already handles ban-duration semantics. The code would be more honest citing that.

**Note the asymmetry**: `OnFailure(429, retryAfter)` parses and passes the value (`parseRetryAfterFromRecorder`, `proxy.go:1416`) only to discard it. The parse cost is negligible; the design inconsistency is that the same header drives breaker penalty escalation while the autoscaler ignores it.

---

## 5.3 "WIP.md: BRANCH IS RELEASE-READY"

**Why it looks right**: All 14 packages pass, staticcheck is clean, race detector clean, smoke test passed, working tree clean (verified in `09_evidence.md`).

**Why it's wrong**: "Release-ready" is a *quality* claim, not a *tests-pass* claim. The release ships a feature whose headline promise (C2) is unmet in the most common configuration (GAP-01, CRITICAL), whose failure signal is masked by its own retry machinery (GAP-02), and which is blind to transport saturation (GAP-03). The verification work was thorough — the *scope* of the verification missed the semantic questions. This is the classic "the build is green" trap: the tests prove the machine runs, not that the machine does what the README promises.

**The actual conclusion**: The branch is *code-complete and test-green* — a mergeable state — but not *feature-complete against its own claims*.

---

## 5.4 "The circuit breaker and the autoscaler agree about upstream health"

**Why it looks right**: Both classify failures via the same `IsFailureStatusWithHeaders`; both are fed from `serveLimited`; both appear in the TUI.

**Why it's wrong**: They are fed from *different points in the request lifecycle*:
- The breaker sees **every attempt** (retry transport calls `RecordFailure` per attempt, `transport.go:376-393`) and defers success to full body copy.
- The autoscaler sees **only the final status** of the whole exchange, with no abort guard.

So on a 5xx-storm-with-recovery: the breaker records N failures, the autoscaler records a success. On an aborted 200: the breaker counts nothing (or a deferred failure), the autoscaler counts a success. An operator watching both panels sees contradictory health signals and has no documented explanation. (GAP-02/GAP-04.)

---

## 5.5 "The `pendingAbsorbs` refactor is a risk to the stable adaptive-headroom path"

**Why it looks right**: The diff rewrites the core release path (`absorbNextRelease`) that every limited request touches, and touches the adaptive timer logic.

**Why it's wrong**: The old boolean path is *behaviorally preserved* for the adaptive-headroom case. `AdaptiveReduce` can never withhold more than 1 slot (`maxWithheld` defaults to 1 and headroom never calls `SetMaxWithheld`), so `pendingAbsorbs ∈ {0,1}` exactly where `absorbNext` used to live, and every branch handles 0/1 identically to the old `absorbNext` logic (verified by trace in `02_queue_mechanics.md`; the adaptive-headroom tests — `AdaptiveStatsPendingAbsorbReportsActualActive`, `AdaptiveReduceWithCooldownDrainsIdle` — were updated and pass). The counter is a strict generalization.

**The actual conclusion**: The refactor is the *least* risky part of the diff. The risky parts are the signal-feed semantics (GAP-02/03/04) and the wiring (GAP-01).

---

## 5.6 "The WIP merge verified the cross-feature interaction (limitAll × autoscaler) is sound"

**Why it looks right**: `TestProxy_AutoscalerWithLimitAll_FeedsFromBlanketLimited` passes, and the WIP notes the guard check.

**Why it's misleading**: The test verifies one direction — blanket-limited traffic feeds the autoscaler. It does not verify the *semantic* consequences: with `-limit-all`, **all** non-matching traffic (health checks, asset fetches, anything) now influences the concurrency target. A 429 on `/health` halves the AI traffic limit; a burst of 200s on a monitoring endpoint ratchets it up. The interaction is "verified sound" only in the mechanical sense that signals flow; whether that is *desirable* is a product question the merge notes never ask. (GAP-01 family.)

---

## 5.7 "The `slotLimiter == p.limiter` guard protects the autoscaler from per-route interference"

**Why it looks right**: The comment at `proxy.go:1407-1409` frames the guard as protecting per-route limiters ("Per-route limiters have their own static limits"), and `TestProxy_AutoscalerSkipsRouteLimiters` asserts it.

**Why it's misleading**: The guard does protect the *limiter* — but it also silently disables the feature. The test asserts the guard's existence; it never asks whether the guard makes the feature useless in the configuration the tool documents as primary. The test is a red herring for the actual question: "does autoscaling work when the user follows the README's own `-limit ...:N` examples?" It does not. (GAP-01.)
