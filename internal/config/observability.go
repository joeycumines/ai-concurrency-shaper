package config

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

func (s *Server) validateObservability() error {
	// Programmatic zero-value configurations use the same defaults as flags.
	if s.HistoryRetention == 0 && !s.historyRetentionExplicit {
		s.HistoryRetention = 168 * time.Hour
	}
	if s.HistoryDurability == "" {
		s.HistoryDurability = "normal"
	}
	if s.TUITheme == "" {
		s.TUITheme = "auto"
	}
	if s.TUIColor == "" {
		s.TUIColor = "auto"
	}
	if s.ExportDir == "" {
		s.ExportDir = "."
	}
	if s.HistoryRetention < time.Hour || s.HistoryRetention > 8760*time.Hour {
		return errors.New("-history-retention must be between 1h and 8760h")
	}
	if !slices.Contains([]string{"normal", "full"}, s.HistoryDurability) {
		return errors.New("-history-durability must be normal or full")
	}
	if s.DiagnosticCaptureBytes < 0 || s.DiagnosticCaptureBytes > 1<<20 {
		return errors.New("-diagnostic-capture-bytes must be between 0 and 1048576")
	}
	if !slices.Contains([]string{"auto", "dark", "light"}, s.TUITheme) {
		return errors.New("-tui-theme must be auto, dark or light")
	}
	if !slices.Contains([]string{"auto", "truecolor", "256", "16", "mono"}, s.TUIColor) {
		return errors.New("-tui-color must be auto, truecolor, 256, 16 or mono")
	}
	if strings.HasPrefix(strings.ToLower(s.HistoryDB), "file:") || strings.Contains(s.HistoryDB, "://") || strings.HasPrefix(s.HistoryDB, `\\`) || strings.HasPrefix(s.HistoryDB, "//") {
		return errors.New("-history-db must be a local filesystem path, not a URI or network share")
	}
	info, err := os.Stat(s.ExportDir)
	if err != nil {
		return fmt.Errorf("-export-dir: %w", err)
	}
	if !info.IsDir() {
		return errors.New("-export-dir must name an existing directory")
	}
	return nil
}
