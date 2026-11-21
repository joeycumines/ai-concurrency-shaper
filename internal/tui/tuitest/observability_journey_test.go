//go:build unix

package tuitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	"github.com/rivo/uniseg"
)

// journeyScreen consumes the application's actual PTY output through Charm's
// ANSI parser. Assertions read the current screen, never text left in the
// scrollback from a previous view. The model covers the renderer's cursor,
// erasure, repeat, scrolling margins and line/character insertion operations;
// SGR affects color, not the cells being asserted. Production uses Bubble Tea's
// native renderer.
type journeyScreen struct {
	mu                         sync.Mutex
	parser                     *ansi.Parser
	cells                      [][]string
	w, h, x, y, savedX, savedY int
	scrollTop, scrollBottom    int
	last                       rune
	revision                   uint64
	raw                        []byte
}

func newJourneyScreen(w, h int) *journeyScreen {
	s := &journeyScreen{}
	s.resize(w, h)
	s.parser = ansi.NewParser()
	s.parser.SetHandler(ansi.Handler{Print: s.print, Execute: s.execute, HandleCsi: s.csi, HandleEsc: s.esc})
	return s
}
func (s *journeyScreen) resize(w, h int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w, s.h = w, h
	s.x, s.y = 0, 0
	s.scrollTop, s.scrollBottom = 0, h-1
	s.cells = make([][]string, h)
	for y := range s.cells {
		s.cells[y] = make([]string, w)
		for x := range s.cells[y] {
			s.cells[y][x] = " "
		}
	}
	s.revision++
}
func (s *journeyScreen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw = append(s.raw, p...)
	for _, b := range p {
		s.parser.Advance(b)
	}
	s.revision++
	return len(p), nil
}
func (s *journeyScreen) text() (string, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []string
	for _, r := range s.cells {
		rows = append(rows, strings.Join(r, ""))
	}
	return strings.Join(rows, "\n"), s.revision
}
func (s *journeyScreen) blankRow(y int) {
	if y < 0 || y >= s.h {
		return
	}
	for x := range s.cells[y] {
		s.cells[y][x] = " "
	}
}
func (s *journeyScreen) print(r rune) {
	w := uniseg.StringWidth(string(r))
	if w == 0 {
		if s.x > 0 && s.y < s.h {
			s.cells[s.y][min(s.x-1, s.w-1)] += string(r)
		}
		return
	}
	if s.x >= s.w {
		s.x = 0
		s.execute('\n')
	}
	if s.x >= 0 && s.y >= 0 && s.y < s.h && s.x < s.w {
		s.cells[s.y][s.x] = string(r)
		for i := 1; i < w && s.x+i < s.w; i++ {
			s.cells[s.y][s.x+i] = ""
		}
	}
	s.x += w
	s.last = r
}
func (s *journeyScreen) execute(b byte) {
	switch b {
	case '\r':
		s.x = 0
	case '\n', 0x0b, 0x0c:
		if s.y == s.scrollBottom {
			s.scrollUp(1)
		} else {
			s.y = min(s.y+1, s.h-1)
		}
	case '\b':
		s.x = max(s.x-1, 0)
	case '\t':
		s.x = min((s.x/8+1)*8, s.w-1)
	}
}
func (s *journeyScreen) esc(c ansi.Cmd) {
	switch c.Final() {
	case '7':
		s.savedX, s.savedY = s.x, s.y
	case '8':
		s.x, s.y = s.savedX, s.savedY
	case 'D':
		s.execute('\n')
	case 'E':
		s.x = 0
		s.execute('\n')
	case 'M':
		if s.y == s.scrollTop {
			s.scrollDown(1)
		} else {
			s.y = max(s.y-1, 0)
		}
	}
}
func (s *journeyScreen) scrollUp(n int) {
	n = min(n, s.scrollBottom-s.scrollTop+1)
	copy(s.cells[s.scrollTop:s.scrollBottom+1], s.cells[s.scrollTop+n:s.scrollBottom+1])
	for y := s.scrollBottom - n + 1; y <= s.scrollBottom; y++ {
		s.cells[y] = make([]string, s.w)
		s.blankRow(y)
	}
}
func (s *journeyScreen) scrollDown(n int) {
	n = min(n, s.scrollBottom-s.scrollTop+1)
	copy(s.cells[s.scrollTop+n:s.scrollBottom+1], s.cells[s.scrollTop:s.scrollBottom-n+1])
	for y := s.scrollTop; y < s.scrollTop+n; y++ {
		s.cells[y] = make([]string, s.w)
		s.blankRow(y)
	}
}
func (s *journeyScreen) csi(c ansi.Cmd, p ansi.Params) {
	v := func(i, def int) int {
		n, _, ok := p.Param(i, def)
		if !ok {
			return def
		}
		return n
	}
	n := max(v(0, 1), 1)
	switch c.Final() {
	case 'H', 'f':
		s.y = max(0, min(v(0, 1)-1, s.h-1))
		s.x = max(0, min(v(1, 1)-1, s.w-1))
	case 'A':
		s.y = max(s.y-n, 0)
	case 'B', 'e':
		s.y = min(s.y+n, s.h-1)
	case 'C', 'a':
		s.x = min(s.x+n, s.w-1)
	case 'D':
		s.x = max(s.x-n, 0)
	case 'E':
		s.x = 0
		s.y = min(s.y+n, s.h-1)
	case 'F':
		s.x = 0
		s.y = max(s.y-n, 0)
	case 'G', '`':
		s.x = max(0, min(v(0, 1)-1, s.w-1))
	case 'd':
		s.y = max(0, min(v(0, 1)-1, s.h-1))
	case 'J':
		switch v(0, 0) {
		case 2, 3:
			for y := range s.cells {
				s.blankRow(y)
			}
		case 0:
			for x := min(s.x, s.w); x < s.w; x++ {
				s.cells[s.y][x] = " "
			}
			for y := s.y + 1; y < s.h; y++ {
				s.blankRow(y)
			}
		case 1:
			for y := 0; y < s.y; y++ {
				s.blankRow(y)
			}
			for x := 0; x <= min(s.x, s.w-1); x++ {
				s.cells[s.y][x] = " "
			}
		}
	case 'K':
		start, end := 0, s.w
		if v(0, 0) == 0 {
			start = min(s.x, s.w)
		} else if v(0, 0) == 1 {
			end = min(s.x+1, s.w)
		}
		for x := start; x < end; x++ {
			s.cells[s.y][x] = " "
		}
	case 'X':
		for x := s.x; x < min(s.x+n, s.w); x++ {
			s.cells[s.y][x] = " "
		}
	case 'b':
		for i := 0; i < n; i++ {
			s.print(s.last)
		}
	case 'P':
		if s.x < s.w {
			n = min(n, s.w-s.x)
			copy(s.cells[s.y][s.x:], s.cells[s.y][s.x+n:])
			for x := s.w - n; x < s.w; x++ {
				s.cells[s.y][x] = " "
			}
		}
	case '@':
		if s.x < s.w {
			n = min(n, s.w-s.x)
			copy(s.cells[s.y][s.x+n:], s.cells[s.y][s.x:])
			for x := s.x; x < s.x+n; x++ {
				s.cells[s.y][x] = " "
			}
		}
	case 'S':
		s.scrollUp(n)
	case 'T':
		s.scrollDown(n)
	case 'r':
		top, bottom := max(v(0, 1)-1, 0), min(v(1, s.h)-1, s.h-1)
		if top < bottom {
			s.scrollTop, s.scrollBottom = top, bottom
			s.x, s.y = 0, 0
		}
	case 'L':
		if s.y < s.scrollTop || s.y > s.scrollBottom {
			break
		}
		n = min(n, s.scrollBottom-s.y+1)
		copy(s.cells[s.y+n:s.scrollBottom+1], s.cells[s.y:s.scrollBottom-n+1])
		for y := s.y; y < s.y+n; y++ {
			s.cells[y] = make([]string, s.w)
			s.blankRow(y)
		}
	case 'M':
		if s.y < s.scrollTop || s.y > s.scrollBottom {
			break
		}
		n = min(n, s.scrollBottom-s.y+1)
		copy(s.cells[s.y:s.scrollBottom+1], s.cells[s.y+n:s.scrollBottom+1])
		for y := s.scrollBottom - n + 1; y <= s.scrollBottom; y++ {
			s.cells[y] = make([]string, s.w)
			s.blankRow(y)
		}
	case 's':
		s.savedX, s.savedY = s.x, s.y
	case 'u':
		s.x, s.y = s.savedX, s.savedY
	case 'h':
		if c.Prefix() == '?' && v(0, 0) == 1049 {
			for y := range s.cells {
				s.blankRow(y)
			}
			s.x, s.y = 0, 0
		}
	}
}

type observabilityJourney struct {
	t            *testing.T
	tty          *os.File
	cmd          *exec.Cmd
	screen       *journeyScreen
	exited       chan error
	done         chan struct{}
	url, dir, db string
	closed       bool
}

func launchObservabilityJourney(t *testing.T, bin, theme string, upstream string) *observabilityJourney {
	t.Helper()
	dir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	args := []string{"-bind", address, "-tui", "-history-db", filepath.Join(dir, "observations.db"), "-export-dir", dir, "-diagnostic-capture-bytes", "4096", "-tui-theme", theme, "-tui-color", "truecolor"}
	for _, name := range []string{"alpha", "beta"} {
		args = append(args, "--provider="+name, "-prefix", "/"+name, "-upstream", upstream, "-retry", "0", "-circuit-breaker=false", "-release-cooldown", "0", "-cancel-cooldown", "0", "-failure-hold", "0", "-retry-min-delay", "0")
		if name == "alpha" {
			args = append(args, "-transcode-messages-chat")
		}
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 40, Rows: 20})
	if err != nil {
		t.Fatal(err)
	}
	j := &observabilityJourney{t: t, tty: tty, cmd: cmd, screen: newJourneyScreen(40, 20), exited: make(chan error, 1), done: make(chan struct{}), url: "http://" + address, dir: dir, db: filepath.Join(dir, "observations.db")}
	go func() { _, _ = io.Copy(j.screen, tty); close(j.done) }()
	go func() { j.exited <- cmd.Wait() }()
	t.Cleanup(j.close)
	j.await("initial40x20", 0, func(s string) bool { return strings.Contains(s, "alpha") && strings.Contains(s, "OVERVIEW") })
	return j
}
func (j *observabilityJourney) close() {
	if j.closed {
		return
	}
	j.closed = true
	_, _ = j.tty.Write([]byte{3})
	select {
	case <-j.exited:
	case <-time.After(8 * time.Second):
		_ = j.cmd.Process.Kill()
		<-j.exited
	}
	_ = j.tty.Close()
	<-j.done
}
func (j *observabilityJourney) await(label string, after uint64, predicate func(string) bool) string {
	j.t.Helper()
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		text, rev := j.screen.text()
		if rev > after && predicate(text) {
			return text
		}
		select {
		case <-deadline.C:
			j.screen.mu.Lock()
			raw := string(j.screen.raw)
			j.screen.mu.Unlock()
			if i := strings.Index(raw, "panic:"); i >= 0 {
				raw = raw[i:min(i+6000, len(raw))]
			} else {
				raw = "No panic recorded."
			}
			j.t.Fatalf("%s: current terminal:\n%s\n%s", label, text, raw)
			return ""
		case <-tick.C:
		}
	}
}
func (j *observabilityJourney) send(keys, label string, predicate func(string) bool) string {
	j.t.Helper()
	_, rev := j.screen.text()
	if _, err := j.tty.WriteString(keys); err != nil {
		j.t.Fatal(err)
	}
	return j.await(label, rev, predicate)
}
func (j *observabilityJourney) resize(w, h uint16, label string) {
	j.t.Helper()
	j.screen.resize(int(w), int(h))
	_, rev := j.screen.text()
	if err := pty.Setsize(j.tty, &pty.Winsize{Cols: w, Rows: h}); err != nil {
		j.t.Fatal(err)
	}
	j.await("resize "+label, rev, func(s string) bool { return strings.Contains(s, label) })
}

var pagePattern = regexp.MustCompile(`Page (\d+)/(\d+)`)

func (j *observabilityJourney) finishReview() {
	j.t.Helper()
	for {
		screen, _ := j.screen.text()
		m := pagePattern.FindStringSubmatch(screen)
		if len(m) != 3 {
			j.t.Fatalf("review page missing:\n%s", screen)
		}
		page, _ := strconv.Atoi(m[1])
		pages, _ := strconv.Atoi(m[2])
		if page == pages {
			return
		}
		j.send("\x1b[C", "next review page", func(s string) bool { return strings.Contains(s, fmt.Sprintf("Page %d/%d", page+1, pages)) })
	}
}
func (j *observabilityJourney) exportFiles() []string {
	entries, _ := os.ReadDir(j.dir)
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(j.dir, e.Name()))
		}
	}
	return files
}
func (j *observabilityJourney) awaitExport(n int) []string {
	j.t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		files := j.exportFiles()
		if len(files) >= n {
			return files
		}
		select {
		case <-deadline:
			j.t.Fatalf("expected %d exports, found %d", n, len(files))
			return nil
		case <-ticker.C:
		}
	}
}
func TestPTY_ObservabilitySmallPaneWorkflow(t *testing.T) {
	bin := buildBinary(t)
	for _, theme := range []string{"dark", "light"} {
		t.Run(theme, func(t *testing.T) {
			var calls atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer up.Close()
			j := launchObservabilityJourney(t, bin, theme, up.URL)
			response, err := http.Post(j.url+"/alpha/v1/messages", "application/json", strings.NewReader(`{"model":"test","max_tokens":16,"new_client_field":"private-value","messages":[{"role":"user","content":"Update the QR code."}]}`))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 400 {
				t.Fatalf("conversion rejection=%d", response.StatusCode)
			}
			response, err = http.Get(j.url + "/beta/health")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if calls.Load() != 1 {
				t.Fatalf("client conversion failure reached upstream: %d calls", calls.Load())
			}
			j.send("7", "history measurements", func(s string) bool {
				return strings.Contains(s, "RATES") && strings.Contains(s, "2 finished") && !strings.Contains(s, "querying")
			})
			j.send("i", "time cohort", func(s string) bool { return strings.Contains(s, "REQUESTS") && strings.Contains(s, "/ 2") })
			j.send("/messages\r", "filter request cohort", func(s string) bool { return strings.Contains(s, "/ messages") && strings.Contains(s, "/ 1") })
			j.send("n", "exact request evidence", func(s string) bool {
				return strings.Contains(s, "NETWORK EVIDENCE") && strings.Contains(s, "Request ID:")
			})
			j.send("\t", "conversion failure details", func(s string) bool {
				return strings.Contains(s, "2/7 Decisions") && strings.Contains(s, "unknown_field")
			})
			j.send("e", "metadata export", func(s string) bool { return strings.Contains(s, "EXPORT SAVED") })
			files := j.awaitExport(1)
			data, _ := os.ReadFile(files[0])
			if strings.Contains(string(data), "private-value") {
				t.Fatal("default export leaked retained client content")
			}
			var metadata struct {
				Count   int  `json:"count"`
				Raw     bool `json:"raw"`
				Entries []struct {
					RequestID string `json:"request_id"`
				} `json:"entries"`
			}
			if err := json.Unmarshal(data, &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata.Count != 1 || metadata.Raw || len(metadata.Entries) != 1 || metadata.Entries[0].RequestID == "" {
				t.Fatalf("wrong exact selection: %s", data)
			}
			j.send("\x1b", "back to cohort", func(s string) bool { return strings.Contains(s, "REQUESTS ·") && !strings.Contains(s, "EXPORT SAVED") })
			j.send("n", "reopen retained request", func(s string) bool { return strings.Contains(s, "NETWORK EVIDENCE") })
			j.send("E", "raw consent", func(s string) bool { return strings.Contains(s, "RAW EXPORT") })
			j.send("RAW\r", "raw review", func(s string) bool { return strings.Contains(s, "RAW INVESTIGATION EXPORT") })
			j.finishReview()
			j.send("\t\r", "raw export saved", func(s string) bool { return strings.Contains(s, "EXPORT SAVED") })
			files = j.awaitExport(2)
			rawFound := false
			for _, p := range files {
				data, _ := os.ReadFile(p)
				var raw struct {
					Raw bool `json:"raw"`
				}
				_ = json.Unmarshal(data, &raw)
				rawFound = rawFound || raw.Raw
			}
			if !rawFound {
				t.Fatal("explicit raw export missing")
			}
			j.send("\x1b", "back from saved export", func(s string) bool { return strings.Contains(s, "REQUESTS ·") && !strings.Contains(s, "EXPORT SAVED") })
			j.send("8", "controls", func(s string) bool { return strings.Contains(s, "LOCAL RUNTIME CONTROLS") })
			j.send("p", "pause review", func(s string) bool { return strings.Contains(s, "REVIEW RUNTIME CHANGE") })
			j.send("\r", "default Cancel", func(s string) bool { return strings.Contains(s, "LOCAL RUNTIME CONTROLS") })
			j.send("p", "pause again", func(s string) bool { return strings.Contains(s, "REVIEW RUNTIME CHANGE") })
			j.finishReview()
			j.send("\t\r", "paused provider", func(s string) bool { return strings.Contains(s, "PAUSED") })
			done := make(chan int, 1)
			go func() {
				client := http.Client{Timeout: 8 * time.Second}
				r, e := client.Get(j.url + "/alpha/health")
				if e != nil {
					done <- 0
					return
				}
				r.Body.Close()
				done <- r.StatusCode
			}()
			select {
			case code := <-done:
				t.Fatalf("paused arrival escaped gate: %d", code)
			case <-time.After(100 * time.Millisecond):
			}
			j.resize(160, 48, "LOCAL RUNTIME CONTROLS")
			j.resize(40, 40, "LOCAL RUNTIME CONTROLS")
			j.send("u", "resume review", func(s string) bool { return strings.Contains(s, "REVIEW RUNTIME CHANGE") })
			j.finishReview()
			j.send("\t\r", "running provider", func(s string) bool { return strings.Contains(s, "RUNNING") })
			select {
			case code := <-done:
				if code != 200 {
					t.Fatalf("resumed request=%d", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("resume did not release waiting request")
			}
			j.close()
			if info, err := os.Stat(j.db); err != nil || info.Size() == 0 {
				t.Fatalf("optional history not persisted: %v", err)
			}
		})
	}
}
