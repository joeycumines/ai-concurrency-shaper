package toast

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
)

// stripANSI removes ANSI escape sequences for reliable assertions.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\x1b' {
			if i+1 < len(s) && s[i+1] == '[' {
				i += 2
				for i < len(s) {
					ch := s[i]
					if ch >= 0x40 && ch <= 0x7E {
						break
					}
					i++
				}
			} else if i+1 < len(s) {
				i++
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func TestToast_BottomRender(t *testing.T) {
	tt := &Toast{Message: "saved"}
	got := tt.Render(20, 5)
	stripped := stripANSI(got)
	if !strings.Contains(stripped, "saved") {
		t.Error("expected output to contain 'saved'")
	}
	lines := strings.Split(stripped, "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines (4 padding + 1 message), got %d", len(lines))
	}
	if lines[0] != "" {
		t.Errorf("expected first line to be empty (padding), got %q", lines[0])
	}
	// The last line should contain "saved" (may have padding from style).
	if !strings.Contains(lines[4], "saved") {
		t.Errorf("expected last line to contain 'saved', got %q", lines[4])
	}
}

func TestToast_WithStyle(t *testing.T) {
	style := lipgloss.NewStyle().Bold(true)
	tt := &Toast{Message: "alert", Style: style}
	got := tt.Render(20, 3)
	stripped := stripANSI(got)
	if !strings.Contains(stripped, "alert") {
		t.Error("expected output to contain 'alert'")
	}
	// Styled text should contain ANSI escape codes in the raw output.
	lastLine := strings.Split(got, "\n")[2]
	if lastLine == "alert" {
		t.Error("expected styled text to contain ANSI escape codes, got plain text")
	}
}

func TestToast_WithWidth(t *testing.T) {
	tt := &Toast{Message: "short", Width: 20}
	got := tt.Render(20, 3)
	stripped := stripANSI(got)
	if !strings.Contains(stripped, "short") {
		t.Error("expected output to contain 'short'")
	}
}

func TestToast_ZeroBounds(t *testing.T) {
	tt := &Toast{Message: "msg"}
	tests := []struct {
		name string
		w, h int
	}{
		{"zero width", 0, 5},
		{"zero height", 5, 0},
		{"negative width", -1, 5},
		{"negative height", 5, -1},
		{"both zero", 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tt.Render(tc.w, tc.h)
			if got != "" {
				t.Errorf("expected empty string, got %q", got)
			}
		})
	}
}

func TestToast_EmptyMessage(t *testing.T) {
	tt := &Toast{}
	if got := tt.Render(20, 5); got != "" {
		t.Errorf("expected empty string for empty message, got %q", got)
	}
}

func TestToast_Expired(t *testing.T) {
	tt := &Toast{Message: "old", Duration: 50 * time.Millisecond}
	tt.Show()
	if tt.Expired() {
		t.Fatal("toast should not be expired immediately")
	}
	time.Sleep(100 * time.Millisecond)
	if !tt.Expired() {
		t.Fatal("toast should be expired after duration")
	}
}

func TestToast_NotExpiredWithZeroDuration(t *testing.T) {
	tt := (&Toast{Message: "sticky"}).Show()
	if tt.Expired() {
		t.Fatal("toast with zero duration should never expire")
	}
}

func TestVisibleToasts(t *testing.T) {
	live := (&Toast{Message: "live"}).Show()
	expired := (&Toast{Message: "expired", Duration: 1 * time.Millisecond}).Show()
	time.Sleep(5 * time.Millisecond)

	result := VisibleToasts([]*Toast{live, expired})
	if len(result) != 1 {
		t.Fatalf("expected 1 visible toast, got %d", len(result))
	}
	if result[0].Message != "live" {
		t.Errorf("expected 'live', got %q", result[0].Message)
	}
}

func TestShowSetsCreatedAt(t *testing.T) {
	tt := &Toast{Message: "test"}
	if !tt.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be zero before Show()")
	}
	tt.Show()
	if tt.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be set after Show()")
	}
}
