# 01 — Core Anatomy: How the AIMD Autoscaler Actually Works

Everything in this document is traced from running code. The authoritative sources are `internal/autoscale/autoscale.go`, the feed block in `internal/proxy/proxy.go`, and `main.go`.

## 1.1 The controller's state

`Autoscaler` holds one scalar of authority — `target` — plus an atomic counter block for stats. The limiter's *actual* capacity is derived state, not stored state: the autoscaler assumes `limiter.withheld == max - target` at all times.

```go
// internal/autoscale/autoscale.go:258-277
type Autoscaler struct {
    limiter *queue.Limiter
    cfg     autoscaleConfig

    mu           sync.Mutex
    target       int
    lastIncrease time.Time

    totalSuccesses atomic.Int64
    total429s      atomic.Int64
    total5xxs      atomic.Int64
    total403Bans   atomic.Int64
    totalIncreases atomic.Int64
    totalDecreases atomic.Int64
}
```

The config (`autoscale.go:46-58`) holds `min`, `max`, `initial`, `increaseStep`, `decreaseRatio`, `increaseCooldown`, `decreaseOn5xx`. Defaults (`applyDefaults`, `autoscale.go:60-75`): `min=1`, `increaseStep=1`, `decreaseRatio=0.5`, `increaseCooldown=0`, `max=1`, `initial=max`. Cross-field validation in `New` (`autoscale.go:281-327`) enforces `1 <= min <= initial <= max` and `0 < decreaseRatio <= 1`.

## 1.2 Construction: the limiter is created at max, then de-rated to initial

`main.go:218-234` creates the limiter at the **maximum** capacity, and `New` withholds the difference down to `initial`:

```go
// internal/autoscale/autoscale.go:313-320
cfg.limiter.SetMaxWithheld(cfg.max - cfg.min)
for range cfg.max - cfg.initial {
    cfg.limiter.WithholdSlot()
}
```

So with `max=8, initial=4`: the channel capacity is 8, `SetMaxWithheld(7)` lifts the headroom ceiling, and 4 slots are withheld → `EffectiveLimit() == 4`. **The channel's physical capacity is never the real limit again** — the effective limit is `cap - withheld`, a moving target.

## 1.3 The signal path: one feed per completed limited exchange

The proxy feeds exactly one signal per `serveLimited` invocation, in the function's `defer` (`internal/proxy/proxy.go:1412-1421`):

```go
if p.autoscaler != nil && reachedUpstream && slotLimiter == p.limiter && !localPanic {
    if recOK {
        now := time.Now()
        if isUpstreamFailureStatus(rec, now) {
            p.autoscaler.OnFailure(rec.status, parseRetryAfterFromRecorder(rec, now))
        } else if isBreakerSuccessStatus(rec, now, 0) {
            p.autoscaler.OnSuccess()
        }
    }
}
```

The four guards and one classification, and what each does:

| Guard | Effect |
|-------|--------|
| `reachedUpstream` (`proxy.go:1374`, `attemptState.started`) | Queue timeouts and pre-upstream errors never feed the controller. Requests that fail `acquireSlot` return at `proxy.go:1326-1343` before the defer exists. |
| `slotLimiter == p.limiter` | Only exchanges that acquired a slot from the **default** limiter feed the controller. Per-route limiters and the global limiter are excluded. |
| `!localPanic` | Proxy-internal panics (502 written by recovery) are not upstream signals. |
| `recOK` | The response writer must be a `statusRecorder` (always true in production, `proxy.go` installs it in `ServeHTTP`). |
| `isUpstreamFailureStatus` vs `isBreakerSuccessStatus` | Failure = 5xx/429/rate-limit-signaled-403 (via `circuitbreaker.IsFailureStatusWithHeaders`). Success = clean 2xx, but NOT a bare 403, NOT 3xx, NOT 101, NOT a half-open-probe resolution (epoch 0). |

**The epoch argument matters.** `isBreakerSuccessStatus(rec, now, 0)` (`proxy.go:1648-1666`) with `epoch==0` disables the half-open-probe clause: only `200-299` counts as success. A 101 upgrade or a bare auth 403 produces **no signal at all** — neither increase nor decrease.

## 1.4 OnSuccess: the ratchet

`autoscale.go:330-352`:

```go
if a.target >= a.cfg.max { return }
if a.cfg.increaseCooldown > 0 && time.Since(a.lastIncrease) < a.cfg.increaseCooldown { return }
newTarget := min(a.target+a.cfg.increaseStep, a.cfg.max)
for range newTarget - a.target { a.limiter.RestoreSlot() }
a.target = newTarget
a.lastIncrease = time.Now()
```

Increases are **unconditional given cooldown and ceiling** — there is no utilization check. Under any sustained stream of 2xx, the target ratchets to `max` one step per cooldown period.

## 1.5 OnFailure: the halving

`autoscale.go:363-404`:

```go
switch {
case statusCode == 429:     a.total429s.Add(1); isDecrease = true
case statusCode == 403:     a.total403Bans.Add(1); isDecrease = true
case statusCode >= 500 && statusCode < 600:
    a.total5xxs.Add(1); isDecrease = a.cfg.decreaseOn5xx
default:
    return // status 0 (transport error), 4xx, 3xx, 1xx — ignored
}
newTarget := max(int(math.Floor(float64(a.target)*a.cfg.decreaseRatio)), a.cfg.min)
if newTarget >= a.target { return }
for range a.target - newTarget { a.limiter.WithholdSlot() }
a.target = newTarget
```

Key facts, in order of importance:

1. **`retryAfter` is parsed, passed, and discarded** (`_ = retryAfter`, `autoscale.go:391-393`). The docstring frames this as a security mitigation (see `05_debunking.md`).
2. **A 403 decreases only if it carried rate-limit signals** — because `isUpstreamFailureStatus` already gates it. The `403` branch in the switch is therefore only reachable for rate-limit-signaled 403s. The `total403Bans` counter is accurate.
3. **Status 0 (transport error) hits the `default` branch and is silently ignored.** A connection reset, DNS failure, or read timeout — the classic symptoms of upstream saturation — never move the target.
4. **`WithholdSlot()`'s return value is ignored.** In the synced state (target ↔ withheld) this is harmless (proved in `02_queue_mechanics.md`); it becomes drift when the limiter is externally perturbed (see `04_gap_analysis.md`, GAP-07).

## 1.6 The complete signal inventory

What does and does not feed the controller (verified by code trace and, where marked, by executed test):

| Upstream/exchange outcome | Autoscaler sees | Verified by |
|---|---|---|
| Final 429 (never retried; `retry-skip-429` default true) | `OnFailure(429)` → halve | `TestProxy_AutoscalerOn429Decreases` |
| Final rate-limit-signaled 403 | `OnFailure(403)` → halve | `TestAutoscaler_403TreatedAsBan` |
| Final 5xx (retries exhausted, breaker closed) | `OnFailure(5xx)` → halve **only if `-autoscale-5xx`** | `TestAutoscaler_OnFailure5xxOnlyWhenEnabled` |
| 5xx that was retried to a 2xx | `OnSuccess()` → **increase** (the 5xx is invisible) | Scratch experiment, `09_evidence.md` |
| Transport error (status 0) | ignored | code trace (`default` branch) |
| Circuit-open rejection / retries aborted by open breaker | no signal (`retryCircuitOpen` guard at `proxy.go:1642`) | code trace |
| Client cancel with ambiguous status | no signal | `TestProxy_AutoscalerNotOnClientCancel` |
| Client cancel after a definitive upstream 429 | `OnFailure(429)` → halve | code trace (mirrors breaker philosophy) |
| Queue timeout | no signal | `TestProxy_AutoscalerNotOnQueueTimeout` |
| **Aborted 2xx** (client disconnected after upstream wrote 200) | `OnSuccess()` → **increase** | Scratch experiment, `09_evidence.md` |
| Bare auth 403 | no signal | code trace (`isBreakerSuccessStatus` epoch-0 clause) |
| 101 upgrade success | no signal | code trace |

## 1.7 What the TUI reports

`main.go:426-444` copies `autoscaler.Stats()` into `metrics.Snapshot.Autoscale` every 250 ms ticker. The TUI (`tui.go:1204-1211`, `1370-1383`, `1699-1716`) switches the gauge denominator from the static `m.conc` to `as.Target` and annotates `(max %d)`, plus a dedicated "Autoscaler (AIMD)" section (`tui.go:1456-1473`). Notably the **active-gauge numerator is `m.snap.Active`**, computed by the metrics collector's `c.active` counter (`metrics.go:519`), *not* by the limiter — so when the target was just cut below current in-flight count, the gauge shows `Active > Target`, which is the honest (overshoot) picture.

## 1.8 What the limiter itself exposes

The queue exposes the raw primitive the autoscaler rides on: `SetMaxWithheld`/`MaxWithheld` (`queue.go:289-304`), `WithholdSlot` (`queue.go:318-339`), `RestoreSlot` (`queue.go:347-369`), plus the pre-existing `AdaptiveReduce` (`queue.go:185-223`). The autoscaler only ever calls the first three; `AdaptiveReduce` and its timer remain wired for the legacy adaptive-headroom path. These two mechanisms share the same `withheld`/`pendingAbsorbs` state — which is the structural overlap analyzed in `07_comparison.md` and the drift hazard in `04_gap_analysis.md` GAP-07.
