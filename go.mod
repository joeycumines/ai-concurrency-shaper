module github.com/joeycumines/ai-concurrency-shaper

go 1.27.1

require (
	charm.land/bubbletea/v2 v2.0.9
	charm.land/lipgloss/v2 v2.0.6
	github.com/charmbracelet/x/term v0.2.2
	github.com/joeycumines/go-prompt v0.0.0-20260825080242-5d867e11e478
	github.com/joeycumines/sesame v0.1.1
	github.com/rivo/uniseg v0.4.7
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20260903151058-ae99b731b8c5 // indirect
	github.com/charmbracelet/x/ansi v0.11.8 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/creack/pty v1.1.24 // indirect
	github.com/joeycumines/simple-command-output-filter v0.2.1 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-runewidth v0.0.29 // indirect
	github.com/mattn/go-tty v0.0.8 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/pkg/term v1.2.0-beta.2 // indirect
	github.com/xo/terminfo v1.0.0 // indirect
	golang.org/x/exp v0.0.0-20260908205506-85c1c2202aba // indirect
	golang.org/x/exp/typeparams v0.0.0-20260908205506-85c1c2202aba // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/telemetry v0.0.0-20260924152758-ed294f943157 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/tools v0.51.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260928230214-8a89bd6388cc // indirect
	honnef.co/go/tools v0.8.1 // indirect
)

tool (
	github.com/joeycumines/simple-command-output-filter
	golang.org/x/tools/cmd/deadcode
	honnef.co/go/tools/cmd/staticcheck
)

replace github.com/joeycumines/sesame => ../sesame
