package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeycumines/ai-concurrency-shaper/internal/observation"
)

func TestObservationFlagsScopeAndBounds(t *testing.T) {
	dir := t.TempDir()
	args := []string{"-history-db", filepath.Join(dir, "observations.db"), "-history-retention", "48h", "-history-durability", "full", "-diagnostic-capture-bytes", "1048576", "-tui-theme", "light", "-tui-color", "256", "-export-dir", dir, "--provider", "-name", "blue", "-prefix", "/blue", "-upstream", "http://localhost:1234"}
	c, err := Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.ResolveAndValidate(); err != nil {
		t.Fatal(err)
	}
	s := c.Server
	if s.HistoryRetention != 48*time.Hour || s.DiagnosticCaptureBytes != 1<<20 || s.TUITheme != "light" || s.TUIColor != "256" || s.HistoryDurability != "full" {
		t.Fatalf("flags not applied: %+v", s)
	}
	if _, err = Parse([]string{"--provider", "-upstream", "http://localhost:1234", "-history-db", "history.db"}); err == nil {
		t.Fatal("server flag accepted inside provider scope")
	}
	for _, tc := range []struct{ flag, value string }{{"-history-retention", "0"}, {"-history-retention", "30m"}, {"-history-retention", "8761h"}, {"-history-durability", "off"}, {"-diagnostic-capture-bytes", "-1"}, {"-diagnostic-capture-bytes", "1048577"}, {"-tui-theme", "blue"}, {"-tui-color", "8"}, {"-history-db", "file:secret.db"}, {"-history-db", "//server/share/db"}, {"-export-dir", filepath.Join(dir, "absent")}} {
		t.Run(tc.flag+tc.value, func(t *testing.T) {
			c, err := Parse([]string{tc.flag, tc.value, "-upstream", "http://localhost:1234"})
			if err == nil {
				defer c.Close()
				err = c.ResolveAndValidate()
			}
			if err == nil {
				t.Fatalf("accepted %s=%s", tc.flag, tc.value)
			}
		})
	}
}
func TestObservationReservedProvider(t *testing.T) {
	c, err := Parse([]string{"-name", "__unresolved__", "-upstream", "http://localhost:1234"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.ResolveAndValidate(); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved name accepted: %v", err)
	}
}

func TestObservationIdentityLimitsRejectInsteadOfMerging(t *testing.T) {
	for _, name := range []string{strings.Repeat("x", 257), "invalid\xff"} {
		c, err := Parse([]string{"-name", name, "-upstream", "http://localhost:1234"})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err = c.ResolveAndValidate(); err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatalf("ambiguous identity accepted: %v", err)
		}
	}
	c := &Config{Providers: make([]*Provider, observation.MaxProviders+1)}
	for i := range c.Providers {
		c.Providers[i] = &Provider{}
	}
	if err := c.ResolveAndValidate(); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("excess providers accepted: %v", err)
	}
}
