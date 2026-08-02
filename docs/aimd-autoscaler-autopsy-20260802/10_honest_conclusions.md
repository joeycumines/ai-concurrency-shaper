# 10 — Honest Conclusions

The verdict, partitioned into what is TRUE, what is UNCERTAIN, and what is FALSE — each with its evidence anchor. Read this first; it is the only document that says "so what?"

---

## True

1. **The AIMD controller's mechanics are sound and well-tested.** Target math, guards, withholding, and the `pendingAbsorbs` refactor are correct, race-free, and behaviorally equivalent to the old path for the adaptive-headroom case (`02_queue_mechanics.md` §2.1-2.5; `09_evidence.md` §9.2-9.3). This is the strongest part of the diff.
2. **The controller converges to the 429-threshold of the default-limited traffic.** The convergence test proves the mechanics end-to-end. The target always stays in `[min, max]` (`07_comparison.md` §7.5).
3. **The queue refactor is a strict generalization, not a regression.** Every adaptive-headroom scenario maps 1:1 onto the new primitives; the adaptive tests pass (`02_queue_mechanics.md`; `05_debunking.md` §5.5).
4. **The feature is conservative by default** (decrease-on-5xx off, guarded feed, hard floor) — it cannot destroy the proxy's concurrency bound.
5. **The work was genuinely verified** — 14 packages green, race-clean, vet/staticcheck clean. The gap is in what the verification *measured*, not in its diligence.

## Uncertain

1. **Whether per-route limiting is the "primary" configuration.** The README's examples and the feature's own `-limit ...:N` support imply it is; but the autoscaler is wired only to the default limiter. If per-route limiting is a niche, GAP-01 is a doc bug; if it is primary, it is a CRITICAL feature bug. **This is the single most important open question.**
2. **Whether upstreams' 429s in the target domain are predominantly transient (N+1 races) or sustained (genuine overload).** The answer determines whether the 50% halving is an over-reaction (transient-dominant: adaptive headroom is the better tool) or a fair response (sustained-dominant). The codebase contains no telemetry to answer this; the README's own rationale (N+1 races) suggests transient-dominant, which indicts the ratio.
3. **Whether the "subsumes adaptive headroom" positioning (README:66) is intentional or rhetorical.** The mechanics do not subsume it — they replace a 1-slot, self-restoring response with a proportional, permanent one. The two mechanisms address different failure signatures (`07_comparison.md` §7.6).

## False

1. **"Finds the upstream's real concurrency ceiling without guessing" (C2).** It finds the 429-threshold of *default-limited traffic*, with a permanent sawtooth, and is blind in per-route configs. Overstated. (`05_debunking.md` §5.1; `07_comparison.md` §7.2, §7.5; GAP-01.)
2. **"Ignoring Retry-After prevents a malicious upstream from collapsing the limit" (C9/C11).** Arithmetic refutes it: a plain 429 already collapses `max → min` in 3 requests. (`05_debunking.md` §5.2.)
3. **"BRANCH IS RELEASE-READY" (WIP).** Test-green ≠ feature-complete against its own claims. KILL-1 (per-route inertness) and KILL-2 (retry-masked 5xx feeds success) are release blockers in the common configuration. (`08_critical_failures.md`; `09_evidence.md` §9.8.)
4. **The breaker and autoscaler agree about upstream health.** They observe different points in the request lifecycle; an operator can watch them disagree in real time. (`05_debunking.md` §5.4.)
5. **"33 flags."** It is 36. Minor, but a concrete sign of doc drift. (`09_evidence.md` §9.8.)

## The bottom line

The diff is **architecturally excellent and semantically misleading**. The queue refactor and controller mechanics are production-grade; the feature's *signal path* (what counts as success/failure) and *wiring* (which limiter it binds to) are misaligned with its claims. Ship the mechanics, fix GAP-01 (per-route starvation), GAP-02 (retry masking), GAP-03 (transport blindness), GAP-04 (abort masking) and the README's framing — and the feature becomes what its own docs say it is.

**Recommended next actions (in order):**
1. Resolve the open question: is per-route `-limit ...:N` primary? (Determines GAP-01 severity.)
2. Fix the feed semantics: clean-success-only for `OnSuccess` (aligns with the breaker's deferred semantics) — removes GAP-02's main path and GAP-04.
3. Add a measure-based decrease (`target = in-flight-at-429 - 1`, floored at `min`) — removes KILL-3 and KILL-6, and most of the sawtooth.
4. Decide the positioning: "subsumes adaptive headroom" is only true if sustained-429s dominate; document the sawtooth and the choice matrix (`07_comparison.md` §7.6) either way.
