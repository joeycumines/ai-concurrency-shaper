# 07 — Comparison: Dynamic Headroom vs AIMD Autoscaling — and What Is Optimal

This is the centerpiece. The diff's defining feature is the *generalization of one mechanism into two* — and the two now overlap on the same shared state. The question the diff never answers explicitly: **when do you use which, and is the new mechanism optimal?**

## 7.1 The two mechanisms, side by side (from code)

| | Adaptive headroom (pre-existing) | AIMD autoscaler (this diff) |
|---|---|---|
| Entry point | `proxy.go:1394-1396` on 429 only | `proxy.go:1412-1421` on success/failure |
| Magnitude | 1 slot | `target × ratio` down, `+step` up |
| Duration | Self-restoring timer (`window`), reset on each 429 | Permanent until opposite signal |
| Ceiling | `maxWithheld = 1` (hardcoded `maxAdaptiveSlots`) | `SetMaxWithheld(max - min)` |
| State | `withheld` + `pendingAbsorbs` + `adaptiveTimer` | Same `withheld` + `pendingAbsorbs` |
| Failure mode | Restores itself after quiet window | Can sit at `min` until successes accumulate |
| Addresses | N+1 teardown/accounting race (transient) | Sustained overload (persistent) |
| Cost | Constant-time, one slot | Can cut capacity 50%+ on one response |

**The structural overlap**: both mechanisms mutate the *same* `withheld`/`pendingAbsorbs` fields on the *same* limiter, through the *same* `serveLimited` defer. They are mutually exclusive only because `main.go:218-221` says so. The queue refactor deliberately unified their primitives (`WithholdSlot`/`RestoreSlot` vs `AdaptiveReduce`/`restoreAdaptiveSlot`) — mechanically elegant, semantically two controllers sharing one actuator.

## 7.2 The shared root cause, and the proportional-response mismatch

Both features exist because of the same observed phenomenon: **the upstream observes N+1 concurrency due to accounting/teardown lag, and expresses it as a 429** (README:181; `queue.go:36-41`). The two mechanisms differ in their *proportionality*:

- **Adaptive headroom**: N+1 observed → remove 1 slot. **Correctly scaled to the artifact.**
- **AIMD autoscaler (default ratio 0.5)**: N+1 observed → remove *half the limit*.

When a provider's 429 is caused by a **transient** accounting race (the case adaptive headroom was built for), the AIMD controller's 50% cut is a 50× over-reaction in proportional terms, and its recovery takes `(max-min)/step × cooldown` successes (e.g. 15× 5s = 75 s to recover from `min=1` to `max=16` with defaults). The controller cannot distinguish "the upstream just saw N+1 for 200 ms" from "the upstream is genuinely overloaded at N" — it has no temporal or magnitude discrimination, only a ratio.

**This is the deepest flaw in the feature's optimality**: the input signal (a 429) does not encode the *severity* of the overload, yet the controller applies a fixed *multiplicative* response. The one header that *does* encode severity — `Retry-After` — is deliberately discarded (see `05_debunking.md` §5.2).

## 7.3 Fidelity assessment — what the AIMD controller models vs reality

The "real system" behaviors and whether the controller models them:

| Real-system behavior | Modeled? | Notes |
|---|---|---|
| 429 ⇒ upstream overload | **Simplified** | A 429 may mean overload, per-key limit, token exhaustion, or a transient N+1 race. All map to one halving. |
| 5xx ⇒ overload | **Missing by default** | Ignored unless `-autoscale-5xx`; masked by retries (GAP-02). |
| Transport errors ⇒ overload | **Missing** | Status 0 → `default` branch (GAP-03). |
| Failure severity (Retry-After) | **Missing** | Deliberately discarded (C9). |
| Load (in-flight count at signal time) | **Missing** | The proxy *measures* `attemptState` and `metrics.Active` but the controller never reads them — a measure-based decrease (`target = in-flight - 1` at 429) would converge in one step instead of `log` steps. |
| Client aborts | **Mis-modeled** | Aborted 200 → success (GAP-04). |
| Per-route concurrency domains | **Missing** | One controller, one limiter, one upstream — no route dimension (GAP-01). |

**Fidelity score**: 1 of 7 behaviors faithfully modeled (the 429-as-concurrency-proxy); 1 simplified; 5 missing or mis-modeled. The controller is a *coarse* model of the thing it claims to find.

## 7.4 Is the mechanism optimal? The honest inventory of design choices

**What is done well (and why)**:
1. The `pendingAbsorbs` counter — necessary, correct, well-tested (GAP-free, `02_queue_mechanics.md`).
2. The guards (`reachedUpstream`, `!localPanic`, `slotLimiter == p.limiter`) — each excludes a genuinely non-upstream signal. The queue-timeout and client-cancel exclusions are tested and correct.
3. The increase cooldown — prevents per-request ratchet thrash. Sensible default (5s).
4. The hard floor (`min`) and ceiling (`max`) — the target is always in `[min, max]`; the controller cannot destroy the proxy's concurrency bound.
5. Decrease-on-5xx off by default — conservative; 5xx is noisier than 429 as a concurrency signal.
6. One knob for magnitude (`ratio`), one for recovery (`step`/`cooldown`) — small, comprehensible config surface.

**What is suboptimal (with the better alternative)**:

| Choice | Current | Optimal-ish alternative | Why |
|---|---|---|---|
| Decrease magnitude | fixed `ratio` | measure-based: `target = max(in-flight-at-429 - 1, min)` | Converges in one step, self-scales to the actual overshoot, never over-cuts on transient N+1. The proxy already tracks in-flight concurrency. |
| Retry-After | discarded | use as a *clamp* (`decrease = max(ratio, retryAfter-scaled)`) with the floor already bounding the damage | The "malicious upstream" fear (C9) is moot — the floor bounds it anyway; the header is free signal. |
| Success definition | any final 2xx | clean 2xx (no aborted, no prior-attempt failures) | Aligns with the breaker's deferred-success semantics; removes GAP-04 and part of GAP-02. |
| Decrease cooldown | none | small per-decrease cooldown | Prevents a burst of concurrent 429s (8 requests in flight all halving) from cascading `max → min` in one round-trip. With 8 concurrent 429s and ratio 0.5, a single wave can produce the full sawtooth collapse. |
| Wiring | default-limiter-only | per-binding-limiter, or refuse invalid combos | Removes GAP-01 (CRITICAL). |
| Warm-up | start at `max` | start at `min` and probe up, or at a conservative fraction | Starting at max guarantees the first overload event is a full-strength halving from the worst possible point. |
| Signal aggregation | 1 response = 1 signal | windowed ratio (e.g. decrease on `k` of `n` recent failures) | Dampens single-spike false positives without a cooldown. |

**On the sawtooth**: with `step=1, ratio=0.5, cooldown=5s`, a ceiling `C` produces a permanent sawtooth between `C/2` and `C+step` with a recovery ramp of `(C/2)/1 × 5s`. For `C=16`: oscillates 8↔16 with 40 s recovery ramps. The average capacity is ~12 — a 25% throughput tax versus knowing `C` statically. That is the *inherent* cost of blind AIMD and is not a bug; it is the price of not guessing. The measure-based decrease removes most of it.

## 7.5 Convergence: the claim vs the toy model

`TestProxy_AutoscalerConverges` proves: with an upstream that 429s at >3 concurrent and a controller with `cooldown=5ms`, the target drops, and `EffectiveLimit == Target` throughout. That is a **mechanics** proof. It does not and cannot prove convergence to a *stable* value: the sawtooth (7.4) means the target is periodic, not stable; and under low load the target ratchets to `max` with no load to probe it (increase is unconditional given 2xx + cooldown). **The honest claim is: "the target tracks the 429 threshold of the default-limited traffic with a bounded sawtooth."** That is a useful property. It is not "finds the ceiling."

## 7.6 Which mechanism should an operator choose?

| Situation | Choice | Rationale |
|---|---|---|
| Provider 429s mostly on teardown/accounting races (spiky, brief) | **Adaptive headroom** | Correct magnitude (1 slot), self-restoring, zero tuning. The AIMD halving over-reacts to the same artifact. |
| Provider 429s under sustained concurrency pressure, stable accounting | **AIMD autoscaler**, with defaults tuned (`ratio 0.8`, `step 2`) | Finds and holds a working range; the sawtooth is a fair price. |
| Per-route-limited config (`-limit ...:N`) | **Neither for auto-tuning** — the autoscaler is starved (GAP-01); use static limits + adaptive headroom on the default pool | The autoscaler cannot act on the binding limiter. |
| `-global-concurrency` below the target | **Do not combine with autoscale** (GAP-05) | The controller adjusts a limiter that isn't binding. |
| 5xx/transport-heavy upstream | **AIMD with `-autoscale-5xx`, retries reduced, or breaker-only** | Default autoscale is blind to the signals that matter (GAP-02/03). |

The product-grade recommendation: **ship the mechanics, fix the wiring (GAP-01) and the signal path (GAP-02/03/04), and document the sawtooth honestly.** As-is, the feature is a well-built tool for a configuration the README does not identify as the only one it works in.
