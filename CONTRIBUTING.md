# Contributing

Bug reports and pull requests are welcome on GitHub at
[joeycumines/ai-concurrency-shaper](https://github.com/joeycumines/ai-concurrency-shaper).

## Setup

You need Go (see `go.mod` for the minimum version) and GNU Make.

## Development

```sh
make build          # compile
make test           # run tests
make lint           # vet + staticcheck + deadcode
make all            # build, then lint + test
make help           # list all targets
```


### Fleet header invariants — `gmake header-gate`

The fleet (`-tui`) header has a regression-sensitive layout (`internal/tui/header.go`
fleet invariants: `first-chip-truncated => wrap-all` with an empty row 0, dynamic
natural-fit breakpoints, and `chipRowsLayout` as the single source of truth for
both rendering and hit-testing). Run the CI-enforced gate before any header edit:

```sh
gmake header-gate          # race + cover: TestFleet|TestHeader|TestChip|TestWrapping|TestRepro
go test ./internal/tui -run '^$' -bench 'BenchmarkChipRowsLayout|BenchmarkRenderHeader' -benchtime=1x -count=1  # deterministic hot-path (<2s)
```

The benchmarks in `internal/tui/header_bench_test.go` cover widths
20/40/80/120/150/180 × heights 5/8/24 for 2-short, 3-long, 5-mixed, 15-long
and CJK/emoji fleets and report `allocs/op` and `ns/op`. The gate
`TestHeaderThemeGeometryIdentity` pins that dark and light palettes produce
byte-identical geometry (stripped header, `chipRowsLayout` providers/widths,
`headerRowCount`, `identityWidth`, sampled `chipAt` lockstep — exhaustive
lockstep is covered by the property fuzz and the chip hit tests) so a palette
edit that changes the chip box model fails fast.

## License

By contributing, you agree that your contributions will be licensed under the
GNU General Public License v3 (see [LICENSE](LICENSE)).
