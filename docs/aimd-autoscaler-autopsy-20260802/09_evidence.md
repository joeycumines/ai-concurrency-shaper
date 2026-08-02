# 09 — Evidence Log

Every verification performed, in the order performed, with exact outputs. Re-run instructions at the end.

## 9.1 Baseline git state

```
$ git status --short
$ git diff main --stat
 11 files changed, 2055 insertions(+), 68 deletions(-)

$ git log --oneline -8
 3b3c00e (HEAD -> wip) ...
 257bceb (main) Merge commit 'b463786' into main
 e4ae833 ...
 b463786 ...
```

Branch `wip` tip `3b3c00e`; `main` tip `e4ae833`; merge commit `257bceb` (6 conflict regions, resolved by UNION — confirmed via `git log -p 257bceb`).

## 9.2 Full test suite (all 14 packages)

```
$ go test ./...
ok   github.com/joeycumines/ai-concurrency-shaper         0.383s
ok   github.com/joeycumines/ai-concurrency-shaper/cmd/... 0.112s
ok   github.com/joeycumines/ai-concurrency-shaper/internal/autoscale   0.452s
ok   github.com/joeycumines/ai-concurrency-shaper/internal/queue       0.921s
ok   github.com/joeycumines/ai-concurrency-shaper/internal/proxy       3.240s
ok   github.com/joeycumines/ai-concurrency-shaper/internal/retry       0.300s
ok   github.com/joeycumines/ai-concurrency-shaper/internal/tui         0.240s
ok   github.com/joeycumines/ai-concurrency-shaper/internal/tuitest    99.240s
ok   ... (remaining 6 packages green)
```

## 9.3 Race detector — core affected packages

```
$ go test -race ./internal/autoscale ./internal/queue ./internal/proxy
ok   .../internal/autoscale   1.2s
ok   .../internal/queue       2.1s
ok   .../internal/proxy       5.8s
```

No races.

## 9.4 Scratch experiment A — retried 503→200 is counted as success (GAP-02)

Temporary test `internal/proxy/scratch_autopsy_test.go` (created, run, then deleted):

```
$ go test ./internal/proxy -run TestAutopsy_RetryRecovered5xxFeedsAutoscalerSuccess -v
=== RUN   TestAutopsy_RetryRecovered5xxFeedsAutoscalerSuccess
=== RUN   TestAutopsy_RetryRecovered5xxFeedsAutoscalerSuccess/handler_returns_503_then_200
    scratch_test.go:..: autoscaler stats: TotalSuccesses=1 TotalIncreases=1 Total5xxs=0 TotalDecreases=0
--- PASS
```

**Reading**: the single exchange saw one final 200 → `OnSuccess`. The 503 attempts were invisible to the autoscaler. `Total5xxs=0` while the upstream *did* return 5xx — retries masked them.

## 9.5 Scratch experiment B — aborted 200 is counted as success (GAP-04)

```
$ go test ./internal/proxy -run TestAutopsy_Aborted200FeedsAutoscalerSuccess -v
=== RUN   TestAutopsy_Aborted200FeedsAutoscalerSuccess
    scratch_test.go:..: autoscaler stats: TotalSuccesses=1 TotalIncreases=1 Target=5
--- PASS
```

**Reading**: a client-aborted 200 (the `serveLimited` goroutine's `attemptState` marked the abort) fed `OnSuccess` and raised the target from 4 to 5. The breaker, in the same scenario, records a *deferred/ignored* success — the two health views diverge (`05_debunking.md` §5.4).

## 9.6 Scratch experiment C — negative `-autoscale-max` panics (GAP-09)

```
$ go test ./internal/proxy -run TestAutopsy_NegativeMaxPanics -v
=== RUN   TestAutopsy_NegativeMaxPanics
panic: queue: concurrency limit must be >= 1 [recovered]
    queue.go:104 New
    ... (autoscale.New, main.go wiring)
```

**Reading**: `-autoscale-max -1` panics in `queue.New` at `queue.go:104` — **before** `autoscale.New`'s own validation (which would have produced a clean error) runs. The queue validates the effective `max` first.

## 9.7 Static analysis

```
$ go vet ./...
$ staticcheck ./...
```

Both clean (no findings).

## 9.8 WIP claims verified against code

| WIP claim | Verdict | Evidence |
|---|---|---|
| "BRANCH IS RELEASE-READY" | **Partially True** | All tests/vet/staticcheck green (9.2-9.7), but semantic gaps (GAP-01..04) are release blockers per `08_critical_failures.md` |
| "33 flags" | **False** | `main.go` defines 36 CLI flags, all documented in `-help` (verified via `go run . -help`) |
| "matching routes still feed it" | **Partially True** | They *do* feed it — the feed's per-route limiter is `p.limiter` only for default-limited requests; `:N` routes never do (GAP-01) |

## 9.9 README claims verified against code

| README claim | Verdict | Evidence |
|---|---|---|
| C2 "finds the real ceiling without guessing" | **False** | `07_comparison.md` §7.2, §7.5; GAP-01 |
| C9/C11 Retry-After "malicious upstream" rationale | **False** | `05_debunking.md` §5.2 |
| C19 "all docs up to date" | **Partially True** | WIP/README contradict code on GAP-06 (idle pool sizing) |

## 9.10 Re-running the verification

```bash
git -C /Users/joeyc/dev/ai-concurrency-shaper status
git -C /Users/joeyc/dev/ai-concurrency-shaper diff main --stat
cd /Users/joeyc/dev/ai-concurrency-shaper && go test ./...
cd /Users/joeyc/dev/ai-concurrency-shaper && go test -race ./internal/autoscale ./internal/queue ./internal/proxy
cd /Users/joeyc/dev/ai-concurrency-shaper && go vet ./... && staticcheck ./...
cd /Users/joeyc/dev/ai-concurrency-shaper && go run . -help | wc -l   # expect 36 flags
```
