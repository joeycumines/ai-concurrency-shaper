# Observation contracts

This package is the shared measurement model used by the gateway, terminal,
Prometheus collector, and optional SQLite recorder. The gateway emits typed
observations at actual request, admission, transport, codec and delivery
boundaries. History does not scrape the gateway's own Prometheus endpoint.

## Identity and lifecycle

A random process epoch plus a monotonically increasing request sequence gives
each logical request its stable ID. The ID is carried in the request context,
through provider resolution, through the request journal, and into the final
request fact. Nested handlers reuse that context; they do not start another
logical request. The journal's existing numeric ID is additional join metadata.
A final fact is immutable and `Finish` is idempotent.

Received counts stay in the original ingress scope. When provider resolution
hands a request to a configured provider, its active and queued occupancy and
final fact move to that provider. Therefore a provider's received and terminal
counts need not describe the same cohort. The reserved `__unresolved__` scope
includes requests refused before a provider can be selected. A transport attempt
keeps the provider which actually started it even if the enclosing request is
subsequently handed off.

A logical request may produce several actual upstream attempts. Initial calls,
retry-policy calls and authentication-recovery resends are separate attempt
kinds. The transport integration starts an attempt after signing, immediately
around the real `RoundTrip`. Each attempt records at most one valid header
status; its duration and outcome finish at the response-body boundary. A 200
header does not prove a successful body. A local 400 or 503 does not fabricate
an upstream response. A protocol upgrade's observation ends at the handshake;
opaque upgraded traffic is not parsed as an HTTP model response.

Admission is a union of actual waits, not time spent executing a fast-path
check. Nested waits contribute to one request's queue duration once. The
integration brackets pause-gate waits, suite buffer admission waits, and route
and global limiter waits. A request which never waited does not contribute a
zero admission sample. The same cancellation/deadline continues across phases.

The registry retains at most 10,000 cancellable live identities. Excess live
requests still contribute to aggregate counts and occupancy; the snapshot
reports how many could not be retained. Completed details use a constant-time
10,000-entry ring. Each request retains its first 31 and latest attempt details,
with the actual total and truncation flag. A separate per-request attempt
histogram retains every attempt's latency bin, so latency-bin drill-down still
finds a request whose individual attempt detail was evicted.

## Meaning of measurements

Canonical counters are process-lifetime observations, independent of the
legacy UI reset. Status counters have a bounded `side` and HTTP `class`; failure
counters use a fixed stage vocabulary. No request ID, model, arbitrary route,
URL, body, fingerprint or error text becomes a canonical Prometheus label.
Provider names are bounded configured identities. The maximum fleet is 128
named providers plus the unresolved scope; provider identity is at most 256
UTF-8 bytes. Resource IDs are at most 512 bytes and never silently truncated
into another resource. The configured resource limit is 256 pools per provider.

Source usage is submitted only by the existing codec at an accepted final
semantic boundary. This package does not reinterpret native passthrough bodies.
Known zero, unknown, and absent usage are different. The first valid report
contributes to monotonic token totals. Later distinct presence/value reports
update the request fact and increment a correction count; they do not rewrite
those totals. A changed source spelling alone is not a correction. An addition
which would overflow a token total is omitted and explicitly counted.

Conversion losses, notes and omitted report entries are separate counters.
An omitted codec-report entry is never guessed to have been a loss or a note.

Durations use 22 finite bounds in seconds:

`0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 20, 30,
60, 120, 300, 600, 1800, 3600, 7200`, followed by an overflow bin.

Stored bins are disjoint; Prometheus exposition converts them to cumulative
buckets. `Quantile` returns the lower and upper bound containing the requested
rank. Merge histograms before calculating a fleet percentile. No percentile is
invented within a bin, and provider percentiles are not averaged. The first bin
includes zero; subsequent bins are `(lower, upper]`. No TTFT is inferred from
HTTP headers or arbitrary transport chunks.

Active requests, queued requests and open attempts are integrated at actual
state transitions using elapsed time. Resource capacity, occupancy, cooldown
and queue occupancy have their own transition integrals. A cooldown lease is a
subset of occupied capacity, not another request. Default, route and global
pools remain distinct. Unknown or unlimited capacity is unavailable, not zero.
A pool's known gauge coverage can be shorter than request coverage; display
utilization only when that pool covers the accepted period.

For a complete all-provider point, fleet gauge means are the sum of provider
means over the common period. Fleet minimum/maximum fields are conservative
bounds obtained from provider extrema, not claims of simultaneous observed
extrema. `ByProvider` uses exactly the same accepted point boundaries as the
fleet result.

## Time, coverage and query semantics

Closed observations retain UTC boundaries, independently measured elapsed
nanoseconds, and a measured clock-read uncertainty bracket. Serializing a
`time.Time` removes its monotonic component, so elapsed duration is persisted
explicitly. Clock-step flags survive rollup. The read of an open interval uses
the same clock rules and does not seal or consume it.

Queries accept a nonzero, increasing time range within the representable
nanosecond timestamp range, no more than 366 days, and 1–1,000 points (120 by
default). A query can cover at most 129 distinct historical provider identities and 256
distinct resource identities per provider. Broader history returns an explicit
error asking for a narrower provider or time scope; it does not silently omit
providers. Source truncation invalidates rate availability. Output cuts follow source boundaries rather than splitting a request
across arbitrary pixels. Every visualization receives real start/end times;
unequal intervals must occupy their actual positions on the time axis.

| Condition | Counts and distributions | Rate/elapsed interpretation |
| --- | --- | --- |
| Complete interval coverage | Retained | Elapsed time is available |
| Requested edge cuts an observation | The cut observation is excluded, never fractionally prorated | Partial; unavailable |
| Missing observations | Available whole observations are retained | Gap; unavailable, not an observed zero |
| Overlapping process epochs | Each distinct epoch's observations are retained | Overlap; unavailable |
| Clock step | Whole observations are retained | Clock step; unavailable |
| Legacy data without elapsed duration | Counts and distributions remain usable | Elapsed time unavailable |

For unavailable rate windows `Complete` is false and `ElapsedNS` is zero as an
availability sentinel, not a measured zero duration. `Coverage` and explicit
flags accompany it. Empty histograms have no percentile. Gauges retain their
known-coverage durations so consumers can make the same distinction.

Request queries support provider, accepted time range, outcome, stage, bounded
metadata search, pagination, and duration cohorts. Client duration and admission
bounds use `(lower, upper]`. Attempt bounds must be histogram edges, allowing
exact bin selection using the full request-local histogram. `Waited` prevents
unobserved admission duration from matching a zero-latency cohort. Metadata
search does not scan retained request or response bodies.

## Storage and scrape boundaries

The hub's periodic tick seals absolute intervals. Neither a TUI render nor a
Prometheus scrape seals a bucket, resets a counter, consumes an event, or queries
SQLite. Snapshots and query results own their mutable slices. Bounded string
prefixes are cloned, so a small retained metadata prefix cannot keep an entire
large source allocation alive.

Without a history sink, the gateway keeps bounded live data and creates no
history file. The live interval buffer is capped at 12,000 provider intervals
and 15 minutes; large fleets may reach the count bound sooner. Missing older
coverage remains explicit. Completed ring eviction in this mode is not counted
as a failed history write.

With a sink, up to 10,000 request facts and 1,024 ordinary markers await a tick.
Overflow remains visible in cached health and a persistent detail-gap marker;
aggregate counts are still measured. Store queue drops and write failures are
separate, visible events. The SQLite implementation additionally bounds its
queue and in-flight batch together by a conservative 64 MiB owned-payload
estimate. The queue holds at most 64 packets, with one additional claimed batch
of at most 64 packets. This is not a process RSS quota.

The official Prometheus collector currently exposes 45 canonical metric
families: 32 counters, 10 gauges and three histograms. The committed-through
timestamp gauge is absent until a successful commit establishes a watermark.
The legacy endpoint families retain their original names and semantics.
Prometheus floating-point samples can lose integer precision for sufficiently
large totals; structured observation exports preserve integer encodings.

## Gateway shutdown and legacy identity

The HTTP lifecycle gives servers their graceful drain budget concurrently. A
server exceeding its budget is force-closed so ordinary HTTP request contexts
cancel, and the shutdown error is surfaced. The gateway runtime separately
rejects late ingress, cancels all tracked handler contexts (including upgraded
connections), and joins handler finalizers before sealing the recorder. A
non-cooperative handler which outlives the close context produces an explicit
error and leaves the recorder open for that handler's eventual fact; a later
`Close` can finish the drain. The recorder is never intentionally sealed while
a tracked ingress handler can still publish a terminal fact.

An unnamed legacy provider keeps the existing Prometheus provider label
`default`. Canonical observations use that provider's derived effective name,
which is also the model table, runtime control and exact journal join identity.
This mapping preserves the pre-existing metric series rather than renaming it.
