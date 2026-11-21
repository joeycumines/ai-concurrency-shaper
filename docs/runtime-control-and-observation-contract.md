# Runtime control and observation contract

The gateway's local controller changes live admission state and existing pools. It does not rewrite CLI configuration, reset the circuit breaker, or expose an HTTP control endpoint. The terminal calls this same typed API for keyboard and mouse interactions.

## Review and application

`Controller.Preview(Command)` validates the requested action and returns a random token, exact runtime revision, a 30-second expiry, the frozen command, a one-sentence effect, and its consequences. Previewing and rendering have no effect on admissions or pools. The controller keeps its own copy; changing an exported preview cannot change the action that will be applied.

`Controller.Apply(token)` accepts only that token. It checks the expiry and exact controller revision before performing the frozen action. A successful token is idempotent: a repeated application returns its original result. A stale or expired preview must be reviewed again. Both prepared tokens and the audit ring are bounded to 256 entries; a token evicted from that bound is unknown, not permission to repeat an action. Audit snapshots are immutable copies.

Pool changes also compare the limiter's configured-limit revision atomically. An independent programmatic `Limiter.Resize` invalidates an older preview and prevents an older TTL from restoring over that newer change. Adaptive headroom changes effective capacity independently and does not reset the configured-limit revision.

## Actions

| Action | New provider arrivals | Already permitted work | Expiry and scope |
| --- | --- | --- | --- |
| Pause | Wait before body reads, breaker admission, or limiter acquisition | Continue, including existing limiter waiters | Optional TTL up to 24 hours |
| Resume | Continue through the ordinary breaker and admission rules | Continue | Optional TTL up to 24 hours |
| Drain | Receive local HTTP 503 | Continue to completion or their existing cancellation/deadline | Optional TTL up to 24 hours |
| Resize | Existing pool admits using its new positive configured limit | Retains every lease and cooldown | Default, configured route/group, or global pool; 1–1,000,000 slots; optional TTL up to 24 hours |
| Cancel | No change to unrelated arrivals | Cooperatively cancels the exact live logical request ID | No TTL; no ring index or ambiguous row number |

Local model discovery remains available while paused or draining. Pause applies after the provider is resolved: a suite router can already have performed its bounded model-dispatch body read. The provider gate itself does not consume the body.

A single queue budget covers the operator pause and subsequent limiter phases. The budget is an admission-only deadline; it is not attached to upstream I/O after admission. A pause does not refresh a client's deadline. Existing route/global queue-depth checks continue to bound their respective limiter queues; the operator pause is a distinct provider gate and does not change those per-pool depth semantics.

A non-expiring action sets the current process baseline. A TTL override restores that baseline only if it has not been superseded. A stale timer cannot undo a newer action. On independent limiter mutation the expiry is recorded as superseded, with `applied=false`; it does not claim restoration. Restarting reloads CLI configuration.

Cancellation works before provider resolution as well as after it. An actual operator cancellation interrupts a slow inbound body read and aborts the client exchange; a request cancelled before response headers cannot fall through to an empty HTTP 200. Normal request-finalization cleanup does not interrupt reads or disrupt keepalive reuse.

Cancellation does not prove that the remote provider has stopped work. It does not remove failure holds, cancellation cooldowns, post-release cooldowns, or a request's breaker obligations.

## Lease accounting

The queue is a FIFO lease allocator. The invariant is:

`occupied = active leases + post-release cooldown leases`

Adaptive withholding is a capacity reduction, not another occupancy. Shrink debt is `max(0, occupied - effective_limit)`. Shrinking never recalls a lease or changes its release deadline. No waiter is granted until occupied is below the effective limit; growing can grant FIFO waiters immediately. Cancelled waiters are removed, and a cancellation racing a grant returns that reservation before returning an error.

The returned release function is idempotent. Proxy failure/cancel holds continue to delay calling release. A limiter's `Active` therefore counts caller-held leases, including a proxy's accounting hold; `Cooldown` starts only after release is called. The distinction is explicit in snapshots and history rather than calling both states active requests.

`SetObserver` runs a bounded, memory-only callback under the limiter lock at each transition, including initial state. The callback cannot call the limiter or perform I/O. This preserves transition order for capacity integrals without sampling away short saturation or cooldown events. The observation hub is independent of the UI refresh cadence.

## Logical requests and actual attempts

A request gets one epoch-qualified logical ID at original ingress. Provider resolution transfers its active scope while retaining the original received-provider cohort. Provider proxying never creates a second logical request when the context already carries one. Route facts use a Gemini template or a configured route/group key; arbitrary path and model strings are not Prometheus labels.

Actual attempts are observed inside signing at the base transport boundary. A signer failure is not an attempt. A retry and a credential refresh resend are separate attempt kinds. The selected response's status and final body outcome remain separate: HTTP 200 can precede a failed body, and a locally emitted 503 is not an upstream status.

Attempt duration extends to response-body EOF, non-EOF read failure, or close. Closing unread success bytes produces an unknown body outcome; it does not prove success. HTTP 101 is measured as an HTTP handshake, and its bidirectional body is returned unchanged, including `CloseWrite` capability. Opaque upgraded protocol traffic is outside HTTP body accounting.

Admission histograms include only requests that actually waited, using the union of suite buffer admission, operator pause, and limiter wait phases. A suite buffer waiter is observed in the unresolved provider scope until model dispatch resolves its provider. Immediate acquisition does not fabricate an admission-wait sample. A request that waited in several phases contributes one final wait sample.

## Terminal outcome and evidence

| Observation | Logical result | Failure stage |
| --- | --- | --- |
| Complete response with ordinary successful HTTP status | Completed | Unknown / no failure |
| Observed upstream error status, transport error, or non-EOF body read failure | Failed | Upstream |
| Concrete downstream write, short-write, or flush failure | Failed | Downstream |
| Client/context cancellation without a more definitive failure | Aborted | Available provenance, otherwise unknown |
| Local admission refusal, including drain or open circuit | Rejected | Admission |
| Per-attempt signer failure | Failed; aborted when caused by cancelled context | Authentication |
| Request contract conversion failure | Failed | Conversion request |
| Response contract or stream conversion failure | Failed | Conversion response |
| Local recovered panic or failed upgrade handoff | Failed | Proxy |

A known upstream failure retains priority over a concurrent cancellation or downstream failure while delivering the error. Earlier retry failures remain attempt facts; a later successful attempt can complete the logical request. Legacy metrics retain their established definitions, including their own aborted-transfer counters, while new observed metrics use the explicit result table above.

Finalization computes the result once, attaches controlled diagnostic failure metadata and a fingerprint, publishes the journal entry, sets the fact's journal ID, and only then publishes the terminal fact. A request inspector can join the exact retained journal entry immediately. If the ring later evicts that entry, the historical fact still identifies the request and its grouping fingerprint. A separate bounded ingress journal retains suite dispatch and mount-routing refusals that never entered a provider, including their partial client evidence and exact request ID.

Native traffic remains semantically opaque. Its selected upstream response records status, observed byte count and EOF/error completion without retaining another copy of its body. The existing journal remains the native raw-body buffer. Transcoded exchanges additionally record their explicit codec/IR decisions and four evidence legs. Downstream byte counts measure bytes accepted by the writer, not bytes offered to it or guaranteed received by the remote peer.

## Verification

Behavior tests cover immutable preview commands, stale and expired tokens, idempotence, bounded audit state, TTL baseline restoration and supersession, independent pool resize races, FIFO shrink debt, retained cooldown and adaptive headroom, concurrent cancellation/resize lease conservation, and shutdown wakeup of paused requests.

Gateway tests exercise pause before body reads and breaker admission, resume, a shared queue deadline across pause, existing queued permits surviving drain, local catalog availability, exact live cancellation retaining the upstream accounting hold, real HTTP 503-to-200 retries, credential resends, failed signing before transport, unchanged HTTP 101 body identity, request/provider identity handoff, HTTP 200 followed by body failure, failures racing cancellation, downstream write failure, and exact diagnostic fingerprint/journal/fact joins. Run the full repository gates as well as these focused tests; existing admission, breaker, streaming and retry suites remain part of the contract.
