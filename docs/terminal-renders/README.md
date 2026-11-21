# Terminal render gallery

These frames come from the implemented Bubble Tea model's `View()` using deterministic **synthetic** observations. The diagrams and text are terminal output, not a separately drawn interface. The real HTTP/SQLite/terminal journeys are tested independently in `internal/tui/tuitest/observability_journey_test.go`.

Each scene includes the original `.ansi`, an escape-free `.txt`, a Freeze `.svg`, and a `.png`. The images are static documentation of the interactive application. Cursor movement, brushing, filters, time selection, drilldown, exports and reviewed runtime controls operate in the actual TUI.

## Small panes

| Scene | Dark | Light |
| --- | --- | --- |
| Overview, 40×20 | [PNG](overview-dark-40x20.png) · [text](overview-dark-40x20.txt) | [PNG](overview-light-40x20.png) · [text](overview-light-40x20.txt) |
| Rates, 40×20 | [PNG](rates-dark-40x20.png) · [ANSI](rates-dark-40x20.ansi) | [PNG](rates-light-40x20.png) · [ANSI](rates-light-40x20.ansi) |
| Controls, 40×20 | [PNG](controls-dark-40x20.png) | [PNG](controls-light-40x20.png) |
| Expanded vertical space, 40×40 | [Rates](rates-dark-40x40.png) | [Latency heatmap](heatmap-light-40x40.png) |
| Investigation, 40×20 | [Requests](requests-dark-40x20.png) · [Evidence](evidence-dark-40x20.png) | [Paged action review](review-light-40x20.png) · [Usage](usage-light-40x20.png) |

## Expanded workspaces

| Visualization | Render |
| --- | --- |
| Rates and numerical inspector | [160×48, dark](rates-dark-160x48.png) |
| Latency density and duration bins | [160×48, light](heatmap-light-160x48.png) |
| Named pool capacity | [80×24, light](capacity-light-80x24.png) |
| Merged duration distribution | [120×40, dark](distribution-dark-120x40.png) |
| Provider comparison | [160×48, dark](comparison-dark-160x48.png) |
| Source-reported usage and coverage | [160×48, dark](usage-dark-160x48.png) |
| Conversion fidelity | [160×48, light](fidelity-light-160x48.png) |
| Actual attempts and retry amplification | [120×40, dark](attempts-dark-120x40.png) |

## Reproduce

Use the repository's Go version. Freeze and Inkscape are documentation tools and are not gateway dependencies. Install Freeze independently with `go install github.com/charmbracelet/freeze@v0.2.2` and install Inkscape through your operating system's package manager.

From the repository root:

```sh
python3 docs/terminal-renders/render.py --frames
```

For the delivered Linux render, DejaVu Sans Mono was embedded and the system supplied Unicode font fallback, including Noto CJK:

```sh
python3 docs/terminal-renders/render.py --frames \
  --font /usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf
```

The script deliberately uses Freeze's SVG output followed by Inkscape. It positions each glyph on the terminal's measured cell grid: SVG fallback fonts otherwise give Braille glyphs proportional advances and shift later columns, unlike a real terminal. Text, ANSI colors and terminal cell contents are preserved. Uncolored text uses the documented terminal profile's foreground, including dark ink on a light background. The supplied scenes have no emoji/ZWJ clusters; the reproduction helper rejects those rather than guessing their widths.

Freeze v0.2.2's own PNG conversion failed with the Go 1.27 toolchain in this environment; that renderer failure is separate from the gateway and its tests. PNG pixels may vary with font availability. Cell dimensions, ANSI source and hashes are recorded in `render-manifest.json`.
