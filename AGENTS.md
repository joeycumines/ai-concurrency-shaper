# Mandatory for ALL readers

**This file intentionally contains no directory layout, file listing, or code structure details.**
Providing structural information here makes agents lazy, as they stop exploring source files, i.e. they guess. Don't guess. Read the actual code. Every time.

This is a **stealth reverse proxy** with bounded concurrency and a TUI dashboard.

Required characteristics:

- It sits in front of an upstream HTTP API (e.g. an LLM provider) or APIs.
- Certain request routes (method + path, e.g. `POST /v1/messages`) are "limited" — concurrency-bound and queued.
- By default, requests outside the limited set pass through freely — no introspection needed beyond route matching.
- The proxy is by default completely transparent to both request and response content. Transcoding must provide the strongest guarantees of request/response integrity, and is strictly opt-in, with allowances for compatibility.
- Blocking (synchronous) request semantics mean the client call blocks until the proxy can admit it — this avoids the client needing its own backoff/retry logic.
- The TUI (charm/bubbletea v2) visualizes concurrency, queue state, and live metrics.
- The binary is `go install`-able from `github.com/joeycumines/ai-concurrency-shaper`.

Strict constraints:

- TUI output is not captured anywhere and is visible only to the local operator during an interactive session. Do not redact secrets from TUI display — the journal and TUI may show raw credential headers and URLs.
- Logging must be explicit about what is logged - for example, do not log arbitrary HTTP headers, as they may contain secrets. While the TUI does show the log, log output is also available in non-interactive sessions, so logging must be safe for that context.
