//go:build unix

package tuitest

import (
	"strings"
	"testing"
	"time"
)

// screenText reconstructs the current terminal cells from the real PTY stream.
// It intentionally does not strip ANSI and search the accumulated scrollback:
// stale views and cursor-diff fragments cannot prove the current visible state.
func (h *TUIHarness) screenText() string {
	screen := newJourneyScreen(h.cols, h.rows)
	_, _ = screen.Write([]byte(h.Console().String()))
	text, _ := screen.text()
	return text
}

func awaitScreen(t *testing.T, h *TUIHarness, description string, match func(string) bool) string {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		screen := h.screenText()
		if match(screen) {
			return screen
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("%s: %v\nCurrent screen:\n%s", description, t.Context().Err(), screen)
		case <-deadline.C:
			t.Fatalf("%s did not appear\nCurrent screen:\n%s", description, screen)
		case <-tick.C:
		}
	}
}

func screenContains(t *testing.T, h *TUIHarness, needles ...string) string {
	t.Helper()
	return awaitScreen(t, h, strings.Join(needles, ", "), func(screen string) bool {
		for _, needle := range needles {
			if !strings.Contains(screen, needle) {
				return false
			}
		}
		return true
	})
}

func writeKeys(t *testing.T, h *TUIHarness, keys string) {
	t.Helper()
	if _, err := h.Console().WriteString(keys); err != nil {
		t.Fatalf("write keys %q: %v", keys, err)
	}
}

func screenBody(screen string) string {
	rows := strings.Split(screen, "\n")
	if len(rows) < 4 {
		return screen
	}
	// Ignore the changing throughput/header and the fixed keyboard footer.
	return strings.Join(rows[3:len(rows)-1], "\n")
}

func TestTerminalScreenScrollMargins(t *testing.T) {
	screen := newJourneyScreen(8, 6)
	_, _ = screen.Write([]byte("header\r\nrow-1\r\nrow-2\r\nrow-3\r\nrow-4\r\nfooter"))
	// The renderer uses this region-limited reverse index when inserting a
	// newly visible breaker row. Header/footer cells must remain stationary.
	_, _ = screen.Write([]byte("\x1b[2;5r\x1b[2;1H\x1bM\x1b[1;6r"))
	got, _ := screen.text()
	want := "header  \n        \nrow-1   \nrow-2   \nrow-3   \nfooter  "
	if got != want {
		t.Fatalf("reverse index moved cells outside the scrolling margins:\n%q\nwant:\n%q", got, want)
	}
	_, _ = screen.Write([]byte("\x1b[2;5r\x1b[5;1H\n"))
	got, _ = screen.text()
	want = "header  \nrow-1   \nrow-2   \nrow-3   \n        \nfooter  "
	if got != want {
		t.Fatalf("line feed did not scroll inside the margins:\n%q\nwant:\n%q", got, want)
	}
}
