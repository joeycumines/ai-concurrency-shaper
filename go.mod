module github.com/joeycumines/ai-concurrency-shaper

go 1.27.4

require (
	charm.land/bubbletea/v2 v2.2.0
	charm.land/lipgloss/v2 v2.1.1
	github.com/charmbracelet/x/term v0.2.2
	github.com/joeycumines/go-prompt v0.0.0-20261113052755-1e50d6e8b310
	github.com/joeycumines/sesame v0.2.1
	github.com/rivo/uniseg v0.4.7
	golang.org/x/mod v0.43.0
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20261125122737-8ed499892fb4 // indirect
	github.com/charmbracelet/x/ansi v0.12.0 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/creack/pty v1.1.24 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.34.0 // indirect
	github.com/joeycumines/simple-command-output-filter v0.2.1 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-colorable v0.1.16 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-runewidth v0.0.31 // indirect
	github.com/mattn/go-tty v0.0.8 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/pkg/term v1.2.0-beta.2 // indirect
	github.com/xo/terminfo v1.3.1 // indirect
	golang.org/x/exp v0.0.0-20261124000051-d6646c509b6d // indirect
	golang.org/x/exp/typeparams v0.0.0-20261124000051-d6646c509b6d // indirect
	golang.org/x/net v0.63.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/telemetry v0.0.0-20261124234621-1402bdb29cb5 // indirect
	golang.org/x/text v0.44.0 // indirect
	golang.org/x/tools v0.53.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20261123191846-1e707ab9fbc2 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261123191846-1e707ab9fbc2 // indirect
	honnef.co/go/tools v0.8.2 // indirect
)

tool (
	github.com/joeycumines/simple-command-output-filter
	golang.org/x/tools/cmd/deadcode
	honnef.co/go/tools/cmd/staticcheck
)
