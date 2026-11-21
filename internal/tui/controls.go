package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/joeycumines/ai-concurrency-shaper/internal/control"
	"github.com/rivo/uniseg"
)

type controlPanelState struct {
	snapshot          control.Snapshot
	preview           *control.Preview
	focus             int
	page, seenThrough int
	form              bool
	input             string
	command           control.Command
	pressed           int
	pending           bool
}
type controlResultMsg struct {
	result control.Result
	err    error
}

func (m Model) selectedControlProvider() (control.ProviderState, bool) {
	for _, p := range m.controlPanel.snapshot.Providers {
		if p.Name == m.providerID(m.active) {
			return p, true
		}
	}
	return control.ProviderState{}, false
}
func (m *Model) previewControl(command control.Command) tea.Cmd {
	if m.runtime.Controls == nil {
		m.tell("Runtime controls are unavailable")
		return nil
	}
	preview, err := m.runtime.Controls.Preview(command)
	if err != nil {
		m.tell(err.Error())
		return nil
	}
	m.controlPanel.preview = &preview
	m.controlPanel.page = 0
	m.controlPanel.seenThrough = 0
	m.controlPanel.focus = 0
	m.controlPanel.pressed = -1
	m.controlPanel.form = false
	return nil
}
func (m *Model) applyControl() tea.Cmd {
	if !m.controlCanApply() {
		m.tell("Read each review page before Apply")
		return nil
	}
	token := m.controlPanel.preview.Token
	controller := m.runtime.Controls
	m.controlPanel.preview = nil
	m.controlPanel.pending = true
	return func() tea.Msg { r, e := controller.Apply(token); return controlResultMsg{r, e} }
}
func (m Model) reviewLines() []string {
	p := m.controlPanel.preview
	if p == nil {
		return nil
	}
	rows := []string{"REVIEW RUNTIME CHANGE", "Action: " + string(p.Command.Action), "Provider: " + p.Command.Provider}
	if p.Command.Pool != "" {
		rows = append(rows, "Pool: "+p.Command.Pool, fmt.Sprintf("New limit: %d", p.Command.Limit))
	}
	if p.Command.RequestID != "" {
		rows = append(rows, "Exact request: "+p.Command.RequestID)
	}
	if p.Command.TTL > 0 {
		rows = append(rows, "Expires after: "+p.Command.TTL.String())
	} else {
		rows = append(rows, "Lifetime: this process, until changed")
	}
	rows = append(rows, "", p.Effect)
	for _, s := range p.Consequences {
		rows = append(rows, "• "+s)
	}
	rows = append(rows, "", fmt.Sprintf("Revision %d · preview expires %s", p.Revision, p.ExpiresAt.UTC().Format("15:04:05 UTC")), "Local change; configuration files are not rewritten.")
	var wrapped []string
	for _, r := range rows {
		wrapped = append(wrapped, wrapText(safeTerminal(r), max(m.viewportWidth()-2, 1))...)
	}
	return wrapped
}
func (m Model) reviewPageSize() int { return max(m.visibleRows()-3, 1) }
func (m Model) reviewPages() int {
	return max((len(m.reviewLines())+m.reviewPageSize()-1)/m.reviewPageSize(), 1)
}
func (m Model) controlCanApply() bool {
	return m.controlPanel.preview != nil && m.controlPanel.seenThrough >= m.reviewPages()-1 && m.width >= 28 && m.visibleRows() >= 6 && time.Now().Before(m.controlPanel.preview.ExpiresAt) && !m.controlPanel.pending
}
func (m *Model) moveReviewPage(delta int) {
	c := &m.controlPanel
	c.page = max(0, min(c.page+delta, m.reviewPages()-1))
	if c.page <= c.seenThrough+1 {
		c.seenThrough = max(c.page, c.seenThrough)
	}
	c.focus = 0
	c.pressed = -1
}
func (m Model) renderControlReview() string {
	h, w := m.visibleRows(), m.viewportWidth()
	rows := m.reviewLines()
	size := m.reviewPageSize()
	start := min(m.controlPanel.page*size, len(rows))
	end := min(start+size, len(rows))
	out := []string{}
	for i := start; i < end; i++ {
		text := " " + rows[i]
		if i == 0 {
			text = m.styles.sectionStyle.Render(text)
		}
		out = append(out, text)
	}
	for len(out) < size {
		out = append(out, "")
	}
	out = append(out, m.styles.dimStyle2.Render(fmt.Sprintf(" Page %d/%d · ←/→ read · Esc cancel", m.controlPanel.page+1, m.reviewPages())))
	reason := "Enter selects focused button"
	if !m.controlCanApply() {
		reason = "Read all pages; Apply stays disabled"
		if m.controlPanel.preview != nil && !time.Now().Before(m.controlPanel.preview.ExpiresAt) {
			reason = "Expired · r refreshes the preview"
		}
	}
	out = append(out, m.styles.dimStyle2.Render(" "+reason))
	cancel, apply := " [ Cancel ] ", " [ Apply ] "
	if m.controlPanel.focus == 0 {
		cancel = m.styles.tabActiveStyle.Padding(0).Render(cancel)
	} else {
		cancel = m.styles.tabInactiveStyle.Padding(0).Render(cancel)
	}
	if !m.controlCanApply() {
		apply = m.styles.dimStyle2.Render(" [ Read first ] ")
	} else if m.controlPanel.focus == 1 {
		apply = m.styles.queueWarnStyle.Bold(true).Reverse(true).Render(apply)
	} else {
		apply = m.styles.queueWarnStyle.Render(apply)
	}
	out = append(out, cancel+"  "+apply)
	return fitRows(out, w, h)
}
func (m Model) renderControlForm() string {
	c := m.controlPanel
	title := "Set concurrency and expiry"
	explain := []string{"Enter: LIMIT TTL", "TTL is optional, e.g. 8 10m.", "0s means a process-lifetime change.", "Existing requests keep their leases.", "Shrinking creates debt until release."}
	if c.command.Action == control.Cancel {
		title = "Cancel one live request"
		explain = []string{"Enter an exact request ID.", "Cancellation is cooperative.", "An upstream may continue working."}
	}
	if c.command.Action == control.Pause || c.command.Action == control.Drain {
		title = string(c.command.Action) + " expiry"
		explain = []string{"Enter a duration, e.g. 10m.", "0s means a process-lifetime change."}
	}
	rows := []string{m.styles.sectionStyle.Render(" " + title), " Provider: " + c.command.Provider}
	if c.command.Pool != "" {
		rows = append(rows, " Pool: "+c.command.Pool)
	}
	rows = append(rows, "")
	for _, s := range explain {
		for _, r := range wrapText(s, max(m.viewportWidth()-2, 1)) {
			rows = append(rows, " "+r)
		}
	}
	rows = append(rows, "", m.styles.filterPromptStyle.Render(inputLine(" > ", c.input, m.viewportWidth())), "", m.styles.dimStyle2.Render(" Enter review · Esc cancel"))
	return fitRows(rows, m.viewportWidth(), m.visibleRows())
}
func (m Model) handleControlKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	c := &m.controlPanel
	if c.preview != nil {
		switch msg.String() {
		case "r":
			command := c.preview.Command
			return m, m.previewControl(command), true
		case "esc":
			c.preview = nil
			c.pressed = -1
			return m, nil, true
		case "right", "pgdown", "j", "down":
			m.moveReviewPage(1)
		case "left", "pgup", "k", "up":
			m.moveReviewPage(-1)
		case "tab", "shift+tab":
			if m.controlCanApply() {
				c.focus = 1 - c.focus
			}
		case "enter":
			if c.focus == 0 {
				c.preview = nil
			} else {
				return m, m.applyControl(), true
			}
		}
		return m, nil, true
	}
	if c.form {
		switch msg.String() {
		case "esc":
			c.form = false
			c.input = ""
			return m, nil, true
		case "ctrl+u":
			c.input = ""
		case "backspace", "ctrl+h":
			c.input = dropGrapheme(c.input)
		case "enter":
			command := c.command
			parts := strings.Fields(c.input)
			if command.Action == control.Cancel {
				command.RequestID = strings.TrimSpace(c.input)
			} else if command.Action == control.Resize {
				if len(parts) < 1 || len(parts) > 2 {
					m.tell("Enter LIMIT and optional TTL")
					return m, nil, true
				}
				n, err := strconv.Atoi(parts[0])
				if err != nil {
					m.tell("Limit must be a positive integer")
					return m, nil, true
				}
				command.Limit = n
				if len(parts) == 2 {
					ttl, e := time.ParseDuration(parts[1])
					if e != nil {
						m.tell("TTL must be a duration such as 10m")
						return m, nil, true
					}
					command.TTL = ttl
				}
			} else {
				ttl, e := time.ParseDuration(strings.TrimSpace(c.input))
				if e != nil {
					m.tell("TTL must be a duration such as 10m")
					return m, nil, true
				}
				command.TTL = ttl
			}
			return m, m.previewControl(command), true
		default:
			if msg.Key().Text != "" && len(c.input) < 256 {
				c.input += msg.Key().Text
			}
		}
		return m, nil, true
	}
	if m.tab != tabControls || m.inspection.open || m.mode != modeBrowse {
		return m, nil, false
	}
	provider, ok := m.selectedControlProvider()
	if !ok {
		return m, nil, false
	}
	command := control.Command{Provider: provider.Name}
	switch msg.String() {
	case "p":
		command.Action = control.Pause
		return m, m.previewControl(command), true
	case "P":
		command.Action = control.Pause
		c.form = true
		c.input = "10m"
		c.command = command
		return m, nil, true
	case "u":
		command.Action = control.Resume
		return m, m.previewControl(command), true
	case "d":
		command.Action = control.Drain
		return m, m.previewControl(command), true
	case "D":
		command.Action = control.Drain
		c.form = true
		c.input = "10m"
		c.command = command
		return m, nil, true
	case "r", "+", "-":
		if len(provider.Pools) == 0 {
			m.tell("No positive configured pools for this provider")
			return m, nil, true
		}
		pool := provider.Pools[min(m.cursor, len(provider.Pools)-1)]
		command.Action = control.Resize
		command.Pool = pool.ID
		n := pool.Limit
		if msg.String() == "+" {
			n++
		}
		if msg.String() == "-" {
			n = max(1, n-1)
		}
		c.form = true
		c.command = command
		c.input = strconv.Itoa(n) + " 0s"
		return m, nil, true
	case "x":
		command.Action = control.Cancel
		c.form = true
		c.command = command
		c.input = ""
		return m, nil, true
	case "j", "down":
		m.cursor = min(m.cursor+1, max(len(provider.Pools)-1, 0))
		return m, nil, true
	case "k", "up":
		m.cursor = max(m.cursor-1, 0)
		return m, nil, true
	case "e":
		return m, m.beginExport("controls"), true
	}
	return m, nil, false
}
func (m Model) renderControls() string {
	h, w := m.visibleRows(), m.viewportWidth()
	p, ok := m.selectedControlProvider()
	if !ok {
		return fitRows([]string{" Runtime controls unavailable."}, w, h)
	}
	stateStyle := m.styles.statusOkStyle
	if p.State != control.Running {
		stateStyle = m.styles.queueWarnStyle
	}
	rows := []string{m.styles.sectionStyle.Render(" LOCAL RUNTIME CONTROLS"), stateStyle.Render(" " + p.Name + " · " + strings.ToUpper(string(p.State))), m.styles.dimStyle2.Render(fmt.Sprintf(" revision %d · preview before apply", m.controlPanel.snapshot.Revision)), ""}
	if !p.ExpiresAt.IsZero() {
		rows = append(rows, " Override expires "+p.ExpiresAt.UTC().Format("15:04:05 UTC"))
	}
	if w < 70 {
		rows = append(rows, m.styles.tableHeaderStyle.Render(" POOL          LIMIT USED HOLD WAIT"))
		for i := m.controlPoolStart(len(p.Pools)); i < min(m.controlPoolStart(len(p.Pools))+max(h-11, 1), len(p.Pools)); i++ {
			pool := p.Pools[i]
			label := truncatePlain(pool.ID, 12)
			row := fmt.Sprintf(" %-12s %5d %4d %4d %4d", label, pool.Limit, pool.Occupied, pool.Cooldown, pool.Waiters)
			style := m.styles.rowStyle
			if i == m.cursor {
				style = m.styles.rowSelectedStyle
			}
			rows = append(rows, style.Render(row))
		}
	} else {
		rows = append(rows, m.styles.tableHeaderStyle.Render(" POOL                             LIMIT  ACTIVE  OCCUPIED  COOLDOWN  WAITING  DEBT"))
		for i := m.controlPoolStart(len(p.Pools)); i < min(m.controlPoolStart(len(p.Pools))+max(h-11, 1), len(p.Pools)); i++ {
			pool := p.Pools[i]
			row := fmt.Sprintf(" %-32s %5d  %6d  %8d  %8d  %7d %5d", truncatePlain(pool.ID, 32), pool.Limit, pool.Active, pool.Occupied, pool.Cooldown, pool.Waiters, pool.Debt)
			style := m.styles.rowStyle
			if i == m.cursor {
				style = m.styles.rowSelectedStyle
			}
			rows = append(rows, style.Render(row))
		}
	}
	if len(p.Pools) == 0 {
		rows = append(rows, " No configured positive pool.")
	}
	rows = append(rows, "", m.styles.queueWarnStyle.Render(" p pause · u resume · d drain"), m.styles.sparklineStyle.Render(" r resize · P/D expiry · x cancel ID"), m.styles.dimStyle2.Render(" Cooldown is part of occupied slots."))
	if len(p.Pools) > 0 {
		p := p.Pools[min(m.cursor, len(p.Pools)-1)]
		rows = append(rows, fmt.Sprintf(" Selected: %s", p.ID), fmt.Sprintf(" baseline %d · effective %d · debt %d", p.Baseline, p.Effective, p.Debt))
	}
	if len(rows)+3 < h {
		rows = append(rows, "", m.styles.sectionStyle.Render(" RECENT CONTROL CHANGES"))
		for i := len(m.controlPanel.snapshot.Audit) - 1; i >= 0 && len(rows) < h; i-- {
			a := m.controlPanel.snapshot.Audit[i]
			rows = append(rows, fmt.Sprintf(" %s %s", a.At.UTC().Format("15:04:05"), safeTerminal(a.Summary)))
		}
	}
	return fitRows(rows, w, h)
}
func dropGrapheme(s string) string {
	g := uniseg.NewGraphemes(s)
	end := 0
	for g.Next() {
		start, _ := g.Positions()
		end = start
	}
	return s[:end]
}

// Buttons only activate on a matching left-button press and release. Resizing,
// blurring, paging, closing or replacing a review clears the armed press.
func (m Model) controlButtonAt(x, y int) int {
	if m.controlPanel.preview == nil || y != m.contentStartRow()+m.visibleRows()-1 {
		return -1
	}
	if x >= 0 && x < 12 {
		return 0
	}
	if x >= 14 && x < 25 && m.controlCanApply() {
		return 1
	}
	return -1
}

func (m Model) controlPoolStart(n int) int {
	visible := max(m.visibleRows()-11, 1)
	return max(0, min(m.cursor-visible+1, max(n-visible, 0)))
}
