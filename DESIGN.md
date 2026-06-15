# Design

## Register

product

## Current visual system

The interface is a single full-screen Bubble Tea v2 terminal dashboard rendered with Lip Gloss v2. All visual values are defined as package-level `lipgloss.Style` variables in `internal/tui/tui.go`.

## Color palette

Palette is terminal-friendly true-color, designed against a near-black background for long operator sessions.

| Token / style               | Hex       | Role                                                                 |
| ---------------------------- | --------- | -------------------------------------------------------------------- |
| Background (`#0D1117`)      | `#0D1117` | Term bg; used in header and footer background.                       |
| Surface (`#161B22`)         | `#161B22` | Mildly elevated surface; tab inactive bg, overlay bg.                |
| Panel (`#21262D`)           | `#21262D` | Gauge empty track, subtle separators.                                  |
| Text primary (`#E6EDF3`)    | `#E6EDF3` | Body text, selected row fg over accent.                                |
| Text secondary (`#8B949E`)    | `#8B949E` | Dim labels, inactive tabs, footer/status text.                       |
| Text muted (`#6E7681`)      | `#6E7681` | Very dim hints, passthrough tag.                                     |
| Accent blue (`#58A6FF`)     | `#58A6FF` | Tab active, link/section titles, table headers, borders.               |
| Accent blue-bright (`#388BFD`)| `#388BFD`| Row selection background               |
| Success green (`#3FB950`)     | `#3FB950` | Active gauge, 2xx status, download segment, CLOSED breaker.           |
| Warning amber (`#D29922`)     | `#D29922` | Queue depth bar, queued waterfall segment, limited tag.               |
| Error red (`#F85149`)         | `#F85149` | 5xx status, OPEN breaker state.                                       |
| Error/alert orange (`#F0883E`)| `#F0883E` | 4xx status, limited tag, limited-tag text, HALF_OPEN breaker.         |
| Circuit closed green (`#3FB950`)| `#3FB950` | CLOSED circuit breaker state.                                          |
| Circuit open red (`#F85149`)  | `#F85149` | OPEN circuit breaker state (bold).                                     |
| Circuit half-open amber (`#F0883E`)| `#F0883E` | HALF_OPEN circuit breaker state.                                    |
| Separator (`#30363D`)         | `#30363D` | Horizontal line separators.                                          |

### Status distribution colors

- `1xx` → `statusInfoStyle` (`#8B949E`)
- `2xx` → `statusOkStyle` (`#3FB950`)
- `3xx` → `statusRedirectStyle` (`#58A6FF`)
- `4xx` → `statusClientErrStyle` (`#F0883E`)
- `5xx` → `statusServerErrStyle` (`#F85149`)

## Typography

- One typeface: terminal monospace (depends on user terminal).
- No manual font sizes; terminal cells are the atomic unit.
- Hierarchy is created through **bold**, **color**, **borders**, and **section labels**.
- Section labels are rendered with `sectionStyle` (bold, accent blue) and wrapped in a single leading space for optical left alignment.

## Layout

- Chrome: header (row 0), tab bar (row 1), separator (row 2), content (rows 3..n-2), footer (last row).
- Content area uses a right-hand scrollbar in the last column.
- Dashboard stacks vertically: Throughput, Concurrency, Queue Depth, Status Distribution, In-Flight Requests, Summary, optional Circuit Breaker.
- Scrollable tabs (Requests, Network, Logs, Concurrency, Routes) have tables/lists with selected-row highlight.

## Components

1. **Header** — bold white text on blue, shows proxy brand, active/queued/req-rate/errors/uptime.
2. **Tab bar** — active tab fills blue on dark, inactive tabs are gray on surface.
3. **Section header** — blue bold text, no background.
4. **Gauge bar** — severity-scaled active blocks `█` (green ≤59%, amber 60–89%, red ≥90%), dark empty blocks `░`, labels inline.
5. **Queue bar** — severity-scaled active blocks `█` (green 0%, amber 1–49%, orange 50–89%, red ≥90%), same dark empty track.
6. **Status distribution bar** — stacked colored blocks `█` proportional to count, then `label:count` colored to match its segment.
7. **Sparkline** — severity-colored throughput trace; the whole line is shaded by the latest bucket's ratio to the window maximum (≤59% blue, 60–89% amber, ≥90% red). Empty data shows a dim em dash.
8. **Table rows** — primary text, selected row uses blue background with dark text.
9. **Scrollbars** — track char `│`, thumb char `█`, proportional.
10. **Toasts** — stacked at bottom (up to 3). [Style to be added.]
11. **Overlays** (help, confirm, detail) — surface background, rounded border in accent blue, padding.

## Motion / interaction

- Static 250 ms ticker is NOT used; dashboard refreshes on incoming `metrics.Snapshot` (~4 fps).
- No cursor blink.
- Reduced-motion users: same behavior, no animated spinners (sparkline is data-driven).
- Mouse: wheel scrolls, content click sets cursor, scrollbar click/drag jumps distance proportionally.
- Keyboard: 1-6 tabs, j/k arrows, PgUp/PgDn, Home/End, Ctrl-U/D, g/G, / filter, t type, s status, Enter inspect, ? help, q quit.

## Resolved design issues

- [x] 1–5 resolved by TUI-COLOR-01..05.
- [x] Sparkline severity color resolved by TUI-COLOR-07.
- [x] In-flight empty state dimming resolved by TUI-COLOR-08.

## Open design issues to resolve

1. The Summary row could use subtle label/value color separation without breaking existing substring tests.
2. The Network tab waterfall uses three fixed colors; consider scaling the queue segment color by relative queue duration.
3. Table headers and section labels could use a slightly more distinctive hierarchy (e.g., underlines or spacing) on very large terminals.
