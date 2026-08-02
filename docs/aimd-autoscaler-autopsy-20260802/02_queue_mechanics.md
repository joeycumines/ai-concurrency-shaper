# 02 — Queue Mechanics: The `pendingAbsorbs` Refactor

The diff's `internal/queue/queue.go` changes are the load-bearing part of the feature. This document proves the mechanism is sound, documents the invariant, and identifies where it can and cannot break.

## 2.1 What changed

The old adaptive headroom used a **single boolean** `absorbNext` (`main` version, `queue.go:74-79`). The new code replaces it with a **counter** `pendingAbsorbs` and generalizes the ceiling from the constant `maxAdaptiveSlots = 1` to the field `maxWithheld` (default 1, raised via `SetMaxWithheld`). Two new primitives were added:

- `WithholdSlot()` (`queue.go:318-339`) — non-timer counterpart to `AdaptiveReduce`: increments `withheld` and `pendingAbsorbs`, then drains an idle token if one exists.
- `RestoreSlot()` (`queue.go:347-369`) — the inverse: decrements `withheld`; if a withhold is still pending, consumes one pending absorb; otherwise inserts a token back into the channel.

## 2.2 Why the boolean was a bug — and the counter fixes it

The queue test suite documents the failure precisely (`queue_test.go`, `TestLimiter_WithholdMultipleWhileAllHeld`):

> "The old single-boolean absorb flag could only track ONE pending absorb, so only one in-flight release was swallowed; the others leaked tokens back into the channel, inflating len(slots) past the effective limit. A later RestoreSlot then blocked forever on a full channel."

This is a **true claim about `main`'s code** — but it was only reachable if the limiter tried to withhold more than one slot while all slots were in use. The old adaptive headroom could never do that (`maxAdaptiveSlots = 1`), which is exactly why the boolean survived. The AIMD controller's multiplicative decrease (`target 8 → 4` = four simultaneous withholds) is what makes the counter *necessary*. **The refactor is not cosmetic; it is the precondition for the feature.**

## 2.3 The invariant

Let `C = cap(slots)`. The following invariant holds at every point where the mutex is not held *during* a cooldown limbo (see 2.4):

```
len(slots) + active + withheld = C
pendingAbsorbs <= withheld
```

- Each `Acquire` decrements `len(slots)`, increments `active`.
- Each absorbed release leaves `len(slots)` unchanged, decrements `active` and `pendingAbsorbs`.
- Each unabsorbed release increments `len(slots)`, decrements `active`.
- Each drain decrements `len(slots)`, increments `withheld` (and decrements `pendingAbsorbs`).
- Each restore increments `len(slots)`, decrements `withheld`.

The `Stats()` formula (`queue.go:383`) is the invariant solved for `active`:

```go
active := max(int64(cap(l.slots))-int64(len(l.slots))-withheld+pendingAbsorbs, 0)
```

`pendingAbsorbs` is added back because each pending absorb corresponds to a token that is in use but whose release will be swallowed — so it is *active* right now.

**Non-blocking proof for the blocking sends**: `RestoreSlot` and `restoreAdaptiveSlot` do `l.slots <- struct{}{}` while holding no lock. This can only block if the channel is full, i.e. `len(slots) == C`. When `pendingAbsorbs == 0` and `withheld >= 1`, the invariant gives `len(slots) = C - active - withheld <= C - 1`, so the send has room. When `pendingAbsorbs > 0` the code path does not send. The send is therefore never blocking. Verified by the `WithholdThenRestoreRoundTrip` test (a blocked `Acquire` unblocks exactly when `RestoreSlot` runs).

## 2.4 The cooldown limbo

The release path (`queue.go:142-157`) is:

```go
if l.cooldown > 0 {
    time.AfterFunc(l.cooldown, func() {
        if !l.absorbNextRelease() { doRelease() }
    })
} else if !l.absorbNextRelease() {
    doRelease()
}
```

Two consequences:

1. **An absorbed release is only consumed after the full cooldown**, not at release time. This is correct — the token is in limbo for the cooldown regardless — but it means a withheld slot's effect on `len(slots)` lags the release by one cooldown. `Stats().Active` under-reports by exactly the number of tokens in cooldown limbo (pre-existing behavior, not a regression).
2. **The invariant of 2.3 temporarily under-counts** during the limbo window (`len + active + withheld < C` by the number of limbo tokens). Every formula that reconstructs `active` from channel length inherits this. It is the same approximation the pre-diff code made; the diff does not worsen it.

## 2.5 RestoreSlot semantics — the "nothing to restore" branch

`RestoreSlot` when `withheld <= 0` (`queue.go:349-355`) clears `pendingAbsorbs` to zero. This is the *only* place `pendingAbsorbs` is force-zeroed, and it exists to clear a stale pending absorb after a timer-based restore already ran (`restoreAdaptiveSlot`, `queue.go:242-248`, does the same). This is defensive and correct: `pendingAbsorbs > withheld` is the one state that would violate the invariant, and both zeroing sites only trigger when `withheld == 0`.

**Caveat**: because `RestoreSlot` is a no-op when `withheld <= 0`, calling it when the limiter is already at full capacity *silently succeeds*. The autoscaler never does this (it guards with `target >= max`), but the primitive is footgun-shaped for library users.

## 2.6 What the tests actually prove

Executed (`09_evidence.md`):

- `TestLimiter_WithholdMultipleWhileAllHeld` — the counter consumes exactly `withholds` releases, channel never over-fills. **This is the load-bearing regression test of the entire feature.**
- `TestLimiter_RestoreSlotClearsPendingAbsorb` — restore-before-absorb leaves no leak.
- `TestLimiter_WithholdThenRestoreRoundTrip` — a blocked `Acquire` proceeds the moment a slot is restored, proving effective-limit enforcement through the channel.
- `TestLimiter_AdaptiveStatsPendingAbsorbReportsActualActive` — the `Stats().Active` correction is exact in the pending state.

All race-detector clean under `-race`.

## 2.7 What is NOT covered by tests

- **The cooldown × pendingAbsorbs interaction** (2.4) has no dedicated test combining `cooldown > 0` with a pending withhold while all slots are held. The code is correct by trace, but this is the only corner where the invariant formula is observably approximate.
- **Drift from external manipulation** (GAP-07) — no test asserts what happens if `AdaptiveReduce` and `WithholdSlot` interleave on the same limiter.
