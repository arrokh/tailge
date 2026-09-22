package tui

// ANSI-safe text sanitization and shared workspace layout helpers.

import (
	ansi "github.com/charmbracelet/x/ansi"
	"strings"
)

func sanitizeTUIText(value string) string {
	// Strip complete ANSI sequences before replacing control bytes. Replacing
	// ESC alone would leak the remaining SGR payload (for example, "[7m")
	// into the rendered view.
	value = ansi.Strip(value)
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
}
func truncate(value string, max int) string {
	if max < 4 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max-3]) + "..."
}
func valueOr(a, b string) string {
	if a == "" {
		return b
	}
	return a
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
