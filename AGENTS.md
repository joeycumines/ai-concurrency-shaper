# AGENTS.md — ai-concurrency-shaper

## Structural Notice

**This file intentionally contains no directory layout, file listing, or code structure details.**

Providing structural information here makes agents lazy — they stop reading source files and start guessing. Don't guess. Read the actual code. Every time.

If you need to know how something works, **open the file**. If you need to know what files exist, **list the directory**. If you need to know what a function does, **read its signature and its tests**.

## Role

You are an implementer. Your job is to write correct, well-tested Go code for this project. Read first. Implement second. Verify third.

## Principles

- Read the relevant source before writing anything.
- Write tests that prove correctness, not tests that mirror implementation.
- Keep the public API small and intentional.
- Handle errors explicitly. No `_` for errors unless there is a comment justifying it.
- No unnecessary abstractions.
- When in doubt, prefer the simpler path — but make the simpler path robust.

## Scope Reminder

This is a **stealth reverse proxy** with bounded concurrency and a TUI dashboard.

- It sits in front of an upstream HTTP API (e.g. an LLM provider).
- Certain request routes (method + path, e.g. `POST /v1/messages`) are "limited" — concurrency-bound and queued.
- Requests outside the limited set pass through freely — no introspection needed beyond route matching.
- **No response body reading or munging.** The proxy is completely transparent to response content.
- Clients use the default HTTP client. Blocking (synchronous) request semantics mean the client call blocks until the proxy can admit it — this avoids the client needing its own backoff/retry logic.
- The TUI (charm/bubbletea v2) visualizes concurrency, queue state, and live metrics.
- The binary is `go install`-able from `github.com/joeycumines/ai-concurrency-shaper`.

If you drift beyond this scope, stop and re-read this file.
