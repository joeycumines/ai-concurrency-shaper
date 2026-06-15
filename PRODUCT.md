# Product

## Register

product

## Users

Engineers and operators running HTTP proxies in front of AI/LLM providers. They run the binary from a terminal or in a headless environment, watch live traffic, and adjust concurrency/queue/circuit-breaker behavior. Primary context: a busy terminal session where they need status at a glance and quick inspection of requests.

## Product Purpose

`ai-concurrency-shaper` is a reverse proxy that enforces bounded concurrency on configured routes and blocks excess requests until slots open. The built-in terminal dashboard (TUI) visualizes live throughput, concurrency, queue depth, status distribution, in-flight requests, circuit breaker state, network timing, and per-route statistics — without requiring a browser.

## Brand Personality

- **Expert confidence:** precise numbers, clear states, no decorative noise.
- **Operator urgency:** high-signal density, fast navigation, obvious health/status.
- **Terminal-native:** respects the terminal grid, works in low-color and true-color, no faux web UI in the console.

## Anti-references

- Avoid terminal dashboards that look like uncanny web pages (rounded shadows, gradients, fake browser chrome).
- Avoid color for decoration; every hue must encode state.
- Avoid novelty typefaces or ASCII art that displaces useful data.
- Avoid auto-playing animations that fight the operator’s intent.

## Design Principles

1. **State is color, color is state.** Hue exists to encode meaning (success/error/queue/active), not mood.
2. **Dense but scannable.** Favor compact tables, sparklines, and labeled gauges over whitespace.
3. **Keyboard-first, mouse-second.** All interactions work via keys; mouse is a convenience layer on top.
4. **Calm under load.** High-contrast accents are reserved for actionable or anomalous states; normal operation stays readable over long sessions.
5. **Terminal realism.** Design for real terminals: limited palette, mouse cell coordinates, no animation that depends on CSS.

## Accessibility & Inclusion

- Tested under `tmux`, `screen`, real `xterm-256color`, and Apple Terminal.
- Uses true-color ANSI sequences when available; falls back to 256-color and 16-color palettes.
- Respects `prefers-reduced-motion` by avoiding flashing progress updates (updates are polling-driven).
- Status hues are chosen for deuteranopia/protanopia distinguishability by pairing color with glyph/position/label.
