# ai-concurrency-shaper

Reverse proxy with bounded concurrency for AI/LLM API endpoints.

Sits in front of an upstream HTTP API (e.g. Anthropic, OpenAI) and limits concurrent requests to configured routes. Requests that exceed the limit block until a slot opens — no client-side backoff needed. Non-matching requests pass through unmodified.

Run with `-tui` for a terminal dashboard with live metrics and request inspection.

<p align="center">
  <img src="docs/assets/tui.webp" alt="TUI dashboard showing live request metrics" width="800">
</p>

## Install

```sh
go install github.com/joeycumines/ai-concurrency-shaper@latest
```

## Usage

```sh
ai-concurrency-shaper -upstream https://api.anthropic.com
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-upstream` | _(required)_ | Upstream base URL |
| `-bind` | `:8080` | Listen address |
| `-limit` | _(repeatable)_ | Route pattern to limit (defaults to common AI endpoints) |
| `-concurrency` | `4` | Max concurrent limited requests |
| `-global-concurrency` | `0` | Global concurrency limit (0 = disabled) |
| `-queue-timeout` | `30s` | Max wait for a concurrency slot |
| `-retry` | `-1` | Max retry attempts (-1 = unlimited, 0 = disabled) |
| `-retry-max-body-mb` | `5` | Max request body size (MB) eligible for retry |
| `-tui` | `false` | Enable terminal dashboard |
| `-version` | | Print version and exit |

### Examples

Proxy with default limits and TUI:

```sh
ai-concurrency-shaper -upstream https://api.anthropic.com -tui
```

Custom concurrency with per-route limits:

```sh
ai-concurrency-shaper \
  -upstream https://api.openai.com \
  -limit "POST /v1/chat/completions:2" \
  -limit "POST /v1/embeddings:4" \
  -global-concurrency 10
```

Grouped routes sharing a limiter:

```sh
ai-concurrency-shaper \
  -upstream https://api.anthropic.com \
  -limit "POST /v1/messages@messages:3" \
  -limit "POST /v1/messages/batches@messages:3"
```

## Building

```sh
make build          # compile
make test           # run tests
make lint           # vet + staticcheck + deadcode
make all            # build, then lint + test
```

## License

[GPL-3.0](LICENSE)
