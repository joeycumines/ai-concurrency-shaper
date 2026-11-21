# Optional SQLite history

`Open` accepts an ordinary local filename. SQLite URIs and network share paths
are rejected. Path metacharacters are URI-escaped internally, including spaces,
Unicode, `#` and a literal `?` on operating systems that permit it. A newly
created database requests mode 0600; Windows effective access also depends on
the directory's ACL. The deployment contract is one gateway writer per local
file. WAL is required; a network filesystem is not a supported substitute.

The pure-Go driver is `modernc.org/sqlite` v1.62.0 with its required
`modernc.org/libc` v1.80.1. One connection owns writes; two query-only connections
serve reads. There is no cgo dependency. `normal` selects WAL `synchronous=NORMAL`;
`full` selects `FULL`. These concern committed SQLite transactions. Neither
makes the in-memory queue durable against process or power loss. A normal
shutdown drains the writer and reports any write failure; a cancelled shutdown
wait does not abandon the drain.

## Ownership and retention

The database is identified by application ID `0x41495348` and schema version 1.
An unrelated or unsupported database fails closed. Initial schema creation is
transactional. Read-only reopen validates ownership without modifying it.

| Table | Purpose |
| --- | --- |
| `metadata` | Schema-owned cutoffs and cached persistent recorder counters/watermark |
| `buckets` | Absolute raw provider observations |
| `rollups` | Absolute minute and hour aggregates |
| `receipts` | Identity/digest receipts for idempotent ingestion |
| `requests` | The most recent 10,000 bounded request facts within retention |
| `markers` | Bounded control, recorder-gap and detail-gap annotations |

Raw observations remain for approximately 15 minutes, aligned to complete
minute groups. Minute observations remain for approximately 24 hours, aligned
to complete hour groups. Hour observations use the selected retention, default
168 hours and configurable from one to 8,760 hours. Parent rollups are updated
incrementally from new absolute observations; an expired raw tier is never used
to reconstruct and replace a parent from an incomplete child set. Retention
cutoffs move forward even if the wall clock moves backward.

An `(epoch, sequence, provider)` receipt makes an identical replay a no-op.
A conflicting replay aborts the whole transaction. Receipts are retained for
the history retention period. A rollup's counts, histograms, gauge integrals,
clock uncertainty, missing coverage and overlap flags survive aggregation.
The raw/minute/hour tiers are alternatives, not additional observations.

Queries choose the available tier appropriate for the requested resolution,
fallback to finer retained children for query edges, and exclude the current
hub's retained sequence tail before overlaying its immutable memory snapshot.
A coarse parent crossing that tail boundary falls back to children rather than
being counted alongside them. Backward-clock intervals use normalized wall
bounds for indexing while retaining their clock-step flags and elapsed duration.

## Work and memory bounds

`Enqueue` transfers an immutable packet and never waits for I/O or queue space.
Both queued packets and the writer's claimed batch remain charged against a
conservative 64 MiB payload budget; a 64-packet limit applies independently.
The queue can hold 64 packets while one batch of at most 64 packets is claimed;
both remain charged to the same byte budget. The estimate counts slice backing capacities and string contents without JSON
encoding on the request path. Shared backing storage may be counted more than
once. Overload drops a whole packet, increments visible loss counters and
produces a recorder-gap annotation at the next successful commit. This bound is
not an RSS bound, nor a hard on-disk byte quota.

Every public read has a two-second cancellation budget, at most 100,000 source
rows, and a 64 MiB encoded interval payload budget. Truncation is explicit.
Interval decoding uses at most four workers with four queued jobs. Query rows are closed
before decoding; workers are joined on success, error and cancellation. A
corrupt stored row produces an error, not an invented empty observation.

Request facts contain bounded operational metadata, durations, full attempt
histograms and at most 32 individual attempt details. They do not contain raw
request/response bodies, arbitrary headers, or application logs. Raw evidence
remains governed by the separate gateway journal and diagnostic capture policy.
The request ID and journal ID permit exact live investigation joins. An evicted
journal entry does not imply that a historical fact can reconstruct its body.

The recorder reports queue depth, byte ownership, write health, drop counters
and the highest committed boundary from cached state. Scraping never reads
SQLite. A committed watermark does not imply complete coverage before it: loss
markers and coverage flags remain authoritative. A crash can lose queued work
without preserving an exact count of that final loss.

## Verification

Behavioral coverage includes native-clock gateway observations through close
and read-only reopen; immutable IDs; queue pressure; byte pressure while a batch
is claimed; replay conflicts and rollback; foreign schemas; read-only writes;
path escaping; error recovery and shutdown errors; cancelled drains; worker
cancellation and deterministic decode errors; query connection release; and
backward clock movement after raw expiration.

The scale fixture writes 262,800 second observations over 73 hours through the
same transaction/retention implementation. A 24-hour query selects 1,440 minute
sources and 86,400 requests; a seven-day query selects 73 hour sources and
262,800 requests with missing earlier coverage. A separate full-payload fixture
contains 1,440 minute rows with three histograms and four resource pools. Its
encoded minute payload exceeds five megabytes and must yield 120 complete
points, 86,400 request and duration samples, exact 24-hour elapsed time and an
exact active mean under the unchanged public read timeout, including race builds.
