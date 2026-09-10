// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package tui

import (
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
)

// logPollInterval is how often the Logs tab poller checks the shared LogBuffer
// for freshly captured log lines.
const logPollInterval = 100 * time.Millisecond

// Run (multi-provider form): see the doc comment above the signature.
// resetCh is the channel the model's "Reset Stats" action signals on; main
// owns it, drains it, and resets every provider's metrics collector on
// receipt. It must be buffered (cap >= 1) so the model's non-blocking send
// never drops the very first request. A nil resetCh disables the signal.
func Run(updates <-chan ProviderUpdate, metas []ProviderMeta, progCh chan<- *tea.Program, resetCh chan struct{}, logBuf *LogBuffer) *tea.Program {
	m := NewModelForProviders(metas)
	if resetCh != nil {
		m.resetCh = resetCh
	}
	if logBuf != nil {
		m.logBuf = logBuf

	}
	p := tea.NewProgram(m)

	done := make(chan struct{})

	if logBuf != nil {
		go func() {
			defer func() { recover() }()
			ticker := time.NewTicker(logPollInterval)
			defer ticker.Stop()
			// Replay any startup lines promptly. The model drains via ReadNew(logBufSeen)
			// inside Update, so this first tick guarantees buffered startup summaries
			// (written before Run) are not delayed a full poll interval.
			// Subsequent ticks are revision-gated to avoid 10 Hz wake-ups when idle.
			// Note: bubbletea v2.0.7 Program.Send is non-blocking after shutdown
			// (select { case <-ctx.Done(): case msgs <- msg: } at tea.go:1183), so even
			// if ticker and done are simultaneously ready at shutdown, Send cannot leak.
			lastSentRev := logBuf.Revision()
			if lastSentRev != 0 {
				p.Send(logPollTickMsg{})
			}
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					if cur := logBuf.Revision(); cur != lastSentRev {
						lastSentRev = cur
						p.Send(logPollTickMsg{})
					}
				}
			}
		}()
	}

	go func() {
		defer func() { recover() }()
		for upd := range updates {
			p.Send(upd)
		}
	}()

	if progCh != nil {
		progCh <- p
	}

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI: %v\n", err)
	}
	close(done)
	return p
}
